package daemon

// The daemon's outbound reads and verb budgets, made to ELAPSE.
//
// Every "unreachable" in this package's existing tests is an immediate
// ECONNREFUSED or a port-1 dial: both fail in microseconds, so not one of
// them ever reaches a timeout. That is the wrong half of the failure
// distribution. A box that is off refuses instantly; a box that is
// wedged — an llama-swap mid-CUDA-fault, a fleetd swapping, a cell whose
// kernel is alive and whose userspace is not — accepts the connection and
// then says nothing, forever. The second kind is what these constants
// exist for, and until this file nothing made one happen.
//
// c20_test.go is the model: it drops a real SSE stream and burns the real
// inflightEvidenceGrace. Everything here follows it — a real server that
// accepts and never answers, a real clock, and a window that fails both
// when the bound is missing and when it is a different bound than the one
// the test names.
//
// Stdlib only, deliberately: every assertion below carries a sentence
// explaining what a failure MEANS, which is this package's house style and
// is worth more here than a shorter line.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetannounce"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	vibev1 "github.com/gallowaysoftware/vibe/proto/vibe/v1"
)

// ─── the wedged far side ────────────────────────────────────────────────────

// hungServer accepts the connection, reads the request, and then answers
// nothing. It is the failure every constant in this unit is written for
// and the one no existing test produces.
//
// The body is drained before parking so the server's own read loop can
// notice the client leaving, and the park watches an explicit release
// channel as well as the request context: httptest's Close WAITS for
// handlers, and a handler that only watches a request context whose
// close-detection never fires turns every one of these tests into a
// five-minute cleanup hang (observed, not theorised).
func hungServer(t *testing.T) *httptest.Server {
	t.Helper()
	var (
		mu  sync.Mutex
		hit int
	)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hit++
		mu.Unlock()
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
		mu.Lock()
		defer mu.Unlock()
		if hit == 0 {
			t.Error("the wedged server was never called: this test proved nothing about any timeout")
		}
	})
	return srv
}

// deadURL is an address nothing listens on — the instant-ECONNREFUSED
// case, used here only to hold one half of a test still while the other
// half is the thing being timed.
func deadURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return "http://" + addr
}

// deadPort is deadURL's port half, for config fields that take one.
func deadPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// wantElapsed fails BOTH ways on purpose. Under lo means the call
// returned before the named budget could have fired, so whatever it
// measured was not this budget; over hi means the operative bound is a
// different (or absent) one — the way an inert guard hides behind a
// larger belt somewhere else.
func wantElapsed(t *testing.T, start time.Time, lo, hi time.Duration, what string) {
	t.Helper()
	el := time.Since(start)
	switch {
	case el < lo:
		t.Fatalf("%s returned after %v, sooner than the %v budget it is supposed to spend — this test is timing something else",
			what, el.Round(time.Millisecond), lo)
	case el > hi:
		t.Fatalf("%s took %v, past the %v ceiling: the bound that ended it is not the one this test names, or nothing armed it",
			what, el.Round(time.Millisecond), hi)
	}
}

// ─── the numbers themselves ─────────────────────────────────────────────────

// TestOutboundBudgets_TheDocumentedValues pins each budget to the number
// its own doc comment argues for.
//
// Every elapse test below is written in terms of these constants, which
// makes the WINDOWS stay honest when a value legitimately changes — and
// makes a changed value invisible to all of them. For a budget the value
// IS the contract: doctor probes every cell in turn, so five seconds is
// the difference between a report a human waits for and one they abandon;
// the announce read is on the heartbeat that proves the cell is alive at
// all. This test is the one place a new number has to be typed
// deliberately, next to the sentence saying what it buys.
func TestOutboundBudgets_TheDocumentedValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  time.Duration
		want time.Duration
		why  string
	}{
		{"cellCmdTimeout", cellCmdTimeout, 60 * time.Second,
			"one systemd unit stop, generously; a box with a slower regime raises cell_cmds.cmd_timeout instead"},
		{"cellCmdWaitDelay", cellCmdWaitDelay, 2 * time.Second,
			"the half of the verb bound the deadline alone does not cover: long enough to still capture a killed command's last stderr line, noise next to a 60s budget"},
		{"doctorAuthTimeout", doctorAuthTimeout, 5 * time.Second,
			"doctor probes EVERY cell serially, so this number times the fleet size is how long a report on a dark fleet takes"},
		{"announceStopTimeout", announceStopTimeout, 3 * time.Second,
			"the shutdown wait for a loop that will not stop: long enough for a heartbeat in flight, short enough that shutdown still means shutdown"},
		{"announceWithdrawTimeout", announceWithdrawTimeout, 5 * time.Second,
			"the goodbye is best-effort (fleetd prunes on staleness anyway), so spending more than this on it buys nothing and costs a shutdown"},
		{"llamaSwapVersionTimeout", llamaSwapVersionTimeout, 2 * time.Second,
			"a wedged llama-swap must cost the announce a blank field, never a missed heartbeat — the cell's only evidence of life"},
		{"residentProbeTimeout", residentProbeTimeout, 3 * time.Second,
			"a 127.0.0.1 read on the path of a verb an operator is waiting on"},
		{"leaseFetchTimeout", leaseFetchTimeout, 3 * time.Second,
			"leases are advisory; a wedged fleetd degrades the pre-drain report and never delays the reclaim"},
		{"intentPostTimeout", intentPostTimeout, 5 * time.Second,
			"the verb has already succeeded and this record is allowed to fail; it may not hold the reply"},
		{"fleetCapacityTimeout", fleetCapacityTimeout, 3 * time.Second,
			"a wedged nvidia-smi costs the heartbeat two numbers, not the heartbeat"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v — %s. Changing it is fine; changing it by accident is not.",
				tc.name, tc.got, tc.want, tc.why)
		}
	}
}

