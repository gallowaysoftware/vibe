package cli

// C24 gates: the packaging in deploy/cell — the launcher wrapper and the
// unit's own intent hook. These run the SHIPPED files, not a copy of
// their logic, because the whole phase is packaging: a wrapper that is
// correct in a Go test and wrong in the file an operator installs has
// shipped nothing.
//
// What is NOT gated here, and cannot be from a test process: a real
// systemd unit stopping during a real shutdown. See the phase doc's gate
// table — recorded NOT RUN, with the reason.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
)

func c24Deploy(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "..", "deploy", "cell", name))
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatalf("deploy/cell/%s is missing: %v", name, err)
	}
	if strings.HasSuffix(name, ".sh") && st.Mode()&0o111 == 0 {
		t.Fatalf("deploy/cell/%s is not executable: the README's `install -m 0755` and every ExecStopPost= line assume it is", name)
	}
	return p
}

// c24FakeVibe writes a stand-in for the vibe binary that records its
// argv and its own pid, then optionally parks until it is signalled.
func c24FakeVibe(t *testing.T, dir string, exitCode int, park bool) (path, argvFile, pidFile, sigFile string) {
	t.Helper()
	path = filepath.Join(dir, "fake-vibe")
	argvFile = filepath.Join(dir, "argv")
	pidFile = filepath.Join(dir, "pid")
	sigFile = filepath.Join(dir, "sig")
	body := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + strconv.Quote(argvFile) + "\n" +
		"echo $$ > " + strconv.Quote(pidFile) + "\n"
	if park {
		body += "trap 'echo INT > " + strconv.Quote(sigFile) + "; exit 130' INT\n" +
			"i=0; while [ $i -lt 200 ]; do sleep 0.05; i=$((i+1)); done\n"
	}
	body += "exit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, argvFile, pidFile, sigFile
}

// TestC24WrapperBuildsTheDeclaredDrain: the launcher's one line has to
// come out as the verb, with the reason and ETA the operator declared
// and the command after the `--`.
func TestC24WrapperBuildsTheDeclaredDrain(t *testing.T) {
	wrapper := c24Deploy(t, "vibe-reclaim.sh")
	dir := t.TempDir()
	fake, argvFile, _, _ := c24FakeVibe(t, dir, 0, false)

	for _, tc := range []struct {
		name string
		env  []string
		want []string
	}{
		{
			name: "defaults",
			want: []string{"cell", "drain", "--reason", "gaming", "--yes", "--until-exit", "--", "/usr/bin/game", "-w"},
		},
		{
			name: "declared reason and eta",
			env:  []string{"VIBE_RECLAIM_REASON=blender", "VIBE_RECLAIM_ETA=23:00"},
			want: []string{"cell", "drain", "--reason", "blender", "--eta", "23:00", "--yes", "--until-exit", "--", "/usr/bin/game", "-w"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(wrapper, "/usr/bin/game", "-w")
			cmd.Env = append(append(os.Environ(), "VIBE_BIN="+fake), tc.env...)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("wrapper: %v: %s", err, out)
			}
			raw, err := os.ReadFile(argvFile)
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Errorf("argv = %q, want %q", got, tc.want)
			}
		})
	}

	// No command is a usage error, not a drain: a wrapper that drains and
	// then has nothing to run has taken the box away for nothing.
	cmd := exec.Command(wrapper)
	cmd.Env = append(os.Environ(), "VIBE_BIN="+fake)
	err := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 64 {
		t.Errorf("no-command run = %v, want EX_USAGE (64)", err)
	}
}

