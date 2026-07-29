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
		typ  string
		set  *bool
		want bool
		why  string
	}{
		{typ: "ntfy", want: false, why: "third-party server sees attack timing"},
		{typ: "webhook", want: false, why: "third-party endpoint sees attack timing"},
		{typ: "smtp", want: false, why: "mail host sees attack timing"},
		{typ: "telegram", want: false, why: "third-party service sees attack timing"},
		{typ: "startos", want: true, why: "delivered by the user's own device"},
		{typ: "umbrel", want: true, why: "delivered by the user's own device"},
		{typ: "ntfy", set: &yes, want: true, why: "explicit opt-in overrides the default"},
		{typ: "startos", set: &no, want: false, why: "explicit opt-out overrides the default"},
	}

	for _, tc := range cases {
		got := TransportConfig{Type: tc.typ, IncludeDetail: tc.set}.EffectiveIncludeDetail()
		if got != tc.want {
			t.Errorf("type %q (set=%v): EffectiveIncludeDetail() = %v, want %v — %s",
				tc.typ, tc.set, got, tc.want, tc.why)
		}
	}
}
