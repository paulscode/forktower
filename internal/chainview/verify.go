package chainview

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/btcsuite/btcd/chainhash/v2"
)

// Identity is what distinguishes one node from another.
//
// Needed because the entire product rests on having two *independent* views. Two
// configurations pointing at the same node produce views that agree by
// construction, so divergence becomes unrepresentable: the daemon would report
// everything healthy, forever, while watching nothing. That failure is silent,
// plausible — a single mis-wired environment variable does it — and produces
// maximum confidence, which is why it is checked explicitly rather than assumed.
type Identity struct {
	// Endpoint is where this view connects, normalised for comparison.
	Endpoint string
	// LocalAddresses are the addresses the node believes it listens on.
	LocalAddresses []string
	// Subversion is the node's self-reported software string, which also carries
	// any operator comment.
	Subversion string
}

// Identifiable is implemented by backends that can describe the node behind them.
type Identifiable interface {
	Identity(ctx context.Context) (Identity, error)
}

// ChainTipper is implemented by backends that can report every branch tip they
// know of, including the ones they have rejected.
type ChainTipper interface {
	ChainTips(ctx context.Context) ([]ChainTip, error)
}

// Deployer is implemented by backends that can report a soft fork's state.
type Deployer interface {
	Deployment(ctx context.Context, name string) (*Deployment, error)
}

// Verification errors.
var (
	// ErrSameNode means two views are talking to one node, so they cannot disagree.
	ErrSameNode = errors.New("chainview: both views are the same node")
	// ErrCannotVerifyDistinct means the backends cannot describe themselves well
	// enough to prove they differ. Distinct from having proven they are the same:
	// the first is a gap in what we can check, the second is a fault.
	ErrCannotVerifyDistinct = errors.New("chainview: cannot establish that the views are different nodes")
	// ErrWrongBranch means a view is not following the chain it is supposed to.
	ErrWrongBranch = errors.New("chainview: view is following the wrong chain")
	// ErrCannotVerifyBranch means there is not yet enough history to judge. Not a
	// fault: a view that has not reached the separation point cannot be checked
	// against it.
	ErrCannotVerifyBranch = errors.New("chainview: not enough history to verify the branch")
)

// VerifyNetwork asserts that a view is on the expected network.
//
// Compares the genesis hash and nothing else. That is sufficient and better than
// comparing the network name: a name is a label the node reports, while the
// genesis hash is the chain's identity, and it needs nothing beyond the interface.
//
// Run for every view before any scanning. A mismatch is fatal rather than
// degraded: a backend pointed at a test network answers every request correctly
// and diverges from the user's node permanently, so nothing downstream could tell
// it apart from a genuine chain split. Refusing to start beats spending a week
// confidently watching the wrong network.
func VerifyNetwork(ctx context.Context, v ChainView, want NetworkParams) error {
	genesis, err := v.BlockHashByHeight(ctx, 0)
	if err != nil {
		return fmt.Errorf("reading the first block to identify the network: %w", err)
	}
	if genesis != want.Genesis {
		return fmt.Errorf(
			"view is on a different network: its first block is %s, but %s starts with %s: %w",
			genesis, describeNetwork(want.Name), want.Genesis, ErrWrongNetwork)
	}
	return nil
}

func describeNetwork(name string) string {
	if name == "" {
		return "the expected network"
	}
	return name
}

// VerifyDistinct proves that two views are different nodes.
//
// Returns ErrSameNode when they are provably the same, and
// ErrCannotVerifyDistinct when the backends cannot say enough to tell. The caller
// must treat those differently: the first means the daemon is watching nothing and
// must stop, the second means one check is unavailable and should be reported as
// such rather than passed off as success.
func VerifyDistinct(ctx context.Context, sf, sq ChainView) error {
	sfID, sqID, ok := identities(ctx, sf, sq)
	if !ok {
		return ErrCannotVerifyDistinct
	}

	if sfID.Endpoint != "" && sfID.Endpoint == sqID.Endpoint {
		return fmt.Errorf("both views are configured to reach %s: %w", sfID.Endpoint, ErrSameNode)
	}

	// The endpoints can differ while still reaching one node — two names for one
	// host, or a proxy. What the node says about itself settles it.
	if shared := sharedAddress(sfID.LocalAddresses, sqID.LocalAddresses); shared != "" {
		return fmt.Errorf(
			"both views reach a node listening on %s, so they are the same node: %w",
			shared, ErrSameNode)
	}

	return nil
}

func identities(ctx context.Context, sf, sq ChainView) (sfID, sqID Identity, ok bool) {
	sfIdent, sfOK := sf.(Identifiable)
	sqIdent, sqOK := sq.(Identifiable)
	if !sfOK || !sqOK {
		return Identity{}, Identity{}, false
	}
	var err error
	if sfID, err = sfIdent.Identity(ctx); err != nil {
		return Identity{}, Identity{}, false
	}
	if sqID, err = sqIdent.Identity(ctx); err != nil {
		return Identity{}, Identity{}, false
	}
	return sfID, sqID, true
}

