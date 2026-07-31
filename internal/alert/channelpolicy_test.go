package alert

import (
	"regexp"
	"strings"
	"testing"

	"github.com/paulscode/forktower/internal/bus"
	"github.com/paulscode/forktower/internal/store"
	"github.com/paulscode/forktower/internal/words"
)

// What the user is told about their own channels, and how urgently. One table,
// because every one of these decisions should be readable in one place.
func TestWhatChannelEventsTellTheUser(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		event    bus.Event
		wantTier store.Tier
		wantKind string
		wantNone bool
	}{
		{
			name: "somebody's commitment confirmed on the other chain",
			event: bus.FundingSpent{
				SpendEventID: 1, ChannelID: 7, Branch: "sq",
				Shape: string(store.ShapeCommitmentUnknown), Status: string(store.SpendConfirmed),
			},
			wantTier: store.TierCritical, wantKind: KindChannelSpent,
		},
		{
			name: "a commitment known to be revoked",
			event: bus.FundingSpent{
				ChannelID: 7, Shape: string(store.ShapeCommitmentRevoked),
				Status: string(store.SpendConfirmed),
			},
			wantTier: store.TierCritical, wantKind: KindChannelSpent,
		},
		{
			// Our own close reaching the other chain is worth knowing about, but
			// nobody is being robbed. Waking someone for it would be the false
			// alarm that teaches them to stop reading these.
			name: "our own close reached the other chain",
			event: bus.FundingSpent{
				ChannelID: 7, Shape: string(store.ShapeCommitmentOurs),
				Status: string(store.SpendConfirmed),
			},
			wantTier: store.TierWarning, wantKind: KindChannelSpent,
		},
		{
			name: "a close both sides agreed to",
			event: bus.FundingSpent{
				ChannelID: 7, Shape: string(store.ShapeMutualClose),
				Status: string(store.SpendConfirmed),
			},
			wantTier: store.TierInfo, wantKind: KindChannelSpent,
		},
		{
			// A spend of a channel that fits no shape is not something to pass
			// over quietly. Unrecognised is not the same as harmless.
			name: "a spend of a channel in a shape nobody recognises",
			event: bus.FundingSpent{
				ChannelID: 7, Shape: string(store.ShapeUnknown),
				Status: string(store.SpendConfirmed),
			},
			wantTier: store.TierCritical, wantKind: KindChannelSpent,
		},
		{
			// The confirmation is what starts a countdown; the sighting has its
			// own wording and its own alert.
			name: "an unconfirmed spend arriving on the confirmed event",
			event: bus.FundingSpent{
				ChannelID: 7, Shape: string(store.ShapeCommitmentUnknown),
				Status: string(store.SpendMempool),
			},
			wantNone: true,
		},
		{
			// Raised at critical even though nothing has confirmed: seeing it
			// early is worth a block of notice, which on a slow chain is a great
			// deal of time.
			name: "a commitment waiting to be mined",
			event: bus.MempoolSighting{
				ChannelID: 7, Shape: string(store.ShapeCommitmentUnknown),
			},
			wantTier: store.TierCritical, wantKind: KindChannelSpentSoon,
		},
		{
			name: "a cooperative close waiting to be mined is not an emergency",
			event: bus.MempoolSighting{
				ChannelID: 7, Shape: string(store.ShapeMutualClose),
			},
			wantNone: true,
		},
		{
			name:     "a close that left the chain",
			event:    bus.SpendReorgedOut{SpendEventID: 3, Branch: "sq"},
			wantTier: store.TierWarning, wantKind: KindSpendDisappeared,
		},
		{
			name: "a countdown getting louder",
			event: bus.DeadlineEscalated{
				DeadlineID: 2, ChannelID: 7, Level: 2, RemainingBlocks: 400,
				EstWallClock: "about 3 days",
			},
			wantTier: store.TierCritical, wantKind: KindDeadlineWarning,
		},
		{
			name:     "a countdown answered",
			event:    bus.DeadlineResolved{DeadlineID: 2, ByTxid: "abc"},
			wantTier: store.TierResolved, wantKind: KindDeadlineResolved,
		},
		{
			name:     "a countdown that ran out",
			event:    bus.DeadlineExpiredLoss{DeadlineID: 2, ChannelID: 7, AmountSat: 1000},
			wantTier: store.TierLoss, wantKind: KindLoss,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := MapEventToAlert(tc.event)
			if tc.wantNone {
				if ok {
					t.Fatalf("raised %q, want nothing", got.Kind)
				}
				return
			}
			if !ok {
				t.Fatal("raised nothing")
			}
			if got.Tier != tc.wantTier {
				t.Errorf("tier = %q, want %q", got.Tier, tc.wantTier)
			}
			if got.Kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", got.Kind, tc.wantKind)
			}
			if got.Message == "" || got.Subject == "" {
				t.Errorf("raised with nothing to read: %+v", got)
			}
			if got.DedupKey == "" {
				t.Error("raised with no deduplication key, so it can never be repeated or closed")
			}
		})
	}
}

