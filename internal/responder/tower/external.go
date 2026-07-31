package tower

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/paulscode/forktower/internal/bus"
	"github.com/paulscode/forktower/internal/store"
)

// ConcernExternalOnly means every tower protecting the user is somebody else's.
//
// Not a fault, and not something to be alarmed about — an external tower is a
// real tower and registering with several is good advice. It is worth *saying*,
// because what can honestly be promised about a tower nobody here runs is
// different: it cannot be restarted from this machine, its configuration is not
// ours, and if it stops the remedy is to register with another.
const ConcernExternalOnly ConcernKind = "tower.external_only"

// Scout records the watchtowers the user's own node is using.
//
// **Discovered rather than configured, and that is the point.** The node already
// knows which towers it backs up to — the user typed them in when they
// registered — so asking them to type the same list into Forktower would be
// asking twice and getting it wrong once. Anything the node reports that this
// installation did not start is recorded as somebody else's.
//
// What can be known about an external tower is exactly what the user's node
// knows: whether sessions exist, how many states have gone across, and at what
// fee rate. There is no separate API to ask, because a watchtower's own
// interface speaks the tower protocol and answers only to clients. That
// limitation is also the honest framing — "is my protection working" is a better
// question than "is that machine up", and only the node can answer it.
type Scout struct {
	store  ScoutStore
	client ClientReader
	bus    *bus.Bus
	log    *slog.Logger
	// managedPubkey is the tower this installation runs, if any. Everything else
	// the node reports is somebody else's.
	managedPubkey string
	interval      time.Duration
	now           func() time.Time

	// announced remembers what has been said, so a standing state is not repeated
	// every pass.
	announced map[string]store.TowerStatus
	// firstSeen is when each external tower was first recorded, which is what the
	// registration grace period is measured from.
	firstSeen map[string]int64
}

// ScoutStore is the storage this reads and writes.
type ScoutStore interface {
	UpsertTower(ctx context.Context, t store.Tower) (int64, bool, error)
	SetTowerStatus(ctx context.Context, id int64, h store.TowerHealth, now int64) error
	ListTowers(ctx context.Context, f store.TowerFilter) ([]store.Tower, error)
	UpsertCoverage(ctx context.Context, c store.Coverage) error
	ListChannels(ctx context.Context, f store.ChannelFilter) ([]store.Channel, error)
}

// ScoutOptions configures a Scout.
type ScoutOptions struct {
	Store  ScoutStore
	Client ClientReader
	Bus    *bus.Bus
	Log    *slog.Logger
	// ManagedPubkey is the tower this installation runs, so it is not recorded
	// twice. Empty when there is no local tower, which is a perfectly ordinary
	// deployment.
	ManagedPubkey string
	Interval      time.Duration
	Now           func() time.Time
}

// NewScout builds a Scout.
func NewScout(opts ScoutOptions) (*Scout, error) {
	if opts.Store == nil || opts.Bus == nil {
		return nil, errors.New("tower: a scout needs storage and a bus")
	}
	if opts.Client == nil {
		return nil, errors.New("tower: a scout needs a node to read")
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
	return &Scout{
		store: opts.Store, client: opts.Client, bus: opts.Bus, log: opts.Log,
		managedPubkey: opts.ManagedPubkey, interval: opts.Interval, now: opts.Now,
		announced: map[string]store.TowerStatus{},
		firstSeen: map[string]int64{},
	}, nil
}

// Run watches until the context is cancelled.
func (s *Scout) Run(ctx context.Context) error {
	s.pass(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.pass(ctx)
		}
	}
}

// pass records what the node is backing up to.
func (s *Scout) pass(ctx context.Context) {
	registered, err := s.client.Towers(ctx)
	switch {
	case errors.Is(err, ErrClientNotActive):
		// Reported by the warden, which has the whole picture. Saying it twice
		// would be two notifications for one fact.
		return
	case err != nil:
		s.log.Error("reading which watchtowers your node uses",
			slog.String("error", err.Error()))
		return
	}

	channels, err := s.store.ListChannels(ctx, store.ChannelFilter{OpenOnly: true})
	if err != nil {
		s.log.Error("reading your channels", slog.String("error", err.Error()))
		return
	}

	clientVersion, err := s.client.Version(ctx)
	if err != nil {
		s.log.Error("reading your node's version", slog.String("error", err.Error()))
		return
	}

	now := s.now().Unix()
	var external int
	for i := range registered {
		t := registered[i]
		if t.Pubkey == "" || t.Pubkey == s.managedPubkey {
			continue
		}
		external++
		s.record(ctx, t, channels, clientVersion, now)
	}

	if external > 0 && s.managedPubkey == "" {
		s.announceExternalOnly(external)
	}
}

