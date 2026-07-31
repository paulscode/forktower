package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// lnTower is the companion watchtower, as a Lightning node this tool can talk to.
//
// Its chain backend is the *other* chain — the one the user's own node does not
// follow — because that is where a breach would be published in these scenarios.
// A tower watching the same chain as the victim's node would be watching the
// wrong place entirely, which is the mistake this whole arm exists to prevent
// people making by hand.
var lnTower = lnNode{name: "tower", service: "tower"}

// towerRegisterTimeout bounds waiting for the client to negotiate a session.
//
// Generous on purpose. LND's session negotiator backs off exponentially, and a
// tower added after the client has already started can sit in that backoff for
// minutes — two and a half in the worst case measured — reporting zero sessions
// and zero backups the whole time, which looks exactly like a tower that is
// broken.
const towerRegisterTimeout = 5 * time.Minute

// commandTowerUp starts the watchtower and points the user's node at it.
//
// Registration happens here rather than being left to the scenario, because the
// ordering is load-bearing and easy to get wrong: a tower only holds the states
// that were revoked *after* the node registered with it, so registering after
// the payments have already happened produces a tower that is running, healthy,
// and holding nothing.
func commandTowerUp(ctx context.Context, register bool) error {
	say("starting the watchtower (its chain is the one your node does not follow)")
	if err := compose(ctx, "--profile", "tower", "up", "-d", "tower"); err != nil {
		return err
	}

	if err := lnTower.waitSynced(ctx, 3*time.Minute); err != nil {
		return fmt.Errorf("the watchtower did not catch up with its chain: %w", err)
	}

	uri, err := towerURI(ctx)
	if err != nil {
		return err
	}
	say("  tower is at %s", uri)

	if !register {
		// Left unregistered on purpose. A tower that is up, healthy, and pointed
		// at by nobody is the failure with no other symptom, and staging it is the
		// only way to check that Forktower says so.
		say("  left unregistered, so it is protecting nothing")
		return nil
	}

	user, err := lnByName(lnUser)
	if err != nil {
		return err
	}
	// Adding a tower that is already registered is not an error, so this is safe
	// to run again — which matters, because a scenario that failed halfway is one
	// somebody re-runs.
	if err := user.lncli(ctx, nil, "wtclient", "add", uri); err != nil &&
		!strings.Contains(err.Error(), "already exists") {
		return fmt.Errorf("registering the user's node with the tower: %w", err)
	}
	say("  the user's node is registered with it")
	return nil
}

// towerInfo is what the tower says about itself.
type towerInfo struct {
	Pubkey string   `json:"pubkey"`
	URIs   []string `json:"uris"`
}

// towerURI is the address the user's node registers with.
func towerURI(ctx context.Context) (string, error) {
	var info towerInfo
	if err := lnTower.lncli(ctx, &info, "tower", "info"); err != nil {
		return "", fmt.Errorf("asking the watchtower where it is: %w", err)
	}
	if info.Pubkey == "" {
		return "", errors.New("the watchtower did not say what its identity is")
	}
	// Built from the pubkey and the service name rather than taken from `uris`,
	// which reports the container's address on whatever network Docker gave it
	// today. The name is stable and is what the other containers resolve.
	return info.Pubkey + "@tower:9911", nil
}

// clientStats is what the user's node says it has backed up.
type clientStats struct {
	NumBackups        uint32 `json:"num_backups"`
	NumPendingBackups uint32 `json:"num_pending_backups"`
	NumFailedBackups  uint32 `json:"num_failed_backups"`
	NumSessionsAcq    uint32 `json:"num_sessions_acquired"`
	NumSessionsExh    uint32 `json:"num_sessions_exhausted"`
}

// commandTowerBackups waits until the user's node has backed up at least `min`
// states.
//
// A separate step from starting the tower because the two are separated by the
// payments that produce the states, and because waiting for it explicitly is the
// only way to make a breach scenario deterministic: staging the attack before
// the backups have landed produces a tower that correctly punishes nothing.
func commandTowerBackups(ctx context.Context, minimum int) error {
	user, err := lnByName(lnUser)
	if err != nil {
		return err
	}

	say("waiting for at least %d channel states to reach the watchtower", minimum)
	deadline := time.Now().Add(towerRegisterTimeout)
	for {
		var stats clientStats
		if err := user.lncli(ctx, &stats, "wtclient", "stats"); err != nil {
			return fmt.Errorf("asking the node what it has backed up: %w", err)
		}
		if int(stats.NumBackups) >= minimum {
			say("  %d states backed up across %d session(s)",
				stats.NumBackups, stats.NumSessionsAcq)
			return nil
		}
		if stats.NumFailedBackups > 0 {
			return fmt.Errorf("the node failed to back up %d states to the tower",
				stats.NumFailedBackups)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf(
				"after %s the node had backed up %d states, wanted %d "+
					"(sessions acquired: %d) — a tower registered after the client "+
					"started can sit in backoff for minutes",
				towerRegisterTimeout, stats.NumBackups, minimum, stats.NumSessionsAcq)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// commandTowerStop takes the watchtower away.
//
// The negative case, and the one worth staging: a tower that is not there is
// indistinguishable from a working one unless somebody is checking, which is
// exactly the failure this arm exists to catch.
func commandTowerStop(ctx context.Context) error {
	say("stopping the watchtower")
	return compose(ctx, "--profile", "tower", "stop", "tower")
}

// commandTowerStatus prints what both ends think.
func commandTowerStatus(ctx context.Context) error {
	uri, err := towerURI(ctx)
	if err != nil {
		say("tower: not answering (%v)", err)
	} else {
		say("tower: %s", uri)
	}

	user, err := lnByName(lnUser)
	if err != nil {
		return err
	}
	var stats clientStats
	if err := user.lncli(ctx, &stats, "wtclient", "stats"); err != nil {
		return fmt.Errorf("asking the node what it has backed up: %w", err)
	}
	say("user's node: %d backed up, %d pending, %d failed, %d session(s)",
		stats.NumBackups, stats.NumPendingBackups, stats.NumFailedBackups,
		stats.NumSessionsAcq)
	return nil
}
