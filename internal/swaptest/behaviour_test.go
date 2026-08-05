package swaptest_test

// The two UPSTREAM BEHAVIOURS the fleet leans on that no wire shape can
// express (fleet-control C16).
//
// TestSwapContract asserts the SHAPE of what llama-swap says. These
// assert what it DOES, and both are load-bearing in a way that is easy to
// lose in an upgrade:
//
//   - B1, the loading-state keepalive, is the only reason a cold 6-10
//     minute model is usable at all. Clients kill silent streams (Claude
//     Code at ~5 min), and llama-swap answers a streaming completion for a
//     model that is still loading with delta frames from the first
//     milliseconds. router-lifecycle.md calls this the riskiest bet in the
//     design; scripts/smoke/llama-swap is the 45-minute six-client rig
//     that proved it against real clients. This is the 8-second mechanical
//     half of the same question, so a bump that removed the behaviour
//     fails in CI rather than in a hallway.
//
//   - B2, SIGTERM's treatment of in-flight streams, is what C2's
//     `drain --wait` exists for. The claim in the docs has been that
//     SIGTERM cancels inference streams immediately; measured here on
//     v239 AND v247 it does not — it closes /api/events instantly and
//     lets inference streams run, then force-closes whatever is still
//     going 30s later. Same operational answer (a generation longer than
//     the grace is truncated, so drain quiesces first), different
//     mechanism, and a different number to watch: the cell unit's
//     TimeoutStopSec has to exceed that grace.
//
// Both need a llama-swap PROCESS (a signal is not a wire), so they are
// env-gated on LLAMA_SWAP_BIN exactly as the live conformance target is.
// Neither needs llama.cpp: the upstream is an in-test HTTP server whose
// readiness and stream pacing are the parameters under test, which also
// isolates the question to llama-swap's own behaviour — a real
// llama-server dying on the same signal would answer a different one.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestSwapBehaviour(t *testing.T) {
	bin := os.Getenv("LLAMA_SWAP_BIN")
	if bin == "" {
		t.Skip("no LLAMA_SWAP_BIN; these assert what a llama-swap PROCESS does with a signal and a cold model")
	}
	t.Run("B1_loading_state_keeps_a_cold_stream_alive", func(t *testing.T) { b1(t, bin) })
	t.Run("B2_sigterm_stream_handling", func(t *testing.T) { b2(t, bin) })
}

// coldStart is how long B1's upstream refuses health checks. Long enough
// that a first byte inside firstByteBudget cannot be the model, short
// enough to keep the test at single-digit seconds.
const coldStart = 6 * time.Second

// firstByteBudget is the keepalive's whole point: bytes now, not bytes
// when the weights are in. The six-client rig uses 5s against a 420s load;
// this is the same claim at lab scale.
const firstByteBudget = 2 * time.Second

// b1 drives a streaming completion at a model that is deliberately not
// ready and requires the client to be receiving bytes long before it is.
func b1(t *testing.T, bin string) {
	up := newFakeUpstream(t, fakeUpstreamConfig{readyAfter: coldStart, chunks: 3, chunkEvery: 50 * time.Millisecond})
	sw := startSwap(t, bin, swapConfig{proxy: up.URL, sendLoadingState: true})

	start := time.Now()
	body, cancel := sw.stream(t, `{"model":"conformance","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	defer cancel()

	first, err := body.ReadString('\n')
	if err != nil {
		t.Fatalf("no first line off a cold streaming completion: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > firstByteBudget {
		t.Fatalf("first byte took %s against a model that stays unready for %s. "+
			"The loading-state keepalive is how a 6-10 minute cold start survives a client's stall timer; "+
			"without it every cold model over ~5 min is unreachable from Claude Code.", elapsed, coldStart)
	}
	if up.healthChecksPassed() {
		t.Fatalf("the upstream reported ready %s into a %s cold start; the timing this test rests on did not hold", elapsed, coldStart)
	}
	// The frame has to be something an OpenAI SSE parser accepts, not just
	// bytes. A stream that opens with an error payload also arrives fast.
	payload, ok := strings.CutPrefix(strings.TrimSpace(first), "data:")
	if !ok {
		t.Fatalf("the first line of a cold stream is %q, not an SSE data frame; an OpenAI SSE parser has nothing to read", strings.TrimSpace(first))
	}
	var chunk struct {
		Choices []struct {
			Delta map[string]any `json:"delta"`
		} `json:"choices"`
		Error any `json:"error"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(payload)), &chunk); err != nil {
		t.Fatalf("the first frame is not JSON: %v (%q)", err, payload)
	}
	if chunk.Error != nil || len(chunk.Choices) == 0 || len(chunk.Choices[0].Delta) == 0 {
		t.Fatalf("the first frame carries no delta: %q. Bytes that a client discards do not reset its stall timer.", payload)
	}

	// And the keepalive has to be the PREFIX OF A REAL ANSWER: the same
	// stream must go on to carry the upstream's tokens once it is up.
	// Without this the test is satisfied by a build that streams a
	// placeholder and then fails.
	deadline := time.Now().Add(coldStart + 20*time.Second)
	for time.Now().Before(deadline) {
		line, err := body.ReadString('\n')
		if err != nil {
			t.Fatalf("the cold stream ended before the model's own tokens arrived: %v", err)
		}
		if strings.Contains(line, "tok0") {
			return
		}
	}
	t.Fatalf("no upstream token reached the client within %s of a %s cold start", coldStart+20*time.Second, coldStart)
}

