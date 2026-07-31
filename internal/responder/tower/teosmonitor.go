package tower

import (
	"context"
	"errors"
	"fmt"

	"github.com/paulscode/forktower/internal/store"
)

// Concerns particular to the Core Lightning arm.
const (
	// ConcernPluginMissing means the node is not running the watchtower plugin,
	// so nothing is being backed up to any tower.
	ConcernPluginMissing ConcernKind = "tower.plugin_missing"
	// ConcernSubscriptionExpiring means the subscription is close to lapsing.
	// **No LND equivalent**, and the sharpest edge on this arm: a split can
	// outlast a subscription registered just before it.
	ConcernSubscriptionExpiring ConcernKind = "tower.subscription_expiring"
	// ConcernSlotsLow means the subscription is nearly out of appointment slots.
	ConcernSlotsLow ConcernKind = "tower.slots_low"
	// ConcernAppointmentsUndelivered means states have been revoked whose
	// appointments never reached the tower. Core Lightning does not wait for the
	// hook, so this happens silently.
	ConcernAppointmentsUndelivered ConcernKind = "tower.appointments_undelivered"
	// ConcernAppointmentsInvalid means the tower rejected appointments as
	// malformed, which points at the node rather than at the tower.
	ConcernAppointmentsInvalid ConcernKind = "tower.appointments_invalid"
	// ConcernTowerMisbehaving means the tower returned a receipt whose signature
	// does not check out, and it comes with the proof.
	ConcernTowerMisbehaving ConcernKind = "tower.misbehaving"
)

// SubscriptionWarningBlocks is how far ahead of expiry to start saying so.
//
// A thousand blocks — roughly a week at ordinary cadence, and longer on a
// minority branch where blocks are further apart, which is exactly the situation
// this arm exists for. Chosen to leave time to re-register unhurriedly rather
// than to notify somebody that it has already happened.
const SubscriptionWarningBlocks = 1000

// SlotsLowFraction is the share of a subscription's slots remaining at which it
// is worth mentioning.
const SlotsLowFraction = 0.10

// TeosMonitor watches what one Core Lightning node is backing up to one teos
// tower.
//
// Deliberately not the same type as [Monitor]. The two arms answer different
// questions from different evidence — LND's coverage is per channel and turns on
// session types, and teos's turns on a subscription that expires — and a single
// type serving both would be one with two disjoint halves and a flag.
type TeosMonitor struct {
	client CLNTowerReader
	// towerPubkey identifies our tower among whatever else the node has
	// registered with. Empty means we were not told, in which case a single
	// registered tower is taken as ours and several are reported as ambiguous
	// rather than guessed between.
	towerPubkey string
	towerID     int64
	// slotsAtStart is the subscription size, used to judge whether the slots
	// remaining are low. Zero means unknown, and then only exhaustion is
	// reported rather than a fraction of an unknown whole.
	slotsAtStart int32
}

// TeosMonitorOptions configures a TeosMonitor.
type TeosMonitorOptions struct {
	Client       CLNTowerReader
	TowerID      int64
	TowerPubkey  string
	SlotsAtStart int32
}

// NewTeosMonitor builds a TeosMonitor.
func NewTeosMonitor(opts TeosMonitorOptions) (*TeosMonitor, error) {
	if opts.Client == nil {
		return nil, errors.New("tower: a teos monitor needs a node to read")
	}
	return &TeosMonitor{
		client:       opts.Client,
		towerPubkey:  opts.TowerPubkey,
		towerID:      opts.TowerID,
		slotsAtStart: opts.SlotsAtStart,
	}, nil
}

// TeosPass is one round of checking.
type TeosPass struct {
	Concerns []Concern
	// Health is what to record about the tower, from the node's point of view —
	// which is a better view than the tower's own API can give, because it is the
	// one that knows whether appointments are being accepted.
	Health store.TowerHealth
	// Found is whether our tower was among those registered.
	Found bool
	// PluginLoaded is false when the node has no watchtower plugin at all, in
	// which case nothing is backed up anywhere and the rest is empty.
	PluginLoaded bool
}

// Check reads the node's plugin and works out what it means.
//
// `tipHeight` is the current height of the chain the tower watches, which is
// what an expiry height has to be measured against.
func (m *TeosMonitor) Check(ctx context.Context, tipHeight int32) (TeosPass, error) {
	pass := TeosPass{PluginLoaded: true}

	towers, err := m.client.Towers(ctx)
	switch {
	case errors.Is(err, ErrPluginNotLoaded):
		pass.PluginLoaded = false
		pass.Concerns = append(pass.Concerns, Concern{
			Kind: ConcernPluginMissing, TowerID: m.towerID,
			Message: "your Lightning node is not running the watchtower plugin, so " +
				"none of your channels are being backed up to any tower. This is a " +
				"plugin on your node and Forktower cannot install it for you.",
		})
		return pass, nil
	case err != nil:
		return TeosPass{}, fmt.Errorf("reading what your node is backing up: %w", err)
	}

	ours, found := m.pick(towers)
	pass.Found = found
	if !found {
		pass.Concerns = append(pass.Concerns, Concern{
			Kind: ConcernNotRegistered, TowerID: m.towerID,
			Message: "your Lightning node has not registered with this tower, so " +
				"nothing is being backed up to it.",
		})
		return pass, nil
	}

	pass.Health = m.health(*ours)
	pass.Concerns = append(pass.Concerns, m.concerns(*ours, tipHeight)...)
	return pass, nil
}

