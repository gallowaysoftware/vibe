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
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/vibe/frontend"
	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
)

const ompExample = "../../../profiles/omp.example.yaml"

const exampleGlob = "../../../profiles/*.example.yaml"

// notAProfile lists files under profiles/ that the profile loader must NOT be
// pointed at, with the reason. profiles/ is the examples directory for every
// vibe YAML kind, not only for profiles: mcp.example.yaml is the worked
// example for an MCP server definition under $XDG_CONFIG_HOME/vibe/mcp/, and
// it is referenced from a profile by name (`frontend.mcps: [datadog]`) rather
// than loaded as one. Feeding it to profile.Load fails at the parse step,
// which is correct behaviour and not a defect in the example.
var notAProfile = map[string]string{
	"mcp.example.yaml": "an MCP server definition for vibe/mcp/, not a profile",
}

// tildePath finds the ~/-rooted filesystem values in an example's raw YAML.
// Load stat()s backend.llama_server.path, backend.comfyui.dir (plus its
// main.py) and frontend.binary, so an example that names a model this box
// does not have fails validation for a reason that has nothing to do with the
// example being correct. Rather than pin a fixture list that goes stale the
// next time an example gains a path, the fixture is derived from the file
// itself: every ~/-rooted value gets materialised under the test's own HOME.
//
// A NEW filesystem-valued key is deliberately not silently tolerated — it is
// not in this regexp, nothing is created for it, and the stat error names the
// field, which is the signal to extend this line.
var tildePath = regexp.MustCompile(`(?m)^\s*(path|dir|binary|mmproj|draft_model|chat_template_file):\s*(~/\S+)\s*$`)

// materialiseFixtures creates, under home, every ~/-rooted path the example
// names. Files are written 0755 because frontend.binary is additionally
// checked for the executable bit; a `dir:` gets the directory plus the
// main.py that backend.comfyui.dir requires.
func materialiseFixtures(t *testing.T, home, exampleFile string) {
	t.Helper()
	raw, err := os.ReadFile(exampleFile)
	if err != nil {
		t.Fatalf("read %s: %v", exampleFile, err)
	}
	for _, m := range tildePath.FindAllStringSubmatch(string(raw), -1) {
		key, target := m[1], filepath.Join(home, strings.TrimPrefix(m[2], "~/"))
		if key == "dir" {
			target = filepath.Join(target, "main.py")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatalf("fixture dir for %s: %v", target, err)
		}
		if err := os.WriteFile(target, []byte("# vibe test fixture\n"), 0o755); err != nil {
			t.Fatalf("fixture %s: %v", target, err)
		}
	}
}

// TestExampleProfilesLoad is the generalisation of the omp case below.
//
// profiles/omp.example.yaml was DESCRIBED correctly and was unloadable for
// months: an example reads as documentation, is never executed, and rots into
// a description of something that does not work. The test below closed that
// for omp alone, which left the other seven examples with exactly the
// protection omp had before it broke — none.
//
// This runs every one of them through the real loader. It is shallow on
// purpose: the deep per-example assertions (rendered config, expanded vars,
// no leaked secrets) are the omp test's job, and the failure this guards is
// the coarse one — the file no longer parses or no longer validates.
func TestExampleProfilesLoad(t *testing.T) {
	files, err := filepath.Glob(exampleGlob)
	if err != nil {
		t.Fatalf("glob %s: %v", exampleGlob, err)
	}
	sort.Strings(files)
	// A glob that silently matches nothing is the classic vacuous pass: the
	// loop body never runs and the test is green having loaded no example at
	// all. The floor is the count today minus the non-profiles.
	if want := len(notAProfile) + 7; len(files) < want {
		t.Fatalf("glob %s matched %d files, want >= %d — the examples moved and this test stopped covering them",
			exampleGlob, len(files), want)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			if why, skip := notAProfile[filepath.Base(f)]; skip {
				t.Skipf("not a profile: %s", why)
			}
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
			t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
			materialiseFixtures(t, home, f)

			p, err := profile.Load(f)
			if err != nil {
				t.Fatalf("%s does not load: %v", f, err)
			}
			if p.Name == "" {
				t.Errorf("%s loaded with an empty name: `vibe start` has nothing to address it by", f)
			}
		})
	}
}

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
