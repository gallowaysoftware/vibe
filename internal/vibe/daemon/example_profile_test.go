package daemon

// profiles/omp.example.yaml is the worked example for pointing a coding
// harness at a cloud model the router serves. It described that shape long
// before it could be loaded, let alone started — which is the failure mode an
// example has: it reads as documentation and is never executed, so it rots
// into a description of something that does not work.
//
// This runs it. The example is loaded through the real profile loader, its
// model vars resolved by the same function Start calls, and its config files
// rendered by the same frontend activation path — everything `vibe start omp`
// does except reaching the network. kind=external spawns no process.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/vibe/frontend"
	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
)

const ompExample = "../../../profiles/omp.example.yaml"

func TestOMPExampleProfileLoadsAndRenders(t *testing.T) {
	// HOME first: the loader tilde-expands write_files paths at load time, so
	// setting it afterwards would send the render at the real home dir.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))

	p, err := profile.Load(ompExample)
	if err != nil {
		t.Fatalf("profiles/omp.example.yaml does not load: %v", err)
	}
	if p.Backend.CloudPeer == nil {
		t.Fatalf("example is no longer a cloud_peer profile; this test guards that pairing")
	}
	// cloud_peer implies external — vibe must not think it owns this
	// lifecycle. Residency belongs to the router (fleet-control.md §4).
	if !p.Backend.External {
		t.Error("cloud_peer did not normalize to external: vibe would try to supervise a peer it does not own")
	}

	alias, ctxLen := frontendModelVars(p, p.Backend.External)
	if alias != "kimi-k3" {
		t.Errorf("${MODEL_ALIAS} = %q, want the peer's single model id", alias)
	}
	if ctxLen != 1048576 {
		t.Errorf("${MODEL_CONTEXT} = %d, want cloud_peer.context", ctxLen)
	}

	// The example documents both as required in config.yaml; supply them the
	// way the daemon does so the render is the one a real start produces.
	res, err := frontend.ActivateWithContext(context.Background(), p, profile.ExpandContext{
		VibeAPI:      "http://router.example:9000/v1",
		ModelAlias:   alias,
		ModelContext: ctxLen,
		VibeStateDir: paths.StateHome(),
		VibeSearch:   "http://search.example:14003",
	})
	if err != nil {
		t.Fatalf("activating the example frontend: %v", err)
	}
	if res == nil || res.WroteFile == "" {
		t.Fatal("frontend reported success having written nothing — the rendered config IS the deliverable for a peer")
	}

	// Both halves of omp's split config have to land: a models.yml without a
	// config.yml leaves the harness with a provider it never selects.
	models := filepath.Join(home, ".omp-vibe", "omp", "agent", "models.yml")
	cfg := filepath.Join(home, ".omp-vibe", "omp", "agent", "config.yml")
	for _, f := range []string{models, cfg} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("expected rendered config at %s: %v", f, err)
		}
	}

	got := readFile(t, models)
	// The id a harness sends back must be the router's, not the profile
	// name: an unexpanded ${MODEL_ALIAS} or a def-name substitution both
	// produce an id that 404s on first use.
	if !strings.Contains(got, "kimi-k3") {
		t.Errorf("models.yml does not name the peer model:\n%s", got)
	}
	for _, leak := range []string{"${MODEL_ALIAS}", "${MODEL_CONTEXT}", "${VIBE_API}"} {
		if strings.Contains(got, leak) {
			t.Errorf("models.yml still carries an unexpanded %s:\n%s", leak, got)
		}
	}
	// contextWindow must be a NUMBER. A whole-string ${MODEL_CONTEXT} keeps
	// its native type on expansion; rendered as the quoted "1048576" instead,
	// omp reads no window at all — a failure that surfaces nowhere near here.
	if !strings.Contains(got, `"contextWindow": 1048576`) {
		t.Errorf("models.yml contextWindow is not the numeric peer window:\n%s", got)
	}

	gotCfg := readFile(t, cfg)
	if !strings.Contains(gotCfg, "fleet/kimi-k3") {
		t.Errorf("config.yml default role does not select the peer model:\n%s", gotCfg)
	}
	if strings.Contains(gotCfg, "${VIBE_SEARCH}") {
		t.Errorf("config.yml still carries an unexpanded ${VIBE_SEARCH}:\n%s", gotCfg)
	}
	// vibe renders config, never secrets. A token reaching a rendered file
	// would be a boundary breach, not a convenience.
	for _, secret := range []string{"token:", "apiKey: sk-", "Bearer "} {
		if strings.Contains(gotCfg, secret) {
			t.Errorf("rendered config carries a secret (%q):\n%s", secret, gotCfg)
		}
	}
}

// An example that references a var the operator has not configured must fail
// naming that var. Rendering an empty endpoint instead is how a harness ends
// up silently talking to a default provider.
func TestOMPExampleFailsNamingUnsetSearchURL(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))

	p, err := profile.Load(ompExample)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	alias, ctxLen := frontendModelVars(p, p.Backend.External)
	_, err = frontend.ActivateWithContext(context.Background(), p, profile.ExpandContext{
		VibeAPI:      "http://router.example:9000/v1",
		ModelAlias:   alias,
		ModelContext: ctxLen,
		VibeStateDir: paths.StateHome(),
		// VibeSearch deliberately unset.
	})
	if err == nil {
		t.Fatal("activation succeeded with no search_url configured")
	}
	if !strings.Contains(err.Error(), "search_url") {
		t.Errorf("err = %v, want it to name search_url as the fix", err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
