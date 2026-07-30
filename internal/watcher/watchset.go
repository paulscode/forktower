package watcher

import (
	"context"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"

	"github.com/paulscode/forktower/internal/chainview"
	"github.com/paulscode/forktower/internal/store"
)

// Kind is what a watched outpoint is.
type Kind string

// The two kinds of thing worth watching.
const (
	// KindFunding is a channel's funding output. Spending it is the event this
	// whole daemon exists to notice.
	KindFunding Kind = "funding"
	// KindCommitmentOutput is an output of a commitment that has already
	// confirmed. Watching these is what lets Forktower report an *outcome* — a
	// justice transaction that landed, or a delay that expired — rather than only
	// a threat.
	KindCommitmentOutput Kind = "commitment_output"
)

// Target is one outpoint being watched, and everything needed to say what
// spending it means.
type Target struct {
	Outpoint wire.OutPoint
	Kind     Kind
	// ChannelID is the channel this belongs to, or zero if the link is not
	// known. Second-order outpoints recorded before their channel was identified
	// carry zero rather than a guess.
	ChannelID int64
	// Role is what a commitment output is for. Empty for funding outputs.
	Role store.OutpointRole
	// SourceSpendEventID is the spend that created this output, which is how a
	// reorg that removes a commitment also removes what it created. Zero for
	// funding outputs, which no spend created.
	SourceSpendEventID int64
	// Script is the outpoint's scriptPubKey, when it is known. Needed only on the
	// light-client tier, where matching is done against a block's compact filter,
	// which commits to scripts rather than to outpoints. Often nil for funding
	// outputs on a pruned node, and nothing is lost by that on a full node.
	Script []byte
}

// Skipped is a row that could not be turned into something watchable.
//
// Carried rather than returned as an error, because one unreadable row must not
// stop every other channel being watched — but it must not vanish either. A
// channel silently missing from the watchset is a channel nobody is looking at,
// which is the failure this whole project is about.
type Skipped struct {
	// What identifies the row: a channel id, or a "txid:vout" for a second-order
	// outpoint.
	What string
	// Why is safe to log and to show.
	Why string
}

// WatchSet is everything that must be matched against a chain's blocks.
//
// Keyed by outpoint because that is how a full node matches: every input of
// every transaction is looked up here, so this must be a map and not a list.
// The set is small — one entry per channel, plus a handful per commitment that
// has confirmed — so holding it in memory is not a decision worth agonising over.
type WatchSet struct {
	targets map[wire.OutPoint]Target
	// Skipped lists the rows that could not be read. Empty in the ordinary case.
	Skipped []Skipped
}

// NewWatchSet builds a set from targets. A later target with the same outpoint
// replaces an earlier one, which is what makes rebuilding idempotent.
func NewWatchSet(targets ...Target) WatchSet {
	ws := WatchSet{targets: make(map[wire.OutPoint]Target, len(targets))}
	for _, t := range targets {
		ws.targets[t.Outpoint] = t
	}
	return ws
}

// Len is how many outpoints are being watched.
func (w WatchSet) Len() int { return len(w.targets) }

// Empty reports whether there is nothing to look for.
func (w WatchSet) Empty() bool { return len(w.targets) == 0 }

// Lookup finds what an outpoint is, if it is watched at all.
func (w WatchSet) Lookup(op wire.OutPoint) (Target, bool) {
	t, ok := w.targets[op]
	return t, ok
}

// Targets returns the watched outpoints in a stable order: funding outputs
// first, then everything else, each group ordered by transaction id and output
// index. Stable because a caller that logs or displays this should not see it
// shuffle between calls for no reason.
func (w WatchSet) Targets() []Target {
	out := make([]Target, 0, len(w.targets))
	for _, t := range w.targets {
		out = append(out, t)
	}
	sortTargets(out)
	return out
}

// ChainViewSet is this set in the form a chain backend understands.
//
// Both forms are filled in because the two kinds of backend match differently:
// a full node is given the outpoints and reads transaction inputs directly,
// while a light client can only test a block's compact filter, which commits to
// scripts. A script we do not have is simply absent, which on a full node costs
// nothing.
func (w WatchSet) ChainViewSet() chainview.WatchSet {
	out := chainview.WatchSet{Outpoints: make(map[wire.OutPoint]struct{}, len(w.targets))}
	for _, t := range w.Targets() {
		out.Outpoints[t.Outpoint] = struct{}{}
		if len(t.Script) > 0 {
			out.Scripts = append(out.Scripts, t.Script)
		}
	}
	return out
}

// Source is the storage this reads. An interface so that a test can make a read
// fail without a broken database.
type Source interface {
	ListChannels(ctx context.Context, f store.ChannelFilter) ([]store.Channel, error)
	ListWatchOutpoints(ctx context.Context, branch store.Branch) ([]store.WatchOutpoint, error)
}

