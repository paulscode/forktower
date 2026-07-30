package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// ValidationError reports every problem found, not just the first. Someone
// fixing a config file should learn about all of it in one pass rather than
// discovering the next fault only after correcting the last.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	if len(e.Problems) == 1 {
		return "invalid configuration: " + e.Problems[0]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "invalid configuration (%d problems):", len(e.Problems))
	for _, p := range e.Problems {
		b.WriteString("\n  - ")
		b.WriteString(p)
	}
	return b.String()
}

// Validate checks the whole configuration, accumulating problems.
func (c Config) Validate() error {
	var p []string

	p = append(p, c.validateSF()...)
	p = append(p, c.validateSQ()...)
	p = append(p, c.validateSentinel()...)
	p = append(p, c.validateAlerts()...)
	p = append(p, c.validateUI()...)
	p = append(p, c.validateStore()...)
	p = append(p, c.validateLog()...)

	if len(p) > 0 {
		return &ValidationError{Problems: p}
	}
	return nil
}

func (c Config) validateSF() []string {
	var p []string
	if c.SF.RPCURL == "" {
		p = append(p, "sf.rpc_url is required (the Bitcoin node Forktower reads from)")
	} else if err := validateRPCURL(c.SF.RPCURL); err != nil {
		p = append(p, fmt.Sprintf("sf.rpc_url is not a usable URL: %v", err))
	}
	p = append(p, validateRPCAuth("sf", c.SF)...)
	return p
}

func (c Config) validateSQ() []string {
	var p []string
	switch c.SQ.Tier {
	case TierBitcoind:
		if c.SQ.Bitcoind.RPCURL == "" {
			p = append(p, "sq.bitcoind.rpc_url is required when sq.tier is \"bitcoind\"")
		} else if err := validateRPCURL(c.SQ.Bitcoind.RPCURL); err != nil {
			p = append(p, fmt.Sprintf("sq.bitcoind.rpc_url is not a usable URL: %v", err))
		}
		p = append(p, validateRPCAuth("sq.bitcoind", c.SQ.Bitcoind)...)
	case TierNeutrino, TierElectrum:
		p = append(p, fmt.Sprintf(
			"sq.tier %q is not implemented yet; only %q is accepted", c.SQ.Tier, TierBitcoind))
	case "":
		p = append(p, fmt.Sprintf("sq.tier is required; only %q is accepted", TierBitcoind))
	default:
		p = append(p, fmt.Sprintf(
			"sq.tier %q is not a known tier; only %q is accepted", c.SQ.Tier, TierBitcoind))
	}
	return p
}

// validateRPCAuth enforces exactly one authentication method per endpoint.
// Both is ambiguous about which we would use, and neither leaves us unable to
// connect; either way the user should be told rather than have one silently win.
func validateRPCAuth(section string, e RPCEndpoint) []string {
	cookie, userpass := e.HasCookieAuth(), e.HasUserPassAuth()
	switch {
	case cookie && userpass:
		return []string{fmt.Sprintf(
			"%s: set either rpc_cookie_path or rpc_user+rpc_pass for auth, not both", section)}
	case !cookie && !userpass:
		return []string{fmt.Sprintf(
			"%s: no auth configured; set rpc_cookie_path or rpc_user+rpc_pass", section)}
	case userpass && (e.RPCUser == "" || e.RPCPass == ""):
		return []string{fmt.Sprintf(
			"%s: auth needs both rpc_user and rpc_pass", section)}
	}
	return nil
}

func validateRPCURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("no host")
	}
	return nil
}

func (c Config) validateSentinel() []string {
	var p []string
	s := c.Sentinel
	if s.SplitConfirmDepth < 1 || s.SplitConfirmDepth > 100 {
		p = append(p, fmt.Sprintf(
			"sentinel.split_confirm_depth %d is out of range [1, 100]", s.SplitConfirmDepth))
	}
	if s.PollIntervalSecs < 1 {
		p = append(p, fmt.Sprintf(
			"sentinel.poll_interval_secs %d is out of range (must be at least 1)", s.PollIntervalSecs))
	}
	if s.MaxAncestorWalk < 1 {
		p = append(p, fmt.Sprintf(
			"sentinel.max_ancestor_walk %d is out of range (must be at least 1)", s.MaxAncestorWalk))
	}
	if s.SQStallFactor <= 0 {
		p = append(p, fmt.Sprintf(
			"sentinel.sq_stall_factor %v is out of range (must be greater than 0)", s.SQStallFactor))
	}
	// Zero means "choose for me"; a negative is a mistake worth reporting.
	if s.ReorgMargin < 0 {
		p = append(p, fmt.Sprintf(
			"sentinel.reorg_margin %d is out of range (must be at least 1, or 0 to choose automatically)",
			s.ReorgMargin))
	}
	return p
}

