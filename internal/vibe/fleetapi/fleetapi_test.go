package fleetapi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeCell is an httptest stand-in for one llama-swap instance: canned
// /running and /v1/models bodies plus a scripted /api/events SSE handler.
type fakeCell struct {
	runningJSON string
	modelsJSON  string
	// eventsDown makes /api/events answer 503 so tests can script
	// cellDown→cellUp transitions.
	eventsDown atomic.Bool
	// proceed gates the scripted frames so a test can attach its own
	// /api/fleet/events subscriber before the fake starts emitting.
	proceed chan struct{}
	srv     *httptest.Server
}

func newFakeCell(t *testing.T) *fakeCell {
	t.Helper()
	f := &fakeCell{
		runningJSON: `{"running":[]}`,
		modelsJSON:  `{"object":"list","data":[]}`,
		proceed:     make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /running", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(f.runningJSON))
	})
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(f.modelsJSON))
	})
	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		if f.eventsDown.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		select {
		case <-f.proceed:
		case <-r.Context().Done():
			return
		}
		writeModelStatus(w, flusher, `[{"id":"qwen","name":"Qwen","state":"starting"}]`)
		time.Sleep(80 * time.Millisecond)
		writeModelStatus(w, flusher, `[{"id":"qwen","name":"Qwen","state":"ready"}]`)
		<-r.Context().Done()
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// writeModelStatus frames a modelStatus message exactly the way llama-swap
// does: the envelope's data field is a JSON *string* containing the payload.
func writeModelStatus(w http.ResponseWriter, flusher http.Flusher, models string) {
	inner, _ := json.Marshal(models)
	fmt.Fprintf(w, "event:message\ndata:{\"type\":\"modelStatus\",\"data\":%s}\n\n", inner)
	flusher.Flush()
}

func testDaemonInfo() DaemonInfo {
	return DaemonInfo{
		ActiveProfile: "chat",
		Services: []ServiceInfo{
			{Name: "classifier", Ready: true, Addr: "127.0.0.1:8000", Pid: 42},
		},
	}
}

// newTestServer wires a Server over the given cell URL with fast backoff and
// a temp history file, mounted on an httptest listener.
func newTestServer(t *testing.T, cellURL string) (*Server, *httptest.Server, string) {
	t.Helper()
	histPath := filepath.Join(t.TempDir(), "start-history.json")
	s := New([]Cell{{Name: "front", URL: cellURL}}, histPath, testDaemonInfo)
	s.baseBackoff = 10 * time.Millisecond
	s.maxBackoff = 50 * time.Millisecond
	mux := http.NewServeMux()
	s.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	t.Cleanup(s.Close)
	return s, ts, histPath
}

func getState(t *testing.T, ts *httptest.Server) stateResponse {
	t.Helper()
	resp, err := http.Get(ts.URL + "/api/fleet/state")
	if err != nil {
		t.Fatalf("GET state: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET state: status %d", resp.StatusCode)
	}
	var st stateResponse
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	return st
}

// subscribeEvents opens /api/fleet/events and feeds decoded frames to the
// returned channel until ctx is canceled.
func subscribeEvents(t *testing.T, ctx context.Context, ts *httptest.Server) <-chan Event {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/fleet/events", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // closed by the reader goroutine below (bodyclose can't see across the goroutine)
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("GET events: status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		resp.Body.Close()
		t.Fatalf("GET events: content-type %q", ct)
	}
	ch := make(chan Event, 64)
	go func() {
		defer resp.Body.Close()
		defer close(ch)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
		var data []string
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				if len(data) > 0 {
					var ev Event
					if err := json.Unmarshal([]byte(strings.Join(data, "\n")), &ev); err == nil {
						ch <- ev
					}
					data = nil
				}
				continue
			}
			if rest, ok := strings.CutPrefix(line, "data:"); ok {
				data = append(data, rest)
			}
		}
	}()
	return ch
}

