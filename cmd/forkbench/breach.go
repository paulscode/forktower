package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// snapshotPath is where a saved channel state is kept.
//
// In a volume of its own, mounted into the counterparty's container. Not on the
// host, because the thing being saved is a database LND is using and the only
// safe way to copy it is from the same filesystem while LND is stopped; and not
// in the data directory, because writing the copy into the directory being
// copied is a knot nobody needs. Its own volume also means `forkbench down`
// takes it away with everything else.
const snapshotPath = "/snapshot/state.tar"

// lndDataDir is what gets saved and put back: the whole of LND's state,
// including the channel database that records which commitments it has promised
// not to publish.
const lndDataDir = "/root/.lnd"

// helperImage is what the copying is done in. Anything with tar; pinned so a
// world built today and a world built next year behave the same.
const helperImage = "alpine:3.20"

// commandSnapshotMallory saves the counterparty's current channel state.
//
// LND is stopped first. Copying a database that is being written to produces a
// file that may or may not restore, and a test world that fails one run in five
// for that reason is worse than no test world.
func commandSnapshotMallory(ctx context.Context) error {
	mallory, err := lnByName(lnMallory)
	if err != nil {
		return err
	}

	say("Stopping the counterparty so its state can be copied cleanly…")
	if err := composeQuiet(ctx, "stop", mallory.service); err != nil {
		return err
	}

	container, err := containerName(ctx, mallory.service)
	if err != nil {
		return err
	}

	say("Saving its channel state…")
	if err := dockerRun(ctx, "run", "--rm",
		"--volumes-from", container, helperImage,
		"tar", "-cf", snapshotPath, "-C", lndDataDir, "."); err != nil {
		return fmt.Errorf("saving the counterparty's state: %w", err)
	}

	say("Starting it again…")
	if err := composeQuiet(ctx, "start", mallory.service); err != nil {
		return err
	}
	if err := mallory.waitSynced(ctx, lnReadyTimeout); err != nil {
		return err
	}

	say("Saved. Make some payments, then `forkbench breach` to publish this state.")
	return nil
}

// commandRestoreMallory puts the counterparty back to the saved state.
//
// This is the whole trick, and it is worth being plain about what it does: LND
// is given back a channel database from before some payments happened, so it now
// believes a commitment it has since revoked is still current. Everything about
// Lightning's punishment mechanism exists to make this a losing move, and this
// tool exists to check that Forktower notices when somebody tries it anyway.
//
// The user's node is stopped for the duration. Left running, it would tell the
// counterparty on reconnection that its state is stale, and LND would correctly
// refuse to publish anything — which is the protocol working, and is not the
// scenario being staged.
func commandRestoreMallory(ctx context.Context) error {
	mallory, err := lnByName(lnMallory)
	if err != nil {
		return err
	}
	user, err := lnByName(lnUser)
	if err != nil {
		return err
	}

	say("Stopping both Lightning nodes…")
	if err := composeQuiet(ctx, "stop", user.service, mallory.service); err != nil {
		return err
	}

	say("Putting the counterparty back to the saved state…")
	container, err := containerName(ctx, mallory.service)
	if err != nil {
		return err
	}
	// The saved state is checked before anything is deleted, and this is not
	// fussiness. The first version of this ran `rm -rf` and the extraction as one
	// `&&` chain: the delete succeeded, the extraction failed because the file was
	// somewhere else, and the counterparty was left with no identity, no wallet
	// and no channel. A restore that can destroy what it was meant to replace is
	// worse than one that refuses.
	if err := dockerRun(ctx, "run", "--rm",
		"--volumes-from", container, helperImage,
		"tar", "-tf", snapshotPath,
	); err != nil {
		return fmt.Errorf(
			"there is no saved state to go back to — run `forkbench snapshot-mallory` "+
				"first, and note that nothing has been changed: %w", err)
	}
	if err := dockerRun(ctx, "run", "--rm",
		"--volumes-from", container, helperImage,
		"sh", "-c", "rm -rf "+lndDataDir+"/* && tar -xf "+snapshotPath+" -C "+lndDataDir,
	); err != nil {
		return fmt.Errorf("restoring the counterparty's state: %w", err)
	}

	say("Starting the counterparty, alone…")
	if err := composeQuiet(ctx, "start", mallory.service); err != nil {
		return err
	}
	if err := mallory.waitSynced(ctx, lnReadyTimeout); err != nil {
		return err
	}
	say("Restored. The counterparty now believes an old commitment is current.")
	return nil
}

