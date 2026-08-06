package bootstrap

import (
	"strings"
	"testing"
	"time"
)

func TestHumanBytesReadsLikeADiskDialogue(t *testing.T) {
	cases := map[int64]string{
		0:             "0 bytes",
		1:             "1 bytes",
		1023:          "1023 bytes",
		1024:          "1.0 KB",
		1536:          "1.5 KB",
		10 * 1024:     "10 KB",
		1 << 20:       "1.0 MB",
		1 << 30:       "1.0 GB",
		9_387_990_306: "8.7 GB",
		1_992_294_400: "1.9 GB",
		2 << 40:       "2.0 TB",
		// Beyond the biggest unit it keeps counting in terabytes rather than
		// falling off the end of the table.
		9999 << 40: "9999 TB",
	}
	for n, want := range cases {
		if got := HumanBytes(n); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

// The published snapshot's size must read as the figure quoted everywhere else.
// A user comparing the dashboard against the release notes should see one number.
func TestThePublishedSizeReadsAsItIsQuoted(t *testing.T) {
	if got := HumanBytes(MainnetHeight935000.TotalBytes()); got != "8.7 GB" {
		t.Errorf("the snapshot's size renders as %q; the release notes say 8.74 GB", got)
	}
}

func TestCommasGroupNumbersForReading(t *testing.T) {
	cases := map[int64]string{
		0:         "0",
		7:         "7",
		935:       "935",
		1000:      "1,000",
		935_000:   "935,000",
		164241311: "164,241,311",
		-1234:     "-1,234",
		-1:        "-1",
	}
	for n, want := range cases {
		if got := Commas(n); got != want {
			t.Errorf("Commas(%d) = %q, want %q", n, got, want)
		}
	}
}

// Deliberately coarse. This is fed by a throughput estimate over a link whose
// speed varies by an order of magnitude minute to minute, and a figure like
// "3h 52m 14s" claims a precision the underlying number does not have.
func TestHumanDurationIsHonestlyVague(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, ""},
		{-time.Hour, ""},
		{30 * time.Second, "less than a minute"},
		{90 * time.Second, "about a minute"},
		{7 * time.Minute, "about 7 minutes"},
		{59 * time.Minute, "about 59 minutes"},
		{90 * time.Minute, "about an hour"},
		{5 * time.Hour, "about 5 hours"},
		{23 * time.Hour, "about 23 hours"},
		{25 * time.Hour, "about a day"},
		{72 * time.Hour, "about 3 days"},
	}
	for _, c := range cases {
		if got := HumanDuration(c.d); got != c.want {
			t.Errorf("HumanDuration(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}

// An estimate declines to guess until enough has moved to mean something.
//
// A progress bar showing a confident time remaining after two seconds is wrong,
// is known to be wrong when it is written, and costs the reader their trust in
// every later estimate.
func TestETADeclinesToGuessTooEarly(t *testing.T) {
	const total = 9 << 30

	if got := ETA(1<<20, total, time.Second); got != 0 {
		t.Errorf("an estimate was offered after one megabyte: %s", got)
	}
	if got := ETA(0, total, time.Minute); got != 0 {
		t.Errorf("an estimate was offered before anything moved: %s", got)
	}
	if got := ETA(total, total, time.Minute); got != 0 {
		t.Errorf("an estimate was offered for a finished transfer: %s", got)
	}
	if got := ETA(1<<30, total, 0); got != 0 {
		t.Errorf("an estimate was offered with no time elapsed: %s", got)
	}
}

func TestETAScalesWithTheObservedRate(t *testing.T) {
	// A gigabyte in a hundred seconds, with eight to go.
	got := ETA(1<<30, 9<<30, 100*time.Second)
	want := 800 * time.Second
	if got < want-2*time.Second || got > want+2*time.Second {
		t.Errorf("ETA = %s, want about %s", got, want)
	}
}

func TestPercentIsSafeOnAnEmptyTransfer(t *testing.T) {
	if got := (Progress{}).Percent(); got != 0 {
		t.Errorf("Percent on a zero Progress = %v", got)
	}
	if got := (Progress{BytesDone: 50, BytesTotal: 200}).Percent(); got != 25 {
		t.Errorf("Percent = %v, want 25", got)
	}
}

func TestDescribeNamesTheChainAndTheHeight(t *testing.T) {
	got := MainnetHeight935000.Describe()
	for _, want := range []string{"main", "935,000", "8.7 GB"} {
		if !strings.Contains(got, want) {
			t.Errorf("Describe() = %q, which does not mention %q", got, want)
		}
	}
}
