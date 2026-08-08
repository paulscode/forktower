package api

import (
	"net/http"

	"github.com/paulscode/forktower/internal/chainview"
	"github.com/paulscode/forktower/internal/redact"
	"github.com/paulscode/forktower/internal/sentinel"
	"github.com/paulscode/forktower/internal/store"
)

// Status is the whole answer to "what is going on", in one request.
//
// One endpoint rather than several because the dashboard's first job is to answer
// "am I OK?" above the fold, and assembling that from four requests means four
// chances to render a half-answer.
type Status struct {
	Headline  Headline        `json:"headline"`
	Split     SplitStatus     `json:"split"`
	Views     map[string]View `json:"views"`
	Readiness []ReadinessItem `json:"readiness"`
}

// SplitStatus describes the relationship between the two chains.
type SplitStatus struct {
	State string `json:"state"`
	// Fork is where the chains separated, once that is known.
	Fork *ForkPoint `json:"fork"`
	// ForkCandidate is where they are currently disagreeing, before that has been
	// confirmed as a split. Separate from Fork so the two can never be confused: one
	// is a finding the daemon stands behind and anchors rescans to, the other is
	// what it is presently looking at.
	ForkCandidate *ForkPoint `json:"fork_candidate,omitempty"`
	// Disagreement is the two chains' own answers at one height, side by side.
	//
	// Included because it is the one thing on this screen a user can verify without
	// trusting the daemon at all: open any block explorer at that height and see
	// which hash it shows. Everything else here asks to be believed.
	Disagreement *HeightDisagreement   `json:"disagreement,omitempty"`
	Branches     map[string]BranchInfo `json:"branches"`
}

// HeightDisagreement is what each chain says is the block at one height.
type HeightDisagreement struct {
	Height int32  `json:"height"`
	SFHash string `json:"sf_hash"`
	SQHash string `json:"sq_hash"`
	// Since is when the two were first seen to differ here.
	Since int64 `json:"since"`
}

// ForkPoint locates the separation.
type ForkPoint struct {
	Hash       string `json:"hash"`
	Height     int32  `json:"height"`
	DetectedAt int64  `json:"detected_at"`
}

// BranchInfo is what one chain looks like now.
type BranchInfo struct {
	TipHash        string `json:"tip_hash"`
	TipHeight      int32  `json:"tip_height"`
	SinceForkDepth int32  `json:"since_fork_depth"`
	// AvgIntervalSecs is what turns a countdown in blocks into a countdown in
	// time. A minority chain's blocks can be far apart, so the same number of
	// blocks can mean a very different amount of human time.
	AvgIntervalSecs float64 `json:"avg_interval_secs"`
}

