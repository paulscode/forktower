package cln

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paulscode/forktower/internal/store"
)

type fakeNode struct {
	server   *httptest.Server
	replies  map[string]string
	status   map[string]int
	runes    chan string
	requests atomic.Int64
}

func newFakeNode(t *testing.T) *fakeNode {
	t.Helper()
	n := &fakeNode{
		replies: map[string]string{},
		status:  map[string]int{},
		runes:   make(chan string, 16),
	}
	n.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.requests.Add(1)
		select {
		case n.runes <- r.Header.Get("Rune"):
		default:
		}
		if code, ok := n.status[r.URL.Path]; ok {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"error":"nope"}`))
			return
		}
		body, ok := n.replies[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(n.server.Close)
	return n
}

func runeFile(t *testing.T, restrictions string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "forktower.rune")
	if err := os.WriteFile(path, []byte(makeRune(t, restrictions)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newTestClient(t *testing.T, n *fakeNode, runePath string) *Client {
	t.Helper()
	c, err := New(Options{BaseURL: n.server.URL, RunePath: runePath, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const getinfoBody = `{"id":"02aabb","alias":"my node","blockheight":850000}`

func TestSnapshotReadsTheNodesChannels(t *testing.T) {
	t.Parallel()

	n := newFakeNode(t)
	n.replies["/v1/getinfo"] = getinfoBody
	raw, err := os.ReadFile(filepath.Join("testdata", "listpeerchannels.json"))
	if err != nil {
		t.Fatal(err)
	}
	n.replies["/v1/listpeerchannels"] = string(raw)

	c := newTestClient(t, n, runeFile(t, "method^list|method=getinfo"))
	snap, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if snap.Node.Pubkey != "02aabb" || snap.Node.Impl != store.ImplCLN {
		t.Errorf("node = %+v", snap.Node)
	}
	if len(snap.Channels) != 2 {
		t.Fatalf("got %d channels, want 2", len(snap.Channels))
	}

	// A channel the node knows is closing is one to look at before the block
	// arrives — the node's belief is earlier than the chain's answer, which is
	// exactly why it is worth carrying.
	var closing int
	for _, ch := range snap.Channels {
		if ch.CloseState == store.ClosePending {
			closing++
			if ch.CloseTxID == "" {
				t.Error("a closing channel carries no closing transaction")
			}
		}
	}
	if closing != 1 {
		t.Errorf("got %d closing channels, want 1", closing)
	}
}

// The credential goes in a header on every request, and nowhere else.
func TestTheCredentialIsSent(t *testing.T) {
	t.Parallel()

	n := newFakeNode(t)
	n.replies["/v1/getinfo"] = getinfoBody

	c := newTestClient(t, n, runeFile(t, "method^list"))
	if _, err := c.Info(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-n.runes:
		if got == "" {
			t.Fatal("the request carried no credential")
		}
		if got != c.Credential().Token {
			t.Error("the credential sent is not the one that was loaded")
		}
	default:
		t.Fatal("no request reached the node")
	}
}

// One unreadable channel must not lose the others.
func TestOneBadChannelDoesNotLoseTheRest(t *testing.T) {
	t.Parallel()

	n := newFakeNode(t)
	n.replies["/v1/getinfo"] = getinfoBody
	n.replies["/v1/listpeerchannels"] = `{"channels":[
		{"peer_id":"03aa","state":"CHANNELD_NORMAL","funding_txid":"short","total_msat":"1msat"},
		{"peer_id":"03bb","state":"CHANNELD_NORMAL",
		 "funding_txid":"4444444444444444444444444444444444444444444444444444444444444444",
		 "funding_outnum":0,"total_msat":"70000000msat"}]}`

	c := newTestClient(t, n, runeFile(t, "method^list"))
	snap, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Channels) != 1 {
		t.Fatalf("got %d channels, want the readable one kept", len(snap.Channels))
	}
	if snap.Channels[0].CapacitySat != 70_000 {
		t.Errorf("got %+v", snap.Channels[0])
	}
}

func TestAFailingNodeIsReported(t *testing.T) {
	t.Parallel()

	n := newFakeNode(t)
	n.replies["/v1/getinfo"] = getinfoBody
	n.status["/v1/listpeerchannels"] = http.StatusUnauthorized

	c := newTestClient(t, n, runeFile(t, "method^list"))
	if _, err := c.Snapshot(context.Background()); err == nil {
		t.Error("a node refusing the channel list was reported as success")
	}

	n.replies["/v1/getinfo"] = `{"alias":"anonymous"}`
	if _, err := c.Info(context.Background()); err == nil {
		t.Error("a node that did not say who it is was accepted")
	}
}

// An unrestricted credential is reported and never refused: a user whose rune is
// broader than we would like is still a user who needs protecting.
func TestAnUnrestrictedCredentialStillWorks(t *testing.T) {
	t.Parallel()

	n := newFakeNode(t)
	n.replies["/v1/getinfo"] = getinfoBody

	path := filepath.Join(t.TempDir(), "wide.rune")
	if err := os.WriteFile(path,
		[]byte(base64.RawURLEncoding.EncodeToString(make([]byte, runeHashBytes))), 0o600); err != nil {
		t.Fatal(err)
	}

	c := newTestClient(t, n, path)
	if !c.Credential().Unrestricted() {
		t.Error("an unrestricted rune was not recognised as such")
	}
	if _, err := c.Info(context.Background()); err != nil {
		t.Errorf("an unrestricted credential was refused: %v", err)
	}
}

func TestNewRejectsWhatCannotWork(t *testing.T) {
	t.Parallel()

	n := newFakeNode(t)
	if _, err := New(Options{RunePath: runeFile(t, "method^list")}); err == nil {
		t.Error("a client with no address was accepted")
	}
	if _, err := New(Options{BaseURL: n.server.URL, RunePath: "/nowhere"}); err == nil {
		t.Error("a client with no credential was accepted")
	}

	notACert := filepath.Join(t.TempDir(), "nope.cert")
	if err := os.WriteFile(notACert, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{BaseURL: n.server.URL, RunePath: runeFile(t, "method^list"),
		TLSCertPath: notACert}); err == nil {
		t.Error("a file that is not a certificate was accepted")
	}
	// clnrest is commonly plain http on loopback, where there is nothing to pin.
	if _, err := New(Options{BaseURL: n.server.URL,
		RunePath: runeFile(t, "method^list")}); err != nil {
		t.Errorf("a plain-http node was refused: %v", err)
	}
}