// Each escalation tier has to be its own alert. Sharing a key would mean the
// second and third warnings quietly editing a message the user had already
// dismissed, which is the same as not sending them.
func TestEachEscalationTierIsItsOwnAlert(t *testing.T) {
	t.Parallel()

	keys := map[string]bool{}
	for level := 1; level <= 3; level++ {
		got, ok := MapEventToAlert(bus.DeadlineEscalated{
			DeadlineID: 2, ChannelID: 7, Level: level, RemainingBlocks: int32(100 * level),
		})
		if !ok {
			t.Fatalf("level %d raised nothing", level)
		}
		if keys[got.DedupKey] {
			t.Errorf("level %d reuses the key %q, so it would overwrite an earlier warning",
				level, got.DedupKey)
		}
		keys[got.DedupKey] = true
	}

	// Two different countdowns at the same tier are also different alerts.
	one, _ := MapEventToAlert(bus.DeadlineEscalated{DeadlineID: 1, Level: 2})
	two, _ := MapEventToAlert(bus.DeadlineEscalated{DeadlineID: 2, Level: 2})
	if one.DedupKey == two.DedupKey {
		t.Error("two countdowns at the same tier share one alert")
	}
}

// The estimate is the part a person can act on. Without it they will assume ten
// minutes a block, and on the chain this is counting that can be wrong by a
// factor of four.
func TestACountdownWarningCarriesTheTimeWhenThereIsOne(t *testing.T) {
	t.Parallel()

	with, _ := MapEventToAlert(bus.DeadlineEscalated{
		DeadlineID: 1, Level: 2, RemainingBlocks: 144, EstWallClock: "about 4 days",
	})
	if !strings.Contains(with.Message, "about 4 days") {
		t.Errorf("the estimate was dropped: %q", with.Message)
	}
	if !strings.Contains(with.Message, "144 blocks") {
		t.Errorf("the block count was dropped: %q", with.Message)
	}

	// And says nothing about time when there is nothing to say.
	without, _ := MapEventToAlert(bus.DeadlineEscalated{
		DeadlineID: 1, Level: 2, RemainingBlocks: 144,
	})
	for _, word := range []string{"hour", "day", "minute"} {
		if strings.Contains(without.Message, word) {
			t.Errorf("claimed %q about time with no estimate to go on: %q", word, without.Message)
		}
	}
}

// A countdown that has run out reads differently from one that is running. "0
// blocks left" is not a warning, it is a report.
func TestACountdownThatHasRunOutSaysSo(t *testing.T) {
	t.Parallel()

	got, _ := MapEventToAlert(bus.DeadlineEscalated{DeadlineID: 1, Level: 3, RemainingBlocks: 0})
	if strings.Contains(got.Message, "0 blocks left") {
		t.Errorf("counted down to zero out loud: %q", got.Message)
	}
	if !strings.Contains(strings.ToLower(got.Subject), "run out") {
		t.Errorf("subject = %q", got.Subject)
	}
}

// A close that disappeared must not read as relief. A counterparty replacing
// their transaction with a higher fee looks exactly like this.
func TestADisappearedCloseDoesNotSoundLikeGoodNews(t *testing.T) {
	t.Parallel()

	got, _ := MapEventToAlert(bus.SpendReorgedOut{SpendEventID: 1, Branch: "sq"})
	lower := strings.ToLower(got.Subject + " " + got.Message)
	for _, word := range []string{"safe", "resolved", "over", "no longer a"} {
		if strings.Contains(lower, word) {
			t.Errorf("a disappeared close reads as reassurance (%q): %q", word, got.Message)
		}
	}
	if got.Tier == store.TierResolved {
		t.Error("a disappeared close was filed as good news")
	}
}

