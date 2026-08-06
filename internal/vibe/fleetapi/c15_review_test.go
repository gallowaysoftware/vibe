package fleetapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
	"github.com/gallowaysoftware/vibe/internal/vibe/router"
)

// fleet-control C15, adversarial-review pass. Each test here pins a
// defect the phase shipped, and each fails against the pre-fix code.

// ── REV-1: the key file's PATH must not reach a guest ───────────────────

// TestSwapAuthDetailCarriesNoFilesystemPath is the guest-boundary pin.
// `GET /api/fleet/state` is C12's one guest-readable route, and the
// unresolvable branch formatted the resolver's error — which embeds the
// declared path — straight into `swap_auth[].detail`. The phase doc
// claimed the opposite ("the config key, not the path, because that
// document is guest-readable") while the code did it in two places.
func TestSwapAuthDetailCarriesNoFilesystemPath(t *testing.T) {
	front := newKeyedSwap(t, labSwapKey)
	dir := filepath.Join(t.TempDir(), "house-secrets")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "front-swap.key")
	s := newC15Server(t, front, hostsWithKeyPath(t, front.srv.URL, missing))

	snap := s.Snapshot(t.Context())
	blob, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if snap.SwapAuth == nil || len(snap.SwapAuth.Cells) != 1 {
		t.Fatalf("no swap_auth row to check: %+v", snap.SwapAuth)
	}
	if strings.Contains(string(blob), missing) || strings.Contains(string(blob), dir) {
		t.Errorf("the guest-readable state document carries the key file's path:\n%s", snap.SwapAuth.Cells[0].Detail)
	}
	// ...and the diagnosis still names what to fix.
	if !strings.Contains(snap.SwapAuth.Cells[0].Detail, "cells.front.swap_key_file") {
		t.Errorf("detail = %q, want the config key named", snap.SwapAuth.Cells[0].Detail)
	}
}

// The 401-with-an-unresolvable-key branch of NoteSwapStatus is a second
// producer of the same sentence, and a guard in one of two producers is
// not a guard.
func TestNoteSwapStatus_UnresolvableDetailCarriesNoPath(t *testing.T) {
	front := newKeyedSwap(t, labSwapKey)
	missing := filepath.Join(t.TempDir(), "gone-at-0300.key")
	s := newC15Server(t, front, hostsWithKeyPath(t, front.srv.URL, missing))
	s.NoteSwapStatus(fleetcfg.FrontCell, 401)
	f, ok := s.swapAuthState(fleetcfg.FrontCell)
	if !ok || f.Kind != SwapAuthUnresolvable {
		t.Fatalf("state = %+v, want an unresolvable record", f)
	}
	if strings.Contains(f.Detail, missing) {
		t.Errorf("detail carries the path on the 401 branch too: %q", f.Detail)
	}
}

// The warm status rows ride the same guest-readable document, and they
// render whatever error the warm produced.
func TestWarmRowCarriesNoFilesystemPath(t *testing.T) {
	front := newKeyedSwap(t, labSwapKey)
	missing := filepath.Join(t.TempDir(), "gone.key")
	s := newC15Server(t, front, hostsWithKeyPath(t, front.srv.URL, missing))
	s.cells = append(s.cells, Cell{Name: "heavy", URL: "http://127.0.0.1:1", Class: "always_on"})
	presenceOf(s, "heavy", AnnounceModel{ID: "default-model", State: "ready"})

	st := &warmTargetState{Cell: "heavy", Model: "default-model"}
	s.mu.Lock()
	s.warmStates = append(s.warmStates, st)
	s.mu.Unlock()
	s.restore(WarmTarget{Cell: "heavy", Model: "default-model"}, st,
		warmLoopConfig{frontURL: front.srv.URL, warmFn: s.warmViaFront}, "nothing resident")

	got := warmTargetStateOf(s, 0)
	if strings.Contains(got.Detail, missing) {
		t.Errorf("the warm row carries the key file's path: %q", got.Detail)
	}
}

// ── REV-2: an unresolvable credential is never routed around ────────────

