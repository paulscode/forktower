package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"text/tabwriter"
	"time"
)

// initialBlocks is how many are mined when the world comes up.
//
// Two hundred puts the coinbase maturity window behind us, so the wallets have
// spendable funds for anything built on top of this later.
const initialBlocks = 200

const (
	readyTimeout      = 90 * time.Second
	propagationWindow = 30 * time.Second
)

// commandUp brings the world up and leaves both nodes agreeing.
//
// Idempotent: running it against a world that is already up connects anything
// that has drifted apart and tops the chain up, rather than refusing.
func commandUp(ctx context.Context) error {
	say("Starting two Bitcoin nodes…")
	if err := compose(ctx, "up", "-d"); err != nil {
		return err
	}
	if err := waitReady(ctx, readyTimeout); err != nil {
		return err
	}

	sf, sq := newClient(nodes()[0].rpcURL), newClient(nodes()[1].rpcURL)

	// A world that is already split is left exactly as it is. Reconnecting the
	// nodes here would quietly undo whatever someone was in the middle of
	// demonstrating, and `up` is the command people run when they are not sure
	// what state things are in.
	sfInfo, err := sf.chainInfo(ctx)
	if err != nil {
		return err
	}
	sqInfo, err := sq.chainInfo(ctx)
	if err != nil {
		return err
	}
	if sfInfo.Best != sqInfo.Best {
		say("Both nodes are up, and already on different chains "+
			"(heights %d and %d). Run `forkbench down` then `up` to start over.",
			sfInfo.Blocks, sqInfo.Blocks)
		return nil
	}

	if err := connect(ctx); err != nil {
		return err
	}

	if sfInfo.Blocks < initialBlocks {
		needed := initialBlocks - sfInfo.Blocks
		say("Mining %d blocks so there is a chain to watch…", needed)
		if _, err := sf.mine(ctx, int(needed)); err != nil {
			return err
		}
	}

	// Waiting for the *same block*, not the same height. Two chains at equal
	// height that do not agree is the entire situation this world exists to
	// produce, so a height comparison here would report a world as ready while it
	// was already split — and would have been the tool quietly lying about the one
	// thing it is for.
	say("Waiting for both nodes to agree…")
	target, err := waitForAgreement(ctx, sf, sq, propagationWindow)
	if err != nil {
		return fmt.Errorf("the two nodes did not converge: %w", err)
	}

	say("Ready. Both nodes are on the same chain at height %d.", target)
	say("Point Forktower at %s and %s, then run: forkbench split",
		nodes()[0].rpcURL, nodes()[1].rpcURL)
	return nil
}

// connect makes the two nodes peers, and lifts any ban a previous split left.
func connect(ctx context.Context) error {
	all := nodes()
	for i, n := range all {
		c := newClient(n.rpcURL)
		other := all[(i+1)%len(all)]

		// Lifting the ban first: addnode has no effect while the peer is banned,
		// and a silent no-op here would leave a world that looks connected and is
		// not.
		if err := c.call(ctx, "setban", []any{other.p2p, "remove"}, nil); err != nil {
			// Not banned is the ordinary case.
			var rpcErr *rpcError
			if !errors.As(err, &rpcErr) {
				return err
			}
		}
		if err := c.call(ctx, "addnode", []any{other.p2p, "onetry"}, nil); err != nil {
			var rpcErr *rpcError
			if !errors.As(err, &rpcErr) {
				return err
			}
		}
	}
	return nil
}

// commandSplit makes the two nodes disagree, permanently.
//
// The shape of it matters, and it is not simply "mine on both sides". Two chains
// of equal height that have merely not seen each other will merge the moment they
// reconnect, which would make this world lie about what it is demonstrating.
// Instead one node is made to *reject* a block the other accepted — which is what
// a rule change actually does — and after that no amount of reconnection heals it.
//
// Idempotent: a world that is already split is left alone.
func commandSplit(ctx context.Context) error {
	sf, sq := newClient(nodes()[0].rpcURL), newClient(nodes()[1].rpcURL)

	sfInfo, err := sf.chainInfo(ctx)
	if err != nil {
		return err
	}
	sqInfo, err := sq.chainInfo(ctx)
	if err != nil {
		return err
	}
	if sfInfo.Best != sqInfo.Best {
		say("Already split: the nodes are on different chains at heights %d and %d.",
			sfInfo.Blocks, sqInfo.Blocks)
		return nil
	}

	// One block, mined by the node standing in for the rest of the network, seen
	// by both. This is the block the other node is about to decide it will not
	// accept.
	say("Mining the block the two nodes are going to disagree about…")
	hashes, err := sq.mine(ctx, 1)
	if err != nil {
		return err
	}
	if len(hashes) != 1 {
		return fmt.Errorf("expected one block, got %d", len(hashes))
	}
	contested := hashes[0]

	if err := waitForBlock(ctx, sf, contested, propagationWindow); err != nil {
		return fmt.Errorf("the first node never saw the block: %w", err)
	}

	say("Separating the nodes…")
	if err := disconnect(ctx); err != nil {
		return err
	}

	// The rejection. From here the first node will not build on that block, nor
	// accept anything built on top of it, however long the other chain grows.
	say("Making the first node reject it…")
	if err := sf.call(ctx, "invalidateblock", []any{contested}, nil); err != nil {
		var rpcErr *rpcError
		if !errors.As(err, &rpcErr) || rpcErr.Code != codeBlockNotFound {
			return fmt.Errorf("rejecting the block: %w", err)
		}
	}

	say("Giving each node its own next block…")
	if _, err := sf.mine(ctx, 1); err != nil {
		return err
	}

	sfInfo, err = sf.chainInfo(ctx)
	if err != nil {
		return err
	}
	sqInfo, err = sq.chainInfo(ctx)
	if err != nil {
		return err
	}
	if sfInfo.Best == sqInfo.Best {
		return errors.New("the nodes still agree, so the split did not take")
	}

	say("Split. The chains separated at height %d.", sqInfo.Blocks-1)
	say("Both nodes keep answering; they simply no longer agree.")
	return nil
}

