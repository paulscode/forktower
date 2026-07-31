package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/paulscode/forktower/internal/deadline"
	"github.com/paulscode/forktower/internal/store"
)

// Threat states, as the dashboard understands them. Machine-readable and stable;
// the words a person reads are in the display block, never these.
const (
	ThreatNone      = "none"
	ThreatWatch     = "watch"
	ThreatMempool   = "mempool"
	ThreatConfirmed = "confirmed"
	ThreatResolved  = "resolved"
	ThreatLoss      = "loss"
)

// Channel is one of the user's channels as the dashboard sees it.
//
// Three layers on purpose. The stored fields are the record; `threat` is what
// Forktower has worked out about it; and `display` is what a person is actually
// shown. Keeping them apart is what stops an internal classification leaking
// onto the screen — the presentation layer can only say what the vocabulary
// allows, because it is the only thing the page renders.
type Channel struct {
	ID          int64  `json:"id"`
	FundingTxID string `json:"funding_txid"`
	FundingVout int32  `json:"funding_vout"`
	CapacitySat int64  `json:"capacity_sat"`
	ChanType    string `json:"chan_type"`
	PeerPubkey  string `json:"peer_pubkey"`
	PeerAlias   string `json:"peer_alias,omitempty"`
	OpenHeight  int32  `json:"open_height,omitempty"`
	SCID        string `json:"scid,omitempty"`
	CloseState  string `json:"close_state"`
	// MirrorFundingOptIn is the user's decision to copy this channel's funding
	// transaction to the other chain. The one setting here that creates exposure
	// rather than reducing it, so it is carried per channel and never inferred.
	MirrorFundingOptIn bool   `json:"mirror_funding_opt_in"`
	CloseTxID          string `json:"close_txid,omitempty"`
	CloseHeight        int32  `json:"close_height,omitempty"`

	Relevance       string `json:"relevance"`
	RelevanceReason string `json:"relevance_reason,omitempty"`
	UpdatedAt       int64  `json:"updated_at"`

	Threat  Threat         `json:"threat"`
	Display ChannelDisplay `json:"display"`
}

// Threat is what Forktower has worked out about a channel.
type Threat struct {
	State string `json:"state"`
	// HeadlineDeadline is the soonest clock running against this channel, or
	// nothing when none is.
	HeadlineDeadline *HeadlineDeadline `json:"headline_deadline"`
}

// HeadlineDeadline is the countdown a channel leads with.
type HeadlineDeadline struct {
	Kind            string `json:"kind"`
	DeadlineHeight  int32  `json:"deadline_height"`
	RemainingBlocks int32  `json:"remaining_blocks"`
	// EstWallclockSecs is the projection from the other chain's recent cadence.
	// Zero when that cadence is not known, and a reader must then say nothing
	// about time rather than assume ten minutes a block.
	EstWallclockSecs int64 `json:"est_wallclock_secs"`
}

// ChannelDisplay is the row a person reads: who, how much, how long, and what is
// being done about it.
//
// Everything here is already in the words it will be shown in. Nothing on the
// page formats a number or picks a phrase, because the moment two places decide
// how to say something they start saying it differently — and because the rule
// that no internal classification name reaches the screen is only enforceable if
// there is one place where the screen's words are chosen.
type ChannelDisplay struct {
	// Partner is the counterparty's chosen name, or a shortened key when they
	// gave none. Attacker-controlled: clamped to printable characters and 32
	// bytes when it is stored, and rendered as text and never as markup.
	Partner string `json:"partner"`
	// AtRiskSat is what could be lost, in satoshis. Integer, never a float.
	AtRiskSat int64 `json:"at_risk_sat"`
	// TimeLeft is a human duration, or empty when there is no countdown or no
	// way to project one.
	TimeLeft string `json:"time_left"`
	// TimeLeftIsEstimate is always true when TimeLeft is set. Carried explicitly
	// so the page cannot forget to say so.
	TimeLeftIsEstimate bool `json:"time_left_is_estimate"`
	// Status is one sentence in plain language, from the agreed vocabulary.
	Status string `json:"status"`
	// StatusAction is the one thing to do, or empty for "nothing — we are
	// handling it". A red row with no action is a source of anxiety rather than
	// information, so the absence is deliberate rather than accidental.
	StatusAction string `json:"status_action,omitempty"`
}

func (s *Server) handleChannels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	rows, err := s.store.ListChannels(ctx, store.ChannelFilter{})
	if err != nil {
		s.fail(w, r, "reading your channels", err)
		return
	}

	spends, err := s.store.ListSpends(ctx, store.SpendFilter{
		Branch: store.BranchSQ, Limit: store.MaxSpendLimit,
	})
	if err != nil {
		s.fail(w, r, "reading what has happened to your channels", err)
		return
	}

	deadlines, err := s.allDeadlines(ctx)
	if err != nil {
		s.fail(w, r, "reading the countdowns", err)
		return
	}

	tip, cadence := s.otherChainProgress()

	out := make([]Channel, 0, len(rows))
	for _, c := range rows {
		out = append(out, s.renderChannel(c, spends, deadlines, tip, cadence))
	}
	writeData(w, out)
}

