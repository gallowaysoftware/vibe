package fleetannounce

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
)

// fakeRegistry captures announces and scripts responses.
type fakeRegistry struct {
	srv *httptest.Server

	mu        sync.Mutex
	announces []fleetapi.AnnounceRequest
	response  fleetapi.AnnounceResponse
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{response: fleetapi.AnnounceResponse{IntervalS: 15}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/fleet/announce", func(w http.ResponseWriter, r *http.Request) {
		var req fleetapi.AnnounceRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.announces = append(f.announces, req)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f.response)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRegistry) calls() []fleetapi.AnnounceRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fleetapi.AnnounceRequest{}, f.announces...)
}

// fakeLlamaSwap serves /running + /v1/models and records unload/warm.
type fakeLlamaSwap struct {
	srv      *httptest.Server
	running  string
	catalog  string
	mu       sync.Mutex
	unloaded []string
	warmed   []string
}

func newFakeLlamaSwap(t *testing.T, running string) *fakeLlamaSwap {
	t.Helper()
	f := &fakeLlamaSwap{running: running}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /running", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(f.running))
	})
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		cat := f.catalog
		if cat == "" {
			// Default: derive from /running so simple fixtures need no
			// second body.
			var wrap struct {
				Running []struct {
					Model string `json:"model"`
				} `json:"running"`
			}
			_ = json.Unmarshal([]byte(f.running), &wrap)
			var data []map[string]string
			for _, m := range wrap.Running {
				data = append(data, map[string]string{"id": m.Model, "object": "model"})
			}
			b, _ := json.Marshal(map[string]any{"object": "list", "data": data})
			cat = string(b)
		}
		_, _ = w.Write([]byte(cat))
	})
	mux.HandleFunc("POST /api/models/unload/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.unloaded = append(f.unloaded, strings.TrimPrefix(r.URL.Path, "/api/models/unload/"))
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.warmed = append(f.warmed, fmt.Sprint(body["model"]))
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "x"})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func testDef(name, fingerprint string) *profile.BackendDef {
	return &profile.BackendDef{
		Name: name,
		Backend: profile.Backend{
			External:    true,
			LlamaServer: &profile.LlamaServerBackend{Path: "/models/" + name + ".gguf", Alias: name, Context: 4096},
		},
		Fingerprint: fingerprint,
	}
}

func testConfig(reg *fakeRegistry, swap *fakeLlamaSwap, defs ...*profile.BackendDef) Config {
	return Config{
		Cell:              "gpu-cell",
		RegistryURL:       reg.srv.URL,
		LlamaSwapURL:      swap.srv.URL,
		Defs:              defs,
		LlamaServerBinary: "/usr/bin/llama-server",
		IntentPath:        filepath.Join(os.TempDir(), fmt.Sprintf("fa-test-%d", time.Now().UnixNano()), "cell-intent.json"),
	}
}

func TestAnnounceSchemaAndSeq(t *testing.T) {
	reg := newFakeRegistry(t)
	swap := newFakeLlamaSwap(t, `{"running":[{"model":"qwen","state":"ready"}]}`)
	c, err := New(testConfig(reg, swap, testDef("qwen", "strict")))
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := c.announceOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	calls := reg.calls()
	if len(calls) != 3 {
		t.Fatalf("announces = %d", len(calls))
	}
	for i, req := range calls {
		if req.V != fleetapi.AnnounceVersion {
			t.Errorf("v = %d", req.V)
		}
		if req.Cell != "gpu-cell" || req.Seq != uint64(i) {
			t.Errorf("cell/seq = %s/%d", req.Cell, req.Seq)
		}
		if req.Intent == nil || req.Intent.State != "serving" {
			t.Errorf("intent = %+v", req.Intent)
		}
		if len(req.Models) != 1 || req.Models[0].ID != "qwen" || req.Models[0].State != "ready" {
			t.Errorf("models = %+v", req.Models)
		}
		if req.Models[0].FlagsSHA256 == "" || req.Models[0].Fingerprint != "strict" {
			t.Errorf("fingerprint = %+v", req.Models[0])
		}
	}
	// The fingerprint is stable across announces (same def, same render).
	if calls[0].Models[0].FlagsSHA256 != calls[2].Models[0].FlagsSHA256 {
		t.Error("hash drifted across identical announces")
	}
}

