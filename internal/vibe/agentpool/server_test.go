package agentpool

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/swaptest"
	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
)

// Every socket here belongs to the test that opened it, per AGENTS.md's
// proxy-test rules: both hops dial through transports the test owns, and a
// parked upstream handler is released on every exit path.

type fakeFront struct {
	mu     sync.Mutex
	models []string
	// down lists member ids the front answers like a withdrawn cell.
	down map[string]bool
}

func (f *fakeFront) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.models...)
}

func (f *fakeFront) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.models = append(f.models, body.Model)
	down := f.down[body.Model]
	f.mu.Unlock()
	if down {
		http.Error(w, "no router for requested model", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"model":"`+body.Model+`","choices":[{"message":{"content":"ok"}}]}`)
}

type rig struct {
	front    *httptest.Server
	frontTr  *http.Transport
	pool     *httptest.Server
	client   *http.Client
	clientTr *http.Transport
	server   *Server
}

func newRig(t *testing.T, front http.Handler, upstream string, members ...MemberConfig) *rig {
	t.Helper()
	r := &rig{frontTr: &http.Transport{}, clientTr: &http.Transport{}}
	if front != nil {
		r.front = httptest.NewServer(front)
		upstream = r.front.URL
	}
	cfg := Config{
		Upstream:     upstream,
		AffinityTTL:  profile.Duration(time.Hour),
		DownCooldown: profile.Duration(time.Minute),
		QueueTimeout: profile.Duration(2 * time.Second),
		Pools:        []PoolConfig{{ID: "agent-exec", Context: 131072, Members: members}},
	}
	r.server = NewServer(cfg, r.frontTr, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.pool = httptest.NewServer(r.server)
	r.client = &http.Client{Transport: r.clientTr}
	t.Cleanup(func() {
		r.clientTr.CloseIdleConnections()
		r.pool.Close()
		r.frontTr.CloseIdleConnections()
		if r.front != nil {
			r.front.Close()
		}
	})
	return r
}

func (r *rig) post(t *testing.T, body string) *http.Response {
	t.Helper()
	resp, err := r.client.Post(r.pool.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	return resp
}

const convA = `{"model":"agent-exec","messages":[{"role":"system","content":"s"},{"role":"user","content":"task A"}]}`

func TestServer_RewritesThePoolIdToTheChosenMember(t *testing.T) {
	front := &fakeFront{}
	r := newRig(t, front, "", MemberConfig{"localmodel/qwen3.8-27b-tools", 1})
	resp := r.post(t, convA)
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if got := front.seen(); len(got) != 1 || got[0] != "localmodel/qwen3.8-27b-tools" {
		t.Fatalf("front saw %v", got)
	}
	if m := resp.Header.Get("X-Agent-Pool-Member"); m != "localmodel/qwen3.8-27b-tools" {
		t.Fatalf("X-Agent-Pool-Member = %q", m)
	}
}

func TestServer_StepsPastAMemberTheFrontCannotServe(t *testing.T) {
	// The shared llama-swap double: a model it does not route gets the
	// front's real "no router for requested model" 404.
	cell := swaptest.NewCell(t, swaptest.WithModels(swaptest.Model{ID: "drift/qwen38fn-mlx", State: "ready"}))
	r := newRig(t, nil, cell.URL(), MemberConfig{"localmodel/qwen3.8-27b-tools", 1}, MemberConfig{"drift/qwen38fn-mlx", 1})
	resp := r.post(t, convA)
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if m := resp.Header.Get("X-Agent-Pool-Member"); m != "drift/qwen38fn-mlx" {
		t.Fatalf("served by %q; want drift after the 5090 was unroutable", m)
	}
	st := r.server.pools["agent-exec"].Status()
	if !st.Members[0].Down || st.Members[0].Busy != 0 || st.Members[1].Busy != 0 {
		t.Fatalf("status after fallback: %+v", st.Members)
	}
}

func TestServer_PathsOtherThanTheExactCompletionPathsNeverReachTheFront(t *testing.T) {
	front := &fakeFront{}
	r := newRig(t, front, "", MemberConfig{"a", 1})
	resp, err := r.client.Post(r.pool.URL+"/openai/v1/chat/completions", "application/json", strings.NewReader(convA))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusNotFound || len(front.seen()) != 0 {
		t.Fatalf("status %d, front saw %v", resp.StatusCode, front.seen())
	}
	if r.server.pools["agent-exec"].Status().Members[0].Down {
		t.Fatal("a misrouted path marked the member down")
	}
}

func TestServer_ForwardsPromptsWithoutHTMLEscaping(t *testing.T) {
	var got []byte
	var mu sync.Mutex
	front := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = b
		mu.Unlock()
		_, _ = io.WriteString(w, `{}`)
	})
	r := newRig(t, front, "", MemberConfig{"a", 1})
	resp := r.post(t, `{"model":"agent-exec","messages":[{"role":"user","content":"if a < b && c > d"}]}`)
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(string(got), `if a < b && c > d`) || !strings.Contains(string(got), `"model":"a"`) {
		t.Fatalf("front received %s", got)
	}
}

func TestServer_DrainFailsQueuedRequestsAtOnce(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unpark := func() { once.Do(func() { close(release) }) }
	front := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-release
	})
	r := newRig(t, front, "", MemberConfig{"a", 1})
	defer unpark()
	first := r.post(t, convA) //nolint:bodyclose // closed below after the member is unparked
	defer func() {
		unpark()
		_, _ = io.Copy(io.Discard, first.Body)
		first.Body.Close()
	}()
	queued := make(chan int, 1)
	go func() {
		resp, err := r.client.Post(r.pool.URL+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"agent-exec","messages":[{"role":"user","content":"task B"}]}`))
		if err != nil {
			queued <- -1
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		queued <- resp.StatusCode
	}()
	waitForWaiters(t, r.server.pools["agent-exec"], 1)
	r.server.Drain()
	select {
	case code := <-queued:
		if code != http.StatusServiceUnavailable {
			t.Fatalf("queued request got %d, want 503", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("drain left the queued request waiting")
	}
}

func TestServer_BadRequestFromTheMemberIsPassedThroughNotRetried(t *testing.T) {
	front := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"context too long"}`, http.StatusBadRequest)
	})
	r := newRig(t, front, "", MemberConfig{"a", 1}, MemberConfig{"b", 1})
	resp := r.post(t, convA)
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want the member's 400", resp.StatusCode)
	}
	if st := r.server.pools["agent-exec"].Status(); st.Members[0].Down {
		t.Fatal("a 400 marked the member down")
	}
}

func TestServer_DeadFrontAnswers503WithRetryAfter(t *testing.T) {
	r := newRig(t, nil, "http://127.0.0.1:1", MemberConfig{"a", 1}, MemberConfig{"b", 1})
	resp := r.post(t, convA)
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("status %d Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

func TestServer_StreamsEachFrameBeforeTheMemberFinishes(t *testing.T) {
	const first = "data: {\"choices\":[{\"delta\":{\"content\":\"one\"}}]}\n\n"
	release := make(chan struct{})
	var once sync.Once
	unpark := func() { once.Do(func() { close(release) }) }
	front := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, first)
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	r := newRig(t, front, "", MemberConfig{"a", 1})
	defer unpark()
	resp := r.post(t, strings.Replace(convA, `"model"`, `"stream":true,"model"`, 1)) //nolint:bodyclose // closed by the teardown below, which bodyclose cannot follow
	got := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		line, _ := bufio.NewReader(resp.Body).ReadString('\n')
		got <- line
	}()
	// Unpark the member, join the reader, then close: closing under a live
	// read is the failure mode that looks exactly like a buffered stream.
	defer func() {
		unpark()
		<-done
		resp.Body.Close()
	}()
	select {
	case line := <-got:
		if !strings.HasPrefix(first, line) {
			t.Fatalf("first line %q", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first frame did not arrive while the member was still generating: the pool buffered the stream")
	}
	unpark()
}

func TestServer_SlotIsHeldUntilTheStreamEnds(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unpark := func() { once.Do(func() { close(release) }) }
	front := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-release
	})
	r := newRig(t, front, "", MemberConfig{"a", 1})
	defer unpark()
	resp := r.post(t, convA)
	defer resp.Body.Close()
	if st := r.server.pools["agent-exec"].Status(); st.Members[0].Busy != 1 {
		t.Fatalf("busy = %d mid-stream", st.Members[0].Busy)
	}
	unpark()
	_, _ = io.Copy(io.Discard, resp.Body)
	deadline := time.Now().Add(2 * time.Second)
	for r.server.pools["agent-exec"].Status().Members[0].Busy != 0 {
		if time.Now().After(deadline) {
			t.Fatal("slot not released after the stream ended")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestServer_UnknownModelIs404(t *testing.T) {
	r := newRig(t, &fakeFront{}, "", MemberConfig{"a", 1})
	resp := r.post(t, `{"model":"heavy/glm53flash","messages":[]}`)
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestServer_ModelsListsEachPoolWithItsContext(t *testing.T) {
	r := newRig(t, &fakeFront{}, "", MemberConfig{"a", 1})
	resp, err := r.client.Get(r.pool.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength int    `json:"context_length"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 1 || out.Data[0].ID != "agent-exec" || out.Data[0].ContextLength != 131072 {
		t.Fatalf("models = %+v", out.Data)
	}
}

