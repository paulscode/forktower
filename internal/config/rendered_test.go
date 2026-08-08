package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The entrypoint writes this package's file format, and nothing else checks
// that the two agree.
//
// **This test exists because that seam broke twice in one afternoon.** First the
// tower's settings were exported as environment variables, which StartOS 0.4.x
// discards — it runs the renderer with --render-only and then starts the daemon
// itself, so the tower would have run with nothing watching it. Then the address
// written was the socket lnd binds rather than the address a client dials, and
// the daemon refused to start at all.
//
// Neither was visible from either side alone. A shell script and a TOML parser
// are a file format apart, and the only way to know they still agree is to run
// one and feed the result to the other.
//
// A third one cost an install on real hardware, and this test could not have
// caught it: it ends where Load's file parsing does, and the daemon does not.
// The environment is applied on top, and that is a separate question with a
// test of its own below.
func TestTheEntrypointWritesAFileThisPackageAccepts(t *testing.T) {
	entrypoint, err := filepath.Abs("../../docker_entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(entrypoint); err != nil {
		t.Skipf("no entrypoint to run: %v", err)
	}

	cases := map[string]struct {
		env    []string
		assert func(*testing.T, Config)
	}{
		"a plain install": {
			assert: func(t *testing.T, cfg Config) {
				t.Helper()
				if cfg.Tower.LND.Enabled {
					t.Error("a tower was configured without being asked for")
				}
			},
		},
		"with the watchtower": {
			env: []string{
				"FORKTOWER_TOWER_LND_ENABLED=true",
				"FORKTOWER_TOWER_LND_EXTERNAL_ADDR=abc123def.onion:9911",
			},
			assert: func(t *testing.T, cfg Config) {
				t.Helper()
				lnd := cfg.Tower.LND
				if !lnd.Enabled {
					t.Fatal("the tower was asked for and not configured")
				}
				// The address a client dials. A bind address here would put
				// 0.0.0.0 on the dashboard as something to paste, and the
				// validation would refuse the file outright.
				if lnd.Listen != "abc123def.onion:9911" {
					t.Errorf("listen = %q, want the address clients dial", lnd.Listen)
				}
				// Without these the tower runs and nothing watches it, which is
				// the exact failure this program exists to complain about.
				if lnd.APIURL == "" || lnd.MacaroonPath == "" || lnd.TLSCertPath == "" {
					t.Errorf("the daemon would not know where to read the tower: %+v", lnd)
				}
				if !strings.HasSuffix(lnd.MacaroonPath, "readonly.macaroon") {
					t.Errorf("the tower is read with %q, which is not its read-only "+
						"credential", lnd.MacaroonPath)
				}
			},
		},
		"asked for, but with no address yet": {
			// Legitimate on a first StartOS boot, before the platform has
			// assigned an onion. Declining is right: a tower nobody can reach
			// protects nothing, and running one would spend memory on a service
			// no client could dial.
			env: []string{"FORKTOWER_TOWER_LND_ENABLED=true"},
			assert: func(t *testing.T, cfg Config) {
				t.Helper()
				if cfg.Tower.LND.Enabled {
					t.Error("a tower with no reachable address was configured anyway")
				}
			},
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			cmd := exec.Command("sh", entrypoint, "--render-only")
			cmd.Env = append([]string{
				"PATH=" + os.Getenv("PATH"),
				"FORKTOWER_DATA_DIR=" + dir,
				"FORKTOWER_SF_RPC_URL=http://127.0.0.1:8332",
				"FORKTOWER_SF_RPC_USER=u",
				"FORKTOWER_SF_RPC_PASS=p",
			}, c.env...)

			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("the entrypoint failed: %v\n%s", err, out)
			}

			cfg, err := Load(filepath.Join(dir, "forktower.toml"))
			if err != nil {
				t.Fatalf("this package rejected what the entrypoint wrote: %v", err)
			}
			c.assert(t, cfg)
		})
	}
}

