package fleetmcp

// C8: the probe_model verb. The three things it must not do are the
// phase's failure modes — warm on demand, force past a guard, or act on
// a verdict — so they are what these tests pin.
//
// The fixture drives the REAL paths end to end: announces go over
// POST /api/fleet/announce and queued commands come back on an announce
// RESPONSE, so nothing here can pass against a facade that only talks to
// test-only setters.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
)

// probeFixture wires a facade over a fleetapi server watching one fake
// cell whose /api/events reports an idle in-flight count.
type probeFixture struct {
	s     *Server
	fleet *fleetapi.Server
	api   *httptest.Server
	seq   uint64
}

func newProbeFixture(t *testing.T) *probeFixture {
	t.Helper()
	cellMux := http.NewServeMux()
	cellMux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			return
		}
		// llama-swap's inflight frame: a JSON envelope whose data is a
		// JSON STRING. Reporting zero requests is what makes the guard
		// EVALUABLE — "unknown in-flight is not zero in-flight".
		fmt.Fprintf(w, "data:%s\n\n", `{"type":"inflight","data":"{\"requests\":[]}"}`)
		fl.Flush()
		<-r.Context().Done()
	})
	cellMux.HandleFunc("GET /running", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"running":[]}`))
	})
	cell := httptest.NewServer(cellMux)
	t.Cleanup(cell.Close)

	dir := t.TempDir()
	cells := map[string]fleetcfg.Cell{
		"front":    {URL: "http://front.invalid"},
		"gpu-cell": {URL: cell.URL, Class: fleetcfg.ClassOpportunistic},
	}
	fleet := fleetapi.New(
		[]fleetapi.Cell{{Name: "front", URL: "http://front.invalid"}, {Name: "gpu-cell", URL: cell.URL, Class: string(fleetcfg.ClassOpportunistic)}},
		dir+"/history.json",
		func() fleetapi.DaemonInfo { return fleetapi.DaemonInfo{} },
		fleetapi.Options{IntentPath: dir + "/intent.json", LastSeenPath: dir + "/last-seen.json"},
	)
	mux := http.NewServeMux()
	fleet.Register(mux)
	api := httptest.NewServer(mux)
	fleet.Start()
	t.Cleanup(func() {
		api.Close()
		fleet.Close()
	})

	f := &probeFixture{s: New(fleet, &fleetcfg.File{Cells: cells}, Options{}), fleet: fleet, api: api}
	f.waitForInFlight(t)
	return f
}

// waitForInFlight blocks until the watcher has parsed one inflight frame.
func (f *probeFixture) waitForInFlight(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, reported := f.fleet.InFlight("gpu-cell").Observed(); reported {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the cell's in-flight count was never reported; the fixture's guard state is not what the test assumes")
}

// announce posts one heartbeat and returns the response, which is where
// queued commands are delivered.
func (f *probeFixture) announce(t *testing.T, models ...fleetapi.AnnounceModel) fleetapi.AnnounceResponse {
	t.Helper()
	f.seq++
	body, err := json.Marshal(fleetapi.AnnounceRequest{
		V: fleetapi.AnnounceVersion, Cell: "gpu-cell", Seq: f.seq,
		Intent: &fleetapi.AnnounceIntent{State: "serving", Since: time.Now().UTC()},
		Models: models,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(f.api.URL+"/api/fleet/announce", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("announce: HTTP %d", resp.StatusCode)
	}
	var out fleetapi.AnnounceResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

type probeToolResult struct {
	Cell   string                  `json:"cell"`
	Model  string                  `json:"model"`
	Note   string                  `json:"note"`
	Latest *fleetapi.AnnounceProbe `json:"latest_result"`
}

func decodeProbeResult(t *testing.T, text string) probeToolResult {
	t.Helper()
	var out probeToolResult
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("probe_model returned non-JSON %q: %v", text, err)
	}
	return out
}

// TestProbeModelTool_QueuesTheProbeAndReturnsTheLastKnownResult.
func TestProbeModelTool_QueuesTheProbeAndReturnsTheLastKnownResult(t *testing.T) {
	f := newProbeFixture(t)
	f.announce(t, fleetapi.AnnounceModel{ID: "qwen", State: "ready", Probe: &fleetapi.AnnounceProbe{
		Metric: "decode_tok_s", Value: 41, BaselineP50: 40, Verdict: fleetapi.VerdictOK,
	}})

	text, err := f.s.toolProbeModel("gpu-cell", "qwen", false)
	if err != nil {
		t.Fatalf("probe_model: %v", err)
	}
	got := decodeProbeResult(t, text)
	if !strings.Contains(got.Note, "queued") {
		t.Fatalf("note does not say the probe was queued: %q", got.Note)
	}
	if got.Latest == nil || got.Latest.Value != 41 {
		t.Fatalf("the last known result did not ride along: %+v", got.Latest)
	}
	resp := f.announce(t, fleetapi.AnnounceModel{ID: "qwen", State: "ready"})
	if len(resp.Commands) != 1 || resp.Commands[0].Verb != "probe" || resp.Commands[0].Model != "qwen" {
		t.Fatalf("the cell did not collect the probe: %+v", resp.Commands)
	}
}

// TestProbeModelTool_RefusesAColdModelAndPointsAtWarm is the load rule
// at the agent surface: the tool must not quietly become a warm.
func TestProbeModelTool_RefusesAColdModelAndPointsAtWarm(t *testing.T) {
	f := newProbeFixture(t)
	f.announce(t, fleetapi.AnnounceModel{ID: "qwen", State: "stopped"})

	text, err := f.s.toolProbeModel("gpu-cell", "qwen", false)
	if err != nil {
		t.Fatalf("a guard skip must be an ANSWER, not a tool error: %v", err)
	}
	got := decodeProbeResult(t, text)
	if !strings.Contains(got.Note, "not resident") || !strings.Contains(got.Note, "warm it first") {
		t.Fatalf("want a residency refusal naming the declared verb, got %q", got.Note)
	}
	if resp := f.announce(t, fleetapi.AnnounceModel{ID: "qwen", State: "stopped"}); len(resp.Commands) != 0 {
		t.Fatalf("queued a probe that would have loaded the model: %+v", resp.Commands)
	}
}

// TestProbeModelTool_ReportsAGuardSkipWithoutAnOverride: there is no
// force flag, by design — a busy box is an answer.
func TestProbeModelTool_ReportsAGuardSkipWithoutAnOverride(t *testing.T) {
	f := newProbeFixture(t)
	// A cell that never announced is the guard state reachable without
	// racing the watcher; the busy and leased variants are pinned against
	// the scheduler in fleetapi's c8_test.go, which shares this guard.
	text, err := f.s.toolProbeModel("gpu-cell", "qwen", false)
	if err != nil {
		t.Fatalf("probe_model: %v", err)
	}
	if got := decodeProbeResult(t, text); !strings.Contains(got.Note, "never announced") {
		t.Fatalf("want the guard reason, got %q", got.Note)
	}
	// And the schema offers no way to override any of it.
	for _, tool := range f.s.mcpTools() {
		m := tool.(map[string]any)
		if m["name"] != "probe_model" {
			continue
		}
		props := m["inputSchema"].(map[string]any)["properties"].(map[string]any)
		for _, banned := range []string{"force", "warm", "load"} {
			if _, ok := props[banned]; ok {
				t.Fatalf("probe_model exposes a %q argument", banned)
			}
		}
	}
}

// TestProbeModelTool_UnknownCellAndEmptyModelFailLoudly: a typo must not
// hit nothing (unload_model's rule).
func TestProbeModelTool_UnknownCellAndEmptyModelFailLoudly(t *testing.T) {
	f := newProbeFixture(t)
	if _, err := f.s.toolProbeModel("nope", "qwen", false); err == nil {
		t.Fatal("probing an unknown cell succeeded")
	}
	if _, err := f.s.toolProbeModel("gpu-cell", "  ", false); err == nil {
		t.Fatal("probing an empty model id succeeded")
	}
}

// TestProbeModelTool_RebaselineIsCarriedToTheCell — the escape hatch has
// to actually reach the prober, or a legitimately-slower model stays
// flagged forever.
func TestProbeModelTool_RebaselineIsCarriedToTheCell(t *testing.T) {
	f := newProbeFixture(t)
	f.announce(t, fleetapi.AnnounceModel{ID: "qwen", State: "ready"})

	text, err := f.s.toolProbeModel("gpu-cell", "qwen", true)
	if err != nil {
		t.Fatalf("probe_model: %v", err)
	}
	if got := decodeProbeResult(t, text); !strings.Contains(got.Note, "baseline") {
		t.Fatalf("the rebaseline is not disclosed to the caller: %q", got.Note)
	}
	resp := f.announce(t, fleetapi.AnnounceModel{ID: "qwen", State: "ready"})
	if len(resp.Commands) != 1 || !resp.Commands[0].Rebaseline {
		t.Fatalf("rebaseline did not reach the queued command: %+v", resp.Commands)
	}
}

// TestProbeModelTool_RefusesTheFrontCell mirrors the scheduler's config
// rule: the front's rendered config is peers-only, so a probe there
// measures a peer THROUGH the front — LAN, proxy hop and the peer's
// queue folded into one unattributable number.
func TestProbeModelTool_RefusesTheFrontCell(t *testing.T) {
	f := newProbeFixture(t)
	if _, err := f.s.toolProbeModel(fleetcfg.FrontCell, "qwen", false); err == nil {
		t.Fatal("probing the front cell succeeded")
	}
}
