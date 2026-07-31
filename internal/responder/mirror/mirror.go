package mirror

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/btcsuite/btcd/wire/v2"

	"github.com/paulscode/forktower/internal/store"
)

// writeTimeout bounds the writes that record an attempt.
//
// Detached from the caller's context, like everywhere else that records
// something observed: a shutdown arriving between a broadcast and the record of
// it must not lose the record. Losing it would mean re-broadcasting on the next
// start with the attempt count reset, which is how a transaction gets retried
// forever without anybody being told.
const writeTimeout = 5 * time.Second

// Broadcaster is the part of a chain backend the mirror needs.
//
// One method, and deliberately the narrowest one: this package offers bytes to a
// chain and does nothing else to it.
type Broadcaster interface {
	Broadcast(ctx context.Context, tx *wire.MsgTx) error
}

// Outcome is what became of one attempt.
type Outcome struct {
	TxID  string
	State store.MirrorState
	// Rejection is why the chain refused it, empty when it did not.
	Rejection Rejection
	// Note is the sentence for the user. Present whenever something went wrong,
	// and empty when the transaction was simply accepted.
	Note string
}

// Mirror offers the transactions the policy allowed to the other chain.
//
// **It never constructs or signs anything.** Everything it sends is bytes it
// observed on one chain, unchanged. That is not caution for its own sake: it is
// what makes the whole arm safe to run unattended, because a program that cannot
// build a transaction cannot build a wrong one.
//
// It also cannot help a transaction along. No fee bump, no child-pays-for-parent,
// no re-signing — all three need keys this daemon refuses to hold. When the other
// chain will not take something because the fee is too low, the honest answer is
// to say so and keep trying, and that is what it does.
type Mirror struct {
	store  Queue
	target Broadcaster
	log    *slog.Logger
	branch store.Branch
	now    func() time.Time
}

// Queue is the storage this reads and writes: the decisions waiting to be
// acted on, and the transactions they refer to.
//
// Named for what it is rather than for where it lives — the observer's own
// interface is the one called Store, and two things called Store in one package
// would be two things nobody could tell apart at a call site.
type Queue interface {
	ListMirrorDecisions(ctx context.Context, f store.MirrorFilter) ([]store.MirrorDecision, error)
	UpdateMirrorState(ctx context.Context, id int64, state store.MirrorState, lastError string, at int64) error
	ListSpends(ctx context.Context, f store.SpendFilter) ([]store.Spend, error)
}

// Options configures a Mirror.
type Options struct {
	Store Queue
	// Target is the chain transactions are offered to, and Branch names it.
	Target Broadcaster
	Branch store.Branch
	Log    *slog.Logger
	Now    func() time.Time
}

// New builds a Mirror.
func New(opts Options) (*Mirror, error) {
	if opts.Store == nil {
		return nil, errors.New("mirror: storage is required")
	}
	if opts.Target == nil {
		return nil, errors.New("mirror: a chain to offer transactions to is required")
	}
	if !opts.Branch.Valid() {
		return nil, fmt.Errorf("mirror: %q is not a chain", opts.Branch)
	}
	if opts.Log == nil {
		opts.Log = slog.New(discardHandler{})
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Mirror{
		store: opts.Store, target: opts.Target, log: opts.Log,
		branch: opts.Branch, now: opts.Now,
	}, nil
}

// Pass offers everything currently waiting, and returns what happened.
//
// Attempts are ordered oldest first. That matters for the one case where order
// is load-bearing: a transaction refused for a missing parent may succeed once
// the parent it follows has gone across, and the parent was necessarily seen
// first.
func (m *Mirror) Pass(ctx context.Context) ([]Outcome, error) {
	waiting, err := m.waiting(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]Outcome, 0, len(waiting))
	for i := len(waiting) - 1; i >= 0; i-- {
		d := waiting[i]
		if !m.due(d) {
			continue
		}
		outcome, attemptErr := m.attempt(ctx, d)
		if attemptErr != nil {
			// Recording an attempt failed, which is different from the attempt
			// failing. Logged and skipped: the transaction stays waiting, and the
			// next pass tries again.
			m.log.Error("could not record what happened to a copied transaction",
				slog.String("txid", d.TxID), slog.String("error", attemptErr.Error()))
			continue
		}
		out = append(out, outcome)
	}
	return out, nil
}

// waiting is everything the policy allowed and the other chain has not taken.
func (m *Mirror) waiting(ctx context.Context) ([]store.MirrorDecision, error) {
	pending, err := m.store.ListMirrorDecisions(ctx, store.MirrorFilter{
		State: store.MirrorPending, TargetBranch: m.branch,
	})
	if err != nil {
		return nil, fmt.Errorf("reading what is waiting to be copied: %w", err)
	}
	rejected, err := m.store.ListMirrorDecisions(ctx, store.MirrorFilter{
		State: store.MirrorRejected, TargetBranch: m.branch,
	})
	if err != nil {
		return nil, fmt.Errorf("reading what the other chain refused: %w", err)
	}
	return append(pending, rejected...), nil
}

