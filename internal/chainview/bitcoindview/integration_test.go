//go:build integration

package bitcoindview

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
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

// startNode brings up a regtest node and returns a View onto it, without
// notifications configured so the polling path is what gets exercised.
func startNode(t *testing.T) *View {
	t.Helper()
	return startNodeWith(t, false)
}

// startNodeZMQ returns a View that uses the node's notification sockets.
func startNodeZMQ(t *testing.T) *View {
	t.Helper()
	return startNodeWith(t, true)
}

func startNodeWith(t *testing.T, useZMQ bool) *View {
	t.Helper()

	pool, err := dockertest.NewPool("")
	if err != nil {
		t.Fatalf("connecting to docker: %v", err)
	}
	v, _ := startNodeResource(t, pool, useZMQ)
	return v
}

// startNodeResource brings up a node and returns both the view and the container,
// so a test can restart it.
func startNodeResource(t *testing.T, pool *dockertest.Pool, useZMQ bool) (*View, *dockertest.Resource) {
	t.Helper()

	if err := pool.Client.Ping(); err != nil {
		t.Skipf("docker is not usable, skipping: %v", err)
	}

	// Fixed host ports rather than whatever Docker picks, so that restarting the
	// container keeps the same mapping. With ephemeral ports a restart silently
	// moves the node and the test would be measuring the wrong thing.
	hostPorts := freePorts(t, 3)

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
			"-zmqpubrawblock=tcp://0.0.0.0:28332",
			"-zmqpubrawtx=tcp://0.0.0.0:28333",
		},
		ExposedPorts: []string{"18443/tcp", "28332/tcp", "28333/tcp"},
		Labels:       map[string]string{"created-by": throwawayLabel},
		PortBindings: map[docker.Port][]docker.PortBinding{
			"18443/tcp": {{HostIP: "127.0.0.1", HostPort: strconv.Itoa(hostPorts[0])}},
			"28332/tcp": {{HostIP: "127.0.0.1", HostPort: strconv.Itoa(hostPorts[1])}},
			"28333/tcp": {{HostIP: "127.0.0.1", HostPort: strconv.Itoa(hostPorts[2])}},
		},
	}, func(hc *docker.HostConfig) {
		// Not auto-removed, because one test restarts the container. Purged in
		// cleanup instead, and labelled so a crashed run leaves something findable
		// rather than something anonymous.
		hc.AutoRemove = false
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

	opts := Options{
		RPCURL:  fmt.Sprintf("http://127.0.0.1:%d", hostPorts[0]),
		User:    rpcUser,
		Pass:    rpcPass,
		Timeout: 20 * time.Second,
	}
	if useZMQ {
		opts.ZMQRawBlock = fmt.Sprintf("tcp://127.0.0.1:%d", hostPorts[1])
		opts.ZMQRawTx = fmt.Sprintf("tcp://127.0.0.1:%d", hostPorts[2])
	}
	v, err := New(opts)
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
	return v, resource
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

// Notifications are the fast path and cannot be faked honestly: the wire protocol
// and the topics are the node's to define, so this is the only place the claim
// gets tested.
func TestSubscribeTipOverNotifications(t *testing.T) {
	v := startNodeZMQ(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := v.SubscribeTip(ctx)
	if err != nil {
		t.Fatalf("SubscribeTip: %v", err)
	}

	// The current tip arrives without waiting for a block.
	initial := awaitTip(t, ch, 20*time.Second, "the tip on subscribing")

	generate(t, v, 1)

	// Well inside the polling interval, which is the point of using notifications.
	got := awaitTip(t, ch, 2*time.Second, "a tip after mining")
	if got.Height != initial.Height+1 {
		t.Errorf("height went %d -> %d, want one more", initial.Height, got.Height)
	}
	if got.PrevHash != initial.Hash {
		t.Errorf("new tip does not build on the old one: prev %s, was %s", got.PrevHash, initial.Hash)
	}
}

// The fallback, against the same real node with notifications switched off. Slower
// by design, and it has to work: the user's own node may not publish at all.
func TestSubscribeTipByPolling(t *testing.T) {
	v := startNode(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := v.SubscribeTip(ctx)
	if err != nil {
		t.Fatalf("SubscribeTip: %v", err)
	}
	initial := awaitTip(t, ch, 20*time.Second, "the tip on subscribing")

	generate(t, v, 1)

	got := awaitTip(t, ch, DefaultPollInterval+2*time.Second, "a tip after mining")
	if got.Height != initial.Height+1 {
		t.Errorf("height went %d -> %d, want one more", initial.Height, got.Height)
	}
}

func TestSubscribeMempoolTxOverNotifications(t *testing.T) {
	v := startNodeZMQ(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Mine for maturity *before* subscribing. The raw-transaction topic publishes
	// the transactions of connected blocks as well as memory-pool arrivals, so
	// mining a hundred blocks after subscribing floods the buffer with coinbase
	// transactions and the one under test is correctly dropped — which is the
	// documented behaviour working, not a fault, but it makes for a useless test.
	generate(t, v, 101)

	ch, err := v.SubscribeMempoolTx(ctx)
	if err != nil {
		t.Fatalf("SubscribeMempoolTx: %v", err)
	}

	sent := sendSomeCoin(t, v)

	deadline := time.After(20 * time.Second)
	for {
		select {
		case tx, ok := <-ch:
			if !ok {
				t.Fatal("the mempool subscription closed")
			}
			if tx.TxHash() == sent {
				return
			}
			// Other transactions may appear; keep looking for ours.
		case <-deadline:
			t.Fatal("the broadcast transaction never arrived over the notification socket")
		}
	}
}

// A node restart is routine. The subscription must recover from it, and must not
// close — closing is how a consumer learns to stop, so it would look like a
// shutdown.
func TestSubscriptionSurvivesANodeRestart(t *testing.T) {
	pool, err := dockertest.NewPool("")
	if err != nil {
		t.Fatalf("connecting to docker: %v", err)
	}

	v, resource := startNodeResource(t, pool, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := v.SubscribeTip(ctx)
	if err != nil {
		t.Fatal(err)
	}
	before := awaitTip(t, ch, 20*time.Second, "the tip on subscribing")

	if err := pool.Client.RestartContainer(resource.Container.ID, 30); err != nil {
		t.Fatalf("restarting the node: %v", err)
	}

	// Wait for it to answer again, then mine. The subscription must still deliver.
	pool.MaxWait = readyTimeout
	if err := pool.Retry(func() error {
		rctx, rcancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer rcancel()
		h, hErr := v.Health(rctx)
		if hErr != nil {
			return hErr
		}
		if h.State == chainview.HealthDown {
			return errors.New("not answering yet")
		}
		return nil
	}); err != nil {
		t.Fatalf("node never came back: %v", err)
	}

	generate(t, v, 1)

	// A restarted node leaves a publish socket that reads as merely idle rather
	// than broken, so recovery may come from the safety-net timer rather than from
	// reconnecting. Either is fine — the point is that the subscription keeps
	// working without being restarted, and does not go quietly blind.
	got := awaitTip(t, ch, 30*time.Second, "a tip after the node restarted")
	if got.Height <= before.Height {
		t.Errorf("height %d did not advance past %d", got.Height, before.Height)
	}
}

func awaitTip(t *testing.T, ch <-chan chainview.BlockMeta, d time.Duration, what string) chainview.BlockMeta {
	t.Helper()
	select {
	case got, ok := <-ch:
		if !ok {
			t.Fatalf("subscription closed while waiting for %s", what)
		}
		return got
	case <-time.After(d):
		t.Fatalf("timed out after %v waiting for %s", d, what)
		return chainview.BlockMeta{}
	}
}

// sendSomeCoin makes and broadcasts a wallet payment, returning its hash.
func sendSomeCoin(t *testing.T, v *View) chainhash.Hash {
	t.Helper()
	ctx := context.Background()
	ensureWallet(t, v)

	var addr string
	if err := v.c.call(ctx, &addr, "getnewaddress"); err != nil {
		t.Fatal(err)
	}
	var txid string
	if err := v.c.call(ctx, &txid, "sendtoaddress", addr, 1.0); err != nil {
		t.Fatalf("sendtoaddress: %v", err)
	}
	h, err := chainhash.NewHashFromStr(txid)
	if err != nil {
		t.Fatal(err)
	}
	return *h
}

// freePorts reserves n host ports by opening and closing listeners, so the
// container can be given fixed bindings that survive a restart.
func freePorts(t *testing.T, n int) []int {
	t.Helper()
	out := make([]int, 0, n)
	for range n {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("reserving a port: %v", err)
		}
		out = append(out, l.Addr().(*net.TCPAddr).Port)
		if err := l.Close(); err != nil {
			t.Fatalf("releasing a reserved port: %v", err)
		}
	}
	return out
}
