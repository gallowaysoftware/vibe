package modelprobe

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
)

// U6 — the model probe's deadlines.
//
// The package's existing tests cover every REFUSAL (not resident,
// cooldown, cap, single-flight, disabled) and every SCORING rule, and
// every one of them is answered instantly by an httptest handler. What
// none of them exercise is the third outcome: a llama-swap that accepts
// the connection and then says nothing. That is not a hypothetical shape
// — it is the exact failure the phase exists for. A llama-server wedged
// on a CUDA fault holds the socket open; it does not refuse it.
//
// Two deadlines stand between that and a hung cell, and until this file
// neither had a test that made it elapse:
//
//   - probeTimeout (3m), the end-to-end bound on one probe;
//   - runningTimeout (5s), the bound on the residency read inside it.
//
// The tests below take two forms deliberately, because one alone proves
// half of it:
//
//   - VALUE tests read the deadline off the request at the transport and
//     assert what it is. They pin the constants themselves, which no
//     behavioural test can do in milliseconds.
//   - ELAPSE tests let a deadline actually fire (in ~150ms, via a shorter
//     caller deadline or a shorter client timeout down the identical
//     code path) and assert what the prober DOES about it.
//
// The load-bearing claim of the second group is the one this repo has got
// wrong six times: a request that never answered is not an answer.

// ── fixtures ────────────────────────────────────────────────────────────

// u6Swap is a llama-swap stand-in whose handlers can be held open. It is
// separate from modelprobe_test.go's `swap` for one reason: this file
// needs to block /running, which is the request that fixture always
// answers instantly.
type u6Swap struct {
	mu sync.Mutex
	// running is the /running answer: model → state.
	running map[string]string
	// tokPerSec is what the chat response's timings block reports.
	tokPerSec float64

	runningCalls atomic.Int64
	chatCalls    atomic.Int64

	// hold, when non-nil for a handler, keeps it inside the handler until
	// the channel is closed. Reading them under mu (rather than the plain
	// field read the sibling fixture uses) so a test may start holding
	// after the server is already serving.
	holdRunning chan struct{}
	holdChat    chan struct{}
}

func newU6Swap(t *testing.T, running map[string]string) (*u6Swap, *httptest.Server) {
	t.Helper()
	s := &u6Swap{running: running, tokPerSec: 40}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /running", func(w http.ResponseWriter, r *http.Request) {
		s.runningCalls.Add(1)
		s.mu.Lock()
		hold := s.holdRunning
		// The FULL live shape, not the two fields the decoder reads. The
		// box this was written on answers `{"running":[]}` with rows that
		// also carry ttl/name/cmd/proxy (fleetapi's fixtures agree), and a
		// fixture trimmed to what the parser wants cannot catch a parser
		// that stopped tolerating the rest.
		rows := make([]string, 0, len(s.running))
		for m, st := range s.running {
			rows = append(rows, fmt.Sprintf(
				`{"model":%q,"state":%q,"ttl":300,"name":%q,"cmd":"llama-server -m /models/%s.gguf","proxy":"http://127.0.0.1:9101"}`,
				m, st, m, m))
		}
		s.mu.Unlock()
		if hold != nil {
			// Held INSIDE the handler: the connection is accepted and the
			// request is read, and then nothing comes back. A refused
			// connection would take a different path in net/http and prove
			// nothing about a deadline.
			<-hold
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"running":[%s]}`, strings.Join(rows, ","))
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		s.chatCalls.Add(1)
		s.mu.Lock()
		hold, tps := s.holdChat, s.tokPerSec
		s.mu.Unlock()
		if hold != nil {
			<-hold
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"usage":{"completion_tokens":%d},"timings":{"predicted_n":%d,"predicted_ms":%f,"predicted_per_second":%f,"prompt_ms":120}}`,
			probeMaxTokens, probeMaxTokens, float64(probeMaxTokens)/tps*1000, tps)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return s, srv
}

