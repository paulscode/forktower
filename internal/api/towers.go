package api

import (
	"fmt"
	"net/http"

	"github.com/paulscode/forktower/internal/store"
	"github.com/paulscode/forktower/internal/words"
)

// Tower is a watchtower as the dashboard sees it.
type Tower struct {
	ID     int64  `json:"id"`
	Kind   string `json:"kind"`
	Pubkey string `json:"pubkey"`
	// URI is what a user pastes into their node to register. The single most
	// important string on this page.
	URI string `json:"uri,omitempty"`
	// Managed distinguishes a tower this installation runs from one the user
	// pointed us at. It decides what may honestly be promised about it.
	Managed bool   `json:"managed"`
	Status  string `json:"status"`
	Detail  string `json:"detail,omitempty"`
	// LastOKAt is when it last answered. Zero means it never has, which is a
	// different fact from having answered once and stopped.
	LastOKAt int64 `json:"last_ok_at,omitempty"`

	// SubscriptionExpiryHeight and SubscriptionSlotsRemaining are teos only.
	// Absent for an LND tower, which has no subscription to expire.
	SubscriptionExpiryHeight   *int32 `json:"subscription_expiry_height,omitempty"`
	SubscriptionSlotsRemaining *int32 `json:"subscription_slots_remaining,omitempty"`

	Display  TowerDisplay    `json:"display"`
	Coverage []TowerCoverage `json:"coverage"`
}

// TowerDisplay is the tower's condition in words.
type TowerDisplay struct {
	// State is one of "protecting", "not protecting", "settling", "unknown" —
	// what the dashboard colours on.
	State string `json:"state"`
	// Summary is one sentence a person can act on.
	Summary string `json:"summary"`
	// Covered and Uncovered count the channels either way, so the card can lead
	// with the number that matters.
	Covered   int `json:"covered"`
	Uncovered int `json:"uncovered"`
}

// TowerCoverage is whether one tower protects one channel.
type TowerCoverage struct {
	ChannelID int64 `json:"channel_id"`
	Coverable bool  `json:"coverable"`
	// Reason is present either way: a refusal without one is an accusation with
	// no evidence, and a "yes" without one gives a reader nothing to check.
	Reason string `json:"reason"`
	// Backups is **not a per-channel figure** — no Lightning node reports one.
	// It is the states held on the sessions of this channel's type. The wire
	// name says so, so a consumer cannot mistake it.
	BackupsForThisChannelType int64 `json:"backups_for_this_channel_type"`
	LastBackupAt              int64 `json:"last_backup_at,omitempty"`
	// SweepFeeSatPerVByte is the rate signed into every justice transaction this
	// session holds, converted from the sat/kW it is stored in. Nobody can raise
	// it afterwards, which is why it is shown at all.
	SweepFeeSatPerVByte *int32 `json:"sweep_fee_sat_per_vbyte,omitempty"`
	CheckedAt           int64  `json:"checked_at"`
}

// satPerKWToSatPerVByte converts back for display. A weight unit is a quarter of
// a virtual byte, so a rate per kilo-weight is 250 times the rate per vB.
const satPerKWToSatPerVByte = 250

func (s *Server) handleTowers(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListTowers(r.Context(), store.TowerFilter{})
	if err != nil {
		s.fail(w, r, "reading the watchtowers", err)
		return
	}

	out := make([]Tower, 0, len(rows))
	for _, t := range rows {
		coverage, covErr := s.store.ListCoverage(r.Context(),
			store.CoverageFilter{TowerID: t.ID})
		if covErr != nil {
			s.fail(w, r, "reading what the watchtowers protect", covErr)
			return
		}
		out = append(out, towerView(t, coverage))
	}

	writeData(w, map[string]any{"towers": out})
}

