package cli

// C19's desk path, from the adversarial-review pass. The findings this
// file pins are both about flags that exist and do nothing: `restore`
// takes destinations and refusal-escapes, and a flag that is registered
// but never reaches RestoreOptions is a switch an operator flips
// mid-incident with no effect and no message.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetmirror"
)

func mirrorFixture(t *testing.T) (archive, state, config string) {
	t.Helper()
	root := t.TempDir()
	state = filepath.Join(root, "state")
	config = filepath.Join(root, "config")
	out := filepath.Join(root, "out")
	for _, d := range []string{filepath.Join(state, "fleet"), config} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(state, "token"), []byte("TOKEN-VALUE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "fleet", "intent.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "hosts.yaml"), []byte("cells: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hosts, err := fleetcfg.LoadFrom(filepath.Join(config, "hosts.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	_, rc, err := fleetmirror.Create(fleetmirror.Options{
		StateDir: state, ConfigDir: config, Out: out, Hosts: hosts, Host: "front-host",
	})
	if err != nil {
		t.Fatal(err)
	}
	return rc.Archive, state, config
}

func runMirrorRestore(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := fleetMirrorRestoreCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// TestFleetMirrorRestore_ProbeAddrFlagReachesTheProbe: a hosts.yaml with
// no fleetd_url and no front cell records no address, so `restore`
// refuses rather than pretending it looked (review REV-3). --probe-addr
// is the escape that is still a probe — and it only is one if the flag
// reaches RestoreOptions.
func TestFleetMirrorRestore_ProbeAddrFlagReachesTheProbe(t *testing.T) {
	archive, _, _ := mirrorFixture(t)
	sb := t.TempDir()

	out, err := runMirrorRestore(t, archive, "--state-dir", filepath.Join(sb, "state"))
	if err == nil {
		t.Fatalf("restore proceeded with nothing to probe:\n%s", out)
	}
	if !strings.Contains(err.Error(), "nothing to probe") {
		t.Errorf("refusal does not say why: %v", err)
	}

	// A dead address, named. The probe runs, finds nothing, and the
	// restore proceeds.
	out, err = runMirrorRestore(t, archive,
		"--state-dir", filepath.Join(sb, "state"), "--probe-addr", "127.0.0.1:1")
	if err != nil {
		t.Fatalf("--probe-addr did not reach the probe: %v\n%s", err, out)
	}
	if !strings.Contains(out, "127.0.0.1:1") {
		t.Errorf("the output does not report what was probed:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(sb, "state", "token")); err != nil {
		t.Errorf("nothing was restored: %v", err)
	}
}
