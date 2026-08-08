package daemon

// The cloud-peer starter template, executed.
//
// `vibe profile new --kind cloud-peer` was unsupported: PR #14 made
// peer-profiles-with-frontends legal and shipped no starter beyond
// profiles/omp.example.yaml, which is a worked example for one specific
// harness rather than something the CLI can generate.
//
// example_profile_test.go is the model, and its argument applies here with
// more force: a template is documentation that a command emits, so it is
// read far more often than it is run. This runs it — filled the way an
// operator fills it, through the real loader, with its frontend rendered by
// the same activation path `vibe start` uses. kind=external spawns nothing.
//
// The cli package has its own coverage that the GENERATED file loads
// (TestProfileInit_Kinds). What lives here is the half cli cannot reach:
// frontendModelVars and frontend.ActivateWithContext are the daemon's, and
// a peer template whose ${MODEL_ALIAS} does not resolve renders a harness
// config that 404s on first use.

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/vibe/frontend"
	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
)

const cloudPeerTemplate = "../cli/profile_templates/cloud-peer.yaml"

// fillCloudPeerTemplate substitutes what an operator substitutes: the
// profile name and the three values only their provider can supply. It
// mirrors internal/vibe/cli's fillReplacements for this kind; if the two
// ever disagree, the loader assertion below is what notices.
func fillCloudPeerTemplate(t *testing.T, body, name string) string {
	t.Helper()
	body = strings.ReplaceAll(body, "__PROFILE_NAME__", name)
	body = strings.ReplaceAll(body, "https://api.REPLACE-provider.example", "https://api.example.com")
	body = strings.ReplaceAll(body, "REPLACE_PROVIDER_API_KEY", "EXAMPLE_API_KEY")
	body = strings.ReplaceAll(body, "REPLACE-model-id", "example-model-id")
	return body
}

func TestCloudPeerTemplateLoadsAndRenders(t *testing.T) {
	// HOME first: the loader tilde-expands frontend paths at load time, so
	// setting it afterwards would send the render at the real home dir.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))

	raw, err := os.ReadFile(cloudPeerTemplate)
	if err != nil {
		t.Fatalf("the cloud-peer starter template is not where `vibe profile new --kind cloud-peer` reads it from: %v", err)
	}
	fixture := filepath.Join(t.TempDir(), "peer.yaml")
	if err := os.WriteFile(fixture, []byte(fillCloudPeerTemplate(t, string(raw), "peer")), 0o644); err != nil {
		t.Fatal(err)
	}

	p, err := profile.Load(fixture)
	if err != nil {
		t.Fatalf("the filled cloud-peer template does not load: %v — a template the CLI can generate and the loader then refuses is a broken command, not a broken profile", err)
	}
	if p.Backend.CloudPeer == nil {
		t.Fatal("the cloud-peer template is no longer a cloud_peer profile; this test guards that pairing")
	}
	// cloud_peer implies external — vibe must gain no opinion about a peer's
	// lifecycle. Residency belongs to the router (fleet-control.md §4).
	if !p.Backend.External {
		t.Error("cloud_peer did not normalize to external: vibe would try to supervise a peer it does not own")
	}
	if p.EstimatedVRAMGB != 0 {
		t.Errorf("estimated_vram_gb = %v: a peer holds no weights on this box, and a non-zero figure would make the daemon's pre-flight refuse a start that costs nothing", p.EstimatedVRAMGB)
	}

	alias, ctxLen := frontendModelVars(p, p.Backend.External)
	if alias != "example-model-id" {
		t.Errorf("${MODEL_ALIAS} = %q, want the peer's single model id: the template says one id keeps it unambiguous, and this is that claim", alias)
	}
	if ctxLen == 0 {
		t.Error("${MODEL_CONTEXT} resolved to 0: the template declares cloud_peer.context precisely so a harness gets a window instead of a zero")
	}

	res, err := frontend.ActivateWithContext(context.Background(), p, profile.ExpandContext{
		VibeAPI:      "http://router.example:9000/v1",
		ModelAlias:   alias,
		ModelContext: ctxLen,
		VibeStateDir: paths.StateHome(),
	})
	if err != nil {
		t.Fatalf("activating the template's frontend: %v", err)
	}
	if res == nil || res.WroteFile == "" {
		t.Fatal("the frontend reported success having written nothing — the rendered config IS the deliverable for a peer profile")
	}

	got := readFile(t, res.WroteFile)
	// The id a harness sends back must be the router's. An unexpanded
	// ${MODEL_ALIAS} and a profile-name substitution both produce an id that
	// 404s on first use, and neither fails anywhere near here.
	if !strings.Contains(got, "example-model-id") {
		t.Errorf("the rendered harness config does not name the peer model:\n%s", got)
	}
	for _, leak := range []string{"${MODEL_ALIAS}", "${MODEL_CONTEXT}", "${VIBE_API}", "${WRITE_FILE}"} {
		if strings.Contains(got, leak) {
			t.Errorf("the rendered config still carries an unexpanded %s:\n%s", leak, got)
		}
	}
	// contextWindow must stay a NUMBER: a whole-string ${MODEL_CONTEXT}
	// keeps its native type on expansion, and quoted instead the harness
	// reads no window at all.
	if !strings.Contains(got, `"context": `+strconv.Itoa(ctxLen)) {
		t.Errorf("the context limit did not render as a number:\n%s", got)
	}
	// vibe renders config, never secrets. The template carries an env var
	// NAME; a key reaching a rendered file would be a boundary breach.
	for _, secret := range []string{"EXAMPLE_API_KEY", "apiKey", "Bearer "} {
		if strings.Contains(got, secret) {
			t.Errorf("the rendered config carries a credential-shaped value (%q): the key belongs in the ROUTER's environment and nowhere vibe writes\n%s", secret, got)
		}
	}
}
