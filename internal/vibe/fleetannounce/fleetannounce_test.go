package fleetannounce

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
	orig := execSh
	execSh = func(ctx context.Context, cmd string) (string, error) {
		verbs = append(verbs, cmd)
		return "", nil
	}
	defer func() { execSh = orig }()

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
	orig := execSh
	execSh = func(ctx context.Context, cmd string) (string, error) {
		return "", fmt.Errorf("exit status 4")
	}
	defer func() { execSh = orig }()

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
	orig := execSh
	execSh = func(ctx context.Context, cmd string) (string, error) {
		verbs++
		return "", nil
	}
	defer func() { execSh = orig }()

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
	orig := execSh
	execSh = func(ctx context.Context, cmd string) (string, error) {
		verbs++
		return "", nil
	}
	defer func() { execSh = orig }()

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

// TestRegistryFailureBackoffAndRecovery drives a registry that fails
// then heals, and asserts both halves the name claims: attempts during
// the outage are BOUNDED well below the announce cadence (the backoff
// actually grows), and a successful announce lands after the flip
// (recovery). Ranges, not exact counts — an exact count is a timing
// flake on loaded CI.
func TestRegistryFailureBackoffAndRecovery(t *testing.T) {
	swap := newFakeLlamaSwap(t, `{"running":[]}`)

	var mu sync.Mutex
	attempts := 0
	healed := false
	okAfterHeal := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		up := healed
		if up {
			okAfterHeal++
		}
		mu.Unlock()
		if !up {
			http.Error(w, "registry down", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"interval_s":1}`))
	}))
	t.Cleanup(srv.Close)

	c, err := New(Config{
		Cell:         "gpu-cell",
		RegistryURL:  srv.URL,
		LlamaSwapURL: swap.srv.URL,
		Interval:     10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	// 3s of outage: at a 10ms cadence an un-backed-off loop would try
	// ~300 times; the 1s→2s→4s… backoff caps it around a handful.
	time.Sleep(3 * time.Second)
	mu.Lock()
	during := attempts
	healed = true
	mu.Unlock()
	if during > 20 {
		t.Errorf("%d attempts during a 3s outage — the backoff is not growing", during)
	}
	if during < 2 {
		t.Errorf("%d attempts during the outage — it stopped retrying entirely", during)
	}

	deadline := time.After(15 * time.Second)
	for {
		mu.Lock()
		ok := okAfterHeal
		mu.Unlock()
		if ok > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("no successful announce after the registry healed")
		case <-time.After(50 * time.Millisecond):
		}
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v, want nil on cancel", err)
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

// TestSetLocalIntentConcurrent pins MIN-D: N concurrent writers leave a
// file that always parses AND matches the in-memory intent at quiesce —
// a fixed ".tmp" name tore the file, and an unserialised (mutate, write)
// left the loser's state on disk.
func TestSetLocalIntentConcurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "intent.json")
	c, err := New(Config{Cell: "laptop", RegistryURL: "http://127.0.0.1:1", IntentPath: path})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := range 32 {
		state := "drained"
		if i%2 == 0 {
			state = "serving"
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.SetLocalIntent(state); err != nil {
				t.Errorf("SetLocalIntent: %v", err)
			}
		}()
	}
	wg.Wait()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk fleetapi.AnnounceIntent
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatalf("intent file did not parse (%v): %s", err, data)
	}
	if got := c.LocalIntent(); onDisk.State != got.State || !onDisk.Since.Equal(got.Since) {
		t.Errorf("on-disk %+v != in-memory %+v", onDisk, got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("leftover temp files: %v", entries)
	}
}

// TestWithdrawConcurrentWithRun pins the mutex behind seq/authRejected:
// a shutdown path can call Withdraw while the loop is still in flight,
// and under -race the unsynchronised version fails here. Seqs must also
// stay UNIQUE — fleetd retires a piggybacked command batch only on a
// higher seq, so two announces sharing a number redeliver forever.
func TestWithdrawConcurrentWithRun(t *testing.T) {
	swap := newFakeLlamaSwap(t, `{"running":[]}`)
	var mu sync.Mutex
	seen := map[uint64]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req fleetapi.AnnounceRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		seen[req.Seq]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"interval_s":1}`))
	}))
	t.Cleanup(srv.Close)

	c, err := New(Config{
		Cell:         "laptop",
		RegistryURL:  srv.URL,
		LlamaSwapURL: swap.srv.URL,
		Interval:     time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer wcancel()
			_ = c.Withdraw(wctx)
		}()
	}
	wg.Wait()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v, want nil on cancel", err)
	}

	mu.Lock()
	defer mu.Unlock()
	for seq, n := range seen {
		if n > 1 {
			t.Errorf("seq %d was handed out %d times", seq, n)
		}
	}
}

