package fleetapi

// C1 acceptance-gate coverage (docs/design/fleet-control-plan/
// c1-observe-intent.md gate 1): multi-cell registry, intent CRUD + the
// derived display states, host-probe vs cell-probe disagreement, and
// last-seen persistence across a fleetd restart.

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newFleetdServer wires a Server the way the daemon's fleet_registry role
// does: cells with class/host_probe plus intent and last-seen files.
func newFleetdServer(t *testing.T, cells []Cell) (*Server, *httptest.Server, Options) {
	t.Helper()
	dir := t.TempDir()
	opts := Options{
		IntentPath:   filepath.Join(dir, "intent.json"),
		LastSeenPath: filepath.Join(dir, "last-seen.json"),
		LeasePath:    filepath.Join(dir, "leases.json"),
	}
	s := New(cells, filepath.Join(dir, "start-history.json"), testDaemonInfo, opts)
	s.baseBackoff = 10 * time.Millisecond
	s.maxBackoff = 50 * time.Millisecond
	mux := http.NewServeMux()
	s.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	t.Cleanup(s.Close)
	return s, ts, opts
}

func TestMultiCellRegistry(t *testing.T) {
	cellA := newFakeCell(t)
	cellA.modelsJSON = `{"object":"list","data":[{"id":"qwen3.6-27b","object":"model"}]}`
	cellB := newFakeCell(t)
	cellB.modelsJSON = `{"object":"list","data":[{"id":"bge-embed","object":"model"}]}`
	cellB.runningJSON = `{"running":[{"model":"bge-embed","state":"ready","ttl":0}]}`

	_, ts, _ := newFleetdServer(t, []Cell{
		{Name: "front", URL: cellA.srv.URL, Class: "always_on"},
		{Name: "utility", URL: cellB.srv.URL, Class: "always_on"},
	})

	st := getState(t, ts)
	if len(st.Cells) != 2 {
		t.Fatalf("got %d cells, want 2", len(st.Cells))
	}
	byName := map[string]CellSnapshot{}
	for _, c := range st.Cells {
		byName[c.Name] = c
	}
	front, ok := byName["front"]
	if !ok || !front.Reachable || front.Class != "always_on" || front.Display != DisplayServing {
		t.Errorf("front cell wrong: %+v", front)
	}
	util := byName["utility"]
	if len(util.Models) != 1 || util.Models[0].ID != "bge-embed" || util.Models[0].State != "ready" {
		t.Errorf("utility models wrong: %+v", util.Models)
	}
}

func TestDisplayStateTable(t *testing.T) {
	up, down := true, false
	drainedIntent := &Intent{State: "drained", Reason: "gaming", Since: time.Now()}
	cases := []struct {
		name   string
		host   *bool
		cellUp bool
		intent *Intent
		want   string
	}{
		{"host up cell up", &up, true, nil, DisplayServing},
		{"host down cell up (proof of life wins)", &down, true, nil, DisplayServing},
		{"host up cell down drained", &up, false, drainedIntent, DisplayDrained},
		{"host up cell down no intent", &up, false, nil, DisplayDrainedQ},
		{"host down drained (was drained first)", &down, false, drainedIntent, DisplayOff},
		{"host down no intent", &down, false, nil, DisplayOffAway},
		{"no probe cell down no intent", nil, false, nil, DisplayOffAwayQ},
		{"cell up but drained intent (resume forgot)", &up, true, drainedIntent, DisplayInconsistent},
		{"no probe drained", nil, false, drainedIntent, DisplayDrained},
	}
	for _, tc := range cases {
		if got := displayState(tc.host, tc.cellUp, tc.intent); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}

// hostProbeListener returns an addr with a live TCP listener (host up),
// closed by test cleanup.
func hostProbeListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	// Accept and drop: the probe only needs the handshake to complete.
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return ln.Addr().String()
}

