// Command forktowerd is the Forktower daemon.
//
// It watches the chain your Bitcoin node follows and the chain it does not, and
// tells you when they stop agreeing.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/paulscode/forktower/internal/app"
	"github.com/paulscode/forktower/internal/config"
	"github.com/paulscode/forktower/internal/redact"
)

// version is set at build time via -ldflags. "dev" when built without it.
var version = "dev"

func main() {
	var (
		configPath  = flag.String("config", "", "path to forktower.toml")
		showVersion = flag.Bool("version", false, "print the version and exit")
		checkOnly   = flag.Bool("check-config", false,
			"load and validate the configuration, then exit")
	)
	flag.Parse()

	if *showVersion {
		// Stdout on purpose, and not through the logger: a script asking a binary
		// its version expects the version and nothing else.
		fmt.Println("forktowerd " + version) //nolint:forbidigo // a command's answer, not a log line
		return
	}

	if err := run(*configPath, *checkOnly); err != nil {
		// Written plainly, without a stack trace or an error code: the person
		// reading this is trying to get their node protected, not debug Go.
		fmt.Fprintln(os.Stderr, "Forktower could not start: "+err.Error())
		os.Exit(1)
	}
}

// run does the work, returning an error whose message is already safe to print.
//
// Redacted here rather than at the print, so that anything which fails during
// startup is stripped of credentials by the same code path a test can reach. A
// node that cannot be reached surfaces as Go's own HTTP error with the request
// URL in it, and that URL may carry the node's password — which would otherwise
// end up in a terminal scrollback, a platform's log pane, and whatever the user
// pastes into a support thread.
func run(configPath string, checkOnly bool) error {
	if err := start(configPath, checkOnly); err != nil {
		return errors.New(redact.Error(err))
	}
	return nil
}

func start(configPath string, checkOnly bool) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	log := newLogger(cfg.Log.Level)
	for _, warning := range cfg.Warnings {
		log.Warn("configuration", slog.String("note", warning))
	}

	if checkOnly {
		//nolint:forbidigo // a command's answer, not a log line
		fmt.Println("The configuration is usable.")
		return nil
	}

	log.Info("starting", slog.String("version", version), slog.String("config", cfg.Path))

	// Cancelled on SIGINT or SIGTERM, which is how every supervisor asks a daemon
	// to stop. Stopping the notifier restores the default handler, so a second
	// signal kills the process rather than being swallowed by a shutdown that has
	// itself got stuck.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	daemon, err := app.New(ctx, cfg, log, app.Deps{})
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := daemon.Close(); closeErr != nil {
			log.Warn("could not shut down cleanly", slog.String("error", closeErr.Error()))
		}
	}()

	if err := daemon.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func newLogger(level config.LogLevel) *slog.Logger {
	var slogLevel slog.Level
	switch level {
	case config.LogDebug:
		slogLevel = slog.LevelDebug
	case config.LogWarn:
		slogLevel = slog.LevelWarn
	case config.LogError:
		slogLevel = slog.LevelError
	case config.LogInfo:
		slogLevel = slog.LevelInfo
	default:
		slogLevel = slog.LevelInfo
	}

	// Text rather than JSON, to stderr: these logs are read by a person looking at
	// a platform's log pane, not shipped to an aggregator.
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slogLevel}))
}
