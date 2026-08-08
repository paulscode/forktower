package chainview_test

import (
	"context"
	"errors"
	"testing"

	"github.com/paulscode/forktower/internal/chainview"
	"github.com/paulscode/forktower/internal/chainview/chainviewtest"
)

func TestVerifyNetwork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	view := chainviewtest.New("mainnet")
	genesis, err := chainview.GenesisOf(ctx, view)
	if err != nil {
		t.Fatal(err)
	}

	if err := chainview.VerifyNetwork(ctx, view, chainview.NetworkParams{
		Name: "main", Genesis: genesis,
	}); err != nil {
		t.Errorf("a view on the expected network was rejected: %v", err)
	}

	// A view on another network answers every request correctly and diverges
	// permanently, which would read as a chain split rather than a
	// misconfiguration — so this must be caught before anything is watched.
	err = chainview.VerifyNetwork(ctx, view, chainview.NetworkParams{
		Name: "main", Genesis: chainviewtest.TaggedHash("genesis-elsewhere", 0),
	})
	if !errors.Is(err, chainview.ErrWrongNetwork) {
		t.Errorf("got %v, want ErrWrongNetwork", err)
	}
	if err != nil && !contains(err.Error(), "main") {
		t.Errorf("the error does not name the expected network: %v", err)
	}
}

func TestVerifyDistinct(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cases := []struct {
		name    string
		sfID    chainview.Identity
		sqID    chainview.Identity
		wantErr error
		why     string
	}{
		{
			name: "different nodes",
			sfID: chainview.Identity{Endpoint: "http://a:8332", LocalAddresses: []string{"a:8333"}},
			sqID: chainview.Identity{Endpoint: "http://b:8432", LocalAddresses: []string{"b:8433"}},
			why:  "the ordinary, correct configuration",
		},
		{
			name:    "identical endpoints",
			sfID:    chainview.Identity{Endpoint: "http://a:8332"},
			sqID:    chainview.Identity{Endpoint: "http://a:8332"},
			wantErr: chainview.ErrSameNode,
			why:     "one mis-wired setting produces exactly this",
		},
		{
			name: "different names, one node",
			sfID: chainview.Identity{
				Endpoint: "http://127.0.0.1:8332", LocalAddresses: []string{"node:8333"},
			},
			sqID: chainview.Identity{
				Endpoint: "http://localhost:8332", LocalAddresses: []string{"node:8333"},
			},
			wantErr: chainview.ErrSameNode,
			why:     "comparing endpoints alone would miss this",
		},
		{
			name: "address comparison ignores case and spacing",
			sfID: chainview.Identity{
				Endpoint: "http://a:8332", LocalAddresses: []string{" NODE:8333 "},
			},
			sqID: chainview.Identity{
				Endpoint: "http://b:8432", LocalAddresses: []string{"node:8333"},
			},
			wantErr: chainview.ErrSameNode,
			why:     "a node's own reporting is not normalised for us",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sf, sq := chainviewtest.NewSharedHistory(1)
			sf.SetIdentity(tc.sfID)
			sq.SetIdentity(tc.sqID)

			err := chainview.VerifyDistinct(ctx, sf, sq)
			if tc.wantErr == nil {
				if err != nil {
					t.Errorf("got %v, want no error — %s", err, tc.why)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("got %v, want %v — %s", err, tc.wantErr, tc.why)
			}
		})
	}
}

// A backend that cannot describe itself leaves the check unavailable, which is a
// different thing from having proven the views are distinct. Conflating them would
// report a check as passed that was never performed.
func TestVerifyDistinctReportsWhenItCannotTell(t *testing.T) {
	t.Parallel()

	sf, sq := chainviewtest.NewSharedHistory(1)
	sq.Fail("Identity", errors.New("cannot say"))

	err := chainview.VerifyDistinct(context.Background(), sf, sq)
	if !errors.Is(err, chainview.ErrCannotVerifyDistinct) {
		t.Errorf("got %v, want ErrCannotVerifyDistinct", err)
	}
}

