package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Lightning node names, as they appear on the command line.
const (
	lnUser    = "user"
	lnMallory = "mallory"
)

// Channel and payment sizes for the staged world.
const (
	// channelSat is the channel opened by `ln-up`. A million satoshis is large
	// enough that a hundred payments barely move it and small enough that a
	// regtest wallet funds it without ceremony.
	channelSat = 1_000_000
	// fundingSat is what each node is given before it opens anything.
	fundingSat = 10_000_000
	// paymentSat is one payment. Small on purpose: the point of paying is to
	// advance the channel state, not to move money.
	paymentSat = 1_000
)

const (
	lnReadyTimeout   = 120 * time.Second
	lnActionTimeout  = 90 * time.Second
	lnPollInterval   = 500 * time.Millisecond
	mempoolWaitLimit = 60 * time.Second
)

// lnNode is one Lightning node in the world.
type lnNode struct {
	name    string
	service string
}

func lnNodes() []lnNode {
	return []lnNode{
		{name: lnUser, service: "lnd-user"},
		{name: lnMallory, service: "lnd-mallory"},
	}
}

func lnByName(name string) (lnNode, error) {
	for _, n := range lnNodes() {
		if n.name == name {
			return n, nil
		}
	}
	// The watchtower is not one of the two channel partners, so it is not in
	// lnNodes() — funding it or opening channels to it would be nonsense. It is
	// still an LND this tool talks to, and credentials have to come out of it the
	// same way.
	if name == lnTower.name {
		return lnTower, nil
	}
	return lnNode{}, fmt.Errorf("no Lightning node called %q; use %q, %q or %q",
		name, lnUser, lnMallory, lnTower.name)
}

// lncli runs a command inside a node's container.
//
// Through the container's own client rather than over the network, because that
// is the one path that needs no certificate wrangling, no macaroon copying and
// no port mapping to get right. This is a development tool driving a throwaway
// world; the parts of it that could go wrong should be the parts being tested.
func (n lnNode) lncli(ctx context.Context, out any, args ...string) error {
	file, err := composeFile()
	if err != nil {
		return err
	}
	full := append([]string{
		composeVerb, "-f", file, "exec", "-T", n.service,
		"lncli", "--network=regtest",
	}, args...)

	// Fixed command; the only variable arguments are this tool's own.
	//nolint:gosec // G204: the arguments are this tool's own
	cmd := exec.CommandContext(ctx, "docker", full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: lncli %s: %w: %s",
			n.name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(stdout.Bytes(), out); err != nil {
		return fmt.Errorf("%s: reading the answer to `lncli %s`: %w",
			n.name, strings.Join(args, " "), err)
	}
	return nil
}

// lnInfo is the part of getinfo this tool needs.
type lnInfo struct {
	Pubkey        string `json:"identity_pubkey"`
	SyncedToChain bool   `json:"synced_to_chain"`
	BlockHeight   int32  `json:"block_height"`
}

func (n lnNode) info(ctx context.Context) (lnInfo, error) {
	var out lnInfo
	err := n.lncli(ctx, &out, "getinfo")
	return out, err
}

// waitSynced waits until a node has caught up with the chain.
//
// Every other command depends on this: a node that has not seen the chain will
// refuse to open a channel, and will do so with an error that says nothing about
// why.
func (n lnNode) waitSynced(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		info, err := n.info(ctx)
		if err == nil && info.SyncedToChain {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("%s did not become ready within %s: %w", n.name, timeout, err)
			}
			return fmt.Errorf("%s did not catch up with the chain within %s", n.name, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(lnPollInterval):
		}
	}
}

// newAddress asks a node for somewhere to be paid.
func (n lnNode) newAddress(ctx context.Context) (string, error) {
	var out struct {
		Address string `json:"address"`
	}
	if err := n.lncli(ctx, &out, "newaddress", "p2wkh"); err != nil {
		return "", err
	}
	return out.Address, nil
}

// lnChannel is one channel as lncli reports it.
type lnChannel struct {
	Active       bool   `json:"active"`
	RemotePubkey string `json:"remote_pubkey"`
	ChannelPoint string `json:"channel_point"`
	Capacity     string `json:"capacity"`
	LocalBalance string `json:"local_balance"`
}

func (n lnNode) channels(ctx context.Context) ([]lnChannel, error) {
	var out struct {
		Channels []lnChannel `json:"channels"`
	}
	err := n.lncli(ctx, &out, "listchannels")
	return out.Channels, err
}

// fundingOutpoint splits a channel point into its two halves.
func (c lnChannel) fundingOutpoint() (txid string, vout uint32, err error) {
	parts := strings.SplitN(c.ChannelPoint, ":", 2)
	if len(parts) != 2 {
		return "", 0, fmt.Errorf("%q is not a channel point", c.ChannelPoint)
	}
	index, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return "", 0, fmt.Errorf("%q is not a channel point: %w", c.ChannelPoint, err)
	}
	return parts[0], uint32(index), nil
}