// allDeadlines reads every deadline in every state, keyed by the spend it
// belongs to.
func (s *Server) allDeadlines(ctx context.Context) (map[int64][]store.Deadline, error) {
	out := map[int64][]store.Deadline{}
	for _, state := range []store.DeadlineState{
		store.DeadlineCounting, store.DeadlineResolved, store.DeadlineExpired,
	} {
		rows, err := s.store.ListDeadlines(ctx, state)
		if err != nil {
			return nil, err
		}
		for _, d := range rows {
			out[d.SpendEventID] = append(out[d.SpendEventID], d)
		}
	}
	return out, nil
}

// otherChainProgress is where the watched chain has got to, and how fast it is
// going.
//
// The cadence is only reported once it has been *measured*. The engine starts
// from the network's nominal ten minutes, which on a minority chain before a
// difficulty retarget is wrong by a factor of four — and a projection built on
// it would tell somebody they had four times longer than they do. Better to
// show no time at all than a confident wrong one.
func (s *Server) otherChainProgress() (tip int32, cadenceSecs float64) {
	st := s.sentinel.State()
	if st.SQTip != nil {
		tip = st.SQTip.Height
	}
	if st.SQCadence.Measured() {
		cadenceSecs = st.SQCadence.IntervalSecs
	}
	return tip, cadenceSecs
}

// renderChannel builds one row of the exposure table.
func (s *Server) renderChannel(
	c store.Channel,
	spends []store.Spend,
	deadlines map[int64][]store.Deadline,
	tip int32,
	cadenceSecs float64,
) Channel {
	out := Channel{
		ID: c.ID, FundingTxID: c.FundingTxID, FundingVout: c.FundingVout,
		CapacitySat: c.CapacitySat, ChanType: string(c.ChanType),
		PeerPubkey: c.PeerPubkey, PeerAlias: c.PeerAlias,
		OpenHeight: c.OpenHeight, SCID: c.SCID,
		CloseState: string(c.CloseState), CloseTxID: c.CloseTxID,
		MirrorFundingOptIn: c.MirrorFundingOptIn,
		CloseHeight:        c.CloseHeight,
		Relevance:          string(c.Relevance), RelevanceReason: c.RelevanceReason,
		UpdatedAt: c.UpdatedAt,
	}

	worst, headline := channelThreat(c, spends, deadlines, tip)
	out.Threat = Threat{State: worst}
	if headline != nil {
		out.Threat.HeadlineDeadline = headline
		if secs, ok := deadline.Project(headline.RemainingBlocks, cadenceSecs); ok {
			headline.EstWallclockSecs = int64(secs.Seconds())
		}
	}
	out.Display = describeChannel(c, out.Threat, cadenceSecs)
	return out
}

// channelThreat works out where a channel stands, and which clock it leads with.
//
// The worst thing that has happened wins. A channel with a resolved spend and a
// live one is in trouble, and reporting the calmer of the two would be reporting
// the wrong one.
func channelThreat(
	c store.Channel,
	spends []store.Spend,
	deadlines map[int64][]store.Deadline,
	tip int32,
) (string, *HeadlineDeadline) {
	state := ThreatNone
	if c.Relevance == store.Relevant || c.Relevance == store.RelevanceUnknown {
		// Being watched is itself a state worth showing: it is the difference
		// between "we have looked at this" and "we have not".
		state = ThreatWatch
	}

	var headline *HeadlineDeadline
	for _, sp := range spends {
		if sp.ChannelID != c.ID {
			continue
		}
		state = worseThreat(state, threatOfSpend(sp, deadlines[sp.ID]))

		for _, d := range deadlines[sp.ID] {
			if d.State != store.DeadlineCounting {
				continue
			}
			candidate := &HeadlineDeadline{
				Kind:            string(d.Kind),
				DeadlineHeight:  d.DeadlineHeight,
				RemainingBlocks: deadline.Remaining(d.DeadlineHeight, tip),
			}
			if headline == nil || candidate.DeadlineHeight < headline.DeadlineHeight {
				headline = candidate
			}
		}
	}
	return state, headline
}

// threatOfSpend is what one spend means for the channel it belongs to.
func threatOfSpend(sp store.Spend, deadlines []store.Deadline) string {
	if sp.Status == store.SpendReorgedOut {
		// It left the chain. Not resolved — it may come back, and the channel is
		// still being watched.
		return ThreatWatch
	}
	if sp.Status == store.SpendMempool {
		return ThreatMempool
	}

	switch sp.Shape {
	case store.ShapeMutualClose:
		// A close both sides agreed to. Nothing is at stake and nothing is owed.
		return ThreatResolved
	case store.ShapeJustice, store.ShapeDelayedSweep, store.ShapeHTLCClaim:
		return ThreatConfirmed
	case store.ShapeCommitmentOurs, store.ShapeCommitmentUnknown,
		store.ShapeCommitmentRevoked, store.ShapeUnknown:
	}

	worst := ThreatConfirmed
	for _, d := range deadlines {
		switch d.State {
		case store.DeadlineExpired:
			worst = worseThreat(worst, ThreatLoss)
		case store.DeadlineResolved:
			worst = worseThreat(worst, ThreatResolved)
		case store.DeadlineCounting:
			worst = worseThreat(worst, ThreatConfirmed)
		}
	}
	return worst
}

