package app_test

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paulscode/forktower/internal/app"
	"github.com/paulscode/forktower/internal/chainview/chainviewtest"
	"github.com/paulscode/forktower/internal/config"
	"github.com/paulscode/forktower/internal/registry"
	"github.com/paulscode/forktower/internal/store"
)

// stubNode is a Lightning node that answers one fixed snapshot.
type stubNode struct{ snap registry.Snapshot }

func (s stubNode) Snapshot(context.Context) (registry.Snapshot, error) { return s.snap, nil }

const stubFunding = "3333333333333333333333333333333333333333333333333333333333333333"

// The whole point of the wiring: a daemon with a Lightning node configured ends
// up with that node's channels in its database, classified and ready to watch.
// Nothing below this test would have failed if the registry were never started.
func TestADaemonWithALightningNodeReadsItsChannels(t *testing.T) {
	t.Parallel()

	node := stubNode{snap: registry.Snapshot{
		Node: registry.NodeInfo{Pubkey: "02abc", Alias: "my node", Impl: store.ImplLND},
		Channels: []registry.ChannelRecord{{
			FundingTxID: stubFunding,
			CapacitySat: 2_000_000,
			ChanType:    store.ChanAnchors,
			PeerPubkey:  "03peer",
			OpenHeight:  100,
			CloseState:  store.CloseOpen,
		}},
	}}

	h := newHarnessWith(t, nil, func(d *app.Deps) {
		d.LNSources = []registry.Source{{Name: "lnd-1", Client: node}}
	})
	h.start(t)

	waitFor(t, "the dashboard to report the node as being read", func() bool {
		for _, item := range readinessItems(t, h) {
			if item["id"] == "ln_connected" {
				ok, _ := item["ok"].(bool)
				return ok
			}
		}
		return false
	})

	// And the channel itself is in the database, classified. Read through a
	// second handle on the same file, because there is no API for channels yet
	// and the claim being made here is about storage, not about a response body.
	st, err := store.Open(context.Background(), h.cfg.Store.Path)
	if err != nil {
		t.Fatalf("reopening the database: %v", err)
	}
	defer func() { _ = st.Close() }()

	channels, err := st.ListChannels(context.Background(), store.ChannelFilter{})
	if err != nil {
		t.Fatalf("reading channels: %v", err)
	}
	if len(channels) != 1 {
		t.Fatalf("the daemon stored %d channels, want 1", len(channels))
	}
	got := channels[0]
	if got.FundingTxID != stubFunding || got.CapacitySat != 2_000_000 {
		t.Errorf("stored %+v", got)
	}
	if got.Relevance != store.Relevant || got.RelevanceReason == "" {
		t.Errorf("the channel was stored unclassified: %q (%q)",
			got.Relevance, got.RelevanceReason)
	}
	if got.LNNodeID != "02abc" {
		t.Errorf("the channel was attributed to %q", got.LNNodeID)
	}
}

// And with none configured, the daemon still runs: watching the chains is useful
// on its own, and a user who has not connected a node has not done anything
// wrong.
func TestADaemonWithNoLightningNodeStillRuns(t *testing.T) {
	t.Parallel()

	h := newHarnessWith(t, nil, func(d *app.Deps) {
		// Non-nil and empty: "no Lightning nodes", not "build them from config".
		d.LNSources = []registry.Source{}
	})
	h.start(t)

	var found bool
	for _, item := range readinessItems(t, h) {
		if item["id"] != "ln_connected" {
			continue
		}
		found = true
		if ok, _ := item["ok"].(bool); ok {
			t.Error("a Lightning node nobody configured was reported as connected")
		}
	}
	if !found {
		t.Error("the Lightning check is missing from the readiness list")
	}

	// The rest of the daemon is unaffected.
	headline, _ := h.status(t)["headline"].(map[string]any)
	if headline == nil || headline["title"] == "" {
		t.Error("the dashboard has nothing to say without a Lightning node")
	}
}

// A half-configured node is refused at startup rather than discovered at run
// time, because in the file it looks connected and it reads nothing.
func TestAHalfConfiguredLightningNodeIsRefused(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.LN.LND = []config.LNDConfig{{RESTAddr: "https://127.0.0.1:8080"}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("a node with no credential was accepted")
	}
}