// View is a backend's own report of itself.
type View struct {
	State        string  `json:"state"`
	PeerCount    int     `json:"peer_count"`
	SyncProgress float64 `json:"sync_progress"`
	Detail       string  `json:"detail"`
	// Software is the node's own version string. Raw, and shown only under
	// Advanced: it is the evidence behind "your node most likely follows these
	// rules", and evidence belongs where someone goes looking for it rather than
	// in a sentence on the front page.
	Software string `json:"software,omitempty"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	state := s.sentinel.State()
	checks := s.sentinel.Checks()
	sfView, sqView := s.sentinel.Views()
	sfIdentity, sqIdentity := s.sentinel.Identities()
	readiness := s.Readiness(ctx)

	writeData(w, Status{
		Headline: ComputeHeadline(HeadlineInput{
			Phase:           store.SplitState(state.Phase),
			DetectedAt:      state.DetectedAt,
			SFHealth:        state.SFHealth,
			SQHealth:        state.SQHealth,
			Paused:          s.sentinel.Paused(),
			PausedSince:     checks.BranchCheckedAt,
			AlertsReachable: alertsReachable(readiness),
			FailingChecks:   blockingFailures(readiness),
			// Either source counts as "the chains differ": the separation search finds
			// where, the direct comparison proves that. Reading only the first meant a
			// failed search left the screen with nothing to say.
			Diverging: state.Fork == nil &&
				(state.ForkCandidate != nil || state.Disagreement != nil),
			DivergingSince: firstOf(state.ForkCandidateSince, state.DisagreementSince),
			SplitSuspected: state.Fork == nil && state.SplitSuspected,
		}),
		Split: splitStatus(state),
		Views: map[string]View{
			string(chainview.BranchSF): viewOf(sfView, sfIdentity),
			string(chainview.BranchSQ): viewOf(sqView, sqIdentity),
		},
		Readiness: readiness,
	})
}

// alertsReachable reads the transport check's own conclusion rather than
// recomputing it, so the headline and the list beneath it cannot disagree.
func alertsReachable(items []ReadinessItem) bool {
	for _, item := range items {
		if item.ID == CheckAlertTransports {
			// An untested transport is not a broken one. Saying "no way to reach
			// you" while the first self-test is still pending would greet every new
			// install with an alarm about itself.
			return item.OK || item.informational
		}
	}
	return true
}

// firstOf returns the earliest non-zero of the two clocks, so "since" reflects
// whichever noticed the disagreement first rather than whichever is listed first.
func firstOf(a, b int64) int64 {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	case b < a:
		return b
	default:
		return a
	}
}

func splitStatus(st sentinel.State) SplitStatus {
	out := SplitStatus{
		State:    string(st.Phase),
		Branches: map[string]BranchInfo{},
	}
	if st.Fork != nil {
		out.Fork = &ForkPoint{
			Hash:       st.Fork.Hash.String(),
			Height:     st.Fork.Height,
			DetectedAt: st.DetectedAt,
		}
	}
	if st.Fork == nil && st.ForkCandidate != nil {
		out.ForkCandidate = &ForkPoint{
			Hash:       st.ForkCandidate.Hash.String(),
			Height:     st.ForkCandidate.Height,
			DetectedAt: st.ForkCandidateSince,
		}
	}
	if st.Disagreement != nil {
		out.Disagreement = &HeightDisagreement{
			Height: st.Disagreement.Height,
			SFHash: st.Disagreement.SFHash.String(),
			SQHash: st.Disagreement.SQHash.String(),
			Since:  st.DisagreementSince,
		}
	}

	// Measured against whichever separation is known. Passing only the confirmed
	// fork reported both chains as nought blocks past a separation while they were
	// visibly drifting apart, which is the one number on this screen that says how
	// far apart they have got.
	fork := st.Fork
	if fork == nil {
		fork = st.ForkCandidate
	}
	out.Branches[string(chainview.BranchSF)] = branchInfo(st.SFTip, fork, st.SFCadence)
	out.Branches[string(chainview.BranchSQ)] = branchInfo(st.SQTip, fork, st.SQCadence)
	return out
}

func branchInfo(tip *chainview.BlockMeta, fork *chainview.BlockRef, c sentinel.Cadence) BranchInfo {
	if tip == nil {
		return BranchInfo{}
	}
	info := BranchInfo{
		TipHash:         tip.Hash.String(),
		TipHeight:       tip.Height,
		AvgIntervalSecs: c.ExpectedInterval().Seconds(),
	}
	if fork != nil && tip.Height > fork.Height {
		info.SinceForkDepth = tip.Height - fork.Height
	}
	return info
}

func viewOf(h chainview.BackendHealth, id chainview.Identity) View {
	return View{
		State:        string(h.State),
		PeerCount:    h.PeerCount,
		SyncProgress: h.SyncProgress,
		// **Redacted here as well as at its source.** The origin — a chain
		// backend turning an RPC error into something a person can read —
		// already removes credentials, and this is the boundary where text
		// stops being ours and becomes something shown to somebody and pasted
		// into support threads. A second implementation of Sentinel, or a later
		// edit at the origin, must not be able to quietly undo that.
		Detail:   redact.String(h.Detail),
		Software: id.Subversion,
	}
}
