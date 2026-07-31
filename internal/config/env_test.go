package config

import (
	"strings"
	"testing"
)

// fakeEnv builds a LookupFunc over a map, so environment mapping is tested
// without mutating the real environment and without serialising the tests.
func fakeEnv(vars map[string]string) LookupFunc {
	return func(key string) (string, bool) {
		v, ok := vars[key]
		return v, ok
	}
}

func TestEnvOverridesFile(t *testing.T) {
	t.Parallel()

	cfg := minimalValid()
	cfg.UI.Listen = "127.0.0.1:8330"
	cfg.Log.Level = "info"

	warnings, err := applyEnv(&cfg, fakeEnv(map[string]string{
		"FORKTOWER_UI_LISTEN": "0.0.0.0:9999",
		"FORKTOWER_LOG_LEVEL": "debug",
	}))
	if err != nil {
		t.Fatalf("applyEnv: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if cfg.UI.Listen != "0.0.0.0:9999" {
		t.Errorf("ui.listen = %q, want the environment value to win", cfg.UI.Listen)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("log.level = %q, want the environment value to win", cfg.Log.Level)
	}
}

func TestEnvLeavesUnsetKeysAlone(t *testing.T) {
	t.Parallel()

	cfg := minimalValid()
	before := cfg
	if _, err := applyEnv(&cfg, fakeEnv(nil)); err != nil {
		t.Fatalf("applyEnv: %v", err)
	}
	if cfg.UI.Listen != before.UI.Listen || cfg.SF.RPCURL != before.SF.RPCURL {
		t.Error("an empty environment changed the configuration")
	}
}

func TestEnvParsesNumbersAndFloats(t *testing.T) {
	t.Parallel()

	cfg := minimalValid()
	if _, err := applyEnv(&cfg, fakeEnv(map[string]string{
		"FORKTOWER_SENTINEL_SPLIT_CONFIRM_DEPTH": "7",
		"FORKTOWER_SENTINEL_SQ_STALL_FACTOR":     "2.5",
		"FORKTOWER_FORK_DIVERGENCE_HEIGHT":       " 961632 ",
		"FORKTOWER_STORE_TIMELINE_MAX_MB":        "64",
	})); err != nil {
		t.Fatalf("applyEnv: %v", err)
	}
	if cfg.Sentinel.SplitConfirmDepth != 7 {
		t.Errorf("split_confirm_depth = %d, want 7", cfg.Sentinel.SplitConfirmDepth)
	}
	if cfg.Sentinel.SQStallFactor != 2.5 {
		t.Errorf("sq_stall_factor = %v, want 2.5", cfg.Sentinel.SQStallFactor)
	}
	if cfg.Fork.DivergenceHeight != 961632 {
		t.Errorf("divergence_height = %d, want 961632 (surrounding spaces trimmed)",
			cfg.Fork.DivergenceHeight)
	}
	if cfg.Store.TimelineMaxMB != 64 {
		t.Errorf("timeline_max_mb = %d, want 64", cfg.Store.TimelineMaxMB)
	}
}

// A malformed override is an error, not a warning: someone who sets a variable
// means to change that setting, and silently keeping the old value would be a
// worse surprise than refusing to start.
func TestEnvMalformedNumberIsAnError(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"FORKTOWER_SENTINEL_SPLIT_CONFIRM_DEPTH": "three",
		"FORKTOWER_SENTINEL_SQ_STALL_FACTOR":     "quite-slow",
		"FORKTOWER_FORK_DIVERGENCE_HEIGHT":       "961_632",
	}
	for key, bad := range cases {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			cfg := minimalValid()
			_, err := applyEnv(&cfg, fakeEnv(map[string]string{key: bad}))
			if err == nil {
				t.Fatalf("applyEnv accepted %s=%q", key, bad)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error does not name the variable, so the user cannot tell which "+
					"one to fix: %v", err)
			}
		})
	}
}

func TestEnvKeysAllCarryThePrefix(t *testing.T) {
	t.Parallel()

	keys := EnvKeys()
	if len(keys) == 0 {
		t.Fatal("no environment bindings declared")
	}
	seen := make(map[string]bool, len(keys))
	for _, k := range keys {
		if !strings.HasPrefix(k, EnvPrefix) {
			t.Errorf("%q does not start with %q", k, EnvPrefix)
		}
		if seen[k] {
			t.Errorf("%q is bound twice; the later binding would silently win", k)
		}
		seen[k] = true
	}
}

func TestEffectiveReorgMarginWidensWhenDivergenceUnknown(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		divergence int32
		configured int32
		want       int32
	}{
		{
			name:       "unknown divergence widens the margin",
			divergence: 0,
			want:       ReorgMarginUnknown,
		},
		{
			name:       "known divergence uses the narrow margin",
			divergence: 961632,
			want:       ReorgMarginKnown,
		},
		{
			name:       "an explicit margin is honoured even when divergence is unknown",
			divergence: 0,
			configured: 50,
			want:       50,
		},
		{
			name:       "an explicit margin is honoured when divergence is known",
			divergence: 961632,
			configured: 7,
			want:       7,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := Default()
			cfg.Fork.DivergenceHeight = tc.divergence
			cfg.Sentinel.ReorgMargin = tc.configured
			if got := cfg.EffectiveReorgMargin(); got != tc.want {
				t.Errorf("EffectiveReorgMargin() = %d, want %d", got, tc.want)
			}
		})
	}
}

