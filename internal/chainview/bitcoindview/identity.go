package bitcoindview

import (
	"context"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/paulscode/forktower/internal/chainview"
)

// networkInfoJSON is the subset of `getnetworkinfo` this package needs.
type networkInfoJSON struct {
	Subversion     string `json:"subversion"`
	LocalAddresses []struct {
		Address string `json:"address"`
		Port    int    `json:"port"`
	} `json:"localaddresses"`
}

// peerInfoJSON is the subset of `getpeerinfo` used to identify this node.
type peerInfoJSON struct {
	// AddrBind is the local socket this connection uses. For an inbound peer that
	// is the node's own listening address, which no second process on the same
	// host can also be bound to — so it identifies the node rather than merely
	// describing it.
	AddrBind string `json:"addrbind"`
	Inbound  bool   `json:"inbound"`
}

// Identity describes the node behind this view.
//
// The whole product rests on having two *independent* views: two configurations
// pointing at one node produce views that agree by construction, so divergence
// becomes unrepresentable and every indicator stays green forever while nothing
// is watched. A single mis-wired setting does it. This is what lets that be
// checked rather than assumed.
//
// Three sources, in decreasing order of how often they are available:
//
//   - the endpoint, which catches the literal case of one URL written twice;
//   - the node's own listening addresses, when it has discovered any — on a home
//     node behind a router that is usually nothing;
//   - the local address of each *inbound* connection, which is the node's
//     listening socket. Two processes cannot bind the same host and port, so a
//     match here is proof rather than a hint. This is the one that actually
//     works in practice.
//
// A node with no peers at all supplies only the first, and the check is reported
// as unavailable rather than passed — which is the honest answer, and one the
// caller is written to distinguish.
func (v *View) Identity(ctx context.Context) (chainview.Identity, error) {
	id := chainview.Identity{Endpoint: normaliseEndpoint(v.c.opts.RPCURL)}

	var info networkInfoJSON
	if err := v.c.call(ctx, &info, "getnetworkinfo"); err != nil {
		return chainview.Identity{}, mapError(err)
	}
	id.Subversion = info.Subversion

	seen := map[string]struct{}{}
	add := func(addr string) {
		addr = strings.TrimSpace(strings.ToLower(addr))
		if addr == "" {
			return
		}
		if _, dup := seen[addr]; dup {
			return
		}
		seen[addr] = struct{}{}
		id.LocalAddresses = append(id.LocalAddresses, addr)
	}

	for _, local := range info.LocalAddresses {
		add(joinHostPort(local.Address, local.Port))
	}

	// Failure here is not fatal: peers come and go, and the addresses above may
	// already be enough. What must not happen is inventing one.
	var peers []peerInfoJSON
	if err := v.c.call(ctx, &peers, "getpeerinfo"); err == nil {
		for _, peer := range peers {
			if peer.Inbound {
				add(peer.AddrBind)
			}
		}
	}

	// Sorted so two reads of the same node produce the same identity, which makes
	// the comparison stable and any diagnostic readable.
	sort.Strings(id.LocalAddresses)
	return id, nil
}

// normaliseEndpoint strips what does not distinguish one node from another, so
// that two spellings of the same address compare equal — and, importantly, drops
// any credentials before this string reaches a log or a diagnostic.
func normaliseEndpoint(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

func joinHostPort(host string, port int) string {
	if host == "" {
		return ""
	}
	if port == 0 {
		return host
	}
	// Not net.JoinHostPort: an IPv6 address from this RPC arrives without
	// brackets, and adding them here would make it compare unequal to the same
	// address seen elsewhere.
	return host + ":" + strconv.Itoa(port)
}