// due reports whether enough time has passed to try again.
func (m *Mirror) due(d store.MirrorDecision) bool {
	if d.Attempts == 0 {
		return true
	}
	return m.now().Unix() >= d.UpdatedAt+int64(Backoff(d.Attempts).Seconds())
}

// attempt offers one transaction and records what came back.
func (m *Mirror) attempt(ctx context.Context, d store.MirrorDecision) (Outcome, error) {
	raw, err := m.rawFor(ctx, d)
	if err != nil {
		// The bytes are gone, which is not something retrying fixes.
		return m.record(ctx, d, Outcome{
			TxID: d.TxID, State: store.MirrorAbandoned,
			Note: "Forktower no longer has the transaction it was going to copy, " +
				"so it cannot copy it.",
		}, err.Error())
	}

	if err := m.target.Broadcast(ctx, raw); err == nil {
		return m.record(ctx, d, Outcome{
			TxID: d.TxID, State: store.MirrorAccepted,
		}, "")
	} else { //nolint:revive // the error is the subject of everything below
		rejection := Classify(err)
		state := store.MirrorRejected
		note := rejection.Explain(err.Error())

		// Give up when trying again cannot help, or when it has been tried enough
		// that saying so is worth more than trying once more. Either way the
		// transaction is still the user's problem, which is why this state exists
		// to be alerted on rather than to close the matter.
		if !rejection.Retriable() || d.Attempts+1 >= MaxAttempts {
			state = store.MirrorAbandoned
			note = rejection.Explain(err.Error()) +
				" Forktower has stopped trying to copy it."
		}
		return m.record(ctx, d, Outcome{
			TxID: d.TxID, State: state, Rejection: rejection, Note: note,
		}, err.Error())
	}
}

// record writes the outcome, so that a restart does not lose the attempt.
func (m *Mirror) record(
	ctx context.Context, d store.MirrorDecision, outcome Outcome, lastError string,
) (Outcome, error) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()

	if err := m.store.UpdateMirrorState(
		writeCtx, d.ID, outcome.State, lastError, m.now().Unix()); err != nil {
		return Outcome{}, err
	}
	return outcome, nil
}

// rawFor finds the bytes to send.
//
// Read back from the spend record rather than held in memory, because a
// transaction may wait a long time between being decided about and being
// accepted — across restarts, and across a fee market changing. The store has
// kept the raw transaction since M2 for exactly this.
func (m *Mirror) rawFor(ctx context.Context, d store.MirrorDecision) (*wire.MsgTx, error) {
	spends, err := m.store.ListSpends(ctx, store.SpendFilter{
		Branch: d.SourceBranch, ChannelID: d.ChannelID, Limit: store.MaxSpendLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("reading the transaction to copy: %w", err)
	}
	for _, sp := range spends {
		if sp.SpendTxID != d.TxID || sp.SpendTxHex == "" {
			continue
		}
		body, decodeErr := hex.DecodeString(sp.SpendTxHex)
		if decodeErr != nil {
			return nil, fmt.Errorf("the stored transaction %s is not readable: %w",
				d.TxID, decodeErr)
		}
		tx := wire.NewMsgTx(2)
		if err := tx.Deserialize(bytes.NewReader(body)); err != nil {
			return nil, fmt.Errorf("the stored transaction %s is not a transaction: %w",
				d.TxID, err)
		}
		return tx, nil
	}
	return nil, fmt.Errorf("the transaction %s is no longer stored", d.TxID)
}

// WhyNoPackageRelay explains an absence, so that nobody re-adds it by reflex.
//
// Doc 05 §2 asks for package submission when a transaction is refused for a
// missing parent, so that a parent below the chain's fee floor can be carried by
// its child. It is not implemented, and the reason is that **there is no pair we
// could ever put in a package**:
//
//   - A cooperative close has no parent to submit with.
//   - Our own force-close and its sweep are separated by the channel's delay, so
//     the sweep is not valid at the moment the commitment is, and the two can
//     never be in one package.
//   - A justice transaction's parent is the counterparty's commitment, which the
//     policy refuses to copy. If that commitment is on the other chain at all, it
//     is because they put it there, and the parent is already present.
//
// Package relay would help only if Forktower could add a paying child, and it
// cannot: that needs keys. So a missing parent is reported rather than worked
// around, which is also the more useful thing to tell somebody — it means the
// close they are waiting on has not reached the other chain.
const WhyNoPackageRelay = "no parent-and-child pair the mirror handles can be " +
	"submitted together: they are either separated by a channel delay or the " +
	"parent is one the policy refuses to copy"
