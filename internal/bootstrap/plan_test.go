package bootstrap

import (
	"strings"
	"testing"
)

// testSnapshot is a small stand-in with the same shape as the real one.
func testSnapshot() Snapshot {
	return Snapshot{
		Network:    "main",
		BaseHeight: 935_000,
		BaseHash:   strings.Repeat("0", 64),
		BaseURL:    "https://example.invalid/x/",
		Parts: []Part{
			{Name: "a.00.part", Bytes: 1000},
			{Name: "a.01.part", Bytes: 500},
		},
	}
}

// healthy is a node the shortcut would help: mainnet, far behind, plenty of room.
func healthy() NodeState {
	return NodeState{
		Network:   "main",
		Blocks:    120_000,
		Headers:   940_000,
		FreeBytes: 100 << 30,
	}
}

func TestAssessOffersTheShortcutToANodeThatWouldBenefit(t *testing.T) {
	got := Assess(testSnapshot(), healthy())
	if !got.Usable {
		t.Fatalf("a fresh mainnet node was refused the shortcut: %s", got.Reason)
	}
	if got.Code != ReasonUsable {
		t.Errorf("Code = %q, want %q", got.Code, ReasonUsable)
	}
	if got.Reason != "" {
		t.Errorf("a usable assessment carries a complaint: %q", got.Reason)
	}
}

// The refusals, and the order they are decided in.
//
// Order is behaviour here, not tidiness. Somebody on a test network must be told
// that rather than being told to free up disk space for a shortcut that would
// then be refused for a different reason entirely.
func TestAssessRefusesForTheMostUsefulReason(t *testing.T) {
	s := testSnapshot()

	cases := []struct {
		name     string
		node     func(NodeState) NodeState
		wantCode string
		wantWord string
	}{
		{
			name: "a node on a test network",
			node: func(n NodeState) NodeState {
				n.Network = "regtest"
				// Also out of disk and past the height, to prove which wins.
				n.FreeBytes = 0
				n.Blocks = 999_999
				return n
			},
			wantCode: ReasonWrongNetwork,
			wantWord: "main Bitcoin network",
		},
		{
			name: "a node that has already taken it",
			node: func(n NodeState) NodeState {
				n.SnapshotLoaded = true
				n.FreeBytes = 0
				return n
			},
			wantCode: ReasonAlreadyLoaded,
			wantWord: "already taken",
		},
		{
			name: "a node already past the snapshot's height",
			node: func(n NodeState) NodeState {
				n.Blocks = 935_001
				return n
			},
			wantCode: ReasonAlreadyPast,
			wantWord: "nothing left",
		},
		{
			name: "a node exactly at the snapshot's height",
			node: func(n NodeState) NodeState {
				// Core refuses a snapshot at a height it has already validated,
				// so "at" must be refused as firmly as "past". An off-by-one here
				// would hand the node a file it is certain to reject, after the
				// entire download.
				n.Blocks = 935_000
				return n
			},
			wantCode: ReasonAlreadyPast,
		},
		{
			name: "a node with nowhere to put it",
			node: func(n NodeState) NodeState {
				n.FreeBytes = 100
				return n
			},
			wantCode: ReasonNotEnoughDisk,
			wantWord: "free while it runs",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Assess(s, c.node(healthy()))
			if got.Usable {
				t.Fatal("the shortcut was offered when it should not have been")
			}
			if got.Code != c.wantCode {
				t.Errorf("Code = %q, want %q (reason: %s)", got.Code, c.wantCode, got.Reason)
			}
			if c.wantWord != "" && !strings.Contains(got.Reason, c.wantWord) {
				t.Errorf("the reason given was %q, which does not mention %q",
					got.Reason, c.wantWord)
			}
			if got.Reason == "" {
				t.Error("a refusal with no reason leaves the user with nothing to act on")
			}
		})
	}
}

// A free-space figure nobody could read must not deny the shortcut.
//
// Statfs fails on some filesystems and in some sandboxes. Treating "unknown" as
// "zero" would refuse a machine with a terabyte spare, and the failure mode of
// guessing wrong the other way is a download that stops with a plain error.
func TestUnknownFreeSpaceDoesNotRefuse(t *testing.T) {
	n := healthy()
	n.FreeBytes = -1
	if got := Assess(testSnapshot(), n); !got.Usable {
		t.Errorf("an unreadable free-space figure refused the shortcut: %s", got.Reason)
	}
}

// The requirement shrinks as the download progresses, because a resumed transfer
// only needs room for what is left.
func TestTheSpaceNeededCountsWhatIsAlreadyDownloaded(t *testing.T) {
	s := testSnapshot()
	n := healthy()

	fresh := Assess(s, n).NeedBytes
	n.StagedBytes = 1000
	resumed := Assess(s, n).NeedBytes

	if resumed != fresh-1000 {
		t.Errorf("with 1000 bytes already fetched the requirement went from %d to "+
			"%d; it should have dropped by exactly 1000", fresh, resumed)
	}
	if fresh != s.TotalBytes()+DiskMargin {
		t.Errorf("a fresh start asks for %d, want the whole file plus the margin (%d)",
			fresh, s.TotalBytes()+DiskMargin)
	}
}

// How much is needed is reported even when there is not enough, because that is
// the first thing somebody asks after being told to clear space.
func TestTheRequirementIsReportedEvenWhenItCannotBeMet(t *testing.T) {
	n := healthy()
	n.FreeBytes = 1
	got := Assess(testSnapshot(), n)
	if got.NeedBytes <= 0 {
		t.Error("a refusal for want of disk did not say how much was wanted")
	}
}

// ReadyToLoad is separate from Assess precisely so the download does not wait for
// headers. If these two were one answer, a fresh node would sit idle for the
// minutes its headers take before starting a transfer that takes hours.
func TestReadyToLoadWaitsOnlyForTheHeaders(t *testing.T) {
	s := testSnapshot()

	if ok, why := ReadyToLoad(s, NodeState{Headers: 934_999}); ok {
		t.Error("a node whose headers stop short of the base block was said to be ready")
	} else if !strings.Contains(why, "935,000") {
		t.Errorf("the explanation %q does not name the height being waited for", why)
	}

	if ok, _ := ReadyToLoad(s, NodeState{Headers: 935_000}); !ok {
		t.Error("headers exactly at the base block were treated as not enough")
	}

	// And it says nothing about disk, network or how far the node has validated
	// — those are Assess's business, and duplicating them here would let the two
	// disagree.
	if ok, _ := ReadyToLoad(s, NodeState{Headers: 940_000, FreeBytes: 0, Network: "regtest"}); !ok {
		t.Error("ReadyToLoad refused for a reason that is not its to judge")
	}
}

func TestNetworkNamesReadAsSentences(t *testing.T) {
	for chain, want := range map[string]string{
		"main":    "the main network",
		"test":    "a test network",
		"testnet": "testnet",
		"signet":  "signet",
		"regtest": "a private regression-test network",
		"":        "a network it did not name",
	} {
		if got := networkName(chain); got != want {
			t.Errorf("networkName(%q) = %q, want %q", chain, got, want)
		}
	}
}