// ─── cellCmdTimeout: the verb budget ────────────────────────────────────────

// TestCellCmdBudget_DefaultsToSixtySecondsAndBadConfigNeverDisarmsIt pins
// the VALUE, which no test could reach while it was a bare constant used
// inline. The interesting rows are the bottom three: a config field that
// fell through to zero on a typo would hand every verb an
// already-expired context, and a box would then report "drain command
// failed" on a perfectly healthy unit with nothing naming the cause.
func TestCellCmdBudget_DefaultsToSixtySecondsAndBadConfigNeverDisarmsIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  string
		want time.Duration
	}{
		{"unset is the documented 60s", "", 60 * time.Second},
		{"a slower regime declares its own", "90s", 90 * time.Second},
		{"a test-scale budget", "150ms", 150 * time.Millisecond},
		{"unparseable falls back, never to zero", "banana", 60 * time.Second},
		{"explicit zero is not a budget", "0s", 60 * time.Second},
		{"negative is not a budget", "-5s", 60 * time.Second},
		{"whitespace is unset", "   ", 60 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := New(Config{CellCmds: CellCmds{Drain: "true", CmdTimeout: tc.cfg}})
			if got := d.cellCmdBudget(); got != tc.want {
				t.Fatalf("cell_cmds.cmd_timeout %q → %v, want %v", tc.cfg, got, tc.want)
			}
		})
	}
}

// TestVerbCtx_CarriesTheBudgetAndOutlivesTheRPC. The verb context is
// detached from the caller (context.WithoutCancel), which is exactly why
// the deadline is load-bearing rather than decorative: a detached context
// has no other end. Both halves in one place so a change that dropped
// either is visible.
func TestVerbCtx_CarriesTheBudgetAndOutlivesTheRPC(t *testing.T) {
	d := New(Config{CellCmds: CellCmds{Drain: "true", CmdTimeout: "150ms"}})

	rpcCtx, cancelRPC := context.WithCancel(context.Background())
	cancelRPC() // the client hung up mid-verb
	vctx, cancel := d.verbCtx(rpcCtx)
	defer cancel()

	if err := vctx.Err(); err != nil {
		t.Fatalf("the verb context died with the RPC (%v): a client disconnecting must not SIGKILL a unit stop in flight", err)
	}
	dl, ok := vctx.Deadline()
	if !ok {
		t.Fatal("the verb context carries NO deadline: detached from the caller and unbounded, a hung `systemctl stop` holds the RPC until the daemon exits")
	}
	if remaining := time.Until(dl); remaining > 150*time.Millisecond || remaining < 100*time.Millisecond {
		t.Fatalf("verb deadline is %v out, want ~150ms: the budget reaching the context is the linkage this pins", remaining)
	}
}

// ─── the verb's command tree ────────────────────────────────────────────────

// forkingVerb is the hung drain command the budget test below runs. %s is
// a path it writes its background child's pid to.
//
// A background job and a `wait`, not the one-word `sleep 30` this test was
// first written with, because that version proved nothing on the machine
// most of this code gets written on: bash turns `sh -c 'sleep 30'` into an
// EXEC, so the single process the deadline kills really is the whole
// command and a budget with no reach past the shell looks armed. dash does
// not, and no shell can for a command with a `;`, a `&&`, a subshell or a
// background job — which is what every real cell_cmds.drain looks like.
// The result was a test that passed on the workstation and failed in CI at
// 30.003s against a 400ms budget.
//
// internal/vibe/shellcmd owns the mechanism and its two failure modes.
// What is left here is this unit's own claim: that the VERB is on it.
const forkingVerb = "sleep 10 & echo $! > %s; wait"

