//go:build integration

package bitcoindview

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"

	"github.com/paulscode/forktower/internal/chainview"
)

// These run against a real node in a container, because the fixture tests prove
// only that this package agrees with its own idea of what a node returns. The
// shapes that matter — a header's fields, an error code, what happens when the
// same transaction is sent twice — are the node's to define, not ours.

const (
	nodeImage      = "lncm/bitcoind"
	nodeTag        = "v28.0"
	rpcUser        = "forktower"
	rpcPass        = "forktower-integration"
	readyTimeout   = 90 * time.Second
	throwawayLabel = "forktower-integration"
)

// startNode brings up a regtest node and returns a View onto it.
func startNode(t *testing.T) *View {
	t.Helper()

	pool, err := dockertest.NewPool("")
	if err != nil {
		t.Fatalf("connecting to docker: %v", err)
	}
	if err := pool.Client.Ping(); err != nil {
		t.Skipf("docker is not usable, skipping: %v", err)
	}

	resource, err := pool.RunWithOptions(&dockertest.RunOptions{
		Repository: nodeImage,
		Tag:        nodeTag,
		Cmd: []string{
			"-regtest=1",
			"-server=1",
			"-rpcbind=0.0.0.0",
			"-rpcallowip=0.0.0.0/0",
			"-rpcuser=" + rpcUser,
			"-rpcpassword=" + rpcPass,
			"-fallbackfee=0.0002",
			"-txindex=1",
		},
		ExposedPorts: []string{"18443/tcp"},
		Labels:       map[string]string{"created-by": throwawayLabel},
	}, func(hc *docker.HostConfig) {
		// The container is disposable and must not outlive a crashed test run.
		hc.AutoRemove = true
		hc.RestartPolicy = docker.RestartPolicy{Name: "no"}
	})
	if err != nil {
		t.Fatalf("starting %s:%s: %v", nodeImage, nodeTag, err)
	}
	t.Cleanup(func() {
		if err := pool.Purge(resource); err != nil {
			t.Logf("purging container: %v", err)
		}
	})

	v, err := New(Options{
		RPCURL:  fmt.Sprintf("http://127.0.0.1:%s", resource.GetPort("18443/tcp")),
		User:    rpcUser,
		Pass:    rpcPass,
		Timeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Wait for it to answer rather than sleeping a guessed interval.
	pool.MaxWait = readyTimeout
	if err := pool.Retry(func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		h, hErr := v.Health(ctx)
		if hErr != nil {
			return hErr
		}
		if h.State == chainview.HealthDown {
			return errors.New("node not answering yet")
		}
		return nil
	}); err != nil {
		t.Fatalf("node never became ready: %v", err)
	}
	return v
}

// ensureWallet creates a wallet if the node has none.
//
// Modern Core no longer creates one automatically, so every address-producing
// call fails without this. Found by running against a real node, which is what
// these tests are for.
func ensureWallet(t *testing.T, v *View) {
	t.Helper()
	ctx := context.Background()

	var names struct {
		Wallets []struct {
			Name string `json:"name"`
		} `json:"wallets"`
	}
	if err := v.c.call(ctx, &names, "listwalletdir"); err == nil && len(names.Wallets) > 0 {
		// Present but possibly not loaded; loading an already-loaded wallet is an
		// error worth ignoring.
		_ = v.c.call(ctx, nil, "loadwallet", names.Wallets[0].Name)
		return
	}
	if err := v.c.call(ctx, nil, "createwallet", "forktower-test"); err != nil {
		t.Fatalf("createwallet: %v", err)
	}
}

// generate mines n blocks to a fresh address.
func generate(t *testing.T, v *View, n int) {
	t.Helper()
	ctx := context.Background()
	ensureWallet(t, v)

	var addr string
	if err := v.c.call(ctx, &addr, "getnewaddress"); err != nil {
		t.Fatalf("getnewaddress: %v", err)
	}
	var hashes []string
	if err := v.c.call(ctx, &hashes, "generatetoaddress", n, addr); err != nil {
		t.Fatalf("generatetoaddress: %v", err)
	}
	if len(hashes) != n {
		t.Fatalf("mined %d blocks, want %d", len(hashes), n)
	}
}

func TestAgainstRealNode(t *testing.T) {
	v := startNode(t)
	ctx := context.Background()

	generate(t, v, 10)

	tip, err := v.BestBlock(ctx)
	if err != nil {
		t.Fatalf("BestBlock: %v", err)
	}
	if tip.Height != 10 {
		t.Fatalf("tip height = %d, want 10", tip.Height)
	}

	// Walk the chain back through both lookups, so the header decode and the
	// height lookup are checked against each other rather than in isolation.
	cursor := tip
	for h := int32(10); h >= 1; h-- {
		if cursor.Height != h {
			t.Fatalf("walked to height %d, want %d", cursor.Height, h)
		}

		byHeight, err := v.BlockHashByHeight(ctx, h)
		if err != nil {
			t.Fatalf("BlockHashByHeight(%d): %v", h, err)
		}
		if byHeight != cursor.Hash {
			t.Fatalf("height %d: hash by height %s, by walk %s", h, byHeight, cursor.Hash)
		}

		blk, err := v.Block(ctx, cursor.Hash)
		if err != nil {
			t.Fatalf("Block(%s): %v", cursor.Hash, err)
		}
		if got := blk.BlockHash(); got != cursor.Hash {
			t.Fatalf("block %s deserialised to %s — the decode is wrong", cursor.Hash, got)
		}
		if len(blk.Transactions) == 0 {
			t.Fatalf("block %d has no transactions, not even a coinbase", h)
		}

		prev, err := v.BlockHeaderByHash(ctx, cursor.PrevHash)
		if err != nil {
			t.Fatalf("BlockHeaderByHash(%s): %v", cursor.PrevHash, err)
		}
		cursor = prev
	}

	// Past the tip is not-found, which the sentinel must convey — callers branch
	// on it rather than treating it as a fault.
	if _, err := v.BlockHashByHeight(ctx, 10_000); !errors.Is(err, chainview.ErrNotFound) {
		t.Errorf("height past the tip returned %v, want ErrNotFound", err)
	}

	health, err := v.Health(ctx)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	// A fresh regtest node has no peers, which this backend reports as degraded:
	// no peers means no new blocks, so nothing would ever be detected.
	if health.State != chainview.HealthDegraded {
		t.Errorf("state = %q, want %q on a peerless regtest node (detail %q)",
			health.State, chainview.HealthDegraded, health.Detail)
	}
	if health.Tip.Height != 10 {
		t.Errorf("health tip height = %d, want 10", health.Tip.Height)
	}

	name, err := v.Network(ctx)
	if err != nil {
		t.Fatalf("Network: %v", err)
	}
	if name != "regtest" {
		t.Errorf("network = %q, want regtest", name)
	}

	tips, err := v.ChainTips(ctx)
	if err != nil {
		t.Fatalf("ChainTips: %v", err)
	}
	if len(tips) == 0 {
		t.Fatal("no chain tips reported")
	}
	if tips[0].Hash != tip.Hash {
		t.Errorf("first tip is %s, want the active tip %s", tips[0].Hash, tip.Hash)
	}
}

// The idempotency claim, checked against the node whose error codes and messages
// define it. This is the one behaviour the fixture tests cannot really settle,
// because the strings and codes are the node's to choose.
func TestBroadcastTwiceIsNotAnError(t *testing.T) {
	v := startNode(t)
	ctx := context.Background()

	// Coinbase outputs need 100 blocks before they can be spent.
	generate(t, v, 101)

	ensureWallet(t, v)
	var addr string
	if err := v.c.call(ctx, &addr, "getnewaddress"); err != nil {
		t.Fatal(err)
	}
	var rawHex string
	if err := v.c.call(ctx, &rawHex, "createrawtransaction", []any{}, map[string]any{addr: 1.0}); err != nil {
		t.Fatalf("createrawtransaction: %v", err)
	}
	var funded struct {
		Hex string `json:"hex"`
	}
	if err := v.c.call(ctx, &funded, "fundrawtransaction", rawHex); err != nil {
		t.Fatalf("fundrawtransaction: %v", err)
	}
	var signed struct {
		Hex      string `json:"hex"`
		Complete bool   `json:"complete"`
	}
	if err := v.c.call(ctx, &signed, "signrawtransactionwithwallet", funded.Hex); err != nil {
		t.Fatalf("signrawtransactionwithwallet: %v", err)
	}
	if !signed.Complete {
		t.Fatal("node did not fully sign the transaction")
	}

	tx := decodeTx(t, signed.Hex)

	if err := v.Broadcast(ctx, tx); err != nil {
		t.Fatalf("first broadcast: %v", err)
	}
	// Still in the memory pool.
	if err := v.Broadcast(ctx, tx); err != nil {
		t.Errorf("re-broadcasting a transaction already in the pool returned an error: %v", err)
	}

	// And once mined, which the node signals differently again.
	generate(t, v, 1)
	if err := v.Broadcast(ctx, tx); err != nil {
		t.Errorf("re-broadcasting a mined transaction returned an error: %v", err)
	}
}

func decodeTx(t *testing.T, hexStr string) *wire.MsgTx {
	t.Helper()
	raw, err := hexToBytes(hexStr)
	if err != nil {
		t.Fatalf("decoding transaction hex: %v", err)
	}
	var tx wire.MsgTx
	if err := tx.Deserialize(bytesReader(raw)); err != nil {
		t.Fatalf("deserialising transaction: %v", err)
	}
	return &tx
}

// A node too old for a call must report unsupported rather than a bare failure.
// Checked here against a real node that genuinely lacks the deployment, which no
// fixture can honestly simulate.
func TestDeploymentAbsentOnRegtest(t *testing.T) {
	v := startNode(t)

	_, err := v.Deployment(context.Background(), "reduced_data")
	if err == nil {
		t.Fatal("regtest reported a deployment it should not have")
	}
	if !errors.Is(err, chainview.ErrNotFound) && !errors.Is(err, chainview.ErrUnsupported) {
		t.Errorf("got %v, want ErrNotFound or ErrUnsupported", err)
	}
}