func TestVerifyBranchAcceptsGenuinelyDifferentChains(t *testing.T) {
	t.Parallel()

	sf, sq := chainviewtest.NewSharedHistory(100)
	sf.Extend("ours", 10)
	sq.Extend("theirs", 10)

	fork := &chainview.BlockRef{Hash: chainviewtest.TaggedHash("shared", 100), Height: 100}
	if err := chainview.VerifyBranch(context.Background(), sf, sq, 99, fork); err != nil {
		t.Errorf("two genuinely different chains were rejected: %v", err)
	}
}

// The failure this check exists for. A view that has quietly adopted the same
// chain as the user's own node is worse than none: it reports cleanly about a
// chain nobody needs watched while the exposure goes unseen.
func TestVerifyBranchCatchesAViewFollowingTheSameChain(t *testing.T) {
	t.Parallel()

	sf, sq := chainviewtest.NewSharedHistory(100)
	sf.Extend("ours", 10)
	sq.Extend("ours", 10) // the same blocks

	fork := &chainview.BlockRef{Hash: chainviewtest.TaggedHash("shared", 100), Height: 100}
	err := chainview.VerifyBranch(context.Background(), sf, sq, 99, fork)
	if !errors.Is(err, chainview.ErrWrongBranch) {
		t.Fatalf("got %v, want ErrWrongBranch", err)
	}
	if !contains(err.Error(), "watching nothing") {
		t.Errorf("the error does not explain the consequence: %v", err)
	}
}

// Shared history cannot differ. A disagreement below the anchor means the view is
// on another chain, another network, or being shown a fabrication.
func TestVerifyBranchCatchesADisagreementInSharedHistory(t *testing.T) {
	t.Parallel()

	sf, sq := chainviewtest.NewSharedHistory(100)
	// The second view rewrites history that both were supposed to share.
	sq.Reorg(50, "fabricated", 50)

	err := chainview.VerifyBranch(context.Background(), sf, sq, 99, nil)
	if !errors.Is(err, chainview.ErrWrongBranch) {
		t.Fatalf("got %v, want ErrWrongBranch", err)
	}
	if !contains(err.Error(), "Shared history cannot differ") {
		t.Errorf("the error does not explain why this is impossible: %v", err)
	}
}

// During the first blocks of a split there is nothing above the separation to
// compare. Not a fault, and not a pass either.
func TestVerifyBranchReportsWhenThereIsNotEnoughHistory(t *testing.T) {
	t.Parallel()

	sf, sq := chainviewtest.NewSharedHistory(100)
	sf.Extend("ours", 5)
	// The second view has not built past the separation at all.

	fork := &chainview.BlockRef{Hash: chainviewtest.TaggedHash("shared", 100), Height: 100}
	err := chainview.VerifyBranch(context.Background(), sf, sq, 99, fork)
	if !errors.Is(err, chainview.ErrCannotVerifyBranch) {
		t.Errorf("got %v, want ErrCannotVerifyBranch", err)
	}
}

func TestVerifyBranchWithoutASeparationPointChecksOnlySharedHistory(t *testing.T) {
	t.Parallel()

	sf, sq := chainviewtest.NewSharedHistory(100)
	sf.Extend("ours", 10)
	sq.Extend("ours", 10) // identical, but no separation point is known yet

	if err := chainview.VerifyBranch(context.Background(), sf, sq, 99, nil); err != nil {
		t.Errorf("without a known separation point only shared history applies: %v", err)
	}
}

