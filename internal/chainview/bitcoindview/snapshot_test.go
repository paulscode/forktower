package bitcoindview

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/paulscode/forktower/internal/bootstrap"
)

// ChainInfo reads two calls, and the second one is the reason it exists.
//
// **A node that has loaded a snapshot reports the snapshot chainstate's height
// in `getblockchaininfo`.** That is exactly the number that would make the
// bootstrap conclude it had nothing left to do — for the right reason, but by
// reading a field that means something else. Asking `getchainstates` is what
// distinguishes "already there" from "took the shortcut".
func TestChainInfoAsksWhetherASnapshotWasAlreadyLoaded(t *testing.T) {
	node := newFakeNode(t).
		reply("getblockchaininfo", map[string]any{
			"chain":   "main",
			"blocks":  935_100,
			"headers": 940_000,
		}).
		reply("getchainstates", map[string]any{
			"headers": 940_000,
			"chainstates": []any{
				map[string]any{"blocks": 120_000},
				map[string]any{
					"blocks":             935_100,
					"snapshot_blockhash": "0000000000000000000147034958af1652b2b91bba607beacc5e72a56f0fb5ee",
				},
			},
		})

	got, err := newTestView(t, node).ChainInfo(context.Background())
	if err != nil {
		t.Fatalf("ChainInfo: %v", err)
	}
	if got.Network != "main" || got.Blocks != 935_100 || got.Headers != 940_000 {
		t.Errorf("ChainInfo = %+v", got)
	}
	if !got.SnapshotLoaded {
		t.Error("a node with a snapshot chainstate was reported as not having one")
	}
}

func TestANodeWithNoSnapshotSaysSo(t *testing.T) {
	node := newFakeNode(t).
		reply("getblockchaininfo", map[string]any{
			"chain": "main", "blocks": 120_000, "headers": 940_000,
		}).
		reply("getchainstates", map[string]any{
			"headers":     940_000,
			"chainstates": []any{map[string]any{"blocks": 120_000}},
		})

	got, err := newTestView(t, node).ChainInfo(context.Background())
	if err != nil {
		t.Fatalf("ChainInfo: %v", err)
	}
	if got.SnapshotLoaded {
		t.Error("an ordinary node was reported as having taken the shortcut")
	}
}

// A node too old for `getchainstates` answers "no snapshot" rather than failing.
//
// The call arrived in Bitcoin Core 26 and the bundled node is far newer — but the
// second node can be somebody else's, and refusing to work because a perfectly
// healthy node cannot answer an optional question would be the wrong trade.
func TestANodeTooOldForTheQuestionIsNotTreatedAsBroken(t *testing.T) {
	node := newFakeNode(t).
		reply("getblockchaininfo", map[string]any{
			"chain": "main", "blocks": 120_000, "headers": 940_000,
		}).
		fail("getchainstates", codeMethodNotFound, "Method not found")

	got, err := newTestView(t, node).ChainInfo(context.Background())
	if err != nil {
		t.Fatalf("an older node made ChainInfo fail: %v", err)
	}
	if got.SnapshotLoaded {
		t.Error("a node that could not answer was assumed to have a snapshot")
	}
	if got.Blocks != 120_000 {
		t.Errorf("the rest of the answer was lost: %+v", got)
	}
}

// Any other failure of that call is reported, because "cannot tell" and "no"
// are different, and only one of them is safe to act on.
func TestAnUnreadableChainstateAnswerIsAnError(t *testing.T) {
	node := newFakeNode(t).
		reply("getblockchaininfo", map[string]any{
			"chain": "main", "blocks": 1, "headers": 2,
		}).
		fail("getchainstates", -1, "something went wrong")

	if _, err := newTestView(t, node).ChainInfo(context.Background()); err == nil {
		t.Error("a node that could not answer at all was treated as having no snapshot")
	}
}

func TestChainInfoReportsAnUnreachableNode(t *testing.T) {
	node := newFakeNode(t).fail("getblockchaininfo", -1, "no")
	if _, err := newTestView(t, node).ChainInfo(context.Background()); err == nil {
		t.Error("a node that would not answer produced a usable ChainInfo")
	}
}

func TestLoadSnapshotPassesThePathAndReturnsWhatTheNodeSaid(t *testing.T) {
	var gotPath string
	node := newFakeNode(t).on("loadtxoutset",
		func(params []json.RawMessage) (any, *rpcError) {
			if len(params) != 1 {
				t.Errorf("loadtxoutset was called with %d parameters", len(params))
				return nil, &rpcError{Code: -1, Message: "bad call"}
			}
			if err := json.Unmarshal(params[0], &gotPath); err != nil {
				t.Fatal(err)
			}
			return map[string]any{
				"coins_loaded": 164_241_311,
				"tip_hash":     "0000000000000000000147034958af1652b2b91bba607beacc5e72a56f0fb5ee",
				"base_height":  935_000,
				"path":         gotPath,
			}, nil
		})

	got, err := newTestView(t, node).LoadSnapshot(
		context.Background(), "/data/sq/utxo-snapshot.dat")
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if gotPath != "/data/sq/utxo-snapshot.dat" {
		t.Errorf("the node was given the path %q", gotPath)
	}
	if got.Coins != 164_241_311 || got.BaseHeight != 935_000 {
		t.Errorf("LoadSnapshot = %+v", got)
	}
}

// A node that read the file and rejected it is different from one that could not
// be reached, and the two want opposite responses: a transport failure is worth
// retrying, and a refusal will be repeated just as firmly.
func TestARefusedSnapshotIsDistinguishableFromAnUnreachableNode(t *testing.T) {
	node := newFakeNode(t).fail("loadtxoutset", -32603,
		"Unable to load UTXO snapshot: hash mismatch")

	_, err := newTestView(t, node).LoadSnapshot(context.Background(), "/tmp/x")
	if !errors.Is(err, ErrSnapshotRefused) {
		t.Fatalf("a node's refusal came back as %v, want ErrSnapshotRefused", err)
	}
	// The node's own message is the only real clue about why, so it is kept.
	if !strings.Contains(err.Error(), "hash mismatch") {
		t.Errorf("the node's explanation was dropped: %v", err)
	}
}

func TestATransportFailureLoadingASnapshotIsNotARefusal(t *testing.T) {
	// Port 1 on loopback: reliably refused at the socket, so nothing that could
	// be mistaken for the node's own opinion ever comes back.
	view, err := New(Options{RPCURL: "http://127.0.0.1:1", User: "u", Pass: "p"})
	if err != nil {
		t.Fatal(err)
	}

	_, err = view.LoadSnapshot(context.Background(), "/tmp/x")
	if err == nil {
		t.Fatal("a node that was never reached reported success")
	}
	if errors.Is(err, ErrSnapshotRefused) {
		t.Error("a node that could not be reached was reported as having refused " +
			"the file, which is the one classification that would stop it being " +
			"retried")
	}
}

// The View has to satisfy the interface the bootstrap works against, or the
// shortcut is silently never offered — the wiring type-asserts, so a signature
// that drifted would fail at run time and nowhere else.
func TestTheViewSatisfiesTheBootstrapNodeInterface(t *testing.T) {
	var _ bootstrap.Node = (*View)(nil)
}
