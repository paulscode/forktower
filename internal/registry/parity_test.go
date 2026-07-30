package registry_test

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/paulscode/forktower/internal/registry"
	"github.com/paulscode/forktower/internal/registry/cln"
	"github.com/paulscode/forktower/internal/registry/lnd"
	"github.com/paulscode/forktower/internal/store"
)

// The two adapters exist so that everything downstream sees one thing. That is
// only true if they actually agree, and the way they could quietly stop agreeing
// is a field mapped from the wrong side — which is exactly the mistake the CSV
// delays invite.
//
// Both fixtures describe the same channel, each in its own node's dialect. This
// drives the real clients over HTTP, so it goes through the whole adapter rather
// than the mapping alone.
func TestBothAdaptersDescribeTheSameChannelTheSameWay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	fromLND := lndSnapshot(t, ctx)
	fromCLN := clnSnapshot(t, ctx)

	lndChan := findChannel(t, fromLND, sharedFundingTxID)
	clnChan := findChannel(t, fromCLN, sharedFundingTxID)

	if !reflect.DeepEqual(lndChan, clnChan) {
		t.Errorf("the adapters disagree about the same channel:\n  lnd: %+v\n  cln: %+v",
			lndChan, clnChan)
		// Name the fields, because "they differ" is not actionable.
		compare(t, "capacity", lndChan.CapacitySat, clnChan.CapacitySat)
		compare(t, "channel type", lndChan.ChanType, clnChan.ChanType)
		compare(t, "csv_delay_local", derefDelay(lndChan.CSVDelayLocal), derefDelay(clnChan.CSVDelayLocal))
		compare(t, "csv_delay_remote", derefDelay(lndChan.CSVDelayRemote), derefDelay(clnChan.CSVDelayRemote))
		compare(t, "open height", lndChan.OpenHeight, clnChan.OpenHeight)
		compare(t, "peer", lndChan.PeerPubkey, clnChan.PeerPubkey)
	}

	// And specifically the pair that would be wrong in the same way on both sides
	// if the rule had been misread once and copied.
	if derefDelay(lndChan.CSVDelayLocal) != 144 || derefDelay(lndChan.CSVDelayRemote) != 720 {
		t.Errorf("the delays are not what both fixtures state: %v/%v",
			lndChan.CSVDelayLocal, lndChan.CSVDelayRemote)
	}

	// The HTLCs too: same amounts, same expiries, same directions.
	if len(lndChan.HTLCs) != len(clnChan.HTLCs) {
		t.Fatalf("got %d HTLCs from LND and %d from CLN",
			len(lndChan.HTLCs), len(clnChan.HTLCs))
	}
	for _, want := range clnChan.HTLCs {
		if !containsHTLC(lndChan.HTLCs, want) {
			t.Errorf("CLN reported %+v, which LND's adapter did not produce", want)
		}
	}
}

const sharedFundingTxID = "8f2c4a1b9e5d3c7f0a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f6071"

func lndSnapshot(t *testing.T, ctx context.Context) registry.Snapshot {
	t.Helper()

	channels := readFixture(t, filepath.Join("lnd", "testdata", "listchannels.json"))
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/getinfo":
			_, _ = w.Write([]byte(`{"identity_pubkey":"02aabb","alias":"node"}`))
		case "/v1/channels":
			_, _ = w.Write(channels)
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)

	c, err := lnd.New(lnd.Options{
		BaseURL:      srv.URL,
		TLSCertPath:  certFile(t, srv.Certificate().Raw),
		MacaroonPath: macaroonFile(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := c.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func clnSnapshot(t *testing.T, ctx context.Context) registry.Snapshot {
	t.Helper()

	channels := readFixture(t, filepath.Join("cln", "testdata", "listpeerchannels.json"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/getinfo":
			_, _ = w.Write([]byte(`{"id":"02aabb","alias":"node"}`))
		case "/v1/listpeerchannels":
			_, _ = w.Write(channels)
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)

	c, err := cln.New(cln.Options{BaseURL: srv.URL, RunePath: runeFile(t)})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := c.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// --- fixtures and credentials -------------------------------------------------

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func certFile(t *testing.T, der []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tls.cert")
	body := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// macaroonFile writes a minimal read-only macaroon, in the two nested formats
// the adapter reads.
func macaroonFile(t *testing.T) string {
	t.Helper()

	varint := func(v uint64) []byte {
		buf := make([]byte, binary.MaxVarintLen64)
		return buf[:binary.PutUvarint(buf, v)]
	}
	field := func(num uint64, value []byte) []byte {
		out := varint(num<<3 | 2)
		out = append(out, varint(uint64(len(value)))...)
		return append(out, value...)
	}

	op := field(1, []byte("info"))
	op = append(op, field(2, []byte("read"))...)

	id := []byte{3}
	id = append(id, field(1, []byte("nonce"))...)
	id = append(id, field(2, []byte("0"))...)
	id = append(id, field(3, op)...)

	mac := []byte{2}
	add := func(fieldType uint64, data []byte) {
		mac = append(mac, varint(fieldType)...)
		mac = append(mac, varint(uint64(len(data)))...)
		mac = append(mac, data...)
	}
	add(1, []byte("lnd"))
	add(2, id)
	mac = append(mac, varint(0)...)
	add(6, make([]byte, 32))

	path := filepath.Join(t.TempDir(), "forktower.macaroon")
	if err := os.WriteFile(path, mac, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// runeFile writes a rune restricted to the read methods.
func runeFile(t *testing.T) string {
	t.Helper()
	body := append(make([]byte, 32), []byte("method^list|method=getinfo")...)
	path := filepath.Join(t.TempDir(), "forktower.rune")
	if err := os.WriteFile(path,
		[]byte(base64.RawURLEncoding.EncodeToString(body)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// --- helpers ------------------------------------------------------------------

func findChannel(t *testing.T, snap registry.Snapshot, txid string) registry.ChannelRecord {
	t.Helper()
	for _, ch := range snap.Channels {
		if ch.FundingTxID == txid {
			return ch
		}
	}
	t.Fatalf("no channel with funding %s in %d channels", txid, len(snap.Channels))
	return registry.ChannelRecord{}
}

func compare(t *testing.T, what string, a, b any) {
	t.Helper()
	if !reflect.DeepEqual(a, b) {
		t.Errorf("  %s: lnd says %v, cln says %v", what, a, b)
	}
}

func derefDelay(v *int32) int32 {
	if v == nil {
		return -1
	}
	return *v
}

func containsHTLC(all []store.HTLCSnapshot, want store.HTLCSnapshot) bool {
	for _, h := range all {
		if h == want {
			return true
		}
	}
	return false
}

var _ = tls.VersionTLS12
