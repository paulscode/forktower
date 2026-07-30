package alert

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/paulscode/forktower/internal/store"
)

// Self-test alert kinds.
const (
	// KindSelfTest is the synthetic alert pushed through every transport.
	KindSelfTest = "self_test"
	// KindTransportFailing is raised for a transport the self-test could not
	// deliver through.
	KindTransportFailing = "transport_failing"
)

// DefaultSelfTestInterval matches the shipped configuration: weekly.
const DefaultSelfTestInterval = 168 * time.Hour

// ErrNoSuchTransport means a test was asked for by a name nothing answers to.
//
// Reported rather than treated as "test nothing": a user who typed a name and
// got a cheerful empty result would conclude their notifications were fine.
var ErrNoSuchTransport = errors.New("alert: no transport with that name")

// SelfTestResult is one transport's outcome.
type SelfTestResult struct {
	Transport string `json:"transport"`
	OK        bool   `json:"ok"`
	// Error is already scrubbed, because this is returned over the API.
	Error string `json:"error,omitempty"`
}

// SelfTest pushes a synthetic alert through every configured transport.
//
// This is the whole reason the milestone has a self-test: a notification path
// that has never been exercised is an assumption, and the moment it is needed is
// the worst possible time to discover that the URL was wrong or the password
// expired. A transport that fails raises a warning of its own, so a broken alarm
// is itself alarming rather than silent.
//
// The tier filter is deliberately ignored. A transport set to critical-only still
// has to be proven to work, and proving it is the point.
func (a *Alerter) SelfTest(ctx context.Context) []SelfTestResult {
	results, _ := a.TestTransports(ctx)
	return results
}

// TransportNames lists the configured transports, in configuration order.
func (a *Alerter) TransportNames() []string {
	out := make([]string, 0, len(a.routes))
	for _, r := range a.routes {
		out = append(out, r.Transport.Name())
	}
	return out
}

// Raise records an alert decided on elsewhere, and delivers it if it is news.
//
// Exported so that engines with something to say — a burst of failed sign-ins,
// for instance — go through the same deduplication, escalation and payload rules
// as everything else, rather than each inventing their own.
func (a *Alerter) Raise(ctx context.Context, c Candidate) { a.raise(ctx, c) }

// TestTransports delivers a synthetic alert through some or all transports.
//
// With no names it tests everything and counts as the scheduled self-test. With
// names it tests only those, and does not disturb the schedule: someone checking
// one webhook should not push the weekly test of the others a week further out.
func (a *Alerter) TestTransports(ctx context.Context, names ...string) ([]SelfTestResult, error) {
	routes, err := a.selectRoutes(names)
	if err != nil {
		return nil, err
	}
	if len(routes) == 0 {
		return nil, nil
	}

	now := a.now().Unix()
	synthetic := store.Alert{
		Tier:     store.TierInfo,
		Kind:     KindSelfTest,
		DedupKey: KindSelfTest,
		Message: "This is Forktower's regular test message. It means your " +
			"notifications are working. Nothing is wrong.",
		CreatedAt:    now,
		LastRaisedAt: now,
	}

	wctx, cancel := writeCtx(ctx)
	defer cancel()

	// Recorded first so every attempt has something to hang off, and so the
	// dashboard can show when the alarm was last proven to work.
	up, err := a.store.UpsertAlert(wctx, synthetic)
	if err != nil {
		a.log.Error("could not record the notification test",
			slog.String("error", err.Error()))
		return nil, fmt.Errorf("recording the notification test: %w", err)
	}
	synthetic.ID = up.ID

	// Sent in full to every transport regardless of their detail setting. This is
	// the one message that is the same for everyone: it says notifications are
	// working and nothing else, so there is nothing to withhold — and a test
	// message reading "open your dashboard to see what happened" would alarm the
	// user with their own alarm.
	payload := Payload{
		Version: PayloadVersion,
		Tier:    string(synthetic.Tier),
		Kind:    synthetic.Kind,
		Message: synthetic.Message,
	}

	results := make([]SelfTestResult, len(routes))
	var wg sync.WaitGroup
	for i, r := range routes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sendErr := a.sendPayload(ctx, r, synthetic, payload)
			results[i] = SelfTestResult{
				Transport: r.Transport.Name(),
				OK:        sendErr == nil,
				Error:     scrubError(sendErr),
			}
		}()
	}
	wg.Wait()

	// Stamped with when the test *began*, not when it finished. Delivery can take
	// the full send timeout on every transport, and measuring the interval from
	// completion would let a slow transport quietly push the schedule later on
	// every run.
	a.recordSelfTest(ctx, results, now, len(names) == 0)
	return results, nil
}

