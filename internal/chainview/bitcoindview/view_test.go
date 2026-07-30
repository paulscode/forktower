package bitcoindview

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"

	"github.com/paulscode/forktower/internal/chainview"
)

// fakeNode is a stand-in Bitcoin node. Responses are supplied per method, so each
// test states only what it cares about.
type fakeNode struct {
	t *testing.T

	mu       sync.Mutex
	handlers map[string]func(params []json.RawMessage) (any, *rpcError)
	calls    []string
	// status overrides the HTTP status for every response, for the paths that only
	// arise when a node misbehaves.
	status int
	// unauthorizedUntil rejects the first n requests with 401, to exercise the
	// credential reload.
	unauthorizedUntil int
	seen              int
}

func newFakeNode(t *testing.T) *fakeNode {
	t.Helper()
	return &fakeNode{t: t, handlers: map[string]func([]json.RawMessage) (any, *rpcError){}}
}

func (f *fakeNode) on(method string, fn func(params []json.RawMessage) (any, *rpcError)) *fakeNode {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[method] = fn
	return f
}

// reply registers a fixed successful result.
func (f *fakeNode) reply(method string, result any) *fakeNode {
	return f.on(method, func([]json.RawMessage) (any, *rpcError) { return result, nil })
}

// fail registers a node-level error.
func (f *fakeNode) fail(method string, code int, msg string) *fakeNode {
	return f.on(method, func([]json.RawMessage) (any, *rpcError) {
		return nil, &rpcError{Code: code, Message: msg}
	})
}

func (f *fakeNode) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeNode) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.seen++
	rejectAuth := f.seen <= f.unauthorizedUntil
	f.mu.Unlock()

	if rejectAuth {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Errorf("reading request: %v", err)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		f.t.Errorf("request is not JSON-RPC: %v", err)
		return
	}

	f.mu.Lock()
	f.calls = append(f.calls, req.Method)
	handler, ok := f.handlers[req.Method]
	status := f.status
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if status != 0 {
		w.WriteHeader(status)
	}

	if !ok {
		writeJSON(w, rpcResponse{Error: &rpcError{
			Code: codeMethodNotFound, Message: "Method not found",
		}})
		return
	}

	result, rpcErr := handler(req.Params)
	if rpcErr != nil {
		writeJSON(w, rpcResponse{Error: rpcErr})
		return
	}
	raw, err := json.Marshal(result)
	if err != nil {
		f.t.Fatalf("encoding fake result: %v", err)
	}
	writeJSON(w, rpcResponse{Result: raw})
}

func writeJSON(w http.ResponseWriter, resp rpcResponse) {
	_ = json.NewEncoder(w).Encode(resp)
}