// grandchildPID reads the pid the verb recorded, polling because "the
// first thing the command does" still happens after a fork and an exec.
func grandchildPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, err := os.ReadFile(path); err == nil {
			if pid, cerr := strconv.Atoi(strings.TrimSpace(string(b))); cerr == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the command never recorded a background pid at %s: the forking verb this test depends on did not run, so nothing here measured a budget", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// pidAlive probes for existence with signal 0, the same technique
// vamp.IsAlive uses. EPERM counts as alive: a process owned by someone
// else is still a process.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// pidGoneWithin polls for a pid to disappear rather than looking once: a
// killed process is a zombie until its reaper gets to it, and the shell
// that would have reaped this one was killed alongside it. Measured at
// ~5ms via init on Linux, but it is not zero.
func pidGoneWithin(pid int, limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return !pidAlive(pid)
}

// TestCellDrain_HungVerbSurfacesUnavailableAtTheBudget is the whole
// sentence in cellCmdTimeout's doc comment, executed: "a hung unit stop
// must surface as Unavailable, not a stuck RPC".
//
// No seam. The real runCellCmd, a real forking `sh -c` (a unit stop that
// never returns), and the real kill — because SetCellCmdRunner proves the
// daemon's half of the contract and proves nothing about whether the
// budget reaches the process tree.
//
// Two answers, not one, because the RPC returning is only half of a bound.
// The reply arriving on time while the work runs on is a drain that
// reported failure and left a half-run reclaim on the box, and on the wire
// that is indistinguishable from the budget having worked. It is also what
// goes red if this call site ever leaves shellcmd for a bare
// exec.CommandContext — the registry mutates exactly that.
func TestCellDrain_HungVerbSurfacesUnavailableAtTheBudget(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	d := New(Config{
		ProxyPort: deadPort(t), // the resident probe must not be what we time
		CellCmds: CellCmds{
			Drain:      fmt.Sprintf(forkingVerb, pidFile),
			CmdTimeout: "400ms",
		},
	})

	start := time.Now()
	_, err := d.CellDrain(context.Background(), connect.NewRequest(&vibev1.CellDrainRequest{}))
	if err == nil {
		t.Fatal("a drain command that never returns reported SUCCESS: the caller would record intent for a box it never reclaimed")
	}
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Fatalf("hung drain → %v (%v), want Unavailable: a stuck verb is the cell being unavailable, not the request being malformed", got, err)
	}
	// The ceiling is the budget plus the wait delay — the two bounds that
	// are allowed to end this call, and nothing else. Written in terms of
	// the constant so a changed value moves the window with it;
	// TestOutboundBudgets_TheDocumentedValues is what keeps the value
	// itself honest. It was a bare 15s, which named no bound at all and
	// would have passed a fourteen-second hang.
	wantElapsed(t, start, 400*time.Millisecond, 400*time.Millisecond+cellCmdWaitDelay+2*time.Second,
		"a drain whose command hangs")

	pid := grandchildPID(t, pidFile)
	if !pidGoneWithin(pid, 5*time.Second) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("the drain command's background child (pid %d) outlived the verb that started it: the budget killed the shell in front of the work and nothing else, so a `systemctl stop` cut off at the budget goes on running with nobody waiting on it", pid)
	}
}

// TestRunHooks_ACancelledPhaseDoesNotWaitOnWhatTheHookForked is the third
// operator-shell call site in this repo, and the one with no deadline of
// its own: lifecycle hooks run under the RPC's context. That makes it the
// site where the missing reach mattered MOST rather than least — there was
// no bound to be inert, so a cancelled `vibe start` killed the hook's
// shell and then waited on whatever the hook had forked, forever.
//
// The context is cancelled WHILE the hook runs, not before: a context
// that is already done makes exec fail at Start, the hook never executes,
// and the test would measure the error path instead of the wait.
func TestRunHooks_ACancelledPhaseDoesNotWaitOnWhatTheHookForked(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "hook.pid")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := runHooks(ctx, "p", "pre_start", []string{fmt.Sprintf(forkingVerb, pidFile)}, true)
	if err == nil {
		t.Fatal("a hook killed by a cancelled context reported success, so pre_start would carry on into a start whose setup never ran")
	}
	if ceiling := 300*time.Millisecond + hookWaitDelay + 2*time.Second; time.Since(start) > ceiling {
		t.Fatalf("the hook phase took %v, past the %v ceiling: nothing bounded the wait, so `vibe start` is held by whatever the hook forked for as long as it lives",
			time.Since(start).Round(time.Millisecond), ceiling)
	}
	pid := grandchildPID(t, pidFile)
	if !pidGoneWithin(pid, 5*time.Second) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("the hook's background child (pid %d) outlived the cancelled phase: the kill reached the shell and not the work", pid)
	}
}