func TestConflictRule_NewerDesiredExecutes(t *testing.T) {
	reg := newFakeRegistry(t)
	swap := newFakeLlamaSwap(t, `{"running":[]}`)
	reg.response.DesiredIntent = &fleetapi.Intent{
		State: "drained", Reason: "via MCP", Since: time.Now().UTC().Add(time.Minute),
	}
	cfg := testConfig(reg, swap)
	cfg.CellCmds = CellCmds{Drain: "systemctl stop x", Resume: "systemctl start x"}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	var verbs []string
	orig := execCmd
	execCmd = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		verbs = append(verbs, strings.Join(args, " "))
		return nil, nil
	}
	defer func() { execCmd = orig }()

	if err := c.announceOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(verbs) != 1 || !strings.Contains(verbs[0], "systemctl stop x") {
		t.Errorf("verbs = %v, want the drain executed", verbs)
	}
	if got := c.LocalIntent(); got.State != "drained" || got.Since.IsZero() {
		t.Errorf("local intent = %+v, want drained with a timestamp", got)
	}
	// Next announce echoes drained; fleetd's stale-request logic drops it.
	reg.response.DesiredIntent = nil
	if err := c.announceOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	last := reg.calls()[len(reg.calls())-1]
	if last.Intent.State != "drained" {
		t.Errorf("echo = %s, want drained", last.Intent.State)
	}
}

func TestConflictRule_FailingVerbNeverAcks(t *testing.T) {
	// The false-ack finding: a drain whose verb FAILS must not stamp
	// intent — the echo stays serving and the request stays pending.
	reg := newFakeRegistry(t)
	swap := newFakeLlamaSwap(t, `{"running":[]}`)
	reg.response.DesiredIntent = &fleetapi.Intent{
		State: "drained", Reason: "via MCP", Since: time.Now().UTC().Add(time.Minute),
	}
	cfg := testConfig(reg, swap)
	cfg.CellCmds = CellCmds{Drain: "false"}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	orig := execCmd
	execCmd = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("exit status 4")
	}
	defer func() { execCmd = orig }()

	if err := c.announceOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := c.LocalIntent(); got.State != "serving" {
		t.Errorf("failed verb stamped intent: %s — a failed drain never records", got.State)
	}
	last := reg.calls()[len(reg.calls())-1]
	if last.Intent.State != "serving" {
		t.Errorf("echo = %s, want serving (request stays pending)", last.Intent.State)
	}
}

func TestConflictRule_NoVerbsNeverAcks(t *testing.T) {
	// The slim-cell case: no cell_cmds at all. The drain request must
	// stay pending forever (visible, actionable) — never a fake ack.
	reg := newFakeRegistry(t)
	swap := newFakeLlamaSwap(t, `{"running":[]}`)
	reg.response.DesiredIntent = &fleetapi.Intent{
		State: "drained", Reason: "via MCP", Since: time.Now().UTC().Add(time.Minute),
	}
	c, err := New(testConfig(reg, swap)) // no CellCmds
	if err != nil {
		t.Fatal(err)
	}
	if err := c.announceOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := c.LocalIntent(); got.State != "serving" {
		t.Errorf("verb-less cell acked a drain: %s", got.State)
	}
}

func TestConflictRule_AlreadyInStateReStamps(t *testing.T) {
	// The ghost-request livelock: the cell is already drained with an
	// OLDER stamp than the request — reconcile must advance the echo
	// (no verb runs) or the request is handed back forever.
	reg := newFakeRegistry(t)
	swap := newFakeLlamaSwap(t, `{"running":[]}`)
	c, err := New(testConfig(reg, swap))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetLocalIntent("drained"); err != nil {
		t.Fatal(err)
	}
	// The request is NEWER than the local stamp (same state).
	reg.response.DesiredIntent = &fleetapi.Intent{
		State: "drained", Reason: "x", Since: time.Now().UTC().Add(time.Minute),
	}
	verbs := 0
	orig := execCmd
	execCmd = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		verbs++
		return nil, nil
	}
	defer func() { execCmd = orig }()

	before := c.LocalIntent().Since
	if err := c.announceOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if verbs != 0 {
		t.Error("a verb ran for an already-in-state request")
	}
	after := c.LocalIntent()
	if after.State != "drained" || !after.Since.After(before) {
		t.Errorf("local intent did not advance: before %v after %+v — the request never resolves", before, after)
	}
}

func TestConflictRule_OlderDesiredIgnored(t *testing.T) {
	reg := newFakeRegistry(t)
	swap := newFakeLlamaSwap(t, `{"running":[]}`)
	reg.response.DesiredIntent = &fleetapi.Intent{
		State: "drained", Reason: "stale", Since: time.Now().UTC().Add(-time.Hour),
	}
	cfg := testConfig(reg, swap)
	cfg.CellCmds = CellCmds{Drain: "true"}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// The human at the box touched intent NOW — newer than the request.
	if err := c.SetLocalIntent("serving"); err != nil {
		t.Fatal(err)
	}
	verbs := 0
	orig := execCmd
	execCmd = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		verbs++
		return nil, nil
	}
	defer func() { execCmd = orig }()

	if err := c.announceOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if verbs != 0 {
		t.Error("a stale registry request executed over a newer local change — split-brain toward the wrong side")
	}
}

