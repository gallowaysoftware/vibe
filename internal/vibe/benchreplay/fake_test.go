package benchreplay

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/swaptest"
)

// A purpose-built llama-swap for the replay half.
//
// internal/swaptest's Cell is the shared double and it models the two
// endpoints the HARVEST needs exactly — /api/metrics/activity with
// has_capture, and GET /api/captures/{id} with upstream's own status codes
// — and TestHarvestRunsAgainstTheSharedDouble drives the production fetch
// path against it for precisely that reason.
//
// What it cannot do is answer a chat completion differently on the third
// request than on the second, and every structural metric in this phase is
// a question about WHICH answer arrived: a declared tool versus an
// invented one, arguments that parse versus arguments that do not, a
// finish reason of length versus stop, a timings block versus none. So the
// replay half gets a fake whose responses are a script. Growing the shared
// double a scripting hook for one phase would make every other consumer
// carry it.

// scripted is one answer the fake gives.
type scripted struct {
	status int
	body   string
}

type fakeCell struct {
	url string

	mu        sync.Mutex
	resident  map[string]bool
	rows      []fakeRow
	captures  map[int64]swaptest.Capture
	reads     map[int64]int
	chatCalls map[string]int
	chatBody  map[string][][]byte
	pages     int

	// chat answers request n (0-based, per model).
	chat func(model string, n int) scripted
}

type fakeRow struct {
	ID         int64  `json:"id"`
	Model      string `json:"model"`
	ReqPath    string `json:"req_path"`
	Status     int    `json:"resp_status_code"`
	HasCapture bool   `json:"has_capture"`
}

