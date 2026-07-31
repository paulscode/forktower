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
			name:   "a companion tower switched on",
			mutate: func(c *Config) { c.Tower.LND.Enabled = true },
			want:   "tower.lnd.enabled",
		},
		{
			name:   "the other companion tower",
			mutate: func(c *Config) { c.Tower.TEOS.Enabled = true },
			want:   "tower.teos.enabled",
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
