package api

import (
	"context"
	"net/http"

	"github.com/paulscode/forktower/internal/bootstrap"
)

// Bootstrap is what the API needs from the snapshot shortcut.
//
// An interface so the handlers can be exercised against a scripted state. Nil
// when the shortcut is switched off, which every reader here must handle: it is
// the ordinary case rather than an error.
type Bootstrap interface {
	State() bootstrap.State
	Start(ctx context.Context) error
	Cancel(ctx context.Context) error
}

// BootstrapView is the snapshot shortcut as the dashboard sees it.
//
// Deliberately free of jargon. "assumeutxo", "UTXO set" and "chainstate" are the
// right words and the wrong ones: the person reading this installed an app to
// protect their Lightning channels, and what they need to know is that there is a
// faster way to start and what it costs.
type BootstrapView struct {
	// Available is false when the shortcut is switched off entirely. Everything
	// else is meaningless when it is false, and the dashboard shows nothing.
	Available bool `json:"available"`

	// Phase is one of bootstrap's phases, for the dashboard to render from.
	Phase string `json:"phase"`

	// Title and Detail are the card's own words.
	Title  string `json:"title"`
	Detail string `json:"detail"`

	// Why explains the trade being offered, and is shown before anybody agrees to
	// it rather than afterwards.
	Why []string `json:"why,omitempty"`

	// Percent, BytesDone and BytesTotal drive the progress bar.
	Percent    float64 `json:"percent"`
	BytesDone  int64   `json:"bytes_done"`
	BytesTotal int64   `json:"bytes_total"`
	// Human is the same progress as a sentence, for a screen reader and for
	// anybody who would rather read than measure a bar.
	Human string `json:"human,omitempty"`

	// Action is the one button, or nothing.
	Action *Action `json:"action"`

	// Error is the last failure, when there was one.
	Error string `json:"error,omitempty"`
}

// Paths the dashboard calls for the shortcut.
const (
	PathBootstrapStart  = "/api/v1/bootstrap/start"
	PathBootstrapCancel = "/api/v1/bootstrap/cancel"
)

func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	writeData(w, s.bootstrapView())
}

func (s *Server) handleBootstrapStart(w http.ResponseWriter, r *http.Request) {
	if s.bootstrap == nil {
		writeError(w, http.StatusNotFound, CodeNotFound,
			"The faster first sync is not available in this installation.")
		return
	}
	if err := s.bootstrap.Start(r.Context()); err != nil {
		writeError(w, http.StatusConflict, CodeWrongState, err.Error())
		return
	}
	writeData(w, s.bootstrapView())
}