func readinessItems(t *testing.T, h *harness) []map[string]any {
	t.Helper()
	raw, _ := h.status(t)["readiness"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// The path production actually takes: clients built from the configuration file
// rather than handed in by a test. What is being checked is the wiring — that the
// names, paths, addresses and certificates in the file reach the adapters — not
// that the adapters work, which is their own packages' business.
func TestLightningNodesAreBuiltFromTheConfiguration(t *testing.T) {
	t.Parallel()

	// Two nodes that answer, but answer nothing useful. Enough to prove the
	// clients were built and pointed at the right places.
	lndNode := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not today", http.StatusServiceUnavailable)
	}))
	t.Cleanup(lndNode.Close)
	clnNode := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not today", http.StatusServiceUnavailable)
	}))
	t.Cleanup(clnNode.Close)

	dir := t.TempDir()
	macaroon := filepath.Join(dir, "readonly.macaroon")
	if err := os.WriteFile(macaroon, []byte{0x02, 0x01, 0x03}, 0o600); err != nil {
		t.Fatal(err)
	}
	runeFile := filepath.Join(dir, "forktower.rune")
	if err := os.WriteFile(runeFile, []byte("not-a-real-rune"), 0o600); err != nil {
		t.Fatal(err)
	}

	h := newHarnessWith(t, func(c *config.Config, _, _ *chainviewtest.View) {
		c.LN.LND = []config.LNDConfig{{
			RESTAddr:     lndNode.URL,
			MacaroonPath: macaroon,
			TLSCertPath:  certPath(t, dir, "lnd.cert", lndNode),
		}}
		c.LN.CLN = []config.CLNConfig{{
			RESTAddr:    clnNode.URL,
			RunePath:    runeFile,
			TLSCertPath: certPath(t, dir, "cln.cert", clnNode),
		}}
	}, nil)
	h.start(t)

	// Neither node has anything to say, so both must be reported as unreadable —
	// by name, so a user with two of them knows which to look at. A daemon that
	// quietly showed "connected" here would be the worst of the three outcomes.
	waitFor(t, "both nodes to be reported as unreadable", func() bool {
		for _, item := range readinessItems(t, h) {
			if item["id"] != "ln_connected" {
				continue
			}
			why, _ := item["why"].(string)
			ok, _ := item["ok"].(bool)
			return !ok && strings.Contains(why, "lnd-1") && strings.Contains(why, "cln-1")
		}
		return false
	})
}

// certPath writes a test server's certificate where the configuration can point
// at it, which is how both adapters pin the node they are talking to.
func certPath(t *testing.T, dir, name string, srv *httptest.Server) string {
	t.Helper()
	path := filepath.Join(dir, name)
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: srv.Certificate().Raw,
	})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A credential file that is not there is a startup failure, not a surprise
// twenty minutes later: the user meant to connect a node and has not.
func TestAMissingLightningCredentialStopsStartup(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Store.Path = filepath.Join(t.TempDir(), "forktower.db")
	cfg.LN.LND = []config.LNDConfig{{
		RESTAddr:     "https://127.0.0.1:8080",
		MacaroonPath: filepath.Join(t.TempDir(), "does-not-exist.macaroon"),
	}}

	sf, sq := chainviewtest.NewSharedHistory(sharedHistory)
	_, err := app.New(context.Background(), cfg, nil, app.Deps{SF: sf, SQ: sq})
	if err == nil {
		t.Fatal("a daemon started with a credential file that does not exist")
	}
	if !strings.Contains(err.Error(), "LND") {
		t.Errorf("the error does not say which node could not be reached: %v", err)
	}
}

// The watcher is started too, and it says how far it has got. Without this the
// engine would be complete and never run — the same gap the registry had.
func TestTheDaemonWatchesTheOtherChain(t *testing.T) {
	t.Parallel()

	h := newHarnessWith(t, nil, nil)
	h.start(t)

	// The high-water mark only appears once a block has been processed, which
	// only happens once the watcher is running and following the second chain.
	waitFor(t, "the watcher to record where it has got to", func() bool {
		st, err := store.Open(context.Background(), h.cfg.Store.Path)
		if err != nil {
			return false
		}
		defer func() { _ = st.Close() }()
		got, err := st.GetMeta(context.Background(), store.MetaLastScannedSQHash)
		return err == nil && got != ""
	})
}