// waitFor pulls events off ch until match passes, failing after 5s.
func waitFor(t *testing.T, ch <-chan Event, what string, match func(Event) bool) Event {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("event stream closed while waiting for %s", what)
			}
			if match(ev) {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func TestStateMergesRunningAndModels(t *testing.T) {
	fake := newFakeCell(t)
	fake.runningJSON = `{"running":[{"model":"qwen","state":"ready","ttl":300,"name":"Qwen Coder","cmd":"llama-server","proxy":"http://127.0.0.1:9101"}]}`
	fake.modelsJSON = `{"object":"list","data":[{"id":"gemma","object":"model","name":"Gemma 4"},{"id":"qwen","object":"model"}]}`
	_, ts, _ := newTestServer(t, fake.srv.URL)

	st := getState(t, ts)
	if st.GeneratedAt.IsZero() {
		t.Errorf("generated_at is zero")
	}
	if len(st.Cells) != 1 {
		t.Fatalf("cells = %d; want 1", len(st.Cells))
	}
	c := st.Cells[0]
	if c.Name != "front" || c.URL != fake.srv.URL || !c.Reachable {
		t.Errorf("cell = %+v; want front/%s/reachable", c, fake.srv.URL)
	}
	want := []ModelState{
		{ID: "gemma", State: "stopped", Name: "Gemma 4"},
		{ID: "qwen", State: "ready", TTL: 300, Name: "Qwen Coder"},
	}
	if len(c.Models) != len(want) {
		t.Fatalf("models = %+v; want %+v", c.Models, want)
	}
	for i, m := range want {
		if c.Models[i] != m {
			t.Errorf("models[%d] = %+v; want %+v", i, c.Models[i], m)
		}
	}
	if st.Daemon.ActiveProfile != "chat" {
		t.Errorf("daemon.active_profile = %q; want chat", st.Daemon.ActiveProfile)
	}
	if len(st.Daemon.Services) != 1 || st.Daemon.Services[0].Name != "classifier" || !st.Daemon.Services[0].Ready {
		t.Errorf("daemon.services = %+v", st.Daemon.Services)
	}
	if st.StartHistory == nil || len(st.StartHistory) != 0 {
		t.Errorf("start_history = %+v; want empty map", st.StartHistory)
	}
}

func TestStateUnreachableCell(t *testing.T) {
	// Grab a port that nothing listens on: connection refused, not timeout.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadURL := "http://" + ln.Addr().String()
	ln.Close()

	_, ts, _ := newTestServer(t, deadURL)
	st := getState(t, ts)
	if len(st.Cells) != 1 {
		t.Fatalf("cells = %d; want 1", len(st.Cells))
	}
	if st.Cells[0].Reachable {
		t.Errorf("reachable = true; want false")
	}
	if len(st.Cells[0].Models) != 0 {
		t.Errorf("models = %+v; want empty", st.Cells[0].Models)
	}
}

func TestEventsWrappingAndStartDuration(t *testing.T) {
	fake := newFakeCell(t)
	s, ts, histPath := newTestServer(t, fake.srv.URL)
	s.Start()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := subscribeEvents(t, ctx, ts)

	waitFor(t, ch, "fleet.cellUp", func(ev Event) bool {
		return ev.Cell == "front" && ev.Type == "fleet.cellUp"
	})
	// Release the scripted starting→ready sequence only after the
	// subscriber is attached so both wrapped frames are observed.
	close(fake.proceed)

	first := waitFor(t, ch, "starting modelStatus", func(ev Event) bool {
		return ev.Type == "modelStatus" && strings.Contains(string(ev.Data), "starting")
	})
	if first.Cell != "front" {
		t.Errorf("modelStatus cell = %q; want front", first.Cell)
	}
	// Upstream's data field is a JSON string; the wrapper must forward it
	// verbatim, not re-encode it as an array.
	var inner string
	if err := json.Unmarshal(first.Data, &inner); err != nil {
		t.Errorf("modelStatus data is not a JSON string (got %s): %v", first.Data, err)
	} else if !strings.Contains(inner, `"id":"qwen"`) {
		t.Errorf("modelStatus inner payload = %q; want it to mention qwen", inner)
	}
	waitFor(t, ch, "ready modelStatus", func(ev Event) bool {
		return ev.Type == "modelStatus" && strings.Contains(string(ev.Data), "ready")
	})

	// The starting→ready transition must land in start_history with a
	// duration at least the scripted 80ms gap.
	deadline := time.Now().Add(5 * time.Second)
	var stats StartStats
	for time.Now().Before(deadline) {
		st := getState(t, ts)
		if got, ok := st.StartHistory["qwen"]; ok {
			stats = got
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if stats.Count != 1 {
		t.Fatalf("start_history[qwen] = %+v; want count 1", stats)
	}
	if stats.LastS < 0.05 || stats.LastS > 5 {
		t.Errorf("last_s = %v; want ~0.08", stats.LastS)
	}
	if stats.P50S != stats.LastS || stats.MaxS != stats.LastS {
		t.Errorf("single-sample stats disagree: %+v", stats)
	}

	// Persistence: the file exists and a fresh instance loads it.
	if _, err := os.Stat(histPath); err != nil {
		t.Fatalf("history file: %v", err)
	}
	reloaded := loadHistory(histPath)
	if got := reloaded.Stats()["qwen"]; got != stats {
		t.Errorf("reloaded stats = %+v; want %+v", got, stats)
	}
}

func TestCellDownUpTransitions(t *testing.T) {
	fake := newFakeCell(t)
	fake.eventsDown.Store(true)
	s, ts, _ := newTestServer(t, fake.srv.URL)
	s.Start()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := subscribeEvents(t, ctx, ts)

	waitFor(t, ch, "fleet.cellDown", func(ev Event) bool {
		return ev.Cell == "front" && ev.Type == "fleet.cellDown"
	})

	fake.eventsDown.Store(false)
	waitFor(t, ch, "fleet.cellUp", func(ev Event) bool {
		return ev.Cell == "front" && ev.Type == "fleet.cellUp"
	})
}
