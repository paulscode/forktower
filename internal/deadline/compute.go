package deadline

import (
	"fmt"
	"math"
	"time"

	"github.com/paulscode/forktower/internal/store"
)

// Escalation tiers. Zero means nothing has been said yet.
const (
	// LevelDetected is raised the moment a commitment confirms, before any of the
	// window has run. The user's best chance of doing something is now.
	LevelDetected int32 = 1
	// LevelHalf is raised once less than half the window is left.
	LevelHalf int32 = 2
	// LevelUrgent is raised once less than a fifth is left, and is where the
	// alerting stops being polite about it.
	LevelUrgent int32 = 3
)

// Fractions of the window at which the tiers change.
const (
	halfThreshold   = 0.50
	urgentThreshold = 0.20
)

// The two directions a payment in flight can point, as the store records them.
const (
	directionIncoming = "incoming"
	directionOutgoing = "outgoing"
)

// What a countdown says at its two extremes.
const (
	noTimeLeft   = "no time left"
	aboutAMinute = "about a minute"
)

// maxSaneDelay bounds a delay we will believe. The protocol caps `to_self_delay`
// at what fits in sixteen bits, so anything larger came from somewhere it should
// not have, and treating it as real would produce a countdown lasting decades.
const maxSaneDelay = 1 << 16

// Inputs are everything the deadline computation is allowed to look at.
type Inputs struct {
	// ConfirmHeight is the block the commitment confirmed in, on the chain being
	// watched.
	ConfirmHeight int32
	// Shape is what that commitment turned out to be. It decides *whose* delay
	// applies, which is the difference between a countdown to losing money and a
	// countdown to being able to claim it.
	Shape store.SpendShape

	// CSVDelayLocal is what we must wait after our own commitment confirms.
	// CSVDelayRemote is what the peer must wait after theirs — which makes it the
	// window we have to answer a breach. Nil means the node did not say.
	CSVDelayLocal  *int32
	CSVDelayRemote *int32

	// HTLCs are the payments that were in flight, as last seen. Their expiries
	// can fall *before* the commitment's own delay, and then they are what
	// matters.
	HTLCs []store.HTLCSnapshot
}

// Computed is one clock, and whether we had to guess at it.
type Computed struct {
	Kind   store.DeadlineKind
	Height int32
	// Assumed means an input was missing and a conservative floor was used. The
	// countdown is still real; the user should simply know it is a floor.
	Assumed bool
}

// Compute works out every deadline a confirmed commitment starts.
//
// Pure: no storage, no clock, no network. The rules here decide when somebody
// loses money, so they are the part that has to be checkable by reading.
//
// **Nothing is ever skipped for a missing input.** The natural implementation —
// no delay, no row — produces no countdown, no escalation and no loss event, so
// the breach would alert once when it was detected and then go silent for
// exactly as long as the window it was supposed to be counting. A floor with a
// flag on it is worth far more than a gap.
func Compute(in Inputs) []Computed {
	out := []Computed{computeCSV(in)}
	for _, h := range in.HTLCs {
		if computed, ok := computeHTLC(in, h); ok {
			out = append(out, computed)
		}
	}
	return out
}

// computeCSV is the commitment's own delay.
func computeCSV(in Inputs) Computed {
	delay, known := applicableDelay(in)
	if !known {
		return Computed{
			Kind:    store.DeadlineCSV,
			Height:  in.ConfirmHeight + store.AssumedDeadlineFloor,
			Assumed: true,
		}
	}
	return Computed{Kind: store.DeadlineCSV, Height: in.ConfirmHeight + delay}
}

// applicableDelay picks whose delay this commitment is subject to.
//
// The distinction is the whole point and is easy to get backwards. A commitment
// the *peer* published leaves their own output encumbered by the delay *they*
// agreed to wait, and that wait is our window to answer it. Our own commitment
// leaves our output encumbered by the delay *we* agreed to.
func applicableDelay(in Inputs) (int32, bool) {
	chosen := in.CSVDelayRemote
	if in.Shape == store.ShapeCommitmentOurs {
		chosen = in.CSVDelayLocal
	}
	if chosen == nil {
		return 0, false
	}
	// A zero or negative delay would put the deadline at or before the block the
	// commitment confirmed in, which reads as already lost. An implausibly large
	// one would put it beyond any horizon worth counting to. Neither is a delay;
	// both are a missing input wearing a number.
	if *chosen <= 0 || *chosen >= maxSaneDelay {
		return 0, false
	}
	return *chosen, true
}

