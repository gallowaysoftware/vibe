// Package swaptest_test holds the llama-swap CONFORMANCE suite: one
// assertion list, run against every wire version of the double AND — when
// the environment supplies one — against a real llama-swap binary.
//
// That two-target shape is the structural answer to "who checks the
// checker". The double is not authoritative; the assertion list is, and the
// real binary is a peer target of the same list. A fake that passes an
// assertion the real thing fails cannot ship green.
//
// Every assertion here is SEMANTIC, never a byte golden. Between v239 and
// v247 upstream added elapsed_ms, resp_bytes, req_headers and metadata to
// the in-flight entry, all harmless; a golden would have flapped on each
// and been -update'd without being read, which is worse than no test.
package swaptest_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/swaptest"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/usagemeter"
)

// target is something that speaks llama-swap. The suite knows nothing else
// about it, which is what lets the fake and the binary share one list.
type target struct {
	name  string
	url   string
	model string
}

// targets enumerates every wire of the double plus the live binary when the
// environment configures one (see live_test.go).
func targets(t *testing.T) []target {
	t.Helper()
	var out []target
	for _, w := range swaptest.Wires() {
		c := swaptest.NewCell(t,
			swaptest.WithWire(w),
			swaptest.WithChatDelay(120*time.Millisecond),
			swaptest.WithModels(
				swaptest.Model{ID: "chat-model", State: "ready", TTL: 1800},
				swaptest.Model{ID: "embed-model", State: "ready"},
			),
		)
		out = append(out, target{name: "fake/" + w.String(), url: c.URL(), model: "chat-model"})
	}
	if lt, ok := liveTarget(t); ok {
		out = append(out, lt)
	}
	return out
}

func TestSwapContract(t *testing.T) {
	for _, tgt := range targets(t) {
		t.Run(tgt.name, func(t *testing.T) {
			t.Run("I1_start_edge_is_visible_as_in_flight", func(t *testing.T) { i1(t, tgt) })
			t.Run("I2_negative_token_sentinels_never_sum", func(t *testing.T) { i2(t, tgt) })
			t.Run("I3_streaming_rows_carry_tokens", func(t *testing.T) { i3(t, tgt) })
			t.Run("I4_refused_requests_still_log_a_row", func(t *testing.T) { i4(t, tgt) })
			t.Run("I5_connect_delivers_current_state", func(t *testing.T) { i5(t, tgt) })
		})
	}
}

// I1 is the assertion this whole package exists for. A frame announcing
// that a request has STARTED must make fleetd's own fold report at least
// one in flight, with the count REPORTED.
//
// Against a v247-shaped stream the pre-fix fold answered (0, reported) —
// a positive claim of idleness — and eight busy guards across drain,
// suspend, probe and the two warm loops disarm on exactly that value.
func i1(t *testing.T, tgt target) {
	srv, cell := watchTarget(t, tgt)
	defer srv.Close()
	waitFor(t, 10*time.Second, "connect snapshot", func() bool {
		_, reported := srv.InFlight(cell)
		return reported
	})

	var maxSeen int
	var sawReported bool
	var mu sync.Mutex
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			n, reported := srv.InFlight(cell)
			mu.Lock()
			if n > maxSeen {
				maxSeen = n
			}
			if reported {
				sawReported = true
			}
			mu.Unlock()
			time.Sleep(time.Millisecond)
		}
	}()

	drive(t, tgt, false)
	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if !sawReported {
		t.Fatal("the in-flight count was never REPORTED; every busy guard is refusing")
	}
	if maxSeen < 1 {
		t.Fatalf("a request ran and the peak in-flight count fleetd saw was %d — the start edge is invisible to this build, and a reported zero is what disarms every busy guard", maxSeen)
	}
}