// commandBreach stages the attack this whole project is about.
//
// The counterparty is put back to an old state, made to publish the commitment
// it had already promised not to, and that transaction is then pushed onto one
// chain only. On the chain the user's own node follows, nothing happened — which
// is exactly what makes this dangerous, and exactly what Forktower is for.
func commandBreach(ctx context.Context, branch, fixtureDir string) error {
	target, err := nodeByName(branch)
	if err != nil {
		return err
	}
	mallory, err := lnByName(lnMallory)
	if err != nil {
		return err
	}
	user, err := lnByName(lnUser)
	if err != nil {
		return err
	}

	if err := commandRestoreMallory(ctx); err != nil {
		return err
	}

	channels, err := mallory.channels(ctx)
	if err != nil {
		return err
	}
	if len(channels) == 0 {
		return errors.New("the counterparty has no channel to breach; run `forkbench ln-up` first")
	}
	channelPoint := channels[0].ChannelPoint
	fundingTxid, fundingVout, err := channels[0].fundingOutpoint()
	if err != nil {
		return err
	}

	say("Making the counterparty publish its old commitment…")
	// The force close is asked for and not waited on: lncli blocks until the
	// close confirms, and the whole point here is that it must not confirm on
	// this chain.
	if err := mallory.lncliBackground(ctx, "closechannel", "--force",
		"--funding_txid="+fundingTxid,
		"--output_index="+fmt.Sprint(fundingVout)); err != nil {
		return err
	}

	sf := newClient(nodes()[0].rpcURL)
	commitmentTxid, err := waitForSpendInMempool(ctx, sf, fundingTxid, fundingVout, mempoolWaitLimit)
	if err != nil {
		return fmt.Errorf("the counterparty's commitment never appeared: %w", err)
	}
	raw, err := rawTransaction(ctx, sf, commitmentTxid)
	if err != nil {
		return err
	}
	say("Captured the commitment: %s", commitmentTxid)

	// Onto the other chain, and only the other chain. The two nodes are not
	// peered after a split, so nothing crosses on its own.
	say("Publishing it on the %s chain only…", target.name)
	other := newClient(target.rpcURL)
	if err := other.call(ctx, "sendrawtransaction", []any{raw}, nil); err != nil {
		return fmt.Errorf("publishing the commitment on %s: %w", target.name, err)
	}
	if _, err := other.mine(ctx, 1); err != nil {
		return err
	}

	if fixtureDir != "" {
		if err := writeFixture(fixtureDir, "force_close_commitment.hex", raw); err != nil {
			return err
		}
	}

	// The user's node comes back up. It has no idea any of this happened, which
	// is the situation Forktower exists to notice.
	say("Starting the user's node again…")
	if err := composeQuiet(ctx, "start", user.service); err != nil {
		return err
	}

	say("")
	say("Done. Channel %s now has a revoked commitment confirmed on the %s chain,",
		channelPoint, target.name)
	say("and nothing at all on the %s chain. Forktower should be reporting a",
		nodes()[0].name)
	say("confirmed commitment it cannot attribute.")
	return nil
}

// commandCoopClose closes the channel the agreeable way, which is the shape
// Forktower must *not* treat as an attack.
func commandCoopClose(ctx context.Context, fixtureDir string) error {
	user, err := lnByName(lnUser)
	if err != nil {
		return err
	}
	channels, err := user.channels(ctx)
	if err != nil {
		return err
	}
	if len(channels) == 0 {
		return errors.New("there is no channel to close; run `forkbench ln-up` first")
	}
	fundingTxid, fundingVout, err := channels[0].fundingOutpoint()
	if err != nil {
		return err
	}

	say("Closing the channel by agreement…")
	if err := user.lncliBackground(ctx, "closechannel",
		"--funding_txid="+fundingTxid,
		"--output_index="+fmt.Sprint(fundingVout)); err != nil {
		return err
	}

	sf := newClient(nodes()[0].rpcURL)
	txid, err := waitForSpendInMempool(ctx, sf, fundingTxid, fundingVout, mempoolWaitLimit)
	if err != nil {
		return fmt.Errorf("the closing transaction never appeared: %w", err)
	}
	if fixtureDir != "" {
		raw, rawErr := rawTransaction(ctx, sf, txid)
		if rawErr != nil {
			return rawErr
		}
		if err := writeFixture(fixtureDir, "coop_close.hex", raw); err != nil {
			return err
		}
	}
	if _, err := sf.mine(ctx, 1); err != nil {
		return err
	}
	say("Closed by agreement: %s", txid)
	return nil
}

