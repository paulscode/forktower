// Command forkbench builds a small world for Forktower to be tested against:
// two Bitcoin nodes that can be made to disagree on demand.
//
// It is a development tool. Nothing here belongs anywhere near real money, and
// the credentials it uses are written into the compose file for exactly that
// reason.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// version is set at build time via -ldflags. "dev" when built without it.
var version = "dev"

const usage = `forkbench — a two-node world for testing Forktower

Usage:
  forkbench up                     start both nodes and give them a chain
  forkbench split                  make the two nodes disagree, permanently
  forkbench mine -node sf -blocks 6  add blocks to one chain
  forkbench status                 show where both chains are
  forkbench down                   remove the world and its state

Lightning:
  forkbench ln-up                  two Lightning nodes with a channel between them
  forkbench ln-status              show the Lightning nodes and their channels
  forkbench ln-credentials -out D  copy a node's certificate and read-only
                                   macaroon out, so Forktower can read it
  forkbench pay -times N           send N payments, advancing the channel state
  forkbench snapshot-mallory       save the counterparty's channel state
  forkbench restore-mallory        put the counterparty back to that state
  forkbench breach -branch sq      publish the counterparty's old commitment,
                                   on one chain only
  forkbench coop-close             close the channel the agreeable way
  forkbench force-close -node user close it unilaterally

Flags:
  -forktower URL   ask a running Forktower what it makes of the world
                   (default http://127.0.0.1:8330; empty to skip)
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "forkbench: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		//nolint:forbidigo // a command's own help, not a log line
		fmt.Print(usage)
		return errors.New("say what to do")
	}

	command, rest := args[0], args[1:]

	flags := flag.NewFlagSet("forkbench "+command, flag.ContinueOnError)
	var (
		forktowerURL = flags.String("forktower", "http://127.0.0.1:8330",
			"base URL of a running Forktower, or empty to skip")
		nodeName = flags.String("node", nodeSF, "which chain to mine on: sf or sq")
		blocks   = flags.Int("blocks", 1, "how many blocks to mine")
		times    = flags.Int("times", 1, "how many payments to send")
		branch   = flags.String("branch", nodeSQ, "which chain to publish on: sf or sq")
		lnName   = flags.String("ln-node", lnUser, "which Lightning node: user or mallory")
		outDir   = flags.String("fixtures", "", "write the transactions seen to this directory")
		credsDir = flags.String("out", "", "where to write a node's credentials")
	)
	if err := flags.Parse(rest); err != nil {
		return err
	}

	// Cancelled on interrupt, so a compose command that is mid-pull stops when
	// asked rather than leaving half a world behind.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch command {
	case "up":
		return commandUp(ctx)
	case "split":
		return commandSplit(ctx)
	case "mine":
		return commandMine(ctx, *nodeName, *blocks)
	case "status":
		return commandStatus(ctx, *forktowerURL)
	case "down":
		return commandDown(ctx)
	case "ln-up":
		return commandLNUp(ctx)
	case "ln-status":
		return commandLNStatus(ctx)
	case "ln-credentials":
		return commandLNCredentials(ctx, *lnName, *credsDir)
	case "pay":
		return commandPay(ctx, *times)
	case "snapshot-mallory":
		return commandSnapshotMallory(ctx)
	case "restore-mallory":
		return commandRestoreMallory(ctx)
	case "breach":
		return commandBreach(ctx, *branch, *outDir)
	case "coop-close":
		return commandCoopClose(ctx, *outDir)
	case "force-close":
		return commandForceClose(ctx, *lnName, *outDir)
	case "version", "-version", "--version":
		//nolint:forbidigo // a command's answer, not a log line
		fmt.Println("forkbench " + version)
		return nil
	case "help", "-h", "--help":
		//nolint:forbidigo // a command's own help, not a log line
		fmt.Print(usage)
		return nil
	default:
		//nolint:forbidigo // a command's own help, not a log line
		fmt.Print(usage)
		return fmt.Errorf("there is no command called %q", command)
	}
}

// say writes progress for a person watching a terminal. Not a log: this tool's
// whole output is a conversation with whoever ran it.
func say(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stdout, format+"\n", args...) //nolint:forbidigo // a tool's own output
}
