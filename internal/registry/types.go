// Package registry keeps Forktower's picture of the user's Lightning channels
// up to date, by reading their node.
//
// Read-only, always. Nothing here ever sends a Lightning node an instruction,
// and nothing here ever needs a credential that could.
package registry

import "github.com/paulscode/forktower/internal/store"

// ChannelRecord is one channel as a Lightning node describes it.
//
// Mirrors the stored row rather than any node's own shape, so the adapters
// converge here and everything downstream sees one thing. What it deliberately
// does not carry is anything only Forktower knows — the close state seen on
// chain, or the relevance classification — because those are not the node's to
// say and a poll must not overwrite them.
type ChannelRecord struct {
	FundingTxID      string
	FundingVout      int32
	FundingScriptHex string
	CapacitySat      int64
	ChanType         store.ChanType

	// CSVDelayLocal is what *we* wait after our own force-close.
	// CSVDelayRemote is what the *peer* waits after theirs, which makes it the
	// window we have to answer a breach against us.
	//
	// Nil means the node did not say. Kept distinct from zero: an unknown delay
	// produces a deadline from a conservative floor, a zero one would produce a
	// deadline that has already passed.
	CSVDelayLocal  *int32
	CSVDelayRemote *int32

	PeerPubkey string
	PeerAlias  string
	OpenHeight int32
	SCID       string

	// CloseState is what the *node* believes, which is not the same as what the
	// chain shows. The watcher decides the latter.
	CloseState store.CloseState
	CloseTxID  string

	HTLCs []store.HTLCSnapshot
}

// NodeInfo identifies the Lightning node itself.
type NodeInfo struct {
	Pubkey string
	Alias  string
	Impl   store.LNImpl
}

// Snapshot is everything one node had to say at one moment.
type Snapshot struct {
	Node     NodeInfo
	Channels []ChannelRecord
}