func TestCommandsExecute(t *testing.T) {
	reg := newFakeRegistry(t)
	swap := newFakeLlamaSwap(t, `{"running":[]}`)
	reg.response.Commands = []fleetapi.AnnounceCommand{
		{Verb: "unload", Model: "qwen"},
		{Verb: "warm", Model: "gemma"},
		{Verb: "bogus", Model: "x"},
	}
	c, err := New(testConfig(reg, swap))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.announceOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(swap.unloaded) != 1 || swap.unloaded[0] != "qwen" {
		t.Errorf("unloaded = %v", swap.unloaded)
	}
	if len(swap.warmed) != 1 || swap.warmed[0] != "gemma" {
		t.Errorf("warmed = %v", swap.warmed)
	}
}

func TestLocalIntentPersistsAcrossRestart(t *testing.T) {
	reg := newFakeRegistry(t)
	swap := newFakeLlamaSwap(t, `{"running":[]}`)
	cfg := testConfig(reg, swap)
	c1, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := c1.SetLocalIntent("drained"); err != nil {
		t.Fatal(err)
	}
	c2, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := c2.LocalIntent(); got.State != "drained" {
		t.Errorf("restarted client intent = %s, want drained (persisted)", got.State)
	}
}

func TestRegistryFailureBackoffAndRecovery(t *testing.T) {
	swap := newFakeLlamaSwap(t, `{"running":[]}`)
	cfg := Config{
		Cell:         "gpu-cell",
		RegistryURL:  "http://127.0.0.1:1", // nothing there
		LlamaSwapURL: swap.srv.URL,
		Interval:     20 * time.Millisecond,
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	// Run must keep retrying quietly and exit cleanly on cancel — the
	// control plane's death is not the data plane's.
	if err := c.Run(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestGatherModelsDefCatalogIntersection(t *testing.T) {
	// A box hosting more than one cell must not let one cell inherit the
	// others' defs (the scratch-cell leak): defs announce only when the
	// cell's own catalog carries them. Catalog ids with no def announce
	// hashless (visible, unverifiable).
	reg := newFakeRegistry(t)
	swap := newFakeLlamaSwap(t, `{"running":[{"model":"mine","state":"ready"}]}`)
	swap.catalog = `{"object":"list","data":[{"id":"mine","object":"model"},{"id":"defless","object":"model"}]}`
	c, err := New(testConfig(reg, swap, testDef("mine", "strict"), testDef("not-here", "")))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.announceOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	last := reg.calls()[len(reg.calls())-1]
	byID := map[string]fleetapi.AnnounceModel{}
	for _, m := range last.Models {
		byID[m.ID] = m
	}
	if _, ok := byID["not-here"]; ok {
		t.Error("a def absent from the cell's catalog announced (the multi-cell-box leak)")
	}
	if m, ok := byID["mine"]; !ok || m.FlagsSHA256 == "" || m.State != "ready" {
		t.Errorf("mine = %+v", m)
	}
	if m, ok := byID["defless"]; !ok || m.FlagsSHA256 != "" {
		t.Errorf("defless = %+v, want hashless but present", m)
	}
}

func TestGatherModelsComfyUIDoesNotPanic(t *testing.T) {
	// Regression (live crash 2026-08-02): gatherModels called ModelCmd on
	// every external def; comfyui has no llama spec → nil deref. The
	// fingerprint must cover spec-rendered kinds only, state included for
	// the rest.
	reg := newFakeRegistry(t)
	swap := newFakeLlamaSwap(t, `{"running":[{"model":"comfyui","state":"ready"},{"model":"qwen","state":"ready"}]}`)
	comfy := &profile.BackendDef{
		Name:    "comfyui",
		Backend: profile.Backend{External: true, ComfyUI: &profile.ComfyUIBackend{Dir: "/opt/comfy", Port: 8188}},
	}
	c, err := New(testConfig(reg, swap, comfy, testDef("qwen", "")))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.announceOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	last := reg.calls()[len(reg.calls())-1]
	var comfyModel, qwenModel *fleetapi.AnnounceModel
	for i := range last.Models {
		switch last.Models[i].ID {
		case "comfyui":
			comfyModel = &last.Models[i]
		case "qwen":
			qwenModel = &last.Models[i]
		}
	}
	if comfyModel == nil || comfyModel.State != "ready" || comfyModel.FlagsSHA256 != "" {
		t.Errorf("comfyui = %+v, want ready with no hash", comfyModel)
	}
	if qwenModel == nil || qwenModel.FlagsSHA256 == "" {
		t.Errorf("qwen = %+v, want hashed", qwenModel)
	}
}

func TestGatherModelsWithoutLlamaSwap(t *testing.T) {
	reg := newFakeRegistry(t)
	// llama-swap down: announce still goes out with an empty model list —
	// "the unit is stopped" must not read as "the cell is gone".
	cfg := testConfig(reg, newFakeLlamaSwap(t, `{"running":[]}`), testDef("qwen", ""))
	cfg.LlamaSwapURL = "http://127.0.0.1:1"
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.announceOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	last := reg.calls()[len(reg.calls())-1]
	if len(last.Models) != 0 {
		t.Errorf("models = %+v, want empty when llama-swap is down", last.Models)
	}
}
