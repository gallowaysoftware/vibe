package fleetlab

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// the fake
//
// A stand-in for one of the four process shapes `lab.sh down` sweeps on.
// It is this test binary, copied to a path whose basename is the one the
// sweep pattern names, and re-executed with VIBE_FLEETLAB_FAKE set. The
// point is the COMMAND LINE: `pgrep -f` and the upstream scan both match
// on argv, so a stand-in only has to wear the right argv to be exactly as
// killable as the real thing.

const (
	fakeEnv      = "VIBE_FLEETLAB_FAKE"
	fakeReadyEnv = "VIBE_FLEETLAB_FAKE_READY"
)

func TestMain(m *testing.M) {
	if os.Getenv(fakeEnv) != "" {
		fakeMain()
		return
	}
	os.Exit(m.Run())
}

// fakeMain never returns. If argv carries `--port N` it binds and serves
// on it, so a survivor can be proven to be still SERVING and not merely
// still scheduled.
func fakeMain() {
	if port := portFromArgs(os.Args); port > 0 {
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake: listen:", err)
			os.Exit(3)
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "ok")
		})}
		go func() { _ = srv.Serve(ln) }()
	}
	if ready := os.Getenv(fakeReadyEnv); ready != "" {
		if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "fake: ready:", err)
			os.Exit(3)
		}
	}
	for {
		time.Sleep(time.Second)
	}
}

func portFromArgs(args []string) int {
	for i := 1; i < len(args); i++ {
		if args[i] == "--port" && i+1 < len(args) {
			if n, err := strconv.Atoi(args[i+1]); err == nil {
				return n
			}
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// harness

type fake struct {
	name string // what it stands in for, for failure messages
	cmd  *exec.Cmd
	port int           // 0 when it binds nothing
	done chan struct{} // closed once the process has exited AND been reaped
}

// alive reads the reaper goroutine's verdict rather than probing with
// signal 0. These are our own children: between exiting and being reaped
// they are zombies, and `kill(pid, 0)` on a zombie still succeeds — a
// swept process would read as a survivor and the gate would pass for the
// worst possible reason.
func (f *fake) alive() bool {
	select {
	case <-f.done:
		return false
	default:
		return true
	}
}

func (f *fake) waitDead(d time.Duration) bool {
	select {
	case <-f.done:
		return true
	case <-time.After(d):
		return false
	}
}

func (f *fake) kill() {
	if f.cmd.Process != nil {
		_ = f.cmd.Process.Kill()
	}
	<-f.done
}

// labBinary copies (or hard-links) this test binary to path, so a process
// started from it wears `path` as argv[0].
func labBinary(t *testing.T, path string) string {
	t.Helper()
	self, err := os.Executable()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	// Both instances deliberately share one llama-server stand-in path, so
	// this is called twice for it. Rewriting a running executable is
	// ETXTBSY; the first copy is already the right bytes.
	if _, err := os.Stat(path); err == nil {
		return path
	}
	if err := os.Link(self, path); err == nil {
		return path
	}
	src, err := os.Open(self)
	require.NoError(t, err)
	defer src.Close()
	dst, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	require.NoError(t, err)
	_, err = io.Copy(dst, src)
	require.NoError(t, dst.Close())
	require.NoError(t, err)
	return path
}

// startFake runs bin with args and blocks until it says it is ready.
func startFake(t *testing.T, name, bin string, port int, args ...string) *fake {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "ready-"+strings.ReplaceAll(name, "/", "_"))
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), fakeEnv+"=1", fakeReadyEnv+"="+ready)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	require.NoError(t, cmd.Start(), "start fake %s", name)
	f := &fake{name: name, cmd: cmd, port: port, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(f.done)
	}()
	t.Cleanup(f.kill)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ready); err == nil {
			return f
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("fake %s never signalled ready (bin %s args %v)", name, bin, args)
	return nil
}

// instance is one lab: its scratch dir, its port base, and the four
// process shapes `lab.sh down` sweeps on.
type instance struct {
	dir   string
	base  int
	fakes []*fake
}

