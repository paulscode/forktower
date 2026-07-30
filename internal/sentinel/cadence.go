package sentinel

import (
	"math"
	"time"
)

// Cadence tracking constants.
const (
	// cadenceAlpha weights each new interval against the running average. High
	// enough to follow a chain whose block rate has genuinely changed — which is
	// the whole point during a split, when one chain may slow by an order of
	// magnitude — and low enough that a single unusually fast or slow block does
	// not move the estimate much.
	cadenceAlpha = 0.3

	// nominalInterval is the interval the network aims for, used to seed the
	// average before anything has been measured and as a floor afterwards.
	//
	// A floor matters: a burst of quick blocks would otherwise drive the estimate
	// down and make the stall threshold hair-trigger, so a chain that is merely
	// lively would keep being reported as stalled.
	nominalInterval = 10 * time.Minute

	// cadenceMaxInterval caps a single measurement. A header timestamp is the
	// miner's claim, not a measurement, and is only loosely constrained by
	// consensus — one absurd value should not poison the average for hours.
	cadenceMaxInterval = 24 * time.Hour
)

// Cadence is a running estimate of how far apart one chain's blocks are.
//
// Estimated rather than assumed, because during a split the two chains can run at
// wildly different rates: a chain with a small share of hashing power may produce
// blocks hours apart, so the same number of blocks means a very different amount
// of human time. Every countdown shown to a user is converted through this, and
// is always labelled an estimate.
type Cadence struct {
	// IntervalSecs is the smoothed interval between blocks.
	IntervalSecs float64
	// LastBlockTime is the header timestamp of the most recent block seen, in unix
	// seconds. Zero before any block.
	LastBlockTime int64
	// Samples counts the intervals folded in, for diagnostics and to distinguish a
	// seeded estimate from a measured one.
	Samples int
}

// NewCadence returns the starting estimate: the network's nominal interval, with
// nothing measured yet.
func NewCadence() Cadence {
	return Cadence{IntervalSecs: nominalInterval.Seconds()}
}

// Measured reports whether any real interval has been folded in.
func (c Cadence) Measured() bool { return c.Samples > 0 }

// Observe folds in a new block's header timestamp.
//
// The first block only establishes a reference point; an interval needs two. A
// timestamp that is not after the previous one is ignored rather than treated as
// a negative interval — header timestamps are permitted to go backwards within
// limits, and a chain doing so is not evidence about its speed.
func (c Cadence) Observe(headerTime int64) Cadence {
	if c.LastBlockTime == 0 {
		c.LastBlockTime = headerTime
		return c
	}
	if headerTime <= c.LastBlockTime {
		// Out of order or duplicated. Keep the later reference so the next interval
		// is measured from the newest point we know of.
		if headerTime > 0 {
			c.LastBlockTime = maxInt64(c.LastBlockTime, headerTime)
		}
		return c
	}

	interval := float64(headerTime - c.LastBlockTime)
	if interval > cadenceMaxInterval.Seconds() {
		interval = cadenceMaxInterval.Seconds()
	}

	c.IntervalSecs = cadenceAlpha*interval + (1-cadenceAlpha)*c.IntervalSecs
	c.LastBlockTime = headerTime
	c.Samples++
	return c
}

// ExpectedInterval is the interval to reason about, never below the network's
// nominal rate.
//
// Floored deliberately. A chain that has just produced several quick blocks would
// otherwise get a very short expectation, and the next ordinary gap would be
// reported as a stall — crying wolf about the thing the user most needs to
// believe.
func (c Cadence) ExpectedInterval() time.Duration {
	secs := math.Max(c.IntervalSecs, nominalInterval.Seconds())
	return time.Duration(secs * float64(time.Second))
}

// Stalled reports whether a chain has been silent for longer than its own rate
// explains.
//
// Judged against that chain's measured pace rather than a fixed threshold,
// because a minority chain may legitimately be very slow, and calling that a
// stall would be wrong in the direction that destroys trust in the alarm.
// lastBlockAt and now are wall-clock unix seconds: this asks how long since we
// last *saw* a block, which is a measurement, unlike a header timestamp.
func (c Cadence) Stalled(now, lastBlockAt int64, factor float64) bool {
	if lastBlockAt <= 0 || now <= lastBlockAt {
		// Nothing seen yet, or a clock that has gone backwards. Neither is evidence
		// of a stall, and guessing here would produce alarms nobody can act on.
		return false
	}
	if factor <= 0 {
		factor = defaultStallFactor
	}
	threshold := float64(c.ExpectedInterval()/time.Second) * factor
	return float64(now-lastBlockAt) > threshold
}

// defaultStallFactor is used when none is configured. Six nominal intervals is an
// hour on a chain running normally: long enough that ordinary variance does not
// trip it, short enough to notice within the window that matters.
const defaultStallFactor = 6.0

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