// A view that has not synced far enough is unverifiable, which is neither wrong
// nor confirmed.
//
// Heights a view does not reach are skipped, and a view holding nothing but its
// first block skips every one of them. Reporting that as success would report a
// check as passed that compared nothing — and agreeing at genesis says only that
// both views are on the same network, which VerifyNetwork already establishes for
// every view before any of this runs.
func TestVerifyBranchNeedsSomethingAboveGenesisToCompare(t *testing.T) {
	t.Parallel()

	sf, _ := chainviewtest.NewSharedHistory(100)
	short := chainviewtest.New("shared")

	err := chainview.VerifyBranch(context.Background(), sf, short, 99, nil)
	if !errors.Is(err, chainview.ErrCannotVerifyBranch) {
		t.Errorf("got %v, want ErrCannotVerifyBranch", err)
	}
	if errors.Is(err, chainview.ErrWrongBranch) {
		t.Error("a view that has not synced that far was treated as wrong")
	}

	// Partway is enough, though. Reaching any shared height above genesis is a real
	// comparison, and waiting for a full sync would leave the check unavailable for
	// days on a fresh install.
	partial, _ := chainviewtest.NewSharedHistory(20)
	if err := chainview.VerifyBranch(context.Background(), sf, partial, 99, nil); err != nil {
		t.Errorf("a partially synced view reaching shared history was not accepted: %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// --- backends that misbehave --------------------------------------------------

var errBackend = errors.New("backend is unwell")

// plainView is a view that cannot describe the node behind it. Embedding the
// interface rather than the fake keeps Identity from being promoted, which is what
// an older or reduced backend looks like.
type plainView struct{ chainview.ChainView }

func TestVerifyDistinctWithABackendThatCannotDescribeItself(t *testing.T) {
	t.Parallel()

	cases := map[string]func(sf, sq *chainviewtest.View) (a, b chainview.ChainView){
		"the first cannot be asked": func(sf, sq *chainviewtest.View) (chainview.ChainView, chainview.ChainView) {
			return plainView{sf}, sq
		},
		"the second cannot be asked": func(sf, sq *chainviewtest.View) (chainview.ChainView, chainview.ChainView) {
			return sf, plainView{sq}
		},
		"the first refuses": func(sf, sq *chainviewtest.View) (chainview.ChainView, chainview.ChainView) {
			sf.Fail("Identity", errBackend)
			return sf, sq
		},
	}

	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sf, sq := chainviewtest.NewSharedHistory(1)
			a, b := setup(sf, sq)
			if err := chainview.VerifyDistinct(context.Background(), a, b); !errors.Is(err, chainview.ErrCannotVerifyDistinct) {
				t.Errorf("got %v, want ErrCannotVerifyDistinct", err)
			}
		})
	}
}

// A node that reports no addresses, or blank ones, leaves nothing to compare. That
// is not evidence of being the same node, so it must not be reported as one.
func TestVerifyDistinctWithNothingToCompare(t *testing.T) {
	t.Parallel()

	cases := map[string][2][]string{
		"neither reports an address": {nil, nil},
		"one reports none":           {{"a:8333"}, nil},
		"the addresses are blank":    {{"  "}, {""}},
	}

	for name, addrs := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sf, sq := chainviewtest.NewSharedHistory(1)
			sf.SetIdentity(chainview.Identity{Endpoint: "http://a:8332", LocalAddresses: addrs[0]})
			sq.SetIdentity(chainview.Identity{Endpoint: "http://b:8432", LocalAddresses: addrs[1]})

			if err := chainview.VerifyDistinct(context.Background(), sf, sq); err != nil {
				t.Errorf("got %v, but nothing here proves the views are one node", err)
			}
		})
	}
}

func TestVerifyNetworkWhenTheNodeCannotBeReached(t *testing.T) {
	t.Parallel()

	view := chainviewtest.New("mainnet")
	view.Fail("BlockHashByHeight", errBackend)

	err := chainview.VerifyNetwork(context.Background(), view, chainview.NetworkParams{Name: "main"})
	if !errors.Is(err, errBackend) {
		t.Errorf("got %v, want the backend's own error", err)
	}
	// Not a wrong network: unreachable and wrong are different problems, and the
	// caller retries one but refuses to start on the other.
	if errors.Is(err, chainview.ErrWrongNetwork) {
		t.Errorf("an unreachable node was reported as the wrong network: %v", err)
	}
}