// TestCellResume_HungVerbSurfacesUnavailableAtTheBudget: the same budget
// bounds resume, and resume is the verb an operator runs when the box is
// already misbehaving — the likeliest place to meet a command that never
// comes back. The runner blocks on the context here (the c20 pattern),
// which additionally proves the context it hands the runner is the
// bounded one.
func TestCellResume_HungVerbSurfacesUnavailableAtTheBudget(t *testing.T) {
	t.Parallel()
	d := New(Config{CellCmds: CellCmds{Resume: "systemctl --user start llama-swap", CmdTimeout: "300ms"}})
	sawDeadline := make(chan bool, 1)
	d.SetCellCmdRunner(func(ctx context.Context, cmd string) (string, error) {
		_, ok := ctx.Deadline()
		sawDeadline <- ok
		<-ctx.Done()
		return "", ctx.Err()
	})

	start := time.Now()
	_, err := d.CellResume(context.Background(), connect.NewRequest(&vibev1.CellResumeRequest{}))
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Fatalf("hung resume → %v (%v), want Unavailable", got, err)
	}
	wantElapsed(t, start, 300*time.Millisecond, 10*time.Second, "a resume whose command hangs")
	if !<-sawDeadline {
		t.Fatal("the runner got a context with no deadline: the budget is computed and then not handed to the verb")
	}
}

// ─── doctorAuthTimeout: the outbound credential probe ───────────────────────

// connectStatusOK writes the unary Connect response a vibe daemon would,
// and records the request headers so a test can read what the client
// asked for. Same wire shape c13_doctor_test.go's fake uses.
func connectStatusOK(t *testing.T, seen chan<- http.Header) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case seen <- r.Header.Clone():
		default:
		}
		body, err := proto.Marshal(&vibev1.StatusResponse{Status: &vibev1.Status{Running: false}})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/proto")
		_, _ = w.Write(body)
	}
}

