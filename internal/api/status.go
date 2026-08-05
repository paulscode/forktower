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
	Fork     *ForkPoint            `json:"fork"`
	Branches map[string]BranchInfo `json:"branches"`
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
			PausedSince:     checks.BranchVerifiedAt,
			AlertsReachable: alertsReachable(readiness),
			FailingChecks:   blockingFailures(readiness),
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
	out.Branches[string(chainview.BranchSF)] = branchInfo(st.SFTip, st.Fork, st.SFCadence)
	out.Branches[string(chainview.BranchSQ)] = branchInfo(st.SQTip, st.Fork, st.SQCadence)
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