// computeHTLC is one payment in flight.
//
// Reported whichever way it points. An outgoing payment times out at its expiry
// and an incoming one must be claimed before its expiry, and both of those are
// heights at which something is lost if nobody acts.
func computeHTLC(in Inputs, h store.HTLCSnapshot) (Computed, bool) {
	kind, ok := htlcKind(h.Direction)
	if !ok {
		return Computed{}, false
	}
	// An expiry nobody recorded gets the same treatment as a missing delay: a
	// floor, flagged, rather than a payment with no clock on it at all.
	if h.CLTVExpiry <= 0 {
		return Computed{
			Kind:    kind,
			Height:  in.ConfirmHeight + store.AssumedDeadlineFloor,
			Assumed: true,
		}, true
	}
	return Computed{Kind: kind, Height: h.CLTVExpiry}, true
}

func htlcKind(direction string) (store.DeadlineKind, bool) {
	switch direction {
	case directionIncoming:
		return store.DeadlineHTLCIncoming, true
	case directionOutgoing:
		return store.DeadlineHTLCOutgoing, true
	default:
		return "", false
	}
}

// Earliest is the deadline that matters: the one that arrives first.
//
// A channel can have several clocks running and only the soonest decides how
// long the user actually has. Ties go to the commitment's own delay, because
// that is the one whose expiry loses the channel rather than a single payment.
func Earliest(computed []Computed) (Computed, bool) {
	var best Computed
	var found bool
	for _, c := range computed {
		switch {
		case !found, c.Height < best.Height:
			best, found = c, true
		case c.Height == best.Height && c.Kind == store.DeadlineCSV:
			best = c
		}
	}
	return best, found
}

// Remaining is how many blocks are left, never below zero.
func Remaining(deadlineHeight, tipHeight int32) int32 {
	if tipHeight >= deadlineHeight {
		return 0
	}
	return deadlineHeight - tipHeight
}

// Level is the escalation tier a countdown has reached.
//
// Measured as a fraction of the window rather than as a fixed number of blocks,
// because the windows differ by an order of magnitude between channels: a fifth
// of a 2016-block delay is still four hundred blocks of warning, and a fifth of
// a 144-block one is thirty. Both are "nearly out of time" for the channel they
// belong to.
func Level(remaining, window int32) int32 {
	if remaining <= 0 {
		return LevelUrgent
	}
	if window <= 0 {
		// No window to measure against. The countdown is still running, and the
		// honest answer is the tier that says "this is happening" without
		// pretending to know how far through it is.
		return LevelDetected
	}
	fraction := float64(remaining) / float64(window)
	switch {
	case fraction < urgentThreshold:
		return LevelUrgent
	case fraction < halfThreshold:
		return LevelHalf
	default:
		return LevelDetected
	}
}

// Project turns a number of blocks into an amount of human time.
//
// The whole reason this exists: a minority chain before a difficulty retarget
// can take half an hour or more per block, so the same block count can mean far
// more human time than anyone's instinct says. Telling somebody "thirty blocks"
// and letting them assume five hours when it is fifteen is the sort of help that
// costs money.
//
// Returns zero when the chain's cadence is not known, and callers must then say
// nothing about time rather than assume ten minutes.
func Project(remaining int32, avgIntervalSecs float64) (time.Duration, bool) {
	if remaining <= 0 {
		return 0, true
	}
	if avgIntervalSecs <= 0 || math.IsNaN(avgIntervalSecs) || math.IsInf(avgIntervalSecs, 0) {
		return 0, false
	}
	seconds := float64(remaining) * avgIntervalSecs
	// A projection beyond a few years is arithmetic rather than information, and
	// would overflow a duration soon after.
	const maxProjection = float64(10 * 365 * 24 * 60 * 60)
	if seconds > maxProjection {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}

// HumanDuration says how long something is in words somebody can act on.
//
// Rounded hard on purpose. This is an estimate built on a chain's recent
// cadence, and "about 7 hours" is honest where "6 hours 51 minutes" would be a
// precision nobody has.
func HumanDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return noTimeLeft
	case d < time.Hour:
		minutes := int(math.Round(d.Minutes()))
		if minutes <= 1 {
			return aboutAMinute
		}
		return fmt.Sprintf("about %d minutes", minutes)
	case d < 36*time.Hour:
		hours := int(math.Round(d.Hours()))
		if hours <= 1 {
			return "about an hour"
		}
		return fmt.Sprintf("about %d hours", hours)
	default:
		days := int(math.Round(d.Hours() / 24))
		if days <= 1 {
			return "about a day"
		}
		return fmt.Sprintf("about %d days", days)
	}
}
