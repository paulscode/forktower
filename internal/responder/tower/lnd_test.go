package tower

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paulscode/forktower/internal/nodeaddr"
)

// serve stands up a fake LND REST API and returns a reader pointed at it.
func serve(t *testing.T, routes map[string]func(http.ResponseWriter, *http.Request)) *LND {
	t.Helper()

	mux := http.NewServeMux()
	for path, handler := range routes {
		mux.HandleFunc(path, handler)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	macPath := filepath.Join(t.TempDir(), "readonly.macaroon")
	if err := os.WriteFile(macPath, []byte{0x02, 0x01, 0x03}, 0o600); err != nil {
		t.Fatal(err)
	}

	l, err := NewLND(LNDOptions{BaseURL: srv.URL, MacaroonPath: macPath})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func json200(body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

// LND returns `bytes` fields base64-encoded over REST. Everything else in this
// project speaks hex, and a pubkey in the wrong encoding silently fails to match
// the tower we are looking for.
func TestAPubkeyIsConvertedFromWhatRESTActuallySends(t *testing.T) {
	t.Parallel()
	raw, err := hex.DecodeString(ourTower)
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(raw)

	l := serve(t, map[string]func(http.ResponseWriter, *http.Request){
		"/v2/watchtower/server": json200(`{
			"pubkey": "` + encoded + `",
			"listeners": ["[::]:9911"],
			"uris": ["` + ourTower + `@abcdef.onion:9911"]
		}`),
	})

	got, err := l.Identity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Pubkey != ourTower {
		t.Errorf("pubkey = %q, want the hex form %q", got.Pubkey, ourTower)
	}
	if len(got.URIs) != 1 {
		t.Errorf("the address a user would paste was lost: %+v", got.URIs)
	}
}

// The credential goes in the header LND expects, and nothing here is a write.
func TestEveryCallIsAReadAndCarriesTheMacaroon(t *testing.T) {
	t.Parallel()

	var methods, headers []string
	record := func(body string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			methods = append(methods, r.Method)
			headers = append(headers, r.Header.Get("Grpc-Metadata-macaroon"))
			json200(body)(w, r)
		}
	}

	l := serve(t, map[string]func(http.ResponseWriter, *http.Request){
		"/v2/watchtower/server":       record(`{"pubkey":"aa","listeners":[],"uris":["x"]}`),
		"/v1/getinfo":                 record(`{"version":"0.18.5-beta","block_height":900000,"synced_to_chain":true}`),
		"/v2/watchtower/client":       record(`{"towers":[]}`),
		"/v2/watchtower/client/stats": record(`{"num_backups":5}`),
	})

	ctx := context.Background()
	if _, err := l.Identity(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Chain(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Towers(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Stats(ctx); err != nil {
		t.Fatal(err)
	}

	if len(methods) != 4 {
		t.Fatalf("made %d calls, want 4", len(methods))
	}
	for i, m := range methods {
		if m != http.MethodGet {
			t.Errorf("call %d used %s — nothing in this package may change anything", i, m)
		}
		if headers[i] == "" {
			t.Errorf("call %d carried no credential", i)
		}
	}
}

// Both subservers answer an ordinary read with "not active" rather than failing
// to answer, which is what makes a plain GET a clean probe.
func TestNotActiveIsRecognisedFromTheBodyOfTheAnswer(t *testing.T) {
	t.Parallel()

	l := serve(t, map[string]func(http.ResponseWriter, *http.Request){
		"/v2/watchtower/server": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"watchtower not active","code":2}`))
		},
		"/v2/watchtower/client": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"watchtower client not active","code":2}`))
		},
	})

	if _, err := l.Identity(context.Background()); err == nil {
		t.Error("a switched-off watchtower answered cleanly")
	} else if !isTowerNotActive(err) {
		t.Errorf("a switched-off watchtower was not recognised: %v", err)
	}

	if _, err := l.Towers(context.Background()); err == nil {
		t.Error("a switched-off client answered cleanly")
	} else if !isClientNotActiveErr(err) {
		t.Errorf("a switched-off client was not recognised: %v", err)
	}
}

func isTowerNotActive(err error) bool {
	return strings.Contains(err.Error(), "watchtower is switched off")
}
func isClientNotActiveErr(err error) bool {
	return strings.Contains(err.Error(), "watchtower client is switched off")
}

// The sessions are the evidence the coverage check runs on, so the shape LND
// actually sends has to survive being read.
func TestSessionsAreReadWithTheirPolicyTypesAndFees(t *testing.T) {
	t.Parallel()
	raw, _ := hex.DecodeString(ourTower)

	l := serve(t, map[string]func(http.ResponseWriter, *http.Request){
		"/v2/watchtower/client": json200(`{
		  "towers": [{
		    "pubkey": "` + base64.StdEncoding.EncodeToString(raw) + `",
		    "addresses": ["abcdef.onion:9911"],
		    "session_info": [
		      {"policy_type": "ANCHOR", "num_sessions": 1, "sessions": [
		        {"num_backups": 120, "num_pending_backups": 2, "max_backups": 1024,
		         "sweep_sat_per_vbyte": 10}]},
		      {"policy_type": "LEGACY", "num_sessions": 1, "sessions": [
		        {"num_backups": 0, "max_backups": 1024, "sweep_sat_per_vbyte": 10}]}
		    ]
		  }]
		}`),
	})

	towers, err := l.Towers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(towers) != 1 || towers[0].Pubkey != ourTower {
		t.Fatalf("towers = %+v", towers)
	}
	if len(towers[0].Sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(towers[0].Sessions))
	}

	byPolicy := map[PolicyType]Session{}
	for _, s := range towers[0].Sessions {
		byPolicy[s.Policy] = s
	}
	anchor, ok := byPolicy[PolicyAnchor]
	if !ok {
		t.Fatalf("no anchor session was read: %+v", towers[0].Sessions)
	}
	if anchor.NumBackups != 120 || anchor.NumPending != 2 || anchor.SweepSatPerVByte != 10 {
		t.Errorf("the anchor session was misread: %+v", anchor)
	}
	if _, ok := byPolicy[PolicyLegacy]; !ok {
		t.Error("the legacy session was not read")
	}
}

