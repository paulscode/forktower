// Package lnd reads channels from an LND node over its REST interface.
//
// REST rather than gRPC: lnd's own protos need a forked protobuf and bring 187
// modules for the five read-only calls this needs. The wire format is
// protobuf's JSON mapping either way, and the types below are the five
// responses Forktower actually reads.
package lnd

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/paulscode/forktower/internal/registry"
	"github.com/paulscode/forktower/internal/store"
)

// The shapes LND sends. Only the fields Forktower reads: a struct that mirrored
// the whole message would be a standing invitation to depend on more of it.
//
// Note the string types on the numbers. protobuf's JSON mapping renders 64-bit
// integers as strings, so `capacity` arrives as "150000". Parsed explicitly
// below, because a silently-zero capacity is worse than a parse error.

type infoJSON struct {
	IdentityPubkey string `json:"identity_pubkey"`
	Alias          string `json:"alias"`
	SyncedToChain  bool   `json:"synced_to_chain"`
	BlockHeight    int32  `json:"block_height"`
}

type constraintsJSON struct {
	CSVDelay *uint32 `json:"csv_delay"`
}

type htlcJSON struct {
	Incoming         bool   `json:"incoming"`
	AmountMsat       string `json:"amount"`
	Amount           string `json:"amount_msat"`
	ExpirationHeight int32  `json:"expiration_height"`
	HashLock         string `json:"hash_lock"`
}

type channelJSON struct {
	ChannelPoint      string           `json:"channel_point"`
	RemotePubkey      string           `json:"remote_pubkey"`
	Capacity          string           `json:"capacity"`
	ChanID            string           `json:"chan_id"`
	CommitmentType    string           `json:"commitment_type"`
	LocalConstraints  *constraintsJSON `json:"local_constraints"`
	RemoteConstraints *constraintsJSON `json:"remote_constraints"`
	PendingHTLCs      []htlcJSON       `json:"pending_htlcs"`
	Active            bool             `json:"active"`
	Initiator         bool             `json:"initiator"`
}

type listChannelsJSON struct {
	Channels []channelJSON `json:"channels"`
}

