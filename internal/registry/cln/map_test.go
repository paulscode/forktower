package cln

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/paulscode/forktower/internal/store"
)

func loadFixture(t *testing.T) listPeerChannelsJSON {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "listpeerchannels.json"))
	if err != nil {
		t.Fatal(err)
	}
	var out listPeerChannelsJSON
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The same check the LND adapter carries, against the opposite naming
// convention. Core Lightning's schema is explicit: our_to_self_delay is "the
// number of blocks before we can take our funds if we unilateral close", and
// theirs likewise for them. So ours is what we wait, theirs is what they wait —
// and it is theirs that bounds our response to a breach.
//
// The fixture uses different values on each side deliberately. Real channels
// frequently carry the same delay both ways, which would pass whichever way
// round the mapping was written.
func TestTheTwoCsvDelaysAreNotSwapped(t *testing.T) {
	t.Parallel()

	got, err := mapChannel(loadFixture(t).Channels[0])
	if err != nil {
		t.Fatal(err)
	}
	if got.CSVDelayLocal == nil || *got.CSVDelayLocal != 144 {
		t.Errorf("csv_delay_local = %v, want 144 — what we wait after our own close",
			got.CSVDelayLocal)
	}
	if got.CSVDelayRemote == nil || *got.CSVDelayRemote != 720 {
		t.Errorf("csv_delay_remote = %v, want 720 — what the peer waits after theirs",
			got.CSVDelayRemote)
	}
}

func TestAbsentDelaysAreNotZero(t *testing.T) {
	t.Parallel()

	got, err := mapChannel(loadFixture(t).Channels[1])
	if err != nil {
		t.Fatal(err)
	}
	if got.CSVDelayLocal != nil || got.CSVDelayRemote != nil {
		t.Errorf("missing delays became %v/%v, want both absent",
			got.CSVDelayLocal, got.CSVDelayRemote)
	}

	zero := int32(0)
	if got := clampDelay(&zero); got == nil || *got != 0 {
		t.Errorf("a reported zero came back as %v; that is a different fact", got)
	}
	for _, v := range []int32{-1, maxToSelfDelay + 1} {
		delay := v
		if got := clampDelay(&delay); got != nil {
			t.Errorf("a delay of %d came back as %d, want it treated as unknown so the "+
				"conservative floor applies", v, *got)
		}
	}
}

// Core Lightning reports millisatoshis, in two spellings depending on the field
// and the version. A silently-zero capacity would make a channel look like it
// holds nothing.
func TestAmountsInBothSpellings(t *testing.T) {
	t.Parallel()

	got, err := mapChannel(loadFixture(t).Channels[0])
	if err != nil {
		t.Fatal(err)
	}
	if got.CapacitySat != 150_000 {
		t.Errorf("capacity = %d sat, want 150000", got.CapacitySat)
	}

	cases := map[any]int64{
		"1500000msat": 1_500_000,
		"1500000":     1_500_000,
		" 42msat ":    42,
		float64(2500): 2500,
		nil:           0,
		"":            0,
	}
	for in, want := range cases {
		got, err := msatFrom(in, "amount")
		if err != nil {
			t.Errorf("msatFrom(%v): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("msatFrom(%v) = %d, want %d", in, got, want)
		}
	}

	for _, bad := range []any{"one thousand", true, []any{1}} {
		if _, err := msatFrom(bad, "amount"); err == nil {
			t.Errorf("msatFrom(%v) was accepted; a zero amount is worse than an error", bad)
		}
	}

	// Beyond exact representation as a float64: it must be refused rather than
	// rounded, because a rounded balance is a wrong balance.
	if _, err := msatFrom(float64(1<<53+2), "amount"); err == nil {
		t.Error("an amount too large to be exact was accepted")
	}
}

func TestChannelTypes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		names []string
		want  store.ChanType
	}{
		{[]string{"static_remotekey/even", "anchors/even"}, store.ChanAnchors},
		{[]string{"static_remotekey/even"}, store.ChanStaticRemote},
		{[]string{"anchors_zero_fee_htlc_tx/even"}, store.ChanAnchors},
		{[]string{"simple_taproot/even"}, store.ChanTaproot},
		{[]string{"something_new/even"}, store.ChanTypeUnknown},
		{nil, store.ChanTypeUnknown},
	}
	for _, tc := range cases {
		typ := &struct {
			Names []string `json:"names"`
		}{Names: tc.names}
		if got := mapChannelType(typ); got != tc.want {
			t.Errorf("%v mapped to %q, want %q", tc.names, got, tc.want)
		}
	}
	if got := mapChannelType(nil); got != store.ChanTypeUnknown {
		t.Errorf("a channel with no type became %q", got)
	}
}

// A state this build has not heard of is treated as closing, not as open: the
// cost of looking at a channel that turns out to be fine is a wasted scan, and
// the cost of the other mistake is a missed close.
func TestUnknownStatesAreTreatedAsClosing(t *testing.T) {
	t.Parallel()

	open := []string{"CHANNELD_NORMAL", "CHANNELD_AWAITING_LOCKIN", "DUALOPEND_AWAITING_LOCKIN"}
	for _, s := range open {
		if got := mapState(s); got != store.CloseOpen {
			t.Errorf("%s mapped to %q, want open", s, got)
		}
	}
	closing := []string{
		"CHANNELD_SHUTTING_DOWN", "CLOSINGD_SIGEXCHANGE", "CLOSINGD_COMPLETE",
		"AWAITING_UNILATERAL", "FUNDING_SPEND_SEEN", "ONCHAIN",
		"SOMETHING_THE_FUTURE_ADDED", "",
	}
	for _, s := range closing {
		if got := mapState(s); got != store.ClosePending {
			t.Errorf("%s mapped to %q, want it treated as closing", s, got)
		}
	}
}

func TestHTLCDirectionIsFromOurPointOfView(t *testing.T) {
	t.Parallel()

	got, err := mapChannel(loadFixture(t).Channels[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(got.HTLCs) != 2 {
		t.Fatalf("got %d HTLCs, want 2", len(got.HTLCs))
	}
	var in, out *store.HTLCSnapshot
	for i := range got.HTLCs {
		switch got.HTLCs[i].Direction {
		case "incoming":
			in = &got.HTLCs[i]
		case "outgoing":
			out = &got.HTLCs[i]
		}
	}
	if in == nil || out == nil {
		t.Fatalf("got %+v, want one of each direction", got.HTLCs)
	}
	if in.AmountMsat != 1_500_000 || in.CLTVExpiry != 850_200 {
		t.Errorf("incoming = %+v", *in)
	}
	if out.AmountMsat != 2_500_000 {
		t.Errorf("outgoing amount = %d msat", out.AmountMsat)
	}
}

func TestMapChannelRejectsWhatItCannotRead(t *testing.T) {
	t.Parallel()

	bad := loadFixture(t).Channels[0]
	bad.FundingTxID = "too short"
	if _, err := mapChannel(bad); err == nil {
		t.Error("a channel with no usable funding transaction was accepted")
	}

	bad = loadFixture(t).Channels[0]
	bad.TotalMsat = "not an amount"
	if _, err := mapChannel(bad); err == nil {
		t.Error("a channel with an unreadable capacity was accepted")
	}
}