// pick finds our tower among those the node knows.
//
// With no configured identity, a single registered tower is taken as ours —
// which is the ordinary case and saves the user copying a key out of a log. More
// than one is reported rather than guessed between: attributing another tower's
// health to ours would be worse than saying we cannot tell.
func (m *TeosMonitor) pick(towers []TeosTower) (*TeosTower, bool) {
	if m.towerPubkey != "" {
		for i := range towers {
			if towers[i].ID == m.towerPubkey {
				return &towers[i], true
			}
		}
		return nil, false
	}
	if len(towers) == 1 {
		return &towers[0], true
	}
	return nil, false
}

// health turns the plugin's view of a tower into what to record.
func (m *TeosMonitor) health(t TeosTower) store.TowerHealth {
	expiry, slots := t.SubscriptionExpiry, t.AvailableSlots
	health := store.TowerHealth{
		Status:       t.Status,
		ExpiryHeight: &expiry,
		SlotsLeft:    &slots,
	}
	switch t.Status {
	case store.TowerMisbehaving:
		health.Detail = "this tower returned a receipt whose signature does not " +
			"check out, which is proof rather than suspicion"
	case store.TowerSubscriptionError:
		health.Detail = "this tower is refusing appointments: the subscription has " +
			"run out of slots or expired"
	case store.TowerTemporarilyUnreachable:
		health.Detail = "this tower is not answering, and your node is still retrying"
	case store.TowerUnreachable:
		health.Detail = "this tower is not answering and your node has stopped retrying"
	case store.TowerReachable, store.TowerStatusUnknown:
		// Nothing to add. The counts and the expiry carry the story.
	}
	return health
}

// concerns is everything worth raising about one tower.
func (m *TeosMonitor) concerns(t TeosTower, tipHeight int32) []Concern {
	var out []Concern

	if t.Status == store.TowerMisbehaving {
		out = append(out, Concern{
			Kind: ConcernTowerMisbehaving, TowerID: m.towerID,
			Message: "this tower returned a receipt whose signature does not check " +
				"out. That is proof it is not behaving as it should, not a guess — " +
				"the evidence is recorded. Do not rely on this tower; register with " +
				"another. " + proofNote(t.MisbehavingProof),
		})
	}

	// **The signal with no LND equivalent.** A subscription lapses on a block
	// height, and a split can easily outlast one registered just before it.
	if t.SubscriptionExpiry > 0 && tipHeight > 0 {
		left := t.SubscriptionExpiry - tipHeight
		switch {
		case left <= 0:
			out = append(out, Concern{
				Kind: ConcernSubscriptionExpiring, TowerID: m.towerID,
				Message: "your subscription with this tower has expired, so it is no " +
					"longer accepting backups. Re-register with it to start a new one.",
			})
		case left <= SubscriptionWarningBlocks:
			out = append(out, Concern{
				Kind: ConcernSubscriptionExpiring, TowerID: m.towerID,
				Message: fmt.Sprintf(
					"your subscription with this tower runs out in about %d blocks. "+
						"Re-register before then: once it lapses the tower stops "+
						"accepting backups, and on a slow chain that can arrive sooner "+
						"in blocks than it sounds in days.", left),
			})
		}
	}

	if t.AvailableSlots == 0 {
		out = append(out, Concern{
			Kind: ConcernSlotsLow, TowerID: m.towerID,
			Message: "this tower has no room left for new backups. Re-register with " +
				"it to get a fresh allowance.",
		})
	} else if m.slotsAtStart > 0 &&
		float64(t.AvailableSlots) <= float64(m.slotsAtStart)*SlotsLowFraction {
		out = append(out, Concern{
			Kind: ConcernSlotsLow, TowerID: m.towerID,
			Message: fmt.Sprintf(
				"this tower has room for %d more backups out of the %d it started "+
					"with. Re-register before it fills up.",
				t.AvailableSlots, m.slotsAtStart),
		})
	}

	// Core Lightning revokes a state and carries on without waiting for the
	// plugin, so a queue that is not draining is protection quietly not
	// happening — and nothing else in the system would ever mention it.
	if t.PendingAppointments > 0 {
		out = append(out, Concern{
			Kind: ConcernAppointmentsUndelivered, TowerID: m.towerID,
			Message: fmt.Sprintf(
				"%d channel updates have not reached this tower yet. Your node moves "+
					"on without waiting for the tower to confirm, so those states are "+
					"in use and not yet protected.", t.PendingAppointments),
		})
	}

	if t.InvalidAppointments > 0 {
		out = append(out, Concern{
			Kind: ConcernAppointmentsInvalid, TowerID: m.towerID,
			Message: fmt.Sprintf(
				"this tower rejected %d backups as malformed. That points at your "+
					"node rather than at the tower, and those states are not protected.",
				t.InvalidAppointments),
		})
	}

	return out
}

// proofNote says whether the evidence was captured, without pasting it into a
// sentence somebody has to read on a phone.
func proofNote(proof string) string {
	if proof == "" {
		return "The proof itself could not be fetched from your node."
	}
	return "The proof is in the details for this tower."
}