// TestC24WrapperExecsSoSignalsReachTheResume. The deferred resume lives
// in the vibe process; a shell sitting between the launcher and vibe is
// one more thing that has to forward SIGINT, and it does not. `exec`
// removes the question: the launcher's child IS vibe.
func TestC24WrapperExecsSoSignalsReachTheResume(t *testing.T) {
	wrapper := c24Deploy(t, "vibe-reclaim.sh")
	dir := t.TempDir()
	fake, _, pidFile, sigFile := c24FakeVibe(t, dir, 0, true)

	cmd := exec.Command(wrapper, "/usr/bin/game")
	cmd.Env = append(os.Environ(), "VIBE_BIN="+fake)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var pid string
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(pidFile); err == nil && len(bytes.TrimSpace(b)) > 0 {
			pid = string(bytes.TrimSpace(b))
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pid == "" {
		_ = cmd.Process.Kill()
		t.Fatal("the wrapped process never started")
	}
	if pid != strconv.Itoa(cmd.Process.Pid) {
		_ = cmd.Process.Kill()
		t.Fatalf("wrapped pid %s != the launcher's child %d — the wrapper did not exec, so a SIGINT from the launcher lands on a shell that forwards nothing", pid, cmd.Process.Pid)
	}
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if _, err := os.Stat(sigFile); err != nil {
		t.Errorf("the wrapped process never saw SIGINT (%v): the resume that fires on it would not have run", err)
	}
}

// TestC24WrapperPropagatesTheChildsStatus, both halves: the shell's
// (`exec` hands the status straight back) and the CLI's — until C24
// `vibe` reported every wrapped failure as 1, which a launcher, a
// .desktop Exec= and every `&&` in a shell all read as "the game
// failed the same way every time".
func TestC24WrapperPropagatesTheChildsStatus(t *testing.T) {
	wrapper := c24Deploy(t, "vibe-reclaim.sh")
	dir := t.TempDir()
	fake, _, _, _ := c24FakeVibe(t, dir, 3, false)

	cmd := exec.Command(wrapper, "/usr/bin/game")
	cmd.Env = append(os.Environ(), "VIBE_BIN="+fake)
	err := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 3 {
		t.Errorf("wrapper exit = %v, want the wrapped command's own 3", err)
	}

	// The CLI half, through the real drain/run/resume path.
	fakeCell := &cellVerbFake{}
	serveCellVerbsOnUnix(t, fakeCell)
	var out bytes.Buffer
	runErr := drainUntilExit(t.Context(), &out, "", "gaming", "", true, 0,
		[]string{"sh", "-c", "exit 3"})
	if got := ExitCode(runErr); got != 3 {
		t.Errorf("ExitCode(%v) = %d, want 3", runErr, got)
	}
	if fakeCell.drains != 1 || fakeCell.resumes != 1 {
		t.Errorf("drains=%d resumes=%d — the status only matters if the cell still came back", fakeCell.drains, fakeCell.resumes)
	}
}

// c24HookEnv is the drop-in's environment, pointed at a test fleetd.
func c24HookEnv(t *testing.T, url, cell string) []string {
	t.Helper()
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("c24-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{
		"VIBE_FLEETD_URL=" + url,
		"VIBE_CELL=" + cell,
		"VIBE_TOKEN_FILE=" + tok,
	}
}

func c24RunHook(t *testing.T, env []string, arg string) (stderr string, code int, took time.Duration) {
	t.Helper()
	hook := c24Deploy(t, "vibe-cell-intent.sh")
	cmd := exec.Command(hook, arg)
	cmd.Env = append(os.Environ(), env...)
	var buf bytes.Buffer
	cmd.Stderr = &buf
	start := time.Now()
	err := cmd.Run()
	took = time.Since(start)
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run hook: %v", err)
	}
	return buf.String(), code, took
}

// c24AuthLog records the credentials the hook presented. Written from
// the server's goroutine and read from the test's, so it carries its own
// lock — `go test -race` catches the sloppy version.
type c24AuthLog struct {
	mu   sync.Mutex
	seen []string
}

func (a *c24AuthLog) add(v string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seen = append(a.seen, v)
}

func (a *c24AuthLog) all() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string{}, a.seen...)
}

// c24Fleetd is a real fleetapi server in the fleetd role — the same
// route, validation and store the hook will meet in production.
func c24Fleetd(t *testing.T, cells ...string) (*fleetapi.Server, *httptest.Server, *c24AuthLog) {
	t.Helper()
	dir := t.TempDir()
	var cs []fleetapi.Cell
	for _, c := range cells {
		cs = append(cs, fleetapi.Cell{Name: c, URL: "http://127.0.0.1:1", Class: "opportunistic"})
	}
	fleet := fleetapi.New(cs, filepath.Join(dir, "hist.json"),
		func() fleetapi.DaemonInfo { return fleetapi.DaemonInfo{} },
		fleetapi.Options{IntentPath: filepath.Join(dir, "intent.json")})
	t.Cleanup(fleet.Close)
	auth := &c24AuthLog{}
	mux := http.NewServeMux()
	fleet.Register(mux)
	wrapped := http.NewServeMux()
	wrapped.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		auth.add(r.Header.Get("Authorization"))
		mux.ServeHTTP(w, r)
	})
	ts := httptest.NewServer(wrapped)
	t.Cleanup(ts.Close)
	return fleet, ts, auth
}

