package alert

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/paulscode/forktower/internal/bus"
	"github.com/paulscode/forktower/internal/config"
	"github.com/paulscode/forktower/internal/redact"
	"github.com/paulscode/forktower/internal/store"
)

// SubscriberName identifies the alerter's bus subscription in drop diagnostics.
const SubscriberName = "alerter"

// Defaults for anything the caller leaves unset.
const (
	// DefaultCriticalRepeat is how long an unacknowledged urgent alert waits
	// before being sent again.
	DefaultCriticalRepeat = 30 * time.Minute
	// DefaultScanInterval is how often unacknowledged alerts are reconsidered.
	// Well below the repeat interval, so the repeat lands close to when it is due
	// rather than up to a whole interval late.
	DefaultScanInterval = time.Minute
	// sendQueue is how many alerts can be waiting for delivery.
	//
	// Delivery is network I/O and the event loop must never wait on it: a bus
	// subscriber that falls behind loses events, and the events this one is
	// subscribed to are the ones that matter most.
	sendQueue = 64
	// DefaultSweepInterval is how often the stored state is re-read for the
	// conditions no event announces.
	//
	// Daily, because the condition it looks for is an absence: a channel closed on
	// the user's own chain whose close has still not reached the other one.
	// Nothing happens to trigger it, which is exactly what makes it dangerous, so
	// something has to go looking.
	DefaultSweepInterval = 24 * time.Hour
	// writeTimeout bounds a storage write that outlives its trigger.
	writeTimeout = 5 * time.Second
)

// Config configures the alerter.
type Config struct {
	// CriticalRepeat is how long an unacknowledged urgent alert waits before
	// being delivered again. Zero uses DefaultCriticalRepeat.
	CriticalRepeat time.Duration
	// ScanInterval is how often the unacknowledged alerts are reconsidered. Zero
	// uses DefaultScanInterval. Tests set it small.
	ScanInterval time.Duration
	// SendTimeout bounds one delivery attempt. Zero uses DefaultSendTimeout.
	SendTimeout time.Duration
	// SelfTestInterval is how often a synthetic alert is pushed through every
	// transport. Zero uses DefaultSelfTestInterval.
	SelfTestInterval time.Duration
	// SweepInterval is how often the stored state is re-read for conditions
	// nothing announces. Zero uses DefaultSweepInterval. Tests set it small.
	SweepInterval time.Duration
	// PlatformNotifications says the surrounding platform raises alerts itself by
	// reading this daemon's API, so having no transports of our own is the
	// expected arrangement rather than a gap. Only affects what is said at
	// startup; the platform's own delivery is not this engine's business.
	PlatformNotifications bool
}

func (c Config) withDefaults() Config {
	if c.CriticalRepeat <= 0 {
		c.CriticalRepeat = DefaultCriticalRepeat
	}
	if c.ScanInterval <= 0 {
		c.ScanInterval = DefaultScanInterval
	}
	if c.SendTimeout <= 0 {
		c.SendTimeout = DefaultSendTimeout
	}
	if c.SelfTestInterval <= 0 {
		c.SelfTestInterval = DefaultSelfTestInterval
	}
	if c.SweepInterval <= 0 {
		c.SweepInterval = DefaultSweepInterval
	}
	return c
}

// Alerter turns events into notifications the user actually receives.
type Alerter struct {
	store  *store.Store
	bus    *bus.Bus
	routes []Route
	cfg    Config
	now    func() time.Time
	log    *slog.Logger

	// events is subscribed at construction, not when Run starts.
	//
	// A goroutine that subscribes when it happens to be scheduled leaves a window
	// between wiring the daemon up and listening, and an event published in that
	// window is gone: unlike the sentinel, this engine has no poll to fall back
	// on, so a missed "the chains have separated" is never noticed at all.
	events <-chan bus.Event
	sends  chan store.Alert
}

