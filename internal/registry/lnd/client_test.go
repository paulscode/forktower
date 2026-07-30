package lnd

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paulscode/forktower/internal/store"
)

// fakeNode is a stand-in LND, served over TLS with a certificate the client is
// given — which is the arrangement a real deployment has.
type fakeNode struct {
	server *httptest.Server
	cert   string

	mu        atomic.Int64
	macaroons chan string
	handlers  map[string]string
	status    map[string]int
}

func newFakeNode(t *testing.T) *fakeNode {
	t.Helper()

	n := &fakeNode{
		macaroons: make(chan string, 16),
		handlers:  map[string]string{},
		status:    map[string]int{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		n.mu.Add(1)
		select {
		case n.macaroons <- r.Header.Get("Grpc-Metadata-macaroon"):
		default:
		}
		if code, ok := n.status[r.URL.Path]; ok {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"error":"nope"}`))
			return
		}
		body, ok := n.handlers[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	})

	n.server = httptest.NewTLSServer(mux)
	t.Cleanup(n.server.Close)

	// Write the server's own certificate out, as LND writes tls.cert.
	certPEM := pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: n.server.Certificate().Raw,
	})
	path := filepath.Join(t.TempDir(), "tls.cert")
	if err := os.WriteFile(path, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	n.cert = path
	return n
}

func (n *fakeNode) reply(path, body string) { n.handlers[path] = body }

// macaroonFile writes a macaroon of the given permissions and returns its path.
func macaroonFile(t *testing.T, ops ...[]byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "forktower.macaroon")
	if err := os.WriteFile(path, macaroonV2(bakeryIdentifier(ops...)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newTestClient(t *testing.T, n *fakeNode, macaroon string) *Client {
	t.Helper()
	c, err := New(Options{
		BaseURL: n.server.URL, TLSCertPath: n.cert, MacaroonPath: macaroon,
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const getinfoBody = `{"identity_pubkey":"02aabb","alias":"my node","synced_to_chain":true}`

func TestSnapshotReadsTheNodesChannels(t *testing.T) {
	t.Parallel()

	n := newFakeNode(t)
	n.reply("/v1/getinfo", getinfoBody)
	raw, err := os.ReadFile(filepath.Join("testdata", "listchannels.json"))
	if err != nil {
		t.Fatal(err)
	}
	n.reply("/v1/channels", string(raw))
	n.reply("/v1/channels/pending", `{"pending_force_closing_channels":[{
		"channel":{"channel_point":"2222222222222222222222222222222222222222222222222222222222222222:0",
		"remote_pubkey":"03cc","capacity":"50000","commitment_type":"ANCHORS",
		"local_constraints":{"csv_delay":144},"remote_constraints":{"csv_delay":144}},
		"closing_txid":"3333333333333333333333333333333333333333333333333333333333333333"}]}`)

	c := newTestClient(t, n, macaroonFile(t, bakeryOp("info", "read")))
	snap, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if snap.Node.Pubkey != "02aabb" || snap.Node.Impl != store.ImplLND {
		t.Errorf("node = %+v", snap.Node)
	}
	// Two open plus one closing. A channel that is closing is exactly when it
	// matters most, so a picture that omitted it would be reassuring and wrong.
	if len(snap.Channels) != 3 {
		t.Fatalf("got %d channels, want 3 including the one closing", len(snap.Channels))
	}

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
func TestTheCredentialIsSentAndNotLogged(t *testing.T) {
	t.Parallel()

	n := newFakeNode(t)
	n.reply("/v1/getinfo", getinfoBody)

	path := macaroonFile(t, bakeryOp("info", "read"))
	c := newTestClient(t, n, path)

	if _, err := c.Info(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-n.macaroons:
		if got == "" {
			t.Fatal("the request carried no credential")
		}
		if got != c.cred.Hex {
			t.Error("the credential sent is not the one that was loaded")
		}
	default:
		t.Fatal("no request reached the node")
	}
}

// One unreadable channel must not lose the others: the rest still need watching.
func TestOneBadChannelDoesNotLoseTheRest(t *testing.T) {
	t.Parallel()

	n := newFakeNode(t)
	n.reply("/v1/getinfo", getinfoBody)
	n.reply("/v1/channels", `{"channels":[
		{"channel_point":"not-a-channel-point","capacity":"1"},
		{"channel_point":"4444444444444444444444444444444444444444444444444444444444444444:0",
		 "remote_pubkey":"03dd","capacity":"70000","commitment_type":"ANCHORS"}]}`)
	n.reply("/v1/channels/pending", `{}`)

	c := newTestClient(t, n, macaroonFile(t, bakeryOp("info", "read")))
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

// Losing the closing channels is worth a warning, not the whole snapshot.
func TestAFailingPendingCallDoesNotLoseTheOpenChannels(t *testing.T) {
	t.Parallel()

	n := newFakeNode(t)
	n.reply("/v1/getinfo", getinfoBody)
	n.reply("/v1/channels", `{"channels":[{"channel_point":"5555555555555555555555555555555555555555555555555555555555555555:0","remote_pubkey":"03ee","capacity":"1000","commitment_type":"ANCHORS"}]}`)
	n.status["/v1/channels/pending"] = http.StatusInternalServerError

	c := newTestClient(t, n, macaroonFile(t, bakeryOp("info", "read")))
	snap, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("a failing pending call lost the whole snapshot: %v", err)
	}
	if len(snap.Channels) != 1 {
		t.Errorf("got %d channels", len(snap.Channels))
	}
}

// The certificate is the node's identity here. A client that accepted any
// certificate for this address would have no way to notice being pointed
// somewhere else.
func TestTheCertificateIsPinned(t *testing.T) {
	t.Parallel()

	n := newFakeNode(t)
	n.reply("/v1/getinfo", getinfoBody)

	// A different, perfectly valid certificate.
	other := filepath.Join(t.TempDir(), "other.cert")
	if err := os.WriteFile(other, selfSignedPEM(t), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := New(Options{
		BaseURL: n.server.URL, TLSCertPath: other,
		MacaroonPath: macaroonFile(t, bakeryOp("info", "read")),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Info(context.Background()); err == nil {
		t.Error("the client accepted a node presenting a different certificate")
	}
}

func TestNewRejectsWhatCannotWork(t *testing.T) {
	t.Parallel()

	n := newFakeNode(t)
	mac := macaroonFile(t, bakeryOp("info", "read"))

	if _, err := New(Options{TLSCertPath: n.cert, MacaroonPath: mac}); err == nil {
		t.Error("a client with no address was accepted")
	}
	if _, err := New(Options{BaseURL: n.server.URL, MacaroonPath: mac}); err == nil {
		t.Error("a client with no certificate was accepted; it pins rather than trusts")
	}
	if _, err := New(Options{BaseURL: n.server.URL, TLSCertPath: n.cert,
		MacaroonPath: "/nowhere"}); err == nil {
		t.Error("a client with no credential was accepted")
	}

	notACert := filepath.Join(t.TempDir(), "nope.cert")
	if err := os.WriteFile(notACert, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{BaseURL: n.server.URL, TLSCertPath: notACert,
		MacaroonPath: mac}); err == nil {
		t.Error("a file that is not a certificate was accepted")
	}
}

// An over-privileged credential is reported, never refused: both target
// platforms hand out admin macaroons, and refusing means no protection at all.
func TestAnAdminCredentialStillWorks(t *testing.T) {
	t.Parallel()

	n := newFakeNode(t)
	n.reply("/v1/getinfo", getinfoBody)

	admin := macaroonFile(t,
		bakeryOp("info", "read", "write"),
		bakeryOp("macaroon", "generate"))

	c := newTestClient(t, n, admin)
	if !c.Credential().Overprivileged() {
		t.Error("an admin credential was not recognised as over-privileged")
	}
	if _, err := c.Info(context.Background()); err != nil {
		t.Errorf("an admin credential was refused: %v", err)
	}
	// The summary names permissions, never the credential.
	if strings.Contains(c.Credential().summary(), c.Credential().Hex) {
		t.Error("the summary contains the credential itself")
	}
}

func TestWatchIsOnlyANudge(t *testing.T) {
	t.Parallel()

	n := newFakeNode(t)
	n.reply("/v1/channels/subscribe", "{\"result\":{}}\n{\"result\":{}}\n")

	c := newTestClient(t, n, macaroonFile(t, bakeryOp("info", "read")))

	var nudges atomic.Int64
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Watch(ctx, func() { nudges.Add(1) }); err != nil {
		t.Fatalf("watching returned %v", err)
	}
	if nudges.Load() != 2 {
		t.Errorf("got %d nudges, want one per event", nudges.Load())
	}
}

// selfSignedPEM makes a certificate for something else entirely.
func selfSignedPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "somewhere else"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
