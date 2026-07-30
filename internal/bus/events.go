package bus

// Event kinds. Constants rather than literals so that a subscriber filtering on
// one cannot quietly mistype it and receive nothing.
const (
	KindSplitStateChanged   = "split.state_changed"
	KindSplitBranchExtended = "split.branch_extended"
	KindViewHealthChanged   = "view.health_changed"
	KindAlertRaised         = "alert.raised"

	KindChannelUpserted = "registry.channel_upserted"
	KindChannelClosedSF = "registry.channel_closed_sf"
)

// BlockRefJSON identifies a block. Hashes are lowercase hex in the usual display
// order; heights are plain numbers.
//
// The JSON mirror types exist because events are serialised into the timeline and
// served over the API. Keeping them separate from the internal chain types means
// the wire format is a deliberate choice rather than a side effect of a struct
// field being renamed.
type BlockRefJSON struct {
	Hash   string `json:"hash"`
	Height int32  `json:"height"`
}

// BlockMetaJSON is a block reference plus what the header says about it. Time is
// unix seconds from the header, which is the miner's claim rather than a
// measurement, and is only ever used for estimates.
type BlockMetaJSON struct {
	Hash     string `json:"hash"`
	Height   int32  `json:"height"`
	PrevHash string `json:"prev_hash"`
	Time     int64  `json:"time"`
}

// SplitStateChanged reports that the relationship between the two chains has
// changed.
type SplitStateChanged struct {
	Old string `json:"old"`
	New string `json:"new"`
	// Fork locates where the chains separated, once that is known. Nil before a
	// split is recorded.
	Fork *BlockRefJSON `json:"fork,omitempty"`
}

// Kind implements Event.
func (SplitStateChanged) Kind() string { return KindSplitStateChanged }

// SplitBranchExtended reports a new block on one of the chains.
type SplitBranchExtended struct {
	Branch string        `json:"branch"`
	Block  BlockMetaJSON `json:"block"`
	// SinceForkDepth is how many blocks this chain has added since the chains
	// separated. Zero when no separation point is known.
	SinceForkDepth int32 `json:"since_fork_depth"`
	// AvgIntervalSecs is the smoothed time between this chain's recent blocks.
	//
	// Carried on the event because it is what turns a countdown in blocks into a
	// countdown in time, and a minority chain's blocks can be far apart — so the
	// same number of blocks can mean a very different amount of human time. Always
	// presented as an estimate.
	AvgIntervalSecs float64 `json:"avg_interval_secs"`
}

// Kind implements Event.
func (SplitBranchExtended) Kind() string { return KindSplitBranchExtended }

// ViewHealthChanged reports that one of the chain views has become more or less
// trustworthy.
type ViewHealthChanged struct {
	// View names which one: the user's own node, the other chain's backend, or a
	// cross-check against an independent source.
	View string `json:"view"`
	Old  string `json:"old"`
	New  string `json:"new"`
	// Detail is a short human explanation, safe to show a user.
	Detail string `json:"detail"`
}

// Kind implements Event.
func (ViewHealthChanged) Kind() string { return KindViewHealthChanged }

// AlertRaised reports that an alert was created or re-raised, so that transports
// can deliver it and the API can surface it.
type AlertRaised struct {
	AlertID   int64  `json:"alert_id"`
	Tier      string `json:"tier"`
	AlertKind string `json:"alert_kind"`
	DedupKey  string `json:"dedup_key"`
	Message   string `json:"message"`
}

// Kind implements Event.
func (AlertRaised) Kind() string { return KindAlertRaised }

// ChannelJSON is a channel as the timeline and the API see it.
//
// A summary rather than the whole row: the fields here are the ones that
// identify a channel to a person reading their own history afterwards, plus the
// classification, which is the part that changes and the part that matters.
// PeerAlias is chosen by the counterparty — who is the adversary in this
// story — and arrives already clamped by the store.
type ChannelJSON struct {
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

	// Relevance is whether this channel is exposed on the chain the user's node
	// does not follow, and Reason is the sentence explaining it. The reason is
	// carried on the event because "why is this one being watched" is the
	// question a user asks, and answering it from the timeline is cheaper than
	// re-deriving it later from state that has since moved on.
	Relevance       string `json:"relevance"`
	RelevanceReason string `json:"relevance_reason,omitempty"`
}

// ChannelUpserted reports that a channel was seen for the first time, or that
// something about it changed. Not published on an unchanged poll: with a poll
// every minute, announcing every channel every time would bury the one event
// that meant something.
type ChannelUpserted struct {
	Channel ChannelJSON `json:"channel"`
	// New distinguishes a channel never seen before from one that changed.
	New bool `json:"new"`
}

// Kind implements Event.
func (ChannelUpserted) Kind() string { return KindChannelUpserted }

// ChannelClosedSF reports that a channel closed, or began closing, on the chain
// the user's own node follows.
//
// Worth its own event rather than folding into ChannelUpserted because closing
// on one chain is precisely when a channel becomes *more* interesting on the
// other: until that close confirms over there too, the old revoked commitments
// remain spendable on the chain nobody is looking at.
type ChannelClosedSF struct {
	ChannelID int64  `json:"channel_id"`
	CloseTxid string `json:"close_txid,omitempty"`
	State     string `json:"state"`
}

// Kind implements Event.
func (ChannelClosedSF) Kind() string { return KindChannelClosedSF }

// AllKinds lists every event kind the bus carries, for diagnostics and for tests
// that check nothing has been added without being registered here.
func AllKinds() []string {
	return []string{
		KindSplitStateChanged,
		KindSplitBranchExtended,
		KindViewHealthChanged,
		KindAlertRaised,
		KindChannelUpserted,
		KindChannelClosedSF,
	}
}