// threatRank orders the states so the worst one wins.
//
// `resolved` deliberately ranks *below* `confirmed`: a channel with one thing
// settled and another still running is not settled.
func threatRank(state string) int {
	switch state {
	case ThreatNone:
		return 0
	case ThreatResolved:
		return 1
	case ThreatWatch:
		return 2
	case ThreatMempool:
		return 3
	case ThreatConfirmed:
		return 4
	case ThreatLoss:
		return 5
	default:
		return 4
	}
}

func worseThreat(a, b string) string {
	if threatRank(b) > threatRank(a) {
		return b
	}
	return a
}

// The sentences a user reads. Written out in full here rather than assembled,
// because these are the words somebody reads when they are frightened, and a
// sentence built from fragments is a sentence nobody has read as a whole.
const (
	statusFine       = "Fine"
	statusWatching   = "Watching this one"
	statusNotExposed = "Not affected"
	statusIncoming   = "A close is about to land on the other chain"
	statusChecking   = "A channel was closed on the other chain — checking whether it was fair"
	statusHandling   = "We are handling it"
	statusSettled    = "Settled"
	statusLost       = "The time ran out"

	actionOpenDetails = "Open Details to see what happened"
)

// describeChannel turns everything known about a channel into the row a person
// reads.
//
// This is the only place the screen's words are chosen, which is what makes the
// rule enforceable: no internal classification name, no height, no hash, no
// transaction id. Those all exist on the record for the Details view, and a
// truncated hash is not an identifier to this audience — it is noise.
func describeChannel(c store.Channel, threat Threat, cadenceSecs float64) ChannelDisplay {
	out := ChannelDisplay{
		Partner:   partnerName(c),
		AtRiskSat: atRisk(c, threat.State),
	}

	if threat.HeadlineDeadline != nil {
		if projected, ok := deadline.Project(
			threat.HeadlineDeadline.RemainingBlocks, cadenceSecs); ok {
			out.TimeLeft = deadline.HumanDuration(projected)
			out.TimeLeftIsEstimate = true
		}
	}

	switch threat.State {
	case ThreatNone:
		out.Status = statusNotExposed
	case ThreatWatch:
		if c.CloseState != store.CloseOpen {
			// The exposure people do not expect: a closed channel feels finished,
			// and on the other chain it is not.
			out.Status = statusWatching
			out.StatusAction = actionOpenDetails
			return out
		}
		out.Status = statusFine
	case ThreatMempool:
		out.Status = statusIncoming
		out.StatusAction = actionOpenDetails
	case ThreatConfirmed:
		// Two sentences for two situations. Before anybody knows whose commitment
		// it was, the honest thing is that it is being checked; once a countdown
		// is running, the honest thing is that it is in hand.
		if threat.HeadlineDeadline != nil {
			out.Status = statusHandling
		} else {
			out.Status = statusChecking
			out.StatusAction = actionOpenDetails
		}
	case ThreatResolved:
		out.Status = statusSettled
	case ThreatLoss:
		out.Status = statusLost
		out.StatusAction = actionOpenDetails
	default:
		out.Status = statusWatching
	}
	return out
}

// partnerName is who the channel is with, in the least misleading form
// available.
//
// The alias when the counterparty gave one — already clamped to printable
// characters and 32 bytes when it was stored, because the counterparty is the
// adversary here — and otherwise a shortened key. Shortened rather than full,
// because sixty-six characters of hex in a table column is not an identifier to
// this reader, it is a wall.
func partnerName(c store.Channel) string {
	if alias := strings.TrimSpace(c.PeerAlias); alias != "" {
		return alias
	}
	if len(c.PeerPubkey) > 12 {
		return c.PeerPubkey[:12] + "…"
	}
	if c.PeerPubkey != "" {
		return c.PeerPubkey
	}
	return "an unnamed node"
}

// atRisk is what could be lost.
//
// The channel's whole capacity when something is happening to it, and nothing
// when nothing is. An upper bound rather than a measurement: what a revoked
// commitment takes is everything in the channel, and the balance at that moment
// is not something this daemon can know. Overstating what is at stake is the
// safe direction; understating it is not.
func atRisk(c store.Channel, threatState string) int64 {
	switch threatState {
	case ThreatMempool, ThreatConfirmed, ThreatLoss:
		return c.CapacitySat
	case ThreatNone, ThreatWatch, ThreatResolved:
		return 0
	default:
		return c.CapacitySat
	}
}

// shortID is how a transaction is named in the Details view, where a reader has
// asked for it.
func shortID(id string) string {
	if len(id) <= 16 {
		return id
	}
	return fmt.Sprintf("%s…%s", id[:8], id[len(id)-8:])
}
