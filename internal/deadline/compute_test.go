package deadline

import (
	"testing"
	"time"

	"github.com/paulscode/forktower/internal/store"
)

func delay(v int32) *int32 { return &v }

// Which delay applies is the mistake that matters most here, and it is easy to
// make backwards. A commitment the peer published leaves *their* output waiting
// on the delay *they* agreed to, and that wait is the user's window to answer
// it. Getting it the wrong way round would produce a countdown of the wrong
// length in the one situation where the length is everything.
func TestWhoseDelayApplies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		shape      store.SpendShape
		local      *int32
		remote     *int32
		wantHeight int32
		wantAssume bool
	}{
		{
			name:       "somebody else's commitment waits on the delay they agreed to",
			shape:      store.ShapeCommitmentUnknown,
			local:      delay(144),
			remote:     delay(2016),
			wantHeight: 1000 + 2016,
		},
		{
			name:       "our own commitment waits on the delay we agreed to",
			shape:      store.ShapeCommitmentOurs,
			local:      delay(144),
			remote:     delay(2016),
			wantHeight: 1000 + 144,
		},
		{
			name:       "a commitment known to be revoked is still theirs",
			shape:      store.ShapeCommitmentRevoked,
			local:      delay(144),
			remote:     delay(2016),
			wantHeight: 1000 + 2016,
		},
		{
			// The rule this project cannot break: no input, no excuse. A skipped row
			// means no countdown, no escalation and no loss event, so the breach
			// would alert once and then go quiet for exactly as long as the window
			// it was supposed to be counting.
			name:       "no delay recorded still produces a countdown, on a floor",
			shape:      store.ShapeCommitmentUnknown,
			local:      delay(144),
			wantHeight: 1000 + store.AssumedDeadlineFloor,
			wantAssume: true,
		},
		{
			name:       "our own commitment with no delay of ours recorded",
			shape:      store.ShapeCommitmentOurs,
			remote:     delay(2016),
			wantHeight: 1000 + store.AssumedDeadlineFloor,
			wantAssume: true,
		},
		{
			// A zero delay would put the deadline in the block the commitment
			// confirmed in, which reads as already lost and would fire a loss event
			// immediately.
			name:       "a delay of zero is a missing input wearing a number",
			shape:      store.ShapeCommitmentUnknown,
			remote:     delay(0),
			wantHeight: 1000 + store.AssumedDeadlineFloor,
			wantAssume: true,
		},
		{
			name:       "a negative delay likewise",
			shape:      store.ShapeCommitmentUnknown,
			remote:     delay(-5),
			wantHeight: 1000 + store.AssumedDeadlineFloor,
			wantAssume: true,
		},
		{
			// The protocol caps this at sixteen bits, so anything larger came from
			// somewhere it should not have and would produce a countdown lasting
			// decades.
			name:       "a delay beyond what the protocol allows",
			shape:      store.ShapeCommitmentUnknown,
			remote:     delay(1 << 20),
			wantHeight: 1000 + store.AssumedDeadlineFloor,
			wantAssume: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Compute(Inputs{
				ConfirmHeight:  1000,
				Shape:          tc.shape,
				CSVDelayLocal:  tc.local,
				CSVDelayRemote: tc.remote,
			})
			if len(got) != 1 {
				t.Fatalf("produced %d deadlines, want just the commitment's", len(got))
			}
			if got[0].Kind != store.DeadlineCSV {
				t.Errorf("kind = %q", got[0].Kind)
			}
			if got[0].Height != tc.wantHeight {
				t.Errorf("deadline height = %d, want %d", got[0].Height, tc.wantHeight)
			}
			if got[0].Assumed != tc.wantAssume {
				t.Errorf("assumed = %v, want %v", got[0].Assumed, tc.wantAssume)
			}
		})
	}
}