// New builds an alerter. A nil logger discards, and a nil clock reads the real
// one, so callers that do not care about either can pass nothing.
func New(
	st *store.Store,
	b *bus.Bus,
	routes []Route,
	cfg Config,
	log *slog.Logger,
	now func() time.Time,
) (*Alerter, error) {
	if st == nil {
		return nil, errors.New("alert: a store is required")
	}
	if b == nil {
		return nil, errors.New("alert: an event bus is required")
	}
	for _, r := range routes {
		if r.Transport == nil {
			return nil, errors.New("alert: a route has no transport")
		}
		if r.Transport.Name() == "" {
			return nil, errors.New("alert: a transport has no name")
		}
	}
	if log == nil {
		log = slog.New(discardHandler{})
	}
	if now == nil {
		now = time.Now
	}

	a := &Alerter{
		store:  st,
		bus:    b,
		routes: routes,
		cfg:    cfg.withDefaults(),
		now:    now,
		log:    log,
		events: b.Subscribe(SubscriberName,
			bus.KindSplitStateChanged, bus.KindViewHealthChanged,
			bus.KindFundingSpent, bus.KindMempoolSighting, bus.KindSpendReorgedOut,
			bus.KindDeadlineEscalated, bus.KindDeadlineResolved,
			bus.KindDeadlineExpiredLoss,
			bus.KindTowerHealthChanged, bus.KindTowerConcern),
		sends: make(chan store.Alert, sendQueue),
	}
	switch {
	case len(routes) > 0:
	case cfg.PlatformNotifications:
		// Not a gap. On StartOS and Umbrel the platform reads this daemon's alerts
		// and raises them in its own notification centre, because an app container
		// has no path to that system. Warning here would send a user looking for a
		// problem they do not have.
		a.log.Info("alerts are raised by the platform's own notification centre")
	default:
		// Not an error either: a user may be watching the dashboard and nothing
		// else. But an alarm nobody can hear is worth saying out loud once, and the
		// readiness check surfaces it in the UI.
		a.log.Warn("no notification transports are configured, so alerts will only " +
			"appear in the dashboard")
	}
	return a, nil
}

// Run consumes events and delivers alerts until the context ends.
//
// Four goroutines: one reads the bus and writes to storage, one delivers, one
// runs the notification self-test on its schedule, and one sweeps the stored
// state for the conditions no event announces. The first two are separate
// because delivery is network I/O and the reader must never wait on it — a
// subscriber that falls behind loses events, and a lost "the chains have
// separated" is the one failure this software exists to prevent.
func (a *Alerter) Run(ctx context.Context) error {
	var g errgroup.Group
	g.Go(func() error { return a.consume(ctx, a.events) })
	g.Go(func() error { return a.deliverQueued(ctx) })
	g.Go(func() error { return a.selfTestLoop(ctx) })
	g.Go(func() error { return a.sweepLoop(ctx) })
	return g.Wait()
}

func (a *Alerter) consume(ctx context.Context, events <-chan bus.Event) error {
	ticker := time.NewTicker(a.cfg.ScanInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil

		case e, ok := <-events:
			if !ok {
				// The bus closed while the context is still alive. Stop listening but
				// keep escalating: an urgent alert already raised still needs repeating.
				events = nil
				continue
			}
			a.handle(ctx, e)

		case <-ticker.C:
			a.escalate(ctx)
		}
	}
}

// handle raises the alert an event warrants, if any.
func (a *Alerter) handle(ctx context.Context, e bus.Event) {
	candidate, ok := MapEventToAlert(e)
	if !ok {
		return
	}
	a.raise(ctx, candidate)
}

// raise records an alert and, if it is news, announces and delivers it.
func (a *Alerter) raise(ctx context.Context, candidate Candidate) {
	a.put(ctx, candidate, false)
}

// raiseIfAbsent records a standing condition without disturbing what the user
// has already done about it.
//
// Used only by the reconciler, and the distinction is the difference between a
// safety net and a nuisance. The reconciler re-derives the same conditions from
// stored state every time it runs, so going through the ordinary path would
// clear an acknowledgement and notify again on every pass — turning "I have seen
// this" into a notification every minute.
func (a *Alerter) raiseIfAbsent(ctx context.Context, candidate Candidate) {
	a.put(ctx, candidate, true)
}