// Build assembles the watchset for one chain from storage.
//
// The safety rule is the whole point of this function: a channel is watched
// unless it has been positively established as not exposed. Both `relevant` and
// `unknown` are in; only an explicit `irrelevant`, which the classifier writes
// with a recorded reason, is left out. Failing toward watching costs a little
// scanning, and failing the other way costs someone their channel.
func Build(ctx context.Context, src Source, branch store.Branch) (WatchSet, error) {
	channels, err := src.ListChannels(ctx, store.ChannelFilter{})
	if err != nil {
		return WatchSet{}, fmt.Errorf("reading your channels to decide what to watch: %w", err)
	}
	outpoints, err := src.ListWatchOutpoints(ctx, branch)
	if err != nil {
		return WatchSet{}, fmt.Errorf("reading what else is being watched: %w", err)
	}

	ws := WatchSet{targets: make(map[wire.OutPoint]Target, len(channels)+len(outpoints))}
	skip := func(what, why string) { ws.Skipped = append(ws.Skipped, Skipped{What: what, Why: why}) }

	for _, c := range channels {
		if c.Relevance == store.Irrelevant {
			continue
		}
		op, opErr := outpointOf(c.FundingTxID, c.FundingVout)
		if opErr != nil {
			skip(fmt.Sprintf("channel %d", c.ID), opErr.Error())
			continue
		}
		ws.targets[op] = Target{
			Outpoint:  op,
			Kind:      KindFunding,
			ChannelID: c.ID,
			Script:    decodeScript(c.FundingScriptHex),
		}
	}

	for _, o := range outpoints {
		op, opErr := outpointOf(o.TxID, o.Vout)
		if opErr != nil {
			skip(fmt.Sprintf("%s:%d", o.TxID, o.Vout), opErr.Error())
			continue
		}
		// A second-order outpoint never displaces a funding output. They cannot
		// collide in practice — a commitment's outputs are not a funding output —
		// but if a bad row ever claimed one, losing the funding entry would be the
		// worst possible outcome of that mistake.
		if existing, taken := ws.targets[op]; taken && existing.Kind == KindFunding {
			skip(fmt.Sprintf("%s:%d", o.TxID, o.Vout),
				"it is already being watched as a channel's funding output")
			continue
		}
		ws.targets[op] = Target{
			Outpoint:           op,
			Kind:               KindCommitmentOutput,
			Role:               o.Role,
			SourceSpendEventID: o.SourceSpendEventID,
			Script:             decodeScript(o.ScriptHex),
		}
	}

	return ws, nil
}

// outpointOf turns a stored transaction id and output index into the form the
// scanner matches on.
//
// Refuses rather than guesses. A transaction id that will not parse is a row we
// cannot watch, and pretending otherwise would put a zero outpoint in the set —
// which every coinbase input in every block would then match.
func outpointOf(txid string, vout int32) (wire.OutPoint, error) {
	if vout < 0 {
		return wire.OutPoint{}, fmt.Errorf("%d is not an output index", vout)
	}
	// Length checked before parsing, because chainhash pads a short string to
	// width rather than refusing it: "11" parses cleanly into a hash of 62 zeroes
	// followed by 0x11. A truncated transaction id would become a different,
	// perfectly valid-looking one, and we would watch an outpoint that does not
	// exist while the real channel went unwatched.
	if len(txid) != chainhash.MaxHashStringSize {
		return wire.OutPoint{}, fmt.Errorf("%q is not a transaction id", txid)
	}
	h, err := chainhash.NewHashFromStr(txid)
	if err != nil {
		return wire.OutPoint{}, fmt.Errorf("%q is not a transaction id", txid)
	}
	if *h == (chainhash.Hash{}) {
		return wire.OutPoint{}, fmt.Errorf("%q is the empty transaction id", txid)
	}
	return wire.OutPoint{Hash: *h, Index: uint32(vout)}, nil
}

// decodeScript reads a stored script, or returns nothing. A script that will not
// decode is treated as absent, which costs nothing on a full node and is caught
// by the readiness check on the tier where it matters.
func decodeScript(hexStr string) []byte {
	if hexStr == "" {
		return nil
	}
	b, err := hex.DecodeString(hexStr)
	if err != nil {
		return nil
	}
	return b
}

// sortTargets orders funding outputs first, then everything else, each group by
// transaction id and output index.
func sortTargets(targets []Target) {
	sort.Slice(targets, func(i, j int) bool {
		a, b := targets[i], targets[j]
		if (a.Kind == KindFunding) != (b.Kind == KindFunding) {
			return a.Kind == KindFunding
		}
		if a.Outpoint.Hash != b.Outpoint.Hash {
			return a.Outpoint.Hash.String() < b.Outpoint.Hash.String()
		}
		return a.Outpoint.Index < b.Outpoint.Index
	})
}
