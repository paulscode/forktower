package config

import (
	"strings"
	"testing"
)

// A container has to bind every address to be reachable at all, and what decides
// its exposure is the port publishing — which this process cannot see. So the
// only way to serve an unauthenticated dashboard off loopback is for the operator
// to say plainly that something else restricts it.
func TestServingWithoutAPasswordOffLoopback(t *testing.T) {
	t.Parallel()

	base := func() Config {
		c := Default()
		c.SF.RPCURL = "http://127.0.0.1:8332"
		c.SF.RPCUser, c.SF.RPCPass = "u", "p"
		c.SQ.Bitcoind.RPCURL = "http://127.0.0.1:8432"
		c.SQ.Bitcoind.RPCUser, c.SQ.Bitcoind.RPCPass = "u", "p"
		c.UI.Auth = AuthNone
		c.UI.Listen = "0.0.0.0:8330"
		return c
	}

	// Refused by default: this is exactly the accident the check exists for.
	err := base().Validate()
	if err == nil {
		t.Fatal("an unauthenticated dashboard on every address was accepted")
	}
	if !strings.Contains(err.Error(), "access_restricted_externally") {
		t.Errorf("the error does not say how to proceed deliberately: %v", err)
	}

	// Permitted when the operator says so.
	acknowledged := base()
	acknowledged.UI.AccessRestrictedExternally = true
	if err := acknowledged.Validate(); err != nil {
		t.Errorf("an acknowledged restriction was still refused: %v", err)
	}

	// And the acknowledgement does not excuse anything else: it says something
	// about who can reach the port, not about which mode is coherent.
	stillWrong := base()
	stillWrong.UI.AccessRestrictedExternally = true
	stillWrong.UI.Auth = AuthPlatform
	stillWrong.UI.Listen = "127.0.0.1:8330"
	if err := stillWrong.Validate(); err == nil {
		t.Error("a platform proxy was accepted against an address it cannot reach")
	}

	// On loopback it changes nothing, because there was nothing to permit.
	onLoopback := base()
	onLoopback.UI.Listen = "127.0.0.1:8330"
	if err := onLoopback.Validate(); err != nil {
		t.Errorf("a loopback dashboard was refused: %v", err)
	}
}