// Payments in flight have their own clocks, and they can run out sooner than the
// commitment's does.
func TestPaymentsInFlightGetTheirOwnClocks(t *testing.T) {
	t.Parallel()

	got := Compute(Inputs{
		ConfirmHeight:  1000,
		Shape:          store.ShapeCommitmentUnknown,
		CSVDelayRemote: delay(1000),
		HTLCs: []store.HTLCSnapshot{
			{Direction: "incoming", CLTVExpiry: 1200, AmountMsat: 5000},
			{Direction: "outgoing", CLTVExpiry: 1500, AmountMsat: 7000},
			{Direction: "sideways", CLTVExpiry: 1100},
		},
	})

	kinds := map[store.DeadlineKind]int32{}
	for _, c := range got {
		kinds[c.Kind] = c.Height
	}
	if kinds[store.DeadlineCSV] != 2000 {
		t.Errorf("the commitment's own deadline is at %d", kinds[store.DeadlineCSV])
	}
	if kinds[store.DeadlineHTLCIncoming] != 1200 {
		t.Errorf("the incoming payment's deadline is at %d", kinds[store.DeadlineHTLCIncoming])
	}
	if kinds[store.DeadlineHTLCOutgoing] != 1500 {
		t.Errorf("the outgoing payment's deadline is at %d", kinds[store.DeadlineHTLCOutgoing])
	}
	// A direction nobody recognises is not turned into a clock pointing the wrong
	// way; it is left out, and the commitment's own deadline still covers the
	// channel.
	if len(got) != 3 {
		t.Errorf("produced %d deadlines, want 3", len(got))
	}
}

func TestAPaymentWithNoExpiryRecordedStillGetsAClock(t *testing.T) {
	t.Parallel()

	got := Compute(Inputs{
		ConfirmHeight:  1000,
		Shape:          store.ShapeCommitmentUnknown,
		CSVDelayRemote: delay(500),
		HTLCs:          []store.HTLCSnapshot{{Direction: "incoming", CLTVExpiry: 0}},
	})
	if len(got) != 2 {
		t.Fatalf("produced %d deadlines, want 2", len(got))
	}
	htlc := got[1]
	if !htlc.Assumed || htlc.Height != 1000+store.AssumedDeadlineFloor {
		t.Errorf("got %+v, want a flagged floor", htlc)
	}
}

// The soonest clock is the one that decides how long the user actually has.
func TestTheSoonestClockIsTheOneThatCounts(t *testing.T) {
	t.Parallel()

	computed := []Computed{
		{Kind: store.DeadlineCSV, Height: 2000},
		{Kind: store.DeadlineHTLCOutgoing, Height: 1500},
		{Kind: store.DeadlineHTLCIncoming, Height: 1200},
	}
	got, ok := Earliest(computed)
	if !ok || got.Height != 1200 || got.Kind != store.DeadlineHTLCIncoming {
		t.Errorf("earliest = %+v", got)
	}

	// A tie goes to the commitment's own deadline, because its expiry loses the
	// channel rather than one payment.
	tied, ok := Earliest([]Computed{
		{Kind: store.DeadlineHTLCIncoming, Height: 1200},
		{Kind: store.DeadlineCSV, Height: 1200},
	})
	if !ok || tied.Kind != store.DeadlineCSV {
		t.Errorf("a tie went to %q", tied.Kind)
	}

	if _, ok := Earliest(nil); ok {
		t.Error("found an earliest deadline among none")
	}
}

func TestRemainingNeverGoesNegative(t *testing.T) {
	t.Parallel()

	cases := []struct{ deadline, tip, want int32 }{
		{2000, 1000, 1000},
		{2000, 1999, 1},
		{2000, 2000, 0},
		{2000, 5000, 0},
	}
	for _, c := range cases {
		if got := Remaining(c.deadline, c.tip); got != c.want {
			t.Errorf("Remaining(%d, %d) = %d, want %d", c.deadline, c.tip, got, c.want)
		}
	}
}

