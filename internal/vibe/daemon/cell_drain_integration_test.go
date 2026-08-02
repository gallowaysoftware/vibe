package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/gallowaysoftware/vibe/internal/vibe/daemon"
	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
	vibev1 "github.com/gallowaysoftware/vibe/proto/vibe/v1"
	"github.com/gallowaysoftware/vibe/proto/vibe/v1/vibev1connect"
)

// intentSink records intent POSTs the way fleetd would, and captures
// announces the way fleetd's presence table does.
type intentSink struct {
	mu        sync.Mutex
	posts     []map[string]any
	announces []map[string]any
}

func (s *intentSink) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/fleet/intent", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.posts = append(s.posts, body)
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"state": "ok"})
	})
	mux.HandleFunc("GET /api/fleet/leases", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"leases":[]}`))
	})
	mux.HandleFunc("POST /api/fleet/announce", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.announces = append(s.announces, body)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"interval_s": 1})
	})
	return mux
}

func (s *intentSink) calls() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any{}, s.posts...)
}

func (s *intentSink) echoes() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, a := range s.announces {
		if intent, ok := a["intent"].(map[string]any); ok {
			out = append(out, intent)
		}
	}
	return out
}

// TestDaemon_CellDrain_IntentWriterByListener proves the C2 one-writer
// rule end to end through the daemon's real listeners: an invocation
// arriving over TCP (fleetd's path) records no intent from the cell
// daemon, while the same call over the unix socket (the local operator's
// path) posts it, best-effort, to the registry.
func TestDaemon_CellDrain_IntentWriterByListener(t *testing.T) {
	setupXDG(t)
	sink := &intentSink{}
	fleetd := httptest.NewServer(sink.handler())
	t.Cleanup(fleetd.Close)

	httpAddr := fmt.Sprintf("127.0.0.1:%d", pickFreePort(t))
	cfg := daemon.Config{
		ProxyPort:    pickFreePort(t),
		DisableProxy: true,
		HTTPAddr:     httpAddr,
		LlamaBinary:  fakeBinary,
		CellCmds:     daemon.CellCmds{Drain: "true", Resume: "true"},
		Fleet:        daemon.FleetConfig{Cell: "gpu-cell", RegistryURL: fleetd.URL},
	}
	d := daemon.New(cfg)
	_, _ = startDaemon(t, d)

	// TCP invocation (fleetd's path): no intent from the cell daemon.
	tokenBytes, err := os.ReadFile(paths.TokenFile())
	if err != nil {
		t.Fatal(err)
	}
	tcpClient := vibev1connect.NewControlServiceClient(http.DefaultClient, "http://"+httpAddr,
		connect.WithInterceptors(tokenInterceptor(strings.TrimSpace(string(tokenBytes)))))
	if _, err := tcpClient.CellDrain(context.Background(), connect.NewRequest(&vibev1.CellDrainRequest{Reason: "via fleetd"})); err != nil {
		t.Fatalf("TCP drain: %v", err)
	}
	if got := sink.calls(); len(got) != 0 {
		t.Errorf("TCP (fleetd) invocation wrote intent from the cell daemon: %v", got)
	}

	// Unix invocation (the local operator): intent lands via the
	// announce ECHO (C3 — the echo is the cell-side record; the C2-era
	// direct POST is skipped for announcing cells).
	unixClient := vibev1connect.NewControlServiceClient(&http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", paths.Socket())
			},
		},
	}, "http://vibe.local")
	if _, err := unixClient.CellDrain(context.Background(), connect.NewRequest(&vibev1.CellDrainRequest{Reason: "gaming", Eta: "23:00"})); err != nil {
		t.Fatalf("unix drain: %v", err)
	}
	if got := sink.calls(); len(got) != 0 {
		t.Errorf("announcing cell still POSTed intent directly: %v", got)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		found := false
		for _, e := range sink.echoes() {
			if e["state"] == "drained" {
				found = true
			}
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no drained echo within 5s; announces: %v", sink.echoes())
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Unix resume: the echo flips back to serving.
	if _, err := unixClient.CellResume(context.Background(), connect.NewRequest(&vibev1.CellResumeRequest{})); err != nil {
		t.Fatalf("unix resume: %v", err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		last := sink.echoes()
		if len(last) > 0 && last[len(last)-1]["state"] == "serving" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no serving echo after resume; echoes: %v", sink.echoes())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

type bearerToken string

func (b bearerToken) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("Authorization", "Bearer "+string(b))
		return next(ctx, req)
	}
}
func (b bearerToken) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("Authorization", "Bearer "+string(b))
		return conn
	}
}
func (b bearerToken) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func tokenInterceptor(tok string) connect.Interceptor { return bearerToken(tok) }
