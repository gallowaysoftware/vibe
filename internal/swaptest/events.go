package swaptest

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// inflightEntry is one live request. It is the double's internal record;
// what goes on the wire is a per-Wire VIEW of it, because the field set
// grew between the two versions and both forms are recorded.
type inflightEntry struct {
	ID        string
	Timestamp string
	Model     string
	ReqPath   string
	Method    string
}

// entryV239 is the field set recorded off v239's wire.
type entryV239 struct {
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Model     string `json:"model"`
	ReqPath   string `json:"req_path"`
	Method    string `json:"method"`
}

// entryV247 is the field set recorded off v247's wire. The five additions
// are why this package asserts on SEMANTICS and never on bytes: every one
// of them is harmless to a decoder that ignores unknown fields, and a byte
// golden would have flapped on all five.
type entryV247 struct {
	ID          string            `json:"id"`
	Timestamp   string            `json:"timestamp"`
	Model       string            `json:"model"`
	ReqPath     string            `json:"req_path"`
	Method      string            `json:"method"`
	ReqHeaders  map[string]string `json:"req_headers"`
	RemoteIP    string            `json:"remote_ip"`
	RespHeaders map[string]string `json:"resp_headers"`
	RespBytes   int64             `json:"resp_bytes"`
	ElapsedMs   int64             `json:"elapsed_ms"`
}

// view renders an entry in the wire's own field set.
func (w Wire) view(e inflightEntry) any { return w.viewAt(e, false) }

// viewAt renders an entry either as it looks when the request ARRIVES
// (resp_headers empty, elapsed_ms 0) or as it looks once the upstream's
// response headers have landed. Real v247 publishes both — two upserts for
// the same id, the second carrying resp_headers and a non-zero elapsed_ms.
func (w Wire) viewAt(e inflightEntry, responded bool) any {
	if w == WireV247 {
		v := entryV247{
			ID: e.ID, Timestamp: e.Timestamp, Model: e.Model, ReqPath: e.ReqPath, Method: e.Method,
			ReqHeaders: map[string]string{
				"Accept-Encoding": "gzip",
				"Content-Type":    "application/json",
				"User-Agent":      "Go-http-client/1.1",
			},
			RemoteIP:    "127.0.0.1",
			RespHeaders: map[string]string{},
		}
		if responded {
			v.RespHeaders = map[string]string{
				"Access-Control-Allow-Origin": "",
				"Content-Type":                "application/json; charset=utf-8",
				"Server":                      "llama.cpp",
			}
			v.ElapsedMs = 54
		}
		return v
	}
	return entryV239(e)
}

func (w Wire) views(es []inflightEntry) []any {
	out := make([]any, 0, len(es))
	for _, e := range es {
		out = append(out, w.view(e))
	}
	return out
}

// stream is one open /api/events connection.
type stream struct {
	ch     chan string
	closed chan struct{}
	once   sync.Once
	sent   int
}

func (s *stream) close() { s.once.Do(func() { close(s.closed) }) }

// frame renders one SSE frame exactly as llama-swap does: an event: line,
// a data: line carrying the {"type","data"} envelope, and a blank line.
// Note the DOUBLE ENCODING — the envelope's data member is a JSON *string*
// whose contents are themselves JSON. A fake that nests a plain object
// there passes trackInFlight's first Unmarshal by failing it, which is a
// vacuous pass.
func frame(typ string, payload any) string {
	inner, err := json.Marshal(payload)
	if err != nil {
		panic("swaptest: marshal inner: " + err.Error())
	}
	return rawFrame(typ, string(inner))
}

// envelope is a struct rather than a map so the encoder emits type before
// data, which is the order llama-swap writes. Key order is not semantic,
// but a fixture that differs from the wire in a visible way invites the
// next reader to wonder what else differs.
type envelope struct {
	Type string `json:"type"`
	Data string `json:"data"`
}

// rawFrame renders a frame whose inner data is already-encoded JSON text,
// for payloads whose exact byte form matters.
func rawFrame(typ, innerJSON string) string {
	env, err := json.Marshal(envelope{Type: typ, Data: innerJSON})
	if err != nil {
		panic("swaptest: marshal envelope: " + err.Error())
	}
	return "event:message\ndata:" + string(env) + "\n\n"
}

