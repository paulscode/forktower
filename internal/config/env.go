package config

import (
	"fmt"
	"strconv"
	"strings"
)

// EnvPrefix is prepended to every environment variable this package reads.
const EnvPrefix = "FORKTOWER_"

// LookupFunc matches os.LookupEnv, injected so the mapping is testable without
// touching the real environment.
type LookupFunc func(key string) (string, bool)

// envBinding maps one environment variable to one field. Written out by hand
// rather than derived by reflection: the set is small, an explicit table is
// obvious to read, and it cannot be surprised by a struct-field rename.
type envBinding struct {
	key   string
	apply func(*Config, string) error
}

// applyEnv overlays environment variables onto cfg. An override always beats the
// file, because that is what makes container deployments possible: the platform
// injects endpoints and credentials it discovered, over a config file baked into
// an image.
//
// A malformed value is an error rather than a warning — a user who sets
// FORKTOWER_UI_LISTEN to nonsense means to change the listen address, and
// silently keeping the old one would be worse than refusing to start.
func applyEnv(cfg *Config, lookup LookupFunc) ([]string, error) {
	var warnings []string
	for _, b := range envBindings() {
		raw, ok := lookup(b.key)
		if !ok {
			continue
		}
		if err := b.apply(cfg, raw); err != nil {
			return nil, fmt.Errorf("environment variable %s: %w", b.key, err)
		}
	}
	return warnings, nil
}

func envBindings() []envBinding {
	str := func(key string, set func(*Config, string)) envBinding {
		return envBinding{key: EnvPrefix + key, apply: func(c *Config, v string) error {
			set(c, v)
			return nil
		}}
	}
	num := func(key string, set func(*Config, int64)) envBinding {
		return envBinding{key: EnvPrefix + key, apply: func(c *Config, v string) error {
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 32)
			if err != nil {
				return fmt.Errorf("expected a whole number, got %q", v)
			}
			set(c, n)
			return nil
		}}
	}
	flt := func(key string, set func(*Config, float64)) envBinding {
		return envBinding{key: EnvPrefix + key, apply: func(c *Config, v string) error {
			f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil {
				return fmt.Errorf("expected a number, got %q", v)
			}
			set(c, f)
			return nil
		}}
	}

	return []envBinding{
		str("SF_RPC_URL", func(c *Config, v string) { c.SF.RPCURL = v }),
		str("SF_RPC_COOKIE_PATH", func(c *Config, v string) { c.SF.RPCCookiePath = v }),
		str("SF_RPC_USER", func(c *Config, v string) { c.SF.RPCUser = v }),
		str("SF_RPC_PASS", func(c *Config, v string) { c.SF.RPCPass = v }),
		str("SF_ZMQ_RAWBLOCK", func(c *Config, v string) { c.SF.ZMQRawBlock = v }),

		str("SQ_TIER", func(c *Config, v string) { c.SQ.Tier = v }),
		str("SQ_BITCOIND_RPC_URL", func(c *Config, v string) { c.SQ.Bitcoind.RPCURL = v }),
		str("SQ_BITCOIND_RPC_COOKIE_PATH", func(c *Config, v string) { c.SQ.Bitcoind.RPCCookiePath = v }),
		str("SQ_BITCOIND_RPC_USER", func(c *Config, v string) { c.SQ.Bitcoind.RPCUser = v }),
		str("SQ_BITCOIND_RPC_PASS", func(c *Config, v string) { c.SQ.Bitcoind.RPCPass = v }),
		str("SQ_BITCOIND_ZMQ_RAWBLOCK", func(c *Config, v string) { c.SQ.Bitcoind.ZMQRawBlock = v }),
		str("SQ_BITCOIND_ZMQ_RAWTX", func(c *Config, v string) { c.SQ.Bitcoind.ZMQRawTx = v }),

		str("FORK_NAME", func(c *Config, v string) { c.Fork.Name = v }),
		num("FORK_SIGNAL_BIT", func(c *Config, n int64) { c.Fork.SignalBit = int32(n) }),
		num("FORK_DIVERGENCE_HEIGHT", func(c *Config, n int64) { c.Fork.DivergenceHeight = int32(n) }),
		num("FORK_RULE_ACTIVATION_HEIGHT", func(c *Config, n int64) { c.Fork.RuleActivationHeight = int32(n) }),
		num("FORK_EXPIRY_HEIGHT", func(c *Config, n int64) { c.Fork.ExpiryHeight = int32(n) }),

		num("SENTINEL_POLL_INTERVAL_SECS", func(c *Config, n int64) { c.Sentinel.PollIntervalSecs = int(n) }),
		num("SENTINEL_SPLIT_CONFIRM_DEPTH", func(c *Config, n int64) { c.Sentinel.SplitConfirmDepth = int32(n) }),
		num("SENTINEL_MAX_ANCESTOR_WALK", func(c *Config, n int64) { c.Sentinel.MaxAncestorWalk = int32(n) }),
		flt("SENTINEL_SQ_STALL_FACTOR", func(c *Config, f float64) { c.Sentinel.SQStallFactor = f }),
		num("SENTINEL_REORG_MARGIN", func(c *Config, n int64) { c.Sentinel.ReorgMargin = int32(n) }),

		num("ALERTS_SELF_TEST_INTERVAL_HOURS", func(c *Config, n int64) { c.Alerts.SelfTestIntervalHours = int(n) }),
		num("ALERTS_CRITICAL_REPEAT_MINS", func(c *Config, n int64) { c.Alerts.CriticalRepeatMins = int(n) }),

		str("UI_LISTEN", func(c *Config, v string) { c.UI.Listen = v }),
		str("UI_AUTH", func(c *Config, v string) { c.UI.Auth = v }),
		str("UI_PASSWORD_HASH", func(c *Config, v string) { c.UI.PasswordHash = v }),

		str("STORE_PATH", func(c *Config, v string) { c.Store.Path = v }),
		num("STORE_TIMELINE_MAX_MB", func(c *Config, n int64) { c.Store.TimelineMaxMB = int(n) }),

		str("LOG_LEVEL", func(c *Config, v string) { c.Log.Level = v }),
	}
}

// EnvKeys returns every environment variable this package reads, sorted as
// declared. Used by the documentation generator and by tests that check the
// example file and the bindings have not drifted apart.
func EnvKeys() []string {
	bs := envBindings()
	keys := make([]string, 0, len(bs))
	for _, b := range bs {
		keys = append(keys, b.key)
	}
	return keys
}