func c24Intent(t *testing.T, fleet *fleetapi.Server, cell string) *fleetapi.Intent {
	t.Helper()
	for _, c := range fleet.Snapshot(context.Background()).Cells {
		if c.Name == cell {
			return c.Intent
		}
	}
	t.Fatalf("cell %s not in the snapshot", cell)
	return nil
}

// TestC24HookRecordsThroughTheRealIntentRoute: the shipped script, the
// shipped JSON, the real route, the real store. The pair of runs is the
// unit's whole lifecycle — stop records, start retires.
func TestC24HookRecordsThroughTheRealIntentRoute(t *testing.T) {
	fleet, ts, auth := c24Fleetd(t, "gpu-cell")
	env := c24HookEnv(t, ts.URL, "gpu-cell")

	stderr, code, _ := c24RunHook(t, env, "stopped")
	if code != 0 {
		t.Fatalf("hook exit %d: %s", code, stderr)
	}
	in := c24Intent(t, fleet, "gpu-cell")
	if in == nil || in.State != "drained" || in.Reason != fleetapi.StopIntentReason {
		t.Fatalf("intent = %+v, want the stop record; hook said: %s", in, stderr)
	}
	if in.Since.IsZero() {
		t.Error("the record carries no timestamp, which is the one thing it knows")
	}
	if seen := auth.all(); len(seen) == 0 || seen[0] != "Bearer c24-token" {
		t.Errorf("Authorization = %q, want the token file's bearer (the intent route is token-only)", seen)
	}

	stderr, code, _ = c24RunHook(t, env, "started")
	if code != 0 {
		t.Fatalf("hook exit %d: %s", code, stderr)
	}
	if in := c24Intent(t, fleet, "gpu-cell"); in != nil {
		t.Errorf("intent = %+v after the unit started, want the stop record retired — a record that outlives the stop is the stale drain this phase exists to prevent", in)
	}
}

// TestC24HookLeavesTheAxisUnknownWhenFleetdIsUnreachable is the gate the
// whole hook is designed around. Six phases of this plan have been bitten
// by absent evidence reading as a healthy value; a stop hook that cannot
// reach fleetd must leave NOTHING behind, so the fleet keeps saying
// DRAINED? — a question, which is what it actually knows.
func TestC24HookLeavesTheAxisUnknownWhenFleetdIsUnreachable(t *testing.T) {
	// A real fleetd exists — it is simply not the one the drop-in points
	// at. That is what a stop during a network outage looks like, and it
	// lets the test assert the store is untouched rather than unobserved.
	fleet, _, _ := c24Fleetd(t, "gpu-cell")
	env := c24HookEnv(t, "http://127.0.0.1:1", "gpu-cell")

	stderr, code, took := c24RunHook(t, env, "stopped")
	if code != 0 {
		t.Errorf("exit %d: an ExecStopPost that fails puts the unit in `failed`, where OnFailure= units fire — a recorder must not be able to trigger anything", code)
	}
	if took > 5*time.Second {
		t.Errorf("took %v: this runs inside the unit's stop, and a hook that outlives TimeoutStopSec is a shutdown that hangs", took)
	}
	if !strings.Contains(stderr, "NOT recorded") || !strings.Contains(stderr, "DRAINED?") {
		t.Errorf("stderr = %q, want it to say the axis was left unknown", stderr)
	}
	if in := c24Intent(t, fleet, "gpu-cell"); in != nil {
		t.Errorf("intent = %+v, want nothing at all", in)
	}
}

// TestC24HookIsBoundedAgainstAHungFleetd: unreachable is the easy case.
// A fleetd that accepts the connection and never answers is the one that
// hangs a shutdown, and only --max-time covers it.
func TestC24HookIsBoundedAgainstAHungFleetd(t *testing.T) {
	stop := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	// Cleanups run LIFO, so the release must be registered AFTER
	// ts.Close: closing a test server waits for its handlers, and a
	// handler parked on a channel nobody has closed yet is a deadlock in
	// the teardown rather than the test.
	t.Cleanup(ts.Close)
	t.Cleanup(func() { close(stop) })

	env := append(c24HookEnv(t, ts.URL, "gpu-cell"), "VIBE_INTENT_TIMEOUT=2")
	stderr, code, took := c24RunHook(t, env, "stopped")
	if code != 0 || took > 6*time.Second {
		t.Errorf("exit=%d took=%v against a hung fleetd: %s", code, took, stderr)
	}
	if !strings.Contains(stderr, "NOT recorded") {
		t.Errorf("stderr = %q, want the give-up line", stderr)
	}

	// The ceiling is enforced too: a drop-in that asks for five minutes
	// inside a stop sequence is a hung shutdown, and it is refused before
	// any request is made.
	env = append(c24HookEnv(t, ts.URL, "gpu-cell"), "VIBE_INTENT_TIMEOUT=300")
	stderr, code, took = c24RunHook(t, env, "stopped")
	if code != 0 || took > 2*time.Second || !strings.Contains(stderr, "ceiling") {
		t.Errorf("exit=%d took=%v stderr=%q, want an immediate refusal", code, took, stderr)
	}
}