// No Lightning node at all is a supported arrangement: split detection works
// without one. A half-configured node is not — it looks connected in the file
// and reads nothing at run time.
func TestALightningNodeMustBeFullyConfiguredOrAbsent(t *testing.T) {
	t.Parallel()

	base := minimalValid()
	if err := base.Validate(); err != nil {
		t.Fatalf("the baseline configuration should be valid: %v", err)
	}

	tests := []struct {
		name    string
		ln      LNConfig
		wantErr string
	}{
		{name: "none configured", ln: LNConfig{}},
		{
			name: "a complete LND node",
			ln: LNConfig{LND: []LNDConfig{{
				RESTAddr: "https://127.0.0.1:8080", MacaroonPath: "/creds/readonly.macaroon",
			}}},
		},
		{
			name: "a complete Core Lightning node",
			ln: LNConfig{CLN: []CLNConfig{{
				RESTAddr: "https://127.0.0.1:3010", RunePath: "/creds/forktower.rune",
			}}},
		},
		{
			name:    "an LND node with no address",
			ln:      LNConfig{LND: []LNDConfig{{MacaroonPath: "/creds/readonly.macaroon"}}},
			wantErr: "ln.lnd[0].rest_addr",
		},
		{
			name:    "an LND node with no credential",
			ln:      LNConfig{LND: []LNDConfig{{RESTAddr: "https://127.0.0.1:8080"}}},
			wantErr: "ln.lnd[0].macaroon_path",
		},
		{
			name: "an LND node whose address is not a URL",
			ln: LNConfig{LND: []LNDConfig{{
				RESTAddr: "127.0.0.1:8080", MacaroonPath: "/creds/readonly.macaroon",
			}}},
			wantErr: "ln.lnd[0].rest_addr",
		},
		{
			name:    "a Core Lightning node with no rune",
			ln:      LNConfig{CLN: []CLNConfig{{RESTAddr: "https://127.0.0.1:3010"}}},
			wantErr: "ln.cln[0].rune_path",
		},
		{
			// Two nodes of the same implementation is supported, and the message has
			// to say *which* one is wrong or it is no help at all.
			name: "the second of two nodes is the broken one",
			ln: LNConfig{LND: []LNDConfig{
				{RESTAddr: "https://127.0.0.1:8080", MacaroonPath: "/a.macaroon"},
				{RESTAddr: "https://127.0.0.1:8081"},
			}},
			wantErr: "ln.lnd[1].macaroon_path",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := minimalValid()
			cfg.LN = tc.ln
			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("refused a usable configuration: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted a half-configured node")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestConfiguredReportsWhetherAnyLightningNodeIsSetUp(t *testing.T) {
	t.Parallel()

	if (LNConfig{}).Configured() {
		t.Error("an empty Lightning section reported a node")
	}
	if !(LNConfig{LND: []LNDConfig{{}}}).Configured() {
		t.Error("an LND node was not reported")
	}
	if !(LNConfig{CLN: []CLNConfig{{}}}).Configured() {
		t.Error("a Core Lightning node was not reported")
	}
}

// Configuration that is accepted and then ignored is the failure this project is
// about. Not a validation error — somebody may be preparing ahead of a milestone
// — but never silence either.
func TestSettingsThatDoNothingAreReported(t *testing.T) {
	t.Parallel()

	clean := minimalValid()
	if got := clean.UnusedSettings(); len(got) != 0 {
		t.Errorf("an ordinary configuration reported %v", got)
	}

	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{
			name: "a companion tower switched on",
			mutate: func(c *Config) {
				// Complete enough to be valid: a tower that is enabled has to
				// say where it listens and where Forktower reads it. It is
				// still not wired into the daemon, which is the separate thing
				// UnusedSettings is reporting.
				c.Tower.LND = TowerInstance{
					Enabled: true, Listen: "abc.onion:9911",
					APIURL: "https://tower-lnd:8080",
				}
			},
			want: "tower.lnd.enabled",
		},
		{
			name: "the other companion tower",
			mutate: func(c *Config) {
				c.Tower.TEOS = TowerInstance{
					Enabled: true, Listen: "abc.onion:9814",
					APIURL: "http://tower-teos:9814",
				}
			},
			want: "tower.teos.enabled",
		},
		{
			name:   "an independent second opinion switched on",
			mutate: func(c *Config) { c.SQ.Witnesses.NeutrinoHeaders = true },
			want:   "sq.witnesses.neutrino_headers",
		},
		{
			name:   "electrum servers listed",
			mutate: func(c *Config) { c.SQ.Witnesses.Electrum = []string{"a:1"} },
			want:   "sq.witnesses.electrum",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := minimalValid()
			tc.mutate(&cfg)

			// Still a usable configuration: this is a warning, not a refusal.
			if err := cfg.Validate(); err != nil {
				t.Fatalf("refused a configuration it should only warn about: %v", err)
			}

			got := cfg.UnusedSettings()
			if len(got) != 1 || !strings.Contains(got[0], tc.want) {
				t.Fatalf("reported %v, want one note naming %q", got, tc.want)
			}
			// And says what it means, not just that something is unused.
			if !strings.Contains(got[0], "not built yet") {
				t.Errorf("the note does not say why: %q", got[0])
			}
		})
	}
}

// A wildcard listener is refused, not warned about.
//
// LND's watchtower accepts a session from anyone who completes the handshake:
// no client allowlist, no session cap, no disk cap (R2). Where it listens is
// therefore the only line of defence, and something a user can click past is not
// a line of defence.
func TestAWildcardTowerListenerIsRefused(t *testing.T) {
	t.Parallel()

	for _, listen := range []string{
		"0.0.0.0:9911", "0.0.0.0", "::", "[::]:9911", ":9911", "*:9911",
	} {
		cfg := minimalValid()
		cfg.Tower.LND.Listen = listen

		err := cfg.Validate()
		if err == nil {
			t.Errorf("%q was accepted as a tower listen address", listen)
			continue
		}
		if !strings.Contains(err.Error(), "every interface") {
			t.Errorf("%q was refused, but not for the right reason: %v", listen, err)
		}
	}
}

// Refused even when the tower is switched off: a wildcard sitting in the file is
// one flag away from being live.
func TestAWildcardIsRefusedEvenWhileTheTowerIsOff(t *testing.T) {
	t.Parallel()
	cfg := minimalValid()
	cfg.Tower.LND.Enabled = false
	cfg.Tower.LND.Listen = "0.0.0.0:9911"

	if err := cfg.Validate(); err == nil {
		t.Error("a wildcard listener was accepted because the tower was switched off")
	}
}

// The whole point of the rule is that a deliberate address is fine. Refusing
// those too would push people back to the wildcard.
func TestADeliberateTowerAddressIsAccepted(t *testing.T) {
	t.Parallel()

	for _, listen := range []string{
		"xyzabc123.onion:9911", "192.168.1.50:9911", "127.0.0.1:9911",
		"[fd00::1]:9911", "tower-lnd:9911",
	} {
		cfg := minimalValid()
		cfg.Tower.LND = TowerInstance{
			Enabled: true, Listen: listen, APIURL: "https://tower-lnd:8080",
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("%q was refused as a tower listen address: %v", listen, err)
		}
	}
}

// An enabled tower has to say enough about itself to be watched, because a tower
// nobody is watching is the failure this whole arm exists to prevent.
func TestAnEnabledTowerMustSayWhereItIs(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		tower TowerInstance
		want  string
	}{
		{"no api url", TowerInstance{Enabled: true, Listen: "a.onion:9911"}, "api_url is required"},
		{"no listen", TowerInstance{Enabled: true, APIURL: "https://t:8080"}, "listen is required"},
		{
			"an api url that is not one",
			TowerInstance{Enabled: true, Listen: "a.onion:9911", APIURL: "not a url"},
			"not a usable URL",
		},
		{
			"a negative disk limit",
			TowerInstance{
				Enabled: true, Listen: "a.onion:9911",
				APIURL: "https://t:8080", MaxDiskMB: -1,
			},
			"not a limit",
		},
	} {
		cfg := minimalValid()
		cfg.Tower.LND = tc.tower
		err := cfg.Validate()
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
}

// A disabled tower with nothing filled in is the shipped default and must stay
// valid.
func TestTheDefaultTowerConfigurationIsValid(t *testing.T) {
	t.Parallel()
	cfg := minimalValid()
	if err := cfg.Validate(); err != nil {
		t.Errorf("the default configuration was refused: %v", err)
	}
	if got := cfg.Tower.LND.DiskLimitMB(); got != DefaultTowerMaxDiskMB {
		t.Errorf("unset disk limit resolved to %d, want %d", got, DefaultTowerMaxDiskMB)
	}
	cfg.Tower.LND.MaxDiskMB = 512
	if got := cfg.Tower.LND.DiskLimitMB(); got != 512 {
		t.Errorf("an explicit disk limit resolved to %d, want 512", got)
	}
}
