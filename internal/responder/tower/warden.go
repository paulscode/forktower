package tower

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/paulscode/forktower/internal/bus"
	"github.com/paulscode/forktower/internal/store"
)

// DefaultInterval is how often a tower is looked at.
//
// A minute, matching the registry's poll. A watchtower does not change quickly
// and the useful signal is "this has been wrong for a while" rather than "this
// was wrong for four seconds", so a faster loop would buy nothing and would
// write to the database for no reason.
const DefaultInterval = time.Minute

// writeTimeout bounds the writes that record what a pass found.
//
// Detached from the caller's context on purpose: a shutdown arriving between
// observing a tower and recording what was observed must not lose the
// observation. Same rule the detection engines follow.
const writeTimeout = 5 * time.Second

// Warden keeps one tower's record up to date and says when something is wrong.
//
// The supervisor watches the tower itself and the monitor watches what the
// user's node is actually backing up to it. Both are needed and neither is
// enough: a tower can be perfectly healthy while nothing is being sent to it,
// which looks like protection and is not.
type Warden struct {
	store      *store.Store
	bus        *bus.Bus
	log        *slog.Logger
	supervisor *Supervisor
	// client is the user's node, or nil when they have none configured. The
	// coverage monitor is built from it on the first pass that learns the
	// tower's own pubkey, because that is what the monitor needs to recognise
	// itself among whatever else the user has registered.
	client ClientReader
	// clnClient is the Core Lightning side, for a teos tower. The two arms read
	// different things from different nodes and cannot share one reader: an LND
	// node has watchtower *sessions*, and a Core Lightning node has a plugin with
	// a subscription.
	clnClient         CLNTowerReader
	monitor           *Monitor
	teos              *TeosMonitor
	lowFeeSatPerVByte uint32
	kind              store.TowerKind
	// managed is whether this installation runs the tower. It changes what may
	// honestly be said about fixing it.
	managed bool
	uri     string
	// canAttachOnion says this packaging can give the tower a Tor address, and
	// that Forktower has asked it to when there is none. Only StartOS 0.4.x can:
	// the 0.3.5.1 and Umbrel packages advertise the only address they have, and
	// telling those users to go and approve a request nobody made would send them
	// looking for a screen that does not exist.
	canAttachOnion bool
	interval       time.Duration
	now            func() time.Time
	// events carries the chain the tower watches, so that a teos subscription's
	// expiry height can be measured against something. Nil for an LND tower,
	// which has no expiry to measure.
	events <-chan bus.Event
	branch store.Branch

	// towerID is filled on the first pass, once the tower has a row. Atomic
	// because TowerID() is read from outside the goroutine running the loop.
	towerID atomic.Int64
	// lastStatus is what was last announced, so an unchanged tower is not
	// announced every minute.
	lastStatus store.TowerStatus
	// lastConcerns is what was last said, so a standing problem is not repeated
	// on every pass. A concern that goes away and comes back is said again.
	lastConcerns map[string]bool
	// settledSaid records which "this no longer applies" declarations have been
	// made since this process started. They are declared rather than diffed
	// against lastConcerns precisely because a restart empties that memory, so
	// they need a memory of their own to avoid repeating every pass.
	settledSaid map[ConcernKind]bool
	// previous is the coverage from the last pass, which is what makes a stalled
	// backup count visible.
	previous []store.Coverage
	// firstSeen is when this tower was first recorded, which is what the grace
	// period is measured from.
	firstSeen int64
	// teosPubkey and teosSlotsAtStart are the Core Lightning arm's inputs.
	// tipHeight is the chain the tower watches, which is what a subscription's
	// expiry height has to be measured against.
	teosPubkey       string
	teosSlotsAtStart int32
	tipHeight        int32
}