// TestUnresolvableCredentialIsNeverPiggybacked. §5 rejects "the front
// refused us, so send the warm to the cell's own llama-swap" by name, and
// the rejection held only for the 401 — a typed *warmHTTPError. An
// unresolvable key file returned an untyped error, which queueWarm reads
// as a DELIVERY failure, so the very first tick after a key file went
// missing queued a warm to the cell and executed it against the cell's
// own llama-swap.
func TestUnresolvableCredentialIsNeverPiggybacked(t *testing.T) {
	front := newKeyedSwap(t, labSwapKey)
	missing := filepath.Join(t.TempDir(), "gone.key")
	s := newC15Server(t, front, hostsWithKeyPath(t, front.srv.URL, missing))
	s.cells = append(s.cells, Cell{Name: "heavy", URL: "http://127.0.0.1:1", Class: "always_on"})
	presenceOf(s, "heavy", AnnounceModel{ID: "default-model", State: "ready"})

	warmErr := s.warmViaFront(t.Context(), front.srv.URL, "default-model")
	if warmErr == nil {
		t.Fatal("an unresolvable key file produced no error")
	}
	if !definitiveWarmRefusal(warmErr) {
		t.Error("fleetd refusing to send was read as a failure to DELIVER")
	}
	if _, qerr := s.queueWarm("heavy", "default-model", warmErr); qerr == nil {
		t.Fatal("the warm was queued to the cell — the cell cannot fix the front's key file, " +
			"and executing it there routes around the broken credential")
	}

	st := &warmTargetState{Cell: "heavy", Model: "default-model"}
	s.mu.Lock()
	s.warmStates = append(s.warmStates, st)
	s.mu.Unlock()
	s.restore(WarmTarget{Cell: "heavy", Model: "default-model"}, st,
		warmLoopConfig{frontURL: front.srv.URL, warmFn: s.warmViaFront}, "nothing resident")
	s.mu.Lock()
	queued := len(s.commands["heavy"])
	s.mu.Unlock()
	if queued != 0 {
		t.Fatalf("the warm-target restore queued %d command(s) after failing to resolve the front's credential", queued)
	}
}

// ── REV-3: a declared front_extras that is not there ────────────────────

// TestRenderRefusesWhenDeclaredExtrasAreMissing. router.mergeExtras maps
// ErrNotExist to "no extras, no error", so a typo'd fleet.front_extras
// (or one whose container mount is absent) rendered a front config with
// NO apiKeys over one that had them — the front then stops demanding a
// key at all, which is strictly worse than the 401 this phase fixes.
func TestRenderRefusesWhenDeclaredExtrasAreMissing(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("apiKeys:\n    - the-front-key\nmodels: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	front := newKeyedSwap(t, labSwapKey)
	hosts := twoCellHosts(t, front.srv.URL)
	s := newC15Server(t, front, hosts)

	wrote := ""
	rl := &renderLoop{srv: s, cfg: RenderLoopConfig{
		FrontExtras:     filepath.Join(dir, "not-mounted.yaml"),
		FrontConfigPath: cfgPath,
		Hosts:           hosts,
		LoadDefs:        func(string) ([]*profile.BackendDef, error) { return []*profile.BackendDef{peerDefC15()}, nil },
		Render:          router.Render,
		WriteFile: func(_ string, b []byte) error {
			wrote = string(b)
			return nil
		},
	}}
	err := rl.renderPass()
	if err == nil {
		t.Fatalf("the render proceeded with an unreadable front_extras and wrote:\n%s", wrote)
	}
	if !strings.Contains(err.Error(), "front_extras") {
		t.Errorf("refusal does not name the config: %v", err)
	}
	if wrote != "" {
		t.Errorf("a config was written anyway:\n%s", wrote)
	}

	// But a fleet with NO front config yet must still render: there is
	// nothing to protect, and refusing would deadlock a first boot.
	rl.cfg.FrontConfigPath = filepath.Join(dir, "never-written.yaml")
	if err := rl.renderPass(); err != nil {
		t.Fatalf("the gate blocked a first-boot render with nothing to erase: %v", err)
	}
	if wrote == "" {
		t.Error("first boot wrote nothing")
	}
}

func peerDefC15() *profile.BackendDef {
	return &profile.BackendDef{
		Name: "peer-model", Cell: "heavy",
		Backend: profile.Backend{
			External:    true,
			LlamaServer: &profile.LlamaServerBackend{Path: "/models/x.gguf", Alias: "peer-model", Context: 4096},
		},
	}
}

func twoCellHosts(t *testing.T, frontURL string) *fleetcfg.File {
	t.Helper()
	dir := t.TempDir()
	kp := filepath.Join(dir, "front.key")
	if err := os.WriteFile(kp, []byte(labSwapKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "hosts.yaml")
	body := "cells:\n  front:\n    url: \"" + frontURL + "\"\n    class: always_on\n    swap_key_file: \"" + kp + "\"\n" +
		"  heavy:\n    url: \"http://127.0.0.1:9999\"\n    class: always_on\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := fleetcfg.LoadFrom(p)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