// The platforms set more than the renderer reads, and the daemon reads some of
// it too.
//
// This is the shape of the bug that reached hardware: a variable meaning "the
// socket lnd binds" to the shell script, and "the address to advertise" to the
// daemon. The file was rendered correctly and then overwritten from the
// environment with a wildcard the validation refuses, so the daemon would not
// start at all.
func TestThePlatformEnvironmentDoesNotOverwriteWhatItJustRendered(t *testing.T) {
	entrypoint, err := filepath.Abs("../../docker_entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(entrypoint); err != nil {
		t.Skipf("no entrypoint to run: %v", err)
	}

	// Exactly what the StartOS 0.4.x package passes.
	platform := []string{
		"FORKTOWER_TOWER_LND_ENABLED=true",
		"FORKTOWER_TOWER_LND_BIND=0.0.0.0:9911",
		"FORKTOWER_TOWER_LND_EXTERNAL_ADDR=forktower.startos:9911",
	}

	dir := t.TempDir()
	cmd := exec.Command("sh", entrypoint, "--render-only")
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"FORKTOWER_DATA_DIR=" + dir,
		"FORKTOWER_SF_RPC_URL=http://127.0.0.1:8332",
		"FORKTOWER_SF_RPC_USER=u",
		"FORKTOWER_SF_RPC_PASS=p",
	}, platform...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the entrypoint failed: %v\n%s", err, out)
	}

	for _, kv := range platform {
		key, value, _ := strings.Cut(kv, "=")
		t.Setenv(key, value)
	}

	cfg, err := Load(filepath.Join(dir, "forktower.toml"))
	if err != nil {
		t.Fatalf("the daemon would refuse to start: %v", err)
	}
	if got := cfg.Tower.LND.Listen; got != "forktower.startos:9911" {
		t.Errorf("tower.lnd.listen = %q after the environment was applied, want "+
			"the address clients dial — a bind address here is refused outright "+
			"and the daemon never starts", got)
	}
}

// A top-level key has to be written while the document is still top level.
//
// The platform setting used to be printed near the end of the file, after the
// optional `[[ln.lnd]]` block — and TOML reads a bare key as belonging to the
// table above it. So on every install with a Lightning node configured, which
// is most of them, it parsed as `ln.lnd.platform`, was dropped with a warning
// nobody reads, and the setup wizard offered no platform directions at all.
//
// Invisible from either side: the file looked right, the parser was right, and
// the only symptom was guidance that silently did not appear. It surfaced in a
// log line on real hardware.
func TestThePlatformSurvivesALightningNodeBeingConfigured(t *testing.T) {
	entrypoint, err := filepath.Abs("../../docker_entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(entrypoint); err != nil {
		t.Skipf("no entrypoint to run: %v", err)
	}

	for name, extra := range map[string][]string{
		"with no Lightning node": nil,
		"with LND": {
			"FORKTOWER_LND_REST_URL=https://lnd.startos:8080",
			"FORKTOWER_LND_MACAROON_PATH=/mnt/lnd/readonly.macaroon",
		},
		"with Core Lightning": {
			"FORKTOWER_CLN_REST_URL=https://cln.startos:3010",
			"FORKTOWER_CLN_RUNE_PATH=/mnt/cln/rune",
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			cmd := exec.Command("sh", entrypoint, "--render-only")
			cmd.Env = append([]string{
				"PATH=" + os.Getenv("PATH"),
				"FORKTOWER_DATA_DIR=" + dir,
				"FORKTOWER_PLATFORM=startos-0.4",
				"FORKTOWER_SF_RPC_URL=http://127.0.0.1:8332",
				"FORKTOWER_SF_RPC_USER=u",
				"FORKTOWER_SF_RPC_PASS=p",
			}, extra...)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("the entrypoint failed: %v\n%s", err, out)
			}

			cfg, err := Load(filepath.Join(dir, "forktower.toml"))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Platform != "startos-0.4" {
				t.Errorf("platform = %q; the setup wizard shows no directions "+
					"without it, and says nothing about why", cfg.Platform)
			}
			// The warning that was the only symptom, and which nobody reads.
			for _, w := range cfg.Warnings {
				if strings.Contains(w, "platform") {
					t.Errorf("the platform key was not parsed as one: %s", w)
				}
			}
		})
	}
}

