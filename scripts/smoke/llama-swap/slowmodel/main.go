// Command slowmodel fakes an inference engine with a multi-minute cold start
// for the llama-swap six-client smoke rig (docs/design/router-lifecycle.md
// section 6.3, roadmap gates A1/A5). For the first --delay it does not listen
// at all, so llama-swap health checks fail-connect exactly like a loading
// vLLM; then it binds and serves a minimal OpenAI + Anthropic surface. Every
// request line goes to stderr so kill-cancel-test.sh can verify whether a
// client disconnect propagated upstream.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

var words = []string{"hello", " there", " friend", " from", " slowmodel", " land"}

type chatRequest struct {
	Stream    bool `json:"stream"`
	MaxTokens int  `json:"max_tokens"`
}

type server struct {
	model     string
	wordDelay time.Duration
	log       *slog.Logger
}

func main() {
	port := flag.Int("port", 18099, "port to bind after the cold-start delay")
	delay := flag.Duration("delay", 90*time.Second, "cold start: how long to stay unbound (e.g. 420s)")
	dieAfter := flag.Duration("die-after", 0, "exit non-zero this long after binding (failure-path tests); 0 disables")
	model := flag.String("model", "slowmodel", "model id reported by /v1/models and echoed in completions")
	wordDelay := flag.Duration("word-delay", 200*time.Millisecond, "pause between streamed tokens")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	logger.Info("cold start: refusing connections", "delay", *delay, "port", *port)
	time.Sleep(*delay)

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		logger.Error("listen failed", "err", err)
		os.Exit(1)
	}
	logger.Info("bound and serving", "addr", ln.Addr().String(), "model", *model)
	if *dieAfter > 0 {
		time.AfterFunc(*dieAfter, func() {
			logger.Error("die-after elapsed, exiting", "after", *dieAfter)
			os.Exit(1)
		})
	}

	s := &server{model: *model, wordDelay: *wordDelay, log: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /v1/models", s.listModels)
	mux.HandleFunc("POST /v1/chat/completions", s.chatCompletions)
	mux.HandleFunc("POST /v1/messages", s.anthropicMessages)
	mux.HandleFunc("POST /v1/messages/count_tokens", s.countTokens)
	if err := http.Serve(ln, s.logRequests(mux)); err != nil {
		logger.Error("serve stopped", "err", err)
		os.Exit(1)
	}
}

func (s *server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.log.Info("request", "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
		next.ServeHTTP(w, r)
	})
}

func (s *server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *server) listModels(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data": []map[string]any{
			{"id": s.model, "object": "model", "created": time.Now().Unix(), "owned_by": "slowmodel"},
		},
	})
}

func (s *server) countTokens(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": 8})
}

// timings is llama.cpp's own block, and llama-swap parses it — not the
// OpenAI `usage` object — to populate an activity row's token counts on
// chat-family paths. Without it this rig drives the REAL llama-swap and
// every row it logs comes back unmeasured, so C7a's whole chat basis
// (`input_tokens` is timings.prompt_n, which is CACHE-MISS ONLY) is
// exercised by nothing here.
//
// cache_n is deliberately non-zero: billable input is input + max(cache,0)
// on this basis, and a rig that always reports a cold prompt never
// exercises the addition.
func timings(n int) map[string]any {
	const promptN, cacheN = 8, 3
	return map[string]any{
		"prompt_n":             promptN,
		"prompt_ms":            12.0,
		"prompt_per_second":    float64(promptN) / 0.012,
		"predicted_n":          n,
		"predicted_ms":         float64(n) * 10,
		"predicted_per_second": 100.0,
		"cache_n":              cacheN,
	}
}

func (s *server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if !s.decode(w, r, &req) {
		return
	}
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	n := tokenCount(req.MaxTokens)
	if !req.Stream {
		writeJSON(w, http.StatusOK, map[string]any{
			"id": id, "object": "chat.completion", "created": time.Now().Unix(), "model": s.model,
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": fullText(n)},
			}},
			"usage":   map[string]any{"prompt_tokens": 8, "completion_tokens": n, "total_tokens": 8 + n},
			"timings": timings(n),
		})
		return
	}
	fl := startSSE(w)
	if fl == nil {
		return
	}
	for i := range n {
		if !s.pause(r) {
			return
		}
		delta := map[string]any{"content": words[i%len(words)]}
		if i == 0 {
			delta["role"] = "assistant"
		}
		writeSSEData(w, map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": s.model,
			"choices": []map[string]any{{"index": 0, "delta": delta}},
		})
		fl.Flush()
	}
	writeSSEData(w, map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": s.model,
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 8, "completion_tokens": n, "total_tokens": 8 + n},
		"timings": timings(n),
	})
	fmt.Fprint(w, "data: [DONE]\n\n")
	fl.Flush()
	s.log.Info("stream complete", "path", r.URL.Path, "id", id, "tokens", n)
}

func (s *server) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if !s.decode(w, r, &req) {
		return
	}
	id := fmt.Sprintf("msg_%d", time.Now().UnixNano())
	n := tokenCount(req.MaxTokens)
	if !req.Stream {
		writeJSON(w, http.StatusOK, map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": s.model,
			"content":     []map[string]any{{"type": "text", "text": fullText(n)}},
			"stop_reason": "end_turn", "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 8, "output_tokens": n},
		})
		return
	}
	fl := startSSE(w)
	if fl == nil {
		return
	}
	writeSSEEvent(w, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": s.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 8, "output_tokens": 0},
		},
	})
	writeSSEEvent(w, "content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
	fl.Flush()
	for i := range n {
		if !s.pause(r) {
			return
		}
		writeSSEEvent(w, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": words[i%len(words)]},
		})
		fl.Flush()
	}
	writeSSEEvent(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	writeSSEEvent(w, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": n},
	})
	writeSSEEvent(w, "message_stop", map[string]any{"type": "message_stop"})
	fl.Flush()
	s.log.Info("stream complete", "path", r.URL.Path, "id", id, "tokens", n)
}

func (s *server) decode(w http.ResponseWriter, r *http.Request, req *chatRequest) bool {
	if err := json.NewDecoder(r.Body).Decode(req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"type": "invalid_request_error", "message": "bad request body: " + err.Error()},
		})
		return false
	}
	return true
}

// pause returns false when the client went away; the log line it emits is
// the signal kill-cancel-test.sh greps for to prove disconnects propagate.
func (s *server) pause(r *http.Request) bool {
	select {
	case <-r.Context().Done():
		s.log.Warn("client disconnected mid-stream", "path", r.URL.Path, "remote", r.RemoteAddr)
		return false
	case <-time.After(s.wordDelay):
		return true
	}
}

// tokenCount bounds the streamed-token loop: long enough that a mid-stream
// kill lands inside it, short enough that agentic clients sending huge
// max_tokens budgets do not stall the smoke run.
func tokenCount(maxTokens int) int {
	switch {
	case maxTokens <= 0:
		return len(words)
	case maxTokens > 60:
		return 60
	default:
		return maxTokens
	}
}

func fullText(n int) string {
	var b strings.Builder
	for i := range n {
		b.WriteString(words[i%len(words)])
	}
	return b.String()
}

func startSSE(w http.ResponseWriter) http.Flusher {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return nil
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl.Flush()
	return fl
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeSSEData(w io.Writer, payload any) {
	b, _ := json.Marshal(payload)
	fmt.Fprintf(w, "data: %s\n\n", b)
}

func writeSSEEvent(w io.Writer, event string, payload any) {
	b, _ := json.Marshal(payload)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
}
