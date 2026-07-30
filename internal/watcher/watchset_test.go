package watcher

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paulscode/forktower/internal/store"
)

const (
	fundingA = "1111111111111111111111111111111111111111111111111111111111111111"
	fundingB = "2222222222222222222222222222222222222222222222222222222222222222"
	fundingC = "3333333333333333333333333333333333333333333333333333333333333333"
	commitTX = "4444444444444444444444444444444444444444444444444444444444444444"
)

// The real store, because the thing being checked is a translation from stored
// rows to matchable outpoints, and a fake would only prove that this test agrees
// with itself.
func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "forktower.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// addSpend records the spend a second-order outpoint hangs off. Every watched
// commitment output has one by design: it is how a reorg that removes the
// commitment also removes what it created.
func addSpend(t *testing.T, st *store.Store, branch store.Branch) int64 {
	t.Helper()
	id, _, err := st.RecordSpend(context.Background(), store.Spend{
		Branch: branch, OutpointTxID: fundingA, OutpointVout: 1,
		SpendTxID: commitTX, SpendTxHex: "00", Status: store.SpendConfirmed,
		FirstSeenAt: 1, UpdatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func addChannel(t *testing.T, st *store.Store, txid string, rel store.Relevance) int64 {
	t.Helper()
	ctx := context.Background()
	if err := st.UpsertLNNode(ctx, store.LNNode{
		ID: "02node", Impl: store.ImplLND, LastSeenAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	id, _, err := st.UpsertChannel(ctx, store.Channel{
		LNNodeID: "02node", FundingTxID: txid, FundingVout: 1,
		CapacitySat: 1000, ChanType: store.ChanAnchors, UpdatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetChannelRelevance(ctx, id, rel, "because the test said so", 1); err != nil {
		t.Fatal(err)
	}
	return id
}

// The safety rule, and the reason this function exists at all: a channel is
// watched unless it has been positively established as not exposed. Not
// knowing is an instruction to keep looking, because a channel we could not
// classify is exactly the one an attacker would choose.
func TestUnknownChannelsAreWatchedAndOnlyIrrelevantOnesAreNot(t *testing.T) {
	t.Parallel()
	st := openStore(t)

	relevant := addChannel(t, st, fundingA, store.Relevant)
	unknown := addChannel(t, st, fundingB, store.RelevanceUnknown)
	addChannel(t, st, fundingC, store.Irrelevant)

	ws, err := Build(context.Background(), st, store.BranchSQ)
	if err != nil {
		t.Fatal(err)
	}
	if ws.Len() != 2 {
		t.Fatalf("watching %d outpoints, want 2: %+v", ws.Len(), ws.Targets())
	}

	watched := map[int64]bool{}
	for _, target := range ws.Targets() {
		if target.Kind != KindFunding {
			t.Errorf("a channel came back as %q", target.Kind)
		}
		watched[target.ChannelID] = true
	}
	if !watched[relevant] {
		t.Error("a channel known to be exposed is not being watched")
	}
	if !watched[unknown] {
		t.Error("a channel nobody could classify is not being watched")
	}
	if len(ws.Skipped) != 0 {
		t.Errorf("rows were skipped: %+v", ws.Skipped)
	}
}

// A channel's funding outpoint is watched whether the channel is open or closed
// on the user's own chain. That is the exposure people do not expect: a close on
// one chain has not happened on the other, so the old commitments there are
// still spendable.
func TestAChannelClosedOnTheUsersChainIsStillWatched(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	ctx := context.Background()

	id := addChannel(t, st, fundingA, store.Relevant)
	if err := st.SetChannelCloseSF(ctx, id, store.CloseCoop, "closetxid", 900, 1); err != nil {
		t.Fatal(err)
	}

	ws, err := Build(ctx, st, store.BranchSQ)
	if err != nil {
		t.Fatal(err)
	}
	if ws.Len() != 1 {
		t.Errorf("a closed channel dropped out of the watchset")
	}
}

func TestSecondOrderOutpointsAreWatchedToo(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	ctx := context.Background()

	addChannel(t, st, fundingA, store.Relevant)
	if err := st.AddWatchOutpoint(ctx, store.WatchOutpoint{
		Branch: store.BranchSQ, TxID: commitTX, Vout: 0, ScriptHex: "0020aabb",
		SourceSpendEventID: addSpend(t, st, store.BranchSQ),
		Role:               store.RoleToLocal,
	}); err != nil {
		t.Fatal(err)
	}

	ws, err := Build(ctx, st, store.BranchSQ)
	if err != nil {
		t.Fatal(err)
	}
	if ws.Len() != 2 {
		t.Fatalf("watching %d outpoints, want 2", ws.Len())
	}

	targets := ws.Targets()
	// Funding outputs first, which is the order a person reading a list of what
	// is being watched would expect.
	if targets[0].Kind != KindFunding {
		t.Errorf("the funding output is not first: %+v", targets)
	}
	second := targets[1]
	if second.Kind != KindCommitmentOutput || second.Role != store.RoleToLocal {
		t.Errorf("the commitment output came back as %+v", second)
	}
	if string(second.Script) != "\x00\x20\xaa\xbb" {
		t.Errorf("the script decoded to %x", second.Script)
	}
}

// Outpoints recorded for the other chain belong to the other chain's scan.
// Watching them here would report a spend on the wrong branch, which is worse
// than not watching at all.
func TestOnlyTheRequestedBranchesOutpointsAreIncluded(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	ctx := context.Background()

	for _, branch := range []store.Branch{store.BranchSF, store.BranchSQ} {
		if err := st.AddWatchOutpoint(ctx, store.WatchOutpoint{
			Branch: branch, TxID: commitTX, Vout: 0, ScriptHex: "51",
			SourceSpendEventID: addSpend(t, st, branch),
			Role:               store.RoleToLocal,
		}); err != nil {
			t.Fatal(err)
		}
	}

	ws, err := Build(ctx, st, store.BranchSQ)
	if err != nil {
		t.Fatal(err)
	}
	if ws.Len() != 1 {
		t.Errorf("watching %d outpoints, want only this branch's one", ws.Len())
	}
}

// A funding outpoint is what everything else hangs off. If a second-order row
// ever claimed the same outpoint, dropping the funding entry would be the worst
// outcome of that mistake, so it is the second-order row that gives way — and it
// is named, not dropped in silence.
func TestASecondOrderRowNeverDisplacesAFundingOutput(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	ctx := context.Background()

	id := addChannel(t, st, fundingA, store.Relevant)
	if err := st.AddWatchOutpoint(ctx, store.WatchOutpoint{
		Branch: store.BranchSQ, TxID: fundingA, Vout: 1, ScriptHex: "51",
		SourceSpendEventID: addSpend(t, st, store.BranchSQ),
		Role:               store.RoleToLocal,
	}); err != nil {
		t.Fatal(err)
	}

	ws, err := Build(ctx, st, store.BranchSQ)
	if err != nil {
		t.Fatal(err)
	}
	if ws.Len() != 1 {
		t.Fatalf("watching %d outpoints, want 1", ws.Len())
	}
	if got := ws.Targets()[0]; got.Kind != KindFunding || got.ChannelID != id {
		t.Errorf("the funding output was displaced: %+v", got)
	}
	if len(ws.Skipped) != 1 {
		t.Fatalf("the collision was not reported: %+v", ws.Skipped)
	}
	if !strings.Contains(ws.Skipped[0].Why, "funding") {
		t.Errorf("the explanation does not say why: %q", ws.Skipped[0].Why)
	}
}

// One unreadable row must not stop every other channel being watched — and must
// not vanish either. A channel silently missing from the watchset is a channel
// nobody is looking at.
func TestARowThatCannotBeReadIsNamedNotFatal(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	ctx := context.Background()

	good := addChannel(t, st, fundingA, store.Relevant)

	// A transaction id that will not parse. It cannot be written through
	// UpsertChannel's own path in the ordinary course of events, which is exactly
	// why the reader has to cope with finding one.
	if _, _, err := st.UpsertChannel(ctx, store.Channel{
		LNNodeID: "02node", FundingTxID: "not a transaction id", FundingVout: 0,
		CapacitySat: 1, ChanType: store.ChanAnchors, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	ws, err := Build(ctx, st, store.BranchSQ)
	if err != nil {
		t.Fatalf("one bad row failed the whole watchset: %v", err)
	}
	if ws.Len() != 1 || ws.Targets()[0].ChannelID != good {
		t.Errorf("the good channel is not being watched: %+v", ws.Targets())
	}
	if len(ws.Skipped) != 1 {
		t.Fatalf("the unreadable row was dropped in silence: %+v", ws.Skipped)
	}
	if !strings.Contains(ws.Skipped[0].What, "channel") {
		t.Errorf("the skipped row is not identified: %q", ws.Skipped[0].What)
	}
}

// The empty transaction id is what a coinbase input names. Letting one into the
// set would make every block ever scanned look like a spend of a channel.
func TestTheEmptyTransactionIdIsRefused(t *testing.T) {
	t.Parallel()

	for _, txid := range []string{
		"0000000000000000000000000000000000000000000000000000000000000000",
		"",
		"zzzz",
		"11",
	} {
		if _, err := outpointOf(txid, 0); err == nil {
			t.Errorf("%q was accepted as a transaction id", txid)
		}
	}
	if _, err := outpointOf(fundingA, -1); err == nil {
		t.Error("a negative output index was accepted")
	}
	if _, err := outpointOf(fundingA, 0); err != nil {
		t.Errorf("a usable outpoint was refused: %v", err)
	}
}

// failing is a store that cannot be read, which is what a database going away
// under a running daemon looks like.
type failing struct {
	channels  error
	outpoints error
}

func (f failing) ListChannels(context.Context, store.ChannelFilter) ([]store.Channel, error) {
	return nil, f.channels
}

func (f failing) ListWatchOutpoints(context.Context, store.Branch) ([]store.WatchOutpoint, error) {
	return nil, f.outpoints
}

// A watchset that could not be read must be an error, not an empty set. An empty
// set scans clean, and a scan that finds nothing looks exactly like a scan that
// had nothing to find.
func TestAnUnreadableStoreIsAnErrorNotAnEmptySet(t *testing.T) {
	t.Parallel()

	boom := errors.New("database is closed")
	for _, src := range []Source{
		failing{channels: boom},
		failing{outpoints: boom},
	} {
		ws, err := Build(context.Background(), src, store.BranchSQ)
		if err == nil {
			t.Fatal("a failed read produced a watchset")
		}
		if !ws.Empty() {
			t.Error("a failed read produced targets")
		}
		if !errors.Is(err, boom) {
			t.Errorf("the underlying failure was lost: %v", err)
		}
	}
}

func TestTheChainBackendFormCarriesBothOutpointsAndScripts(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	ctx := context.Background()

	addChannel(t, st, fundingA, store.Relevant) // no funding script known
	if err := st.AddWatchOutpoint(ctx, store.WatchOutpoint{
		Branch: store.BranchSQ, TxID: commitTX, Vout: 0, ScriptHex: "0020aabb",
		SourceSpendEventID: addSpend(t, st, store.BranchSQ),
		Role:               store.RoleToLocal,
	}); err != nil {
		t.Fatal(err)
	}

	ws, err := Build(ctx, st, store.BranchSQ)
	if err != nil {
		t.Fatal(err)
	}
	got := ws.ChainViewSet()
	if len(got.Outpoints) != 2 {
		t.Errorf("passed %d outpoints to the backend, want 2", len(got.Outpoints))
	}
	// A funding script nobody could find is simply absent, which costs nothing on
	// a full node — it matches outpoints, not scripts.
	if len(got.Scripts) != 1 {
		t.Errorf("passed %d scripts, want the one that is known", len(got.Scripts))
	}
	if got.Empty() {
		t.Error("a non-empty watchset was passed to the backend as empty")
	}
}

func TestAnEmptyStoreProducesAnEmptySet(t *testing.T) {
	t.Parallel()
	st := openStore(t)

	ws, err := Build(context.Background(), st, store.BranchSQ)
	if err != nil {
		t.Fatal(err)
	}
	if !ws.Empty() || ws.Len() != 0 {
		t.Errorf("an empty database produced %d targets", ws.Len())
	}
	if _, found := ws.Lookup(outpoint(t, 0x11, 0)); found {
		t.Error("an empty set claimed to be watching something")
	}
	if got := ws.ChainViewSet(); !got.Empty() {
		t.Error("an empty set was not empty to the backend")
	}
}

// Rebuilding must give the same answer, so the live loop can rebuild whenever
// the registry says a channel changed without worrying about drift.
func TestRebuildingIsIdempotent(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	ctx := context.Background()

	addChannel(t, st, fundingA, store.Relevant)
	addChannel(t, st, fundingB, store.RelevanceUnknown)

	first, err := Build(ctx, st, store.BranchSQ)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Build(ctx, st, store.BranchSQ)
	if err != nil {
		t.Fatal(err)
	}
	if first.Len() != second.Len() {
		t.Fatalf("two builds gave %d and %d targets", first.Len(), second.Len())
	}
	for i, target := range first.Targets() {
		if second.Targets()[i].Outpoint != target.Outpoint {
			t.Errorf("target %d differs between builds", i)
		}
	}

	// And a duplicate target replaces rather than duplicates.
	dup := NewWatchSet(
		funding(outpoint(t, 0x11, 0), 1),
		funding(outpoint(t, 0x11, 0), 2),
	)
	if dup.Len() != 1 {
		t.Errorf("the same outpoint was watched twice")
	}
	if got, _ := dup.Lookup(outpoint(t, 0x11, 0)); got.ChannelID != 2 {
		t.Errorf("the later target did not win: channel %d", got.ChannelID)
	}
}