// towerView turns a stored tower into what the dashboard renders.
func towerView(t store.Tower, coverage []store.Coverage) Tower {
	view := Tower{
		ID: t.ID, Kind: string(t.Kind), Pubkey: t.Pubkey, URI: t.URI,
		Managed: t.Managed, Status: string(t.Status), Detail: t.StatusDetail,
		LastOKAt:                   t.LastOKAt,
		SubscriptionExpiryHeight:   t.SubscriptionExpiryHeight,
		SubscriptionSlotsRemaining: t.SubscriptionSlotsRemaining,
		Coverage:                   make([]TowerCoverage, 0, len(coverage)),
	}

	var covered, uncovered int
	for _, c := range coverage {
		if c.Coverable {
			covered++
		} else {
			uncovered++
		}
		row := TowerCoverage{
			ChannelID: c.ChannelID, Coverable: c.Coverable, Reason: c.Reason,
			BackupsForThisChannelType: c.NumBackups,
			LastBackupAt:              c.LastBackupAt,
			CheckedAt:                 c.CheckedAt,
		}
		if c.SweepFeeSatPerKW != nil {
			rate := *c.SweepFeeSatPerKW / satPerKWToSatPerVByte
			row.SweepFeeSatPerVByte = &rate
		}
		view.Coverage = append(view.Coverage, row)
	}

	view.Display = towerDisplay(t, covered, uncovered)
	view.Display.Covered, view.Display.Uncovered = covered, uncovered
	return view
}

// The states the dashboard colours on.
const (
	towerStateProtecting = "protecting"
	towerStateNot        = "not protecting"
	towerStateSettling   = "settling"
	towerStateUnknown    = "unknown"
)

// towerDisplay says how a tower is doing, in a sentence.
//
// **A tower that is reachable is not the same as a tower that is protecting
// anything**, and the summary must not let those read alike. A perfectly healthy
// watchtower with no sessions is the exact shape of the failure this whole arm
// exists to catch, so it gets the same "not protecting" state as one that is
// down — with a different sentence, because the remedy is different.
func towerDisplay(t store.Tower, covered, uncovered int) TowerDisplay {
	switch t.Status {
	case store.TowerStatusUnknown:
		return TowerDisplay{
			State:   towerStateUnknown,
			Summary: "Forktower has not managed to ask this tower how it is doing yet.",
		}
	case store.TowerUnreachable:
		return TowerDisplay{State: towerStateNot, Summary: unreachableSummary(t)}
	case store.TowerTemporarilyUnreachable:
		return TowerDisplay{State: towerStateSettling, Summary: settlingSummary(t)}
	case store.TowerSubscriptionError:
		return TowerDisplay{
			State: towerStateNot,
			Summary: "This tower is no longer accepting backups: the subscription " +
				"has run out. Re-register with it to start a new one.",
		}
	case store.TowerMisbehaving:
		return TowerDisplay{
			State: towerStateNot,
			Summary: "This tower returned a receipt that does not check out, which " +
				"is proof it is not behaving as it should. Do not rely on it.",
		}
	case store.TowerReachable:
		return reachableDisplay(covered, uncovered)
	default:
		return TowerDisplay{State: towerStateUnknown, Summary: t.StatusDetail}
	}
}

func unreachableSummary(t store.Tower) string {
	if t.StatusDetail != "" {
		return "This tower is not protecting anything: " + t.StatusDetail
	}
	if t.Managed {
		return "This tower is not answering, so nothing is being backed up to it."
	}
	return "This tower is not answering. It is not one Forktower runs, so it " +
		"cannot be restarted from here."
}

func settlingSummary(t store.Tower) string {
	if t.StatusDetail != "" {
		return t.StatusDetail
	}
	return "This tower is starting up."
}

// reachableDisplay is the interesting case: answering, and possibly protecting
// nothing at all.
func reachableDisplay(covered, uncovered int) TowerDisplay {
	switch {
	case covered == 0 && uncovered == 0:
		return TowerDisplay{
			State: towerStateSettling,
			Summary: "This tower is running and ready. Nothing is registered with " +
				"it yet — follow the steps below to point your Lightning node at it.",
		}
	case covered == 0:
		return TowerDisplay{
			State: towerStateNot,
			Summary: "This tower is running, but none of your channels are backed " +
				"up to it. A tower that is reachable and receiving nothing protects " +
				"nothing.",
		}
	case uncovered > 0:
		return TowerDisplay{
			State: towerStateNot,
			Summary: fmt.Sprintf(
				"%d %s not protected by this tower. Open the details below to see why.",
				uncovered, plural(uncovered, "channel is", "channels are")),
		}
	default:
		return TowerDisplay{
			State: towerStateProtecting,
			Summary: fmt.Sprintf(
				"This tower is watching %s for a revoked commitment against %d of "+
					"your channels.", words.OtherChain, covered),
		}
	}
}
