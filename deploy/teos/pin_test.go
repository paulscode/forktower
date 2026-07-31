// Package teos holds nothing but the checks that keep the vendored watchtower
// pinned where it was put.
//
// There is no Go code in this deployment: the tower is a Rust binary built from
// [third_party/rust-teos]. What lives here is the small set of facts that decide
// whether that build is the one that was reviewed — and those are worth
// asserting, because the project they point at has no active maintainer and
// nothing upstream will notice if they drift.
package teos

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pinnedCommit is the rust-teos this project was reviewed against.
//
// Written out here, in a file somebody has to edit deliberately, so that a
// change to the pin is a change to a test rather than a line in a shell script
// nobody reads. Not the `v0.2.0` tag: that is from February 2023 and predates
// three years of fixes, and there will not be another release.
const pinnedCommit = "be344ecc5286dd9436bf343d30954135da8ad4ac"

// pinnedRust is the toolchain the source itself asks for, in its
// rust-toolchain.toml.
const pinnedRust = "1.81.0"

func readEnv(t *testing.T) map[string]string {
	t.Helper()
	f, err := os.Open("pinned.env")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	out := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[key] = value
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// A silent bump is the thing this whole arrangement exists to prevent.
func TestThePinIsWhereItWasPut(t *testing.T) {
	t.Parallel()
	env := readEnv(t)

	if got := env["TEOS_COMMIT"]; got != pinnedCommit {
		t.Errorf("pinned.env points at %s, not the reviewed %s.\n"+
			"Changing this is a decision, not a bump: there is no release to read "+
			"notes for and no maintainer to have vetted it. See deploy/teos/README.md.",
			got, pinnedCommit)
	}
	if got := env["TEOS_RUST"]; got != pinnedRust {
		t.Errorf("the toolchain is pinned at %s, want %s — the source asks for a "+
			"specific one in rust-toolchain.toml", got, pinnedRust)
	}
	if got := env["TEOS_REPO"]; !strings.Contains(got, "talaia-labs/rust-teos") {
		t.Errorf("the source repository is %q, which is not the one that was reviewed", got)
	}
}

// The vendored copy has to *be* the pinned commit, not merely claim it in a
// configuration file next door.
func TestTheVendoredCopyIsTheCommitClaimed(t *testing.T) {
	t.Parallel()
	stamp := filepath.Join("..", "..", "third_party", "rust-teos", "VENDORED")

	body, err := os.ReadFile(stamp)
	if err != nil {
		t.Skipf("no vendored copy to check (run `make vendor-teos`): %v", err)
	}
	if !strings.Contains(string(body), "commit="+pinnedCommit) {
		t.Errorf("the vendored copy does not record the pinned commit:\n%s", body)
	}
}

// The lockfile is the only reason a project with no active maintainer still
// builds. Losing it would resolve three years of dependency drift in one step.
func TestTheVendoredCopyKeepsItsLockfileAndLicence(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..", "third_party", "rust-teos")

	if _, err := os.Stat(root); err != nil {
		t.Skipf("no vendored copy to check (run `make vendor-teos`): %v", err)
	}
	for _, name := range []string{"Cargo.lock", "LICENSE", "rust-toolchain.toml"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Errorf("the vendored copy is missing %s: %v", name, err)
		}
	}
}

// The vendored source is somebody else's work, unmodified. Its toolchain file is
// the authority on which compiler it needs, so our pin has to agree with it
// rather than the other way round.
func TestOurToolchainPinAgreesWithTheSource(t *testing.T) {
	t.Parallel()
	path := filepath.Join("..", "..", "third_party", "rust-teos", "rust-toolchain.toml")

	body, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("no vendored copy to check (run `make vendor-teos`): %v", err)
	}
	if !strings.Contains(string(body), pinnedRust) {
		t.Errorf("the source asks for a different toolchain than we pin (%s):\n%s",
			pinnedRust, body)
	}
}

// `--locked` is the mechanical form of "never run cargo update". Without it a
// build on a machine that wanted a newer dependency would resolve differently
// and succeed, and the first anyone would know is when it stopped.
func TestTheBuildRefusesToResolveDependenciesAfresh(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)

	if !strings.Contains(text, "--locked") {
		t.Error("the Dockerfile builds without --locked, so a machine that wanted a " +
			"newer dependency would quietly build something else")
	}
	if strings.Contains(text, "cargo update") {
		t.Error("the Dockerfile runs cargo update, which resolves three years of " +
			"dependency drift against a codebase nobody maintains")
	}
	// protoc without the well-known types fails a long way into the compile, with
	// a message that reads as a problem in teos's own .proto files.
	if !strings.Contains(text, "libprotobuf-dev") {
		t.Error("the build installs protoc without the well-known protobuf types, " +
			"which the tower's service definition imports")
	}
	// A watchtower accepts a session from anyone who can reach it, so the image
	// must not be handing out root as well.
	if !strings.Contains(text, "USER teos") {
		t.Error("the image runs as root")
	}
}