// deadAddr returns an address nothing listens on.
func deadAddr(t *testing.T) string {
	t.Helper()
	// A RESERVED address, not a recycled ephemeral one. The old shape
	// (listen on :0, read the port, close) hands the port straight back
	// to the kernel, and a test binary that stands up dozens of
	// httptest servers can be given that exact port a few microseconds
	// later — at which point the "dead" cell answers 404 and the test
	// fails for a reason that has nothing to do with what it asserts.
	// Seen once in ~25 package runs of `go test -race ./...`, which is
	// precisely the failure rate that makes a green CI meaningless.
	// Port 1 is privileged: nothing in this binary can bind it, and a
	// loopback connect gets an immediate ECONNREFUSED. It is already the
	// idiom these tests use for an unreachable front.
	_ = t
	return "127.0.0.1:1"
}

func TestHostProbeVsCellProbeDisagreement(t *testing.T) {
	// Cell answers HTTP but the host probe port is dead: SERVING — a
	// responding cell is proof of life (firewalled SSH port loses).
	liveCell := newFakeCell(t)
	_, ts, _ := newFleetdServer(t, []Cell{
		{Name: "gpu-cell", URL: liveCell.srv.URL, Class: "opportunistic", HostProbe: deadAddr(t)},
	})
	st := getState(t, ts)
	c := st.Cells[0]
	if c.HostReachable == nil || *c.HostReachable {
		t.Errorf("host_reachable = %v, want false", c.HostReachable)
	}
	if c.Display != DisplayServing {
		t.Errorf("display = %s, want SERVING (cell proof of life beats host probe)", c.Display)
	}
}

func TestHostUpCellDownShowsDrainedQuestion(t *testing.T) {
	// Host probe answers, cell HTTP is dead, no intent: DRAINED? —
	// deliberate stop or crash loop; flagged, never acted on.
	deadCell := deadAddr(t)
	_, ts, _ := newFleetdServer(t, []Cell{
		{Name: "gpu-cell", URL: "http://" + deadCell, Class: "opportunistic", HostProbe: hostProbeListener(t)},
	})
	st := getState(t, ts)
	c := st.Cells[0]
	if c.Reachable {
		t.Fatal("cell unexpectedly reachable")
	}
	if c.HostReachable == nil || !*c.HostReachable {
		t.Errorf("host_reachable = %v, want true", c.HostReachable)
	}
	if c.Display != DisplayDrainedQ {
		t.Errorf("display = %s, want DRAINED?", c.Display)
	}
}

