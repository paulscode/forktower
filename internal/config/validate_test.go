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