// holdOpen starts holding a handler and returns the release. Registered
// with Cleanup as well so a failing assertion cannot leave a handler
// goroutine parked on a channel nobody closes.
func (s *u6Swap) holdOpen(t *testing.T, which string) (release func()) {
	t.Helper()
	ch := make(chan struct{})
	s.mu.Lock()
	switch which {
	case "running":
		s.holdRunning = ch
	case "chat":
		s.holdChat = ch
	default:
		s.mu.Unlock()
		t.Fatalf("holdOpen: unknown handler %q", which)
	}
	s.mu.Unlock()
	var once sync.Once
	release = func() {
		once.Do(func() {
			s.mu.Lock()
			switch which {
			case "running":
				s.holdRunning = nil
			case "chat":
				s.holdChat = nil
			}
			s.mu.Unlock()
			close(ch)
		})
	}
	t.Cleanup(release)
	return release
}

func (s *u6Swap) setRunning(model, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running[model] = state
}

// seen is one request as the transport saw it: which endpoint, and how
// much time the context it carried had left.
type seen struct {
	path        string
	hasDeadline bool
	remaining   time.Duration
}

// deadlineRT records the per-request deadline on the way past. This is
// the seam that makes a 3-minute constant assertable in a millisecond:
// the deadline is a VALUE on the outgoing request, so it can be read
// without waiting for it.
type deadlineRT struct {
	next http.RoundTripper
	mu   sync.Mutex
	got  []seen
}

func (d *deadlineRT) RoundTrip(r *http.Request) (*http.Response, error) {
	s := seen{path: r.URL.Path}
	if dl, ok := r.Context().Deadline(); ok {
		s.hasDeadline, s.remaining = true, time.Until(dl)
	}
	d.mu.Lock()
	d.got = append(d.got, s)
	d.mu.Unlock()
	return d.next.RoundTrip(r)
}

func (d *deadlineRT) find(path string) (seen, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, s := range d.got {
		if s.path == path {
			return s, true
		}
	}
	return seen{}, false
}