func postIntent(t *testing.T, ts *httptest.Server, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(ts.URL+"/api/fleet/intent", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST intent: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestIntentCRUDAndDisplay(t *testing.T) {
	deadCell := deadAddr(t)
	_, ts, opts := newFleetdServer(t, []Cell{
		{Name: "gpu-cell", URL: "http://" + deadCell, Class: "opportunistic", HostProbe: hostProbeListener(t)},
	})

	// Unknown cell → 400, and nothing is recorded.
	code, _ := postIntent(t, ts, `{"cell":"typo-cell","state":"drained"}`)
	if code != http.StatusBadRequest {
		t.Errorf("unknown cell: got %d, want 400", code)
	}
	// Unknown state → 400.
	code, _ = postIntent(t, ts, `{"cell":"gpu-cell","state":"napping"}`)
	if code != http.StatusBadRequest {
		t.Errorf("bad state: got %d, want 400", code)
	}

	// Drain: state shows intent + DRAINED (host up, cell down).
	code, out := postIntent(t, ts, `{"cell":"gpu-cell","state":"drained","reason":"gaming","eta":"23:00"}`)
	if code != http.StatusOK {
		t.Fatalf("drain: got %d", code)
	}
	if out["state"] != "drained" {
		t.Errorf("response state = %v", out["state"])
	}
	st := getState(t, ts)
	c := st.Cells[0]
	if c.Intent == nil || c.Intent.Reason != "gaming" || c.Intent.ETA != "23:00" {
		t.Errorf("intent not in state: %+v", c.Intent)
	}
	if c.Display != DisplayDrained {
		t.Errorf("display = %s, want DRAINED", c.Display)
	}

	// The file on disk round-trips (fleetd restart path).
	data, err := os.ReadFile(opts.IntentPath)
	if err != nil {
		t.Fatalf("read intent file: %v", err)
	}
	var onDisk map[string]Intent
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatalf("parse intent file: %v", err)
	}
	if onDisk["gpu-cell"].Reason != "gaming" {
		t.Errorf("intent file = %v", onDisk)
	}

	// Serving clears: intent gone, display back to DRAINED? (host up,
	// cell down, nothing declared).
	code, out = postIntent(t, ts, `{"cell":"gpu-cell","state":"serving"}`)
	if code != http.StatusOK || out["state"] != "serving" {
		t.Errorf("serving: got %d %v", code, out)
	}
	st = getState(t, ts)
	if st.Cells[0].Intent != nil {
		t.Errorf("intent not cleared: %+v", st.Cells[0].Intent)
	}
	if st.Cells[0].Display != DisplayDrainedQ {
		t.Errorf("display = %s, want DRAINED? after intent cleared", st.Cells[0].Display)
	}
}

func TestIntentSurvivesServerRecreate(t *testing.T) {
	deadCell := deadAddr(t)
	cells := []Cell{{Name: "gpu-cell", URL: "http://" + deadCell, Class: "opportunistic"}}
	_, ts, opts := newFleetdServer(t, cells)
	if code, _ := postIntent(t, ts, `{"cell":"gpu-cell","state":"drained","reason":"gaming"}`); code != http.StatusOK {
		t.Fatal("drain failed")
	}

	// Recreate the server over the same state files, as a fleetd restart
	// would, and confirm intent and display are still there.
	s2 := New(cells, filepath.Join(t.TempDir(), "h.json"), testDaemonInfo, opts)
	defer s2.Close()
	mux := http.NewServeMux()
	s2.Register(mux)
	ts2 := httptest.NewServer(mux)
	defer ts2.Close()
	st := getState(t, ts2)
	if st.Cells[0].Intent == nil || st.Cells[0].Intent.Reason != "gaming" {
		t.Errorf("intent lost across recreate: %+v", st.Cells[0].Intent)
	}
	if st.Cells[0].Display != DisplayDrained {
		t.Errorf("display = %s, want DRAINED across recreate", st.Cells[0].Display)
	}
}

func TestLastSeenPersistsAcrossRecreate(t *testing.T) {
	liveCell := newFakeCell(t)
	cells := []Cell{{Name: "laptop", URL: liveCell.srv.URL, Class: "roaming"}}
	_, ts, opts := newFleetdServer(t, cells)

	st := getState(t, ts)
	if st.Cells[0].LastSeen == nil {
		t.Fatal("last_seen not recorded for a sighted cell")
	}
	sighted := *st.Cells[0].LastSeen

	// Cell vanishes and fleetd restarts over the same state dir: the
	// sighting must survive both.
	liveCell.srv.Close()
	s2 := New(cells, filepath.Join(t.TempDir(), "h.json"), testDaemonInfo, opts)
	defer s2.Close()
	mux := http.NewServeMux()
	s2.Register(mux)
	ts2 := httptest.NewServer(mux)
	defer ts2.Close()
	st = getState(t, ts2)
	c := st.Cells[0]
	if c.Reachable {
		t.Fatal("cell unexpectedly reachable after close")
	}
	if c.LastSeen == nil || !c.LastSeen.Equal(sighted) {
		t.Errorf("last_seen = %v, want persisted %v", c.LastSeen, sighted)
	}
	if c.Display != DisplayOffAwayQ {
		t.Errorf("display = %s, want OFF/AWAY? (no host_probe)", c.Display)
	}
}

func TestIntentConcurrentPostsSerialize(t *testing.T) {
	// Regression for the map race the adversarial review caught: two
	// overlapping POSTs must serialize (no fatal concurrent map
	// iteration/write), and the final state must be one of the two
	// writers', consistent between memory and file.
	deadCell := deadAddr(t)
	_, ts, opts := newFleetdServer(t, []Cell{
		{Name: "gpu-cell", URL: "http://" + deadCell, Class: "opportunistic"},
	})

	var wg sync.WaitGroup
	codes := make([]int, 40)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			state := "drained"
			if i%2 == 0 {
				state = "serving"
			}
			body := fmt.Sprintf(`{"cell":"gpu-cell","state":%q,"reason":"r%d"}`, state, i)
			code, _ := postIntent(t, ts, body)
			codes[i] = code
		}(i)
	}
	wg.Wait()
	for i, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("post %d: HTTP %d", i, code)
		}
	}

	// Memory and file agree on the winner.
	st := getState(t, ts)
	data, err := os.ReadFile(opts.IntentPath)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk map[string]Intent
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatal(err)
	}
	memDrained := st.Cells[0].Intent != nil
	_, fileDrained := onDisk["gpu-cell"]
	if memDrained != fileDrained {
		t.Errorf("memory/file disagree: memory drained=%v file drained=%v", memDrained, fileDrained)
	}
}

