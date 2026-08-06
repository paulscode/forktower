package bootstrap

import "fmt"

// NodeState is everything about the second node that bears on whether a snapshot
// would help it.
//
// Gathered by the caller and passed in, so that the decision below is a function
// of stated facts and nothing else. The whole point of separating this out is
// that "should we download nine gigabytes onto somebody's machine" is answerable
// in a unit test.
type NodeState struct {
	// Network is what the node calls its chain: "main", "test", "regtest".
	Network string
	// Blocks is how many it has validated, and Headers how many it knows of.
	Blocks  int32
	Headers int32
	// SnapshotLoaded is true when the node already has a snapshot chainstate,
	// whether or not its background validation has finished.
	SnapshotLoaded bool
	// FreeBytes is the space available where the file would be staged. Negative
	// means it could not be determined, which is not treated as a refusal: a
	// download that runs out of disk fails plainly and loses only time, whereas
	// refusing on a number nobody could read would deny the shortcut to somebody
	// who has plenty of room.
	FreeBytes int64
	// StagedBytes is how much of the file is already on disk from an earlier,
	// interrupted attempt.
	StagedBytes int64
}

// Chain names, as a Bitcoin node reports them. Constants because the comparison
// against what a node says is what stops a mainnet UTXO set being loaded into a
// node on another network, and a typo in that string would silently allow it.
const (
	ChainMain    = "main"
	ChainRegtest = "regtest"
	ChainSignet  = "signet"
)

// Reason codes. Stable and machine-readable; the sentence beside them is what a
// user reads, and it never contains one of these.
const (
	ReasonUsable        = "usable"
	ReasonWrongNetwork  = "wrong_network"
	ReasonAlreadyPast   = "already_past"
	ReasonAlreadyLoaded = "already_loaded"
	ReasonNotEnoughDisk = "not_enough_disk"
)

// Assessment is whether this snapshot is worth offering, and why not when it is
// not.
type Assessment struct {
	Usable bool
	Code   string
	// Reason is addressed to the user and says what it means for them, not what
	// was checked. Empty when Usable.
	Reason string
	// NeedBytes is the free space this would require. Reported whether or not
	// there is enough, because "how much do I have to clear" is the first thing
	// somebody asks after being told there is not.
	NeedBytes int64
}

// DiskMargin is the headroom demanded on top of the download itself.
//
// **The staged file is the only extra space this path needs.** The chainstate
// the node builds afterwards it would have built anyway, by whichever route it
// reached that height, so demanding room for it here would make the fast path
// look more expensive than the slow one it replaces. What the margin covers is
// the blocks the node keeps downloading while the file is being fetched, which
// happens concurrently and would otherwise be able to fill the disk out from
// under a download that had almost finished.
const DiskMargin = 2 << 30

// Assess reports whether loading this snapshot would help this node.
//
// Order matters: the checks that mean "this will never work" come before the one
// that means "not right now", so a user on a test network is told that rather
// than being told to free up disk space for something that would then be
// refused.
func Assess(s Snapshot, n NodeState) Assessment {
	need := s.TotalBytes() - n.StagedBytes + DiskMargin
	if need < 0 {
		need = 0
	}

	switch {
	case n.Network != s.Network:
		return Assessment{
			Code: ReasonWrongNetwork, NeedBytes: need,
			Reason: fmt.Sprintf(
				"This shortcut only exists for the main Bitcoin network, and the "+
					"second node is on %s.", networkName(n.Network)),
		}

	case n.SnapshotLoaded:
		return Assessment{
			Code: ReasonAlreadyLoaded, NeedBytes: need,
			Reason: "The second node has already taken this shortcut. It is " +
				"filling in the earlier history in the background.",
		}

	// Strictly greater: a node sitting exactly at the base height has validated
	// that block, so the snapshot would tell it nothing it does not know, and
	// Core refuses it.
	case n.Blocks >= s.BaseHeight:
		return Assessment{
			Code: ReasonAlreadyPast, NeedBytes: need,
			Reason: fmt.Sprintf(
				"The second node is already past block %s, so there is nothing "+
					"left for this to skip.", Commas(int64(s.BaseHeight))),
		}

	case n.FreeBytes >= 0 && n.FreeBytes < need:
		return Assessment{
			Code: ReasonNotEnoughDisk, NeedBytes: need,
			Reason: fmt.Sprintf(
				"This needs about %s free while it runs, and there is %s. The "+
					"space is given back as soon as the shortcut is taken — it is "+
					"the download that needs room, not the result.",
				HumanBytes(need), HumanBytes(n.FreeBytes)),
		}

	default:
		return Assessment{Usable: true, Code: ReasonUsable, NeedBytes: need}
	}
}

// ReadyToLoad reports whether the node can accept the snapshot yet.
//
// **Separate from Assess, because the answer changes while the download runs and
// the download must not wait for it.** Core refuses a snapshot whose base block
// is not already in its header chain, and headers arrive in minutes where the
// file takes hours — so the two proceed at once and this is checked at the end.
// Making the download wait for headers would add the slower of the two to the
// faster instead of hiding it.
func ReadyToLoad(s Snapshot, n NodeState) (ready bool, waitingFor string) {
	if n.Headers < s.BaseHeight {
		return false, fmt.Sprintf(
			"The second node has not yet seen the block headers up to %s. That "+
				"usually takes a few minutes.", Commas(int64(s.BaseHeight)))
	}
	return true, ""
}

// networkName says which chain a node is on in words a sentence can hold.
func networkName(chain string) string {
	const someTestNetwork = "a test network"
	switch chain {
	case ChainMain:
		return "the main network"
	case "test", "testnet3", "test4", "testnet4":
		return someTestNetwork
	case ChainSignet:
		return ChainSignet
	case ChainRegtest:
		return "a private regression-test network"
	case "":
		return "a network it did not name"
	default:
		return chain
	}
}
