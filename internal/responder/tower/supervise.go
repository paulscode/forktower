// Package tower watches the companion watchtowers that answer a breach.
//
// Forktower does not *run* these processes: they are separate services started
// by whatever supervises the rest of the deployment, and reaching for a
// container runtime socket to control them would hand this daemon far more
// authority than it needs. What it does instead is watch them, and say so when
// they are not doing their job — which is the failure mode that matters, because
// a watchtower that has quietly stopped working looks exactly like one that is
// working.
package tower

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/paulscode/forktower/internal/store"
)

// notActiveMarker is what LND says when the watchtower is switched off.
//
// The tower subserver returns this rather than failing to answer, which makes an
// ordinary read a clean probe for "is the watchtower actually on" — no mutating
// call, no guessing. The same shape as the wtclient side, where
// `ErrWtclientNotActive` answers the matching question about the user's node.
const notActiveMarker = "watchtower not active"

// startingMarker is what LND says while it is coming up.
//
// **It is telling us plainly that it is starting, and we were calling it
// stopped.** Every read is an error until lnd finishes opening its subservers,
// and mapping those to "unreachable" put "your watchtower has stopped answering"
// in front of a user whose tower was doing nothing wrong — on a first run, and
// again on every restart, where the stored status is "reachable" and so the
// startup guard in the alerting cannot help either.
//
// Matched on prose, like the marker above, because that is what the interface
// offers: it arrives as a 500 carrying a gRPC status whose code is shared by
// half a dozen unrelated refusals, and the sentence is the only part that
// identifies this one.
const startingMarker = "in the process of starting up"

// ErrTowerNotActive means the tower process is running but its watchtower is
// switched off. A different problem from being unreachable, and a different
// thing to tell the user: the remedy is a setting, not a restart.
var ErrTowerNotActive = errors.New("the tower is running but its watchtower is switched off")

// Identity is what a tower says about itself.
type Identity struct {
	// Pubkey is the tower's identity key. Not the node's — LND derives a
	// separate one for the tower — so it cannot be read off the node info.
	Pubkey string
	// Listeners are the addresses it is bound to. URIs are what a client would
	// actually paste, and are what the registration wizard shows.
	Listeners []string
	URIs      []string
}

// Chain is what the tower's own node knows about the chain it watches.
//
// It matters because a tower that is not synced cannot see a breach: the lookout
// works from block notifications, so a tower behind the tip is one that will
// punish nothing until it catches up.
type Chain struct {
	Version       string
	BlockHeight   int32
	SyncedToChain bool
}

// Reader is the part of a tower's own API this package needs.
//
// An interface because the two implementations answer differently and because a
// supervisor that can only be tested against a live watchtower is one that will
// not be tested.
type Reader interface {
	// Identity returns what the tower says about itself, or ErrTowerNotActive if
	// the process is up but the watchtower is off.
	Identity(ctx context.Context) (Identity, error)
	// Chain returns the sync state of the node the tower runs on.
	Chain(ctx context.Context) (Chain, error)
}

// DiskUsage measures a directory. Separate from Reader because it is answered
// from the filesystem rather than from the tower, and because a deployment where
// Forktower cannot see the directory is a normal one that must degrade honestly
// rather than claim everything is fine.
type DiskUsage func(dir string) (int64, error)

// Observation is one pass of watching a tower.
type Observation struct {
	Identity Identity
	Chain    Chain
	Health   store.TowerHealth

	// UsedBytes and LimitBytes describe the tower's storage. LimitBytes is zero
	// when the directory is not visible from here, which is not the same as
	// being within the limit.
	UsedBytes  int64
	LimitBytes int64
	DiskKnown  bool
	// OverDiskLimit is the cap LND does not impose on itself: it has no client
	// allowlist, no session cap and no disk cap, so anyone completing the
	// handshake gets a session and consumes disk on the same host as the Bitcoin
	// node the user depends on.
	OverDiskLimit bool
}

// NearDiskLimit is the fraction of the cap at which it is worth saying
// something.
//
// Well before the limit, because the useful moment is while there is still time
// to do something unhurried about it. A warning that arrives as the disk fills
// is a notification of a problem rather than a chance to avoid one.
const NearDiskLimit = 0.80

// Supervisor watches one tower.
type Supervisor struct {
	kind    store.TowerKind
	reader  Reader
	dataDir string
	limitMB int64
	usage   DiskUsage
}

// Options configures a Supervisor.
type Options struct {
	Kind    store.TowerKind
	Reader  Reader
	DataDir string
	LimitMB int64
	// Usage measures the data directory. Nil means [DirectorySize].
	Usage DiskUsage
}

// New builds a Supervisor.
func New(opts Options) (*Supervisor, error) {
	if !opts.Kind.Valid() {
		return nil, fmt.Errorf("tower: %q is not a watchtower kind", opts.Kind)
	}
	if opts.Reader == nil {
		return nil, errors.New("tower: a supervisor needs something to read the tower with")
	}
	usage := opts.Usage
	if usage == nil {
		usage = DirectorySize
	}
	return &Supervisor{
		kind:    opts.Kind,
		reader:  opts.Reader,
		dataDir: opts.DataDir,
		limitMB: opts.LimitMB,
		usage:   usage,
	}, nil
}

