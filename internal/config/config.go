// Package config loads and validates the daemon's configuration.
//
// Validation collects every problem and reports them together, rather than
// stopping at the first, because a user fixing a config file should learn about
// all of it in one pass.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Defaults applied before the file is read, so an omitted key means "the sensible
// value" rather than a zero. Anything security-relevant is deliberately absent
// from this list: see ForkDescriptor.
const (
	DefaultUIListen           = "127.0.0.1:8330"
	DefaultPollIntervalSecs   = 10
	DefaultSplitConfirmDepth  = 3
	DefaultMaxAncestorWalk    = 20000
	DefaultSQStallFactor      = 6.0
	DefaultSelfTestIntervalHr = 168
	DefaultCriticalRepeatMins = 30
	DefaultTimelineMaxMB      = 256
	DefaultStorePath          = "/data/forktower.db"

	// ReorgMarginKnown applies when the divergence height is known; the wider
	// ReorgMarginUnknown applies when it is not. The trust anchor is capped by
	// the minimum of several terms, and the cost of a cap that is too low is a
	// little redundant verification, while the cost of one that is too high is
	// anchoring to the wrong branch. So when we know less, we bias lower.
	ReorgMarginKnown   = 100
	ReorgMarginUnknown = 2016
)

// Config is the whole configuration. Field names and TOML tags match the
// documented file exactly; the shape is deliberately flat and boring.
type Config struct {
	SF       RPCEndpoint    `toml:"sf"`
	SQ       SQConfig       `toml:"sq"`
	Fork     ForkDescriptor `toml:"fork"`
	LN       LNConfig       `toml:"ln"`
	Sentinel SentinelConfig `toml:"sentinel"`
	Alerts   AlertsConfig   `toml:"alerts"`
	Tower    TowerConfig    `toml:"tower"`
	UI       UIConfig       `toml:"ui"`
	Store    StoreConfig    `toml:"store"`
	Log      LogConfig      `toml:"log"`

	// Warnings are non-fatal problems found while loading: unknown keys,
	// permissive file modes, values we will ignore. They are reported rather
	// than swallowed, because a config that "works" while silently discarding
	// half of what the user wrote is worse than one that complains.
	Warnings []string `toml:"-"`

	// Path is the file this was loaded from, empty if built from defaults.
	Path string `toml:"-"`
}

// RPCEndpoint describes how to reach a bitcoind. Authentication is either a
// cookie file or a user/password pair, never both.
type RPCEndpoint struct {
	RPCURL        string `toml:"rpc_url"`
	RPCCookiePath string `toml:"rpc_cookie_path"`
	RPCUser       string `toml:"rpc_user"`
	RPCPass       string `toml:"rpc_pass"`
	ZMQRawBlock   string `toml:"zmq_rawblock"`
	ZMQRawTx      string `toml:"zmq_rawtx"`
}

// HasCookieAuth reports whether a cookie file was configured.
func (e RPCEndpoint) HasCookieAuth() bool { return e.RPCCookiePath != "" }

// HasUserPassAuth reports whether a user and password were configured.
func (e RPCEndpoint) HasUserPassAuth() bool { return e.RPCUser != "" || e.RPCPass != "" }

// BackendTier selects how the daemon obtains its view of the other chain.
type BackendTier string

// Backend tiers.
const (
	// TierBitcoind uses a second full node. The only tier implemented so far.
	TierBitcoind BackendTier = "bitcoind"
	// TierNeutrino uses a light client. Not yet implemented.
	TierNeutrino BackendTier = "neutrino"
	// TierElectrum uses a remote server. Not yet implemented.
	TierElectrum BackendTier = "electrum"
)

// SQConfig selects and configures the backend for the chain the user's own node
// is not following.
type SQConfig struct {
	Tier      BackendTier     `toml:"tier"`
	Bitcoind  RPCEndpoint     `toml:"bitcoind"`
	Witnesses WitnessesConfig `toml:"witnesses"`
	Neutrino  NeutrinoConfig  `toml:"neutrino"`
}

