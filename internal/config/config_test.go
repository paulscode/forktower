package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// exampleConfigPath is the file shipped for self-hosters. The spec requires it to
// stay in sync with what this package accepts, so it is parsed as a test rather
// than trusted.
const exampleConfigPath = "../../deploy/compose/forktower.example.toml"

func TestExampleConfigLoads(t *testing.T) {
	t.Parallel()

	cfg, err := Load(exampleConfigPath)
	if err != nil {
		t.Fatalf("the shipped example config does not load: %v", err)
	}

	// Unknown keys are only a warning, so a silent drift between the example and
	// the structs would otherwise pass. Fail on it here.
	for _, w := range cfg.Warnings {
		if strings.Contains(w, "unknown config key") {
			t.Errorf("example config has a key this package does not know: %s", w)
		}
	}

	if cfg.SQ.Tier != TierBitcoind {
		t.Errorf("sq.tier = %q, want %q", cfg.SQ.Tier, TierBitcoind)
	}
	if cfg.UI.Auth != AuthNone {
		t.Errorf("ui.auth = %q, want %q", cfg.UI.Auth, AuthNone)
	}
	// The example deliberately leaves the fork descriptor unset so that it is
	// read from the node instead. If that changes, the reasoning in the file's
	// comments no longer matches its contents.
	if cfg.Fork.DivergenceHeightKnown() {
		t.Errorf("example config pins fork.divergence_height = %d; it should be 0 so the "+
			"value is derived from the node", cfg.Fork.DivergenceHeight)
	}
}

func TestDefaultsAreValid(t *testing.T) {
	t.Parallel()

	// Defaults alone are incomplete on purpose (no node endpoints), so this
	// checks that the *only* complaints are the missing endpoints — not that a
	// default is out of its own permitted range.
	cfg := Default()
	err := cfg.Validate()
	if err == nil {
		t.Fatal("defaults validated with no node configured; expected missing-endpoint errors")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("Validate returned %T, want *ValidationError", err)
	}
	for _, p := range ve.Problems {
		if !strings.HasPrefix(p, "sf") && !strings.HasPrefix(p, "sq") {
			t.Errorf("a default value is itself invalid: %s", p)
		}
	}
}

// minimalValid is the smallest configuration that passes validation. Tests mutate
// a copy of it so that each case exercises exactly one rule.
func minimalValid() Config {
	c := Default()
	c.SF.RPCURL = "http://127.0.0.1:8332"
	c.SF.RPCCookiePath = "/tmp/sf.cookie"
	c.SQ.Bitcoind.RPCURL = "http://127.0.0.1:8432"
	c.SQ.Bitcoind.RPCCookiePath = "/tmp/sq.cookie"
	return c
}

func TestMinimalValidPasses(t *testing.T) {
	t.Parallel()
	cfg := minimalValid()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("minimal valid config rejected: %v", err)
	}
}

// TestValidationRules covers each documented rule with a configuration that
// breaks exactly that rule, asserting the message names the offending setting —
// a validation error that does not say which key is wrong is not much help.
func TestValidationRules(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		mutate  func(*Config)
		wantSub string
	}{
		{
			name:    "sf.rpc_url missing",
			mutate:  func(c *Config) { c.SF.RPCURL = "" },
			wantSub: "sf.rpc_url",
		},
		{
			name:    "sf.rpc_url not a url",
			mutate:  func(c *Config) { c.SF.RPCURL = "not://a/url:::" },
			wantSub: "sf.rpc_url",
		},
		{
			name:    "sf.rpc_url wrong scheme",
			mutate:  func(c *Config) { c.SF.RPCURL = "ftp://host:8332" },
			wantSub: "sf.rpc_url",
		},
		{
			name: "both auth methods on one endpoint",
			mutate: func(c *Config) {
				c.SF.RPCUser = "u"
				c.SF.RPCPass = "p"
			},
			wantSub: "auth",
		},
		{
			name: "no auth method",
			mutate: func(c *Config) {
				c.SF.RPCCookiePath = ""
			},
			wantSub: "auth",
		},
		{
			name: "user without pass",
			mutate: func(c *Config) {
				c.SF.RPCCookiePath = ""
				c.SF.RPCUser = "u"
			},
			wantSub: "auth",
		},
		{
			name:    "unknown tier",
			mutate:  func(c *Config) { c.SQ.Tier = "carrier-pigeon" },
			wantSub: "sq.tier",
		},
		{
			name:    "tier not yet implemented",
			mutate:  func(c *Config) { c.SQ.Tier = TierNeutrino },
			wantSub: "sq.tier",
		},
		{
			name:    "bitcoind tier without url",
			mutate:  func(c *Config) { c.SQ.Bitcoind.RPCURL = "" },
			wantSub: "sq.bitcoind",
		},
		{
			name:    "split_confirm_depth too low",
			mutate:  func(c *Config) { c.Sentinel.SplitConfirmDepth = 0 },
			wantSub: "range",
		},
		{
			name:    "split_confirm_depth too high",
			mutate:  func(c *Config) { c.Sentinel.SplitConfirmDepth = 101 },
			wantSub: "range",
		},
		{
			name:    "reorg_margin negative",
			mutate:  func(c *Config) { c.Sentinel.ReorgMargin = -1 },
			wantSub: "range",
		},
		{
			name:    "timeline_max_mb too low",
			mutate:  func(c *Config) { c.Store.TimelineMaxMB = 0 },
			wantSub: "range",
		},
		{
			name: "duplicate transport name",
			mutate: func(c *Config) {
				c.Alerts.Transport = []TransportConfig{
					{Name: "same", Type: TransportWebhook, MinTier: MinTierInfo},
					{Name: "same", Type: TransportNtfy, MinTier: MinTierInfo},
				}
			},
			wantSub: "transport name",
		},
		{
			name: "unknown transport type",
			mutate: func(c *Config) {
				c.Alerts.Transport = []TransportConfig{{Name: "t", Type: "smoke-signal"}}
			},
			wantSub: "transport name",
		},
		{
			name: "invalid transport tier",
			mutate: func(c *Config) {
				c.Alerts.Transport = []TransportConfig{{Name: "t", Type: TransportNtfy, MinTier: "shouty"}}
			},
			wantSub: "transport name",
		},
		{
			name: "auth none on a routable address",
			mutate: func(c *Config) {
				c.UI.Listen = "0.0.0.0:8330"
				c.UI.Auth = AuthNone
			},
			wantSub: "ui.auth",
		},
		{
			name: "auth platform on loopback",
			mutate: func(c *Config) {
				c.UI.Listen = "127.0.0.1:8330"
				c.UI.Auth = AuthPlatform
			},
			wantSub: "ui.auth",
		},
		{
			name: "auth password without a hash",
			mutate: func(c *Config) {
				c.UI.Auth = AuthPassword
				c.UI.PasswordHash = ""
			},
			wantSub: "ui.auth",
		},
		{
			name: "auth password with a non-bcrypt hash",
			mutate: func(c *Config) {
				c.UI.Auth = AuthPassword
				c.UI.PasswordHash = "hunter2"
			},
			wantSub: "ui.auth",
		},
		{
			name:    "unknown auth mode",
			mutate:  func(c *Config) { c.UI.Auth = "wide-open" },
			wantSub: "ui.auth",
		},
		{
			name:    "unknown log level",
			mutate:  func(c *Config) { c.Log.Level = "chatty" },
			wantSub: "log.level",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := minimalValid()
			tc.mutate(&cfg)

			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected a validation error mentioning %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error does not mention %q, so the user cannot tell what to fix:\n%v",
					tc.wantSub, err)
			}
		})
	}
}