func TestChainAndVersionAreReadFromTheNode(t *testing.T) {
	t.Parallel()
	l := serve(t, map[string]func(http.ResponseWriter, *http.Request){
		"/v1/getinfo": json200(
			`{"version":"0.18.5-beta commit=v0.18.5-beta","block_height":900123,"synced_to_chain":true}`),
	})

	chain, err := l.Chain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if chain.BlockHeight != 900123 || !chain.SyncedToChain {
		t.Errorf("chain = %+v", chain)
	}

	v, err := l.Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !v.Known || v.Minor != 18 || v.Patch != 5 {
		t.Errorf("version = %+v", v)
	}
}

// An error that is not "switched off" must come back as itself, with enough of
// the answer to act on.
func TestAnOrdinaryFailureIsReportedWithWhatTheNodeSaid(t *testing.T) {
	t.Parallel()
	l := serve(t, map[string]func(http.ResponseWriter, *http.Request){
		"/v1/getinfo": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"permission denied"}`))
		},
	})

	_, err := l.Chain(context.Background())
	if err == nil {
		t.Fatal("a rejected read succeeded")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("the node's own words were dropped: %v", err)
	}
}

func TestAReaderNeedsAnAddressAndACredentialItCanRead(t *testing.T) {
	t.Parallel()

	if _, err := NewLND(LNDOptions{MacaroonPath: "/dev/null"}); err == nil {
		t.Error("a reader with no address was built")
	}
	if _, err := NewLND(LNDOptions{
		BaseURL: "https://tower:8080", MacaroonPath: "/nowhere/readonly.macaroon",
	}); err == nil {
		t.Error("a reader was built with a credential that does not exist")
	}

	dir := t.TempDir()
	macPath := filepath.Join(dir, "m")
	if err := os.WriteFile(macPath, []byte{1}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLND(LNDOptions{
		BaseURL: "https://tower:8080", MacaroonPath: macPath,
		TLSCertPath: filepath.Join(dir, "nope.cert"),
	}); err == nil {
		t.Error("a reader was built with a certificate that does not exist")
	}

	badCert := filepath.Join(dir, "bad.cert")
	if err := os.WriteFile(badCert, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLND(LNDOptions{
		BaseURL: "https://tower:8080", MacaroonPath: macPath, TLSCertPath: badCert,
	}); err == nil {
		t.Error("a reader was built pinning something that is not a certificate")
	}
}

// serveFixture answers from a payload captured off a real LND.
func serveFixture(t *testing.T, path, file string) *LND {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	return serve(t, map[string]func(http.ResponseWriter, *http.Request){
		path: json200(string(body)),
	})
}

// Captured from lnd v0.18.5-beta on regtest, because the proto does not say what
// the JSON looks like and guessing at it is how a decoder silently returns
// nothing.
func TestTheShapeARealNodeSendsIsDecoded(t *testing.T) {
	t.Parallel()
	l := serveFixture(t, "/v2/watchtower/client", "listtowers_with_sessions.json")

	towers, err := l.Towers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(towers) != 1 {
		t.Fatalf("got %d towers from a real payload, want 1", len(towers))
	}
	if !strings.HasPrefix(towers[0].Pubkey, "02abe7b6a178594b") {
		t.Errorf("the base64 pubkey was not converted to hex: %q", towers[0].Pubkey)
	}

	held := sessionsByPolicy(&towers[0])
	if len(held) != 3 {
		t.Fatalf("got sessions for %d policy types, want 3: %+v", len(held), held)
	}
	// Only the anchor sessions have taken anything: the node's one channel is an
	// anchor channel. The other two exist and are empty.
	if held[PolicyAnchor].backups != 12 {
		t.Errorf("anchor backups = %d, want 12", held[PolicyAnchor].backups)
	}
	if held[PolicyLegacy].backups != 0 || held[PolicyTaproot].backups != 0 {
		t.Errorf("legacy/taproot were expected to be empty: %+v", held)
	}
	if got := held[PolicyAnchor].feeSatPerKW; got == nil || *got != 2500 {
		t.Errorf("fee rate = %v, want 2500 sat/kW from the reported 10 sat/vB", got)
	}
}

// **The trap this fixture exists for.** A real node reports a `session_info`
// entry for every policy type from the moment of registration, each with an
// empty `sessions` list. Reading coverage off those entries would report every
// channel protected before a single session existed.
func TestPolicyEntriesWithNoSessionsCoverNothing(t *testing.T) {
	t.Parallel()
	l := serveFixture(t, "/v2/watchtower/client", "listtowers_just_registered.json")

	towers, err := l.Towers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(towers) != 1 {
		t.Fatalf("got %d towers, want 1", len(towers))
	}
	if len(towers[0].Sessions) != 0 {
		t.Errorf("a tower with no sessions produced %d: %+v",
			len(towers[0].Sessions), towers[0].Sessions)
	}
	if held := sessionsByPolicy(&towers[0]); len(held) != 0 {
		t.Errorf("policy entries with no sessions were counted as coverage: %+v", held)
	}
}

func TestTheTowerIdentityARealTowerSendsIsDecoded(t *testing.T) {
	t.Parallel()
	l := serveFixture(t, "/v2/watchtower/server", "towerinfo.json")

	got, err := l.Identity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.Pubkey, "02abe7b6a178594b") {
		t.Errorf("pubkey = %q, want the hex form", got.Pubkey)
	}
	if len(got.URIs) == 0 || !strings.Contains(got.URIs[0], "@") {
		t.Errorf("the address a user would paste was not read: %+v", got.URIs)
	}
	if len(got.Listeners) == 0 {
		t.Errorf("the listeners were not read: %+v", got.Listeners)
	}
}

// The tower's client follows a node that moved, like the registry's does.
//
// **This is the sibling that was forgotten.** The registry's client learned to
// re-resolve in 0.6.11; this one kept dialling a container address that no
// longer existed, so a user saw a healthy dashboard — the channel list had
// recovered — and a recurring "no route to host" in the log from the coverage
// check that had not.
func TestTheTowerClientFollowsANodeThatMoved(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Skipf("this system has no second loopback address: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"towers":[]}`))
		}))
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)

	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	host, _, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	l := &LND{
		// Pointed where nothing listens, with the name that now answers elsewhere.
		addr: nodeaddr.New("http://127.0.0.1:"+port, host, nil),
		http: &http.Client{Timeout: 2 * time.Second},
	}
	if _, err := l.Towers(t.Context()); err != nil {
		t.Fatalf("a node that had moved was not followed: %v", err)
	}
}