// record files one external tower and what it protects.
func (s *Scout) record(
	ctx context.Context, t RegisteredTower, channels []store.Channel,
	clientVersion Version, now int64,
) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()

	first, seen := s.firstSeen[t.Pubkey]
	if !seen {
		first = now
	}

	id, _, err := s.store.UpsertTower(writeCtx, store.Tower{
		Kind: store.TowerLND, Pubkey: t.Pubkey, URI: addressOf(t),
		// Not ours. The dashboard says so, because what may honestly be promised
		// about somebody else's machine is different.
		Managed: false, FirstSeenAt: first, UpdatedAt: now,
	})
	if err != nil {
		s.log.Error("recording a watchtower your node uses",
			slog.String("error", err.Error()))
		return
	}
	if !seen {
		s.firstSeen[t.Pubkey] = s.firstSeenOf(writeCtx, id, now)
	}

	held := sessionsByPolicy(&t)
	present := make(map[PolicyType]bool, len(held))
	for policy := range held {
		present[policy] = true
	}

	health := store.TowerHealth{Status: store.TowerReachable}
	if len(held) == 0 {
		health.Status = store.TowerTemporarilyUnreachable
		health.Detail = "your node has registered with this tower but has not " +
			"agreed a session with it yet"
	}
	if err := s.store.SetTowerStatus(writeCtx, id, health, now); err != nil {
		s.log.Error("recording a watchtower's condition", slog.String("error", err.Error()))
		return
	}
	s.announceHealth(t.Pubkey, id, health)

	for _, ch := range channels {
		verdict := Assess(Inputs{
			ChanType: ch.ChanType,
			// **Unknown, and honestly so.** An external tower's version cannot be
			// read: there is no interface to ask. Coverage therefore rests entirely
			// on whether a session of the right kind exists, which is evidence
			// rather than inference and is the better basis anyway.
			TowerVersion:         Version{},
			ClientVersion:        clientVersion,
			SessionPolicies:      present,
			RegisteredForSeconds: now - s.firstSeen[t.Pubkey],
		})
		figures := held[verdict.Policy]
		cover := store.Coverage{
			ChannelID: ch.ID, TowerID: id,
			Coverable: verdict.Coverable, Reason: verdict.Reason,
			NumBackups: figures.backups, CheckedAt: now,
		}
		if figures.feeSatPerKW != nil {
			cover.SweepFeeSatPerKW = figures.feeSatPerKW
		}
		if err := s.store.UpsertCoverage(writeCtx, cover); err != nil {
			s.log.Error("recording what a watchtower protects",
				slog.String("error", err.Error()))
			return
		}
	}
}

// firstSeenOf reads back when a tower was first recorded, so that a restart does
// not restart its grace period.
func (s *Scout) firstSeenOf(ctx context.Context, id, now int64) int64 {
	rows, err := s.store.ListTowers(ctx, store.TowerFilter{})
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

// announceHealth publishes a change, and only a change.
func (s *Scout) announceHealth(pubkey string, id int64, health store.TowerHealth) {
	if s.announced[pubkey] == health.Status {
		return
	}
	previous := s.announced[pubkey]
	s.announced[pubkey] = health.Status
	if previous == "" {
		previous = store.TowerStatusUnknown
	}
	s.bus.Publish(bus.TowerHealthChanged{
		TowerID: id, TowerKind: string(store.TowerLND), Pubkey: pubkey,
		Managed: false, Status: string(health.Status), Detail: health.Detail,
		Previous: string(previous),
	})
}

// announceExternalOnly says once that every tower protecting the user belongs to
// somebody else.
//
// Said at all because it changes what can be done when one stops: there is no
// process here to restart and no configuration here to correct, and the remedy
// is to register with another. Said *once*, because it is a description of the
// deployment rather than a problem with it.
func (s *Scout) announceExternalOnly(count int) {
	const key = "external-only"
	if s.announced[key] != "" {
		return
	}
	s.announced[key] = store.TowerReachable

	s.bus.Publish(bus.TowerConcern{
		Concern: string(ConcernExternalOnly),
		Message: fmt.Sprintf(
			"your channels are protected by %s that Forktower does not run. That "+
				"works, and registering with more than one is sensible — but if one "+
				"stops there is nothing here to restart, and no settings here to "+
				"correct. The answer would be to register with another.",
			plainCount(count, "a watchtower", "watchtowers")),
	})
}

// plainCount reads as a person would say it.
func plainCount(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// addressOf is where a tower can be reached, as the node reports it.
func addressOf(t RegisteredTower) string {
	if len(t.Addresses) == 0 {
		return ""
	}
	return t.Pubkey + "@" + t.Addresses[0]
}