// WardenOptions configures a Warden.
type WardenOptions struct {
	Store      *store.Store
	Bus        *bus.Bus
	Log        *slog.Logger
	Supervisor *Supervisor
	// Client is the user's LND, or nil when they have none. The tower is still
	// watched without one — it is still running and still costing disk — but
	// nothing can be said about what it protects.
	Client ClientReader
	// CLNClient is the user's Core Lightning node, for a teos tower.
	CLNClient CLNTowerReader
	// LowFeeSatPerVByte, when set, is the rate below which a session's baked-in
	// justice fee is worth mentioning.
	LowFeeSatPerVByte uint32
	Kind              store.TowerKind
	Managed           bool
	URI               string
	// CanAttachOnion says this packaging can give the tower a Tor address on
	// request. See the field of the same name on Warden.
	CanAttachOnion bool
	// TeosPubkey identifies our teos tower among those the node has registered
	// with, and TeosSlots is the subscription size the tower was configured with,
	// so that "running low" can be judged as a fraction rather than only as
	// exhaustion. Zero means unknown, which is the honest state for a tower
	// somebody else configured.
	TeosPubkey string
	TeosSlots  int32
	// Branch is the chain the tower watches. Its tip is what a subscription's
	// expiry height is measured against.
	Branch   store.Branch
	Interval time.Duration
	Now      func() time.Time
}

// NewWarden builds a Warden.
func NewWarden(opts WardenOptions) (*Warden, error) {
	if opts.Store == nil || opts.Bus == nil {
		return nil, errors.New("tower: a warden needs storage and a bus")
	}
	if opts.Supervisor == nil {
		return nil, errors.New("tower: a warden needs a supervisor")
	}
	if !opts.Kind.Valid() {
		return nil, fmt.Errorf("tower: %q is not a watchtower kind", opts.Kind)
	}
	if opts.Log == nil {
		opts.Log = slog.New(discardHandler{})
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Warden{
		store: opts.Store, bus: opts.Bus, log: opts.Log,
		supervisor: opts.Supervisor, client: opts.Client, clnClient: opts.CLNClient,
		lowFeeSatPerVByte: opts.LowFeeSatPerVByte,
		kind:              opts.Kind, managed: opts.Managed, uri: opts.URI,
		canAttachOnion:   opts.CanAttachOnion,
		teosPubkey:       opts.TeosPubkey,
		teosSlotsAtStart: opts.TeosSlots,
		branch:           opts.Branch,
		interval:         opts.Interval, now: opts.Now,
		lastConcerns: map[string]bool{},
		settledSaid:  map[ConcernKind]bool{},
		// The chain the tower watches, for the one thing that needs a height: a
		// teos subscription lapses at one, and nothing else here reads a tip.
		events: opts.Bus.Subscribe(
			SubscriberName+":"+string(opts.Kind), bus.KindSplitBranchExtended),
	}, nil
}

// SubscriberName identifies this engine on the bus and in logs.
const SubscriberName = "tower"

// Run watches until the context is cancelled.
func (w *Warden) Run(ctx context.Context) error {
	// One pass immediately, because a user who has just started the daemon
	// should not wait a minute to be told their tower is unreachable.
	w.pass(ctx)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil

		case ev, ok := <-w.events:
			if !ok {
				return nil
			}
			// Only the height is wanted, and only for the chain this tower
			// watches. A subscription that lapses at a block height needs a block
			// height to be compared against, and nothing else here reads one.
			if extended, isBlock := ev.(bus.SplitBranchExtended); isBlock &&
				store.Branch(extended.Branch) == w.branch {
				w.tipHeight = extended.Block.Height
			}

		case <-ticker.C:
			w.pass(ctx)
		}
	}
}