// newTestView wires a View to a fake node.
func newTestView(t *testing.T, node *fakeNode) *View {
	t.Helper()
	srv := httptest.NewServer(node)
	t.Cleanup(srv.Close)

	v, err := New(Options{
		RPCURL: srv.URL,
		User:   "u",
		Pass:   "p",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return v
}

// A real header, so the decode path is exercised against the shape a node
// actually returns rather than one invented to match the parser.
func headerFixture(hash, prev string, height int32, unixTime int64) map[string]any {
	return map[string]any{
		"hash":              hash,
		"height":            height,
		"time":              unixTime,
		"previousblockhash": prev,
		"version":           2,
		"merkleroot":        strings.Repeat("11", 32),
		"nonce":             12345,
		"bits":              "170355f0",
		"difficulty":        1.0,
		"confirmations":     1,
	}
}

const (
	tipHash  = "0000000000000000000161687732d89fddeb491149e72f52e518c22fe001ba8c"
	prevHash = "00000000000000000001eda153ed2b1fb0462ef5827f620d8074ae17f617964b"
)

func TestBestBlock(t *testing.T) {
	t.Parallel()

	node := newFakeNode(t).
		reply("getbestblockhash", tipHash).
		reply("getblockheader", headerFixture(tipHash, prevHash, 959911, 1790000000))

	got, err := newTestView(t, node).BestBlock(context.Background())
	if err != nil {
		t.Fatalf("BestBlock: %v", err)
	}
	if got.Hash.String() != tipHash {
		t.Errorf("hash = %s, want %s", got.Hash, tipHash)
	}
	if got.Height != 959911 {
		t.Errorf("height = %d, want 959911", got.Height)
	}
	if got.PrevHash.String() != prevHash {
		t.Errorf("prev hash = %s, want %s", got.PrevHash, prevHash)
	}
	if !got.Time.Equal(time.Unix(1790000000, 0).UTC()) {
		t.Errorf("time = %v, want the header's timestamp", got.Time)
	}
}

func TestBlockHeaderGenesisHasNoPredecessor(t *testing.T) {
	t.Parallel()

	// The one block with no previous hash. A parser that assumed the field is
	// always present would fail exactly where the network check reads.
	genesis := "000000000019d6689c085ae165831e934ff763ae46a2a6c172b3f1b60a8ce26f"
	node := newFakeNode(t).reply("getblockheader", headerFixture(genesis, "", 0, 1231006505))

	got, err := newTestView(t, node).BlockHeaderByHash(context.Background(), chainhash.Hash{})
	if err != nil {
		t.Fatalf("BlockHeaderByHash: %v", err)
	}
	if got.PrevHash != (chainhash.Hash{}) {
		t.Errorf("genesis previous hash = %s, want the zero hash", got.PrevHash)
	}
}

// Not-found must be a sentinel rather than a raw error: callers branch on it —
// a pruned node or a height past the tip is an expected answer, not a fault.
func TestNotFoundMapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		code int
		msg  string
	}{
		{"unknown block hash", codeInvalidAddressOrKey, "Block not found"},
		{"height past the tip", codeInvalidParameter, "Block height out of range"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			node := newFakeNode(t).
				fail("getblockheader", tc.code, tc.msg).
				fail("getblockhash", tc.code, tc.msg).
				fail("getblock", tc.code, tc.msg)
			v := newTestView(t, node)
			ctx := context.Background()

			if _, err := v.BlockHeaderByHash(ctx, chainhash.Hash{}); !errors.Is(err, chainview.ErrNotFound) {
				t.Errorf("BlockHeaderByHash returned %v, want ErrNotFound", err)
			}
			if _, err := v.BlockHashByHeight(ctx, 999999); !errors.Is(err, chainview.ErrNotFound) {
				t.Errorf("BlockHashByHeight returned %v, want ErrNotFound", err)
			}
			if _, err := v.Block(ctx, chainhash.Hash{}); !errors.Is(err, chainview.ErrNotFound) {
				t.Errorf("Block returned %v, want ErrNotFound", err)
			}
		})
	}
}

// An old node that lacks a call must report unsupported, not a generic failure:
// the caller degrades rather than retrying something that will never work.
func TestMethodNotFoundBecomesUnsupported(t *testing.T) {
	t.Parallel()

	node := newFakeNode(t) // no handlers, so everything is method-not-found
	_, err := newTestView(t, node).Deployment(context.Background(), "reduced_data")
	if !errors.Is(err, chainview.ErrUnsupported) {
		t.Errorf("Deployment on a node without the call returned %v, want ErrUnsupported", err)
	}
}

func TestBlockRoundTrip(t *testing.T) {
	t.Parallel()

	// A real serialised block, built here so the test does not depend on a
	// hand-written hex blob nobody can check.
	blk := wire.MsgBlock{
		Header: wire.BlockHeader{Version: 2, Timestamp: time.Unix(1790000000, 0)},
	}
	coinbase := wire.NewMsgTx(1)
	coinbase.AddTxIn(&wire.TxIn{SignatureScript: []byte{0x51}})
	coinbase.AddTxOut(&wire.TxOut{Value: 5000000000, PkScript: []byte{0x51}})
	if err := blk.AddTransaction(coinbase); err != nil {
		t.Fatal(err)
	}

	var encoded writerBuf
	if err := blk.Serialize(&encoded); err != nil {
		t.Fatal(err)
	}

	node := newFakeNode(t).reply("getblock", hex.EncodeToString(encoded.bytes))
	got, err := newTestView(t, node).Block(context.Background(), chainhash.Hash{})
	if err != nil {
		t.Fatalf("Block: %v", err)
	}
	if len(got.Transactions) != 1 {
		t.Fatalf("decoded %d transactions, want 1", len(got.Transactions))
	}
	if got.Transactions[0].TxOut[0].Value != 5000000000 {
		t.Errorf("decoded output value = %d", got.Transactions[0].TxOut[0].Value)
	}

	// Asked for verbosity 0. The JSON form is larger, slower, and drops detail
	// needed to see how an output is spent, so requesting it would be a mistake
	// worth catching.
	if calls := node.called(); len(calls) != 1 || calls[0] != "getblock" {
		t.Errorf("unexpected calls: %v", calls)
	}
}

