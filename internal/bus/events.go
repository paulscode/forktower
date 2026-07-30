package bus

// Event kinds. Constants rather than literals so that a subscriber filtering on
// one cannot quietly mistype it and receive nothing.
const (
	KindSplitStateChanged   = "split.state_changed"
	KindSplitBranchExtended = "split.branch_extended"
	KindViewHealthChanged   = "view.health_changed"
	KindAlertRaised         = "alert.raised"
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

// AllKinds lists every event kind the bus carries, for diagnostics and for tests
// that check nothing has been added without being registered here.
func AllKinds() []string {
	return []string{
		KindSplitStateChanged,
		KindSplitBranchExtended,
		KindViewHealthChanged,
		KindAlertRaised,
	}
}