// standUp starts one stand-in per sweep pattern. The llama-swap, vibe and
// host-probe stand-ins are anchored on the instance's $LAB (which is what
// makes them un-matchable from a sibling); the llama-server stand-in
// deliberately lives in a SHARED bin dir carrying no lab path at all, so
// the only handle the sweep has on it is its port — the exact situation
// the derived window has to be correct about.
func standUp(t *testing.T, sharedBin, dir string, base, upstreamPort int) *instance {
	t.Helper()
	tag := filepath.Base(dir)
	in := &instance{dir: dir, base: base}

	upstream := labBinary(t, filepath.Join(sharedBin, "llama-server"))
	swap := labBinary(t, filepath.Join(sharedBin, "llama-swap"))
	probe := labBinary(t, filepath.Join(sharedBin, "socat"))
	vibe := labBinary(t, filepath.Join(dir, "bin", "vibe"))

	in.fakes = append(in.fakes,
		startFake(t, tag+"/llama-server", upstream, upstreamPort,
			"--model", "/dev/null", "--port", strconv.Itoa(upstreamPort)),
		startFake(t, tag+"/llama-swap", swap, 0,
			"-config", filepath.Join(dir, "cells", "front", "config.yaml")),
		startFake(t, tag+"/vibe", vibe, 0,
			"fleet", "announce", "--cell", "alpha"),
		startFake(t, tag+"/hostprobe", probe, 0,
			"TCP-LISTEN:0", "SYSTEM:echo "+tag+"-hostprobe-alpha"),
	)
	return in
}

func (in *instance) requireAllAlive(t *testing.T, why string) {
	t.Helper()
	for _, f := range in.fakes {
		require.True(t, f.alive(), "%s: %s should still be running", why, f.name)
	}
}

func (in *instance) requireAllDead(t *testing.T, why string) {
	t.Helper()
	for _, f := range in.fakes {
		require.True(t, f.waitDead(20*time.Second), "%s: %s should have been swept", why, f.name)
	}
}

func requireServing(t *testing.T, port int, why string) {
	t.Helper()
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/v1/models")
	require.NoError(t, err, "%s: port %d should still answer", why, port)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s: port %d", why, port)
}

// labDown runs `lab.sh down` for one instance and returns its output.
func labDown(t *testing.T, in *instance) string {
	t.Helper()
	cmd := exec.Command("bash", filepath.Join("..", "..", "scripts", "fleetlab", "lab.sh"), "down")
	cmd.Env = append(os.Environ(),
		"FLEETLAB_DIR="+in.dir,
		"FLEETLAB_PORT_BASE="+strconv.Itoa(in.base))
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "lab.sh down: %s", out)
	return string(out)
}

// freeBase walks candidate bases (multiples of 200) until it finds one
// whose upstream ports are all bindable. A base in use is exactly the
// condition this whole feature exists for, so the test refuses to guess.
func freeBase(t *testing.T, from int, want int) int {
	t.Helper()
	for base := from; base < from+4000; base += 200 {
		ok := true
		for off := 0; off < want; off++ {
			p := base - 3620 + off
			ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
			if err != nil {
				ok = false
				break
			}
			_ = ln.Close()
		}
		if ok {
			return base
		}
	}
	t.Skipf("no free port base near %d", from)
	return 0
}

func requireTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"bash", "ps", "pgrep", "awk"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}
}

// ---------------------------------------------------------------------------
// the gate

// TestDownCannotReachAnotherInstance is C23's gate. Two labs, two
// non-default bases, two scratch dirs; tear one down and the other must
// still be running AND still serving.
//
// The negative control is the same test with ONE base shared between the
// instances, which is the world before FLEETLAB_PORT_BASE existed: there
// the survivor's llama-server dies, because a shared upstream range is
// the one handle a sibling's sweep has on it. Without that half, a green
// result here would be consistent with a sweep that kills nothing at all.
func TestDownCannotReachAnotherInstance(t *testing.T) {
	requireTools(t)

	root := t.TempDir()
	sharedBin := filepath.Join(root, "sharedbin")
	require.NoError(t, os.MkdirAll(sharedBin, 0o755))

	baseA := freeBase(t, 20000, 2)
	baseB := freeBase(t, baseA+200, 2)
	require.NotEqual(t, baseA, baseB)
	t.Logf("A base %d (upstreams %d-%d), B base %d (upstreams %d-%d)",
		baseA, baseA-3620, baseA-3620+39, baseB, baseB-3620, baseB-3620+39)

	a := standUp(t, sharedBin, filepath.Join(root, "lab-a"), baseA, baseA-3620)
	b := standUp(t, sharedBin, filepath.Join(root, "lab-b"), baseB, baseB-3620)

	a.requireAllAlive(t, "before A's down")
	b.requireAllAlive(t, "before A's down")
	requireServing(t, b.fakes[0].port, "before A's down")

	t.Log(labDown(t, a))

	a.requireAllDead(t, "A's own processes after A's down")
	b.requireAllAlive(t, "B's processes after A's down")
	requireServing(t, b.fakes[0].port, "B after A's down")
}

