// Package cln reads channels from a Core Lightning node over clnrest.
//
// The mapping targets are the same as the LND adapter's: both converge on
// registry.ChannelRecord so that everything downstream sees one thing.
package cln

import (
	"fmt"
	"strings"

	"github.com/paulscode/forktower/internal/registry"
	"github.com/paulscode/forktower/internal/store"
)

// The shapes clnrest sends. Only the fields Forktower reads.
//
// Unlike LND's REST, Core Lightning sends JSON numbers as numbers — its amounts
// arrive as strings with an "msat" suffix instead, which is the quirk to watch
// here.

type getinfoJSON struct {
	ID          string `json:"id"`
	Alias       string `json:"alias"`
	Blockheight int32  `json:"blockheight"`
}

type htlcJSON struct {
	Direction   string `json:"direction"` // "in" or "out", from our point of view
	AmountMsat  any    `json:"amount_msat"`
	Expiry      int32  `json:"expiry"`
	PaymentHash string `json:"payment_hash"`
}

type peerChannelJSON struct {
	PeerID         string `json:"peer_id"`
	State          string `json:"state"`
	FundingTxID    string `json:"funding_txid"`
	FundingOutnum  int32  `json:"funding_outnum"`
	TotalMsat      any    `json:"total_msat"`
	ShortChannelID string `json:"short_channel_id"`
	ChannelType    *struct {
		Names []string `json:"names"`
	} `json:"channel_type"`
	// The two delays, and the schema is explicit about which is which:
	// our_to_self_delay is "the number of blocks before we can take our funds if
	// we unilateral close", theirs likewise for them. So ours is what we wait and
	// theirs is what they wait — the same rule the LND adapter arrives at from
	// the opposite naming convention.
	OurToSelfDelay   *int32     `json:"our_to_self_delay"`
	TheirToSelfDelay *int32     `json:"their_to_self_delay"`
	HTLCs            []htlcJSON `json:"htlcs"`
	CloseTxID        string     `json:"scratch_txid"`
}

type listPeerChannelsJSON struct {
	Channels []peerChannelJSON `json:"channels"`
}

// mapChannel turns one of Core Lightning's channels into the shape Forktower
// stores.
func mapChannel(c peerChannelJSON) (registry.ChannelRecord, error) {
	if len(c.FundingTxID) != 64 {
		return registry.ChannelRecord{},
			fmt.Errorf("%q is not a funding transaction id", c.FundingTxID)
	}

	capacityMsat, err := msatFrom(c.TotalMsat, "channel capacity")
	if err != nil {
		return registry.ChannelRecord{}, err
	}

	rec := registry.ChannelRecord{
		FundingTxID: strings.ToLower(c.FundingTxID),
		FundingVout: c.FundingOutnum,
		// Core Lightning reports capacity in millisatoshis; the store keeps
		// satoshis, as the rest of Bitcoin does.
		CapacitySat:    capacityMsat / 1000,
		ChanType:       mapChannelType(c.ChannelType),
		CSVDelayLocal:  clampDelay(c.OurToSelfDelay),
		CSVDelayRemote: clampDelay(c.TheirToSelfDelay),
		PeerPubkey:     c.PeerID,
		SCID:           c.ShortChannelID,
		CloseState:     mapState(c.State),
	}
	if rec.CloseState != store.CloseOpen {
		rec.CloseTxID = c.CloseTxID
	}
	rec.OpenHeight = registry.BlockFromShortChannelID(c.ShortChannelID)

	for _, h := range c.HTLCs {
		snap, htlcErr := mapHTLC(h)
		if htlcErr != nil {
			return registry.ChannelRecord{}, htlcErr
		}
		rec.HTLCs = append(rec.HTLCs, snap)
	}
	return rec, nil
}

// maxToSelfDelay is the largest delay the protocol can express: BOLT-2 carries
// `to_self_delay` as a 16-bit field.
const maxToSelfDelay = 65535

// clampDelay keeps "not reported" and "reported as zero" apart, and treats a
// value the protocol cannot express as not reported.
//
// A delay outside the range is nonsense from the node, and keeping it would put
// a deadline before the height the spend confirmed at — reading as already
// passed, and silencing the countdown. Absent routes it to the conservative
// floor instead, which is the path built for an input we do not have.
func clampDelay(v *int32) *int32 {
	if v == nil || *v < 0 || *v > maxToSelfDelay {
		return nil
	}
	out := *v
	return &out
}

// mapChannelType reads Core Lightning's feature names.
//
// An unrecognised set becomes `unknown` rather than an error: a channel of a
// kind this build has not heard of still needs watching, and refusing it would
// leave the user unprotected on exactly the channel that is unusual.
func mapChannelType(t *struct {
	Names []string `json:"names"`
}) store.ChanType {
	if t == nil {
		return store.ChanTypeUnknown
	}
	var anchors, staticRemote, taproot bool
	for _, name := range t.Names {
		switch n := strings.ToLower(name); {
		case strings.Contains(n, "simple_taproot"):
			taproot = true
		case strings.Contains(n, "anchors"):
			anchors = true
		case strings.Contains(n, "static_remotekey"):
			staticRemote = true
		}
	}
	switch {
	case taproot:
		return store.ChanTaproot
	case anchors:
		return store.ChanAnchors
	case staticRemote:
		return store.ChanStaticRemote
	default:
		return store.ChanTypeUnknown
	}
}

// mapState reads what the *node* believes about the channel.
//
// A belief, not a fact about the chain: the watcher decides that, from the
// chain, and its answer wins. This is the earlier and less certain of the two,
// which is exactly why it is worth having — a channel the node knows is closing
// is one to look at before the block arrives.
func mapState(state string) store.CloseState {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "CHANNELD_NORMAL", "CHANNELD_AWAITING_LOCKIN",
		"DUALOPEND_OPEN_INIT", "DUALOPEND_AWAITING_LOCKIN",
		"DUALOPEND_OPEN_COMMITTED", "DUALOPEND_OPEN_COMMIT_READY",
		"OPENINGD":
		return store.CloseOpen
	case "CHANNELD_SHUTTING_DOWN", "CLOSINGD_SIGEXCHANGE", "CLOSINGD_COMPLETE",
		"AWAITING_UNILATERAL", "FUNDING_SPEND_SEEN", "ONCHAIN":
		return store.ClosePending
	default:
		// A state this build has not heard of. Treated as closing rather than
		// open: the cost of looking at a channel that turns out to be fine is a
		// wasted scan, and the cost of the other mistake is a missed close.
		return store.ClosePending
	}
}

// mapHTLC turns one in-flight HTLC into a snapshot row.
//
// Direction is from our point of view — "in" is an HTLC we may need to claim
// before its expiry, "out" one that times out at its own.
func mapHTLC(h htlcJSON) (store.HTLCSnapshot, error) {
	direction := "outgoing"
	if strings.EqualFold(strings.TrimSpace(h.Direction), "in") {
		direction = "incoming"
	}
	amount, err := msatFrom(h.AmountMsat, "HTLC amount")
	if err != nil {
		return store.HTLCSnapshot{}, err
	}
	return store.HTLCSnapshot{
		Direction:   direction,
		AmountMsat:  amount,
		CLTVExpiry:  h.Expiry,
		PaymentHash: h.PaymentHash,
	}, nil
}
