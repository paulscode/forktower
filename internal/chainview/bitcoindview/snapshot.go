package bitcoindview

import (
	"context"
	"errors"
	"fmt"

	"github.com/paulscode/forktower/internal/bootstrap"
)

// ChainInfo reports how far this node has got, and whether it has already taken
// the snapshot shortcut.
//
// Implements bootstrap.Node. Two calls rather than one because the chainstate
// question is not answered by `getblockchaininfo`: a node that has loaded a
// snapshot reports the *snapshot* chainstate's height there, which is exactly the
// number that would make the bootstrap decide it had nothing left to do — for the
// right reason, but by reading a field that means something else.
func (v *View) ChainInfo(ctx context.Context) (bootstrap.ChainInfo, error) {
	var info blockchainInfoJSON
	if err := v.c.call(ctx, &info, "getblockchaininfo"); err != nil {
		return bootstrap.ChainInfo{}, mapError(err)
	}

	loaded, err := v.snapshotLoaded(ctx)
	if err != nil {
		return bootstrap.ChainInfo{}, err
	}

	return bootstrap.ChainInfo{
		Network:        info.Chain,
		Blocks:         info.Blocks,
		Headers:        info.Headers,
		SnapshotLoaded: loaded,
	}, nil
}

// chainStatesJSON is the subset of `getchainstates` this package needs.
type chainStatesJSON struct {
	ChainStates []struct {
		// SnapshotBlockHash is present only on a chainstate that came from a
		// snapshot, which is what makes it the thing to look for. Its absence on
		// every entry means the node reached its height the ordinary way.
		SnapshotBlockHash string `json:"snapshot_blockhash"`
	} `json:"chainstates"`
}

// snapshotLoaded reports whether any of the node's chainstates came from a
// snapshot.
//
// A node too old for `getchainstates` answers "no" rather than failing. The call
// arrived in Bitcoin Core 26 and the bundled node is far newer, but the second
// node can be somebody else's — and refusing to run because a perfectly healthy
// node cannot answer an optional question would be the wrong trade.
func (v *View) snapshotLoaded(ctx context.Context) (bool, error) {
	var states chainStatesJSON
	if err := v.c.call(ctx, &states, "getchainstates"); err != nil {
		if e, ok := asRPCError(err); ok && e.Code == codeMethodNotFound {
			return false, nil
		}
		return false, mapError(err)
	}
	for _, cs := range states.ChainStates {
		if cs.SnapshotBlockHash != "" {
			return true, nil
		}
	}
	return false, nil
}

// loadTxOutSetJSON is what `loadtxoutset` returns on success.
type loadTxOutSetJSON struct {
	CoinsLoaded uint64 `json:"coins_loaded"`
	TipHash     string `json:"tip_hash"`
	BaseHeight  int32  `json:"base_height"`
	Path        string `json:"path"`
}

// LoadSnapshot hands the node a UTXO snapshot and waits while it reads it.
//
// Implements bootstrap.Node. Blocks for minutes, which is why it is the one call
// in this package made without a client-side timeout.
//
// Nothing here checks the file. The node recomputes the UTXO set's hash and
// compares it against the value compiled into Bitcoin Core, so a snapshot that is
// wrong in any way — corrupted, truncated, reassembled out of order, or
// deliberately altered — is refused by the node itself. A check here would
// duplicate that at best, and at worst would imply that this program's opinion of
// the file was what made it safe.
func (v *View) LoadSnapshot(ctx context.Context, path string) (bootstrap.Loaded, error) {
	var out loadTxOutSetJSON
	if err := v.c.callLong(ctx, &out, "loadtxoutset", path); err != nil {
		return bootstrap.Loaded{}, snapshotError(err)
	}
	return bootstrap.Loaded{
		Coins:      out.CoinsLoaded,
		BaseHeight: out.BaseHeight,
		TipHash:    out.TipHash,
	}, nil
}

// ErrSnapshotRefused means the node would not accept the file.
//
// Distinguished from a transport failure because the two want opposite responses:
// a node that could not be reached is worth retrying, and a node that read the
// file and rejected it will reject it again just as firmly.
var ErrSnapshotRefused = errors.New("bitcoindview: the node refused the snapshot")

// snapshotError turns a node's refusal into something worth showing a user.
//
// The node's own message is kept — it is specific and usually the only clue —
// but it arrives as a bare sentence about a file path, and on its own it reads
// like the software is broken rather than like a download that needs doing again.
func snapshotError(err error) error {
	e, ok := asRPCError(err)
	if !ok {
		return fmt.Errorf("handing the snapshot to the node: %w", err)
	}
	return fmt.Errorf("%w: %s", ErrSnapshotRefused, e.Message)
}