// writerBuf collects bytes, avoiding a dependency just to serialise in a test.
type writerBuf struct{ bytes []byte }

func (w *writerBuf) Write(p []byte) (int, error) {
	w.bytes = append(w.bytes, p...)
	return len(p), nil
}

func TestMatchBlockAlwaysSaysMaybe(t *testing.T) {
	t.Parallel()

	// False is a promise under the contract; a full node has no cheap way to make
	// it, so it must never be returned here.
	got, err := newTestView(t, newFakeNode(t)).
		MatchBlock(context.Background(), chainhash.Hash{}, chainview.WatchSet{})
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("MatchBlock said no; this backend cannot know that without looking")
	}
}

// Rebroadcasting is routine — on retries and after restarts — so an
// already-known transaction is the wanted outcome, not a failure to report.
func TestBroadcastTreatsAlreadyKnownAsSuccess(t *testing.T) {
	t.Parallel()

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{})
	tx.AddTxOut(&wire.TxOut{Value: 1000, PkScript: []byte{0x51}})

	cases := []struct {
		name    string
		code    int
		msg     string
		wantErr bool
	}{
		{name: "already in mempool", code: -25, msg: "txn-already-in-mempool"},
		{name: "already known", code: -25, msg: "txn-already-known"},
		{name: "already in chain by code", code: codeVerifyAlreadyInChain, msg: "irrelevant prose"},
		{name: "already in chain by message", code: -25, msg: "Transaction already in block chain"},
		{name: "mixed case message", code: -25, msg: "TXN-ALREADY-IN-MEMPOOL"},
		{name: "genuine rejection", code: -26, msg: "min relay fee not met", wantErr: true},
		{name: "missing inputs", code: -25, msg: "bad-txns-inputs-missingorspent", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			node := newFakeNode(t).fail("sendrawtransaction", tc.code, tc.msg)
			err := newTestView(t, node).Broadcast(context.Background(), tx)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("a genuine rejection (%s) was reported as success", tc.msg)
				}
				if !strings.Contains(err.Error(), tc.msg) {
					t.Errorf("error drops the node's own reason, which is the only clue "+
						"about why it was refused: %v", err)
				}
				return
			}
			if err != nil {
				t.Errorf("already-known transaction reported an error: %v", err)
			}
		})
	}
}

func TestBroadcastSucceeds(t *testing.T) {
	t.Parallel()

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{})
	tx.AddTxOut(&wire.TxOut{Value: 1000, PkScript: []byte{0x51}})

	var gotHex string
	node := newFakeNode(t).on("sendrawtransaction", func(p []json.RawMessage) (any, *rpcError) {
		_ = json.Unmarshal(p[0], &gotHex)
		return "txid", nil
	})
	if err := newTestView(t, node).Broadcast(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	if _, err := hex.DecodeString(gotHex); err != nil || gotHex == "" {
		t.Errorf("transaction was not sent as hex: %q", gotHex)
	}
}

func TestBroadcastRejectsNil(t *testing.T) {
	t.Parallel()
	if err := newTestView(t, newFakeNode(t)).Broadcast(context.Background(), nil); err == nil {
		t.Error("Broadcast accepted a nil transaction")
	}
}