// u6Prober wires a prober against srv with the given client.
func u6Prober(t *testing.T, srv *httptest.Server, hc *http.Client, clk *clock) *Prober {
	t.Helper()
	p, err := New(Config{
		LlamaSwapURL: srv.URL,
		StatePath:    filepath.Join(t.TempDir(), "model-probe.json"),
		HTTPClient:   hc,
		Now:          clk.now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// waitFor polls until cond or the (generous, CI-safe) deadline passes.
func waitFor(t *testing.T, why string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", why)
}

// ── value: what bound does each request actually carry ──────────────────

// TestProbeDeadlines_EveryProbeRequestCarriesItsOwnBound is the VALUE
// half. It reads the deadline off each outgoing request and asserts the
// two constants are real and nested.
//
// Nested is the part worth stating. probeTimeout is deliberately generous
// (a model degraded 100x still answers inside it), and the residency read
// is a localhost GET that does no model work at all. If /running inherited
// the probe budget, a llama-swap that accepted connections and stopped
// answering would hold this cell's single-flight lock for three minutes
// per command — refusing every other model's probe — to learn nothing.
// The inner bound is what turns that into five seconds.
//
// Both entry points are checked. Start's used to carry the bound and Run
// used to have none, which left `vibe model try measure` (C18 calls Run
// directly, with a client that has no timeout of its own) with nothing
// bounding it at all.
func TestProbeDeadlines_EveryProbeRequestCarriesItsOwnBound(t *testing.T) {
	for _, tc := range []struct {
		name  string
		enter func(p *Prober)
	}{
		{"Run", func(p *Prober) { p.Run(context.Background(), "qwen", false) }},
		{"Start", func(p *Prober) { p.Start("qwen", false) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sw, srv := newU6Swap(t, map[string]string{"qwen": "ready"})
			rt := &deadlineRT{next: srv.Client().Transport}
			p := u6Prober(t, srv, &http.Client{Transport: rt}, testClock())

			tc.enter(p)
			// Start is asynchronous; Run is not. Waiting on the counter
			// covers both without a sleep.
			waitFor(t, "the probe to reach both endpoints", func() bool {
				return sw.chatCalls.Load() > 0
			})
			// Join the background probe (Start's) before asserting: its
			// state write must land before the temp dir goes, and Results
			// is only populated after record() has saved.
			waitFor(t, "the probe to finish recording", func() bool {
				_, ok := rt.find("/v1/chat/completions")
				return ok && p.Results()["qwen"] != nil
			})

			running, ok := rt.find("/running")
			if !ok {
				t.Fatal("no /running request: the residency read is the one thing that must always happen first")
			}
			if !running.hasDeadline {
				t.Fatal("the /running read carried NO deadline: a llama-swap that accepts the connection and never answers would park the prober forever")
			}
			// The numbers are WRITTEN OUT rather than referred to the
			// constants they check. A first draft said
			// `remaining > runningTimeout`, which is a tautology: widening
			// runningTimeout to 60s moved the assertion with it and the
			// mutation survived. An assertion about a constant has to spell
			// the constant.
			//
			// Bounds, not equality, because the clock moves between
			// WithTimeout and RoundTrip — wide enough not to flake on a
			// loaded box, tight enough that any other value fails.
			if running.remaining > 5*time.Second || running.remaining < 3*time.Second {
				t.Fatalf("/running carried %s of budget, want ~5s (runningTimeout). "+
					"Longer and a llama-swap that accepts connections and never answers holds this cell's "+
					"only probe slot for the whole probe budget; shorter and a busy box's own localhost read starts failing", running.remaining)
			}

			chat, ok := rt.find("/v1/chat/completions")
			if !ok {
				t.Fatal("no completion request recorded")
			}
			if !chat.hasDeadline {
				t.Fatal("the probe request carried NO deadline: probeTimeout is documented as bounding one probe end to end, and this entry point does not apply it")
			}
			// Spelled out for the same reason as above.
			if chat.remaining > 3*time.Minute || chat.remaining < 150*time.Second {
				t.Fatalf("the probe request carried %s of budget, want ~3m (probeTimeout). "+
					"It is generous on purpose: 64 tokens at 0.5 tok/s is ~128s, and a model degraded 100x "+
					"is the exact case this phase exists to MEASURE rather than to time out on", chat.remaining)
			}

			if running.remaining >= chat.remaining {
				t.Fatalf("the residency read (%s) is not bounded INSIDE the probe (%s): a hung /running would spend the whole probe budget holding the single-flight lock",
					running.remaining, chat.remaining)
			}
		})
	}
}

// TestProbeDeadlines_ACallerWithLessTimeKeepsIt: Run takes the MINIMUM of
// probeTimeout and whatever the caller brought. `vibe model try` runs
// under a command context an operator can ^C, and a Run that replaced
// that context's deadline with its own three minutes would ignore it.
func TestProbeDeadlines_ACallerWithLessTimeKeepsIt(t *testing.T) {
	sw, srv := newU6Swap(t, map[string]string{"qwen": "ready"})
	rt := &deadlineRT{next: srv.Client().Transport}
	p := u6Prober(t, srv, &http.Client{Transport: rt}, testClock())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	p.Run(ctx, "qwen", false)

	if sw.chatCalls.Load() != 1 {
		t.Fatalf("chat calls = %d, want 1", sw.chatCalls.Load())
	}
	chat, ok := rt.find("/v1/chat/completions")
	if !ok || !chat.hasDeadline {
		t.Fatal("no deadline on the probe request")
	}
	if chat.remaining > 2*time.Second {
		t.Fatalf("the caller's 2s deadline was widened to %s: Run must take the minimum, never the maximum", chat.remaining)
	}
	// And the inner bound still applies under a shorter outer one.
	running, ok := rt.find("/running")
	if !ok || !running.hasDeadline {
		t.Fatal("no deadline on the residency read")
	}
	if running.remaining > 2*time.Second {
		t.Fatalf("the residency read carried %s under a 2s caller deadline", running.remaining)
	}
}

// ── elapse: what the prober does when a deadline actually fires ─────────

// TestProbeDeadlines_AResidencyReadThatNeverAnswersIsNotAResidencyVerDict
// is THE hazard of this unit, and the one C20 class that keeps recurring
// here: absent evidence read as a definite value.
//
// isResident returns (bool, error). The error carries the whole
// distinction — `false, nil` is "llama-swap answered, and the model is
// not there"; `false, err` is "llama-swap did not answer". A caller that
// keeps only the bool reports the second as the first, and the first has
// a specific, load-bearing meaning in this phase: it is the note that
// says the cardinal rule fired. An operator reading "not resident — warm
// it first" acts on it. Printing that because a localhost GET timed out
// sends them to warm a model that was already warm.
//
// The elapse is the caller's 150ms rather than runningTimeout's 5s; it is
// the same context deadline down the same line of code, and it keeps the
// test in milliseconds. The VALUE test above is what pins the 5s itself.
func TestProbeDeadlines_AResidencyReadThatNeverAnswersIsNotAResidencyVerdict(t *testing.T) {
	sw, srv := newU6Swap(t, map[string]string{"qwen": "ready"})
	release := sw.holdOpen(t, "running")
	clk := testClock()
	p := u6Prober(t, srv, srv.Client(), clk)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	res := p.Run(ctx, "qwen", false)

	if res == nil {
		t.Fatal("a probe that could not read /running recorded nothing at all")
	}
	if !strings.Contains(res.Note, "cannot read local llama-swap /running") {
		t.Fatalf("note = %q; a residency read that never answered must say so", res.Note)
	}
	if strings.Contains(res.Note, "not resident") {
		t.Fatalf("note = %q: a /running that TIMED OUT was reported as a residency verdict. "+
			"That is the absent-evidence-as-a-value defect: nothing was checked, and the operator is being told the model is missing", res.Note)
	}
	// The cardinal rule holds through the failure path too: no answer is
	// not permission.
	if n := sw.chatCalls.Load(); n != 0 {
		t.Fatalf("issued %d completions without a residency answer; a probe must never be the thing that loads a model", n)
	}

	// ...and the failure cost nothing. Two consequences, one assertion
	// each, because they are separate mistakes:
	//
	// (a) it was not billed as an attempt — an unanswered read spent no
	//     GPU time, and keying the cooldown on it would let a wedged
	//     llama-swap lock this model out of probing for five minutes per
	//     command;
	// (b) the single-flight lock was released on the inner bound, not
	//     held for the whole probe budget.
	release()
	second := p.Run(context.Background(), "qwen", false)
	if strings.Contains(second.Note, "cooldown") {
		t.Fatalf("the unanswered read started the cooldown clock: %q", second.Note)
	}
	if strings.Contains(second.Note, "already running") {
		t.Fatalf("the single-flight lock outlived the failed read: %q", second.Note)
	}
	if n := sw.chatCalls.Load(); n != 1 {
		t.Fatalf("the retry ran %d probes, want 1: the prober did not recover from a timed-out residency read", n)
	}
	if second.Value <= 0 {
		t.Fatalf("the recovered probe measured nothing: %+v", second)
	}
}

// TestProbeDeadlines_AResidencyRefusalStillReadsAsNotResident is the
// other side of the same coin, and the reason the test above cannot
// simply assert "never say not resident". When llama-swap DOES answer and
// the model is genuinely absent, the note must still be the cardinal
// rule's. A fix that turned every negative into "cannot read" would pass
// the hazard test and destroy the phase's central refusal.
func TestProbeDeadlines_AResidencyRefusalStillReadsAsNotResident(t *testing.T) {
	sw, srv := newU6Swap(t, map[string]string{})
	p := u6Prober(t, srv, srv.Client(), testClock())

	res := p.Run(context.Background(), "qwen", false)
	if !strings.Contains(res.Note, "not resident") {
		t.Fatalf("note = %q; an ANSWERED /running without the model is the residency refusal", res.Note)
	}
	if strings.Contains(res.Note, "cannot read") {
		t.Fatalf("note = %q: an answered read was reported as an unreadable one", res.Note)
	}
	if n := sw.chatCalls.Load(); n != 0 {
		t.Fatalf("issued %d completions for a non-resident model", n)
	}
	if sw.runningCalls.Load() == 0 {
		t.Fatal("no /running read happened at all")
	}
}

// TestProbeDeadlines_AnAbandonedProbeIsAFailedAttemptAndNeverAMeasurement
// is the second thing probeTimeout's own comment promises: past it "the
// probe is abandoned and recorded as a failed attempt". Three separate
// claims live in that sentence, and each has its own way of going wrong:
//
//   - it counts as an ATTEMPT. The attempt is marked before the request
//     because an abandoned probe spent the GPU time anyway; marking it
//     after would make a timing-out model the CHEAPEST thing to probe and
//     let a wedged box retry it every interval forever, against neither
//     the cooldown nor the 96/day cap.
//   - it never enters the baseline. A timeout is not a slow measurement:
//     there is no number, and a zero would drag the median down and then
//     re-base the model onto its own failure.
//   - it never publishes a rate. `0 tok/s` in fleet_status is a claim
//     about the model; "the probe did not answer" is a claim about the
//     probe, and only the second one is true.
//
// Both elapse mechanisms are covered. They reach p.http.Do's error return
// by different routes — a cancelled context vs. the client's own timer —
// and they are the two ways this can happen in production (probeTimeout
// itself, and an operator-configured client timeout).
func TestProbeDeadlines_AnAbandonedProbeIsAFailedAttemptAndNeverAMeasurement(t *testing.T) {
	for _, tc := range []struct {
		name   string
		client func(srv *httptest.Server) *http.Client
		ctx    func(t *testing.T) context.Context
	}{
		{
			name:   "context deadline (probeTimeout's own mechanism)",
			client: func(srv *httptest.Server) *http.Client { return srv.Client() },
			ctx: func(t *testing.T) context.Context {
				ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
				t.Cleanup(cancel)
				return ctx
			},
		},
		{
			name: "client timeout",
			client: func(srv *httptest.Server) *http.Client {
				c := *srv.Client()
				c.Timeout = 150 * time.Millisecond
				return &c
			},
			ctx: func(*testing.T) context.Context { return context.Background() },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sw, srv := newU6Swap(t, map[string]string{"qwen": "ready"})
			clk := testClock()
			p := u6Prober(t, srv, tc.client(srv), clk)

			// A real baseline first, so "did the timeout enter it" is
			// answerable.
			var healthy *fleetapi.AnnounceProbe
			for range minSamples + 1 {
				clk.advance(minGap)
				healthy = p.Run(context.Background(), "qwen", false)
			}
			if healthy.Verdict != fleetapi.VerdictOK {
				t.Fatalf("setup: want a healthy baseline, got %+v", healthy)
			}
			p50Before, samplesBefore := healthy.BaselineP50, healthy.Samples

			release := sw.holdOpen(t, "chat")
			clk.advance(minGap)
			timedOut := p.Run(tc.ctx(t), "qwen", false)

			if !strings.Contains(timedOut.Note, "probe request failed") {
				t.Fatalf("note = %q; an abandoned probe must be recorded with its reason", timedOut.Note)
			}
			// A timeout is not a measurement of zero. The block keeps the
			// last real number (note's carry-forward rule) rather than
			// publishing a rate nobody measured.
			if timedOut.Value != healthy.Value || timedOut.Metric != healthy.Metric {
				t.Fatalf("an abandoned probe published %v %s, want the last real measurement (%v %s) carried forward: a timeout is not a slow measurement",
					timedOut.Value, timedOut.Metric, healthy.Value, healthy.Metric)
			}

			// (1) It was billed. The cooldown is the observable proof: it
			// keys on the last ATTEMPT.
			refused := p.Run(context.Background(), "qwen", false)
			if !strings.Contains(refused.Note, "cooldown") {
				t.Fatalf("an abandoned probe did not count as an attempt (next note %q): a timing-out model would be free to re-probe every interval, against neither the cooldown nor the daily cap", refused.Note)
			}

			// (2) It never entered the baseline.
			release()
			clk.advance(minGap)
			after := p.Run(context.Background(), "qwen", false)
			if after.Samples != samplesBefore+1 {
				t.Fatalf("baseline holds %d samples after one healthy probe past the timeout, want %d: the abandoned probe was scored", after.Samples, samplesBefore+1)
			}
			if after.BaselineP50 < p50Before*0.9 {
				t.Fatalf("baseline p50 fell from %v to %v across a timeout: an abandoned probe was recorded as a slow one", p50Before, after.BaselineP50)
			}
			if after.Verdict != fleetapi.VerdictOK {
				t.Fatalf("a healthy probe after a timeout reads %q: %+v", after.Verdict, after)
			}
		})
	}
}

// TestProbeDeadlines_AHungProbeStillReleasesTheCell: the single-flight
// lock is held for the whole of Run, so a probe that hangs holds the
// cell's only probe slot. probeTimeout is what puts a ceiling on that;
// without it, one wedged model would make every OTHER model on the box
// unprobeable for as long as the socket stayed open.
func TestProbeDeadlines_AHungProbeStillReleasesTheCell(t *testing.T) {
	sw, srv := newU6Swap(t, map[string]string{"qwen": "ready", "other": "ready"})
	release := sw.holdOpen(t, "chat")
	clk := testClock()
	c := *srv.Client()
	c.Timeout = 150 * time.Millisecond
	p := u6Prober(t, srv, &c, clk)

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(context.Background(), "qwen", false)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the probe never gave up on a llama-swap that accepted the request and said nothing")
	}

	// A different model is probeable immediately: the lock came back.
	res := p.Run(context.Background(), "other", false)
	if strings.Contains(res.Note, "already running") {
		t.Fatalf("the abandoned probe still holds the cell: %q", res.Note)
	}
	release()
}

// ── structural: the hazard, pinned where it would be reintroduced ───────

// TestIsResidentsErrorIsNeverDiscarded scans this package for the one
// line that turns "no answer" into "not resident": `ok, _ :=
// p.isResident(...)`.
//
// It is a package-local scan on purpose. isResident is unexported, so the
// package IS the blast radius — and C20's module-wide scans do not reach
// this shape at all: TestNoNewMeasurementAndBoolReturn matches
// (numeric, bool) returns, and `bool` is deliberately not in its
// numericResult table (a comma-ok lookup is fine), so a (bool, error)
// evidence carrier is invisible to it. See the PR for the note to that
// scanner's owner.
//
// Same two meta-guards the rest of this repo's structural tests carry: an
// exemption table where each entry is a written reason, and a floor on
// how much was examined, so a rename cannot make the scan pass over
// nothing.
var isResidentDiscardExempt = map[string]string{}

func TestIsResidentsErrorIsNeverDiscarded(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	calls, dropped, used := 0, 0, map[string]bool{}
	files := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		files++
		ast.Inspect(f, func(n ast.Node) bool {
			var lhs, rhs []ast.Expr
			switch v := n.(type) {
			case *ast.AssignStmt:
				lhs, rhs = v.Lhs, v.Rhs
			case *ast.ValueSpec:
				for _, id := range v.Names {
					lhs = append(lhs, id)
				}
				rhs = v.Values
			default:
				return true
			}
			if len(rhs) != 1 || len(lhs) != 2 {
				return true
			}
			call, ok := rhs[0].(*ast.CallExpr)
			if !ok {
				return true
			}
			// Both spellings: p.isResident(...) and a bare isResident(...)
			// if it ever stops being a method.
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				if fn.Sel.Name != "isResident" {
					return true
				}
			case *ast.Ident:
				if fn.Name != "isResident" {
					return true
				}
			default:
				return true
			}
			calls++
			id, ok := lhs[1].(*ast.Ident)
			if !ok || id.Name != "_" {
				return true
			}
			dropped++
			key := fmt.Sprintf("%s:%d", name, fset.Position(id.Pos()).Line)
			if _, exempt := isResidentDiscardExempt[key]; exempt {
				used[key] = true
				return true
			}
			t.Errorf("%s:%d: discards isResident's error. `false, err` is NO ANSWER and `false, nil` "+
				"is an answered no; dropping the error reports a llama-swap that never replied as "+
				"\"not resident — warm it first\", which is the note that means this phase's cardinal "+
				"rule fired. Handle the error, or add %s to isResidentDiscardExempt with the reason "+
				"absence and absence-of-answer are the same thing there.", name, fset.Position(id.Pos()).Line, key)
			return true
		})
	}
	if files == 0 {
		t.Fatal("scanned no production files — the scan is inert")
	}
	if calls == 0 {
		t.Fatalf("found no isResident call sites across %d files: the residency check was renamed or removed, and this scan now guards nothing", files)
	}
	for key := range isResidentDiscardExempt {
		if !used[key] {
			t.Errorf("exemption %q matches no call: it is STALE. Re-point it or delete it.", key)
		}
	}
}
