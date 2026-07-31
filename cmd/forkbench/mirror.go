package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// commandTxPresent reports whether a chain has heard of a transaction.
//
// **The assertion the mirror scenarios actually need.** Forktower's own record
// says what it decided and what the node told it; this says whether the bytes are
// really there. A test that only asked Forktower would pass just as happily
// against a mirror that recorded "accepted" and sent nothing.
//
// Counts a transaction in the memory pool as present. On a chain where nothing
// is mining, that is what "it got there" looks like, and waiting for a block
// would be waiting for a different thing.
func commandTxPresent(ctx context.Context, nodeName, txid string) error {
	if strings.TrimSpace(txid) == "" {
		return errors.New("say which transaction with -txid")
	}
	n, err := nodeByName(nodeName)
	if err != nil {
		return err
	}
	c := newClient(n.rpcURL)

	present, where, err := txPresence(ctx, c, txid)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("%s has never heard of %s", n.name, short(txid))
	}
	say("%s has %s (%s)", n.name, short(txid), where)
	return nil
}

// txPresence asks a node whether it knows a transaction, and from where.
func txPresence(ctx context.Context, c *client, txid string) (present bool, where string, err error) {
	// Verbose, because the answer tells confirmed from unconfirmed and the two
	// are different facts about whether the mirror worked.
	var raw struct {
		Confirmations int64  `json:"confirmations"`
		BlockHash     string `json:"blockhash"`
	}
	err = c.call(ctx, "getrawtransaction", []any{txid, true}, &raw)
	if err == nil {
		if raw.BlockHash != "" {
			return true, fmt.Sprintf("confirmed, %d deep", raw.Confirmations), nil
		}
		return true, "in the memory pool", nil
	}

	// A node that has never seen it says so with a specific error; anything else
	// is a problem with the question rather than an answer to it.
	if isNoSuchTransaction(err) {
		return false, "", nil
	}
	return false, "", fmt.Errorf("asking whether the transaction is there: %w", err)
}

// isNoSuchTransaction reports whether an error means "not found" rather than
// "something went wrong".
//
// Matched on the message as well as the code: Core answers a genuinely unknown
// transaction with -5, and the same code covers a few other absences, so the
// wording is what distinguishes them.
func isNoSuchTransaction(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such mempool or blockchain transaction") ||
		strings.Contains(msg, "no such mempool transaction") ||
		strings.Contains(msg, "transaction not in mempool") ||
		strings.Contains(msg, "-5")
}
