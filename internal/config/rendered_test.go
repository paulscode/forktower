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