// put is the shared body of both.
func (a *Alerter) put(ctx context.Context, candidate Candidate, keepAck bool) {
	now := a.now().Unix()
	record := store.Alert{
		Tier:         candidate.Tier,
		Kind:         candidate.Kind,
		DedupKey:     candidate.DedupKey,
		Subject:      candidate.Subject,
		Message:      candidate.Message,
		CreatedAt:    now,
		LastRaisedAt: now,
	}

	// Persisted before it is announced, and with a context that outlives a
	// shutdown arriving mid-write: an alert the user was told about but which is
	// not in the dashboard afterwards is worse than one that arrives late.
	wctx, cancel := writeCtx(ctx)
	defer cancel()

	// A resolution closes the thread it names, when it names one. Sharing the
	// warning's key used to mean bumping that warning and clearing its
	// acknowledgement, which resurrected the failure instead of retiring it.
	//
	// **Falls through when there is nothing standing**, rather than being
	// dropped. Not every resolution closes something: a split ending, a view
	// coming back and a countdown stopping are all announcements under keys of
	// their own, and they are the best news this program has to give. Treating
	// "nothing to close" as "nothing to say" would have silenced all three.
	if candidate.Tier == store.TierResolved {
		id, resolveErr := a.store.ResolveAlert(wctx, record)
		if resolveErr != nil {
			a.log.Error("could not resolve an alert",
				slog.String("kind", candidate.Kind),
				slog.String("error", resolveErr.Error()))
			return
		}
		if id > 0 {
			record.ID = id
			a.bus.Publish(bus.AlertRaised{
				AlertID:   id,
				Tier:      string(record.Tier),
				AlertKind: record.Kind,
				DedupKey:  record.DedupKey,
				Message:   record.Message,
			})
			a.enqueue(record)
			return
		}
	}

	upsert := a.store.UpsertAlert
	if keepAck {
		upsert = a.store.ReconcileAlert
	}
	up, err := upsert(wctx, record)
	if err != nil {
		a.log.Error("could not record an alert",
			slog.String("kind", candidate.Kind), slog.String("error", err.Error()))
		return
	}
	if !up.Notify() {
		// The same condition, still in front of the user. Repeating it is the
		// escalation policy's job.
		a.log.Debug("alert re-raised without notifying",
			slog.String("kind", candidate.Kind), slog.Int64("alert_id", up.ID))
		return
	}

	record.ID = up.ID
	a.bus.Publish(bus.AlertRaised{
		AlertID:   up.ID,
		Tier:      string(record.Tier),
		AlertKind: record.Kind,
		DedupKey:  record.DedupKey,
		Message:   record.Message,
	})
	a.enqueue(record)
}

// escalate re-delivers unacknowledged urgent alerts.
//
// Read from storage rather than from memory, so a restart does not silence an
// alert nobody has acknowledged yet.
func (a *Alerter) escalate(ctx context.Context) {
	if len(a.routes) == 0 {
		return
	}

	alerts, err := a.store.ListAlerts(ctx, store.AlertFilter{UnackedOnly: true})
	if err != nil {
		a.log.Warn("could not read unacknowledged alerts",
			slog.String("error", err.Error()))
		return
	}

	now := a.now().Unix()
	repeatSecs := int64(a.cfg.CriticalRepeat / time.Second)

	for _, al := range alerts {
		if !Urgent(al.Tier) {
			continue
		}
		last := a.lastAttemptAt(ctx, al.ID)
		if last == 0 {
			// Never delivered — most likely every transport was failing. Measure
			// from when it was raised so it is retried rather than retried forever
			// on the first scan.
			last = al.LastRaisedAt
		}
		if now-last < repeatSecs {
			continue
		}
		a.log.Info("repeating an unacknowledged urgent alert",
			slog.Int64("alert_id", al.ID), slog.String("kind", al.Kind))
		a.enqueue(al)
	}
}

func (a *Alerter) lastAttemptAt(ctx context.Context, alertID int64) int64 {
	deliveries, err := a.store.ListDeliveries(ctx, alertID)
	if err != nil {
		a.log.Debug("could not read delivery history",
			slog.Int64("alert_id", alertID), slog.String("error", err.Error()))
		return 0
	}
	var last int64
	for _, d := range deliveries {
		if d.AttemptedAt > last {
			last = d.AttemptedAt
		}
	}
	return last
}

// enqueue hands an alert to the sender, or drops it rather than blocking.
//
// A full queue means every transport is wedged. The alert is still stored and
// still on the dashboard; what is lost is the notification, and that is said
// loudly rather than by stalling the loop that watches for the next split.
func (a *Alerter) enqueue(al store.Alert) {
	if len(a.routes) == 0 {
		return
	}
	select {
	case a.sends <- al:
	default:
		a.log.Error("the notification queue is full, so an alert was not delivered",
			slog.Int64("alert_id", al.ID), slog.String("kind", al.Kind))
	}
}

func (a *Alerter) deliverQueued(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case al := <-a.sends:
			a.Deliver(ctx, al)
		}
	}
}

// Deliver sends one alert through every transport that wants it, recording each
// attempt. It reports how many succeeded, which is what a self-test needs.
//
// Transports run concurrently: one that is slow or unreachable must not delay the
// others, because the whole point of configuring several is that they do not fail
// together.
func (a *Alerter) Deliver(ctx context.Context, al store.Alert) int {
	var g errgroup.Group
	results := make([]bool, len(a.routes))

	for i, r := range a.routes {
		if !Deliverable(al.Tier, r.MinTier) {
			continue
		}
		g.Go(func() error {
			results[i] = a.send(ctx, r, al) == nil
			return nil
		})
	}
	_ = g.Wait()

	sent := 0
	for _, ok := range results {
		if ok {
			sent++
		}
	}
	return sent
}

