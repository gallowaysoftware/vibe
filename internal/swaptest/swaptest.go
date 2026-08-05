package swaptest

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TB is the slice of *testing.T this package uses. Declared locally so the
// package never imports "testing" — see the package doc.
type TB interface {
	Helper()
	Cleanup(func())
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
}

// Wire is a llama-swap protocol version. The value that matters is the
// inflight frame shape; everything else has been additive across the range
// this package covers.
type Wire int

const (
	// WireV239 sends the full in-flight request list on every edge. It is
	// what the fleet runs today (v239, dd81801) and what fixtures/v239 was
	// recorded from.
	WireV239 Wire = iota
	// WireV247 sends operation-tagged deltas: snapshot at connect, upsert
	// on start, remove on completion, with `requests` omitempty and
	// therefore absent on both deltas.
	WireV247
)

func (w Wire) String() string {
	switch w {
	case WireV239:
		return "v239"
	case WireV247:
		return "v247"
	default:
		return "wire" + strconv.Itoa(int(w))
	}
}

// Default is the wire NewCell speaks when a test does not choose one. Set
// it in a TestMain to run an entire package against another llama-swap
// contract.
var Default = WireV239

// Model is one entry in the double's catalog.
type Model struct {
	ID      string
	Aliases []string
	// State is llama-swap's own vocabulary: "ready", "starting",
	// "stopped". Only "ready" appears in /running.
	State    string
	TTL      int
	Unlisted bool
	PeerID   string
}

// Tokens mirrors an activity row's token block. Every field is signed
// because llama-swap reports -1 for "not measured" on the three it can
// fail to observe, and clamping that to 0 at the fixture boundary would
// hide the exact case C7a's arithmetic exists to handle.
type Tokens struct {
	Cache           int64
	Draft           int64
	DraftAcc        int64
	Input           int64
	Output          int64
	PromptPerSecond float64
	TokensPerSecond float64
}

// Option configures a Cell at construction.
type Option func(*config)

type config struct {
	wire      Wire
	models    []Model
	ringCap   int
	firstID   int64
	chatDelay time.Duration
}

// WithChatDelay holds POST /v1/chat/completions open for d before it
// answers, so a test can observe the cell while work is genuinely in
// flight. Real generation takes hundreds of milliseconds to minutes; a
// double that answers instantly makes the in-flight window unobservable.
func WithChatDelay(d time.Duration) Option { return func(c *config) { c.chatDelay = d } }

// WithWire selects the llama-swap protocol version.
func WithWire(w Wire) Option { return func(c *config) { c.wire = w } }

// WithModels replaces the default catalog.
func WithModels(ms ...Model) Option {
	return func(c *config) { c.models = append([]Model(nil), ms...) }
}

// WithRingCap sizes the activity store. llama-swap's DEFAULT store is an
// in-memory ring capped at 1000 rows while ids keep climbing; a persistent
// store needs a `store:` block with a path in its config. Tests that walk
// the activity log want the ring, because the ring is what a cell without
// that block actually has.
func WithRingCap(n int) Option { return func(c *config) { c.ringCap = n } }

// WithFirstID seeds the activity id counter. Real cells are found at
// whatever id they have climbed to (93371 on the box these fixtures came
// from), so a test that assumes ids start near zero is testing a fleet
// that does not exist.
func WithFirstID(id int64) Option { return func(c *config) { c.firstID = id } }

// DefaultRingCap is llama-swap's in-memory activity ring size.
const DefaultRingCap = 1000

// Cell is one fake llama-swap instance listening on loopback.
type Cell struct {
	wire Wire
	url  string

	mu        sync.Mutex
	models    []Model
	rows      []Row
	live      []inflightEntry
	ringCap   int
	nextID    int64
	chatDelay time.Duration
	subs      map[*stream]struct{}

	// faults
	dropAfter     int
	loseRemove    bool
	evictAfterReq int
	evictRows     int
	pageReads     int

	// counters, for tests that assert on the double's own behaviour
	connects int
	streams  int
}

