package lnd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/paulscode/forktower/internal/store"
)

func loadFixture(t *testing.T) listChannelsJSON {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "listchannels.json"))
	if err != nil {
		t.Fatal(err)
	}
	var out listChannelsJSON
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The mapping that matters more than any other in this package.
//
// Verified against lnd's own source, not inferred: rpcserver.go builds
// LocalConstraints from LocalChanCfg, and ChannelConfig.CsvDelay is the delay
// applied to outputs paying the owner of that config. So local is what we wait,
// remote is what the peer waits — and it is the peer's that bounds our response
// to a breach.
//
// The fixture uses different values on each side on purpose. A fixture with 144
// on both, which is what a real channel often has, would pass whichever way
// round the mapping was written.
func TestTheTwoCsvDelaysAreNotSwapped(t *testing.T) {
	t.Parallel()

	got, err := mapChannel(loadFixture(t).Channels[0])
	if err != nil {
		t.Fatal(err)
	}

	if got.CSVDelayLocal == nil || *got.CSVDelayLocal != 144 {
		t.Errorf("csv_delay_local = %v, want 144 — what we wait after our own force-close",
			got.CSVDelayLocal)
	}
	if got.CSVDelayRemote == nil || *got.CSVDelayRemote != 720 {
		t.Errorf("csv_delay_remote = %v, want 720 — what the peer waits after theirs, "+
			"which is the window we have to answer a breach", got.CSVDelayRemote)
	}
}

// A channel whose constraints LND did not report must not come back as zero: a
// zero delay produces a deadline that has already passed, an absent one produces
// a deadline from a conservative floor.
func TestAbsentConstraintsAreNotZero(t *testing.T) {
	t.Parallel()

	got, err := mapChannel(loadFixture(t).Channels[1])
	if err != nil {
		t.Fatal(err)
	}
	if got.CSVDelayLocal != nil || got.CSVDelayRemote != nil {
		t.Errorf("missing constraints became %v/%v, want both absent",
			got.CSVDelayLocal, got.CSVDelayRemote)
	}

	// And a reported zero stays zero, because that is a different fact.
	zero := uint32(0)
	withZero := loadFixture(t).Channels[0]
	withZero.RemoteConstraints = &constraintsJSON{CSVDelay: &zero}
	got, err = mapChannel(withZero)
	if err != nil {
		t.Fatal(err)
	}
	if got.CSVDelayRemote == nil || *got.CSVDelayRemote != 0 {
		t.Errorf("a reported zero came back as %v", got.CSVDelayRemote)
	}
}

func TestCommitmentTypes(t *testing.T) {
	t.Parallel()

	cases := map[string]store.ChanType{
		"LEGACY":                   store.ChanLegacy,
		"STATIC_REMOTE_KEY":        store.ChanStaticRemote,
		"ANCHORS":                  store.ChanAnchors,
		"ANCHORS_ZERO_FEE_HTLC_TX": store.ChanAnchors,
		"SIMPLE_TAPROOT":           store.ChanTaproot,
		"SIMPLE_TAPROOT_OVERLAY":   store.ChanTaproot,
		"anchors":                  store.ChanAnchors,
		"  ANCHORS  ":              store.ChanAnchors,
		"":                         store.ChanTypeUnknown,
		// A type this build has not heard of. It still needs watching, so it is
		// recorded as unknown rather than refused — refusing would leave the user
		// unprotected on exactly the channel that is unusual.
		"SOMETHING_NEW": store.ChanTypeUnknown,
	}
	for in, want := range cases {
		if got := mapCommitmentType(in); got != want {
			t.Errorf("mapCommitmentType(%q) = %q, want %q", in, got, want)
		}
	}
	for _, ct := range cases {
		if !ct.Valid() {
			t.Errorf("%q is not a type the store accepts", ct)
		}
	}
}

