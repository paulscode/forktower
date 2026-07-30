// Package chainview is the interface every chain backend implements, plus the
// types it exchanges.
//
// Backends supply blocks and match hints only. Outpoint matching, reorg
// bookkeeping and scan progress live above this layer so that they are
// implemented once, independently of which backend is in use.
package chainview

import (
	"errors"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

// Branch identifies which of the two chains a view follows.
//
// Named by role, not by merit: "sf" is the chain the user's own node follows and
// "sq" is the other one. Nothing here ranks them, and the daemon takes no
// position on which is legitimate.
type Branch string

// The two chains.
const (
	// BranchSF is the chain the user's own Bitcoin node follows.
	BranchSF Branch = "sf"
	// BranchSQ is the chain it does not — the one we watch on their behalf.
	BranchSQ Branch = "sq"
)

// Valid reports whether b is one of the two chains.
func (b Branch) Valid() bool {
	switch b {
	case BranchSF, BranchSQ:
		return true
	default:
		return false
	}
}

// Other returns the opposite chain, or an empty Branch if b is not a valid one.
func (b Branch) Other() Branch {
	switch b {
	case BranchSF:
		return BranchSQ
	case BranchSQ:
		return BranchSF
	default:
		return ""
	}
}

// BlockRef identifies a block by hash and height.
type BlockRef struct {
	Hash   chainhash.Hash
	Height int32
}

// BlockMeta is a block reference plus what its header says.
type BlockMeta struct {
	BlockRef
	PrevHash chainhash.Hash
	// Time is the header timestamp: the miner's claim about when the block was
	// made, not a measurement. Only ever used for estimates, and never for a
	// deadline — those are counted in blocks precisely because this cannot be
	// trusted to the minute.
	Time time.Time
}

// WatchSet is what a caller wants matched in blocks.
//
// Both forms are carried because the two kinds of backend match differently. A
// full node is given the outpoints and scans transaction inputs directly; a
// light client can only test a block's compact filter, which commits to scripts
// rather than to outpoints. Callers populate whichever they can and the backend
// uses what it needs.
type WatchSet struct {
	// Outpoints are the specific outputs whose spending matters.
	Outpoints map[wire.OutPoint]struct{}
	// Scripts are the scriptPubKeys of those outputs, needed for filter matching.
	Scripts [][]byte
}

// Empty reports whether there is nothing to look for. A backend asked to match an
// empty set should say so rather than reporting every block as a possible match.
func (w WatchSet) Empty() bool { return len(w.Outpoints) == 0 && len(w.Scripts) == 0 }

// HasOutpoint reports whether op is being watched.
func (w WatchSet) HasOutpoint(op wire.OutPoint) bool {
	_, ok := w.Outpoints[op]
	return ok
}

// HealthState is how much a chain view can currently be relied on.
type HealthState string

// Health states. A backend reports the first three about itself; the other three
// are conclusions drawn above it, because none of them is visible from inside a
// single view — they all require comparing one view against another.
const (
	// HealthSyncing means the backend is still catching up and its answers are
	// incomplete rather than wrong.
	HealthSyncing HealthState = "SYNCING"
	// HealthOK means it is caught up and answering.
	HealthOK HealthState = "OK"
	// HealthDegraded means it is answering but something is off: few peers, a tip
	// that has not moved for longer than expected.
	HealthDegraded HealthState = "DEGRADED"
	// HealthEclipseSuspect means independent sources disagree about this chain's
	// tip, so the view may be being fed a fabricated quiet chain. Set above the
	// backend, never by it.
	HealthEclipseSuspect HealthState = "ECLIPSE_SUSPECT"
	// HealthWrongBranch means the backend is not on the chain we believe it is —
	// the worst of these, because scanning the wrong chain produces false comfort
	// rather than an error. Set above the backend, never by it.
	HealthWrongBranch HealthState = "WRONG_BRANCH"
	// HealthDown means it is not answering at all.
	HealthDown HealthState = "DOWN"
)

// Valid reports whether h is a known state.
func (h HealthState) Valid() bool {
	switch h {
	case HealthSyncing, HealthOK, HealthDegraded,
		HealthEclipseSuspect, HealthWrongBranch, HealthDown:
		return true
	default:
		return false
	}
}

// Usable reports whether a view in this state can be relied on for detection.
//
// Only OK qualifies. Syncing is incomplete, degraded is suspect, and the
// remaining states are worse — scanning while wrong-branch or eclipsed would
// produce a clean report about a chain nobody else is on, which is more dangerous
// than reporting nothing.
func (h HealthState) Usable() bool { return h == HealthOK }

// BackendHealth is a backend's own account of itself.
type BackendHealth struct {
	State HealthState
	Tip   BlockMeta
	// PeerCount is how many peers the backend is connected to. On the chain we do
	// not control, a collapse here is the first sign of losing sight of it.
	PeerCount int
	// SyncProgress runs 0..1, reaching 1 when caught up.
	SyncProgress float64
	// Detail is a short human explanation, safe to show a user.
	Detail string
}

// NetworkParams identifies the chain a view must be on.
//
// Checked before anything else because nothing downstream can detect the
// failure: a backend pointed at a test network answers every request correctly
// and diverges from the user's node permanently, which would read as a chain
// split rather than as a misconfiguration.
type NetworkParams struct {
	// Name matches what the node calls its network: "main", "test", "signet",
	// "regtest".
	Name string
	// Genesis is the first block's hash, checked as well as the name because a
	// name is a label and this is the chain's identity.
	Genesis chainhash.Hash
}

// ChainTip is one of the branch tips a full node knows about, including those it
// has rejected.
//
// The rejected ones are the interesting ones: a node that has seen a block from
// the other chain and refused it is direct evidence of divergence from the user's
// own node, needing no agreement from any peer.
type ChainTip struct {
	Hash   chainhash.Hash
	Height int32
	// BranchLen is how many blocks this tip is from the active chain, and 0 for
	// the active tip itself.
	BranchLen int32
	// Status is the node's own word for it: active, invalid, headers-only,
	// valid-headers, valid-fork, or unknown.
	Status string
}

// Chain tip statuses, as full nodes report them.
const (
	// TipActive is the node's current best chain.
	TipActive = "active"
	// TipInvalid means the node checked this branch and rejected it. The strongest
	// evidence of a rule disagreement that exists.
	TipInvalid = "invalid"
	// TipHeadersOnly means headers are known but the blocks were never fetched.
	TipHeadersOnly = "headers-only"
	// TipValidHeaders means the headers check out but the blocks were not
	// validated.
	TipValidHeaders = "valid-headers"
	// TipValidFork means a fully validated branch that is not the best chain.
	TipValidFork = "valid-fork"
	// TipUnknown means the node would not say.
	TipUnknown = "unknown"
)

// Rejected reports whether the node has actively refused this branch, as opposed
// to merely not having pursued it.
func (t ChainTip) Rejected() bool { return t.Status == TipInvalid }

// Deployment is a soft fork's state as the user's own node reports it.
//
// Read from the node rather than configured: it cannot go stale, it is that
// node's own truth rather than a value copied from a document, and it generalises
// to any future fork deployed the same way.
type Deployment struct {
	Name string
	// Type is how the fork activates, such as a miner-signalled deployment or one
	// buried at a fixed height.
	Type string
	// Active reports whether the rules are being enforced yet.
	Active bool
	// Bit is the version bit miners set to signal support.
	Bit int32
	// StartTime and Timeout bound the signalling period, in unix seconds.
	StartTime int64
	Timeout   int64
	// MinActivationHeight and MaxActivationHeight bound when the rules can begin
	// to bind.
	MinActivationHeight int32
	MaxActivationHeight int32
	// Status is the node's word for where the deployment has got to: defined,
	// started, locked_in, active, or failed.
	Status string
	// Since is the height at which the current status began.
	Since int32

	// Period is the signalling window length in blocks, and Elapsed, Count and
	// Threshold describe support within the current one. Zero when the node does
	// not report them.
	//
	// Worth surfacing during a run-up: the share of blocks signalling is the one
	// number that tells a user how likely a split actually is.
	Period    int32
	Elapsed   int32
	Count     int32
	Threshold int32
}

// Errors returned by backends.
var (
	// ErrUnsupported means this backend cannot do that at all — a light client
	// asked for the memory pool, for instance. A permanent answer, not a failure:
	// callers degrade rather than retry.
	ErrUnsupported = errors.New("chainview: operation unsupported by backend")
	// ErrNotFound means the backend does not have it. Includes a pruned node asked
	// for an old block, which callers must treat as a health problem rather than a
	// crash.
	ErrNotFound = errors.New("chainview: not found")
	// ErrWrongNetwork means the backend is on a different network entirely.
	ErrWrongNetwork = errors.New("chainview: backend is on the wrong network")
)