func TestAffinityKey_SameForAGrowingConversationAndForReorderedFields(t *testing.T) {
	open := []json.RawMessage{
		json.RawMessage(`{"role":"system","content":"s"}`),
		json.RawMessage(`{"role":"user","content":"task"}`),
	}
	grown := append(append([]json.RawMessage{}, open...),
		json.RawMessage(`{"role":"assistant","content":"x"}`),
		json.RawMessage(`{"role":"user","content":"more"}`))
	reordered := []json.RawMessage{
		json.RawMessage(`{"content":"s","role":"system"}`),
		json.RawMessage(`{ "content": "task", "role": "user" }`),
	}
	k := affinityKey(open)
	if k == "" || affinityKey(grown) != k || affinityKey(reordered) != k {
		t.Fatal("one conversation produced different keys")
	}
	other := []json.RawMessage{open[0], json.RawMessage(`{"role":"user","content":"other task"}`)}
	if affinityKey(other) == k {
		t.Fatal("different tasks share a key")
	}
	if affinityKey([]json.RawMessage{open[0]}) != "" {
		t.Fatal("a conversation with no user message got a key")
	}
}

func TestLoadConfig_FillsDefaultsAndRejectsBadPools(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "c.yaml")
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	c, err := LoadConfig(write("upstream: http://front:9000\npools:\n  - id: agent-exec\n    members:\n      - {model: a, slots: 1}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9010" || time.Duration(c.DownCooldown) != 30*time.Second || time.Duration(c.QueueTimeout) != 15*time.Minute {
		t.Fatalf("defaults not filled: %+v", c)
	}
	for _, bad := range []string{
		"pools:\n  - id: p\n    members: [{model: a, slots: 1}]\n",
		"upstream: x\npools:\n  - id: p\n    members: [{model: a, slots: 0}]\n",
		"upstream: x\npools:\n  - id: p\n    members: []\n",
		"upstream: x\nbogus: 1\npools:\n  - id: p\n    members: [{model: a, slots: 1}]\n",
		"upstream: x\ndown_cooldown: -30s\npools:\n  - id: p\n    members: [{model: a, slots: 1}]\n",
	} {
		if _, err := LoadConfig(write(bad)); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