// sharedAddress returns an address both nodes claim to listen on, if any.
func sharedAddress(a, b []string) string {
	if len(a) == 0 || len(b) == 0 {
		return ""
	}
	set := make(map[string]struct{}, len(a))
	for _, addr := range a {
		if norm := strings.TrimSpace(strings.ToLower(addr)); norm != "" {
			set[norm] = struct{}{}
		}
	}
	for _, addr := range b {
		norm := strings.TrimSpace(strings.ToLower(addr))
		if norm == "" {
			continue
		}
		if _, found := set[norm]; found {
			return norm
		}
	}
	return ""
}

// branchSampleHeights picks the heights at which two views are compared below the
// anchor.
//
// Sampled rather than walked: two hash lookups per height is cheap enough to
// repeat on a schedule, and repetition is what catches a backend that drifts later
// — which a one-off check at startup never would. Spread across the range so a
// disagreement anywhere in shared history is likely to be caught, with the anchor
// itself always included because that is the height that matters most.
func branchSampleHeights(anchor int32) []int32 {
	if anchor < 0 {
		return nil
	}
	candidates := []int32{anchor, anchor - 1, anchor / 2, anchor / 4, anchor / 8, 1000, 1, 0}

	seen := make(map[int32]struct{}, len(candidates))
	out := make([]int32, 0, len(candidates))
	for _, h := range candidates {
		if h < 0 || h > anchor {
			continue
		}
		if _, dup := seen[h]; dup {
			continue
		}
		seen[h] = struct{}{}
		out = append(out, h)
	}
	return out
}

// VerifyBranch confirms that sq follows the chain the user's node does not.
//
// Two halves, both needed:
//
//   - Agreement below the anchor. History from before the chains could possibly
//     disagree is shared, so any difference there means the backend is on another
//     chain, another network, or being shown a fabrication.
//
//   - Divergence above the separation point. Once that point is known, the two
//     views must actually differ just above it. A backend that has quietly followed
//     the same chain as the user's node is the failure this catches, and it is worse
//     than having no backend at all: it produces a clean report about a chain that
//     needs no watching, while the exposure it was installed to find goes unseen.
//
// anchorHeight is capped strictly below the height at which the two rule sets
// could first disagree. fork may be nil before a separation point is known, in
// which case only the first half applies.
//
// Returns ErrCannotVerifyBranch when a view has not enough history to judge, which
// callers report as an unavailable check rather than a failure.
func VerifyBranch(ctx context.Context, sf, sq ChainView, anchorHeight int32, fork *BlockRef) error {
	if err := verifySharedHistory(ctx, sf, sq, anchorHeight); err != nil {
		return err
	}
	if fork == nil {
		return nil
	}
	return verifyDivergence(ctx, sf, sq, *fork)
}

func verifySharedHistory(ctx context.Context, sf, sq ChainView, anchorHeight int32) error {
	for _, h := range branchSampleHeights(anchorHeight) {
		sfHash, err := sf.BlockHashByHeight(ctx, h)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				// The view does not reach this height yet. Not a disagreement.
				continue
			}
			return fmt.Errorf("reading height %d from the chain your node follows: %w", h, err)
		}
		sqHash, err := sq.BlockHashByHeight(ctx, h)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return fmt.Errorf("reading height %d from the other chain: %w", h, err)
		}
		if sfHash != sqHash {
			return fmt.Errorf(
				"the two views disagree at height %d, which is below where the rules could "+
					"differ: your node has %s and the other view has %s. Shared history cannot "+
					"differ, so the second view is on a different chain or network: %w",
				h, sfHash, sqHash, ErrWrongBranch)
		}
	}
	return nil
}

func verifyDivergence(ctx context.Context, sf, sq ChainView, fork BlockRef) error {
	check := fork.Height + 1

	sfHash, sfErr := sf.BlockHashByHeight(ctx, check)
	sqHash, sqErr := sq.BlockHashByHeight(ctx, check)

	switch {
	case errors.Is(sfErr, ErrNotFound) || errors.Is(sqErr, ErrNotFound):
		// One side has not built past the separation yet, so there is nothing to
		// compare. Expected during the first blocks of a split.
		return fmt.Errorf("neither view has reached height %d yet: %w", check, ErrCannotVerifyBranch)
	case sfErr != nil:
		return fmt.Errorf("reading height %d from the chain your node follows: %w", check, sfErr)
	case sqErr != nil:
		return fmt.Errorf("reading height %d from the other chain: %w", check, sqErr)
	}

	if sfHash == sqHash {
		return fmt.Errorf(
			"the other view has the same block %s at height %d as your own node, so it is "+
				"following the same chain and is watching nothing: %w",
			sfHash, check, ErrWrongBranch)
	}
	return nil
}

// GenesisOf reads a view's first block, for building NetworkParams from a node
// already known to be correct.
func GenesisOf(ctx context.Context, v ChainView) (chainhash.Hash, error) {
	h, err := v.BlockHashByHeight(ctx, 0)
	if err != nil {
		return chainhash.Hash{}, fmt.Errorf("reading the first block: %w", err)
	}
	return h, nil
}
