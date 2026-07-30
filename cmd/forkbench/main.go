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