// pass is one round of looking.
//
// Never returns an error: a tower that cannot be read is the answer this engine
// exists to produce, not a reason to stop producing answers.
func (w *Warden) pass(ctx context.Context) {
	obs := w.supervisor.Observe(ctx)
	now := w.now().Unix()

	if err := w.record(ctx, obs, now); err != nil {
		w.log.Error("recording the tower's condition", slog.String("error", err.Error()))
		return
	}

	// **Announced whether or not there is a row to attach it to.** A tower that
	// has never answered has no identity to key a row on — it is keyed by its
	// pubkey, and it has not told us one — but a configured tower that has never
	// answered is exactly the thing a user needs to hear about. Reporting only
	// what we could file would mean the one case where protection was never there
	// at all is the one case nobody is told about.
	w.announceHealth(obs)

	var concerns []Concern
	if obs.NearingDiskLimit() {
		concerns = append(concerns, Concern{
			Kind: ConcernDiskFilling, TowerID: w.towerID.Load(),
			Message: fmt.Sprintf(
				"the tower's storage is at %d MB of the %d MB Forktower allows it. "+
					"A watchtower accepts a session from anyone who can reach it, so "+
					"this is worth looking at rather than waiting on.",
				obs.UsedBytes>>20, obs.LimitBytes>>20),
		})
	}
	coverage, settled, assessed := w.checkCoverage(ctx, obs, now)
	concerns = append(concerns, coverage...)

	for _, c := range concerns {
		w.raise(c)
	}
	// Anything that was a problem last time and is not one now is forgotten, so
	// that a problem which clears and comes back is said again. Without this a
	// tower could go bad, be fixed, go bad again, and be announced only once.
	//
	// **Only when the check reached a verdict.** An empty list can mean "nothing
	// is wrong" or "could not tell", and the second is common: the tower's own
	// RPC takes a while to come up after a restart. Since forgetting now says so
	// out loud, treating the two alike would tell a user their watchtower client
	// had been switched on every time the tower restarted, then warn them again a
	// minute later.
	if assessed {
		w.forgetResolved(concerns, settled)
	}
}

// ConcernDiskFilling means the tower's storage is approaching the cap Forktower
// imposes on it, because LND imposes none of its own.
const ConcernDiskFilling ConcernKind = "tower.disk_filling"

// record writes what was observed, so that a restart does not lose it.
func (w *Warden) record(ctx context.Context, obs Observation, now int64) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()

	pubkey := obs.Identity.Pubkey
	if pubkey == "" && w.towerID.Load() == 0 {
		// A tower that has not answered yet has no identity to key on. Nothing to
		// record, and not an error: it is the ordinary state while it starts up.
		return nil
	}
	if w.towerID.Load() == 0 {
		id, _, err := w.store.UpsertTower(writeCtx, store.Tower{
			Kind: w.kind, Pubkey: pubkey, URI: w.uriOf(obs),
			Managed: w.managed, FirstSeenAt: now, UpdatedAt: now,
		})
		if err != nil {
			return err
		}
		w.towerID.Store(id)
		// Read back when this tower was *first* seen, rather than assuming it was
		// now. A tower already in the database has been known since long before
		// this process started, and taking `now` would restart the registration
		// grace period on every restart — suppressing real problems for ten
		// minutes each time the daemon comes back, which is exactly when somebody
		// is most likely to be watching.
		w.firstSeen = w.firstSeenOf(writeCtx, id, now)
	} else if pubkey != "" {
		if _, _, err := w.store.UpsertTower(writeCtx, store.Tower{
			Kind: w.kind, Pubkey: pubkey, URI: w.uriOf(obs),
			Managed: w.managed, FirstSeenAt: w.firstSeen, UpdatedAt: now,
		}); err != nil {
			return err
		}
	}

	return w.store.SetTowerStatus(writeCtx, w.towerID.Load(), obs.Health, now)
}

// firstSeenOf reads back when this tower was first recorded, falling back to now
// if it cannot be read — which errs towards patience rather than towards
// complaining about a tower nobody has had a chance to register with.
func (w *Warden) firstSeenOf(ctx context.Context, id, now int64) int64 {
	rows, err := w.store.ListTowers(ctx, store.TowerFilter{})
	if err != nil {
		return now
	}
	for _, t := range rows {
		if t.ID == id && t.FirstSeenAt > 0 {
			return t.FirstSeenAt
		}
	}
	return now
}

// uriOf prefers the address the tower reports over the one configured, because
// the tower knows where it actually ended up and a Tor address is published by
// the tower rather than written down in advance.
// uriOf is the address a user should paste into their own node.
//
// **What the tower reports wins, unless it has resolved a name into an
// address.** Those are two different situations and the earlier version of this
// only handled one of them.
//
// A Tor onion is created by the tower and cannot be written down in advance, so
// asking it is the only way to know — that is the ordinary case and it still
// takes precedence.
//
// But LND resolves `watchtower.externalip` at startup and advertises the
// result. Configured with a hostname, it reports back an address, and on a
// container platform that address comes from a pool and changes when the
// container is rebuilt. Measured on StartOS 0.4.0.1: configured
// `forktower.startos:9911`, advertised `10.0.3.76:9911`. Somebody who pasted
// the second would have a registration that silently stopped working, with the
// tower still healthy and nothing anywhere saying why.
//
// So a bare address loses to a name we configured, and nothing else changes.
func (w *Warden) uriOf(obs Observation) string {
	reported := ""
	if len(obs.Identity.URIs) > 0 {
		reported = obs.Identity.URIs[0]
	}
	if reported == "" {
		return w.uri
	}
	if w.uri != "" && isBareAddress(reported) && !isBareAddress(w.uri) {
		if pubkey := obs.Identity.Pubkey; pubkey != "" {
			return pubkey + "@" + w.uri
		}
		return w.uri
	}
	return reported
}

