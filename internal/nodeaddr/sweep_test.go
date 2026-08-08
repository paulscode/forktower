package nodeaddr_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every client that holds a node's address must be able to follow it.
//
// **Because the same defect shipped twice.** The registry's LND client learned
// to re-resolve in 0.6.11; its sibling in the tower package kept dialling a
// container address that no longer existed and shipped the identical bug a day
// later. A user saw a healthy dashboard — the channel list had recovered — and a
// recurring "no route to host" in the log from the check that had not.
//
// This looks at the source rather than at behaviour, which is unusual and
// deliberate: what went wrong was not a client behaving badly but a client
// nobody remembered to change. A test of behaviour cannot fail for code that
// does not exist yet; this one can.
//
// A client that legitimately dials a name rather than a resolved address does
// not need a Follower. Say so here, by name, so that the exemption is a decision
// somebody made rather than an omission.
func TestEveryClientHoldingAnAddressCanFollowIt(t *testing.T) {
	t.Parallel()

	// Clients whose address is a name the packaging never resolves, so there is
	// nothing to look up again.
	exempt := map[string]string{
		"internal/responder/tower/teos.go": "the teos tower is reached at the " +
			"address the user configured, which is theirs and not a container's",
		"internal/registry/cln/client.go": "clnrest is configured by address on " +
			"every packaging that supports it, and none resolves a name first",
	}

	root := filepath.Join("..", "..")
	var missing []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		switch {
		case err != nil:
			return err
		case info.IsDir(), !strings.HasSuffix(path, ".go"),
			strings.HasSuffix(path, "_test.go"):
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		text := string(body)
		// The shape this is looking for: a client that builds requests against a
		// base address it was handed.
		if !strings.Contains(text, "http.NewRequestWithContext") ||
			!strings.Contains(text, "BaseURL") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		if _, ok := exempt[rel]; ok {
			return nil
		}
		// **The call, not the import.** Checking for the package name alone
		// passes a client that declares a Follower field and never asks it
		// anything — which is exactly the half-done state a hurried fix leaves.
		if !strings.Contains(text, ".Moved(ctx)") {
			missing = append(missing, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(missing) > 0 {
		t.Errorf("these dial a node at an address they were handed and cannot "+
			"follow it if it moves: %v\n\nUse nodeaddr.Follower, or add the file "+
			"to the exempt list above with the reason it does not need one.",
			missing)
	}
}