// TestC24HookActuatesNothing. The rule is that the hook RECORDS: it
// starts nothing, stops nothing and signals nothing. Asserted by putting
// tripwires ahead of everything on PATH and running every path the hook
// has — success, unreachable, and misconfigured.
func TestC24HookActuatesNothing(t *testing.T) {
	trip := t.TempDir()
	log := filepath.Join(trip, "fired.log")
	for _, name := range []string{
		"systemctl", "service", "loginctl", "pkill", "killall", "kill",
		"reboot", "shutdown", "poweroff", "vibe", "nvidia-smi", "docker",
	} {
		body := "#!/bin/sh\necho \"$0 $*\" >> " + strconv.Quote(log) + "\nexit 0\n"
		if err := os.WriteFile(filepath.Join(trip, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pathEnv := "PATH=" + trip + string(os.PathListSeparator) + os.Getenv("PATH")

	_, ts, _ := c24Fleetd(t, "gpu-cell")
	runs := [][]string{
		append(c24HookEnv(t, ts.URL, "gpu-cell"), pathEnv),
		append(c24HookEnv(t, "http://127.0.0.1:1", "gpu-cell"), pathEnv),
		{pathEnv}, // nothing configured at all
	}
	for _, env := range runs {
		for _, arg := range []string{"stopped", "started", "nonsense"} {
			if _, code, _ := c24RunHook(t, env, arg); code != 0 {
				t.Errorf("arg %q: exit %d, want 0 on every path", arg, code)
			}
		}
	}
	if b, err := os.ReadFile(log); err == nil && len(bytes.TrimSpace(b)) > 0 {
		t.Errorf("the hook invoked %s — it records, it does not actuate", bytes.TrimSpace(b))
	}
}

// TestC24HookRefusesAnUnrecordableCell: the cell name is interpolated
// into a JSON document, so it is validated rather than escaped. A name
// that cannot be recorded safely is not recorded at all.
func TestC24HookRefusesAnUnrecordableCell(t *testing.T) {
	_, ts, _ := c24Fleetd(t, "gpu-cell")
	for _, cell := range []string{`gpu"cell`, `gpu cell`, `$(id)`, ``} {
		env := c24HookEnv(t, ts.URL, cell)
		stderr, code, _ := c24RunHook(t, env, "stopped")
		if code != 0 || !strings.Contains(stderr, "NOT recorded") {
			t.Errorf("cell %q: exit=%d stderr=%q, want a refusal that leaves the axis alone", cell, code, stderr)
		}
	}
}

// TestC24VersionedVerbsMatchTheScript pins the two strings that span
// two languages. They are the whole version-skew guard: an unknown state
// is a 400 on every build of this endpoint, where an unknown REASON on a
// known state is silently recorded as a human-looking drain — which an
// old fleetd then hands back to the cell as a command to drain itself.
func TestC24VersionedVerbsMatchTheScript(t *testing.T) {
	b, err := os.ReadFile(c24Deploy(t, "vibe-cell-intent.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(b)
	for _, want := range []string{
		"state=" + fleetapi.IntentStateUnitStopped,
		"state=" + fleetapi.IntentStateUnitStarted,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("deploy/cell/vibe-cell-intent.sh does not carry `%s`; the hook and fleetapi must agree byte for byte", want)
		}
	}
	// The reason is fleetd's to write. A hook that sends one is a hook
	// that can dress a stop up as a declaration.
	if strings.Contains(script, fleetapi.StopIntentReason) {
		t.Errorf("the hook sends the reserved reason itself; it must send only the versioned state and let fleetd stamp the reason")
	}
}

// TestC24HookAgainstAPreC24Fleetd is the skew gate. An old front does
// not know the unit_* verbs and answers 400 — the hook must treat that
// as every other failure (record nothing, exit 0) and must say what a
// 400 means, because the fix is a human upgrading the front.
func TestC24HookAgainstAPreC24Fleetd(t *testing.T) {
	var bodies []string
	var mu sync.Mutex
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		// Verbatim from the pre-C24 handler.
		http.Error(w, `state must be "drained" or "serving"`, http.StatusBadRequest)
	}))
	t.Cleanup(ts.Close)

	for _, arg := range []string{"stopped", "started"} {
		stderr, code, _ := c24RunHook(t, c24HookEnv(t, ts.URL, "gpu-cell"), arg)
		if code != 0 {
			t.Errorf("%s: exit %d, want 0", arg, code)
		}
		if !strings.Contains(stderr, "NOT recorded") || !strings.Contains(stderr, "predates") {
			t.Errorf("%s: stderr = %q, want a give-up that names the skew", arg, stderr)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("bodies = %v, want one request per verb", bodies)
	}
	for _, b := range bodies {
		if !strings.Contains(b, `"state":"unit_`) {
			t.Errorf("body %q does not use a versioned state, so an old fleetd would have ACCEPTED it", b)
		}
	}
}

// TestC24UnitFragmentIsInstallable reads the shipped drop-in the way
// systemd would. It is an EXAMPLE, which is exactly why it needs a gate:
// an example is what gets copied.
func TestC24UnitFragmentIsInstallable(t *testing.T) {
	path := c24Deploy(t, filepath.Join("llama-swap.service.d", "50-vibe-intent.conf"))
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	env := map[string]string{}
	var hooks []string
	timeoutStop := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "Environment":
			k, v, _ := strings.Cut(val, "=")
			env[k] = v
		case "ExecStartPost", "ExecStopPost":
			hooks = append(hooks, val)
		case "TimeoutStopSec":
			timeoutStop, _ = strconv.Atoi(val)
		case "ExecStart", "Restart", "OnFailure", "ExecReload":
			t.Errorf("the drop-in sets %s=%s: this fragment records a lifecycle, it must not change one", key, val)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}

	for _, k := range []string{"VIBE_FLEETD_URL", "VIBE_CELL", "VIBE_TOKEN_FILE"} {
		if env[k] == "" {
			t.Errorf("drop-in sets no %s; the hook would give up before its first request", k)
		}
	}
	if len(hooks) != 2 {
		t.Fatalf("hooks = %v, want both halves (a stop record with no start half is a drain that never clears)", hooks)
	}
	for _, h := range hooks {
		if !strings.HasPrefix(h, "-") {
			t.Errorf("%q is not prefixed with `-`: a failing ExecStopPost puts the unit in `failed`, where OnFailure= units fire", h)
		}
		if !strings.Contains(h, "vibe-cell-intent.sh") {
			t.Errorf("%q does not run the shipped hook", h)
		}
	}
	if !strings.Contains(strings.Join(hooks, " "), "stopped") || !strings.Contains(strings.Join(hooks, " "), "started") {
		t.Errorf("hooks = %v, want one `stopped` and one `started`", hooks)
	}
	// llama-swap force-closes in-flight streams at a hardcoded 30s on
	// SIGTERM (C16); the hook's own bound sits on top of that. A
	// TimeoutStopSec under the sum is a SIGKILL that skips the hook.
	timeout, _ := strconv.Atoi(env["VIBE_INTENT_TIMEOUT"])
	if min := 30 + timeout; timeoutStop < min {
		t.Errorf("TimeoutStopSec=%d, want at least %d (llama-swap's 30s stream grace + the hook's %ds bound)", timeoutStop, min, timeout)
	}
}

// TestC24ReadmeDocumentsBothLauncherForms is a documentation gate, and
// it is the phase's actual deliverable: the mechanism already existed
// (drainUntilExit, C2) and what was missing was the line an operator
// pastes into Steam.
func TestC24ReadmeDocumentsBothLauncherForms(t *testing.T) {
	b, err := os.ReadFile(c24Deploy(t, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	readme := string(b)
	for _, want := range []string{"%command%", "vibe-reclaim.desktop", "ExecStopPost", "DRAINED?"} {
		if !strings.Contains(readme, want) {
			t.Errorf("deploy/cell/README.md never mentions %q", want)
		}
	}
	desktop, err := os.ReadFile(c24Deploy(t, "vibe-reclaim.desktop"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(desktop), "vibe-reclaim.sh") {
		t.Error("the .desktop example does not run the wrapper")
	}
}