// isBareAddress reports whether a tower address names a host by number.
//
// A number is a fact about where something is now. A name is a fact about what
// it is, and only one of those survives a container being rebuilt.
func isBareAddress(uri string) bool {
	hostPort := uri
	if _, after, found := strings.Cut(uri, "@"); found {
		hostPort = after
	}
	host := hostPort
	if h, _, err := net.SplitHostPort(hostPort); err == nil {
		host = h
	}
	return net.ParseIP(strings.TrimSpace(host)) != nil
}

// announceHealth publishes a change, and only a change.
func (w *Warden) announceHealth(obs Observation) {
	if obs.Health.Status == w.lastStatus {
		return
	}
	previous := w.lastStatus
	w.lastStatus = obs.Health.Status
	if previous == "" {
		previous = store.TowerStatusUnknown
	}
	w.bus.Publish(bus.TowerHealthChanged{
		TowerID: w.towerID.Load(), TowerKind: string(w.kind),
		Pubkey: obs.Identity.Pubkey, Managed: w.managed,
		Status: string(obs.Health.Status), Detail: obs.Health.Detail,
		Previous: string(previous),
	})
}

// checkCoverage works out what is protected and records it.
//
// The second return says whether it got far enough to have an opinion. Several
// of the early exits here are "ask again shortly" rather than "nothing is
// wrong" — the tower's RPC still starting up is the common one — and a caller
// that read an empty list as all-clear would announce that every concern had
// been fixed, then raise them all again on the next pass.
func (w *Warden) checkCoverage(
	ctx context.Context, obs Observation, now int64,
) (_ []Concern, settled []ConcernKind, assessed bool) {
	if w.towerID.Load() == 0 {
		return nil, nil, false
	}
	if w.kind == store.TowerTeos {
		concerns, ok := w.checkTeosCoverage(ctx, now)
		return concerns, nil, ok
	}
	if w.client == nil {
		return nil, nil, false
	}
	// Built on the first pass that learns the tower's own identity, and rebuilt
	// when the registration age moves. Not at construction: the pubkey and the
	// version both come from the tower, and a monitor built before it answered
	// would be one that could not recognise it.
	if obs.Identity.Pubkey == "" {
		return nil, nil, false
	}
	monitor, err := NewMonitor(MonitorOptions{
		Client: w.client, TowerID: w.towerID.Load(),
		TowerPubkey:          obs.Identity.Pubkey,
		TowerVersion:         ParseVersion(obs.Chain.Version),
		RegisteredForSeconds: now - w.firstSeen,
		LowFeeSatPerVByte:    w.lowFeeSatPerVByte,
		// The tower's own condition decides whether a missing session is the
		// user's business at all. A tower whose node is still catching up accepts
		// nothing, and the user's node is already dialling it on a retry.
		TowerServing:       obs.Health.Status == store.TowerReachable,
		TowerNotServingWhy: obs.Health.Detail,
		// The address to hand back when telling somebody to re-register. Asking
		// for that without supplying it is most of the way to asking nothing.
		TowerURI:       w.uriOf(obs),
		CanAttachOnion: w.canAttachOnion,
	})
	if err != nil {
		w.log.Error("setting up the coverage check", slog.String("error", err.Error()))
		return nil, nil, false
	}
	w.monitor = monitor

	channels, listErr := w.store.ListChannels(ctx, store.ChannelFilter{OpenOnly: true})
	if listErr != nil {
		w.log.Error("reading channels to check their protection",
			slog.String("error", listErr.Error()))
		return nil, nil, false
	}
	if len(channels) == 0 {
		// Nothing to have an opinion about. Not an all-clear: with no channels
		// there is no way to tell whether the node's client is switched on, and
		// saying so would be a guess dressed as good news.
		return nil, nil, false
	}

	pass, err := w.monitor.Check(ctx, channels, w.previous, now)
	if err != nil {
		w.log.Error("reading what your node is backing up", slog.String("error", err.Error()))
		return nil, nil, false
	}

	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	for _, c := range pass.Coverage {
		if err := w.store.UpsertCoverage(writeCtx, c); err != nil {
			w.log.Error("recording what this tower protects",
				slog.String("error", err.Error()))
			return nil, nil, false
		}
	}

	pass.Concerns = append(pass.Concerns, w.monitor.StalledBackups(
		pass.Coverage, w.previous, advancedSince(w.previous, pass.Coverage))...)
	w.previous = pass.Coverage
	return pass.Concerns, pass.Settled, true
}