func newFakeCell(t *testing.T) *fakeCell {
	t.Helper()
	f := &fakeCell{
		resident:  map[string]bool{},
		captures:  map[int64]swaptest.Capture{},
		reads:     map[int64]int{},
		chatCalls: map[string]int{},
		chatBody:  map[string][][]byte{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /running", f.handleRunning)
	mux.HandleFunc("GET /api/metrics/activity", f.handleActivity)
	mux.HandleFunc("GET /api/captures/{id}", f.handleCapture)
	mux.HandleFunc("POST /v1/chat/completions", f.handleChat)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

func (f *fakeCell) handleRunning(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	type entry struct {
		Model string `json:"model"`
		State string `json:"state"`
	}
	out := []entry{}
	for m, ok := range f.resident {
		if ok {
			out = append(out, entry{Model: m, State: "ready"})
		}
	}
	f.mu.Unlock()
	writeJSON(w, map[string]any{"running": out})
}

func (f *fakeCell) handleActivity(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	f.mu.Lock()
	f.pages++
	rows := append([]fakeRow(nil), f.rows...)
	f.mu.Unlock()
	if page > 1 {
		rows = nil
	}
	writeJSON(w, map[string]any{"data": rows, "page": page, "limit": 999, "total": len(rows), "total_pages": 1})
}

func (f *fakeCell) handleCapture(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.reads[id]++
	c, ok := f.captures[id]
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"capture not found"}`))
		return
	}
	writeJSON(w, c)
}

func (f *fakeCell) handleChat(w http.ResponseWriter, r *http.Request) {
	var probe struct {
		Model string `json:"model"`
	}
	raw := make([]byte, 0, 1024)
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		raw = append(raw, buf[:n]...)
		if err != nil {
			break
		}
	}
	_ = json.Unmarshal(raw, &probe)
	f.mu.Lock()
	n := f.chatCalls[probe.Model]
	f.chatCalls[probe.Model] = n + 1
	f.chatBody[probe.Model] = append(f.chatBody[probe.Model], append([]byte(nil), raw...))
	fn := f.chat
	f.mu.Unlock()
	res := scripted{status: 200, body: proseResponse("ok", "stop", 8, 40)}
	if fn != nil {
		res = fn(probe.Model, n)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(res.status)
	_, _ = w.Write([]byte(res.body))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// ── fixtures the tests build ────────────────────────────────────────────

func (f *fakeCell) warm(models ...string) {
	f.mu.Lock()
	for _, m := range models {
		f.resident[m] = true
	}
	f.mu.Unlock()
}

// row adds an activity row AND the capture behind it. Both, because a row
// without a capture is the eviction case and it must be spelled out
// deliberately (see rowWithoutCapture).
func (f *fakeCell) row(id int64, model, reqPath string, status int, reqBody, respBody string) {
	f.mu.Lock()
	f.rows = append(f.rows, fakeRow{ID: id, Model: model, ReqPath: reqPath, Status: status, HasCapture: true})
	f.captures[id] = swaptest.Capture{
		ID: int(id), ReqPath: reqPath,
		ReqBody:  []byte(reqBody),
		RespBody: []byte(respBody),
	}
	f.mu.Unlock()
}

// rowWithoutCapture advertises has_capture and serves a 404 — the FIFO
// buffer having dropped it between the activity read and the fetch.
func (f *fakeCell) rowWithoutCapture(id int64, model, reqPath string) {
	f.mu.Lock()
	f.rows = append(f.rows, fakeRow{ID: id, Model: model, ReqPath: reqPath, Status: 200, HasCapture: true})
	f.mu.Unlock()
}

func (f *fakeCell) captureReads(id int64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads[id]
}

func (f *fakeCell) sentBodies(model string) [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.chatBody[model]...)
}

// ── synthetic bodies ────────────────────────────────────────────────────
//
// Every byte below was typed here. Nothing in this repo may hold a real
// capture, and a test fixture is the exact place that rule gets broken.

// toolRequest is a chat request declaring one tool with one required
// argument. `marker` stands in for the private text a real capture
// carries: the prompt, the system prompt and the tool description.
func toolRequest(marker string, maxTokens int) string {
	return fmt.Sprintf(`{
	  "model": "incumbent",
	  "temperature": 0.7,
	  "max_tokens": %d,
	  "stream": true,
	  "stream_options": {"include_usage": true},
	  "messages": [
	    {"role": "system", "content": %q},
	    {"role": "user", "content": %q}
	  ],
	  "tools": [
	    {"type": "function", "function": {
	      "name": "read_file",
	      "description": %q,
	      "parameters": {"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"]}
	    }}
	  ]
	}`, maxTokens, marker, marker, marker)
}

// plainRequest declares no tools.
func plainRequest(marker string) string {
	return fmt.Sprintf(`{"model":"incumbent","temperature":0.7,"max_tokens":128,
	  "messages":[{"role":"user","content":%q}]}`, marker)
}

func schemaRequest(marker string) string {
	return fmt.Sprintf(`{"model":"incumbent","max_tokens":64,
	  "response_format":{"type":"json_object"},
	  "messages":[{"role":"user","content":%q}]}`, marker)
}

// toolResponse is a non-streaming completion that calls a tool.
func toolResponse(name, args string, tokens int, tokS float64) string {
	return fmt.Sprintf(`{"choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,
	  "tool_calls":[{"id":"call_1","type":"function","function":{"name":%q,"arguments":%q}}]}}],
	  "usage":{"completion_tokens":%d},
	  "timings":{"predicted_n":%d,"predicted_ms":1000,"predicted_per_second":%g}}`,
		name, args, tokens, tokens, tokS)
}

func proseResponse(text, finish string, tokens int, tokS float64) string {
	return fmt.Sprintf(`{"choices":[{"index":0,"finish_reason":%q,"message":{"role":"assistant","content":%q}}],
	  "usage":{"completion_tokens":%d},
	  "timings":{"predicted_n":%d,"predicted_ms":1000,"predicted_per_second":%g}}`,
		finish, text, tokens, tokens, tokS)
}

// proseResponseNoTimings is what an engine without llama.cpp's timings
// block answers — mlx, or a future upstream. It is what forces the E2E
// fallback metric, and the metric is why a ratio gets refused.
func proseResponseNoTimings(text, finish string, tokens int) string {
	return fmt.Sprintf(`{"choices":[{"index":0,"finish_reason":%q,"message":{"role":"assistant","content":%q}}],
	  "usage":{"completion_tokens":%d}}`, finish, text, tokens)
}

// streamedToolResponse is a RECORDED response in llama-swap's stored form:
// the raw buffered SSE frames, with the tool-call arguments split across
// chunks the way llama.cpp emits them.
func streamedToolResponse(name, argsA, argsB, finish string, tokens int) string {
	return "data: " + fmt.Sprintf(`{"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"function":{"name":%q,"arguments":%q}}]}}]}`, name, argsA) + "\n\n" +
		"data: " + fmt.Sprintf(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":%q}}]}}]}`, argsB) + "\n\n" +
		"data: " + fmt.Sprintf(`{"choices":[{"index":0,"delta":{},"finish_reason":%q}],"usage":{"completion_tokens":%d}}`, finish, tokens) + "\n\n" +
		"data: [DONE]\n\n"
}

func streamedProseResponse(text, finish string, tokens int) string {
	return "data: " + fmt.Sprintf(`{"choices":[{"index":0,"delta":{"role":"assistant","content":%q}}]}`, text) + "\n\n" +
		"data: " + fmt.Sprintf(`{"choices":[{"index":0,"delta":{},"finish_reason":%q}],"usage":{"completion_tokens":%d}}`, finish, tokens) + "\n\n" +
		"data: [DONE]\n\n"
}
