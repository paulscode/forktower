package alert

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/paulscode/forktower/internal/redact"
	"github.com/paulscode/forktower/internal/store"
	"github.com/paulscode/forktower/internal/words"
)

// Alert kinds for the transaction mirror.
const (
	// KindMirrorStuck means a transaction the user needs on the other chain is
	// not getting there.
	KindMirrorStuck = "mirror_stuck"
	// KindMirrorGaveUp means Forktower has stopped trying.
	KindMirrorGaveUp = "mirror_gave_up"
)

// mirrorSweep raises what the mirror could not get across.
//
// **Driven from stored state rather than from an event, and that is the point.**
// A transaction refused by the other chain is not a moment; it is a condition
// that persists and matters more the longer it lasts. Reading it from the
// database means the alert survives a restart, survives a dropped event, and
// arrives even if the refusal happened while the daemon was stopped.
//
// This is also the shape of the answer to the alerter's oldest gap: everything
// else here is raised by something happening, so anything that failed to happen
// went unmentioned. See [Alerter.reconcile].
func (a *Alerter) mirrorSweep(ctx context.Context) {
	stuck, err := a.store.ListMirrorDecisions(ctx, store.MirrorFilter{
		State: store.MirrorRejected,
	})
	if err != nil {
		a.log.Error("reading what could not be copied to the other chain",
			slog.String("error", err.Error()))
		return
	}
	for _, d := range stuck {
		// Only once it has been tried enough that this is a condition rather than
		// a moment. A transaction refused on its first attempt is usually accepted
		// on its second, and alerting immediately would train people to ignore it.
		const triedEnough = 3
		if d.Attempts < triedEnough {
			continue
		}
		a.raise(ctx, Candidate{
			Tier: store.TierWarning, Kind: KindMirrorStuck,
			DedupKey: fmt.Sprintf("%s:%s", KindMirrorStuck, d.TxID),
			Subject:  "A transaction is not reaching " + words.OtherChain,
			Message: "Forktower has tried " + attemptCount(d.Attempts) +
				" to copy one of your channel's transactions to " + words.OtherChain +
				" and it has not been accepted. " + mirrorWhy(d),
		})
	}

	abandoned, err := a.store.ListMirrorDecisions(ctx, store.MirrorFilter{
		State: store.MirrorAbandoned,
	})
	if err != nil {
		a.log.Error("reading what could not be copied to the other chain",
			slog.String("error", err.Error()))
		return
	}
	for _, d := range abandoned {
		a.raise(ctx, Candidate{
			Tier: store.TierWarning, Kind: KindMirrorGaveUp,
			DedupKey: fmt.Sprintf("%s:%s", KindMirrorGaveUp, d.TxID),
			Subject:  "A transaction could not be copied to " + words.OtherChain,
			Message: "Forktower has stopped trying to copy one of your channel's " +
				"transactions to " + words.OtherChain + ". " + mirrorWhy(d) +
				" This may need you to do something: open Forktower to see what.",
		})
	}
}

// mirrorWhy is the node's own account of the refusal, cleaned up enough to read.
//
// Passed through rather than summarised. The classification is Forktower's
// reading of it; the words are the evidence, and a user comparing them with what
// their own node says needs them to match.
func mirrorWhy(d store.MirrorDecision) string {
	if d.LastError == "" {
		return "The other chain did not say why."
	}
	return "The other chain said: " + redact.String(d.LastError) + "."
}

// attemptCount says "1 time" or "4 times".
//
// Written out rather than reaching for the shared plural helper next door: that
// one picks between two words, this one has to put the number in front, and the
// two needs kept colliding.
func attemptCount(n int64) string {
	if n == 1 {
		return "1 time"
	}
	return fmt.Sprintf("%d times", n)
}

// reconcile raises what stored state says is wrong but no event announced.
//
// **This is the answer to the gap this alerter has had since it was written.**
// Every other alert here is raised by an event, and an event is delivered once:
// if the bus dropped it, if the alerter was busy, if the daemon was stopped when
// it happened — the alert never existed and never will. For a program whose
// entire purpose is to be the thing that notices, an alert that can be missed
// permanently is the wrong shape.
//
// So the conditions where being missed costs the most are also derived from
// storage on a timer. That does not make the events redundant: an event arrives
// in seconds and a sweep in minutes, and for a breach seconds matter. It makes
// the events an optimisation rather than the only path, which is the property
// worth having.
//
// Deliberately narrow. Reconciling everything would mean re-deriving each alert
// from state, and the states that are genuinely *only* knowable from an event —
// a reorg that undid something, a countdown crossing a threshold — have no
// stored form to read. What is covered here is what persists: a tower that is
// not protecting anything, and a transaction that is not getting across.
func (a *Alerter) reconcile(ctx context.Context) {
	a.mirrorSweep(ctx)
	a.towerSweep(ctx)
}

// towerSweep raises the tower conditions that stored state can prove.
func (a *Alerter) towerSweep(ctx context.Context) {
	towers, err := a.store.ListTowers(ctx, store.TowerFilter{})
	if err != nil {
		a.log.Error("reading the watchtowers", slog.String("error", err.Error()))
		return
	}

	for _, t := range towers {
		if t.Status == store.TowerUnreachable {
			a.raise(ctx, Candidate{
				Tier: store.TierWarning, Kind: KindTowerDown,
				DedupKey: fmt.Sprintf("%s:%d", KindTowerDown, t.ID),
				Subject:  "Your watchtower is not answering",
				Message: "The watchtower that would punish a broken promise on " +
					words.OtherChain + " is not answering. Until it is back, a channel " +
					"closed against you there would not be answered. " +
					detailSentence(t.StatusDetail),
			})
		}

		uncovered, covErr := a.store.ListCoverage(ctx, store.CoverageFilter{
			TowerID: t.ID, UncoverableOnly: true,
		})
		if covErr != nil {
			a.log.Error("reading what the watchtowers protect",
				slog.String("error", covErr.Error()))
			continue
		}
		for _, c := range uncovered {
			a.raise(ctx, Candidate{
				Tier: store.TierWarning, Kind: KindTowerNotProtecting,
				DedupKey: fmt.Sprintf("%s:channel:%d", KindTowerNotProtecting, c.ChannelID),
				Subject:  "One of your channels is not protected by your watchtower",
				Message: "One of your channels is not being backed up to your " +
					"watchtower, so a broken promise against it on " + words.OtherChain +
					" would not be answered. " + c.Reason + ".",
			})
		}
	}
}