// The 0.3.5.1 package has to end up with credentials for the user's node.
//
// **It shipped without any.** That entrypoint read BITCOIND_RPC_USER and
// BITCOIND_RPC_PASSWORD from the environment, on the belief that the platform
// set them for a declared dependency. It does not, there is no cookie file on
// that platform either, and the result was a configuration with an RPC address
// and no way to authenticate to it: the daemon refused to start, restarted,
// refused again, and "Launch UI" opened a page nothing was serving.
//
// This runs that entrypoint against the file the Bitcoin package actually
// publishes, and asserts the credentials come out the other end.
func TestTheEmbassyEntrypointFindsTheNodesCredentials(t *testing.T) {
	entrypoint, err := filepath.Abs("../../docker_entrypoint_0351.sh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(entrypoint); err != nil {
		t.Skipf("no entrypoint to run: %v", err)
	}
	if _, err := exec.LookPath("yq"); err != nil {
		t.Skip("yq is not on PATH; the image has it")
	}

	// Exactly the shape that package writes, keys and spaces included.
	stats := filepath.Join(t.TempDir(), "stats.yaml")
	if err := os.WriteFile(stats, []byte(`version: 2
data:
  RPC Username:
    type: string
    value: "bitcoin"
  RPC Password:
    type: string
    value: "s3cr3t-from-the-node"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	cmd := exec.Command("sh", entrypoint, "--render-only")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"FORKTOWER_DATA_DIR=" + dir,
		"BITCOIND_STATS=" + stats,
		// The shared renderer this hands over to, and the platform's data
		// directory, both of which are container paths in production.
		"FORKTOWER_ENTRYPOINT=" + mustAbs(t, "../../docker_entrypoint.sh"),
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the entrypoint failed: %v\n%s", err, out)
	}

	cfg, err := Load(filepath.Join(dir, "forktower.toml"))
	if err != nil {
		t.Fatalf("the daemon would refuse to start: %v", err)
	}
	if cfg.SF.RPCUser != "bitcoin" || cfg.SF.RPCPass != "s3cr3t-from-the-node" {
		t.Errorf("sf credentials = %q/%q; without them the daemon crash-loops and "+
			"the dashboard is never served", cfg.SF.RPCUser, cfg.SF.RPCPass)
	}
}

func mustAbs(t *testing.T, path string) string {
	t.Helper()
	p, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// The entrypoint has to render the alternate addresses as a TOML array the
// daemon can read back.
//
// **Asserted against the rendered file, not against a loaded Config.** The first
// version of this test called `Load` with the same variables set in the
// environment, which overlays them on top of whatever the file said — so it
// passed while the entrypoint was rendering an empty list. A test of rendering
// has to read what was rendered.
func TestTheOtherAddressesTheTowerAnswersOnSurviveRendering(t *testing.T) {
	t.Parallel()
	entrypoint := "../../docker_entrypoint.sh"
	if _, err := os.Stat(entrypoint); err != nil {
		t.Skipf("no entrypoint to run: %v", err)
	}

	dir := t.TempDir()
	cmd := exec.Command("sh", entrypoint, "--render-only")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"FORKTOWER_DATA_DIR=" + dir,
		"FORKTOWER_SF_RPC_URL=http://127.0.0.1:8332",
		"FORKTOWER_SF_RPC_USER=u",
		"FORKTOWER_SF_RPC_PASS=p",
		"FORKTOWER_TOWER_LND_ENABLED=true",
		"FORKTOWER_TOWER_LND_BIND=0.0.0.0:9911",
		"FORKTOWER_TOWER_LND_EXTERNAL_ADDR=abcdef.onion:9911",
		// One address, no trailing comma: the case every real deployment has,
		// and the one a `tr | read` pipeline silently drops.
		"FORKTOWER_TOWER_LND_ALSO_REACHABLE_AT=forktower.startos:9911",
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the entrypoint failed: %v\n%s", err, out)
	}

	rendered, err := os.ReadFile(filepath.Join(dir, "forktower.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rendered), `also_reachable_at = ["forktower.startos:9911"]`) {
		t.Errorf("the rendered config does not list the sibling hostname — "+
			"without it, every registration made before the tower had an onion is "+
			"reported as pointing somewhere dead. Got:\n%s", rendered)
	}

	// And it still parses, which is the other half of rendering an array by hand.
	cfg, err := Load(filepath.Join(dir, "forktower.toml"))
	if err != nil {
		t.Fatalf("the daemon would refuse to start: %v", err)
	}
	if got := cfg.Tower.LND.AlsoReachableAt; len(got) != 1 {
		t.Errorf("tower.lnd.also_reachable_at parsed as %q, want one address", got)
	}
}

// Several addresses, because rendering a list by hand is where separators go
// wrong and a malformed array stops the daemon starting at all.
func TestSeveralOtherAddressesRenderAsAValidArray(t *testing.T) {
	t.Parallel()
	entrypoint := "../../docker_entrypoint.sh"
	if _, err := os.Stat(entrypoint); err != nil {
		t.Skipf("no entrypoint to run: %v", err)
	}

	dir := t.TempDir()
	cmd := exec.Command("sh", entrypoint, "--render-only")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"FORKTOWER_DATA_DIR=" + dir,
		"FORKTOWER_SF_RPC_URL=http://127.0.0.1:8332",
		"FORKTOWER_SF_RPC_USER=u",
		"FORKTOWER_SF_RPC_PASS=p",
		"FORKTOWER_TOWER_LND_ENABLED=true",
		"FORKTOWER_TOWER_LND_BIND=0.0.0.0:9911",
		"FORKTOWER_TOWER_LND_EXTERNAL_ADDR=abcdef.onion:9911",
		"FORKTOWER_TOWER_LND_ALSO_REACHABLE_AT=a.startos:9911,b.startos:9911",
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the entrypoint failed: %v\n%s", err, out)
	}

	cfg, err := Load(filepath.Join(dir, "forktower.toml"))
	if err != nil {
		t.Fatalf("the daemon would refuse to start: %v", err)
	}
	got := cfg.Tower.LND.AlsoReachableAt
	if len(got) != 2 || got[0] != "a.startos:9911" || got[1] != "b.startos:9911" {
		t.Errorf("tower.lnd.also_reachable_at = %q, want both addresses", got)
	}
}

// An empty value is the ordinary case — the tower is advertised at the only
// address it has — and must not become a list containing nothing.
func TestNoOtherAddressesRendersNothing(t *testing.T) {
	t.Parallel()
	entrypoint := "../../docker_entrypoint.sh"
	if _, err := os.Stat(entrypoint); err != nil {
		t.Skipf("no entrypoint to run: %v", err)
	}

	dir := t.TempDir()
	cmd := exec.Command("sh", entrypoint, "--render-only")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"FORKTOWER_DATA_DIR=" + dir,
		"FORKTOWER_SF_RPC_URL=http://127.0.0.1:8332",
		"FORKTOWER_SF_RPC_USER=u",
		"FORKTOWER_SF_RPC_PASS=p",
		"FORKTOWER_TOWER_LND_ENABLED=true",
		"FORKTOWER_TOWER_LND_BIND=0.0.0.0:9911",
		"FORKTOWER_TOWER_LND_EXTERNAL_ADDR=forktower.startos:9911",
		"FORKTOWER_TOWER_LND_ALSO_REACHABLE_AT=",
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the entrypoint failed: %v\n%s", err, out)
	}

	cfg, err := Load(filepath.Join(dir, "forktower.toml"))
	if err != nil {
		t.Fatalf("the daemon would refuse to start: %v", err)
	}
	if got := cfg.Tower.LND.AlsoReachableAt; len(got) != 0 {
		t.Errorf("tower.lnd.also_reachable_at = %q, want nothing at all", got)
	}
}

// **The 0.3.5.1 packaging, end to end through its own entrypoint.**
//
// That package declares a `watchtower` interface with a `tor-config`, so the
// platform assigns the tower an onion of its own — a stable address, unlike the
// sibling hostname lnd flattens to a container number that dies on the next
// rebuild. This runs the real `docker_entrypoint_0351.sh` against a config file
// shaped like the one StartOS writes, and checks the address that comes out.
//
// The sibling hostname has to survive into the alternates, or every registration
// made before this change — all of which still work — is reported as pointing
// somewhere dead.
func TestTheOlderPackagingAdvertisesItsOnion(t *testing.T) {
	t.Parallel()
	entrypoint := "../../docker_entrypoint_0351.sh"
	if _, err := os.Stat(entrypoint); err != nil {
		t.Skipf("no entrypoint to run: %v", err)
	}
	if _, err := exec.LookPath("yq"); err != nil {
		t.Skipf("yq is needed to read the platform's config file: %v", err)
	}

	const onion = "s4uoylvbtjc5crtbetccetb3yuhzkmwwpapgrqxv5ehze4n7cpezmeqd.onion"
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "start9"), 0o755); err != nil {
		t.Fatal(err)
	}
	// What the platform writes once the pointer in getConfig.ts is resolved.
	cfgYAML := "watchtower-address: " + onion + "\n"
	if err := os.WriteFile(
		filepath.Join(dir, "start9", "config.yaml"), []byte(cfgYAML), 0o644,
	); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sh", entrypoint, "--render-only")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"FORKTOWER_DATA_DIR=" + dir,
		"FORKTOWER_CONFIG_YAML=" + filepath.Join(dir, "start9", "config.yaml"),
		"FORKTOWER_ENTRYPOINT=" + mustAbs(t, "../../docker_entrypoint.sh"),
		"FORKTOWER_SF_RPC_URL=http://127.0.0.1:8332",
		"FORKTOWER_SF_RPC_USER=u",
		"FORKTOWER_SF_RPC_PASS=p",
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the entrypoint failed: %v\n%s", err, out)
	}

	rendered, err := os.ReadFile(filepath.Join(dir, "forktower.toml"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(rendered)
	if !strings.Contains(got, `listen = "`+onion+`:9911"`) {
		t.Errorf("the onion is not the advertised address, so a registration made "+
			"against it still rots on the next rebuild:\n%s", got)
	}
	if !strings.Contains(got, `also_reachable_at = ["forktower.embassy:9911"]`) {
		t.Errorf("the sibling hostname was dropped from the alternates, which "+
			"reports every registration made before this change as dead:\n%s", got)
	}
}

// And with no onion assigned — a first boot, or a config predating the pointer —
// it falls back to the address that has always worked, and claims no alternates.
func TestTheOlderPackagingFallsBackToTheSiblingHostname(t *testing.T) {
	t.Parallel()
	entrypoint := "../../docker_entrypoint_0351.sh"
	if _, err := os.Stat(entrypoint); err != nil {
		t.Skipf("no entrypoint to run: %v", err)
	}
	if _, err := exec.LookPath("yq"); err != nil {
		t.Skipf("yq is needed to read the platform's config file: %v", err)
	}

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "start9"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(dir, "start9", "config.yaml"), []byte("{}\n"), 0o644,
	); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sh", entrypoint, "--render-only")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"FORKTOWER_DATA_DIR=" + dir,
		"FORKTOWER_CONFIG_YAML=" + filepath.Join(dir, "start9", "config.yaml"),
		"FORKTOWER_ENTRYPOINT=" + mustAbs(t, "../../docker_entrypoint.sh"),
		"FORKTOWER_SF_RPC_URL=http://127.0.0.1:8332",
		"FORKTOWER_SF_RPC_USER=u",
		"FORKTOWER_SF_RPC_PASS=p",
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the entrypoint failed: %v\n%s", err, out)
	}

	rendered, err := os.ReadFile(filepath.Join(dir, "forktower.toml"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(rendered)
	if !strings.Contains(got, `listen = "forktower.embassy:9911"`) {
		t.Errorf("without an onion the tower lost the address that does work:\n%s", got)
	}
	if strings.Contains(got, "also_reachable_at") {
		t.Errorf("alternates were claimed where the advertised address is the "+
			"only one there is:\n%s", got)
	}
}

// The companion tower does not go looking for Lightning peers.
//
// **Reported by a user reading their log.** `nolisten` stops the tower accepting
// peers; it does not stop lnd asking the DNS seeds for some at every start,
// failing to use them, and logging `Unable to retrieve initial bootstrap peers`
// as an error. Harmless — the tower needs a chain backend and inbound watchtower
// connections, and peers are neither — but a recurring error that is expected
// teaches people to skim past the ones that are not. On a node routing through
// Tor the lookups and dials are not free either.
func TestTheTowerDoesNotHuntForPeersItDoesNotWant(t *testing.T) {
	t.Parallel()
	entrypoint := "../../docker_entrypoint.sh"
	if _, err := os.Stat(entrypoint); err != nil {
		t.Skipf("no entrypoint to run: %v", err)
	}

	dir := t.TempDir()
	cmd := exec.Command("sh", entrypoint, "--render-only")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"FORKTOWER_DATA_DIR=" + dir,
		"FORKTOWER_SF_RPC_URL=http://127.0.0.1:8332",
		"FORKTOWER_SF_RPC_USER=u",
		"FORKTOWER_SF_RPC_PASS=p",
		"FORKTOWER_TOWER_LND_ENABLED=true",
		"FORKTOWER_TOWER_LND_BIND=0.0.0.0:9911",
		// Without an address clients could dial, the entrypoint switches the
		// tower off and writes no configuration at all.
		"FORKTOWER_TOWER_LND_EXTERNAL_ADDR=abcdef.onion:9911",
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the entrypoint failed: %v\n%s", err, out)
	}

	conf, err := os.ReadFile(filepath.Join(dir, "tower", "lnd.conf"))
	if err != nil {
		t.Fatalf("the tower's configuration was not written: %v", err)
	}
	text := string(conf)
	for _, want := range []string{"nobootstrap=true", "nolisten=true"} {
		if !strings.Contains(text, want) {
			t.Errorf("the tower's configuration is missing %q:\n%s", want, text)
		}
	}
}