// Valid reports whether t is a transport the daemon understands.
//
// Written as an exhaustive switch rather than a lookup table so that adding a
// transport without deciding whether it is valid here becomes a lint failure
// instead of a silent rejection at runtime.
func (t TransportType) Valid() bool {
	switch t {
	case TransportNtfy, TransportWebhook, TransportSMTP, TransportTelegram,
		TransportStartOS, TransportUmbrel:
		return true
	default:
		return false
	}
}

// Valid reports whether m is an accepted minimum severity.
//
// The alert tiers themselves are a longer list: "resolved" and "loss" are ranked
// against these rather than being selectable, so a user who asks for
// critical-only still hears about a loss and is not paged about a resolution.
func (m MinTier) Valid() bool {
	switch m {
	case MinTierInfo, MinTierWarning, MinTierCritical:
		return true
	default:
		return false
	}
}

func (c Config) validateAlerts() []string {
	var p []string
	if c.Alerts.SelfTestIntervalHours < 1 {
		p = append(p, fmt.Sprintf(
			"alerts.self_test_interval_hours %d is out of range (must be at least 1)",
			c.Alerts.SelfTestIntervalHours))
	}
	if c.Alerts.CriticalRepeatMins < 1 {
		p = append(p, fmt.Sprintf(
			"alerts.critical_repeat_mins %d is out of range (must be at least 1)",
			c.Alerts.CriticalRepeatMins))
	}

	seen := make(map[string]bool, len(c.Alerts.Transport))
	for i, t := range c.Alerts.Transport {
		where := fmt.Sprintf("alerts.transport[%d]", i)
		switch {
		case t.Name == "":
			p = append(p, where+": transport name is required")
		case seen[t.Name]:
			p = append(p, fmt.Sprintf("%s: duplicate transport name %q", where, t.Name))
		default:
			seen[t.Name] = true
		}
		if !t.Type.Valid() {
			p = append(p, fmt.Sprintf(
				"%s (transport name %q): unknown type %q", where, t.Name, t.Type))
		}
		if t.MinTier != "" && !t.MinTier.Valid() {
			p = append(p, fmt.Sprintf(
				"%s (transport name %q): min_tier %q must be info, warning or critical",
				where, t.Name, t.MinTier))
		}
	}
	return p
}

func (c Config) validateUI() []string {
	var p []string

	host, _, err := net.SplitHostPort(c.UI.Listen)
	if err != nil {
		p = append(p, fmt.Sprintf("ui.listen %q is not host:port: %v", c.UI.Listen, err))
		return p
	}

	loopback := isLoopbackHost(host)

	switch c.UI.Auth {
	case AuthNone:
		if !loopback {
			p = append(p, fmt.Sprintf(
				"ui.auth is %q but ui.listen %q is not loopback — that would serve the dashboard "+
					"unauthenticated on the network. Use %q when a platform proxy fronts this port, "+
					"or %q to require a password.",
				AuthNone, c.UI.Listen, AuthPlatform, AuthPassword))
		}
	case AuthPlatform:
		if loopback {
			p = append(p, fmt.Sprintf(
				"ui.auth is %q but ui.listen %q is loopback, so no proxy can reach it — use %q instead",
				AuthPlatform, c.UI.Listen, AuthNone))
		}
	case AuthPassword:
		if c.UI.PasswordHash == "" {
			p = append(p, fmt.Sprintf("ui.auth is %q but ui.password_hash is empty", AuthPassword))
		} else if _, err := bcrypt.Cost([]byte(c.UI.PasswordHash)); err != nil {
			p = append(p, fmt.Sprintf("ui.auth is %q but ui.password_hash is not a bcrypt hash: %v",
				AuthPassword, err))
		}
	case "":
		p = append(p, fmt.Sprintf("ui.auth is required; one of %q, %q or %q",
			AuthNone, AuthPlatform, AuthPassword))
	default:
		p = append(p, fmt.Sprintf("ui.auth %q is unknown; one of %q, %q or %q",
			c.UI.Auth, AuthNone, AuthPlatform, AuthPassword))
	}
	return p
}

// isLoopbackHost reports whether a listen host is confined to this machine. An
// empty host means "all interfaces", which is not loopback.
func isLoopbackHost(host string) bool {
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func (c Config) validateStore() []string {
	var p []string
	if c.Store.Path == "" {
		p = append(p, "store.path is required")
	}
	if c.Store.TimelineMaxMB < 1 {
		p = append(p, fmt.Sprintf(
			"store.timeline_max_mb %d is out of range (must be at least 1)", c.Store.TimelineMaxMB))
	}
	return p
}

// Valid reports whether l is an accepted verbosity.
func (l LogLevel) Valid() bool {
	switch l {
	case LogDebug, LogInfo, LogWarn, LogError:
		return true
	default:
		return false
	}
}

func (c Config) validateLog() []string {
	if !c.Log.Level.Valid() {
		return []string{fmt.Sprintf(
			"log.level %q is unknown; one of debug, info, warn or error", c.Log.Level)}
	}
	return nil
}