// Observe takes one look at the tower.
//
// Never returns an error for the tower being in a bad state — that is the
// answer, not a failure to produce one. An error here means the observation
// itself could not be made.
func (s *Supervisor) Observe(ctx context.Context) Observation {
	obs := Observation{Health: store.TowerHealth{Status: store.TowerStatusUnknown}}

	identity, err := s.reader.Identity(ctx)
	switch {
	case errors.Is(err, ErrTowerNotActive):
		// Running, but not being a watchtower. Reachable would be a lie and
		// unreachable would send the user looking in the wrong place.
		obs.Health.Status = store.TowerUnreachable
		obs.Health.Detail = "the tower process is running but its watchtower is " +
			"switched off, so it is accepting no sessions"
		s.measureDisk(&obs)
		return obs
	case isStillStarting(err):
		// Not down. Coming up, and saying so.
		obs.Health.Status = store.TowerTemporarilyUnreachable
		obs.Health.Detail = "the tower is still starting up"
		s.measureDisk(&obs)
		return obs
	case err != nil:
		obs.Health.Status = store.TowerUnreachable
		obs.Health.Detail = "the tower did not answer: " + err.Error()
		s.measureDisk(&obs)
		return obs
	}
	obs.Identity = identity

	chain, err := s.reader.Chain(ctx)
	if err != nil {
		obs.Health.Status = store.TowerUnreachable
		obs.Health.Detail = "the tower answered but its node did not: " + err.Error()
		s.measureDisk(&obs)
		return obs
	}
	obs.Chain = chain

	switch {
	case !chain.SyncedToChain:
		// Up, listening, and blind. The lookout works from block notifications,
		// so a tower behind the tip punishes nothing until it catches up — and
		// it will keep accepting backups the whole time, which is what makes
		// this worth saying out loud.
		obs.Health.Status = store.TowerTemporarilyUnreachable
		obs.Health.Detail = fmt.Sprintf(
			"the tower is running but its node is still catching up with the "+
				"chain (height %d), so it would not see a breach yet",
			chain.BlockHeight)
	case len(identity.URIs) == 0:
		obs.Health.Status = store.TowerTemporarilyUnreachable
		obs.Health.Detail = "the tower is running but is not reachable at any " +
			"address yet, so no node can register with it"
	default:
		obs.Health.Status = store.TowerReachable
	}

	s.measureDisk(&obs)
	if obs.OverDiskLimit && obs.Health.Status == store.TowerReachable {
		// Still answering, so not unreachable — but it is over a limit we
		// imposed because LND imposes none, and carrying on quietly is how the
		// host runs out of disk under the Bitcoin node.
		obs.Health.Detail = fmt.Sprintf(
			"the tower is working, but its storage has passed the %d MB limit "+
				"Forktower sets for it", s.limitMB)
	}
	return obs
}

// measureDisk fills in the storage part of an observation.
//
// A directory Forktower cannot see leaves DiskKnown false rather than a zero
// that reads as "empty, nothing to worry about". Not being able to check is its
// own answer and the UI says so.
func (s *Supervisor) measureDisk(obs *Observation) {
	if s.dataDir == "" || s.limitMB <= 0 {
		return
	}
	used, err := s.usage(s.dataDir)
	if err != nil {
		return
	}
	const bytesPerMB = 1 << 20
	obs.UsedBytes = used
	obs.LimitBytes = s.limitMB * bytesPerMB
	obs.DiskKnown = true
	obs.OverDiskLimit = used > obs.LimitBytes
}

// NearingDiskLimit reports whether the storage is far enough along to mention.
func (o Observation) NearingDiskLimit() bool {
	if !o.DiskKnown || o.LimitBytes <= 0 {
		return false
	}
	return float64(o.UsedBytes) >= float64(o.LimitBytes)*NearDiskLimit
}

// DirectorySize adds up everything under a directory.
//
// Counts file sizes rather than blocks on disk: the number is being compared
// against a limit we chose, and an approximation that is stable across
// filesystems is worth more here than an exact figure that is not. Unreadable
// entries are skipped rather than failing the whole measurement, because a
// tower's storage being partly unreadable is still worth reporting a size for.
func DirectorySize(dir string) (int64, error) {
	if dir == "" {
		return 0, errors.New("tower: no directory to measure")
	}
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory that vanished mid-walk, or one we may not read. Neither
			// is a reason to have no answer at all.
			if errors.Is(err, fs.ErrPermission) || errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			// A file that vanished between being listed and being asked about.
			// Skipping it costs a few bytes of accuracy in a number that is
			// being compared against a limit we chose; failing the whole
			// measurement would cost the answer entirely.
			//nolint:nilerr // deliberate: one unreadable entry must not lose the total
			return nil
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("measuring the tower's storage: %w", err)
	}
	return total, nil
}

// IsNotActive reports whether an API error means the watchtower is switched off.
//
// Matched on the message because that is what the wire carries: the tower's API
// returns a plain error string, and there is no code to key on.
func IsNotActive(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), notActiveMarker)
}

// isStillStarting reports whether the tower is coming up rather than absent.
func isStillStarting(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), startingMarker)
}