// Tiers are a fraction of the window rather than a fixed number of blocks,
// because windows differ by an order of magnitude between channels. A fifth of a
// 2016-block delay is four hundred blocks of warning; a fifth of a 144-block one
// is thirty. Both mean "nearly out of time" for the channel they belong to.
func TestEscalationTiersAreAFractionOfTheWindow(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name              string
		remaining, window int32
		want              int32
	}{
		{"the whole window still ahead", 2016, 2016, LevelDetected},
		{"just over half left", 1100, 2016, LevelDetected},
		{"just under half left", 1000, 2016, LevelHalf},
		{"a quarter left", 500, 2016, LevelHalf},
		{"under a fifth left", 400, 2016, LevelUrgent},
		{"almost none left", 1, 2016, LevelUrgent},
		{"none left", 0, 2016, LevelUrgent},
		{"past the deadline", -50, 2016, LevelUrgent},

		// The same fractions on a much shorter window.
		{"short window, half left", 70, 144, LevelHalf},
		{"short window, a fifth left", 28, 144, LevelUrgent},

		// No window to measure against: still counting, but no claim about how far
		// through it is.
		{"no window known", 100, 0, LevelDetected},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := Level(c.remaining, c.window); got != c.want {
				t.Errorf("Level(%d, %d) = %d, want %d", c.remaining, c.window, got, c.want)
			}
		})
	}
}

// A block count on its own is not an answer. A minority chain before a retarget
// can take half an hour a block, so the same count can mean far more human time
// than instinct says.
func TestTheTimeProjection(t *testing.T) {
	t.Parallel()

	got, ok := Project(144, 600)
	if !ok || got != 24*time.Hour {
		t.Errorf("144 blocks at ten minutes = %v (ok=%v)", got, ok)
	}

	// The same block count on a chain producing a block every forty minutes is
	// four times as much human time, which is the whole point of projecting.
	slow, ok := Project(144, 2400)
	if !ok || slow != 96*time.Hour {
		t.Errorf("144 blocks at forty minutes = %v (ok=%v)", slow, ok)
	}

	// No cadence, no claim. A caller must say nothing about time rather than
	// assume ten minutes.
	for _, interval := range []float64{0, -1} {
		if _, ok := Project(144, interval); ok {
			t.Errorf("projected a time from a cadence of %v", interval)
		}
	}
	if _, ok := Project(1<<30, 600); ok {
		t.Error("projected a time beyond any horizon worth counting to")
	}
	if got, ok := Project(0, 600); !ok || got != 0 {
		t.Errorf("no blocks left projected as %v", got)
	}
}

// Rounded hard on purpose: this is an estimate built on a chain's recent
// cadence, and a precision nobody has is a precision that misleads.
func TestTimeIsSaidInWordsSomebodyCanActOn(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "no time left"},
		{-time.Hour, "no time left"},
		{30 * time.Second, aboutAMinute},
		{25 * time.Minute, "about 25 minutes"},
		{90 * time.Minute, "about 2 hours"},
		{6*time.Hour + 51*time.Minute, "about 7 hours"},
		{30 * time.Hour, "about 30 hours"},
		{50 * time.Hour, "about 2 days"},
		{14 * 24 * time.Hour, "about 14 days"},
	}
	for _, c := range cases {
		if got := HumanDuration(c.in); got != c.want {
			t.Errorf("HumanDuration(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Whatever the inputs, a confirmed commitment always produces at least one
// countdown, and every one of them is a kind the schema accepts.
func TestEveryCommitmentGetsAtLeastOneClock(t *testing.T) {
	t.Parallel()

	shapes := []store.SpendShape{
		store.ShapeCommitmentOurs, store.ShapeCommitmentUnknown,
		store.ShapeCommitmentRevoked,
	}
	delays := []*int32{nil, delay(-1), delay(0), delay(1), delay(144), delay(1 << 20)}
	heights := []int32{0, 1, 1000}

	for _, shape := range shapes {
		for _, local := range delays {
			for _, remote := range delays {
				for _, height := range heights {
					got := Compute(Inputs{
						ConfirmHeight: height, Shape: shape,
						CSVDelayLocal: local, CSVDelayRemote: remote,
					})
					if len(got) == 0 {
						t.Fatalf("shape %q with delays %v/%v produced no countdown at all",
							shape, local, remote)
					}
					for _, c := range got {
						if !c.Kind.Valid() {
							t.Fatalf("produced kind %q", c.Kind)
						}
						if c.Height <= height {
							t.Fatalf("shape %q produced a deadline at %d, at or before the "+
								"block it started from (%d)", shape, c.Height, height)
						}
					}
				}
			}
		}
	}
}