// checkTeosCoverage is the Core Lightning arm.
//
// **Coverage is all-or-nothing here, and that is a fact about teos rather than a
// simplification.** Core Lightning builds the penalty transaction itself and
// hands the tower an opaque blob, so a teos tower never sees a channel type and
// there is no per-channel session to hold or to lack. What decides whether a
// channel is protected is whether the *subscription* is alive and has room —
// which is the same answer for every channel at once.
func (w *Warden) checkTeosCoverage(ctx context.Context, now int64) (_ []Concern, assessed bool) {
	if w.clnClient == nil {
		return nil, false
	}

	channels, err := w.store.ListChannels(ctx, store.ChannelFilter{OpenOnly: true})
	if err != nil {
		w.log.Error("reading channels to check their protection",
			slog.String("error", err.Error()))
		return nil, false
	}

	monitor, err := NewTeosMonitor(TeosMonitorOptions{
		Client: w.clnClient, TowerID: w.towerID.Load(),
		TowerPubkey:  w.teosPubkey,
		SlotsAtStart: w.teosSlotsAtStart,
	})
	if err != nil {
		w.log.Error("setting up the coverage check", slog.String("error", err.Error()))
		return nil, false
	}
	w.teos = monitor

	pass, err := monitor.Check(ctx, w.tipHeight)
	if err != nil {
		w.log.Error("reading what your node is backing up", slog.String("error", err.Error()))
		return nil, false
	}

	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()

	// The tower's condition comes from the node's own view of it, which is richer
	// than anything the tower's public interface can say: it knows whether
	// appointments are being accepted, when the subscription lapses, and whether
	// the tower has provably misbehaved.
	if pass.Found {
		if err := w.store.SetTowerStatus(writeCtx, w.towerID.Load(), pass.Health, now); err != nil {
			w.log.Error("recording the tower's condition", slog.String("error", err.Error()))
		}
	}

	protected := pass.Found && pass.PluginLoaded &&
		pass.Health.Status == store.TowerReachable
	reason := "your node holds a live subscription with this tower, which covers " +
		"every channel it has"
	if !protected {
		reason = teosGapReason(pass)
	}

	for _, ch := range channels {
		cover := store.Coverage{
			ChannelID: ch.ID, TowerID: w.towerID.Load(),
			Coverable: protected, Reason: reason, CheckedAt: now,
		}
		if err := w.store.UpsertCoverage(writeCtx, cover); err != nil {
			w.log.Error("recording what this tower protects",
				slog.String("error", err.Error()))
			return pass.Concerns, false
		}
	}
	return pass.Concerns, true
}

// teosGapReason says why a Core Lightning channel is not covered.
func teosGapReason(pass TeosPass) string {
	switch {
	case !pass.PluginLoaded:
		return "your node is not running the watchtower plugin, so nothing is " +
			"being backed up to any tower"
	case !pass.Found:
		return "your node has not registered with this tower"
	case pass.Health.Status == store.TowerSubscriptionError:
		return "the subscription with this tower has run out, so it is no longer " +
			"accepting backups"
	case pass.Health.Status == store.TowerMisbehaving:
		return "this tower returned a receipt whose signature does not check out"
	default:
		return "this tower is not currently accepting backups from your node"
	}
}

