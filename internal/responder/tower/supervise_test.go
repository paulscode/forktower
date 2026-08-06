package tower

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paulscode/forktower/internal/store"
)

// fakeTower answers however a test needs it to.
type fakeTower struct {
	identity    Identity
	identityErr error
	chain       Chain
	chainErr    error
}

func (f *fakeTower) Identity(context.Context) (Identity, error) {
	return f.identity, f.identityErr
}

func (f *fakeTower) Chain(context.Context) (Chain, error) {
	return f.chain, f.chainErr
}

func healthy() *fakeTower {
	return &fakeTower{
		identity: Identity{
			Pubkey:    "03f3660d3209930439f5c975615c4653460ab7d466a97338a133663ac1e4150890",
			Listeners: []string{"[::]:9911"},
			URIs:      []string{"03f366...@abcdef.onion:9911"},
		},
		chain: Chain{Version: "0.18.5-beta", BlockHeight: 900_000, SyncedToChain: true},
	}
}

func newSupervisor(t *testing.T, r Reader, opts ...func(*Options)) *Supervisor {
	t.Helper()
	o := Options{Kind: store.TowerLND, Reader: r}
	for _, fn := range opts {
		fn(&o)
	}
	s, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAWorkingTowerIsReachable(t *testing.T) {
	t.Parallel()
	obs := newSupervisor(t, healthy()).Observe(context.Background())

	if obs.Health.Status != store.TowerReachable {
		t.Errorf("status = %q (%s), want %q",
			obs.Health.Status, obs.Health.Detail, store.TowerReachable)
	}
	if obs.Identity.Pubkey == "" {
		t.Error("the tower's identity was not recorded")
	}
	if obs.Chain.Version != "0.18.5-beta" {
		t.Errorf("the tower's version was not recorded: %q", obs.Chain.Version)
	}
}

// A tower running with its watchtower switched off is a different problem from
// one that is unreachable, and the remedy is a setting rather than a restart.
func TestAWatchtowerSwitchedOffSaysSo(t *testing.T) {
	t.Parallel()
	f := healthy()
	f.identityErr = ErrTowerNotActive

	obs := newSupervisor(t, f).Observe(context.Background())

	if obs.Health.Status == store.TowerReachable {
		t.Error("a tower with its watchtower switched off was reported as working")
	}
	if !strings.Contains(obs.Health.Detail, "switched off") {
		t.Errorf("the detail does not say what is actually wrong: %q", obs.Health.Detail)
	}
	if strings.Contains(obs.Health.Detail, "did not answer") {
		t.Errorf("a tower that answered was described as silent: %q", obs.Health.Detail)
	}
}

// The message an LND tower returns is what the wire carries, so recognising it
// is what makes the probe work.
func TestTheNotActiveMessageIsRecognised(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("watchtower not active"), true},
		{errors.New("rpc error: code = Unknown desc = watchtower not active"), true},
		{errors.New("Watchtower Not Active"), true},
		{errors.New("connection refused"), false},
	} {
		if got := IsNotActive(tc.err); got != tc.want {
			t.Errorf("IsNotActive(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// Up, listening, and blind. The lookout works from block notifications, so a
// tower behind the tip punishes nothing — while still accepting backups, which
// is what makes it worth saying out loud rather than treating as "starting up".
func TestATowerThatIsBehindTheChainIsNotReportedAsWorking(t *testing.T) {
	t.Parallel()
	f := healthy()
	f.chain.SyncedToChain = false
	f.chain.BlockHeight = 899_000

	obs := newSupervisor(t, f).Observe(context.Background())

	if obs.Health.Status == store.TowerReachable {
		t.Error("a tower that cannot see the chain tip was reported as working")
	}
	if !strings.Contains(obs.Health.Detail, "catching up") {
		t.Errorf("the detail does not explain the problem: %q", obs.Health.Detail)
	}
}

// A tower with no address is a tower nobody can register with, however healthy
// it looks from the inside.
func TestATowerWithNoAddressIsNotReadyYet(t *testing.T) {
	t.Parallel()
	f := healthy()
	f.identity.URIs = nil

	obs := newSupervisor(t, f).Observe(context.Background())

	if obs.Health.Status == store.TowerReachable {
		t.Error("a tower reachable at no address was reported as working")
	}
	if !strings.Contains(obs.Health.Detail, "register") {
		t.Errorf("the detail does not say why it matters: %q", obs.Health.Detail)
	}
}

// A tower that does not answer, and one whose node does not, are both failures —
// but they send someone looking in different places.
func TestTheTwoWaysOfNotAnsweringAreDistinguished(t *testing.T) {
	t.Parallel()

	silent := healthy()
	silent.identityErr = errors.New("connection refused")
	obs := newSupervisor(t, silent).Observe(context.Background())
	if obs.Health.Status != store.TowerUnreachable {
		t.Errorf("a silent tower: status = %q", obs.Health.Status)
	}
	if !strings.Contains(obs.Health.Detail, "did not answer") {
		t.Errorf("a silent tower: detail = %q", obs.Health.Detail)
	}

	halfThere := healthy()
	halfThere.chainErr = errors.New("wallet is locked")
	obs = newSupervisor(t, halfThere).Observe(context.Background())
	if !strings.Contains(obs.Health.Detail, "its node did not") {
		t.Errorf("a tower whose node is unwell: detail = %q", obs.Health.Detail)
	}
}

// The cap LND does not impose on itself. Over it, the tower is still working —
// so it is not unreachable — but carrying on quietly is how the host runs out of
// disk underneath the Bitcoin node.
func TestPassingTheDiskLimitIsReportedWithoutCallingTheTowerBroken(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	big := func(string) (int64, error) { return 3 << 20, nil } // 3 MB

	obs := newSupervisor(t, healthy(), func(o *Options) {
		o.DataDir, o.LimitMB, o.Usage = dir, 2, big
	}).Observe(context.Background())

	if !obs.DiskKnown {
		t.Fatal("the tower's storage was not measured")
	}
	if !obs.OverDiskLimit {
		t.Errorf("used %d bytes against a limit of %d and was not over it",
			obs.UsedBytes, obs.LimitBytes)
	}
	if obs.Health.Status != store.TowerReachable {
		t.Errorf("status = %q — a tower over its disk limit is still answering",
			obs.Health.Status)
	}
	if !strings.Contains(obs.Health.Detail, "limit") {
		t.Errorf("nothing was said about the storage: %q", obs.Health.Detail)
	}
}

// The useful moment is while there is still time to do something unhurried.
func TestApproachingTheDiskLimitIsWorthMentioningBeforeItIsReached(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	for _, tc := range []struct {
		name  string
		used  int64
		near  bool
		over  bool
		limit int64
	}{
		{"barely started", 1 << 20, false, false, 100},
		{"most of the way", 85 << 20, true, false, 100},
		{"exactly at the mark", 80 << 20, true, false, 100},
		{"past it", 101 << 20, true, true, 100},
	} {
		used := tc.used
		obs := newSupervisor(t, healthy(), func(o *Options) {
			o.DataDir, o.LimitMB = dir, tc.limit
			o.Usage = func(string) (int64, error) { return used, nil }
		}).Observe(context.Background())

		if obs.NearingDiskLimit() != tc.near {
			t.Errorf("%s: nearing = %v, want %v", tc.name, obs.NearingDiskLimit(), tc.near)
		}
		if obs.OverDiskLimit != tc.over {
			t.Errorf("%s: over = %v, want %v", tc.name, obs.OverDiskLimit, tc.over)
		}
	}
}

// A directory Forktower cannot see is not a directory that is empty. Reporting
// zero would read as "nothing to worry about", which is a claim we cannot make.
func TestAnUnreadableDirectoryIsNotReportedAsEmpty(t *testing.T) {
	t.Parallel()

	failing := func(string) (int64, error) { return 0, errors.New("no such directory") }
	obs := newSupervisor(t, healthy(), func(o *Options) {
		o.DataDir, o.LimitMB, o.Usage = "/nowhere", 100, failing
	}).Observe(context.Background())

	if obs.DiskKnown {
		t.Error("a directory that could not be read was reported as measured")
	}
	if obs.OverDiskLimit || obs.NearingDiskLimit() {
		t.Error("an unmeasurable directory produced a verdict about its size")
	}
	// And the tower itself is still judged on its own merits.
	if obs.Health.Status != store.TowerReachable {
		t.Errorf("status = %q — not being able to measure disk says nothing about "+
			"whether the tower works", obs.Health.Status)
	}
}

// Storage is only measured when there is something to measure it against.
func TestNoDirectoryOrNoLimitMeansNoVerdict(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		dir   string
		limit int64
	}{
		{"no directory configured", "", 100},
		{"no limit configured", "/somewhere", 0},
	} {
		obs := newSupervisor(t, healthy(), func(o *Options) {
			o.DataDir, o.LimitMB = tc.dir, tc.limit
			o.Usage = func(string) (int64, error) { return 1 << 30, nil }
		}).Observe(context.Background())
		if obs.DiskKnown {
			t.Errorf("%s: a size was reported anyway", tc.name)
		}
	}
}

func TestDirectorySizeAddsUpWhatIsThere(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(dir, "data", "chain"), 0o700); err != nil {
		t.Fatal(err)
	}
	want := int64(0)
	for i, body := range []string{"aaaa", "bbbbbbbb", "c"} {
		path := filepath.Join(dir, "data", "chain", fmt.Sprintf("f%d", i))
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		want += int64(len(body))
	}

	got, err := DirectorySize(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("measured %d bytes, want %d", got, want)
	}

	if _, err := DirectorySize(""); err == nil {
		t.Error("measuring nothing was accepted")
	}
	if _, err := DirectorySize(filepath.Join(dir, "not-here")); err != nil {
		t.Errorf("a directory that does not exist should measure as nothing, not fail: %v", err)
	}
}

func TestASupervisorNeedsSomethingToWatchAndAKind(t *testing.T) {
	t.Parallel()

	if _, err := New(Options{Kind: store.TowerLND}); err == nil {
		t.Error("a supervisor with nothing to read the tower with was built")
	}
	if _, err := New(Options{Kind: "nostr", Reader: healthy()}); err == nil {
		t.Error("a supervisor for a watchtower kind that does not exist was built")
	}
	s, err := New(Options{Kind: store.TowerTeos, Reader: healthy()})
	if err != nil {
		t.Fatal(err)
	}
	if s.usage == nil {
		t.Error("a supervisor with no measurer supplied did not get the default one")
	}
}

// LND says plainly that it is starting, and this used to call it stopped.
//
// **Verbatim from a StartOS install.** Every read is an error until lnd finishes
// opening its subservers, and mapping those to "unreachable" put "your
// watchtower has stopped answering" in front of a user whose tower was doing
// nothing wrong — on a first run, and again on every restart, where the stored
// status is "reachable" so the startup guard in the alerting cannot help either.
func TestATowerStillOpeningItsSubserversIsNotCalledUnreachable(t *testing.T) {
	t.Parallel()

	err := errors.New(`answered 500 Internal Server Error for ` +
		`/v2/watchtower/server: {"code":2, "message":"the RPC server is in the ` +
		`process of starting up, but not yet ready to accept calls", "details":[]}`)

	if !isStillStarting(err) {
		t.Fatal("lnd's own startup message was not recognised as starting")
	}

	// And a genuine failure is still a failure.
	for _, other := range []error{
		errors.New("connection refused"),
		errors.New("no route to host"),
		errors.New(`{"code":2, "message":"something else entirely"}`),
	} {
		if isStillStarting(other) {
			t.Errorf("%q was mistaken for a tower that is starting", other)
		}
	}
	if isStillStarting(nil) {
		t.Error("no error at all was read as starting")
	}

	// And the whole way through: a supervisor reading that error reports a tower
	// that is settling, not one that is gone.
	tower := healthy()
	tower.identityErr = err
	obs := newSupervisor(t, tower).Observe(context.Background())

	if obs.Health.Status != store.TowerTemporarilyUnreachable {
		t.Errorf("status = %q, want %q — anything else raises a warning about a "+
			"tower that is doing nothing wrong",
			obs.Health.Status, store.TowerTemporarilyUnreachable)
	}
	if strings.Contains(obs.Health.Detail, "500") ||
		strings.Contains(obs.Health.Detail, "code") {
		t.Errorf("the detail hands the reader transport bookkeeping: %q",
			obs.Health.Detail)
	}
	if !strings.Contains(obs.Health.Detail, "starting up") {
		t.Errorf("the detail does not say what is happening: %q", obs.Health.Detail)
	}
}