// Numbers arrive as strings, because that is protobuf's JSON mapping. A
// silently-zero capacity would be worse than a parse error.
func TestNumbersArriveAsStrings(t *testing.T) {
	t.Parallel()

	chans := loadFixture(t).Channels
	got, err := mapChannel(chans[0])
	if err != nil {
		t.Fatal(err)
	}
	if got.CapacitySat != 150_000 {
		t.Errorf("capacity = %d, want 150000", got.CapacitySat)
	}

	// Beyond what a float64 can hold exactly, which is what decoding these into
	// a number type would have done.
	big, err := mapChannel(chans[1])
	if err != nil {
		t.Fatal(err)
	}
	if big.CapacitySat != 9_007_199_254_740_993 {
		t.Errorf("capacity = %d, want it exact", big.CapacitySat)
	}

	bad := chans[0]
	bad.Capacity = "one hundred and fifty thousand"
	if _, err := mapChannel(bad); err == nil {
		t.Error("an unreadable capacity was accepted; a zero one is worse than an error")
	}
}

func TestChannelPoints(t *testing.T) {
	t.Parallel()

	const txid = "8f2c4a1b9e5d3c7f0a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f6071"
	gotTxID, gotVout, err := splitChannelPoint(txid + ":3")
	if err != nil {
		t.Fatal(err)
	}
	if gotTxID != txid || gotVout != 3 {
		t.Errorf("got %s:%d", gotTxID, gotVout)
	}

	for _, bad := range []string{
		"", ":", "abc:0", txid, txid + ":", txid + ":-1", txid + ":x",
		"nothex" + txid[6:] + ":0",
	} {
		if _, _, err := splitChannelPoint(bad); err == nil {
			t.Errorf("%q was accepted as a channel point", bad)
		}
	}
}

// The funding height comes free from the short channel id. Worth taking: the
// alternative is asking the chain for a transaction older than a pruned node
// keeps, which on the hardware this ships to often fails.
func TestFundingHeightFromTheShortChannelId(t *testing.T) {
	t.Parallel()

	// 850000 << 40 | 1 << 16 | 0
	scid := uint64(850_000)<<40 | uint64(1)<<16
	if got := heightFromSCID(strconv.FormatUint(scid, 10)); got != 850_000 {
		t.Errorf("height = %d, want 850000", got)
	}

	// No usable id is zero, which is the honest answer for a channel that has not
	// confirmed — not a guess.
	for _, in := range []string{"", "0", "not a number"} {
		if got := heightFromSCID(in); got != 0 {
			t.Errorf("heightFromSCID(%q) = %d, want 0", in, got)
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

	var incoming, outgoing *store.HTLCSnapshot
	for i := range got.HTLCs {
		switch got.HTLCs[i].Direction {
		case "incoming":
			incoming = &got.HTLCs[i]
		case "outgoing":
			outgoing = &got.HTLCs[i]
		}
	}
	if incoming == nil || outgoing == nil {
		t.Fatalf("got %+v, want one of each direction", got.HTLCs)
	}
	if incoming.AmountMsat != 1_500_000 {
		t.Errorf("incoming amount = %d msat, want 1500000", incoming.AmountMsat)
	}
	if incoming.CLTVExpiry != 850_200 {
		t.Errorf("incoming expiry = %d", incoming.CLTVExpiry)
	}
	if incoming.PaymentHash == "" {
		t.Error("the payment hash was dropped")
	}

	// Falling back to the satoshi figure when the millisatoshi one is absent: a
	// rounded amount is worth more than none.
	h := htlcJSON{Incoming: true, AmountMsat: "1500", ExpirationHeight: 1}
	snap, err := mapHTLC(h)
	if err != nil {
		t.Fatal(err)
	}
	if snap.AmountMsat != 1_500_000 {
		t.Errorf("amount = %d msat from a satoshi figure, want 1500000", snap.AmountMsat)
	}
}

// A delay the protocol cannot express is not a delay we know. Converting it
// would wrap negative, putting the deadline before the height the spend
// confirmed at — which reads as already passed and silences the countdown.
func TestAnImpossibleCsvDelayIsTreatedAsUnknown(t *testing.T) {
	t.Parallel()

	for _, v := range []uint32{maxToSelfDelay + 1, 1 << 31, ^uint32(0)} {
		delay := v
		if got := csvFrom(&constraintsJSON{CSVDelay: &delay}); got != nil {
			t.Errorf("a delay of %d came back as %d, want it treated as unknown so the "+
				"conservative floor applies", v, *got)
		}
	}

	// The largest the protocol can express is still a real delay.
	largest := uint32(maxToSelfDelay)
	if got := csvFrom(&constraintsJSON{CSVDelay: &largest}); got == nil || *got != maxToSelfDelay {
		t.Errorf("the largest expressible delay was rejected: %v", got)
	}
}