// TestDownReachesASiblingOnTheSameBase is the negative control: it is the
// pre-C23 world, where every instance shared one port table. Instance B's
// $LAB-anchored processes survive (those patterns were always
// instance-scoped); its llama-server does NOT, because the sweep's only
// handle on an upstream is its port. This is the failure C16's L4 gate
// walked into, reproduced deliberately.
func TestDownReachesASiblingOnTheSameBase(t *testing.T) {
	requireTools(t)

	root := t.TempDir()
	sharedBin := filepath.Join(root, "sharedbin")
	require.NoError(t, os.MkdirAll(sharedBin, 0o755))

	base := freeBase(t, 24000, 2)
	a := standUp(t, sharedBin, filepath.Join(root, "lab-a"), base, base-3620)
	b := standUp(t, sharedBin, filepath.Join(root, "lab-b"), base, base-3620+1)

	a.requireAllAlive(t, "before A's down")
	b.requireAllAlive(t, "before A's down")

	t.Log(labDown(t, a))

	a.requireAllDead(t, "A's own processes")

	upstream := b.fakes[0]
	require.True(t, upstream.waitDead(20*time.Second),
		"a sibling on the SAME base must be reachable by the sweep — if this passes, "+
			"the isolation assertion in TestDownCannotReachAnotherInstance proves nothing")
	for _, f := range b.fakes[1:] {
		require.True(t, f.alive(), "%s is $LAB-anchored and was never the shared-constant half", f.name)
	}
}

// ---------------------------------------------------------------------------
// the derivation

func labPorts(t *testing.T, base string) (string, int) {
	t.Helper()
	cmd := exec.Command("bash", filepath.Join("..", "..", "scripts", "fleetlab", "lab.sh"), "ports")
	cmd.Env = os.Environ()
	if base != "" {
		cmd.Env = append(cmd.Env, "FLEETLAB_PORT_BASE="+base)
	} else {
		// t.Setenv is not usable here (parallel-unsafe) and the ambient
		// environment must not decide what "the default" means.
		filtered := cmd.Env[:0]
		for _, kv := range cmd.Env {
			if !strings.HasPrefix(kv, "FLEETLAB_PORT_BASE=") {
				filtered = append(filtered, kv)
			}
		}
		cmd.Env = filtered
	}
	out, err := cmd.CombinedOutput()
	code := 0
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		code = ee.ExitCode()
	case err != nil:
		t.Fatalf("lab.sh ports: %v", err)
	}
	return string(out), code
}

// TestDefaultBaseIsTodaysTable locks the compatibility claim. Every gate
// script in scripts/fleetlab, every phase-doc transcript and every
// README table names these ports; the knob is only safe to add because
// its default reproduces them exactly.
func TestDefaultBaseIsTodaysTable(t *testing.T) {
	requireTools(t)
	out, code := labPorts(t, "")
	require.Zero(t, code, out)
	require.Equal(t, strings.Join([]string{
		"port_base 9600",
		"upstream_base 5980",
		"listen_window 9600-9799",
		"upstream_window 5980-6019",
		"cell front 9640 always_on 5980 -",
		"cell alpha 9641 always_on 5990 9651",
		"cell bravo 9642 opportunistic 6000 9652",
		"cell charlie 9643 roaming 6010 9653",
		"proxy 9720",
		"fleetd 9721",
		"bravo_daemon 9723",
		"notify 9724",
		"",
	}, "\n"), out)
}

// TestBaseShiftsEveryPort — one knob, and NOTHING left behind on the old
// block. A single port that did not move is a cross-instance collision.
func TestBaseShiftsEveryPort(t *testing.T) {
	requireTools(t)
	def, code := labPorts(t, "")
	require.Zero(t, code, def)

	for _, base := range []int{10000, 10200, 20000} {
		shift := base - 9600
		moved, code := labPorts(t, strconv.Itoa(base))
		require.Zero(t, code, moved)

		defLines := strings.Split(strings.TrimRight(def, "\n"), "\n")
		movedLines := strings.Split(strings.TrimRight(moved, "\n"), "\n")
		require.Equal(t, len(defLines), len(movedLines), "base %d changed the table's SHAPE", base)

		for i, dl := range defLines {
			want := shiftNumbers(dl, shift)
			require.Equal(t, want, movedLines[i], "base %d, line %d", base, i)
		}
		require.NotContains(t, moved, " 96", "base %d left a default-block port behind: %s", base, moved)
	}
}