// WitnessesConfig configures independent second opinions about the other chain's
// tip. Parsed but not yet used.
type WitnessesConfig struct {
	NeutrinoHeaders bool     `toml:"neutrino_headers"`
	Electrum        []string `toml:"electrum"`
}

// NeutrinoConfig configures the light-client backend. Parsed but not yet used.
type NeutrinoConfig struct {
	Peers   []string `toml:"peers"`
	DataDir string   `toml:"datadir"`
}

// ForkDescriptor labels the fork and, in one field, constrains how far the
// daemon will trust the user's node as validated history.
//
// These values are an *override*, not the source of truth: they are normally
// derived from the node itself, which cannot go stale and generalises to any
// future fork of the same shape. A zero means "not configured, derive it".
type ForkDescriptor struct {
	Name string `toml:"name"`
	// SignalBit is the version bit miners set to signal. -1 when unknown.
	SignalBit int32 `toml:"signal_bit"`
	// DivergenceHeight is the first height at which the two rule sets can
	// disagree about a block's validity — for a mandatory-signalling fork, the
	// start of that window, which is where a chain can first split.
	//
	// This is the only security-relevant field here: it caps how far the daemon
	// treats the user's node as validated history. Setting it too high straddles
	// the point where the chains separated and would anchor the second view to
	// blocks from the first, which is the exact failure the cap prevents. When in
	// doubt it is left at zero, and a wider, node-derived margin is used instead.
	DivergenceHeight int32 `toml:"divergence_height"`
	// RuleActivationHeight is when the new transaction rules begin to bind.
	// Labelling and user-facing text only; nothing security-relevant reads it.
	RuleActivationHeight int32 `toml:"rule_activation_height"`
	// ExpiryHeight is when a temporary fork's rules stop being enforced, or 0
	// for a permanent one. Labelling only. Note that expiry does not heal a
	// split: blocks already rejected stay rejected.
	ExpiryHeight int32 `toml:"expiry_height"`
}

// DivergenceHeightKnown reports whether a divergence height was configured.
func (f ForkDescriptor) DivergenceHeightKnown() bool { return f.DivergenceHeight > 0 }

// LNConfig holds zero or more Lightning nodes of each implementation.
//
// Both are read over their REST interface rather than gRPC. That is not a
// preference: pinning lnd's gRPC bindings pulls in most of lnd for five
// read-only calls, and then does not build without lnd's own fork of the
// protobuf runtime. REST is what both platforms already expose, and it is
// reachable with the standard library alone.
type LNConfig struct {
	LND []LNDConfig `toml:"lnd"`
	CLN []CLNConfig `toml:"cln"`
}

// LNDConfig describes how to reach an LND node read-only.
type LNDConfig struct {
	// RESTAddr is the node's REST address, e.g. "https://127.0.0.1:8080".
	RESTAddr string `toml:"rest_addr"`
	// TLSCertPath is lnd's self-signed certificate, which is pinned rather than
	// trusted through the system roots: the certificate is the node's identity
	// here, and accepting any certificate for this address would mean no way to
	// notice being pointed somewhere else.
	TLSCertPath string `toml:"tls_cert_path"`
	// MacaroonPath is the credential file. A path, never the credential itself:
	// a secret written into a configuration file is a secret that ends up pasted
	// into support threads.
	MacaroonPath string `toml:"macaroon_path"`
}

// CLNConfig describes how to reach a Core Lightning node read-only.
type CLNConfig struct {
	// RESTAddr is the clnrest address, e.g. "https://127.0.0.1:3010".
	RESTAddr string `toml:"rest_addr"`
	// RunePath is the credential file, for the same reason LND's is a path.
	RunePath string `toml:"rune_path"`
	// TLSCertPath pins the node's certificate when it serves https. Optional:
	// clnrest is often plain http on the loopback address, where there is
	// nothing to pin and nothing in between.
	TLSCertPath string `toml:"tls_cert_path"`
}

// Configured reports whether any Lightning node is set up. None is a supported
// arrangement — split detection is useful on its own, and a user may not have
// connected a node yet — so this is a question, not a check.
func (l LNConfig) Configured() bool { return len(l.LND)+len(l.CLN) > 0 }