// mapChannel turns one of LND's channels into the shape Forktower stores.
//
// **The two CSV delays are the part worth reading twice.** Verified against
// lnd's own source rather than inferred:
//
//   - `rpcserver.go` builds `LocalConstraints` from `dbChannel.LocalChanCfg` and
//     `RemoteConstraints` from `dbChannel.RemoteChanCfg`.
//   - `channeldb.ChannelConfig.CsvDelay` is documented as the delay applied to
//     outputs paying *the owner of this channel configuration*, on that owner's
//     own commitment.
//
// So `local_constraints.csv_delay` is what **we** wait on our own commitment,
// and `remote_constraints.csv_delay` is what the **peer** waits on theirs —
// which is the window we have to answer a breach against us.
//
// The trap this avoids: BOLT-2's `to_self_delay` is the delay the *sender*
// requires of the *receiver*, which is inverted relative to lnd's
// config-owner convention. Anyone mapping straight from BOLT-2 gets it
// backwards, and the result is a deadline that looks right and expires at the
// wrong time.
func mapChannel(c channelJSON) (registry.ChannelRecord, error) {
	txid, vout, err := splitChannelPoint(c.ChannelPoint)
	if err != nil {
		return registry.ChannelRecord{}, err
	}

	capacity, err := parseInt64(c.Capacity, "capacity")
	if err != nil {
		return registry.ChannelRecord{}, err
	}

	rec := registry.ChannelRecord{
		FundingTxID:    txid,
		FundingVout:    vout,
		CapacitySat:    capacity,
		ChanType:       mapCommitmentType(c.CommitmentType),
		CSVDelayLocal:  csvFrom(c.LocalConstraints),
		CSVDelayRemote: csvFrom(c.RemoteConstraints),
		PeerPubkey:     c.RemotePubkey,
		CloseState:     store.CloseOpen,
	}

	// LND packs the short channel id into an integer; the stored form is the
	// readable one both implementations can be compared in. Converting here is
	// what keeps the same channel from being recorded two different ways
	// depending on which node reported it.
	if packed, parseErr := strconv.ParseUint(c.ChanID, 10, 64); parseErr == nil {
		if scid, ok := registry.ShortChannelIDFromPacked(packed); ok {
			rec.SCID = scid
			rec.OpenHeight = registry.BlockFromShortChannelID(scid)
		}
	}

	for _, h := range c.PendingHTLCs {
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

// csvFrom keeps "the node did not say" distinct from "the node said zero", and
// treats a value the protocol cannot express as not said at all.
//
// The third case matters more than it looks. A delay outside the protocol's
// range is nonsense from the node, and converting it would wrap: a negative
// delay puts the deadline *before* the height the spend confirmed at, which
// reads as already passed and silences the countdown. Returning nil instead
// routes it to the conservative floor, which is the path already designed for an
// input we do not have.
func csvFrom(c *constraintsJSON) *int32 {
	if c == nil || c.CSVDelay == nil {
		return nil
	}
	if *c.CSVDelay > maxToSelfDelay {
		return nil
	}
	v := int32(*c.CSVDelay) //nolint:gosec // bounded above by maxToSelfDelay
	return &v
}

// mapCommitmentType maps LND's names onto ours.
//
// An unrecognised type becomes `unknown` rather than an error: a channel of a
// kind this build has not heard of still needs watching, and refusing to record
// it would leave the user unprotected on exactly the channel that is unusual.
// It stays visible as `unknown` so nothing pretends to understand it.
func mapCommitmentType(t string) store.ChanType {
	switch strings.ToUpper(strings.TrimSpace(t)) {
	case "LEGACY":
		return store.ChanLegacy
	case "STATIC_REMOTE_KEY":
		return store.ChanStaticRemote
	case "ANCHORS", "ANCHORS_ZERO_FEE_HTLC_TX":
		return store.ChanAnchors
	case "SIMPLE_TAPROOT", "SIMPLE_TAPROOT_OVERLAY", "TAPROOT":
		return store.ChanTaproot
	default:
		return store.ChanTypeUnknown
	}
}

// mapHTLC turns one in-flight HTLC into a snapshot row.
//
// Direction is from *our* point of view, which is what the deadline engine
// needs: an incoming HTLC we know the preimage for must be claimed before its
// expiry, an outgoing one times out at its own.
func mapHTLC(h htlcJSON) (store.HTLCSnapshot, error) {
	direction := "outgoing"
	if h.Incoming {
		direction = "incoming"
	}

	// LND reports both `amount` (satoshis) and `amount_msat`. Prefer the
	// millisatoshi figure and fall back, because a value rounded to satoshis is
	// still worth more than none.
	raw, unit := h.Amount, int64(1)
	if raw == "" {
		raw, unit = h.AmountMsat, 1000
	}
	amount, err := parseInt64(raw, "HTLC amount")
	if err != nil {
		return store.HTLCSnapshot{}, err
	}

	return store.HTLCSnapshot{
		Direction:   direction,
		AmountMsat:  amount * unit,
		CLTVExpiry:  h.ExpirationHeight,
		PaymentHash: h.HashLock,
	}, nil
}

// splitChannelPoint parses LND's "txid:vout".
func splitChannelPoint(cp string) (txid string, vout int32, err error) {
	at := strings.LastIndex(cp, ":")
	if at <= 0 || at == len(cp)-1 {
		return "", 0, fmt.Errorf("%q is not a channel point of the form txid:vout", cp)
	}
	txid = strings.ToLower(cp[:at])
	if _, decodeErr := hex.DecodeString(txid); decodeErr != nil || len(txid) != 64 {
		return "", 0, fmt.Errorf("%q does not contain a transaction id", cp)
	}
	n, parseErr := strconv.ParseInt(cp[at+1:], 10, 32)
	if parseErr != nil || n < 0 {
		return "", 0, fmt.Errorf("%q does not contain an output index", cp)
	}
	return txid, int32(n), nil
}

func parseInt64(s, what string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("reading the %s %q: %w", what, s, err)
	}
	return v, nil
}
