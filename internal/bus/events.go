package bus

// Event kinds. Constants rather than literals so that a subscriber filtering on
// one cannot quietly mistype it and receive nothing.
const (
	KindSplitStateChanged   = "split.state_changed"
	KindSplitSuspected      = "split.suspected"
	KindSplitBranchExtended = "split.branch_extended"
	KindViewHealthChanged   = "view.health_changed"
	KindAlertRaised         = "alert.raised"

	KindChannelUpserted = "registry.channel_upserted"
	KindChannelClosedSF = "registry.channel_closed_sf"

	KindFundingSpent     = "watch.funding_spent"
	KindSecondOrderSpent = "watch.second_order_spent"
	KindSpendReorgedOut  = "watch.spend_reorged_out"
	KindMempoolSighting  = "watch.mempool_sighting"

	KindTowerHealthChanged = "tower.health_changed"
	KindTowerConcern       = "tower.concern"

	KindDeadlineEscalated   = "deadline.escalated"
	KindDeadlineResolved    = "deadline.resolved"
	KindDeadlineExpiredLoss = "deadline.expired_loss"
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

// SplitSuspected reports that the two chains are disagreeing in a way worth
// warning about, before that has been confirmed as a split.
//
// A separate event from SplitStateChanged because it is deliberately raised
// earlier and claims less. Anyone can see a fork on a block explorer the moment
// there is one; a daemon that says nothing until it is certain has left the user
// less informed than a web page, during the one event it exists for. Suspected is
// the honest thing to say in between, and it has to travel to the alert layer to
// be said at all — on the platforms this runs on, nothing reaches the user except
// through a raised alert.
type SplitSuspected struct {
	// Suspected is false when the disagreement has ended without becoming a split,
	// which closes the warning rather than leaving it standing.
	Suspected bool `json:"suspected"`
	// Height, SFHash and SQHash are the two chains' own answers at one height, when
	// a direct comparison was available. They are what makes the warning checkable
	// against any block explorer.
	Height int32  `json:"height,omitempty"`
	SFHash string `json:"sf_hash,omitempty"`
	SQHash string `json:"sq_hash,omitempty"`
	Since  int64  `json:"since,omitempty"`
}

// Kind implements Event.
func (SplitSuspected) Kind() string { return KindSplitSuspected }

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

// FundingSpent reports that a channel's funding output was spent on one of the
// chains — the event this daemon exists to notice.
//
// Shape is what the transaction looks like it was, and Status how firmly it has
// happened. Both are carried because a subscriber deciding how loudly to react
// needs them together: an unconfirmed sighting of something that might be a
// cooperative close is a very different message from a confirmed commitment.
type FundingSpent struct {
	SpendEventID int64  `json:"spend_event_id"`
	ChannelID    int64  `json:"channel_id"`
	Branch       string `json:"branch"`
	SpendTxid    string `json:"spend_txid"`
	Shape        string `json:"shape"`
	Status       string `json:"status"`
	Height       int32  `json:"height"`
}

// Kind implements Event.
func (FundingSpent) Kind() string { return KindFundingSpent }

// SecondOrderSpent reports that an output of a commitment that had already
// confirmed has itself been spent.
//
// This is how outcomes are learned rather than only threats: a sweep before the
// delay expires is somebody's justice transaction landing, and one after it is a
// loss. SourceSpendEventID points back at the commitment, which is what lets a
// reorg that removes the commitment also remove what it created.
type SecondOrderSpent struct {
	SpendEventID       int64  `json:"spend_event_id"`
	SourceSpendEventID int64  `json:"source_spend_event_id"`
	Role               string `json:"role"`
	Shape              string `json:"shape"`
}

// Kind implements Event.
func (SecondOrderSpent) Kind() string { return KindSecondOrderSpent }

// MempoolSighting reports a spend of something being watched, seen before any
// block contains it.
//
// The early warning. A commitment noticed while it is still unconfirmed buys the
// user a block of time they would not otherwise have had, which on a chain with
// slow blocks can be a great deal of time. It is a sighting and not a fact: an
// unconfirmed transaction may be replaced, may never confirm, and may not even
// be what the rest of the network is seeing. Carries no height for that reason.
type MempoolSighting struct {
	SpendEventID int64  `json:"spend_event_id"`
	ChannelID    int64  `json:"channel_id"`
	Branch       string `json:"branch"`
	SpendTxid    string `json:"spend_txid"`
	Shape        string `json:"shape"`
}

// Kind implements Event.
func (MempoolSighting) Kind() string { return KindMempoolSighting }

// SpendReorgedOut reports that a spend we had recorded is no longer on the
// chain.
//
// Alert-worthy in its own right, which is not obvious. A breach that disappears
// has not necessarily gone away: the counterparty may be replacing it with a
// higher fee, or miners may have reorganised it out and it may return. Treating
// this as good news would be the wrong instinct.
type SpendReorgedOut struct {
	SpendEventID int64  `json:"spend_event_id"`
	Branch       string `json:"branch"`
}

// Kind implements Event.
func (SpendReorgedOut) Kind() string { return KindSpendReorgedOut }

// DeadlineEscalated reports that a countdown has reached a louder tier.
//
// Carries the time as well as the block count, because a block count on its own
// is not an answer: a minority chain can take half an hour a block, so the same
// number of blocks can mean far more human time than instinct says. EstWallClock
// is empty when the chain's cadence is not known, and a reader must then say
// nothing about time rather than assume ten minutes.
type DeadlineEscalated struct {
	DeadlineID      int64  `json:"deadline_id"`
	ChannelID       int64  `json:"channel_id"`
	Level           int    `json:"level"`
	RemainingBlocks int32  `json:"remaining_blocks"`
	EstWallClock    string `json:"est_wall_clock,omitempty"`
}

// Kind implements Event.
func (DeadlineEscalated) Kind() string { return KindDeadlineEscalated }

// DeadlineResolved reports a countdown that stopped without anybody losing
// anything.
//
// ByTxid names the transaction that answered it, when one did. Empty means the
// threat simply went away — the close it was counting from left the chain and
// did not come back.
type DeadlineResolved struct {
	DeadlineID int64  `json:"deadline_id"`
	ByTxid     string `json:"by_txid,omitempty"`
}

// Kind implements Event.
func (DeadlineResolved) Kind() string { return KindDeadlineResolved }

// DeadlineExpiredLoss reports a countdown that ran out with nobody having
// answered it.
//
// AmountSat is the channel's whole capacity: an upper bound rather than a
// measurement, because what a revoked commitment takes is everything in the
// channel and the balance at that moment is not something this daemon can know.
type DeadlineExpiredLoss struct {
	DeadlineID int64 `json:"deadline_id"`
	ChannelID  int64 `json:"channel_id"`
	AmountSat  int64 `json:"amount_sat"`
}

// Kind implements Event.
func (DeadlineExpiredLoss) Kind() string { return KindDeadlineExpiredLoss }

// AllKinds lists every event kind the bus carries, for diagnostics and for tests
// that check nothing has been added without being registered here.
func AllKinds() []string {
	return []string{
		KindSplitStateChanged,
		KindSplitSuspected,
		KindSplitBranchExtended,
		KindViewHealthChanged,
		KindAlertRaised,
		KindChannelUpserted,
		KindChannelClosedSF,
		KindFundingSpent,
		KindSecondOrderSpent,
		KindSpendReorgedOut,
		KindMempoolSighting,
		KindTowerHealthChanged,
		KindTowerConcern,
		KindDeadlineEscalated,
		KindDeadlineResolved,
		KindDeadlineExpiredLoss,
	}
}

// TowerHealthChanged reports that a watchtower's condition changed.
//
// Only on a change, like ViewHealthChanged and for the same reason: the tower is
// polled continuously, and announcing "still fine" every minute would bury the
// one time it was not.
type TowerHealthChanged struct {
	TowerID int64 `json:"tower_id"`
	// TowerKind is `lnd` or `teos`. Named so rather than `Kind` because Kind()
	// is how every event on this bus announces what it is.
	TowerKind string `json:"tower_kind"`
	Pubkey    string `json:"pubkey"`
	// Managed distinguishes a tower this installation runs from one the user
	// pointed us at. It changes what may honestly be promised about fixing it.
	Managed bool   `json:"managed"`
	Status  string `json:"status"`
	Detail  string `json:"detail"`
	// Previous is what it was before, so a subscriber can tell recovery from
	// deterioration without keeping its own copy.
	Previous string `json:"previous"`
}

// Kind implements Event.
func (TowerHealthChanged) Kind() string { return KindTowerHealthChanged }

// TowerConcern reports something worth telling the user about a tower or about
// one channel's protection.
//
// Deliberately not one event per problem type. The concerns differ in what they
// say and not in how they travel, and a subscriber that had to know about six
// event types would be one that silently ignores the seventh.
type TowerConcern struct {
	TowerID int64  `json:"tower_id"`
	Concern string `json:"concern"`
	// ChannelID is set when the concern is about one channel, zero when it is
	// about the tower as a whole.
	ChannelID int64  `json:"channel_id,omitempty"`
	Message   string `json:"message"`
	// Cleared says this concern no longer applies.
	//
	// **Announced, because the user is the one who fixed it.** Turning on a
	// node's watchtower client is a setting Forktower cannot change and cannot
	// confirm any other way — so somebody who does it comes straight back to see
	// whether it took, and used to find the same warning still sitting there
	// with nothing beside it to say the condition had passed.
	Cleared bool `json:"cleared,omitempty"`
}

// Kind implements Event.
func (TowerConcern) Kind() string { return KindTowerConcern }
