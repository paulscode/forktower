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
	// num32 and numInt both parse with a 32-bit bound, so the narrowing
	// conversion cannot overflow — ParseInt has already rejected anything out of
	// range. Keeping the conversion here rather than at each call site means the
	// argument for its safety lives in one place.
	num32 := func(key string, set func(*Config, int32)) envBinding {
		return envBinding{key: EnvPrefix + key, apply: func(c *Config, v string) error {
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 32)
			if err != nil {
				return fmt.Errorf("expected a whole number, got %q", v)
			}
			set(c, int32(n))
			return nil
		}}
	}
	// yesNo is deliberately strict. A platform that injects an empty string, or
	// "on", meant something by it, and quietly reading that as false would
	// switch a watchtower off without saying so.
	yesNo := func(key string, set func(*Config, bool)) envBinding {
		return envBinding{key: EnvPrefix + key, apply: func(c *Config, v string) error {
			b, err := strconv.ParseBool(strings.TrimSpace(v))
			if err != nil {
				return fmt.Errorf("expected true or false, got %q", v)
			}
			set(c, b)
			return nil
		}}
	}
	num64 := func(key string, set func(*Config, int64)) envBinding {
		return envBinding{key: EnvPrefix + key, apply: func(c *Config, v string) error {
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return fmt.Errorf("expected a whole number, got %q", v)
			}
			set(c, n)
			return nil
		}}
	}
	numInt := func(key string, set func(*Config, int)) envBinding {
		return envBinding{key: EnvPrefix + key, apply: func(c *Config, v string) error {
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 32)
			if err != nil {
				return fmt.Errorf("expected a whole number, got %q", v)
			}
			set(c, int(n))
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

		str("SQ_TIER", func(c *Config, v string) { c.SQ.Tier = BackendTier(v) }),
		str("SQ_BITCOIND_RPC_URL", func(c *Config, v string) { c.SQ.Bitcoind.RPCURL = v }),
		str("SQ_BITCOIND_RPC_COOKIE_PATH", func(c *Config, v string) { c.SQ.Bitcoind.RPCCookiePath = v }),
		str("SQ_BITCOIND_RPC_USER", func(c *Config, v string) { c.SQ.Bitcoind.RPCUser = v }),
		str("SQ_BITCOIND_RPC_PASS", func(c *Config, v string) { c.SQ.Bitcoind.RPCPass = v }),
		str("SQ_BITCOIND_ZMQ_RAWBLOCK", func(c *Config, v string) { c.SQ.Bitcoind.ZMQRawBlock = v }),
		str("SQ_BITCOIND_ZMQ_RAWTX", func(c *Config, v string) { c.SQ.Bitcoind.ZMQRawTx = v }),

		str("FORK_NAME", func(c *Config, v string) { c.Fork.Name = v }),
		num32("FORK_SIGNAL_BIT", func(c *Config, n int32) { c.Fork.SignalBit = n }),
		num32("FORK_DIVERGENCE_HEIGHT", func(c *Config, n int32) { c.Fork.DivergenceHeight = n }),
		num32("FORK_RULE_ACTIVATION_HEIGHT", func(c *Config, n int32) { c.Fork.RuleActivationHeight = n }),
		num32("FORK_EXPIRY_HEIGHT", func(c *Config, n int32) { c.Fork.ExpiryHeight = n }),

		numInt("SENTINEL_POLL_INTERVAL_SECS", func(c *Config, n int) { c.Sentinel.PollIntervalSecs = n }),
		num32("SENTINEL_SPLIT_CONFIRM_DEPTH", func(c *Config, n int32) { c.Sentinel.SplitConfirmDepth = n }),
		num32("SENTINEL_MAX_ANCESTOR_WALK", func(c *Config, n int32) { c.Sentinel.MaxAncestorWalk = n }),
		flt("SENTINEL_SQ_STALL_FACTOR", func(c *Config, f float64) { c.Sentinel.SQStallFactor = f }),
		num32("SENTINEL_REORG_MARGIN", func(c *Config, n int32) { c.Sentinel.ReorgMargin = n }),

		numInt("ALERTS_SELF_TEST_INTERVAL_HOURS", func(c *Config, n int) { c.Alerts.SelfTestIntervalHours = n }),
		numInt("ALERTS_CRITICAL_REPEAT_MINS", func(c *Config, n int) { c.Alerts.CriticalRepeatMins = n }),

		str("UI_LISTEN", func(c *Config, v string) { c.UI.Listen = v }),
		str("UI_AUTH", func(c *Config, v string) { c.UI.Auth = AuthMode(v) }),
		str("UI_PASSWORD_HASH", func(c *Config, v string) { c.UI.PasswordHash = v }),

		yesNo("TOWER_LND_ENABLED", func(c *Config, b bool) { c.Tower.LND.Enabled = b }),
		str("TOWER_LND_LISTEN", func(c *Config, v string) { c.Tower.LND.Listen = v }),
		str("TOWER_LND_API_URL", func(c *Config, v string) { c.Tower.LND.APIURL = v }),
		str("TOWER_LND_MACAROON_PATH", func(c *Config, v string) { c.Tower.LND.MacaroonPath = v }),
		str("TOWER_LND_TLS_CERT_PATH", func(c *Config, v string) { c.Tower.LND.TLSCertPath = v }),
		str("TOWER_LND_DATA_DIR", func(c *Config, v string) { c.Tower.LND.DataDir = v }),
		num64("TOWER_LND_MAX_DISK_MB", func(c *Config, n int64) { c.Tower.LND.MaxDiskMB = n }),

		yesNo("TOWER_TEOS_ENABLED", func(c *Config, b bool) { c.Tower.TEOS.Enabled = b }),
		str("TOWER_TEOS_LISTEN", func(c *Config, v string) { c.Tower.TEOS.Listen = v }),
		str("TOWER_TEOS_API_URL", func(c *Config, v string) { c.Tower.TEOS.APIURL = v }),
		str("TOWER_TEOS_PUBKEY", func(c *Config, v string) { c.Tower.TEOS.Pubkey = v }),
		str("TOWER_TEOS_DATA_DIR", func(c *Config, v string) { c.Tower.TEOS.DataDir = v }),
		num64("TOWER_TEOS_MAX_DISK_MB", func(c *Config, n int64) { c.Tower.TEOS.MaxDiskMB = n }),

		str("STORE_PATH", func(c *Config, v string) { c.Store.Path = v }),
		numInt("STORE_TIMELINE_MAX_MB", func(c *Config, n int) { c.Store.TimelineMaxMB = n }),

		str("LOG_LEVEL", func(c *Config, v string) { c.Log.Level = LogLevel(v) }),
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