// The default exists to keep alert timing away from third parties, so it is
// asserted per transport type rather than assumed.
func TestEffectiveIncludeDetailDefaults(t *testing.T) {
	t.Parallel()

	yes, no := true, false
	cases := []struct {
		typ  TransportType
		set  *bool
		want bool
		why  string
	}{
		{typ: TransportNtfy, want: false, why: "third-party server sees attack timing"},
		{typ: TransportWebhook, want: false, why: "third-party endpoint sees attack timing"},
		{typ: TransportSMTP, want: false, why: "mail host sees attack timing"},
		{typ: TransportTelegram, want: false, why: "third-party service sees attack timing"},
		{typ: TransportStartOS, want: true, why: "delivered by the user's own device"},
		{typ: TransportUmbrel, want: true, why: "delivered by the user's own device"},
		{typ: TransportNtfy, set: &yes, want: true, why: "explicit opt-in overrides the default"},
		{typ: TransportStartOS, set: &no, want: false, why: "explicit opt-out overrides the default"},
	}

	for _, tc := range cases {
		got := TransportConfig{Type: tc.typ, IncludeDetail: tc.set}.EffectiveIncludeDetail()
		if got != tc.want {
			t.Errorf("type %q (set=%v): EffectiveIncludeDetail() = %v, want %v — %s",
				tc.typ, tc.set, got, tc.want, tc.why)
		}
	}
}

// The companion towers are configured entirely from the environment in a
// container deployment, so every one of their keys has to actually land.
func TestTheTowerSettingsCanBeSetFromTheEnvironment(t *testing.T) {
	t.Parallel()

	env := map[string]string{
		"FORKTOWER_TOWER_LND_ENABLED":       "true",
		"FORKTOWER_TOWER_LND_LISTEN":        "abcdef.onion:9911",
		"FORKTOWER_TOWER_LND_API_URL":       "https://tower-lnd:8080",
		"FORKTOWER_TOWER_LND_MACAROON_PATH": "/mnt/tower/readonly.macaroon",
		"FORKTOWER_TOWER_LND_TLS_CERT_PATH": "/mnt/tower/tls.cert",
		"FORKTOWER_TOWER_LND_DATA_DIR":      "/mnt/tower",
		"FORKTOWER_TOWER_LND_MAX_DISK_MB":   "4096",
		"FORKTOWER_TOWER_TEOS_ENABLED":      "true",
		"FORKTOWER_TOWER_TEOS_LISTEN":       "fedcba.onion:9814",
		"FORKTOWER_TOWER_TEOS_API_URL":      "http://tower-teos:9814",
		"FORKTOWER_TOWER_TEOS_PUBKEY":       "021d0a0474eb64a593fb3eba314059409bd353dd5eb847caf3d4361e28871bf593",
		"FORKTOWER_TOWER_TEOS_DATA_DIR":     "/mnt/teos",
		"FORKTOWER_TOWER_TEOS_MAX_DISK_MB":  "512",
	}

	cfg := Default()
	if _, err := applyEnv(&cfg, func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}); err != nil {
		t.Fatal(err)
	}

	if !cfg.Tower.LND.Enabled || cfg.Tower.LND.APIURL != "https://tower-lnd:8080" ||
		cfg.Tower.LND.MacaroonPath == "" || cfg.Tower.LND.MaxDiskMB != 4096 {
		t.Errorf("the LND tower settings did not land: %+v", cfg.Tower.LND)
	}
	if !cfg.Tower.TEOS.Enabled || cfg.Tower.TEOS.Pubkey == "" ||
		cfg.Tower.TEOS.DataDir != "/mnt/teos" || cfg.Tower.TEOS.MaxDiskMB != 512 {
		t.Errorf("the teos tower settings did not land: %+v", cfg.Tower.TEOS)
	}
}

// A platform injecting an empty string, or "on", meant something by it, and
// reading either as false would switch a watchtower off in silence.
func TestASwitchThatCannotBeReadIsRefusedRatherThanTakenAsOff(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"", "on", "yes", "1.5", "maybe"} {
		cfg := Default()
		_, err := applyEnv(&cfg, func(key string) (string, bool) {
			if key == "FORKTOWER_TOWER_LND_ENABLED" {
				return value, true
			}
			return "", false
		})
		if err == nil {
			t.Errorf("%q was accepted as a yes-or-no setting", value)
		}
		if cfg.Tower.LND.Enabled {
			t.Errorf("%q switched the tower on", value)
		}
	}

	// And the forms that do mean something still work.
	for _, value := range []string{"true", "TRUE", "1", "t", " true "} {
		cfg := Default()
		if _, err := applyEnv(&cfg, func(key string) (string, bool) {
			if key == "FORKTOWER_TOWER_LND_ENABLED" {
				return value, true
			}
			return "", false
		}); err != nil {
			t.Errorf("%q was refused: %v", value, err)
		} else if !cfg.Tower.LND.Enabled {
			t.Errorf("%q did not switch the tower on", value)
		}
	}
}