// doctorProbeDaemon wires a fleetd-role daemon whose single cell has a
// daemon_url pointing at handler, with a real token file — the credential
// path cellAuthProbe resolves, not a stubbed one.
func doctorProbeDaemon(t *testing.T, daemonURL string) *Daemon {
	t.Helper()
	tok := filepath.Join(t.TempDir(), "gpu.token")
	if err := os.WriteFile(tok, []byte("gpu-cell-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := New(Config{FleetRegistry: true})
	d.hosts = &fleetcfg.File{Cells: map[string]fleetcfg.Cell{
		fleetcfg.FrontCell: {URL: "http://127.0.0.1:1", Class: fleetcfg.ClassAlwaysOn},
		"gpu-cell": {
			URL:       "http://127.0.0.1:1",
			Class:     fleetcfg.ClassOpportunistic,
			DaemonURL: daemonURL,
			TokenFile: tok,
		},
	}}
	return d
}

// TestDoctorAuthProbe_ATimeoutIsNeverADenial is the hazard this unit
// owns, and it is the half of cellAuthProbe that was already CORRECT and
// entirely untested: Denied is set only for CodeUnauthenticated and
// CodePermissionDenied, because "a 401 is the far side ANSWERING; a
// timeout is the absence of an answer".
//
// Nothing produced a timeout here before, so a regression that set Denied
// on the transport path would have passed every test in the package —
// and doctor would then render a wedged box as `the cell daemon REFUSED
// fleetd's credential` (LevelFail, "drain/resume will 401 until the two
// sides carry the same token"), sending an operator to rotate a token
// that was never wrong while the actual fault was a box that needed a
// reboot. That is this fleet's recurring defect class in its most
// expensive form: absent evidence spent as a DEFINITE value.
func TestDoctorAuthProbe_ATimeoutIsNeverADenial(t *testing.T) {
	t.Parallel()
	d := doctorProbeDaemon(t, hungServer(t).URL)

	start := time.Now()
	res := d.cellAuthProbe(context.Background(), "gpu-cell")
	wantElapsed(t, start, doctorAuthTimeout, doctorAuthTimeout+3*time.Second,
		"one outbound credential probe against a cell that accepts and never answers")

	if !res.Attempted {
		t.Fatal("attempted = false after a probe that really left the box")
	}
	if res.Denied {
		t.Fatal("a probe that TIMED OUT was recorded as Denied. doctor renders that as `the cell daemon REFUSED fleetd's credential` " +
			"and sends the operator to rotate a token, when what actually happened is that nothing answered. " +
			"A 401 is the far side answering; a timeout is the absence of an answer.")
	}
	if res.OK {
		t.Fatal("a probe that never got an answer reported OK — the same mistake with the sign flipped")
	}
	if res.Err == "" {
		t.Fatal("no Err recorded: doctor's UNKNOWN branch prints this string, so an empty one leaves the operator with a level and no reason")
	}
	if res.CredentialErr != "" {
		t.Fatalf("credential_err = %q: the credential resolved fine locally; only the far side is silent, and doctor scores CredentialErr as FAIL", res.CredentialErr)
	}
}

// TestDoctorAuthProbe_A401IsStillADenial is the other side of the same
// pair, unit-scoped. Without it, a "fix" that simply never sets Denied
// would satisfy the test above — and doctor would score a genuinely wrong
// credential as "could not reach the cell daemon", which is the failure
// that made C13 write the distinction down in the first place.
func TestDoctorAuthProbe_A401IsStillADenial(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	d := doctorProbeDaemon(t, srv.URL)

	res := d.cellAuthProbe(context.Background(), "gpu-cell")
	if !res.Attempted || res.OK {
		t.Fatalf("probe = %+v, want an attempted, failed call", res)
	}
	if !res.Denied {
		t.Fatal("a 401 from the cell daemon was not recorded as Denied: doctor would report `could not reach the cell daemon` " +
			"for a box that answered immediately and precisely")
	}
}

// TestDoctorAuthProbe_SpendsAtMostDoctorAuthTimeoutPerCell pins the VALUE
// without spending it: connect-go propagates the client's deadline as
// Connect-Timeout-Ms, so the far side can report the budget fleetd gave
// it. doctor asks EVERY cell, serially — the constant is the difference
// between a report that takes seconds on a dark fleet and one that takes
// minutes, which is the whole reason it is short.
func TestDoctorAuthProbe_SpendsAtMostDoctorAuthTimeoutPerCell(t *testing.T) {
	t.Parallel()
	seen := make(chan http.Header, 1)
	srv := httptest.NewServer(connectStatusOK(t, seen))
	t.Cleanup(srv.Close)
	d := doctorProbeDaemon(t, srv.URL)

	res := d.cellAuthProbe(context.Background(), "gpu-cell")
	if !res.OK {
		t.Fatalf("probe against a healthy cell daemon = %+v, want OK", res)
	}
	var hdr http.Header
	select {
	case hdr = <-seen:
	default:
		t.Fatal("the cell daemon was never called")
	}
	raw := hdr.Get("Connect-Timeout-Ms")
	if raw == "" {
		t.Fatal("the probe carried no Connect-Timeout-Ms: its context has no deadline, so a cell that accepts and stops talking holds the whole doctor report")
	}
	ms, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("Connect-Timeout-Ms = %q: %v", raw, err)
	}
	budget := time.Duration(ms) * time.Millisecond
	if budget > doctorAuthTimeout || budget < doctorAuthTimeout-500*time.Millisecond {
		t.Fatalf("the cell was told it had %v; doctorAuthTimeout is %v. doctor probes every cell in turn, so this number IS the report's worst case",
			budget, doctorAuthTimeout)
	}
	if got := hdr.Get("Authorization"); got != "Bearer gpu-cell-token" {
		t.Fatalf("Authorization = %q, want the per-cell token_file credential the actuation verbs resolve", got)
	}
}

// ─── announceStopTimeout / announceWithdrawTimeout: shutdown ────────────────

// withdrawFixture builds a daemon whose announce client points at
// registryURL, with the loop's stop signal under the test's control.
// Assigning the three fields directly is what startAnnounce does, and it
// is the only way to hold the loop in the state shutdown fears: still
// running when the cancel arrives.
func withdrawFixture(t *testing.T, registryURL string, done <-chan struct{}) (*Daemon, *bool) {
	t.Helper()
	ann, err := fleetannounce.New(fleetannounce.Config{
		Cell:        "gpu-cell",
		RegistryURL: registryURL,
		IntentPath:  filepath.Join(t.TempDir(), "intent.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	canceled := false
	d := New(Config{Fleet: FleetConfig{Cell: "gpu-cell", RegistryURL: registryURL}})
	d.announce = ann
	d.announceCancel = func() { canceled = true }
	d.announceDone = done
	return d, &canceled
}

// TestWithdrawAnnounce_AStuckLoopDoesNotHoldShutdownOpen: the loop is
// cancelled and does not stop. Shutdown waits announceStopTimeout and
// then withdraws anyway, because the goodbye matters more than the tidy
// exit — a cell that never says goodbye lingers in fleetd's registry
// until staleness prunes it, and a shutdown that never returns is worse
// than either.
//
// The registry is a dead port here so the goodbye itself costs nothing:
// the only thing being timed is the wait for the loop.
func TestWithdrawAnnounce_AStuckLoopDoesNotHoldShutdownOpen(t *testing.T) {
	t.Parallel()
	stuck := make(chan struct{}) // never closed: the loop that will not die
	d, canceled := withdrawFixture(t, deadURL(t), stuck)

	start := time.Now()
	d.withdrawAnnounce()
	wantElapsed(t, start, announceStopTimeout, announceStopTimeout+2*time.Second,
		"shutdown waiting on an announce loop that never stops")
	if !*canceled {
		t.Fatal("the loop was never cancelled: withdrawing while a heartbeat can still fire lets a late announce land after the goodbye and resurrect the cell")
	}
}

// TestWithdrawAnnounce_TheGoodbyeIsBoundedAgainstAWedgedRegistry. The
// loop is already stopped, so the only thing left to time is the
// Withdraw POST — against a registry that accepts the connection and
// never answers.
//
// The ceiling is the point. fleetannounce's own http.Client carries a 10s
// timeout, so a withdraw context that was accidentally unbounded (or
// context.WithoutCancel'd, the way a guard in this fleet once shipped
// INERT) would still return — at 10s, twice the budget, on every
// shutdown. A test that only asserted "it returned" would pass on the
// inert version.
func TestWithdrawAnnounce_TheGoodbyeIsBoundedAgainstAWedgedRegistry(t *testing.T) {
	t.Parallel()
	stopped := make(chan struct{})
	close(stopped)
	d, _ := withdrawFixture(t, hungServer(t).URL, stopped)

	start := time.Now()
	d.withdrawAnnounce()
	wantElapsed(t, start, announceWithdrawTimeout, announceWithdrawTimeout+3*time.Second,
		"the withdraw goodbye against a registry that never answers")
}

// TestWithdrawAnnounce_HealthyShutdownPaysNeitherBudget: both timeouts
// are ceilings, not sleeps. A shutdown against a live registry with a
// loop that exits promptly must cost approximately nothing — otherwise
// every restart on the fleet pays eight seconds for a case that is not
// happening.
func TestWithdrawAnnounce_HealthyShutdownPaysNeitherBudget(t *testing.T) {
	t.Parallel()
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"interval_s":1}`))
	}))
	t.Cleanup(reg.Close)
	stopped := make(chan struct{})
	close(stopped)
	d, canceled := withdrawFixture(t, reg.URL, stopped)

	start := time.Now()
	d.withdrawAnnounce()
	if el := time.Since(start); el > time.Second {
		t.Fatalf("a healthy shutdown took %v: the stop wait and the goodbye are ceilings, and a shutdown path that spends them unconditionally taxes every restart in the fleet", el)
	}
	if !*canceled {
		t.Fatal("the loop was never cancelled on the healthy path either")
	}
}

// ─── llamaSwapVersionTimeout: the local version read ────────────────────────

// TestLlamaSwapVersion_AWedgedSwapCostsMillisecondsAndABlankField, where
// "milliseconds" is the constant's word and two seconds is its value. The
// heartbeat is the cell's only evidence of life (C8's rule about the
// prober): a version read that outlived the announce interval would make
// a wedged llama-swap look like a DEAD CELL to fleetd, which is a
// different and much louder wrong answer than a missing version string.
func TestLlamaSwapVersion_AWedgedSwapCostsMillisecondsAndABlankField(t *testing.T) {
	t.Parallel()
	swap := hungServer(t)

	start := time.Now()
	got := llamaSwapVersion(swap.URL)
	wantElapsed(t, start, llamaSwapVersionTimeout, llamaSwapVersionTimeout+3*time.Second,
		"the local llama-swap version read against a wedged swap")
	if got != "" {
		t.Fatalf("llamaSwapVersion = %q from a server that answered nothing", got)
	}
	// Both bounds are armed and equal, so neither alone can be the reason
	// this returned: the context's, and the dedicated client's.
	if swapVersionClient.Timeout != llamaSwapVersionTimeout {
		t.Fatalf("swapVersionClient.Timeout = %v, want %v — the client exists precisely so this read carries its own bound",
			swapVersionClient.Timeout, llamaSwapVersionTimeout)
	}
}

// TestFleetVersionsAt_AWedgedSwapLosesOneFieldAndNotTheBlock is the
// consumer half of the same read, and the answer to "does llamaSwapVersion
// returning "" on every failure discard a known bit". It does collapse
// three causes into one empty string — but the field is UNKNOWN to every
// consumer at that spelling and never "agreement" (fleetapi's
// checkVersions has no branch that reads an empty llama_swap as a
// version, and TestSwapVersionParity_* pins that), so the bit is not
// dropped, it is simply not distinguished. What must not happen is the
// wedged swap taking the whole versions block down with it: defs_sha
// parity is the check C13 wrote this producer for.
func TestFleetVersionsAt_AWedgedSwapLosesOneFieldAndNotTheBlock(t *testing.T) {
	t.Parallel()
	swap := hungServer(t)

	v := fleetVersionsAt(t.TempDir(), swap.URL)
	if v == nil {
		t.Fatal("no versions block at all: a wedged llama-swap took the whole announce field with it, and def-SHA parity silently stops covering this cell")
	}
	if v.LlamaSwap != "" {
		t.Fatalf("versions.llama_swap = %q from a swap that never answered — the one thing this field may never be is a guess", v.LlamaSwap)
	}
	if v.Vibe == "" {
		t.Fatal("versions.vibe is empty: the build string is read locally and cannot depend on llama-swap answering")
	}
}

// TestFleetCapacity_AnUninterruptibleProbeCostsTheBlockAndNotTheHeartbeat.
// The capacity block's other half of the same rule: nvidia-smi on a box
// with a wedged driver does not return — it is the classic D-state
// process, and it is exactly the box whose heartbeat matters most.
// Bounded, the announce loses two numbers; unbounded, fleetd loses the
// cell and reports it stale, which pages someone.
func TestFleetCapacity_AnUninterruptibleProbeCostsTheBlockAndNotTheHeartbeat(t *testing.T) {
	t.Parallel()
	d := New(Config{})
	stuck := func(ctx context.Context) (float64, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	d.SetVRAMProbe(stuck)
	d.SetCapacityProbe(stuck)

	start := time.Now()
	cap := d.fleetCapacity()
	wantElapsed(t, start, fleetCapacityTimeout, fleetCapacityTimeout+3*time.Second,
		"the announce capacity block with a probe that never returns")
	if cap == nil {
		t.Fatal("no capacity block at all: a stuck probe took the whole heartbeat field with it")
	}
	if cap.VRAMFreeGB != 0 || cap.VRAMTotalGB != 0 {
		t.Fatalf("capacity = %+v: a probe that never answered must leave zero (fleetd reads that as not reported), never a guess", cap)
	}
}

// ─── the pre-drain probes ───────────────────────────────────────────────────

// TestProbeResidentModels_UnknownIsNotAnEmptyList. "No models are
// loaded" is a claim about the box; "nothing answered" is a claim about
// the evidence, and the empty slice spelled both before this. That is the
// exact shape observed.Value exists to remove (fleet-control C20), and
// this is package daemon's first use of it — the wedged case is now
// UNKNOWN, and a caller has to write down what it wants absence to mean.
func TestProbeResidentModels_UnknownIsNotAnEmptyList(t *testing.T) {
	t.Parallel()

	t.Run("a wedged llama-swap is unknown, and bounded", func(t *testing.T) {
		t.Parallel()
		swap := hungServer(t)
		d := New(Config{ProxyPort: swap.Listener.Addr().(*net.TCPAddr).Port})

		start := time.Now()
		got := d.probeResidentModels(context.Background())
		wantElapsed(t, start, residentProbeTimeout, residentProbeTimeout+3*time.Second,
			"the pre-drain resident-model probe against a wedged llama-swap")
		if got.IsKnown() {
			models, _ := got.Observed()
			t.Fatalf("a swap that answered NOTHING reported %v as an observation: the pre-drain report would tell an operator "+
				"no models are resident on a box that may be holding two", models)
		}
	})

	t.Run("a router that is not there is unknown", func(t *testing.T) {
		t.Parallel()
		d := New(Config{ProxyPort: deadPort(t)})
		if d.probeResidentModels(context.Background()).IsKnown() {
			t.Fatal("an ECONNREFUSED counted as an observation of an empty box")
		}
	})

	t.Run("an error status is unknown", func(t *testing.T) {
		t.Parallel()
		d := New(Config{ProxyPort: swapPort(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		})})
		if d.probeResidentModels(context.Background()).IsKnown() {
			t.Fatal("a 500 from /running counted as an observation of an empty box")
		}
	})

	t.Run("a genuinely empty swap is KNOWN empty", func(t *testing.T) {
		t.Parallel()
		d := New(Config{ProxyPort: swapPort(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"running":[]}`))
		})})
		got := d.probeResidentModels(context.Background())
		models, ok := got.Observed()
		if !ok {
			t.Fatal("a swap that answered `no models loaded` was recorded as unknown: the other half of the mistake, and it would make every idle cell look unmeasured")
		}
		if len(models) != 0 {
			t.Fatalf("models = %v, want none", models)
		}
	})

	t.Run("residents are observed, stopped ones excluded", func(t *testing.T) {
		t.Parallel()
		d := New(Config{ProxyPort: swapPort(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"running":[{"model":"qwen","state":"ready"},{"model":"old","state":"stopped"}]}`))
		})})
		models, ok := d.probeResidentModels(context.Background()).Observed()
		if !ok || len(models) != 1 || models[0] != "qwen" {
			t.Fatalf("models = %v (known=%v), want [qwen]", models, ok)
		}
	})
}

// swapPort serves handler on a loopback port and returns the port, for
// configs that take a ProxyPort rather than a URL.
func swapPort(t *testing.T, handler http.HandlerFunc) int {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().(*net.TCPAddr).Port
}

// TestCellDrain_AWedgedFleetdBoundsTheLeaseFetchAndSaysSo. The existing
// coverage for leases_unavailable dials port 1 and gets ECONNREFUSED in
// microseconds — the case where fleetd is DOWN. The case that costs
// something is fleetd being up and stuck (swapping, GC-thrashing, holding
// its own lock), and the drain must still return: leases are advisory,
// and the operator asking to reclaim their box is not waiting on them.
//
// Invoked remotely on purpose, which skips the intent POST — so the only
// thing this measures is the lease fetch.
func TestCellDrain_AWedgedFleetdBoundsTheLeaseFetchAndSaysSo(t *testing.T) {
	t.Parallel()
	fleetd := hungServer(t)
	d := New(Config{
		ProxyPort: deadPort(t),
		CellCmds:  CellCmds{Drain: "true"},
		Fleet:     FleetConfig{Cell: "gpu-cell", RegistryURL: fleetd.URL},
	})
	ran := false
	d.SetCellCmdRunner(func(ctx context.Context, cmd string) (string, error) { ran = true; return "", nil })

	start := time.Now()
	ctx := context.WithValue(context.Background(), remoteInvocationKey{}, true)
	resp, err := d.CellDrain(ctx, connect.NewRequest(&vibev1.CellDrainRequest{}))
	if err != nil {
		t.Fatalf("a wedged fleetd failed the drain itself: %v — the lease list is advisory and must never be able to refuse a reclaim", err)
	}
	wantElapsed(t, start, leaseFetchTimeout, leaseFetchTimeout+4*time.Second,
		"a drain whose lease fetch meets a fleetd that never answers")
	if !resp.Msg.LeasesUnavailable {
		t.Fatal("leases_unavailable is false after a lease fetch that timed out: the report claims it ruled out stranded batch work when it never heard back")
	}
	if len(resp.Msg.ActiveLeases) != 0 {
		t.Fatalf("active_leases = %v from a fleetd that answered nothing", resp.Msg.ActiveLeases)
	}
	if !ran {
		t.Fatal("the drain command never ran: a stuck registry stopped an operator reclaiming their own box")
	}
}

// TestPostIntentBestEffort_AWedgedFleetdCannotHoldTheVerb. This POST
// happens AFTER the verb has already succeeded, on a context built from
// context.Background — so its timeout is the only end it has. Unbounded,
// a fleetd that accepts and never answers holds the drain's REPLY behind
// a record that is best-effort by definition: the box is drained, the
// operator is still waiting, and the thing they are waiting for is
// allowed to fail.
func TestPostIntentBestEffort_AWedgedFleetdCannotHoldTheVerb(t *testing.T) {
	t.Parallel()
	fleetd := hungServer(t)
	d := New(Config{Fleet: FleetConfig{Cell: "gpu-cell", RegistryURL: fleetd.URL}})

	start := time.Now()
	d.postIntentBestEffort(fleetapi.Intent{State: "drained", Reason: "gaming"})
	wantElapsed(t, start, intentPostTimeout, intentPostTimeout+3*time.Second,
		"the best-effort intent POST against a fleetd that never answers")
}