// shiftNumbers adds shift to every run of digits in a line, leaving the
// words alone. `cell alpha 9641 always_on 5990 9651` -> every number moves
// by the same amount, which is the whole claim.
func shiftNumbers(line string, shift int) string {
	var b strings.Builder
	for i := 0; i < len(line); {
		if line[i] < '0' || line[i] > '9' {
			b.WriteByte(line[i])
			i++
			continue
		}
		j := i
		for j < len(line) && line[j] >= '0' && line[j] <= '9' {
			j++
		}
		n, _ := strconv.Atoi(line[i:j])
		b.WriteString(strconv.Itoa(n + shift))
		i = j
	}
	return b.String()
}

// TestGuardRefusesADangerousBase. The lab runs beside a production
// llama-swap on :9000 holding a resident model and real traffic, and
// `down` kills what its patterns match. A base whose windows cover a
// production port must not be a runtime surprise — it must not start.
func TestGuardRefusesADangerousBase(t *testing.T) {
	requireTools(t)
	for _, tc := range []struct {
		name, base, want string
	}{
		{"listen window covers :9000 and :9001", "9000", "9000-9001"},
		{"upstream window covers :9000", "12600", "9000-9001"},
		{"listen window covers production upstreams", "9400", "5800-5809"},
		{"listen window covers ritual.sh", "9800", "9810-9819"},
		{"not a multiple of the block size", "9700", "multiple of 200"},
		{"not a number", "nine-thousand", "whole number"},
		{"upstream window below 1024", "4600", "below 1024"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, code := labPorts(t, tc.base)
			require.NotZero(t, code, "base %s was accepted: %s", tc.base, out)
			require.Contains(t, out, tc.want, "base %s: %s", tc.base, out)
		})
	}
}

// TestGuardRefusesAMissingSweepTool. `down`'s upstream half reads `ps`
// and filters with `awk`. Missing either does not fail — it reaps
// NOTHING, quietly, while every line of output says the lab came down
// cleanly. That is the same shape of silence this whole phase is about,
// so it is refused rather than degraded.
func TestGuardRefusesAMissingSweepTool(t *testing.T) {
	requireTools(t)
	bash, err := exec.LookPath("bash")
	require.NoError(t, err)

	// A PATH holding everything lab.sh needs to reach the guard, minus
	// `ps`. Symlinks rather than a copied directory so the farm stays
	// obviously exhaustive.
	farm := filepath.Join(t.TempDir(), "bin")
	require.NoError(t, os.MkdirAll(farm, 0o755))
	for _, tool := range []string{"dirname", "basename", "sed", "cat", "grep", "awk", "pgrep", "env"} {
		src, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s not on PATH", tool)
		}
		require.NoError(t, os.Symlink(src, filepath.Join(farm, tool)))
	}

	cmd := exec.Command(bash, filepath.Join("..", "..", "scripts", "fleetlab", "lab.sh"), "ports")
	cmd.Env = []string{"PATH=" + farm, "HOME=" + t.TempDir()}
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "a lab that cannot run `ps` must refuse: %s", out)
	require.Contains(t, string(out), "ps is required")
}

// TestGuardRefusesBeforeAnythingIsSwept — the guard is not advice. `down`
// on a refused base must not reach the sweep at all, or a mistyped base
// would still kill whatever the old patterns matched.
func TestGuardRefusesBeforeAnythingIsSwept(t *testing.T) {
	requireTools(t)

	root := t.TempDir()
	sharedBin := filepath.Join(root, "sharedbin")
	require.NoError(t, os.MkdirAll(sharedBin, 0o755))
	base := freeBase(t, 28000, 1)
	victim := standUp(t, sharedBin, filepath.Join(root, "lab-a"), base, base-3620)

	cmd := exec.Command("bash", filepath.Join("..", "..", "scripts", "fleetlab", "lab.sh"), "down")
	cmd.Env = append(os.Environ(),
		"FLEETLAB_DIR="+victim.dir,
		"FLEETLAB_PORT_BASE=9000")
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "down on base 9000 must refuse: %s", out)
	require.Contains(t, string(out), "9000-9001")
	require.NotContains(t, string(out), "down (idempotent)")

	victim.requireAllAlive(t, "after a refused down")
}
