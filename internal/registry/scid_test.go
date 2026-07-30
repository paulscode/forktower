package registry

import "testing"

// The two implementations spell this differently, and the stored form is the
// readable one. Getting the conversion wrong records the same channel two ways
// depending on which node reported it — which is what happened before this
// existed, and what the cross-adapter test caught.
func TestShortChannelIDs(t *testing.T) {
	t.Parallel()

	if got := ShortChannelID(850_000, 1, 0); got != "850000x1x0" {
		t.Errorf("got %q", got)
	}

	// The packed layout is fixed by the protocol: 24 bits of block, 24 of
	// transaction index, 16 of output.
	packed := uint64(850_000)<<40 | uint64(1)<<16
	got, ok := ShortChannelIDFromPacked(packed)
	if !ok || got != "850000x1x0" {
		t.Errorf("got %q (%v), want 850000x1x0", got, ok)
	}

	// A channel that has not confirmed has no short id. Inventing "0x0x0" would
	// read as one that confirmed in the genesis block.
	if _, ok := ShortChannelIDFromPacked(0); ok {
		t.Error("an unconfirmed channel was given a short id")
	}

	block, tx, out, ok := ParseShortChannelID("850000x1x2")
	if !ok || block != 850_000 || tx != 1 || out != 2 {
		t.Errorf("got %d/%d/%d (%v)", block, tx, out, ok)
	}
	for _, bad := range []string{"", "850000x1", "axbxc", "850000x1x2x3", "-1x0x0"} {
		if _, _, _, ok := ParseShortChannelID(bad); ok {
			t.Errorf("%q was accepted as a short channel id", bad)
		}
	}

	if got := BlockFromShortChannelID("850000x1x0"); got != 850_000 {
		t.Errorf("block = %d", got)
	}
	if got := BlockFromShortChannelID("nonsense"); got != 0 {
		t.Errorf("an unusable id gave block %d, want 0", got)
	}

	// Round trip: every spelling one implementation produces must read back as
	// what the other would.
	for _, tc := range []struct{ block, tx, out uint32 }{
		{1, 0, 0}, {850_000, 1, 0}, {1 << 23, 1 << 23, 1<<16 - 1},
	} {
		packed := uint64(tc.block)<<40 | uint64(tc.tx)<<16 | uint64(tc.out)
		spelled, ok := ShortChannelIDFromPacked(packed)
		if !ok {
			t.Fatalf("%v could not be spelled", tc)
		}
		b, x, o, ok := ParseShortChannelID(spelled)
		if !ok || b != tc.block || x != tc.tx || o != tc.out {
			t.Errorf("%v round-tripped as %d/%d/%d via %q", tc, b, x, o, spelled)
		}
	}
}