func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	t.Parallel()

	cfg := minimalValid()
	cfg.SF.RPCURL = ""
	cfg.SQ.Tier = "nonsense"
	cfg.Log.Level = "loud"
	cfg.Store.TimelineMaxMB = 0

	err := cfg.Validate()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("Validate returned %T, want *ValidationError", err)
	}
	if len(ve.Problems) < 4 {
		t.Errorf("got %d problems, want at least 4 — validation should not stop at the first:\n%v",
			len(ve.Problems), err)
	}
}

func TestLoadRejectsMissingFile(t *testing.T) {
	t.Parallel()
	if _, err := Load(filepath.Join(t.TempDir(), "absent.toml")); err == nil {
		t.Fatal("Load accepted a path that does not exist")
	}
}

func TestUnknownKeyIsAWarningNotAnError(t *testing.T) {
	t.Parallel()

	path := writeTempConfig(t, `
[sf]
rpc_url = "http://127.0.0.1:8332"
rpc_cookie_path = "/tmp/sf.cookie"
[sq]
tier = "bitcoind"
[sq.bitcoind]
rpc_url = "http://127.0.0.1:8432"
rpc_cookie_path = "/tmp/sq.cookie"
[sentinel]
this_key_does_not_exist = 42
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("an unknown key made loading fail; it should only warn: %v", err)
	}
	if !hasWarningContaining(cfg.Warnings, "this_key_does_not_exist") {
		t.Errorf("unknown key was silently discarded; warnings were %v", cfg.Warnings)
	}
}

func TestPermissiveFileModeWarns(t *testing.T) {
	t.Parallel()

	path := writeTempConfig(t, `
[sf]
rpc_url = "http://127.0.0.1:8332"
rpc_user = "u"
rpc_pass = "p"
[sq.bitcoind]
rpc_url = "http://127.0.0.1:8432"
rpc_cookie_path = "/tmp/sq.cookie"
`)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("a permissive mode made loading fail; it should only warn: %v", err)
	}
	if !hasWarningContaining(cfg.Warnings, "readable by other users") {
		t.Errorf("world-readable config holding credentials did not warn; warnings were %v",
			cfg.Warnings)
	}

	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if hasWarningContaining(cfg.Warnings, "readable by other users") {
		t.Errorf("mode 0600 warned anyway; warnings were %v", cfg.Warnings)
	}
}

func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "forktower.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func hasWarningContaining(warnings []string, sub string) bool {
	for _, w := range warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

// TestExampleConfigRoundTrips encodes the loaded example back to TOML and reads
// it again. This catches a struct tag that is wrong in only one direction, which
// a decode-only test would miss: a mis-tagged field would silently reappear under
// a different key and then be reported as unknown on the next load.
func TestExampleConfigRoundTrips(t *testing.T) {
	t.Parallel()

	first, err := Load(exampleConfigPath)
	if err != nil {
		t.Fatalf("loading example: %v", err)
	}

	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(first); err != nil {
		t.Fatalf("encoding: %v", err)
	}

	path := filepath.Join(t.TempDir(), "roundtrip.toml")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	second, err := Load(path)
	if err != nil {
		t.Fatalf("re-loading encoded config: %v\n--- encoded ---\n%s", err, buf.String())
	}
	for _, w := range second.Warnings {
		if strings.Contains(w, "unknown config key") {
			t.Errorf("re-encoded config has a key we cannot read back — a struct tag is "+
				"wrong in one direction: %s", w)
		}
	}

	// Path and Warnings are load-time metadata, not configuration.
	first.Path, second.Path = "", ""
	first.Warnings, second.Warnings = nil, nil

	if !reflect.DeepEqual(first, second) {
		t.Errorf("config changed across a round trip:\n first: %+v\nsecond: %+v", first, second)
	}
}