func TestIntentRouteAbsentWithoutStore(t *testing.T) {
	// The single-box default (Options{}) must not grow an intent surface:
	// POST /api/fleet/intent is unregistered.
	cell := newFakeCell(t)
	_, ts, _ := newTestServer(t, cell.srv.URL)
	code, _ := postIntent(t, ts, `{"cell":"front","state":"drained"}`)
	if code != http.StatusNotFound {
		t.Errorf("got %d, want 404 when intent store disabled", code)
	}
}

func TestClassAndProbeFieldsAbsentByDefault(t *testing.T) {
	// Regression guard for the single-box shape: no class, no
	// host_reachable key, display present.
	cell := newFakeCell(t)
	_, ts, _ := newTestServer(t, cell.srv.URL)
	resp, err := http.Get(ts.URL + "/api/fleet/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	first := raw["cells"].([]any)[0].(map[string]any)
	if _, ok := first["host_reachable"]; ok {
		t.Error("host_reachable present without a configured probe")
	}
	if _, ok := first["class"]; ok {
		t.Error("class present on the default cell")
	}
	if first["display"] != DisplayServing {
		t.Errorf("display = %v, want SERVING", first["display"])
	}
}

func TestOllamaShapeCatalogCell(t *testing.T) {
	// A vibe-daemon-proxy cell answers /v1/models with BOTH the OpenAI
	// data[] and the Ollama passthrough models[] shape (and /running
	// 404s): the model must show as ready — llama.cpp only advertises
	// what it has loaded, and the data[] default of "stopped" is a lie
	// for a cell with no /running to merge against.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"qwen3.6-35b-a3b","object":"model"}],"models":[{"name":"qwen3.6-35b-a3b","model":"qwen3.6-35b-a3b","capabilities":["completion"]}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	_, ts, _ := newFleetdServer(t, []Cell{
		{Name: "laptop", URL: srv.URL, Class: "roaming"},
	})
	st := getState(t, ts)
	c := st.Cells[0]
	if !c.Reachable {
		t.Fatal("daemon-proxy cell unreachable")
	}
	if len(c.Models) != 1 || c.Models[0].ID != "qwen3.6-35b-a3b" || c.Models[0].State != "ready" {
		t.Errorf("models = %+v, want qwen3.6-35b-a3b ready", c.Models)
	}
	if c.Display != DisplayServing {
		t.Errorf("display = %s, want SERVING", c.Display)
	}
}

func TestStateUnreachableCellDisplay(t *testing.T) {
	// An unreachable cell with no probe and no intent is OFF/AWAY? — the
	// honest "unknowable" form, distinct from a probed host-down.
	_, ts, _ := newFleetdServer(t, []Cell{
		{Name: "laptop", URL: fmt.Sprintf("http://%s", deadAddr(t)), Class: "roaming"},
	})
	st := getState(t, ts)
	if st.Cells[0].Display != DisplayOffAwayQ {
		t.Errorf("display = %s, want OFF/AWAY?", st.Cells[0].Display)
	}
}