// selectRoutes narrows the transports to the named ones.
func (a *Alerter) selectRoutes(names []string) ([]Route, error) {
	if len(names) == 0 {
		return a.routes, nil
	}
	out := make([]Route, 0, len(names))
	for _, want := range names {
		found := false
		for _, r := range a.routes {
			if r.Transport.Name() == want {
				out = append(out, r)
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("%q: %w", want, ErrNoSuchTransport)
		}
	}
	return out, nil
}

// recordSelfTest turns the outcome into something the user can see.
func (a *Alerter) recordSelfTest(ctx context.Context, results []SelfTestResult, at int64, full bool) {
	failed := 0
	for _, r := range results {
		if r.OK {
			a.clearTransportFailure(ctx, r.Transport)
			continue
		}
		failed++
		a.raise(ctx, Candidate{
			Tier:     store.TierWarning,
			Kind:     KindTransportFailing,
			DedupKey: fmt.Sprintf("%s:%s", KindTransportFailing, r.Transport),
			Subject:  r.Transport,
			Message: fmt.Sprintf(
				"Forktower could not send a test message through %q, so alerts may not "+
					"reach you. Check its settings in Forktower.", r.Transport),
		})
	}

	if failed > 0 {
		a.log.Warn("some notification transports failed their test",
			slog.Int("failed", failed), slog.Int("total", len(results)))
	} else {
		a.log.Info("notification test delivered through every transport",
			slog.Int("transports", len(results)))
	}

	if !full {
		// A test of one transport is not the scheduled test of all of them, and
		// must not push the next one a week further out.
		return
	}
	wctx, cancel := writeCtx(ctx)
	defer cancel()
	if err := a.store.SetMetaInt64(wctx, store.MetaLastSelfTestAt, at); err != nil {
		a.log.Warn("could not record when the notification test ran",
			slog.String("error", err.Error()))
	}
}

// clearTransportFailure marks a previous failure as no longer needing attention.
//
// Acknowledged rather than announced: the condition has demonstrably ended, and a
// separate "your webhook works again" notification for a test the user never
// asked for is noise. The alert stays in the history with its original timestamp,
// and if the transport breaks again the raise reopens it.
func (a *Alerter) clearTransportFailure(ctx context.Context, transport string) {
	key := fmt.Sprintf("%s:%s", KindTransportFailing, transport)

	open, err := a.store.ListAlerts(ctx, store.AlertFilter{UnackedOnly: true})
	if err != nil {
		a.log.Debug("could not check for an earlier transport failure",
			slog.String("error", err.Error()))
		return
	}
	for _, al := range open {
		if al.DedupKey != key {
			continue
		}
		wctx, cancel := writeCtx(ctx)
		if _, err := a.store.AckAlert(wctx, al.ID, a.now().Unix()); err != nil {
			a.log.Debug("could not clear an earlier transport failure",
				slog.String("error", err.Error()))
		}
		cancel()
		a.log.Info("a transport that had been failing is working again",
			slog.String("transport", transport))
		return
	}
}

// selfTestLoop runs the test on its schedule, and once at first run.
//
// The first run is not waiting for a week to elapse. A user who has just
// configured notifications wants to know now that they work, and an alarm nobody
// has proven works is not an alarm.
func (a *Alerter) selfTestLoop(ctx context.Context) error {
	if len(a.routes) == 0 {
		return nil
	}

	ticker := time.NewTicker(a.cfg.ScanInterval)
	defer ticker.Stop()

	for {
		if a.selfTestDue(ctx) {
			a.SelfTest(ctx)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (a *Alerter) selfTestDue(ctx context.Context) bool {
	last, err := a.store.GetMetaInt64(ctx, store.MetaLastSelfTestAt, 0)
	if err != nil {
		a.log.Debug("could not read when the notification test last ran",
			slog.String("error", err.Error()))
		return false
	}
	if last == 0 {
		// Never run. This is the first-run case.
		return true
	}
	return a.now().Unix()-last >= int64(a.cfg.SelfTestInterval/time.Second)
}