// sigtermGrace is llama-swap's shutdown deadline, measured at 30.00s on
// v239 and v247. The window is wide because CI runners are not
// wristwatches; it is bounded on BOTH sides because either direction is a
// change an operator has to act on — shorter truncates generations the
// cell unit was configured to protect, longer needs a matching
// TimeoutStopSec.
const sigtermGrace = 30 * time.Second

func b2(t *testing.T, bin string) {
	// 60s of stream: long enough to still be running at the grace deadline.
	up := newFakeUpstream(t, fakeUpstreamConfig{chunks: 300, chunkEvery: 200 * time.Millisecond})
	sw := startSwap(t, bin, swapConfig{proxy: up.URL})

	events, eventsCancel := sw.get(t, "/api/events")
	defer eventsCancel()
	eventsClosed := readUntilEOF(events)

	infl, inflCancel := sw.stream(t, `{"model":"conformance","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	defer inflCancel()
	chunks := make(chan time.Time, 512)
	inflClosed := make(chan time.Time, 1)
	go func() {
		sc := bufio.NewScanner(infl)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "data:") {
				select {
				case chunks <- time.Now():
				default:
				}
			}
		}
		inflClosed <- time.Now()
	}()

	// Wait for the stream to be genuinely flowing: signalling a request
	// that has not started proves nothing about in-flight work.
	select {
	case <-chunks:
	case <-time.After(20 * time.Second):
		t.Fatal("the inference stream produced no chunk before the signal; nothing was in flight to observe")
	}

	sent := time.Now()
	sw.terminate(t)

	// (a) /api/events is closed at once. This IS the CloseStreams the docs
	// have attributed to inference streams since C2.
	select {
	case at := <-eventsClosed:
		if d := at.Sub(sent); d > 2*time.Second {
			t.Errorf("/api/events survived SIGTERM by %s; fleetd's watcher would keep a dead cell's count", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("/api/events was still open 5s after SIGTERM")
	}

	// (b) the inference stream is NOT cancelled: it keeps delivering.
	drained := false
	deadline := time.After(3 * time.Second)
wait:
	for {
		select {
		case at := <-chunks:
			if at.After(sent.Add(1500 * time.Millisecond)) {
				drained = true
				break wait
			}
		case <-inflClosed:
			break wait
		case <-deadline:
			break wait
		}
	}
	if !drained {
		t.Errorf("the in-flight inference stream stopped within 1.5s of SIGTERM. That is the behaviour C2 documented and this build does not have to keep — " +
			"but `vibe cell drain --wait` and the cell unit's TimeoutStopSec are both written for the other one, so re-read docs/design/fleet-control.md §7 before shipping it.")
	}

	// (c) whatever is still running at the grace deadline is force-closed.
	select {
	case at := <-inflClosed:
		d := at.Sub(sent)
		if d < sigtermGrace-5*time.Second || d > sigtermGrace+5*time.Second {
			t.Errorf("the in-flight stream ended %s after SIGTERM; the measured grace on v239/v247 is %s. "+
				"A cell unit's TimeoutStopSec is sized against this number.", d, sigtermGrace)
		}
	case <-time.After(sigtermGrace + 15*time.Second):
		t.Errorf("the in-flight stream was still open %s after SIGTERM; llama-swap used to force-close at %s", sigtermGrace+15*time.Second, sigtermGrace)
	}
}

// ─── an upstream whose readiness and pacing are the parameters ──────────────

type fakeUpstreamConfig struct {
	// readyAfter keeps /health answering 503 for this long. Zero means
	// ready immediately.
	readyAfter time.Duration
	chunks     int
	chunkEvery time.Duration
}

type fakeUpstream struct {
	*httptest.Server
	passed atomic.Bool
}

func (u *fakeUpstream) healthChecksPassed() bool { return u.passed.Load() }

func newFakeUpstream(t *testing.T, cfg fakeUpstreamConfig) *fakeUpstream {
	t.Helper()
	u := &fakeUpstream{}
	start := time.Now()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if time.Since(start) < cfg.readyAfter {
			http.Error(w, "loading", http.StatusServiceUnavailable)
			return
		}
		u.passed.Store(true)
		fmt.Fprint(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"object":"list","data":[{"id":"conformance","object":"model"}]}`)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flush, _ := w.(http.Flusher)
		for i := range cfg.chunks {
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"tok%d \"}}]}\n\n", i)
			if flush != nil {
				flush.Flush()
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(cfg.chunkEvery):
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flush != nil {
			flush.Flush()
		}
	})
	u.Server = httptest.NewServer(mux)
	t.Cleanup(u.Close)
	return u
}