// Nothing about a channel may appear in the words. These alerts travel through
// other people's servers, and an operator told which channel, how much, or how
// long has been handed the attacker's ideal input.
func TestChannelAlertsNameNothingSpecific(t *testing.T) {
	t.Parallel()

	events := []bus.Event{
		bus.FundingSpent{
			SpendEventID: 12345, ChannelID: 7, Branch: "sq",
			SpendTxid: "deadbeefcafe1234", Shape: string(store.ShapeCommitmentUnknown),
			Status: string(store.SpendConfirmed), Height: 987654,
		},
		bus.MempoolSighting{
			SpendEventID: 12345, ChannelID: 7, Branch: "sq",
			SpendTxid: "deadbeefcafe1234", Shape: string(store.ShapeCommitmentUnknown),
		},
		bus.SpendReorgedOut{SpendEventID: 12345, Branch: "sq"},
		bus.DeadlineExpiredLoss{DeadlineID: 2, ChannelID: 7, AmountSat: 4_200_000},
		bus.DeadlineResolved{DeadlineID: 2, ByTxid: "deadbeefcafe1234"},
	}
	forbidden := []string{"deadbeef", "4200000", "4,200,000", "987654"}
	// Any run of digits long enough to be an amount, a height or an identifier.
	longNumber := regexp.MustCompile(`\d{5,}`)

	for _, e := range events {
		got, ok := MapEventToAlert(e)
		if !ok {
			continue
		}
		text := got.Subject + " " + got.Message
		for _, leak := range forbidden {
			if strings.Contains(text, leak) {
				t.Errorf("%T leaks %q: %q", e, leak, text)
			}
		}
		// And nothing this program calls things among itself, from the one list
		// the dashboard and the timeline also check against.
		if leak := words.FindInternal(text); leak != "" {
			t.Errorf("%T puts the internal name %q in front of a user: %q", e, leak, text)
		}
		if found := longNumber.FindString(text); found != "" {
			t.Errorf("%T carries the number %q, which belongs on the dashboard: %q",
				e, found, text)
		}
	}
}

// The slow-burn warning is not raised by anything happening, which is the whole
// point of it.
func TestTheSlowBurnWarningExplainsTheExposure(t *testing.T) {
	t.Parallel()

	got := ClosedOnlyOnYourChain(7)
	if got.Tier != store.TierWarning || got.Kind != KindClosedOnlyOnYours {
		t.Errorf("got %+v", got)
	}
	if got.DedupKey != "closed_only_on_your_chain:7" {
		t.Errorf("dedup key = %q", got.DedupKey)
	}
	// The exposure people do not expect, said in a sentence: a closed channel
	// feels finished, and on the other chain it is not.
	lower := strings.ToLower(got.Message)
	if !strings.Contains(lower, "closed") || !strings.Contains(lower, "still be spent") {
		t.Errorf("the message does not explain the exposure: %q", got.Message)
	}
	// Two channels are two warnings.
	if ClosedOnlyOnYourChain(8).DedupKey == got.DedupKey {
		t.Error("two channels share one warning")
	}
}

// Every kind this package can raise must be a stable, non-empty string: they are
// in every payload, including the content-free ones, and a user's automation may
// key off them.
func TestEveryChannelAlertKindIsStable(t *testing.T) {
	t.Parallel()

	kinds := []string{
		KindChannelSpent, KindChannelSpentSoon, KindSpendDisappeared,
		KindDeadlineWarning, KindDeadlineResolved, KindLoss, KindClosedOnlyOnYours,
	}
	seen := map[string]bool{}
	for _, k := range kinds {
		if k == "" {
			t.Fatal("an alert kind is empty")
		}
		if seen[k] {
			t.Errorf("%q is used twice", k)
		}
		seen[k] = true
		if strings.ToLower(k) != k || strings.Contains(k, " ") {
			t.Errorf("%q is not a stable machine-readable name", k)
		}
	}
}

// A loss must reach somebody who asked only for critical alerts. Being told you
// lost money is not a "resolved" you can filter away.
func TestALossReachesEvenTheNarrowestFilter(t *testing.T) {
	t.Parallel()

	got, ok := MapEventToAlert(bus.DeadlineExpiredLoss{DeadlineID: 1, ChannelID: 7})
	if !ok {
		t.Fatal("a loss raised nothing")
	}
	if !Urgent(got.Tier) {
		t.Error("a loss is not urgent enough to be repeated until acknowledged")
	}
}