func (a *Alerter) send(ctx context.Context, r Route, al store.Alert) error {
	return a.sendPayload(ctx, r, al, PayloadFor(al, r.IncludeDetail))
}

// sendPayload delivers a payload the caller has already decided on.
//
// Separate from send because the self-test is the one alert whose text is the
// same for everyone: it says only that notifications are working, which reveals
// nothing about the user's situation to anyone, and replacing it with "open your
// dashboard to see what happened" would alarm the user with their own alarm.
func (a *Alerter) sendPayload(ctx context.Context, r Route, al store.Alert, p Payload) error {
	sendCtx, cancel := context.WithTimeout(ctx, a.cfg.SendTimeout)
	defer cancel()

	err := r.Transport.Send(sendCtx, p)

	// Recorded with a context that survives shutdown, and scrubbed before it is
	// written: a transport error routinely echoes the request URL, and that URL
	// may carry a token into a database people are invited to email to a
	// maintainer.
	wctx, wcancel := writeCtx(ctx)
	defer wcancel()

	if _, recErr := a.store.RecordDelivery(wctx, store.Delivery{
		AlertID:     al.ID,
		Transport:   r.Transport.Name(),
		AttemptedAt: a.now().Unix(),
		OK:          err == nil,
		Error:       redact.Error(err),
	}); recErr != nil {
		a.log.Warn("could not record a delivery attempt",
			slog.String("transport", r.Transport.Name()),
			slog.String("error", recErr.Error()))
	}

	if err != nil {
		a.log.Warn("could not deliver an alert",
			slog.String("transport", r.Transport.Name()),
			slog.Int64("alert_id", al.ID),
			slog.String("error", redact.Error(err)))
	}
	return err
}

// writeCtx returns a context for storage writes that survives shutdown.
//
// Without this, a shutdown arriving between deciding to alert and recording it
// loses the record — and the record is what the dashboard shows afterwards.
func writeCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
}

// RoutesFromConfig builds the transports M1 supports.
//
// Types this milestone does not implement yet are reported rather than skipped:
// a user who configured a notification channel and hears nothing must be told
// why, not left believing they are covered.
func RoutesFromConfig(cfg []config.TransportConfig, timeout time.Duration) ([]Route, error) {
	routes := make([]Route, 0, len(cfg))
	for _, t := range cfg {
		switch t.Type {
		case config.TransportWebhook:
			w, err := NewWebhook(t.Name, t.URL, timeout)
			if err != nil {
				return nil, err
			}
			routes = append(routes, Route{
				Transport:     w,
				MinTier:       t.MinTier,
				IncludeDetail: t.EffectiveIncludeDetail(),
			})
		case config.TransportNtfy:
			n, err := NewNtfy(t.Name, t.URL, t.Token, timeout)
			if err != nil {
				return nil, err
			}
			routes = append(routes, Route{
				Transport:     n,
				MinTier:       t.MinTier,
				IncludeDetail: t.EffectiveIncludeDetail(),
			})

		case config.TransportSMTP:
			m, err := NewSMTP(SMTPOptions{
				Name: t.Name, Host: t.Host, Port: t.Port,
				User: t.User, Pass: t.Pass, From: t.From, To: t.To,
				StartTLS: t.StartTLS, Timeout: timeout,
			})
			if err != nil {
				return nil, err
			}
			routes = append(routes, Route{
				Transport:     m,
				MinTier:       t.MinTier,
				IncludeDetail: t.EffectiveIncludeDetail(),
			})

		case config.TransportStartOS, config.TransportUmbrel:
			// Not a transport this daemon can ever implement. Neither platform's
			// notification system is reachable from inside an app container —
			// verified on both — so there the platform reads this daemon's API and
			// raises the notification itself. Set alerts.platform_notifications
			// instead, which the packaging does.
			return nil, fmt.Errorf(
				"transport %q is of type %q, which Forktower cannot deliver to from "+
					"inside its own container; on that platform the notifications come "+
					"from the platform itself, so set alerts.platform_notifications "+
					"rather than listing a transport", t.Name, t.Type)

		case config.TransportTelegram:
			return nil, fmt.Errorf(
				"transport %q is of type %q, which this version cannot deliver to yet",
				t.Name, t.Type)
		default:
			return nil, fmt.Errorf("transport %q has unknown type %q", t.Name, t.Type)
		}
	}
	return routes, nil
}
