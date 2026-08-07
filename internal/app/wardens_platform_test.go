package app

import (
	"testing"

	"github.com/paulscode/forktower/internal/config"
)

// Which packagings claim they can attach a Tor address to the tower. Pinned
// because the claim is made in a message the user is asked to act on: saying it
// where it is false sends somebody hunting for a screen that does not exist, and
// nothing else in the build would catch that.
func TestOnlyStartOS04ClaimsItCanAttachAnOnion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		platform config.Platform
		want     bool
	}{
		{config.PlatformStartOS04, true},
		// Its node dials local addresses directly, and its entrypoint advertises
		// the one address it has. No onion is ever requested.
		{config.PlatformStartOS035, false},
		// Same, and its container address is pinned by the app definition.
		{config.PlatformUmbrel, false},
		// Nothing here knows what a self-hosted deployment could do.
		{config.PlatformUnknown, false},
	} {
		if got := canAttachOnion(tc.platform); got != tc.want {
			t.Errorf("canAttachOnion(%q) = %v, want %v", tc.platform, got, tc.want)
		}
	}
}
