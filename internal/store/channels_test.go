package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func seedNode(t *testing.T, s *Store) string {
	t.Helper()
	const pubkey = "02aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	if err := s.UpsertLNNode(context.Background(), LNNode{
		ID: pubkey, Impl: ImplLND, Alias: "my node", LastSeenAt: 1_790_000_000,
	}); err != nil {
		t.Fatal(err)
	}
	return pubkey
}

func sampleChannel(node string) Channel {
	local, remote := int32(144), int32(288)
	return Channel{
		LNNodeID:         node,
		FundingTxID:      "aa" + strings.Repeat("0", 62),
		FundingVout:      0,
		FundingScriptHex: "0020" + strings.Repeat("cc", 32),
		CapacitySat:      2_100_000,
		ChanType:         ChanAnchors,
		CSVDelayLocal:    &local,
		CSVDelayRemote:   &remote,
		PeerPubkey:       "03" + strings.Repeat("ab", 32),
		PeerAlias:        "ACINQ",
		OpenHeight:       850_000,
		SCID:             "850000x1x0",
		UpdatedAt:        1_790_000_000,
	}
}

// The `changed` flag is what stops the registry announcing every channel on
// every poll. With a sixty-second cycle, emitting unconditionally would bury a
// real change in a stream of identical events.
func TestUpsertChannelReportsWhetherAnythingChanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)
	node := seedNode(t, s)

	c := sampleChannel(node)
	id, changed, err := s.UpsertChannel(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("a channel seen for the first time was reported as unchanged")
	}

	// The same channel again: one row, and nothing to announce.
	again, changed, err := s.UpsertChannel(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if again != id {
		t.Errorf("the same channel got a second row: %d then %d", id, again)
	}
	if changed {
		t.Error("an unchanged channel was reported as changed, which would announce it every poll")
	}

	// Something that actually moved.
	c.CapacitySat = 2_200_000
	_, changed, err = s.UpsertChannel(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("a changed capacity was not reported as a change")
	}
}

// A missing CSV delay is a different fact from a delay of zero: the first
// produces a deadline from a conservative floor, the second would produce one
// that has already passed.
func TestAMissingCsvDelayIsNotZero(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)
	node := seedNode(t, s)

	c := sampleChannel(node)
	c.CSVDelayLocal = nil
	c.CSVDelayRemote = nil
	if _, _, err := s.UpsertChannel(ctx, c); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListChannels(ctx, ChannelFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d channels", len(got))
	}
	if got[0].CSVDelayLocal != nil || got[0].CSVDelayRemote != nil {
		t.Errorf("an unknown delay came back as %v/%v, want both absent",
			got[0].CSVDelayLocal, got[0].CSVDelayRemote)
	}

	zero := int32(0)
	c.CSVDelayRemote = &zero
	if _, _, err := s.UpsertChannel(ctx, c); err != nil {
		t.Fatal(err)
	}
	got, err = s.ListChannels(ctx, ChannelFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].CSVDelayRemote == nil {
		t.Error("a delay of zero came back as unknown; they are different facts")
	}
}

// The peer's alias is 32 bytes chosen by the counterparty, who is the adversary
// in this threat model. Clamped once on the way in, because there is one way in
// and several ways out, and the one that gets forgotten is always an output.
func TestAPeerAliasIsClampedOnTheWayIn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)
	node := seedNode(t, s)

	// Each case needs its own channel. Sharing one funding outpoint made them
	// overwrite each other, which went unnoticed only because they happened to
	// run in order.
	cases := map[string]struct {
		txid  string
		alias string
		check func(t *testing.T, got string)
	}{
		"control characters removed": {
			txid:  "b1" + strings.Repeat("0", 62),
			alias: "ACINQ\x00\x1b[31m\x07",
			check: func(t *testing.T, got string) {
				t.Helper()
				if strings.ContainsAny(got, "\x00\x1b\x07") {
					t.Errorf("control characters survived: %q", got)
				}
			},
		},
		"markup is not special, but is not markup either": {
			txid:  "b2" + strings.Repeat("0", 62),
			alias: `<img src=x onerror=alert(1)>`,
			check: func(t *testing.T, got string) {
				t.Helper()
				// Stored as written — escaping belongs at the point of rendering,
				// and the UI uses textContent. What matters here is the length.
				if len(got) > MaxPeerAliasBytes {
					t.Errorf("%d bytes stored, want at most %d", len(got), MaxPeerAliasBytes)
				}
			},
		},
		"over-long is cut": {
			txid:  "b3" + strings.Repeat("0", 62),
			alias: strings.Repeat("A", 200),
			check: func(t *testing.T, got string) {
				t.Helper()
				if len(got) != MaxPeerAliasBytes {
					t.Errorf("%d bytes stored, want %d", len(got), MaxPeerAliasBytes)
				}
			},
		},
		"cut on a rune boundary": {
			txid:  "b4" + strings.Repeat("0", 62),
			alias: strings.Repeat("é", 40),
			check: func(t *testing.T, got string) {
				t.Helper()
				if len(got) > MaxPeerAliasBytes {
					t.Errorf("%d bytes stored, want at most %d", len(got), MaxPeerAliasBytes)
				}
				for _, r := range got {
					if r == '�' {
						t.Errorf("a half-written rune was stored: %q", got)
					}
				}
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := sampleChannel(node)
			c.FundingTxID = tc.txid
			c.PeerAlias = tc.alias
			if _, _, err := s.UpsertChannel(ctx, c); err != nil {
				t.Fatal(err)
			}
			got, err := s.ListChannels(ctx, ChannelFilter{})
			if err != nil {
				t.Fatal(err)
			}
			for _, ch := range got {
				if ch.FundingTxID == c.FundingTxID {
					tc.check(t, ch.PeerAlias)
					return
				}
			}
			t.Fatal("the channel was not stored")
		})
	}
}

