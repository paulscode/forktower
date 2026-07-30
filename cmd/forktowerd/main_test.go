package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paulscode/forktower/internal/config"
)

// A node URL may carry the node's password. Go's HTTP client puts the request
// URL in its errors, so a node that cannot be reached is the ordinary way a
// credential ends up in a terminal scrollback, a platform's log pane, and
// whatever the user pastes into a support thread.
func TestStartupFailuresCarryNoCredentials(t *testing.T) {
	t.Parallel()

	const password = "hunter2-do-not-print-me"
	dir := t.TempDir()
	path := filepath.Join(dir, "forktower.toml")

	// Nothing is listening on these ports, so startup fails while reaching the
	// node — which is the path that carries the URL.
	body := `
[sf]
rpc_url = "http://someone:` + password + `@127.0.0.1:19443"
rpc_user = "someone"
rpc_pass = "` + password + `"

[sq.bitcoind]
rpc_url = "http://127.0.0.1:19444"
rpc_user = "someone"
rpc_pass = "` + password + `"

[store]
path = "` + filepath.Join(dir, "forktower.db") + `"

[ui]
listen = "127.0.0.1:19445"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	err := run(path, false)
	if err == nil {
		t.Fatal("starting against nothing succeeded")
	}
	if strings.Contains(err.Error(), password) {
		t.Errorf("the node's password is in the message shown to the user:\n%s", err)
	}
	// And it still says something useful: a message scrubbed to nothing would
	// push people towards reading logs that are not scrubbed at all.
	if !strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("the message no longer says which node could not be reached:\n%s", err)
	}
}

// A configuration that could never work fails while someone is watching the
// terminal, not hours later.
func TestCheckConfigRefusesAnUnusableFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "forktower.toml")
	if err := os.WriteFile(path, []byte("[sf]\nrpc_url = \"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := run(path, true); err == nil {
		t.Error("a configuration with no node to talk to was accepted")
	}
}

func TestCheckConfigAcceptsAUsableFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "forktower.toml")
	body := `
[sf]
rpc_url = "http://127.0.0.1:8332"
rpc_user = "someone"
rpc_pass = "something"

[sq.bitcoind]
rpc_url = "http://127.0.0.1:8432"
rpc_user = "someone"
rpc_pass = "something"

[store]
path = "` + filepath.Join(dir, "forktower.db") + `"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	// Deliberately does not open the database or touch the network: someone
	// checking a file before deploying it should not have a database created as a
	// side effect.
	if err := run(path, true); err != nil {
		t.Fatalf("a usable configuration was refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "forktower.db")); err == nil {
		t.Error("checking the configuration created a database")
	}
}

// Every level a user can configure has to produce a working logger; falling
// through to a silent one would mean a daemon that says nothing about itself.
func TestEveryLogLevelProducesALogger(t *testing.T) {
	t.Parallel()

	levels := []config.LogLevel{
		config.LogDebug, config.LogInfo, config.LogWarn, config.LogError,
		config.LogLevel("something-else"),
	}
	for _, level := range levels {
		log := newLogger(level)
		if log == nil {
			t.Fatalf("level %q produced no logger", level)
		}
		// An unrecognised level must not silence the daemon.
		if !log.Enabled(context.Background(), slog.LevelError) {
			t.Errorf("level %q silences errors", level)
		}
	}
	if newLogger(config.LogDebug).Enabled(context.Background(), slog.LevelDebug) !=
		true {
		t.Error("debug does not enable debug")
	}
	if newLogger(config.LogError).Enabled(context.Background(), slog.LevelInfo) {
		t.Error("error level still logs info")
	}
}

// A file that is not there, and one that is not readable as configuration, both
// have to fail while someone is watching.
func TestAMissingOrUnreadableConfigurationIsRefused(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if err := run(filepath.Join(dir, "not-here.toml"), true); err == nil {
		t.Error("a configuration file that does not exist was accepted")
	}

	broken := filepath.Join(dir, "broken.toml")
	if err := os.WriteFile(broken, []byte("this is not = = toml"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(broken, true); err == nil {
		t.Error("a file that is not configuration was accepted")
	}
}