// disconnect separates the nodes and keeps them apart.
//
// Both a disconnect and a ban: a disconnect alone is undone within seconds by
// either node's own peer discovery, which would silently heal a split someone was
// in the middle of demonstrating.
func disconnect(ctx context.Context) error {
	all := nodes()
	for i, n := range all {
		c := newClient(n.rpcURL)
		other := all[(i+1)%len(all)]

		if err := c.call(ctx, "disconnectnode", []any{other.p2p}, nil); err != nil {
			var rpcErr *rpcError
			if !errors.As(err, &rpcErr) {
				return err
			}
		}
		// A long ban rather than a permanent one, so a world left running
		// overnight is not quietly unusable the next morning for a reason nobody
		// remembers.
		if err := c.call(ctx, "setban", []any{other.p2p, "add", 86400}, nil); err != nil {
			var rpcErr *rpcError
			if !errors.As(err, &rpcErr) {
				return err
			}
		}
	}
	return nil
}

// commandMine adds blocks to one chain, which is how a split is made to grow.
func commandMine(ctx context.Context, nodeName string, blocks int) error {
	if blocks <= 0 {
		return errors.New("say how many blocks to mine, with -blocks")
	}
	n, err := nodeByName(nodeName)
	if err != nil {
		return err
	}

	c := newClient(n.rpcURL)
	if _, err := c.mine(ctx, blocks); err != nil {
		return err
	}
	info, err := c.chainInfo(ctx)
	if err != nil {
		return err
	}
	say("Mined %d block(s) on %s. It is now at height %d.", blocks, n.name, info.Blocks)
	return nil
}

// commandStatus shows where both chains are, and what Forktower makes of it.
func commandStatus(ctx context.Context, forktowerURL string) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NODE\tHEIGHT\tTIP\tBRANCHES")

	tips := map[string]string{}
	for _, n := range nodes() {
		c := newClient(n.rpcURL)
		info, err := c.chainInfo(ctx)
		if err != nil {
			_, _ = fmt.Fprintf(w, "%s\t-\tnot answering\t-\n", n.name)
			continue
		}
		tips[n.name] = info.Best

		branches, err := c.chainTips(ctx)
		if err != nil {
			branches = nil
		}
		_, _ = fmt.Fprintf(w, "%s\t%d\t%s\t%s\n",
			n.name, info.Blocks, short(info.Best), describeBranches(branches))
	}
	if err := w.Flush(); err != nil {
		return err
	}

	if len(tips) == 2 {
		if tips[nodeSF] == tips[nodeSQ] {
			say("\nThe two nodes agree.")
		} else {
			say("\nThe two nodes are on different chains.")
		}
	}

	return showForktower(ctx, forktowerURL)
}

// describeBranches summarises what a node thinks of the branches it knows about.
// The rejected ones are the interesting part: they are how a node says it has
// seen the other chain and will not have it.
func describeBranches(tips []chainTip) string {
	if len(tips) == 0 {
		return "-"
	}
	rejected := 0
	for _, t := range tips {
		switch t.Status {
		case "invalid", "headers-only", "valid-headers":
			rejected++
		}
	}
	if rejected == 0 {
		return fmt.Sprintf("%d", len(tips))
	}
	return fmt.Sprintf("%d (%d rejected)", len(tips), rejected)
}

// showForktower reports what the daemon makes of the world, when one is running.
func showForktower(ctx context.Context, baseURL string) error {
	if baseURL == "" {
		return nil
	}

	// The address comes from a flag the person running this typed. Asking a
	// development tool to fetch an address its operator chose is what it is for.
	//nolint:gosec // G704: the URL is an operator-supplied flag, by design
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v1/status", http.NoBody)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: see above
	if err != nil {
		say("\nForktower is not answering at %s.", baseURL)
		return nil
	}
	defer func() { _ = resp.Body.Close() }()

	var envelope struct {
		Data struct {
			Headline struct {
				State  string `json:"state"`
				Title  string `json:"title"`
				Detail string `json:"detail"`
			} `json:"headline"`
			Split struct {
				State string `json:"state"`
			} `json:"split"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		say("\nForktower answered with something unreadable.")
		return nil
	}

	say("\nForktower says: %s", envelope.Data.Headline.Title)
	say("  %s", envelope.Data.Headline.Detail)
	say("  (state %s, chains %s)",
		envelope.Data.Headline.State, envelope.Data.Split.State)
	return nil
}

// commandDown removes the world, including its state.
func commandDown(ctx context.Context) error {
	say("Removing the world and everything in it…")
	// **Every profile, not just the enabled ones.** `down` only touches services
	// whose profile is active, so a service behind one survives — container,
	// volume and all — and carries a dead world's state into the next one. The
	// watchtower is an LND with its own chain database, and one that remembers a
	// chain nobody has any more spends the next scenario wedged in a reorg it
	// cannot resolve, reporting "block not found" for a block that no longer
	// exists on either node.
	if err := compose(ctx, "--profile", "*", "down", "-v"); err != nil {
		return err
	}
	say("Gone.")
	return nil
}

func short(hash string) string {
	if len(hash) <= 16 {
		return hash
	}
	return hash[:8] + "…" + hash[len(hash)-8:]
}