// SentinelConfig tunes split detection.
type SentinelConfig struct {
	PollIntervalSecs  int     `toml:"poll_interval_secs"`
	SplitConfirmDepth int32   `toml:"split_confirm_depth"`
	MaxAncestorWalk   int32   `toml:"max_ancestor_walk"`
	SQStallFactor     float64 `toml:"sq_stall_factor"`
	// ReorgMargin is the safety margin below the user's node's tip when
	// computing the trust anchor. Zero means "choose for me", which resolves to
	// ReorgMarginKnown or ReorgMarginUnknown depending on whether the divergence
	// height is configured. Use EffectiveReorgMargin rather than this field.
	ReorgMargin int32 `toml:"reorg_margin"`
}

// EffectiveReorgMargin returns the margin to use, widening it when the
// divergence height is unknown. An explicit non-zero configured value is
// honoured as-is.
func (c Config) EffectiveReorgMargin() int32 {
	if c.Sentinel.ReorgMargin > 0 {
		return c.Sentinel.ReorgMargin
	}
	if c.Fork.DivergenceHeightKnown() {
		return ReorgMarginKnown
	}
	return ReorgMarginUnknown
}

// AlertsConfig configures notification delivery.
type AlertsConfig struct {
	SelfTestIntervalHours int               `toml:"self_test_interval_hours"`
	CriticalRepeatMins    int               `toml:"critical_repeat_mins"`
	Transport             []TransportConfig `toml:"transport"`

	// PlatformNotifications says the surrounding platform raises alerts on
	// Forktower's behalf, by reading its API.
	//
	// Set by StartOS and Umbrel packaging, never by hand. It exists because
	// neither platform's notification system is reachable from inside an app
	// container — verified on both, 2026-07-30 — so the platform pulls rather
	// than the daemon pushing. Without this the dashboard would tell a platform
	// user it had no way to reach them, which on those platforms is untrue.
	PlatformNotifications bool `toml:"platform_notifications"`
}

// TransportType is a notification delivery mechanism.
type TransportType string

// Notification transports. The platform-local ones are delivered by the user's
// own device, so unlike the rest they have no third-party operator who would
// learn from an alert that this user is under attack.
const (
	TransportNtfy     TransportType = "ntfy"
	TransportWebhook  TransportType = "webhook"
	TransportSMTP     TransportType = "smtp"
	TransportTelegram TransportType = "telegram"
	TransportStartOS  TransportType = "startos"
	TransportUmbrel   TransportType = "umbrel"
)

// MinTier is the lowest alert severity a transport will deliver.
type MinTier string

// Minimum severities a transport can be set to.
const (
	MinTierInfo     MinTier = "info"
	MinTierWarning  MinTier = "warning"
	MinTierCritical MinTier = "critical"
)

// TransportConfig is one notification channel.
type TransportConfig struct {
	Name    string        `toml:"name"`
	Type    TransportType `toml:"type"`
	MinTier MinTier       `toml:"min_tier"`
	URL     string        `toml:"url"`
	Token   string        `toml:"token"`

	// IncludeDetail controls whether the payload names the channel, the amount
	// and the time remaining. It defaults to false for third-party transports
	// and true for platform-local ones: an alert otherwise tells whoever runs
	// that server that this user is under attack and how long they have, which
	// is the most useful thing an attacker could learn. A platform notification
	// has no such operator.
	//
	// Nil means "not set, apply the per-type default". Use EffectiveIncludeDetail.
	IncludeDetail *bool `toml:"include_detail"`

	// SMTP-only fields.
	Host     string `toml:"host"`
	Port     int    `toml:"port"`
	User     string `toml:"user"`
	Pass     string `toml:"pass"`
	From     string `toml:"from"`
	To       string `toml:"to"`
	StartTLS bool   `toml:"starttls"`
}

// IsPlatformLocal reports whether this transport is delivered by the user's own
// device rather than by a third-party server.
func (t TransportType) IsPlatformLocal() bool {
	switch t {
	case TransportStartOS, TransportUmbrel:
		return true
	case TransportNtfy, TransportWebhook, TransportSMTP, TransportTelegram:
		return false
	default:
		return false
	}
}