// NewCell starts a fake llama-swap and registers its shutdown with t.
func NewCell(t TB, opts ...Option) *Cell {
	t.Helper()
	cfg := config{
		wire:    Default,
		ringCap: DefaultRingCap,
		firstID: 1,
		models: []Model{
			{ID: "chat-model", State: "ready", TTL: 1800},
			{ID: "embed-model", State: "stopped"},
		},
	}
	for _, o := range opts {
		o(&cfg)
	}
	c := &Cell{
		wire:      cfg.wire,
		models:    cfg.models,
		ringCap:   cfg.ringCap,
		nextID:    cfg.firstID,
		chatDelay: cfg.chatDelay,
		subs:      map[*stream]struct{}{},
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("swaptest: listen: %v", err)
	}
	c.url = "http://" + ln.Addr().String()

	mux := http.NewServeMux()
	mux.HandleFunc("/running", c.handleRunning)
	mux.HandleFunc("/v1/models", c.handleModels)
	mux.HandleFunc("/api/events", c.handleEvents)
	mux.HandleFunc("/api/metrics/activity", c.handleActivity)
	mux.HandleFunc("/v1/chat/completions", c.handleChat)
	mux.HandleFunc("/v1/embeddings", c.handleEmbed)
	mux.HandleFunc("/unload", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		c.closeStreams()
		_ = srv.Close()
	})
	return c
}

// URL is the cell's base URL, in the form fleetcfg and fleetapi expect.
func (c *Cell) URL() string { return c.url }

// Wire reports which protocol version this cell speaks.
func (c *Cell) Wire() Wire { return c.wire }

// ─── catalog ────────────────────────────────────────────────────────────────