func activityFrame(id int64) string {
	return rawFrame("activity", `{"id":`+strconv.FormatInt(id, 10)+`}`)
}

// inflightStartFramesLocked renders the edge(s) a request START produces on
// this wire. c.mu must be held.
func (c *Cell) inflightStartFramesLocked(e inflightEntry) []string {
	c.live = append(c.live, e)
	switch c.wire {
	case WireV247:
		// TWO upserts for the same id, which is what a real v247 sends: one
		// when the request arrives and one when the upstream's response
		// headers land. Recorded in fixtures/v247/events-request.sse and
		// re-observed live. It is the reason the consumer's fold has to be a
		// SET — a counter reads the pair as two requests in flight and never
		// gets back to zero on the single remove that retires them.
		return []string{
			frame("inflight", map[string]any{"operation": "upsert", "request": c.wire.viewAt(e, false)}),
			frame("inflight", map[string]any{"operation": "upsert", "request": c.wire.viewAt(e, true)}),
		}
	default:
		return []string{frame("inflight", map[string]any{"requests": c.wire.views(c.live)})}
	}
}

// inflightEndFramesLocked renders the edge(s) a request COMPLETION
// produces. c.mu must be held.
func (c *Cell) inflightEndFramesLocked(e inflightEntry) []string {
	out := c.live[:0]
	for _, x := range c.live {
		if x.ID != e.ID {
			out = append(out, x)
		}
	}
	c.live = out
	switch c.wire {
	case WireV247:
		// The remove edge names ONLY the id. Everything a consumer wants to
		// know about the request that just finished — its model above all —
		// has to have been remembered from the upsert.
		return []string{frame("inflight", map[string]any{"operation": "remove", "id": e.ID})}
	default:
		return []string{frame("inflight", map[string]any{"requests": c.wire.views(c.live)})}
	}
}

// inflightSnapshotFrameLocked is the current-state frame a fresh connection
// receives. On v239 it is an ordinary full-list frame; on v247 it is tagged
// `snapshot`, and `requests` is omitempty so an idle cell sends the
// operation alone.
func (c *Cell) inflightSnapshotFrameLocked() string {
	switch c.wire {
	case WireV247:
		m := map[string]any{"operation": "snapshot"}
		if len(c.live) > 0 {
			m["requests"] = c.wire.views(c.live)
		}
		return frame("inflight", m)
	default:
		return frame("inflight", map[string]any{"requests": c.wire.views(c.live)})
	}
}

func (c *Cell) modelStatusFrameLocked() string {
	type ms struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		Description string   `json:"description"`
		State       string   `json:"state"`
		Unlisted    bool     `json:"unlisted"`
		PeerID      string   `json:"peerID"`
		Aliases     []string `json:"aliases,omitempty"`
	}
	out := make([]ms, 0, len(c.models))
	for _, m := range c.models {
		out = append(out, ms{ID: m.ID, State: m.State, Unlisted: m.Unlisted, PeerID: m.PeerID, Aliases: m.Aliases})
	}
	return frame("modelStatus", out)
}

// handleEvents serves the SSE stream. The connect burst reproduces the one
// recorded off v239: logData, logData, modelStatus, inflight — all four
// inside ~200ms of the connect (v247 adds uiConfig and profileChanged
// before the snapshot). The trailing inflight snapshot is why fleetapi's
// "no inflight frame seen yet" branch is a sub-second window in production
// rather than a state a cell sits in.
func (c *Cell) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flush", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	st := &stream{ch: make(chan string, 256), closed: make(chan struct{})}
	c.mu.Lock()
	c.connects++
	c.streams++
	c.subs[st] = struct{}{}
	dropAfter := c.dropAfter
	burst := []string{
		// TWO logData frames, as both recordings carry: llama-swap replays
		// its whole log history on connect and it does not arrive as one
		// frame. Their combined size is the thing that matters — 210 KB when
		// v239 was recorded, against fleetapi's 8 MB scanner cap.
		frame("logData", map[string]string{"data": "[INFO] llama-swap listening\n"}),
		frame("logData", map[string]string{"data": "[INFO] loading config\n"}),
		c.modelStatusFrameLocked(),
	}
	if c.wire == WireV247 {
		// Recorded off a real v247: two frame types v239 never sends. The
		// module forwards unknown envelope types to subscribers untouched,
		// so these are additive — which is exactly the claim worth having a
		// fake emit, rather than assuming.
		burst = append(burst,
			rawFrame("uiConfig", `{"activity":{"session_id":["X-Session-ID","X-Litellm-Session-Id"]}}`),
			rawFrame("profileChanged", `{"active":null}`),
		)
	}
	burst = append(burst, c.inflightSnapshotFrameLocked())
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.subs, st)
		c.streams--
		c.mu.Unlock()
		st.close()
	}()

	send := func(s string) bool {
		if _, err := w.Write([]byte(s)); err != nil {
			return false
		}
		fl.Flush()
		st.sent++
		return dropAfter <= 0 || st.sent < dropAfter
	}
	for _, f := range burst {
		if !send(f) {
			return
		}
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-st.closed:
			return
		case f := <-st.ch:
			if !send(f) {
				return
			}
		}
	}
}