// EffectiveIncludeDetail resolves IncludeDetail against the per-type default.
func (t TransportConfig) EffectiveIncludeDetail() bool {
	if t.IncludeDetail != nil {
		return *t.IncludeDetail
	}
	return t.Type.IsPlatformLocal()
}

// TowerConfig configures companion watchtowers.
type TowerConfig struct {
	LND  TowerInstance `toml:"lnd"`
	TEOS TowerInstance `toml:"teos"`
}

// DefaultTowerMaxDiskMB caps a companion tower's data directory.
//
// A limit exists at all because LND's watchtower has none of its own: R2
// established that it accepts a session from anybody completing the handshake,
// with no client allowlist, no session cap and no disk cap. Each session costs
// disk on the same host as the Bitcoin node the user depends on, so an
// unbounded tower is a resource-exhaustion path we opened ourselves. Two
// gigabytes is generous for the sessions of one node's channels and small
// enough to notice.
const DefaultTowerMaxDiskMB = 2048

// TowerInstance is one companion tower.
type TowerInstance struct {
	Enabled bool `toml:"enabled"`
	// Listen is where the tower accepts clients: a Tor onion address by
	// default, or an explicit LAN address as a deliberate opt-in. Never a
	// wildcard — see validateTowers.
	Listen string `toml:"listen"`

	// APIURL is where Forktower reads the tower's own status. Not the address
	// clients connect to: that is Listen, and it speaks a different protocol.
	APIURL string `toml:"api_url"`
	// MacaroonPath and TLSCertPath are the read-only credentials for an LND
	// tower's own API. Unused by teos, which has its own.
	MacaroonPath string `toml:"macaroon_path"`
	TLSCertPath  string `toml:"tls_cert_path"`

	// DataDir is the tower's storage, watched against MaxDiskMB. Empty means
	// Forktower cannot see it and will say so rather than assume it is fine.
	DataDir string `toml:"data_dir"`
	// MaxDiskMB caps that directory. Zero means DefaultTowerMaxDiskMB.
	MaxDiskMB int64 `toml:"max_disk_mb"`
}

// DiskLimitMB is the cap to apply, resolving zero to the default.
func (t TowerInstance) DiskLimitMB() int64 {
	if t.MaxDiskMB <= 0 {
		return DefaultTowerMaxDiskMB
	}
	return t.MaxDiskMB
}

// UIConfig configures the dashboard and HTTP API.
type UIConfig struct {
	Listen string `toml:"listen"`
	// Auth is one of AuthNone, AuthPlatform or AuthPassword. It is never
	// inferred from the environment: delegating authentication has to be a
	// decision someone wrote down.
	Auth         AuthMode `toml:"auth"`
	PasswordHash string   `toml:"password_hash"`

	// AllowedOrigins lists extra origins accepted on requests that change
	// something, as `scheme://host[:port]`.
	//
	// Normally empty. The check compares the browser's Origin against the host
	// the request arrived on, which is correct without configuration for a
	// dashboard reached directly or through a proxy that preserves the Host
	// header — including when embedded in a platform's own page, because the
	// framed document's origin is this dashboard. It needs setting only for a
	// proxy that rewrites Host.
	AllowedOrigins []string `toml:"allowed_origins"`

	// AccessRestrictedExternally says that something outside Forktower controls
	// who can reach ui.listen.
	//
	// Needed because a container has to bind every address to be reachable at
	// all, and what actually decides its exposure is the port publishing — which
	// this process cannot see. From in here, "reachable from the whole network"
	// and "published to one machine's loopback" look identical.
	//
	// So it is the operator's statement, not an inference, and it is the only way
	// to serve an unauthenticated dashboard on a non-loopback address. Forktower
	// says at startup exactly what it is taking on trust.
	AccessRestrictedExternally bool `toml:"access_restricted_externally"`

	// FrameAncestors lists origins permitted to embed the dashboard in a frame.
	//
	// Empty means 'self'. Both platforms embed their apps, so this cannot simply
	// be denied; naming who may do it is the difference between that and letting
	// any page frame the dashboard to trick someone into clicking through it.
	FrameAncestors []string `toml:"frame_ancestors"`
}

