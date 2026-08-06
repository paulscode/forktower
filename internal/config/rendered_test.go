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