func (c *Cell) broadcast(f string) {
	c.mu.Lock()
	subs := make([]*stream, 0, len(c.subs))
	for s := range c.subs {
		subs = append(subs, s)
	}
	c.mu.Unlock()
	dropped := 0
	for _, s := range subs {
		select {
		case s.ch <- f:
		case <-s.closed:
			// Not a drop: the subscriber is gone, which is exactly what
			// DropStreams and a client disconnect look like.
		default:
			dropped++
		}
	}
	if dropped > 0 {
		c.mu.Lock()
		c.dropped += dropped
		c.mu.Unlock()
	}
}

// DroppedFrames counts frames this cell could not hand to a LIVE subscriber
// because that subscriber's buffer was full.
//
// It exists so the number is never zero-by-assumption. A double that
// silently discards an inflight edge produces a consumer stuck at a stale
// count, and the test that notices fails somewhere else entirely, minutes
// later, in a way that reads as a race in production code. NewCell fails
// the test if this ends non-zero.
func (c *Cell) DroppedFrames() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dropped
}

func (c *Cell) closeStreams() {
	c.mu.Lock()
	subs := make([]*stream, 0, len(c.subs))
	for s := range c.subs {
		subs = append(subs, s)
	}
	c.mu.Unlock()
	for _, s := range subs {
		s.close()
	}
}

// ─── fault injection ────────────────────────────────────────────────────────

// DropStreams closes every open /api/events connection. This is the fault a
// container cannot perform on demand: the consumer reconnects, and whatever
// it believed about in-flight work has to survive — or be discarded — on its
// own terms.
func (c *Cell) DropStreams() { c.closeStreams() }

// DropStreamAfter makes every subsequent /api/events connection die after
// delivering n frames. n <= 0 disables it.
func (c *Cell) DropStreamAfter(n int) {
	c.mu.Lock()
	c.dropAfter = n
	c.mu.Unlock()
}

// LoseRemoveEdge drops the completion edge of the NEXT request. Real
// llama-swap loses one whenever a stream drops mid-request; on v247 the
// consequence is a live request id that nothing will ever retire.
func (c *Cell) LoseRemoveEdge() {
	c.mu.Lock()
	c.loseRemove = true
	c.mu.Unlock()
}

// EmitLogData publishes a logData frame whose payload is n bytes. The real
// connect burst carries the whole log history in ONE line — 106 KB on the
// box these fixtures came from — and fleetapi caps its SSE scanner at 8 MB.
// Above that cap bufio returns ErrTooLong, which is indistinguishable from
// a clean close in code that never inspects Scanner.Err.
func (c *Cell) EmitLogData(n int) {
	c.broadcast(frame("logData", map[string]string{"data": strings.Repeat("x", n)}))
}

// Connects reports how many /api/events connections this cell has accepted,
// so a test can wait for a reconnect instead of sleeping.
func (c *Cell) Connects() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connects
}

// OpenStreams reports how many /api/events connections are open now.
func (c *Cell) OpenStreams() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.streams
}

// WaitConnects blocks until Connects() reaches n or the deadline passes,
// reporting whether it got there.
func (c *Cell) WaitConnects(n int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if c.Connects() >= n {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return c.Connects() >= n
}