// Health reports rather than fails: an unreachable node is a state to display and
// work around, not an error that stops the daemon.
func TestHealth(t *testing.T) {
	t.Parallel()

	baseInfo := func(mutate func(map[string]any)) map[string]any {
		m := map[string]any{
			"chain":                "main",
			"blocks":               959911,
			"headers":              959911,
			"bestblockhash":        tipHash,
			"verificationprogress": 0.9999,
			"initialblockdownload": false,
			"pruned":               false,
		}
		if mutate != nil {
			mutate(m)
		}
		return m
	}

	cases := []struct {
		name       string
		info       map[string]any
		peers      int
		wantState  chainview.HealthState
		wantDetail string
	}{
		{
			name:      "caught up with peers",
			info:      baseInfo(nil),
			peers:     8,
			wantState: chainview.HealthOK,
		},
		{
			name:       "still syncing",
			info:       baseInfo(func(m map[string]any) { m["initialblockdownload"] = true }),
			peers:      8,
			wantState:  chainview.HealthSyncing,
			wantDetail: "catching up",
		},
		{
			name:       "behind the headers it has",
			info:       baseInfo(func(m map[string]any) { m["blocks"] = 959000 }),
			peers:      8,
			wantState:  chainview.HealthSyncing,
			wantDetail: "behind the headers",
		},
		{
			// Looks healthy and is blind. Worth naming loudly: no peers means no new
			// blocks, so nothing will ever be detected.
			name:       "no peers",
			info:       baseInfo(nil),
			peers:      0,
			wantState:  chainview.HealthDegraded,
			wantDetail: "no peers",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			node := newFakeNode(t).
				reply("getblockchaininfo", tc.info).
				reply("getnetworkinfo", map[string]any{"connections": tc.peers}).
				reply("getblockheader", headerFixture(tipHash, prevHash, 959911, 1790000000))

			got, err := newTestView(t, node).Health(context.Background())
			if err != nil {
				t.Fatalf("Health returned an error rather than a state: %v", err)
			}
			if got.State != tc.wantState {
				t.Errorf("state = %q, want %q (detail %q)", got.State, tc.wantState, got.Detail)
			}
			if tc.wantDetail != "" && !strings.Contains(got.Detail, tc.wantDetail) {
				t.Errorf("detail = %q, want it to mention %q", got.Detail, tc.wantDetail)
			}
			if got.PeerCount != tc.peers {
				t.Errorf("peer count = %d, want %d", got.PeerCount, tc.peers)
			}
		})
	}
}