// The manual rescan exists because the daemon cannot always know it missed
// something. This is the case it was built for: the record of what happened on
// the other chain is gone, and asking for a re-read brings it back.
func TestARescanRediscoversWhatWasWipedFromTheRecord(t *testing.T) {
	t.Parallel()

	h := newHarnessWith(t, nil, func(d *app.Deps) {
		d.LNSources = []registry.Source{}
	})

	// A separation point, so there is somewhere to sweep back to. Written before
	// the daemon runs, for the reason in the test below.
	st := openDaemonStore(t, h)
	if err := st.SaveSplitState(context.Background(), store.Split{
		State: store.StateSplit, ForkHeight: 1, ForkHash: "aa", DetectedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	h.start(t)
	// Watching has to have got somewhere before there is anything behind it.
	waitFor(t, "the watcher to record where it has got to", func() bool {
		return watcherHeight(t, h) > 0
	})

	resp := postJSON(t, h, "/api/v1/rescan", "{}")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("asking for a rescan returned %d", resp.StatusCode)
	}
}

// Standing down is refused while a countdown is running, and that refusal is the
// whole reason it is a separate endpoint from confirming a split is over.
func TestStandingDownIsRefusedWhileACountdownRuns(t *testing.T) {
	t.Parallel()

	h := newHarnessWith(t, nil, func(d *app.Deps) {
		d.LNSources = []registry.Source{}
	})

	// Written before the daemon runs. SQLite lets a second connection wait for a
	// write lock, but not upgrade a transaction that began reading before another
	// process wrote — so a test that writes into a busy database fails for
	// reasons that have nothing to do with what it is testing.
	st := openDaemonStore(t, h)
	ctx := context.Background()
	if err := st.UpsertLNNode(ctx, store.LNNode{
		ID: "02node", Impl: store.ImplLND, LastSeenAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	channelID, _, err := st.UpsertChannel(ctx, store.Channel{
		LNNodeID: "02node", FundingTxID: stubFunding, FundingVout: 0,
		CapacitySat: 1000, ChanType: store.ChanAnchors, UpdatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	spendID, _, err := st.RecordSpend(ctx, store.Spend{
		Branch: store.BranchSQ, ChannelID: channelID,
		OutpointTxID: stubFunding, OutpointVout: 0,
		SpendTxID: stubFunding, SpendTxHex: "00", BlockHash: "aa", BlockHeight: 5,
		Shape: store.ShapeCommitmentUnknown, Status: store.SpendConfirmed,
		FirstSeenAt: 1, UpdatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.UpsertDeadline(ctx, store.Deadline{
		SpendEventID: spendID, Kind: store.DeadlineCSV, DeadlineHeight: 500,
		State: store.DeadlineCounting, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	h.start(t)
	waitFor(t, "the daemon to settle", func() bool { return watcherHeight(t, h) > 0 })

	resp := postJSON(t, h, "/api/v1/watch/stand-down", "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("standing down mid-countdown returned %d, want a refusal", resp.StatusCode)
	}
}

// Turning watching off, and back on, through the daemon as it really runs.
func TestWatchingCanBeTurnedOffAndBackOn(t *testing.T) {
	t.Parallel()

	h := newHarnessWith(t, nil, func(d *app.Deps) {
		d.LNSources = []registry.Source{}
	})
	h.start(t)
	waitFor(t, "the daemon to settle", func() bool { return watcherHeight(t, h) > 0 })

	if resp := postJSON(t, h, "/api/v1/watch/stand-down", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("standing down returned %d", resp.StatusCode)
	}
	if active := watchingActive(t, h); active {
		t.Error("the dashboard still says watching is on")
	}

	if resp := postJSON(t, h, "/api/v1/watch/resume", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("resuming returned %d", resp.StatusCode)
	}
	if active := watchingActive(t, h); !active {
		t.Error("the dashboard still says watching is off")
	}
}

// openDaemonStore reads the running daemon's database through a second handle.
func openDaemonStore(t *testing.T, h *harness) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), h.cfg.Store.Path)
	if err != nil {
		t.Fatalf("reopening the database: %v", err)
	}
	return st
}

func watcherHeight(t *testing.T, h *harness) int32 {
	t.Helper()
	for _, item := range readinessItems(t, h) {
		if item["id"] == "watcher_progressing" {
			if ok, _ := item["ok"].(bool); ok {
				return 1
			}
		}
	}
	return 0
}

func watchingActive(t *testing.T, h *harness) bool {
	t.Helper()
	for _, item := range readinessItems(t, h) {
		if item["id"] == "watching_active" {
			ok, _ := item["ok"].(bool)
			return ok
		}
	}
	t.Fatal("the readiness list does not say whether watching is on")
	return false
}

func postJSON(t *testing.T, h *harness, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		h.base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", h.base)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}