// The close state and the relevance come from the chain and the classifier, not
// from the Lightning node. A poll must not be able to undo either.
func TestAPollDoesNotUndoWhatTheChainSaid(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)
	node := seedNode(t, s)

	c := sampleChannel(node)
	id, _, err := s.UpsertChannel(ctx, c)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.SetChannelCloseSF(ctx, id, CloseForce,
		"cc"+strings.Repeat("0", 62), 850_100, 1_790_000_100); err != nil {
		t.Fatal(err)
	}
	if err := s.SetChannelRelevance(ctx, id, Relevant, "open at the fork point", 1_790_000_100); err != nil {
		t.Fatal(err)
	}

	// A later poll, carrying the Lightning node's own idea of the channel.
	c.CapacitySat = 2_300_000
	if _, _, err := s.UpsertChannel(ctx, c); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListChannels(ctx, ChannelFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].CloseState != CloseForce {
		t.Errorf("close state = %q after a poll, want it preserved", got[0].CloseState)
	}
	if got[0].Relevance != Relevant {
		t.Errorf("relevance = %q after a poll, want it preserved", got[0].Relevance)
	}
	if got[0].CapacitySat != 2_300_000 {
		t.Error("the poll's own data was not applied")
	}
}

func TestListChannelsFilters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)
	node := seedNode(t, s)

	for i, r := range []Relevance{Relevant, Irrelevant, RelevanceUnknown} {
		c := sampleChannel(node)
		c.FundingTxID = string(rune('a'+i)) + strings.Repeat("0", 63)
		id, _, err := s.UpsertChannel(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetChannelRelevance(ctx, id, r, "", 1); err != nil {
			t.Fatal(err)
		}
		if r == Irrelevant {
			if err := s.SetChannelCloseSF(ctx, id, CloseCoop, "dd", 1, 1); err != nil {
				t.Fatal(err)
			}
		}
	}

	all, err := s.ListChannels(ctx, ChannelFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d channels, want 3", len(all))
	}

	relevant, err := s.ListChannels(ctx, ChannelFilter{Relevance: Relevant})
	if err != nil {
		t.Fatal(err)
	}
	if len(relevant) != 1 {
		t.Errorf("got %d relevant channels, want 1", len(relevant))
	}

	open, err := s.ListChannels(ctx, ChannelFilter{OpenOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 2 {
		t.Errorf("got %d open channels, want 2", len(open))
	}

	none, err := s.ListChannels(ctx, ChannelFilter{LNNodeID: "somebody else"})
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("got %d channels for an unknown node", len(none))
	}
}

// In-flight HTLCs are a live picture, not a history: one that has settled must
// stop producing a deadline.
func TestHTLCSnapshotsAreReplacedNotAccumulated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)
	node := seedNode(t, s)

	id, _, err := s.UpsertChannel(ctx, sampleChannel(node))
	if err != nil {
		t.Fatal(err)
	}

	first := []HTLCSnapshot{
		{Direction: "incoming", AmountMsat: 1000, CLTVExpiry: 850_200, PaymentHash: "aa"},
		{Direction: "outgoing", AmountMsat: 2000, CLTVExpiry: 850_150, PaymentHash: "bb"},
	}
	if err := s.ReplaceHTLCSnapshot(ctx, id, 100, first); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListHTLCs(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d HTLCs, want 2", len(got))
	}
	// Soonest expiry first: that is the one that matters.
	if got[0].CLTVExpiry > got[1].CLTVExpiry {
		t.Error("HTLCs are not ordered by expiry")
	}

	// One settles.
	if err := s.ReplaceHTLCSnapshot(ctx, id, 200, first[:1]); err != nil {
		t.Fatal(err)
	}
	got, err = s.ListHTLCs(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("got %d HTLCs after one settled, want 1 — a settled HTLC must stop "+
			"producing a deadline", len(got))
	}

	// All of them settle.
	if err := s.ReplaceHTLCSnapshot(ctx, id, 300, nil); err != nil {
		t.Fatal(err)
	}
	got, err = s.ListHTLCs(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %d HTLCs after all settled", len(got))
	}
}

func TestChannelWritesRejectWhatTheSchemaWouldNotAccept(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemp(t)
	node := seedNode(t, s)

	bad := map[string]Channel{
		"no funding transaction": {LNNodeID: node, ChanType: ChanAnchors},
		"no node":                {FundingTxID: "aa", ChanType: ChanAnchors},
		"unknown channel type":   {LNNodeID: node, FundingTxID: "aa", ChanType: "something-new"},
	}
	for name, c := range bad {
		if _, _, err := s.UpsertChannel(ctx, c); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}

	if err := s.UpsertLNNode(ctx, LNNode{ID: "x", Impl: "eclair"}); err == nil {
		t.Error("an implementation this build cannot read was accepted")
	}
	if err := s.UpsertLNNode(ctx, LNNode{Impl: ImplLND}); err == nil {
		t.Error("a node with no pubkey was accepted")
	}

	// Updating something that is not there is an error, not a silent no-op: a
	// caller that believes it recorded something it did not is worse off than one
	// that fails.
	if err := s.SetChannelRelevance(ctx, 9999, Relevant, "", 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
	if err := s.SetChannelCloseSF(ctx, 9999, CloseCoop, "", 1, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}