func (s *Server) handleBootstrapCancel(w http.ResponseWriter, r *http.Request) {
	if s.bootstrap == nil {
		writeError(w, http.StatusNotFound, CodeNotFound,
			"The faster first sync is not available in this installation.")
		return
	}
	if err := s.bootstrap.Cancel(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	writeData(w, s.bootstrapView())
}

// bootstrapView turns the runner's state into the card.
func (s *Server) bootstrapView() BootstrapView {
	if s.bootstrap == nil {
		return BootstrapView{Available: false, Phase: string(bootstrap.PhaseOff)}
	}

	st := s.bootstrap.State()
	view := BootstrapView{
		Available:  true,
		Phase:      string(st.Phase),
		BytesDone:  st.Progress.BytesDone,
		BytesTotal: st.Snapshot.TotalBytes(),
		Error:      st.Error,
	}
	if view.BytesTotal > 0 {
		view.Percent = float64(view.BytesDone) / float64(view.BytesTotal) * 100
	}

	switch st.Phase {
	case bootstrap.PhaseOff:
		view.Available = false

	case bootstrap.PhaseUnavailable:
		view.Available = false
		view.Title = "A faster first sync is not needed here"
		view.Detail = st.Assessment.Reason

	case bootstrap.PhaseOffered:
		view.Title = "Start watching the other chain today, not in three days"
		// **Both figures, because the default is the slow one.** The shortcut was
		// measured at 48 minutes end to end over a direct connection, and this
		// runs through the same Tor proxy the second node peers over unless the
		// user changed that — where it is several hours. Quoting only the fast
		// number would be quoting the configuration almost nobody has.
		view.Detail = "Left to itself, the second Bitcoin node takes about three " +
			"days to catch up, and until it has, Forktower cannot see the other " +
			"chain at all. The shortcut takes a few hours through Tor, or under " +
			"an hour on a direct connection."
		view.Why = bootstrapOfferReasons(st)
		view.Action = &Action{Label: "Use the faster sync", Endpoint: PathBootstrapStart}

	case bootstrap.PhaseDownloading:
		view.Title = "Fetching the head start"
		view.Detail = "The second node keeps catching up on its own while this runs, " +
			"so nothing is being wasted if it finishes first."
		view.Human = bootstrapProgressSentence(st)
		view.Action = &Action{Label: "Stop and sync the slow way", Endpoint: PathBootstrapCancel}

	case bootstrap.PhaseLoading:
		view.Title = "Handing the head start to the second node"
		view.Detail = "This takes several minutes, and the node will not answer " +
			"anything while it reads. That is expected."
		view.Percent = 100

	case bootstrap.PhaseDone:
		view.Title = "The second node took the shortcut"
		view.Detail = "It is checking the earlier history in the background, and " +
			"watching the other chain while it does."
		view.Percent = 100

	case bootstrap.PhaseFailed:
		view.Title = "The faster sync did not finish"
		view.Detail = "Forktower will try again shortly, resuming from where it " +
			"stopped rather than starting over. The second node is still catching " +
			"up on its own in the meantime."
		view.Human = bootstrapProgressSentence(st)
		view.Action = &Action{Label: "Try again now", Endpoint: PathBootstrapStart}
	}

	return view
}

// bootstrapOfferReasons is what somebody should know before agreeing.
//
// **The download is listed first and the privacy cost second, because those are
// the two things that would make a reasonable person say no.** Putting the
// benefit first and the costs in a footnote is how consent forms are written by
// people who do not want it read.
func bootstrapOfferReasons(st bootstrap.State) []string {
	return []string{
		"Downloads about " + bootstrap.HumanBytes(st.Snapshot.TotalBytes()) +
			", which is deleted as soon as it has been used.",
		"Fetched from this project's release page. Bitcoin Core checks it against " +
			"a value built into Bitcoin Core itself, so a wrong or tampered file is " +
			"refused by your node rather than trusted.",
		"Everything below block " + bootstrap.Commas(int64(st.Snapshot.BaseHeight)) +
			" is still verified in full, in the background, after the shortcut.",
		"This is the only thing Forktower ever downloads.",
	}
}

// bootstrapProgressSentence says where the transfer has got to, in words.
func bootstrapProgressSentence(st bootstrap.State) string {
	p := st.Progress
	if p.BytesTotal <= 0 || p.BytesDone <= 0 {
		if st.StagedBytes > 0 {
			return bootstrap.HumanBytes(st.StagedBytes) + " of " +
				bootstrap.HumanBytes(st.Snapshot.TotalBytes()) + " already fetched."
		}
		return "Starting."
	}

	out := bootstrap.HumanBytes(p.BytesDone) + " of " +
		bootstrap.HumanBytes(p.BytesTotal)
	if p.Parts > 1 {
		out += ", part " + bootstrap.Commas(int64(p.Part)) + " of " +
			bootstrap.Commas(int64(p.Parts))
	}
	if remaining := bootstrap.HumanDuration(p.Remaining); remaining != "" {
		out += ", " + remaining + " to go"
	}
	return out + "."
}

// bootstrapOffered reports whether the shortcut is available and waiting to be
// accepted.
//
// Narrow on purpose: only PhaseOffered counts. A shortcut already running, or
// already taken, is not something to put a button in front of somebody for.
func (s *Server) bootstrapOffered() bool {
	if s.bootstrap == nil {
		return false
	}
	return s.bootstrap.State().Phase == bootstrap.PhaseOffered
}

// MountBootstrap attaches the snapshot shortcut.
//
// Optional wiring rather than a constructor parameter, in the same shape as
// MountUI: the shortcut is absent in most installations and in every test that
// is not about it, and threading a nil through ten positional arguments teaches
// nobody anything.
func (s *Server) MountBootstrap(b Bootstrap) { s.bootstrap = b }