func TestVerifyNetworkWithoutANameToReport(t *testing.T) {
	t.Parallel()

	view := chainviewtest.New("mainnet")
	err := chainview.VerifyNetwork(context.Background(), view, chainview.NetworkParams{
		Genesis: chainviewtest.TaggedHash("genesis-elsewhere", 0),
	})
	if !errors.Is(err, chainview.ErrWrongNetwork) {
		t.Fatalf("got %v, want ErrWrongNetwork", err)
	}
	if !contains(err.Error(), "the expected network") {
		t.Errorf("the message is not readable without a network name: %v", err)
	}
}

// A backend that errors is not a backend that disagrees. Reporting a transport
// fault as ErrWrongBranch would make the daemon refuse to start over a node that
// was merely busy.
func TestVerifyBranchDistinguishesFaultsFromDisagreement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	fork := &chainview.BlockRef{Hash: chainviewtest.TaggedHash("shared", 100), Height: 100}

	cases := map[string]struct {
		failSF, failSQ bool
		anchor         int32
	}{
		"the first fails in shared history":  {failSF: true, anchor: 99},
		"the second fails in shared history": {failSQ: true, anchor: 99},
		// A negative anchor means no shared history is checked at all, which is how
		// the divergence half is reached on its own.
		"the first fails above the separation":  {failSF: true, anchor: -1},
		"the second fails above the separation": {failSQ: true, anchor: -1},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sf, sq := chainviewtest.NewSharedHistory(100)
			sf.Extend("ours", 10)
			sq.Extend("theirs", 10)
			if tc.failSF {
				sf.Fail("BlockHashByHeight", errBackend)
			}
			if tc.failSQ {
				sq.Fail("BlockHashByHeight", errBackend)
			}

			err := chainview.VerifyBranch(ctx, sf, sq, tc.anchor, fork)
			if !errors.Is(err, errBackend) {
				t.Errorf("got %v, want the backend's own error", err)
			}
			if errors.Is(err, chainview.ErrWrongBranch) {
				t.Errorf("a backend fault was reported as the wrong chain: %v", err)
			}
		})
	}
}

func TestGenesisOfReportsWhatWentWrong(t *testing.T) {
	t.Parallel()

	view := chainviewtest.New("mainnet")
	view.Fail("BlockHashByHeight", errBackend)

	if _, err := chainview.GenesisOf(context.Background(), view); !errors.Is(err, errBackend) {
		t.Errorf("got %v, want the backend's own error", err)
	}
}

// Either view may be the shorter one — during initial sync, or when one node is
// catching up after downtime — and neither case is a disagreement. Symmetric on
// purpose: whichever is short, the answer is "cannot tell yet", never "wrong".
func TestVerifyBranchWhenEitherViewIsShort(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	long, _ := chainviewtest.NewSharedHistory(100)
	short := chainviewtest.New("shared")

	for name, err := range map[string]error{
		"the first view is short":  chainview.VerifyBranch(ctx, short, long, 99, nil),
		"the second view is short": chainview.VerifyBranch(ctx, long, short, 99, nil),
	} {
		if errors.Is(err, chainview.ErrWrongBranch) {
			t.Errorf("%s: a short view was treated as wrong: %v", name, err)
		}
		if !errors.Is(err, chainview.ErrCannotVerifyBranch) {
			t.Errorf("%s: got %v, want ErrCannotVerifyBranch", name, err)
		}
	}
}

// A low anchor makes the sampled heights collapse onto each other — on regtest, or
// early in a chain. Comparing the same height repeatedly is wasteful but harmless;
// what would not be harmless is sampling a height above the anchor.
func TestVerifyBranchWithVeryLittleHistory(t *testing.T) {
	t.Parallel()

	sf, sq := chainviewtest.NewSharedHistory(2)
	sq.Reorg(2, "theirs", 1) // differs only above the anchor

	if err := chainview.VerifyBranch(context.Background(), sf, sq, 2, nil); err != nil {
		t.Errorf("got %v, but the two agree everywhere at or below the anchor", err)
	}
}