// ─── a llama-swap process pointed at it ─────────────────────────────────────

type swapConfig struct {
	proxy            string
	sendLoadingState bool
}

type swapProc struct {
	url  string
	cmd  *exec.Cmd
	done chan struct{}
}

// startSwap runs a real llama-swap whose one model proxies to url. The
// model's `cmd` is a sleep: llama-swap requires one, but the process under
// test here is llama-swap, and an upstream it can kill would answer B2's
// question for it.
func startSwap(t *testing.T, bin string, cfg swapConfig) *swapProc {
	t.Helper()
	dir := t.TempDir()
	port := freePort(t)
	yaml := fmt.Sprintf(`
healthCheckTimeout: 60
logLevel: info
sendLoadingState: %v
models:
  conformance:
    proxy: %s
    cmd: sleep 120
    checkEndpoint: /health
    ttl: 300
`, cfg.sendLoadingState, cfg.proxy)
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cmd := exec.Command(bin, "-config", cfgPath, "-listen", fmt.Sprintf("127.0.0.1:%d", port)) //nolint:gosec // bin is operator-supplied test configuration
	// A FILE, not the testWriter live_test.go uses. An io.Writer makes
	// os/exec build a pipe, the model's `sleep` inherits its write end, and
	// Wait then blocks until the sleep exits — long after llama-swap is
	// gone. That reads as a hung assertion rather than as a held
	// descriptor, which is the most expensive kind of test bug.
	logPath := filepath.Join(dir, "llama-swap.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("create log: %v", err)
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start llama-swap: %v", err)
	}
	t.Cleanup(func() {
		_ = logFile.Close()
		if t.Failed() {
			b, _ := os.ReadFile(logPath)
			t.Logf("llama-swap log:\n%s", b)
		}
	})
	// done is CLOSED, never sent to: cleanup and terminate both wait on it,
	// and a buffered send lets whichever one runs first consume the only
	// value and hang the other.
	sw := &swapProc{url: fmt.Sprintf("http://127.0.0.1:%d", port), cmd: cmd, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(sw.done) }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGKILL)
		<-sw.done
	})

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, sw.url+"/running", nil)
		resp, err := http.DefaultClient.Do(req)
		cancel()
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return sw
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("llama-swap did not answer /running on %s", sw.url)
	return nil
}

// terminate signals llama-swap and requires it to actually exit: a
// shutdown path that hangs is its own incident.
func (s *swapProc) terminate(t *testing.T) {
	t.Helper()
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	go func() {
		select {
		case <-s.done:
		case <-time.After(sigtermGrace + 20*time.Second):
			_ = s.cmd.Process.Signal(syscall.SIGKILL)
		}
	}()
}

func (s *swapProc) stream(t *testing.T, body string) (*bufio.Reader, context.CancelFunc) {
	t.Helper()
	return s.do(t, http.MethodPost, "/v1/chat/completions", body)
}

func (s *swapProc) get(t *testing.T, path string) (*bufio.Reader, context.CancelFunc) {
	t.Helper()
	return s.do(t, http.MethodGet, path, "")
}

func (s *swapProc) do(t *testing.T, method, path, body string) (*bufio.Reader, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	var req *http.Request
	var err error
	if rdr != nil {
		req, err = http.NewRequestWithContext(ctx, method, s.url+path, rdr)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, s.url+path, nil)
	}
	if err != nil {
		cancel()
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// A transport of its own: DefaultClient is shared with the rest of the
	// suite, and a connection pooled from a llama-swap that is about to be
	// signalled has no business being reused.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return bufio.NewReader(resp.Body), cancel
}

// readUntilEOF drains r and reports WHEN it ended.
func readUntilEOF(r *bufio.Reader) <-chan time.Time {
	closed := make(chan time.Time, 1)
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := r.Read(buf); err != nil {
				closed <- time.Now()
				return
			}
		}
	}()
	return closed
}