func (c *Cell) handleRunning(w http.ResponseWriter, _ *http.Request) {
	c.mu.Lock()
	type entry struct {
		Model string `json:"model"`
		State string `json:"state"`
		Cmd   string `json:"cmd"`
		Proxy string `json:"proxy"`
		TTL   int    `json:"ttl"`
		// The real /running carries these two and they are always empty:
		// a model's display name lives on modelStatus, not here. A fake
		// that answers a name from /running lets a test assert a value
		// production can never see.
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	out := []entry{}
	for _, m := range c.models {
		if m.State != "ready" && m.State != "starting" {
			continue
		}
		out = append(out, entry{
			Model: m.ID,
			State: m.State,
			Cmd:   "/usr/bin/llama-server\n--model /models/" + m.ID + ".gguf\n--host 127.0.0.1\n--port 5804",
			Proxy: "http://localhost:5804",
			TTL:   m.TTL,
		})
	}
	c.mu.Unlock()
	writeJSON(w, map[string]any{"running": out})
}

func (c *Cell) handleModels(w http.ResponseWriter, _ *http.Request) {
	c.mu.Lock()
	type entry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	out := []entry{}
	for _, m := range c.models {
		out = append(out, entry{ID: m.ID, Object: "model", Created: 0, OwnedBy: "llama-swap"})
		for _, a := range m.Aliases {
			out = append(out, entry{ID: a, Object: "model", Created: 0, OwnedBy: "llama-swap"})
		}
	}
	c.mu.Unlock()
	writeJSON(w, map[string]any{"object": "list", "data": out})
}

// SetModelState moves a model between llama-swap's states and publishes the
// modelStatus frame that move produces.
func (c *Cell) SetModelState(id, state string) {
	c.mu.Lock()
	found := false
	for i := range c.models {
		if c.models[i].ID == id {
			c.models[i].State = state
			found = true
		}
	}
	if !found {
		c.models = append(c.models, Model{ID: id, State: state})
	}
	frame := c.modelStatusFrameLocked()
	c.mu.Unlock()
	c.broadcast(frame)
}

// ─── the request lifecycle ──────────────────────────────────────────────────

// Request drives one complete request through the double: the in-flight
// start edge, llama-swap's `activity` id frame, the completion edge, and
// the activity row. It returns the row id.
//
// dur is recorded as duration_ms; nothing sleeps.
func (c *Cell) Request(model, reqPath string, tok Tokens, dur time.Duration) int64 {
	return c.request(model, reqPath, tok, dur, 200, "", contentTypeFor(reqPath))
}

// Fail drives a request that the upstream refused. A 4xx still produces an
// activity row with error_msg set and tokens largely zero — observed on the
// real v239 (a 400 with duration_ms 48) — and a fake that drops the row
// entirely hides C7a's err_req counter from every test.
func (c *Cell) Fail(model, reqPath string, status int, msg string) int64 {
	return c.request(model, reqPath, Tokens{}, 0, status, msg, "application/json")
}

// known reports whether the model (or one of its aliases) is in the
// catalog.
func (c *Cell) known(model string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.models {
		if m.ID == model {
			return true
		}
		for _, a := range m.Aliases {
			if a == model {
				return true
			}
		}
	}
	return false
}

func (c *Cell) request(model, reqPath string, tok Tokens, dur time.Duration, status int, errMsg, ctype string) int64 {
	c.mu.Lock()
	id := c.nextID
	c.nextID++
	entry := inflightEntry{
		ID:        strconv.FormatInt(id, 10),
		Timestamp: time.Now().Format(time.RFC3339Nano),
		Model:     model,
		ReqPath:   reqPath,
		Method:    http.MethodPost,
	}
	startFrames := c.inflightStartFramesLocked(entry)
	endFrames := c.inflightEndFramesLocked(entry)
	if c.loseRemove {
		c.loseRemove = false
		endFrames = nil
	}
	row := Row{
		ID:              id,
		Timestamp:       time.Now().Format(time.RFC3339),
		Model:           model,
		ReqPath:         reqPath,
		RespContentType: ctype,
		RespStatusCode:  status,
		DurationMs:      dur.Milliseconds(),
		ErrorMsg:        errMsg,
		Tokens: RowTokens{
			CacheTokens:     tok.Cache,
			DraftTokens:     tok.Draft,
			DraftAccTokens:  tok.DraftAcc,
			InputTokens:     tok.Input,
			OutputTokens:    tok.Output,
			PromptPerSecond: tok.PromptPerSecond,
			TokensPerSecond: tok.TokensPerSecond,
		},
	}
	c.mu.Unlock()

	for _, f := range startFrames {
		c.broadcast(f)
	}
	c.broadcast(activityFrame(id))
	for _, f := range endFrames {
		c.broadcast(f)
	}

	c.mu.Lock()
	c.rows = append(c.rows, row)
	if c.ringCap > 0 && len(c.rows) > c.ringCap {
		c.rows = c.rows[len(c.rows)-c.ringCap:]
	}
	c.mu.Unlock()
	return id
}

// StartRequest opens a request and returns the closer, for tests that need
// to observe the cell while work is genuinely in flight.
func (c *Cell) StartRequest(model, reqPath string) (id int64, done func(Tokens, time.Duration)) {
	c.mu.Lock()
	id = c.nextID
	c.nextID++
	entry := inflightEntry{
		ID:        strconv.FormatInt(id, 10),
		Timestamp: time.Now().Format(time.RFC3339Nano),
		Model:     model,
		ReqPath:   reqPath,
		Method:    http.MethodPost,
	}
	startFrames := c.inflightStartFramesLocked(entry)
	c.mu.Unlock()
	for _, f := range startFrames {
		c.broadcast(f)
	}
	return id, func(tok Tokens, dur time.Duration) {
		c.mu.Lock()
		endFrames := c.inflightEndFramesLocked(entry)
		if c.loseRemove {
			c.loseRemove = false
			endFrames = nil
		}
		row := Row{
			ID: id, Timestamp: time.Now().Format(time.RFC3339), Model: model,
			ReqPath: reqPath, RespContentType: contentTypeFor(reqPath), RespStatusCode: 200,
			DurationMs: dur.Milliseconds(),
			Tokens: RowTokens{
				CacheTokens: tok.Cache, DraftTokens: tok.Draft, DraftAccTokens: tok.DraftAcc,
				InputTokens: tok.Input, OutputTokens: tok.Output,
				PromptPerSecond: tok.PromptPerSecond, TokensPerSecond: tok.TokensPerSecond,
			},
		}
		c.mu.Unlock()
		c.broadcast(activityFrame(id))
		for _, f := range endFrames {
			c.broadcast(f)
		}
		c.mu.Lock()
		c.rows = append(c.rows, row)
		if c.ringCap > 0 && len(c.rows) > c.ringCap {
			c.rows = c.rows[len(c.rows)-c.ringCap:]
		}
		c.mu.Unlock()
	}
}

func (c *Cell) handleChat(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model    string           `json:"model"`
		Messages []map[string]any `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model == "" {
		c.Fail(body.Model, "/v1/chat/completions", http.StatusBadRequest, "invalid request body")
		http.Error(w, `{"error":{"message":"invalid request body"}}`, http.StatusBadRequest)
		return
	}
	if !c.known(body.Model) {
		// Verbatim from the row recorded off v239: an unrouteable model is
		// a 404 with error_msg "no router for requested model", zero
		// tokens and duration_ms 0 — nothing upstream was ever dialled.
		// The row still exists, which is the whole point: C7a's err_req
		// counter has something to count.
		c.Fail(body.Model, "/v1/chat/completions", http.StatusNotFound, "no router for requested model")
		http.Error(w, `{"error":"no router for requested model"}`, http.StatusNotFound)
		return
	}
	c.mu.Lock()
	delay := c.chatDelay
	c.mu.Unlock()
	_, done := c.StartRequest(body.Model, "/v1/chat/completions")
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
		}
	}
	done(Tokens{
		Cache: 0, Draft: NotReported, DraftAcc: NotReported, Input: 12, Output: 1,
		PromptPerSecond: 1200, TokensPerSecond: 90,
	}, 200*time.Millisecond)
	writeJSON(w, map[string]any{
		"id":      "chatcmpl-swaptest",
		"object":  "chat.completion",
		"model":   body.Model,
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 12, "completion_tokens": 1, "total_tokens": 13},
	})
}

func (c *Cell) handleEmbed(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	// llama.cpp emits no timings block on /v1/embeddings, so llama-swap
	// records the OpenAI usage figure with cache already included. That is
	// exactly why C7a branches its token arithmetic on req_path.
	c.Request(body.Model, "/v1/embeddings", Tokens{Cache: -1, Draft: -1, DraftAcc: -1, Input: 24, Output: 0}, 30*time.Millisecond)
	writeJSON(w, map[string]any{
		"object": "list",
		"data":   []any{map[string]any{"object": "embedding", "index": 0, "embedding": []float64{0.1, 0.2}}},
		"usage":  map[string]any{"prompt_tokens": 24, "total_tokens": 24},
	})
}

// NotReported is llama-swap's sentinel for a token counter it could not
// observe. It is -1, not 0, on cache_tokens, draft_tokens and
// draft_acc_tokens alike, and every consumer must clamp rather than sum it.
const NotReported int64 = -1

func contentTypeFor(reqPath string) string {
	if strings.Contains(reqPath, "chat") {
		// Streaming responses DO carry full token counts: llama-swap parses
		// llama.cpp's timings block out of the stream, so no stream_options
		// cooperation is needed. A fake that zeroes tokens on
		// text/event-stream teaches the opposite.
		return "text/event-stream"
	}
	return "application/json"
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (c *Cell) String() string { return fmt.Sprintf("swaptest.Cell(%s, %s)", c.url, c.wire) }