func TestHealthOnAnUnreachableNodeReportsDown(t *testing.T) {
	t.Parallel()

	// Point at a closed port rather than faking a failure, so the real transport
	// error path is exercised.
	v, err := New(Options{RPCURL: "http://127.0.0.1:1", User: "u", Pass: "p",
		Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	got, err := v.Health(context.Background())
	if err != nil {
		t.Fatalf("Health on an unreachable node returned an error rather than DOWN: %v", err)
	}
	if got.State != chainview.HealthDown {
		t.Errorf("state = %q, want %q", got.State, chainview.HealthDown)
	}
	if got.Detail == "" {
		t.Error("no detail, so the user is told nothing about why it is down")
	}
}

func TestChainTips(t *testing.T) {
	t.Parallel()

	node := newFakeNode(t).reply("getchaintips", []map[string]any{
		{"height": 959911, "hash": tipHash, "branchlen": 0, "status": "active"},
		{"height": 961700, "hash": prevHash, "branchlen": 68, "status": "invalid"},
	})

	got, err := newTestView(t, node).ChainTips(context.Background())
	if err != nil {
		t.Fatalf("ChainTips: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d tips, want 2", len(got))
	}
	if got[0].Rejected() {
		t.Error("the active tip was reported as rejected")
	}
	// The rejected branch is the point of this call: a node that fetched a block
	// and refused it is local evidence of a rule disagreement, needing no peer to
	// agree.
	if !got[1].Rejected() {
		t.Error("an invalid tip was not reported as rejected")
	}
	if got[1].BranchLen != 68 {
		t.Errorf("branch length = %d, want 68", got[1].BranchLen)
	}
}

func TestDeployment(t *testing.T) {
	t.Parallel()

	// Shaped after what a real node returns, so the parser is tested against
	// reality rather than against itself.
	node := newFakeNode(t).reply("getdeploymentinfo", map[string]any{
		"height": 959911,
		"deployments": map[string]any{
			"taproot": map[string]any{
				"type": "bip9", "active": true, "height": 709632,
			},
			"reduced_data": map[string]any{
				"type":   "bip9",
				"active": false,
				"bip9": map[string]any{
					"bit":                   4,
					"start_time":            1764547200,
					"timeout":               9223372036854775807,
					"min_activation_height": 0,
					"max_activation_height": 965664,
					"status":                "started",
					"since":                 927360,
					"statistics": map[string]any{
						"period": 2016, "elapsed": 296, "count": 7, "threshold": 1109,
					},
				},
			},
		},
	})

	v := newTestView(t, node)
	got, err := v.Deployment(context.Background(), "reduced_data")
	if err != nil {
		t.Fatalf("Deployment: %v", err)
	}
	if got.Bit != 4 {
		t.Errorf("bit = %d, want 4", got.Bit)
	}
	if got.MaxActivationHeight != 965664 {
		t.Errorf("max activation height = %d, want 965664", got.MaxActivationHeight)
	}
	if got.Status != "started" || got.Active {
		t.Errorf("status = %q active = %v", got.Status, got.Active)
	}
	// The signalling share is the one number that tells a user how likely a split
	// actually is, so it must survive the parse.
	if got.Period != 2016 || got.Elapsed != 296 || got.Count != 7 || got.Threshold != 1109 {
		t.Errorf("signalling statistics lost: %+v", got)
	}

	if _, err := v.Deployment(context.Background(), "no-such-fork"); !errors.Is(err, chainview.ErrNotFound) {
		t.Errorf("unknown deployment returned %v, want ErrNotFound", err)
	}
}

func TestNetworkAndPruneHeight(t *testing.T) {
	t.Parallel()

	node := newFakeNode(t).reply("getblockchaininfo", map[string]any{
		"chain": "regtest", "blocks": 200, "headers": 200,
		"bestblockhash": tipHash, "verificationprogress": 1.0,
		"pruned": true, "pruneheight": 150,
	})
	v := newTestView(t, node)

	name, err := v.Network(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if name != "regtest" {
		t.Errorf("network = %q, want regtest", name)
	}

	// A watcher whose history has been pruned past the point the chains separated
	// has a blind spot it must report rather than discover later.
	height, pruned, err := v.PruneHeight(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !pruned || height != 150 {
		t.Errorf("PruneHeight = (%d, %v), want (150, true)", height, pruned)
	}
}

// A node rewrites its cookie when it restarts, so a long-running watcher has to
// re-read it. Without this, losing sight of the chain would look like nothing at
// all until the next alert failed to arrive.
func TestCookieIsRereadAfterRejection(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cookie := filepath.Join(dir, ".cookie")
	if err := os.WriteFile(cookie, []byte("__cookie__:stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	node := newFakeNode(t)
	node.unauthorizedUntil = 1 // reject once, as a node with a new cookie would
	node.reply("getbestblockhash", tipHash).
		reply("getblockheader", headerFixture(tipHash, prevHash, 1, 1))

	srv := httptest.NewServer(node)
	t.Cleanup(srv.Close)

	v, err := New(Options{RPCURL: srv.URL, CookiePath: cookie})
	if err != nil {
		t.Fatal(err)
	}

	// The node restarts and writes a new cookie between the rejection and the retry.
	if err := os.WriteFile(cookie, []byte("__cookie__:fresh"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := v.BestBlock(context.Background()); err != nil {
		t.Fatalf("a rotated cookie was not recovered from: %v", err)
	}
}

func TestPersistentlyRefusedCredentialsGiveAClearError(t *testing.T) {
	t.Parallel()

	node := newFakeNode(t)
	node.unauthorizedUntil = 100 // never accept
	srv := httptest.NewServer(node)
	t.Cleanup(srv.Close)

	v, err := New(Options{RPCURL: srv.URL, User: "u", Pass: "wrong"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = v.BestBlock(context.Background())
	if err == nil {
		t.Fatal("bad credentials were accepted")
	}
	// The message has to say what to fix; "unauthorized" alone sends someone
	// looking in the wrong place.
	if !strings.Contains(err.Error(), "cookie path or user and password") {
		t.Errorf("error does not say what to check: %v", err)
	}
}

func TestContextCancellationIsHonoured(t *testing.T) {
	t.Parallel()

	// The reason this package does not use the ecosystem's RPC client: a cancelled
	// call must actually stop, not return early while its request runs on.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)

	v, err := New(Options{RPCURL: srv.URL, User: "u", Pass: "p"})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, callErr := v.BestBlock(ctx)
		done <- callErr
	}()
	cancel()

	select {
	case callErr := <-done:
		if !errors.Is(callErr, context.Canceled) {
			t.Errorf("cancelling returned %v, want a context error", callErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the context did not stop the call")
	}
}

func TestOptionsValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		opts    Options
		wantSub string
	}{
		{"no url", Options{User: "u", Pass: "p"}, "rpc url is required"},
		{"bad scheme", Options{RPCURL: "ftp://h:1", User: "u", Pass: "p"}, "http or https"},
		{"no host", Options{RPCURL: "http://", User: "u", Pass: "p"}, "no host"},
		{"no auth", Options{RPCURL: "http://h:1"}, "no authentication"},
		{
			"both auth methods",
			Options{RPCURL: "http://h:1", CookiePath: "/c", User: "u", Pass: "p"},
			"not both",
		},
		{"user without pass", Options{RPCURL: "http://h:1", User: "u"}, "both a user and a password"},
		{
			"negative timeout",
			Options{RPCURL: "http://h:1", User: "u", Pass: "p", Timeout: -time.Second},
			"negative",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.opts.Validate()
			if err == nil {
				t.Fatalf("accepted %+v", tc.opts)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error does not mention %q: %v", tc.wantSub, err)
			}
			if _, newErr := New(tc.opts); newErr == nil {
				t.Error("New accepted options that do not validate")
			}
		})
	}

	// Every problem at once, so someone fixing options learns about all of them.
	err := Options{RPCURL: "ftp://h", Timeout: -1}.Validate()
	if err == nil || !strings.Contains(err.Error(), ";") {
		t.Errorf("multiple problems were not reported together: %v", err)
	}
}

func TestValidOptionsAreAccepted(t *testing.T) {
	t.Parallel()

	for _, o := range []Options{
		{RPCURL: "http://127.0.0.1:8332", User: "u", Pass: "p"},
		{RPCURL: "https://node:8332", CookiePath: "/data/.cookie"},
	} {
		if err := o.Validate(); err != nil {
			t.Errorf("rejected usable options %+v: %v", o, err)
		}
	}
}

func TestMissingCookieFileIsReported(t *testing.T) {
	t.Parallel()

	v, err := New(Options{RPCURL: "http://127.0.0.1:1",
		CookiePath: filepath.Join(t.TempDir(), "absent")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = v.BestBlock(context.Background())
	if err == nil {
		t.Fatal("a missing cookie file was not reported")
	}
	if !strings.Contains(err.Error(), "cookie") {
		t.Errorf("error does not mention the cookie: %v", err)
	}
}

func TestEmptyCookieFileIsReported(t *testing.T) {
	t.Parallel()

	cookie := filepath.Join(t.TempDir(), ".cookie")
	if err := os.WriteFile(cookie, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := New(Options{RPCURL: "http://127.0.0.1:1", CookiePath: cookie})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.BestBlock(context.Background()); !errors.Is(err, ErrNoAuth) {
		t.Errorf("an empty cookie returned %v, want ErrNoAuth", err)
	}
}

// A node reports its own errors inside a 500, so the body must be read whatever
// the status — otherwise the reason a transaction was refused is thrown away.
func TestNodeErrorInsideAnHTTPErrorIsStillDecoded(t *testing.T) {
	t.Parallel()

	node := newFakeNode(t)
	node.status = http.StatusInternalServerError
	node.fail("getblockhash", codeInvalidParameter, "Block height out of range")

	_, err := newTestView(t, node).BlockHashByHeight(context.Background(), 1<<30)
	if !errors.Is(err, chainview.ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound decoded from the body of a 500", err)
	}
}

func TestUnreadableHashFromNodeIsReported(t *testing.T) {
	t.Parallel()

	node := newFakeNode(t).reply("getbestblockhash", "not-a-hash")
	_, err := newTestView(t, node).BestBlock(context.Background())
	if err == nil {
		t.Fatal("an unreadable hash was accepted")
	}
	if !strings.Contains(err.Error(), "not-a-hash") {
		t.Errorf("error does not quote the offending value: %v", err)
	}
}

func TestUserAgentIsSent(t *testing.T) {
	t.Parallel()

	// A courtesy to whoever has to work out what is talking to their node.
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("User-Agent")
		writeJSON(w, rpcResponse{Result: json.RawMessage(fmt.Sprintf("%q", tipHash))})
	}))
	t.Cleanup(srv.Close)

	v, err := New(Options{RPCURL: srv.URL, User: "u", Pass: "p"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = v.BlockHashByHeight(context.Background(), 1)
	if !strings.Contains(seen, DefaultUserAgent) {
		t.Errorf("User-Agent = %q, want it to mention %q", seen, DefaultUserAgent)
	}
}