// I2: cache_tokens (and draft_tokens, and draft_acc_tokens) may be -1 for
// "not reported", and -1 must never reach a sum. Asserted through the real
// classifier, over rows the target actually produced.
func i2(t *testing.T, tgt target) {
	drive(t, tgt, false)
	driveEmbed(t, tgt)
	rows := activityRows(t, tgt, 20)
	if len(rows) == 0 {
		t.Fatal("no activity rows; the target logs nothing and every C7a assertion below is vacuous")
	}
	sawSentinel := false
	for _, r := range rows {
		if r.Tokens.CacheTokens < 0 || r.Tokens.DraftTokens < 0 || r.Tokens.DraftAccTokens < 0 {
			sawSentinel = true
		}
		_, c := usagemeter.Classify(asMeterRow(r))
		if c.InFresh < 0 || c.InCached < 0 || c.Out < 0 || c.BusyMS < 0 {
			t.Errorf("row %d (%s) classified to a negative contribution: %+v", r.ID, r.ReqPath, c)
		}
	}
	if !sawSentinel {
		t.Logf("note: no -1 sentinel appeared in %d rows; the clamp is untested on this target", len(rows))
	}
}

// I3: a streaming response still carries full token counts. llama-swap
// parses llama.cpp's timings block out of the stream, so no stream_options
// cooperation is needed — and a fake that zeroes tokens on
// text/event-stream teaches the opposite of what production does.
func i3(t *testing.T, tgt target) {
	drive(t, tgt, true)
	// The row is written after the response completes, so give it a moment
	// rather than reading once and skipping — a skip that is really a race
	// is an invariant quietly not checked.
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, r := range activityRows(t, tgt, 20) {
			if r.RespContentType != "text/event-stream" || r.RespStatusCode != 200 {
				continue
			}
			if r.Tokens.InputTokens <= 0 && r.Tokens.OutputTokens <= 0 {
				t.Fatalf("streaming row %d carries no tokens (%+v); C7a would bill it as unmeasured", r.ID, r.Tokens)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Skip("no 200 text/event-stream row on this target")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// I4: a refused request still produces an activity row naming the error.
//
// The recording is why this asserts on error_msg and the status alone. The
// evaluation that motivated this suite claimed duration_ms > 0 on such
// rows, citing a 400 that took 48ms; the row actually captured off v239
// here (a 404, "no router for requested model") has duration_ms 0, because
// nothing upstream was ever dialled. Asserting the stronger thing would
// have shipped a suite that fails on real data.
func i4(t *testing.T, tgt target) {
	before := headID(t, tgt)
	postJSON(t, tgt.url+"/v1/chat/completions", `{"model":"swaptest-no-such-model-`+fmt.Sprint(time.Now().UnixNano())+`"}`)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, r := range activityRows(t, tgt, 10) {
			if r.ID <= before || r.RespStatusCode < 400 {
				continue
			}
			if r.ErrorMsg == "" {
				t.Fatalf("row %d is a %d with no error_msg; C7a's err_req counter has nothing to name", r.ID, r.RespStatusCode)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("a refused request produced no activity row above the head id")
}

// I5: a fresh /api/events connection delivers a current-state in-flight
// frame. Measured at ~198ms on the real v239 (logData, logData,
// modelStatus, inflight — the whole burst). It is what makes fleetapi's
// "no inflight frame seen yet" branch a sub-second window rather than a
// state a cell sits in, and it is what makes clearing the count on a
// stream drop safe.
func i5(t *testing.T, tgt target) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, tgt.url+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/events: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 32<<20)
	var data []string
	for sc.Scan() {
		line := sc.Text()
		if line != "" {
			if rest, ok := strings.CutPrefix(line, "data:"); ok {
				data = append(data, strings.TrimPrefix(rest, " "))
			}
			continue
		}
		var env struct {
			Type string `json:"type"`
			Data string `json:"data"`
		}
		payload := strings.Join(data, "\n")
		data = nil
		if json.Unmarshal([]byte(payload), &env) != nil || env.Type != "inflight" {
			continue
		}
		var inner map[string]json.RawMessage
		if err := json.Unmarshal([]byte(env.Data), &inner); err != nil {
			t.Fatalf("inflight data is not double-encoded JSON: %v (%q)", err, env.Data)
		}
		_, hasRequests := inner["requests"]
		op := ""
		if raw, ok := inner["operation"]; ok {
			_ = json.Unmarshal(raw, &op)
		}
		if !hasRequests && op == "" {
			t.Fatalf("the connect inflight frame names neither requests nor an operation: %q", env.Data)
		}
		if op != "" && op != "snapshot" {
			t.Fatalf("the first inflight frame after a connect is %q, not a current-state snapshot; a consumer that reconnects has nothing to re-seed from", op)
		}
		return
	}
	t.Fatal("no inflight frame arrived on a fresh /api/events connection within the budget")
}

// ─── plumbing ───────────────────────────────────────────────────────────────

// watchTarget points a REAL fleetapi.Server at the target. The suite folds
// frames through production code rather than re-implementing the parser —
// a test-file copy of a predicate is how this repo previously hid a
// disabled production one.
func watchTarget(t *testing.T, tgt target) (*fleetapi.Server, string) {
	t.Helper()
	const cell = "conformance"
	srv := fleetapi.New(
		[]fleetapi.Cell{{Name: cell, URL: tgt.url}},
		t.TempDir()+"/history.json",
		func() fleetapi.DaemonInfo { return fleetapi.DaemonInfo{} },
		fleetapi.Options{},
	)
	srv.Start()
	return srv, cell
}

func waitFor(t *testing.T, budget time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", budget, what)
}

func drive(t *testing.T, tgt target, stream bool) {
	t.Helper()
	body := fmt.Sprintf(`{"model":%q,"max_tokens":8,"stream":%v,"messages":[{"role":"user","content":"hi"}]}`, tgt.model, stream)
	postJSON(t, tgt.url+"/v1/chat/completions", body)
}

func driveEmbed(t *testing.T, tgt target) {
	t.Helper()
	postJSON(t, tgt.url+"/v1/embeddings", `{"model":"embed-model","input":"hello"}`)
}

func postJSON(t *testing.T, url, body string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	_, _ = bufio.NewReader(resp.Body).WriteTo(discard{})
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// activityRows decodes into swaptest.Row rather than
// usagemeter.ActivityRow on purpose: the production struct omits
// resp_content_type and error_msg, so decoding through it would make I3
// and I4 structurally unable to see the fields they are about. Reading the
// wire and converting is the only way a test can notice a field the
// production type does not model.
func activityRows(t *testing.T, tgt target, limit int) []swaptest.Row {
	t.Helper()
	url := fmt.Sprintf("%s/api/metrics/activity?sort=id&order=desc&limit=%d&page=1", tgt.url, limit)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	var p struct {
		Data       []swaptest.Row `json:"data"`
		Page       int            `json:"page"`
		Limit      int            `json:"limit"`
		Total      int            `json:"total"`
		TotalPages int            `json:"total_pages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("decode activity page: %v", err)
	}
	if p.Page != 1 || p.Limit != limit {
		t.Errorf("page/limit echoed back as %d/%d, want 1/%d", p.Page, p.Limit, limit)
	}
	return p.Data
}

func asMeterRow(r swaptest.Row) usagemeter.ActivityRow {
	ts, _ := time.Parse(time.RFC3339, r.Timestamp)
	return usagemeter.ActivityRow{
		ID: r.ID, Timestamp: ts, Model: r.Model, ReqPath: r.ReqPath,
		RespStatusCode: r.RespStatusCode, DurationMs: r.DurationMs,
		Tokens: usagemeter.Tokens{
			CacheTokens: r.Tokens.CacheTokens, DraftTokens: r.Tokens.DraftTokens,
			DraftAccTokens: r.Tokens.DraftAccTokens, InputTokens: r.Tokens.InputTokens,
			OutputTokens:    r.Tokens.OutputTokens,
			PromptPerSecond: r.Tokens.PromptPerSecond, TokensPerSecond: r.Tokens.TokensPerSecond,
		},
	}
}

func headID(t *testing.T, tgt target) int64 {
	t.Helper()
	rows := activityRows(t, tgt, 1)
	if len(rows) == 0 {
		return 0
	}
	return rows[0].ID
}