// AuthMode is how the dashboard authenticates callers.
type AuthMode string

// UI authentication modes.
const (
	// AuthNone serves without authentication and is confined to loopback.
	AuthNone AuthMode = "none"
	// AuthPlatform permits a non-loopback bind, delegating authentication to the
	// app proxy that fronts it. Required because a container must bind a
	// routable address for its platform to reach it.
	AuthPlatform AuthMode = "platform"
	// AuthPassword checks a bcrypt hash and issues a session cookie.
	AuthPassword AuthMode = "password"
)

// StoreConfig configures on-disk state.
type StoreConfig struct {
	Path string `toml:"path"`
	// TimelineMaxMB is the size above which the event timeline is rotated into
	// an archive file. Rotation, never deletion: an audit trail that can be
	// quietly trimmed is not an audit trail.
	TimelineMaxMB int `toml:"timeline_max_mb"`
}

// LogLevel is logging verbosity.
type LogLevel string

// Logging verbosities.
const (
	LogDebug LogLevel = "debug"
	LogInfo  LogLevel = "info"
	LogWarn  LogLevel = "warn"
	LogError LogLevel = "error"
)

// LogConfig configures logging.
type LogConfig struct {
	Level LogLevel `toml:"level"`
}

// Default returns the configuration with defaults applied and nothing else.
func Default() Config {
	return Config{
		SQ:   SQConfig{Tier: TierBitcoind},
		Fork: ForkDescriptor{SignalBit: -1},
		Sentinel: SentinelConfig{
			PollIntervalSecs:  DefaultPollIntervalSecs,
			SplitConfirmDepth: DefaultSplitConfirmDepth,
			MaxAncestorWalk:   DefaultMaxAncestorWalk,
			SQStallFactor:     DefaultSQStallFactor,
		},
		Alerts: AlertsConfig{
			SelfTestIntervalHours: DefaultSelfTestIntervalHr,
			CriticalRepeatMins:    DefaultCriticalRepeatMins,
		},
		UI:    UIConfig{Listen: DefaultUIListen, Auth: AuthNone},
		Store: StoreConfig{Path: DefaultStorePath, TimelineMaxMB: DefaultTimelineMaxMB},
		Log:   LogConfig{Level: LogInfo},
	}
}

// Load reads the file at path, applies environment overrides, and validates the
// result. A non-empty path that does not exist is an error; an empty path yields
// defaults plus environment overrides.
//
// The returned Config is usable only when err is nil. Non-fatal problems are in
// Config.Warnings and the caller is expected to log them.
func Load(path string) (Config, error) {
	cfg := Default()
	cfg.Path = path

	if path != "" {
		md, err := toml.DecodeFile(path, &cfg)
		if err != nil {
			return Config{}, fmt.Errorf("reading config %s: %w", path, err)
		}
		for _, key := range md.Undecoded() {
			cfg.Warnings = append(cfg.Warnings,
				fmt.Sprintf("unknown config key %q ignored", key.String()))
		}
		if w := checkFileMode(path); w != "" {
			cfg.Warnings = append(cfg.Warnings, w)
		}
	}

	envWarnings, err := applyEnv(&cfg, os.LookupEnv)
	if err != nil {
		return Config{}, err
	}
	cfg.Warnings = append(cfg.Warnings, envWarnings...)

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// checkFileMode returns a warning when a file holding credentials is readable by
// anyone other than its owner. A warning rather than an error: a permissive mode
// is worth telling the user about, but refusing to start would leave them with no
// protection at all, which is worse.
func checkFileMode(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Sprintf(
			"config file %s is readable by other users (mode %04o); it holds credentials — run: chmod 600 %s",
			path, perm, filepath.Clean(path))
	}
	return ""
}

// ErrNoConfig is returned when a configuration file is required but absent.
var ErrNoConfig = errors.New("config: no configuration file")

// Exists reports whether a readable file is present at path.
func Exists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir() && info.Mode()&fs.ModeType == 0
}
