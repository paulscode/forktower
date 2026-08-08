// Package nodeaddr keeps a client pointed at a node that has moved.
//
// **Because a container's address outlives neither the container nor the user's
// next update.** StartOS 0.4.x issues its Lightning node a certificate naming IP
// addresses and no DNS names, so its packaging resolves the sibling hostname
// once and dials the result — correct, and stale the moment the node is updated
// and comes back somewhere else. Every read afterwards fails with "no route to
// host" while the dashboard goes on serving what it last read, so nothing looks
// wrong until something changes.
//
// This exists as a package rather than a method on one client because the same
// fix was written for one of them and forgotten for its sibling, which shipped
// the identical bug a day later. One implementation, used by both.
package nodeaddr

import (
	"context"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ReresolveEvery is the shortest gap between two attempts to look a name up
// again.
//
// The reads that trigger it are minutes apart, so this only matters when
// something is failing repeatedly — and then it is the difference between one
// lookup a minute and one per failed request.
const ReresolveEvery = time.Minute

// Follower holds the address a client dials, and the name it came from.
//
// The zero value is not usable; call New.
type Follower struct {
	mu      sync.Mutex
	base    string
	host    string
	lastAt  time.Time
	now     func() time.Time
	resolve func(ctx context.Context, host string) ([]string, error)
}

// New builds a Follower.
//
// An empty host leaves the address fixed: there is nothing to look up, and
// Moved always reports false. That is the honest answer for a client configured
// with a name it dials directly, which needs none of this.
func New(base, host string, now func() time.Time) *Follower {
	if now == nil {
		now = time.Now
	}
	return &Follower{
		base: strings.TrimRight(base, "/"),
		host: host,
		now:  now,
		resolve: func(ctx context.Context, h string) ([]string, error) {
			return net.DefaultResolver.LookupHost(ctx, h)
		},
	}
}

// Base is the address to dial now.
func (f *Follower) Base() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.base
}

// Moved looks the name up again and reports whether the address changed.
//
// **False means "do not retry", and it means it in every doubtful case**: no
// name to look up, a lookup that failed, a lookup inside the cooldown, or an
// answer that is what was already being dialled. That last one is what stops a
// node which is simply down from producing a retry for every request — it
// resolves to the same address, so nothing has moved and nothing is retried.
func (f *Follower) Moved(ctx context.Context) bool {
	f.mu.Lock()
	host, last, current := f.host, f.lastAt, f.base
	f.mu.Unlock()

	if host == "" {
		return false
	}
	now := f.now()
	if !last.IsZero() && now.Sub(last) < ReresolveEvery {
		return false
	}

	addrs, err := f.resolve(ctx, host)
	f.mu.Lock()
	f.lastAt = now
	f.mu.Unlock()
	if err != nil || len(addrs) == 0 {
		return false
	}

	updated, changed := swapHost(current, addrs[0])
	if !changed {
		return false
	}
	f.mu.Lock()
	f.base = updated
	f.mu.Unlock()
	return true
}

// swapHost rewrites the host of a URL, keeping the scheme, port and path.
//
// The port is kept deliberately: on the platform this is for, the port is fixed
// by the package and only the address moves.
func swapHost(raw, addr string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return raw, false
	}
	host := addr
	if port := u.Port(); port != "" {
		host = net.JoinHostPort(addr, port)
	}
	if host == u.Host {
		return raw, false
	}
	u.Host = host
	return strings.TrimRight(u.String(), "/"), true
}