// commandLNUp brings the Lightning layer up: two nodes, funded, with a channel
// between them.
//
// Idempotent, like `up`. Running it against a world that already has the channel
// says so and stops, rather than opening a second one — a scripted scenario
// re-run after a failure should not quietly end up with two channels and
// ambiguous assertions.
func commandLNUp(ctx context.Context) error {
	user, mallory := lnNodes()[0], lnNodes()[1]

	if err := freshenChain(ctx); err != nil {
		return err
	}

	say("Waiting for both Lightning nodes to catch up with the chain…")
	for _, n := range lnNodes() {
		if err := n.waitSynced(ctx, lnReadyTimeout); err != nil {
			return err
		}
	}

	existing, err := user.channels(ctx)
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		say("There is already a channel: %s", existing[0].ChannelPoint)
		return nil
	}

	if err := fundBothNodes(ctx); err != nil {
		return err
	}
	if err := connectLightningNodes(ctx); err != nil {
		return err
	}

	mallInfo, err := mallory.info(ctx)
	if err != nil {
		return err
	}

	say("Opening a %d-satoshi channel from user to mallory…", channelSat)
	// Balanced on both sides, because a channel with nothing on the far side
	// cannot be paid *back* — and a breach only means anything when the
	// counterparty has a state worth reverting to.
	var opened struct {
		FundingTxid string `json:"funding_txid"`
	}
	if err := user.lncli(ctx, &opened, "openchannel",
		"--node_key="+mallInfo.Pubkey,
		"--local_amt="+strconv.Itoa(channelSat),
		"--push_amt="+strconv.Itoa(channelSat/2),
	); err != nil {
		return err
	}

	say("Confirming it…")
	if _, err := newClient(nodes()[0].rpcURL).mine(ctx, 6); err != nil {
		return err
	}
	if err := waitForActiveChannel(ctx, user, lnActionTimeout); err != nil {
		return err
	}

	channels, err := user.channels(ctx)
	if err != nil {
		return err
	}
	say("Ready. Channel %s is open and active.", channels[0].ChannelPoint)
	return nil
}

// staleTipAge is how old the chain's tip may be before LND stops believing it
// has caught up.
//
// LND decides it is synced by comparing the tip's timestamp with the clock, and
// on a real chain that is exactly right. On a regtest world that has been
// sitting idle since yesterday it is a trap: everything is healthy, every node
// agrees, and LND reports `synced_to_chain: false` forever with nothing in its
// log to say why. Two hours is LND's own threshold; one hour leaves room.
const staleTipAge = time.Hour

// freshenChain mines a block if the tip is old enough that LND would refuse to
// call itself synced.
//
// Costs one block and saves whoever comes back to a world they left running an
// afternoon of looking for a fault that is not there.
func freshenChain(ctx context.Context) error {
	sf := newClient(nodes()[0].rpcURL)
	info, err := sf.chainInfo(ctx)
	if err != nil {
		return err
	}
	age, err := sf.tipAge(ctx, info.Best)
	if err != nil {
		return err
	}
	if age < staleTipAge {
		return nil
	}

	say("The chain has been idle for %s, which LND reads as not yet caught up. "+
		"Mining a block…", age.Round(time.Minute))
	if _, err := sf.mine(ctx, 1); err != nil {
		return err
	}
	return nil
}

// fundBothNodes gives each Lightning node coins to work with.
func fundBothNodes(ctx context.Context) error {
	sf := newClient(nodes()[0].rpcURL).wallet()

	for _, n := range lnNodes() {
		address, err := n.newAddress(ctx)
		if err != nil {
			return err
		}
		say("Funding %s…", n.name)
		if err := sf.call(ctx, "sendtoaddress",
			[]any{address, float64(fundingSat) / 1e8}, nil); err != nil {
			return fmt.Errorf("funding %s: %w", n.name, err)
		}
	}

	// Mined so the funds are spendable, and enough of them that the coinbase
	// they came from has matured.
	if _, err := newClient(nodes()[0].rpcURL).mine(ctx, 6); err != nil {
		return err
	}
	for _, n := range lnNodes() {
		if err := n.waitSynced(ctx, lnActionTimeout); err != nil {
			return err
		}
	}
	return nil
}

// connectLightningNodes makes the two nodes peers.
func connectLightningNodes(ctx context.Context) error {
	user, mallory := lnNodes()[0], lnNodes()[1]

	mallInfo, err := mallory.info(ctx)
	if err != nil {
		return err
	}
	err = user.lncli(ctx, nil, "connect", mallInfo.Pubkey+"@lnd-mallory:9735")
	if err != nil && !strings.Contains(err.Error(), "already connected") {
		return err
	}
	return nil
}

