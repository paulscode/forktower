package bitcoindview

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/paulscode/forktower/internal/chainview"
)

// The check this feeds is the one whose failure is invisible everywhere else:
// two configurations pointing at one node produce views that agree by
// construction, so every indicator stays green forever while nothing is watched.
func TestIdentityGathersEnoughToTellTwoNodesApart(t *testing.T) {
	t.Parallel()

	node := newFakeNode(t).
		reply("getnetworkinfo", map[string]any{
			"subversion": "/Satoshi:29.3.0/Knots:20260508/",
			"localaddresses": []map[string]any{
				{"address": "203.0.113.7", "port": 8333},
			},
		}).
		reply("getpeerinfo", []map[string]any{
			// The node's own listening socket. No second process on this host can
			// also be bound to it, so a match here is proof rather than a hint.
			{"addrbind": "172.22.0.2:8333", "inbound": true},
			// An outbound connection binds an ephemeral port and says nothing
			// about which node this is.
			{"addrbind": "172.22.0.2:54886", "inbound": false},
		})

	v := newTestView(t, node)
	id, err := v.Identity(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if id.Subversion != "/Satoshi:29.3.0/Knots:20260508/" {
		t.Errorf("subversion = %q", id.Subversion)
	}
	if !strings.HasPrefix(id.Endpoint, "http://127.0.0.1:") {
		t.Errorf("endpoint = %q, want the node's address", id.Endpoint)
	}

	want := map[string]bool{"203.0.113.7:8333": false, "172.22.0.2:8333": false}
	for _, addr := range id.LocalAddresses {
		if _, expected := want[addr]; !expected {
			t.Errorf("unexpected address %q — an outbound connection's local port "+
				"identifies nothing", addr)
		}
		want[addr] = true
	}
	for addr, found := range want {
		if !found {
			t.Errorf("%q is missing, so a comparison would have less to go on", addr)
		}
	}
}

// A home node behind a router usually has no discovered address of its own. The
// listening socket seen on an inbound connection is what makes the check work in
// practice, so this is the case that matters most.
func TestIdentityWorksWithNoDiscoveredAddresses(t *testing.T) {
	t.Parallel()

	node := newFakeNode(t).
		reply("getnetworkinfo", map[string]any{
			"subversion":     "/Satoshi:28.0.0/",
			"localaddresses": []any{},
		}).
		reply("getpeerinfo", []map[string]any{
			{"addrbind": "172.22.0.3:18445", "inbound": true},
		})

	id, err := newTestView(t, node).Identity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(id.LocalAddresses) != 1 || id.LocalAddresses[0] != "172.22.0.3:18445" {
		t.Errorf("addresses = %v, want the listening socket", id.LocalAddresses)
	}
}

// Two views of genuinely different nodes must be provable as such, and two views
// of one node must be caught. Asserted through the check that consumes this,
// because the identity is only worth what that check can do with it.
func TestIdentityIsEnoughForTheDistinctNodeCheck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	newNode := func(bind string) *View {
		return newTestView(t, newFakeNode(t).
			reply("getnetworkinfo", map[string]any{
				"subversion": "/Satoshi:28.0.0/", "localaddresses": []any{},
			}).
			reply("getpeerinfo", []map[string]any{
				{"addrbind": bind, "inbound": true},
			}))
	}

	// Two nodes, each with its own listening socket.
	if err := chainview.VerifyDistinct(ctx, newNode("172.22.0.2:18445"),
		newNode("172.22.0.3:18445")); err != nil {
		t.Errorf("two separate nodes were not accepted: %v", err)
	}

	// The same node reached two ways: a different URL, the same socket. This is
	// the mis-wiring the check exists for, and comparing endpoints alone would
	// miss it.
	err := chainview.VerifyDistinct(ctx, newNode("172.22.0.2:18445"), newNode("172.22.0.2:18445"))
	if !errors.Is(err, chainview.ErrSameNode) {
		t.Errorf("got %v, want ErrSameNode", err)
	}
}

// A node with no peers supplies nothing to corroborate the endpoints. Reporting
// that honestly is the point: claiming the check passed would be the same false
// assurance the check exists to prevent.
func TestIdentityWithNothingToGoOn(t *testing.T) {
	t.Parallel()

	node := newFakeNode(t).
		reply("getnetworkinfo", map[string]any{"subversion": "/Satoshi:28.0.0/"}).
		reply("getpeerinfo", []any{})

	id, err := newTestView(t, node).Identity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(id.LocalAddresses) != 0 {
		t.Errorf("addresses = %v, want none invented", id.LocalAddresses)
	}
	if id.Endpoint == "" {
		t.Error("the endpoint is always available and must still be reported")
	}
}

// The endpoint travels into diagnostics, so anything a user put in the URL must
// not travel with it.
func TestTheReportedEndpointCarriesNoCredentials(t *testing.T) {
	t.Parallel()

	got := normaliseEndpoint("http://someone:hunter2@127.0.0.1:8332/")
	if strings.Contains(got, "hunter2") || strings.Contains(got, "someone") {
		t.Errorf("normaliseEndpoint kept the credentials: %q", got)
	}
	if !strings.Contains(got, "127.0.0.1:8332") {
		t.Errorf("normaliseEndpoint lost the address: %q", got)
	}

	// Two spellings of one address must compare equal, or the check reports two
	// views of one node as separate.
	if normaliseEndpoint("HTTP://127.0.0.1:8332") != normaliseEndpoint("http://127.0.0.1:8332/") {
		t.Error("case and a trailing slash produce different identities")
	}
	// Something unparseable is returned as it is rather than dropped: a
	// comparison against a garbled string is still better than against nothing.
	if got := normaliseEndpoint("::not a url::"); got == "" {
		t.Error("an unparseable endpoint became empty")
	}
}

// A node that will not answer produces an error, not an identity built from
// half the sources.
func TestIdentityFailsRatherThanGuessing(t *testing.T) {
	t.Parallel()

	node := newFakeNode(t).fail("getnetworkinfo", -28, "Loading block index...")
	if _, err := newTestView(t, node).Identity(context.Background()); err == nil {
		t.Error("a node that refused to describe itself produced an identity anyway")
	}
}