// advancedSince is which channels moved to a new state between two passes.
//
// Approximated from the coverage rows themselves rather than asked of the
// registry: a channel whose last-backup time moved has plainly been active.
// Deliberately conservative — it under-reports rather than over-reports, so a
// stalled backup is only ever claimed about a channel we have positive evidence
// was doing something.
func advancedSince(previous, current []store.Coverage) map[int64]bool {
	was := make(map[int64]store.Coverage, len(previous))
	for _, c := range previous {
		was[c.ChannelID] = c
	}
	out := map[int64]bool{}
	for _, c := range current {
		prior, ok := was[c.ChannelID]
		if ok && c.LastBackupAt > prior.LastBackupAt {
			out[c.ChannelID] = true
		}
	}
	return out
}

// raise publishes a concern, unless it is the same one as last time.
//
// A standing problem said every minute is a problem nobody reads. It is said
// again if it goes away and comes back, because that is news.
func (w *Warden) raise(c Concern) {
	key := fmt.Sprintf("%s:%d", c.Kind, c.ChannelID)
	// Raising it again means the next settling is news again.
	delete(w.settledSaid, c.Kind)
	if w.lastConcerns[key] {
		return
	}
	w.lastConcerns[key] = true
	w.bus.Publish(bus.TowerConcern{
		TowerID: w.towerID.Load(), Concern: string(c.Kind),
		ChannelID: c.ChannelID, Message: c.Message,
	})
}

// forgetResolved clears the memory of concerns that no longer apply, so that one
// recurring is announced again — and says that each has passed.
//
// **It used to forget in silence.** The warning it had raised stayed on the
// dashboard with nothing beside it, so the one thing a user does here — go and
// change a setting on their own node — had no visible outcome. They came back to
// the same sentence and no way to tell whether it was current or a record of
// something already dealt with.
func (w *Warden) forgetResolved(current []Concern, settled []ConcernKind) {
	still := make(map[string]bool, len(current))
	for _, c := range current {
		still[fmt.Sprintf("%s:%d", c.Kind, c.ChannelID)] = true
	}

	// **Positively settled, so declared rather than diffed against memory.** The
	// loop below withdraws only what *this process* remembers raising, which
	// means a restart between a fault and its repair strands the warning forever
	// with nothing able to retire it. These are the ones the pass proved do not
	// apply, so they are announced whether or not memory knows about them; the
	// alert layer drops the announcement when nothing is standing — see
	// Candidate.OnlyIfStanding.
	//
	// Only what was proven. "Absent" is not "settled": a tower still starting and
	// a grace period still running both look like absence, and retiring a real
	// warning on that basis is the failure this exists to avoid.
	for _, kind := range settled {
		key := fmt.Sprintf("%s:%d", kind, 0)
		delete(w.lastConcerns, key)
		// Once per start, not once a minute. A restart is the only way a standing
		// warning can be stranded, so declaring at the first opportunity after one
		// is enough — and repeating it every pass would be a bus event a minute
		// saying nothing has happened.
		if w.settledSaid[kind] {
			continue
		}
		w.settledSaid[kind] = true
		w.bus.Publish(bus.TowerConcern{
			TowerID: w.towerID.Load(), Concern: string(kind), Cleared: true,
		})
	}

	for key := range w.lastConcerns {
		if still[key] {
			continue
		}
		kind, channelID := splitConcernKey(key)
		delete(w.lastConcerns, key)
		w.bus.Publish(bus.TowerConcern{
			TowerID: w.towerID.Load(), Concern: kind,
			ChannelID: channelID, Cleared: true,
		})
	}
}

// splitConcernKey undoes the key raise builds, so a cleared concern can be
// announced as the same kind it was raised as.
func splitConcernKey(key string) (kind string, channelID int64) {
	at := strings.LastIndex(key, ":")
	if at < 0 {
		return key, 0
	}
	id, err := strconv.ParseInt(key[at+1:], 10, 64)
	if err != nil {
		return key, 0
	}
	return key[:at], id
}

// TowerID is the row this warden is keeping up to date. Zero until the tower has
// answered once.
func (w *Warden) TowerID() int64 { return w.towerID.Load() }

// discardHandler drops log records, for a warden built without a logger.
type discardHandler struct{ slog.Handler }

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (d discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return d }
func (d discardHandler) WithGroup(string) slog.Handler           { return d }