// C7a: the usage block is additive and rides the heartbeat. A nil Usage
// hook must leave the field absent — old cells announce no usage and
// fleetd renders them unmeasured, which is the truth, not zero.
func TestAnnounce_UsageBlockRidesTheHeartbeatAndIsOptional(t *testing.T) {
	reg := newFakeRegistry(t)
	swap := newFakeLlamaSwap(t, `{"running":[]}`)

	base := Config{
		Cell:         "gpu",
		RegistryURL:  reg.srv.URL,
		LlamaSwapURL: swap.srv.URL,
		IntentPath:   filepath.Join(t.TempDir(), "cell-intent.json"),
	}

	silent, err := New(base)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := silent.announceOnce(context.Background()); err != nil {
		t.Fatalf("announceOnce: %v", err)
	}
	if got := reg.calls(); len(got) != 1 || got[0].Usage != nil {
		t.Fatalf("nil Usage hook must omit the block, got %+v", got[0].Usage)
	}

	withUsage := base
	var sawCtx bool
	withUsage.Usage = func(ctx context.Context) *fleetapi.AnnounceUsage {
		sawCtx = ctx != nil
		return &fleetapi.AnnounceUsage{
			Epoch:  "e1",
			Models: []fleetapi.AnnounceUsageModel{{Model: "qwen", Basis: "chat", Req: 3, InFresh: 30, InCached: 900, Out: 12}},
		}
	}
	c, err := New(withUsage)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.announceOnce(context.Background()); err != nil {
		t.Fatalf("announceOnce: %v", err)
	}
	calls := reg.calls()
	last := calls[len(calls)-1]
	if last.Usage == nil || last.Usage.Epoch != "e1" || len(last.Usage.Models) != 1 {
		t.Fatalf("usage block did not reach the registry: %+v", last.Usage)
	}
	if m := last.Usage.Models[0]; m.InFresh != 30 || m.InCached != 900 || m.Basis != "chat" {
		t.Errorf("usage model = %+v", m)
	}
	if !sawCtx {
		t.Error("Usage hook was called without the announce context")
	}
}

// A cloud peer's catalog ids are its cloud_peer.models entries, never its def
// name, so they can never land in `covered` — and every announce reported all
// of them as drift with no backend def. The def is right there; the message
// sends an operator hunting a YAML that already exists. Announcing them
// hashless is correct (there is no argv to fingerprint for a model this cell
// does not run) — only the diagnostic was wrong.
func TestGatherModelsCloudPeerIDsAreNotReportedAsDefless(t *testing.T) {
	reg := newFakeRegistry(t)
	swap := newFakeLlamaSwap(t, `{"running":[{"model":"qwen","state":"ready"}]}`)
	// The catalog carries the peer's model ids plus one genuine stray.
	swap.catalog = `{"object":"list","data":[{"id":"qwen"},{"id":"claude-sonnet-5"},{"id":"claude-opus-5"},{"id":"a-real-stray"}]}`

	peer := &profile.BackendDef{
		Name: "anthropic",
		Backend: profile.Backend{External: true, CloudPeer: &profile.CloudPeerBackend{
			BaseURL: "https://api.example.test",
			Models:  []string{"claude-sonnet-5", "claude-opus-5"},
		}},
	}
	var logs bytes.Buffer
	cfg := testConfig(reg, swap, testDef("qwen", ""), peer)
	cfg.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.announceOnce(t.Context()); err != nil {
		t.Fatal(err)
	}

	got := logs.String()
	for _, id := range []string{"claude-sonnet-5", "claude-opus-5"} {
		if strings.Contains(got, id) {
			t.Errorf("peer model %q was reported as having no backend def:\n%s", id, got)
		}
	}
	// The warning must still fire for something that genuinely has no def, or
	// this fix would have silenced real drift.
	if !strings.Contains(got, "a-real-stray") {
		t.Errorf("a genuinely defless catalog id stopped being reported:\n%s", got)
	}

	// ...and the peer models are still announced, hashless.
	last := reg.calls()[len(reg.calls())-1]
	byID := map[string]fleetapi.AnnounceModel{}
	for _, m := range last.Models {
		byID[m.ID] = m
	}
	for _, id := range []string{"claude-sonnet-5", "claude-opus-5"} {
		m, ok := byID[id]
		if !ok {
			t.Errorf("peer model %q stopped being announced", id)
			continue
		}
		if m.FlagsSHA256 != "" {
			t.Errorf("peer model %q announced a flags fingerprint; there is no argv to hash", id)
		}
	}
}