// waitForActiveChannel waits until a node has a channel it can actually use.
func waitForActiveChannel(ctx context.Context, n lnNode, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		channels, err := n.channels(ctx)
		if err == nil {
			for _, c := range channels {
				if c.Active {
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s had no usable channel within %s", n.name, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(lnPollInterval):
		}
	}
}

// commandPay sends payments from the user to mallory, which is how the channel
// is advanced past a state somebody might later revert to.
func commandPay(ctx context.Context, times int) error {
	if times < 1 {
		return errors.New("say how many payments to make")
	}
	user, mallory := lnNodes()[0], lnNodes()[1]

	if err := waitForActiveChannel(ctx, user, lnActionTimeout); err != nil {
		return err
	}

	for i := 1; i <= times; i++ {
		var invoice struct {
			PaymentRequest string `json:"payment_request"`
		}
		if err := mallory.lncli(ctx, &invoice, "addinvoice",
			"--amt="+strconv.Itoa(paymentSat)); err != nil {
			return err
		}
		if err := user.lncli(ctx, nil, "payinvoice", "--force",
			"--json", invoice.PaymentRequest); err != nil {
			return fmt.Errorf("payment %d of %d: %w", i, times, err)
		}
		say("Paid %d of %d.", i, times)
	}
	return nil
}

// commandLNStatus shows what the Lightning layer is doing.
func commandLNStatus(ctx context.Context) error {
	for _, n := range lnNodes() {
		info, err := n.info(ctx)
		if err != nil {
			say("%s: not answering (%v)", n.name, err)
			continue
		}
		channels, err := n.channels(ctx)
		if err != nil {
			return err
		}
		say("%s: %s at height %d, %d channel(s)",
			n.name, short(info.Pubkey), info.BlockHeight, len(channels))
		for _, c := range channels {
			state := "inactive"
			if c.Active {
				state = "active"
			}
			say("    %s  %s  capacity %s, ours %s",
				c.ChannelPoint, state, c.Capacity, c.LocalBalance)
		}
	}
	return nil
}

// containerName resolves a compose service to the container docker knows it by,
// which is what the stop/start and tar commands need.
func containerName(ctx context.Context, service string) (string, error) {
	file, err := composeFile()
	if err != nil {
		return "", err
	}
	// `-a`, because the container this is asked about is usually a stopped one:
	// the state being copied belongs to a node that was shut down precisely so it
	// would hold still. Without it this returns nothing and the caller gets a
	// docker error about an empty argument instead of the truth.
	//nolint:gosec // G204: the arguments are this tool's own
	cmd := exec.CommandContext(ctx, "docker", composeVerb, "-f", file, "ps", "-aq", service)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("finding the %s container: %w", service, err)
	}
	id := strings.TrimSpace(stdout.String())
	if id == "" {
		return "", fmt.Errorf("there is no %s container running", service)
	}
	return id, nil
}

// credentialFiles are what a client outside the container needs: the certificate
// that identifies the node, and a macaroon that may only read.
//
// The read-only macaroon specifically. Forktower never sends a Lightning node an
// instruction and has no code that could, so handing it the admin credential to
// save a step would be demonstrating the wrong habit in the one place people
// copy from.
var credentialFiles = map[string]string{
	"tls.cert":          "/root/.lnd/tls.cert",
	"readonly.macaroon": "/root/.lnd/data/chain/bitcoin/regtest/readonly.macaroon",
}

// commandLNCredentials copies a node's certificate and read-only macaroon out of
// its container, so a Forktower running on the host can read its channels.
func commandLNCredentials(ctx context.Context, nodeName, outDir string) error {
	n, err := lnByName(nodeName)
	if err != nil {
		return err
	}
	if outDir == "" {
		return errors.New("say where to put them with -out")
	}
	container, err := containerName(ctx, n.service)
	if err != nil {
		return err
	}

	target := filepath.Join(outDir, n.name)
	// The directory is where the operator asked for them, which is the point of
	// the flag.
	//nolint:gosec // G703: an operator-supplied output path, by design
	if err := os.MkdirAll(target, 0o750); err != nil {
		return fmt.Errorf("making room for the credentials: %w", err)
	}

	for name, inside := range credentialFiles {
		dest := filepath.Join(target, name)
		if err := dockerRun(ctx, "cp", container+":"+inside, dest); err != nil {
			return fmt.Errorf("copying %s out of %s: %w", name, n.name, err)
		}
		// A credential is a credential even in a throwaway world, and a file mode
		// people copy from should be the one they should be copying.
		//nolint:gosec // G703: under the operator-supplied output path, by design
		if err := os.Chmod(dest, 0o600); err != nil {
			return fmt.Errorf("securing %s: %w", dest, err)
		}
	}

	say("Wrote %s/tls.cert and %s/readonly.macaroon", target, target)
	say("Point Forktower's [[ln.lnd]] section at those, with "+
		"rest_addr = \"https://127.0.0.1:%s\"", restPortOf(n.name))
	return nil
}

// restPortOf is where each node's REST interface is published on the host.
func restPortOf(name string) string {
	switch name {
	case lnMallory:
		return "8082"
	case lnTower.name:
		return "8083"
	default:
		return "8081"
	}
}
