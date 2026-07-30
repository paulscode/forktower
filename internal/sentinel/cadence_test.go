package sentinel

import (
	"testing"
	"time"
)

func TestCadenceStartsAtTheNominalInterval(t *testing.T) {
	t.Parallel()

	c := NewCadence()
	if c.Measured() {
		t.Error("a fresh estimate claims to have measured something")
	}
	if c.ExpectedInterval() != nominalInterval {
		t.Errorf("expected interval = %v, want the nominal %v", c.ExpectedInterval(), nominalInterval)
	}
}

func TestCadenceNeedsTwoBlocksToMeasureAnything(t *testing.T) {
	t.Parallel()

	c := NewCadence().Observe(1000)
	if c.Measured() {
		t.Error("one block is not an interval")
	}
	c = c.Observe(1000 + 600)
	if !c.Measured() {
		t.Error("two blocks should give an interval")
	}
}

// A chain that has genuinely slowed must be followed, since that is the whole
// point during a split: a countdown in blocks means a very different amount of
// human time on a chain producing one block an hour.
func TestCadenceFollowsAChainThatSlowsDown(t *testing.T) {
	t.Parallel()

	c := NewCadence()
	at := int64(0)
	// Twenty blocks two hours apart.
	for range 20 {
		at += 7200
		c = c.Observe(at)
	}
	got := c.ExpectedInterval()
	if got < 90*time.Minute {
		t.Errorf("expected interval = %v; the estimate did not follow a chain producing "+
			"blocks two hours apart", got)
	}
}

// The floor matters: without it a burst of quick blocks would make the stall
// threshold hair-trigger, and a merely lively chain would keep being reported as
// stalled — crying wolf about the thing the user most needs to believe.
func TestCadenceNeverDropsBelowTheNominalInterval(t *testing.T) {
	t.Parallel()

	c := NewCadence()
	at := int64(0)
	for range 50 {
		at += 10 // ten seconds apart
		c = c.Observe(at)
	}
	if got := c.ExpectedInterval(); got < nominalInterval {
		t.Errorf("expected interval = %v, want never below the nominal %v", got, nominalInterval)
	}
}

// Header timestamps are the miner's claim, not a measurement, and are only loosely
// constrained. One absurd value must not poison the estimate for hours.
func TestCadenceIgnoresBackwardsAndAbsurdTimestamps(t *testing.T) {
	t.Parallel()

	c := NewCadence().Observe(10_000).Observe(10_600)
	before := c.IntervalSecs

	backwards := c.Observe(9_000)
	if backwards.Samples != c.Samples {
		t.Error("a timestamp older than the previous block was counted as an interval")
	}

	absurd := c.Observe(10_600 + int64(365*24*time.Hour/time.Second))
	if absurd.IntervalSecs > cadenceMaxInterval.Seconds() {
		t.Errorf("estimate = %v, want it capped at %v", absurd.IntervalSecs,
			cadenceMaxInterval.Seconds())
	}
	if absurd.IntervalSecs <= before {
		t.Error("an unusually long gap should still move the estimate up somewhat")
	}
}

func TestStalledBoundary(t *testing.T) {
	t.Parallel()

	c := NewCadence() // ten-minute expectation
	const factor = 6.0
	threshold := int64(nominalInterval/time.Second) * int64(factor)

	cases := []struct {
		name        string
		now         int64
		lastBlockAt int64
		want        bool
		why         string
	}{
		{
			name: "just inside the threshold",
			now:  1000 + threshold, lastBlockAt: 1000, want: false,
			why: "exactly at the threshold is not yet over it",
		},
		{
			name: "just past the threshold",
			now:  1000 + threshold + 1, lastBlockAt: 1000, want: true,
			why: "one second past is past",
		},
		{
			name: "nothing seen yet",
			now:  100_000, lastBlockAt: 0, want: false,
			why: "never having seen a block is not evidence of a stall",
		},
		{
			name: "clock went backwards",
			now:  900, lastBlockAt: 1000, want: false,
			why: "a clock stepping backwards must not raise an alarm nobody can act on",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := c.Stalled(tc.now, tc.lastBlockAt, factor); got != tc.want {
				t.Errorf("Stalled = %v, want %v — %s", got, tc.want, tc.why)
			}
		})
	}
}

// A minority chain may legitimately be very slow. Judging it against a fixed
// threshold would call that a stall, which is wrong in the direction that destroys
// trust in the alarm.
func TestStalledIsJudgedAgainstTheChainsOwnPace(t *testing.T) {
	t.Parallel()

	slow := NewCadence()
	at := int64(0)
	for range 20 {
		at += 7200 // two hours apart
		slow = slow.Observe(at)
	}

	// Six hours of silence is unremarkable for this chain.
	sixHours := int64(6 * time.Hour / time.Second)
	if slow.Stalled(at+sixHours, at, 6.0) {
		t.Error("a chain producing blocks two hours apart was called stalled after six hours")
	}
	// Several days is not.
	fourDays := int64(96 * time.Hour / time.Second)
	if !slow.Stalled(at+fourDays, at, 6.0) {
		t.Error("four days of silence was not called a stall even for a slow chain")
	}
}

func TestStalledFallsBackToADefaultFactor(t *testing.T) {
	t.Parallel()

	c := NewCadence()
	threshold := int64(nominalInterval/time.Second) * int64(defaultStallFactor)
	if !c.Stalled(1000+threshold+1, 1000, 0) {
		t.Error("a zero factor should fall back to the default rather than never firing")
	}
}