// commandForceClose closes the channel unilaterally, from a node that is *not*
// cheating — the current commitment, published honestly.
//
// Worth having distinct from the breach: it produces the same shape on the chain
// as a breach does, which is precisely why Forktower cannot tell them apart
// without help, and why it says `commitment_unknown` rather than guessing.
func commandForceClose(ctx context.Context, nodeName, fixtureDir string) error {
	n, err := lnByName(nodeName)
	if err != nil {
		return err
	}
	channels, err := n.channels(ctx)
	if err != nil {
		return err
	}
	if len(channels) == 0 {
		return fmt.Errorf("%s has no channel to close; run `forkbench ln-up` first", n.name)
	}
	fundingTxid, fundingVout, err := channels[0].fundingOutpoint()
	if err != nil {
		return err
	}

	say("Making %s force-close…", n.name)
	if err := n.lncliBackground(ctx, "closechannel", "--force",
		"--funding_txid="+fundingTxid,
		"--output_index="+fmt.Sprint(fundingVout)); err != nil {
		return err
	}

	sf := newClient(nodes()[0].rpcURL)
	txid, err := waitForSpendInMempool(ctx, sf, fundingTxid, fundingVout, mempoolWaitLimit)
	if err != nil {
		return fmt.Errorf("the commitment never appeared: %w", err)
	}
	if fixtureDir != "" {
		raw, rawErr := rawTransaction(ctx, sf, txid)
		if rawErr != nil {
			return rawErr
		}
		if err := writeFixture(fixtureDir, "force_close_"+n.name+".hex", raw); err != nil {
			return err
		}
	}
	if _, err := sf.mine(ctx, 1); err != nil {
		return err
	}
	say("Force-closed by %s: %s", n.name, txid)
	return nil
}

// lncliBackground asks for something and does not wait for it to finish.
//
// `closechannel` blocks until the close confirms, and a breach must not confirm
// on the chain it was broadcast to. Started detached, and the transaction is
// then found in the memory pool rather than in the command's answer.
func (n lnNode) lncliBackground(ctx context.Context, args ...string) error {
	file, err := composeFile()
	if err != nil {
		return err
	}
	full := append([]string{
		composeVerb, "-f", file, "exec", "-d", n.service,
		"lncli", "--network=regtest",
	}, args...)

	//nolint:gosec // G204: the arguments are this tool's own
	cmd := exec.CommandContext(ctx, "docker", full...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: lncli %s: %w: %s",
			n.name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// waitForSpendInMempool watches a node's memory pool for whatever spends an
// outpoint, and returns that transaction's id.
//
// Found by what it spends rather than by taking whatever turned up, because more
// than one thing can be in a memory pool and picking the wrong one would make
// every later assertion meaningless.
func waitForSpendInMempool(
	ctx context.Context, c *client, txid string, vout uint32, timeout time.Duration,
) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		var pool []string
		if err := c.call(ctx, "getrawmempool", nil, &pool); err != nil {
			return "", err
		}
		for _, candidate := range pool {
			spends, err := spendsOutpoint(ctx, c, candidate, txid, vout)
			if err != nil {
				continue
			}
			if spends {
				return candidate, nil
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("nothing spending %s:%d appeared within %s", txid, vout, timeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(lnPollInterval):
		}
	}
}

// spendsOutpoint reports whether a transaction spends a particular output.
func spendsOutpoint(
	ctx context.Context, c *client, candidate, txid string, vout uint32,
) (bool, error) {
	var decoded struct {
		Vin []struct {
			TxID string `json:"txid"`
			Vout uint32 `json:"vout"`
		} `json:"vin"`
	}
	if err := c.call(ctx, "getrawtransaction", []any{candidate, true}, &decoded); err != nil {
		return false, err
	}
	for _, in := range decoded.Vin {
		if in.TxID == txid && in.Vout == vout {
			return true, nil
		}
	}
	return false, nil
}

// rawTransaction fetches a transaction as bytes, which is what the other chain
// has to be given.
func rawTransaction(ctx context.Context, c *client, txid string) (string, error) {
	var raw string
	if err := c.call(ctx, "getrawtransaction", []any{txid}, &raw); err != nil {
		return "", err
	}
	return raw, nil
}

// writeFixture saves a transaction where the classifier's tests can read it.
//
// The reason this exists: the shape classifier is tested against transactions
// this project builds, which proves the rules are implemented but not that they
// match what LND actually broadcasts. These are the real thing.
func writeFixture(dir, name, raw string) error {
	// The directory is where the operator running this tool asked for the
	// fixtures. That it is under their control is the whole point of the flag.
	//nolint:gosec // G703: an operator-supplied output path, by design
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("making room for fixtures: %w", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(raw+"\n"), 0o644); err != nil { //nolint:gosec // a test fixture
		return fmt.Errorf("writing %s: %w", path, err)
	}
	say("Wrote %s", path)
	return nil
}

// composeQuiet runs a compose command without letting its chatter drown the
// narration these commands are built around.
func composeQuiet(ctx context.Context, args ...string) error {
	file, err := composeFile()
	if err != nil {
		return err
	}
	full := append([]string{composeVerb, "-f", file}, args...)
	return dockerRun(ctx, full...)
}

// dockerRun runs docker quietly, and says what it printed only when it failed.
//
// Quiet because these commands run inside narrated steps: docker's own progress
// chatter in the middle of "Putting the counterparty back to the saved state…"
// makes the story harder to follow, and its error output is the only part anyone
// wants.
func dockerRun(ctx context.Context, args ...string) error {
	//nolint:gosec // G204: the arguments are this tool's own
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker %s: %w: %s",
			args[0], err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
