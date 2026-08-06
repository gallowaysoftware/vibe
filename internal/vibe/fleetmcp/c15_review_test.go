package fleetmcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
)

// fleet-control C15, adversarial-review pass.
//
// TestRenderFrontMergesTheRenderLoopsExtras. `render_front` answers
// "what would fleetd's render write". It read the CLI's default extras
// file (`~/.config/vibe/router-extras.yaml`) while the render LOOP merges
// `fleet.front_extras`, so on the fleet this phase exists to support the
// dry run reported the front's own `apiKeys:` as a deletion — and on
// fleetd (a container, where the CLI default does not exist) it reported
// every hand-written section that way. A tool whose whole job is
// answering "what changes" must merge what the writer merges.
func TestRenderFrontMergesTheRenderLoopsExtras(t *testing.T) {
	dir := t.TempDir()
	defs := filepath.Join(dir, "backends")
	if err := os.MkdirAll(defs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(defs, "peer-model.yaml"), []byte(
		"name: peer-model\ncell: heavy\nbackend:\n  external: true\n  llama_server:\n    path: /models/x.gguf\n    alias: peer-model\n    context: 4096\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	extras := filepath.Join(dir, "front-extras.yaml")
	if err := os.WriteFile(extras, []byte("apiKeys:\n  - the-front-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hosts := &fleetcfg.File{Cells: map[string]fleetcfg.Cell{
		fleetcfg.FrontCell: {URL: "http://127.0.0.1:9000", Class: fleetcfg.ClassAlwaysOn},
		"heavy":            {URL: "http://127.0.0.1:9999", Class: fleetcfg.ClassAlwaysOn},
	}}
	fleet := fleetapi.New([]fleetapi.Cell{{Name: fleetcfg.FrontCell, URL: "http://127.0.0.1:9000"}},
		filepath.Join(dir, "history.json"),
		func() fleetapi.DaemonInfo { return fleetapi.DaemonInfo{} },
		fleetapi.Options{Hosts: hosts, IntentPath: filepath.Join(dir, "intent.json")})
	t.Cleanup(fleet.Close)

	live := filepath.Join(dir, "front.yaml")
	if err := os.WriteFile(live, []byte("placeholder\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New(fleet, hosts, Options{FrontConfig: live, BackendsDir: defs, FrontExtras: extras})

	yes := true
	out, err := s.toolRenderFront(context.Background(), &yes)
	if err != nil {
		t.Fatalf("render_front: %v", err)
	}
	if !strings.Contains(out, "apiKeys") || !strings.Contains(out, "the-front-key") {
		t.Fatalf("the dry run drops the front's apiKeys, so it disagrees with the render loop that keeps them:\n%s", out)
	}
}
