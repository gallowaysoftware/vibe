package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
	vibev1 "github.com/gallowaysoftware/vibe/proto/vibe/v1"
	"github.com/gallowaysoftware/vibe/proto/vibe/v1/vibev1connect"
)

// ─── checkLlamaBinary ───────────────────────────────────────────────────────

// The four shapes a box can be in. Before C26a this check ran the top two
// rows only — it asked $PATH and nothing else — so row three FAILED doctor
// (exit non-zero) on a laptop whose only profiles are cloud_peer, pointed
// at a peer through a remote front. The same mis-fire hit comfyui-only and
// mlx-only boxes, which is why the applicable set is computed from what is
// DECLARED rather than from any one backend kind.

func TestCheckLlamaBinary_DeclaredAndPresent(t *testing.T) {
	env := &doctorEnv{lookPath: func(name string) (string, error) {
		if name != "llama-server" {
			t.Fatalf("unexpected lookup of %q", name)
		}
		return "/opt/bin/llama-server", nil
	}}
	r, ok := checkLlamaBinary(env, []string{"profile coder"})
	if !ok {
		t.Fatal("a box that declares a llama_server backend must be told about the binary")
	}
	if r.Status != statusOK {
		t.Fatalf("status = %v, want OK", r.Status)
	}
	if !strings.Contains(r.Message, "/opt/bin/llama-server") || !strings.Contains(r.Message, "profile coder") {
		t.Fatalf("message = %q, want the path AND what needs it", r.Message)
	}
}

func TestCheckLlamaBinary_DeclaredAndMissingStillFails(t *testing.T) {
	env := &doctorEnv{lookPath: func(string) (string, error) {
		return "", errors.New("not found")
	}}
	r, ok := checkLlamaBinary(env, []string{"profile coder"})
	if !ok {
		t.Fatal("not-applicable on a box that declares a llama_server backend: the check would never fire again")
	}
	if r.Status != statusFail {
		t.Fatalf("status = %v, want FAIL — a declared llama_server backend with no binary cannot start", r.Status)
	}
	if !strings.Contains(r.Message, "install llama.cpp") {
		t.Fatalf("message lacks install hint: %q", r.Message)
	}
	if !strings.Contains(r.Message, "profile coder") {
		t.Fatalf("message does not name what needs it: %q", r.Message)
	}
}

// The finding itself: nothing on disk spawns llama-server, so its absence
// is not a fault. Not-applicable, NOT a pass — a green "llama-server: ok"
// on a box with no binary would be a claim doctor cannot make.
func TestCheckLlamaBinary_NothingDeclaresItIsNotApplicable(t *testing.T) {
	env := &doctorEnv{lookPath: func(string) (string, error) {
		t.Fatal("$PATH was consulted for a binary nothing on this box would ever invoke")
		return "", nil
	}}
	if _, ok := checkLlamaBinary(env, nil); ok {
		t.Fatal("a cloud_peer-only (or comfyui-only, or mlx-only) box was failed for a missing llama-server it will never invoke")
	}
}

// ─── llamaServerUsers: the declaration scan ─────────────────────────────────

// TestLlamaServerUsers_TheFourShapesOnDisk is the scan the check is gated
// on. The mixed box is the row that matters: one cloud_peer profile and one
// llama_server backend def must still produce a user, or a fleet box would
// stop being told its binary is missing.
func TestLlamaServerUsers_TheFourShapesOnDisk(t *testing.T) {
	const cloudPeer = `name: peer
backend:
  cloud_peer:
    base_url: https://api.example.com
    api_key_env: EXAMPLE_API_KEY
    models: [example-model]
`
	const llamaProfile = `name: coder
backend:
  llama_server:
    path: /nonexistent/model.gguf
    alias: coder
    context: 4096
`
	// A definition that pins its own binary never consults $PATH.
	const pinnedBinary = `name: custom
backend:
  llama_server:
    binary: /opt/llama/bin/llama-server
    path: /nonexistent/model.gguf
    alias: custom
    context: 4096
`
	for _, tc := range []struct {
		name     string
		profiles map[string]string
		backends map[string]string
		want     []string
	}{
		{"nothing at all", nil, nil, nil},
		{"a cloud_peer-only box", map[string]string{"peer.yaml": cloudPeer}, nil, nil},
		{"a llama_server profile", map[string]string{"coder.yaml": llamaProfile}, nil, []string{"profile coder"}},
		{"a llama_server backend def", nil, map[string]string{"gpu.yaml": llamaProfile}, []string{"backend gpu"}},
		{"a mixed box", map[string]string{"peer.yaml": cloudPeer}, map[string]string{"gpu.yaml": llamaProfile},
			[]string{"backend gpu"}},
		{"a pinned binary is not a $PATH user", map[string]string{"custom.yaml": pinnedBinary}, nil, nil},
		{"an unparseable file is skipped, not fatal", map[string]string{"junk.yaml": "::: not yaml"}, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backendsDir, profilesDir := setDoctorFixtureDirs(t)
			for n, body := range tc.profiles {
				if err := os.WriteFile(filepath.Join(profilesDir, n), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			for n, body := range tc.backends {
				if err := os.WriteFile(filepath.Join(backendsDir, n), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got := llamaServerUsers(profilesDir, backendsDir)
			if len(got) != len(tc.want) {
				t.Fatalf("users = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("users = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// A llama_server profile whose model file is not on disk yet still DECLARES
// the binary. Scanning through profile.Load (which validates the path) would
// drop it and quietly make the check not-applicable on exactly the box that
// needs it — the disarm this repo keeps finding, one layer down.
func TestLlamaServerUsers_AnUnpulledModelStillDeclaresTheBinary(t *testing.T) {
	_, profilesDir := setDoctorFixtureDirs(t)
	body := `name: coder
backend:
  llama_server:
    path: ` + filepath.Join(t.TempDir(), "not-downloaded-yet.gguf") + `
    alias: coder
    context: 4096
`
	path := filepath.Join(profilesDir, "coder.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := profile.Load(path); err == nil {
		t.Fatal("the premise of this test is gone: the loader now accepts a missing model path")
	}
	if got := llamaServerUsers(profilesDir, paths.BackendsDir()); len(got) != 1 {
		t.Fatalf("users = %v, want the profile counted despite its model not being pulled yet", got)
	}
}

// ─── checkLlamaVersion ──────────────────────────────────────────────────────

func TestCheckLlamaVersion_OK(t *testing.T) {
	env := &doctorEnv{
		lookPath: func(string) (string, error) { return "/usr/bin/llama-server", nil },
		run: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
			return []byte("version: b9159-88b55f4\n"), nil
		},
	}
	r, ok := checkLlamaVersion(context.Background(), env, []string{"profile coder"})
	if !ok {
		t.Fatal("not-applicable on a box that declares a llama_server backend")
	}
	if r.Status != statusOK {
		t.Fatalf("status = %v, want OK; message=%q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "b9159") {
		t.Fatalf("message = %q", r.Message)
	}
}

func TestCheckLlamaVersion_NonZeroExitWithOutput(t *testing.T) {
	env := &doctorEnv{
		lookPath: func(string) (string, error) { return "/usr/bin/llama-server", nil },
		run: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
			return []byte("version foo\n"), errors.New("exit status 1")
		},
	}
	r, _ := checkLlamaVersion(context.Background(), env, []string{"profile coder"})
	if r.Status != statusWarn {
		t.Fatalf("status = %v, want WARN", r.Status)
	}
}

func TestCheckLlamaVersion_Skipped(t *testing.T) {
	env := &doctorEnv{
		lookPath: func(string) (string, error) { return "", errors.New("nope") },
		run: func(context.Context, string, ...string) ([]byte, error) {
			t.Fatal("run should not be called when binary missing")
			return nil, nil
		},
	}
	r, ok := checkLlamaVersion(context.Background(), env, []string{"profile coder"})
	if !ok || r.Status != statusWarn {
		t.Fatalf("ok=%v status = %v, want ok=true WARN (skipped)", ok, r.Status)
	}
}

// The version check is gated on the SAME scan as the binary check: warning
// "skipped (llama-server not on $PATH)" on a box that never spawns it is
// the same mis-fire one line down, and gating one and not the other is how
// a guard ends up on some of its call paths.
func TestCheckLlamaVersion_NothingDeclaresItIsNotApplicable(t *testing.T) {
	env := &doctorEnv{
		lookPath: func(string) (string, error) {
			t.Fatal("$PATH was consulted for a binary nothing on this box would ever invoke")
			return "", nil
		},
	}
	if _, ok := checkLlamaVersion(context.Background(), env, nil); ok {
		t.Fatal("a cloud_peer-only box was warned about a llama-server version it will never ask for")
	}
}

// ─── checkHFBinary / checkHFAuth ────────────────────────────────────────────

func TestCheckHFBinary_Missing(t *testing.T) {
	env := &doctorEnv{lookPath: func(string) (string, error) { return "", errors.New("x") }}
	r := checkHFBinary(env)
	if r.Status != statusWarn {
		t.Fatalf("status = %v, want WARN", r.Status)
	}
}

func TestCheckHFBinary_Found(t *testing.T) {
	env := &doctorEnv{lookPath: func(string) (string, error) { return "/usr/bin/hf", nil }}
	r := checkHFBinary(env)
	if r.Status != statusOK {
		t.Fatalf("status = %v, want OK", r.Status)
	}
}

func TestCheckHFAuth_NotLoggedIn(t *testing.T) {
	env := &doctorEnv{
		lookPath: func(string) (string, error) { return "/usr/bin/hf", nil },
		run: func(context.Context, string, ...string) ([]byte, error) {
			return []byte("Not logged in\n"), nil
		},
	}
	r := checkHFAuth(context.Background(), env)
	if r.Status != statusWarn {
		t.Fatalf("status = %v, want WARN", r.Status)
	}
	if !strings.Contains(r.Message, "hf auth login") {
		t.Fatalf("message lacks hint: %q", r.Message)
	}
}

func TestCheckHFAuth_LoggedIn(t *testing.T) {
	env := &doctorEnv{
		lookPath: func(string) (string, error) { return "/usr/bin/hf", nil },
		run: func(context.Context, string, ...string) ([]byte, error) {
			return []byte("kyle-galloway\n"), nil
		},
	}
	r := checkHFAuth(context.Background(), env)
	if r.Status != statusOK {
		t.Fatalf("status = %v, want OK; msg=%q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "kyle-galloway") {
		t.Fatalf("message = %q", r.Message)
	}
}

func TestCheckHFAuth_Skipped(t *testing.T) {
	env := &doctorEnv{lookPath: func(string) (string, error) { return "", errors.New("x") }}
	r := checkHFAuth(context.Background(), env)
	if r.Status != statusWarn {
		t.Fatalf("status = %v, want WARN (skipped)", r.Status)
	}
}

// ─── checkXDGDirs ───────────────────────────────────────────────────────────

func TestCheckXDGDirs_OK(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(tmp, "runtime"))
	r := checkXDGDirs()
	if r.Status != statusOK {
		t.Fatalf("status = %v, want OK; msg=%q", r.Status, r.Message)
	}
}

func TestProbeDirWritable(t *testing.T) {
	dir := t.TempDir()
	if err := probeDirWritable(dir); err != nil {
		t.Fatalf("writable temp dir failed: %v", err)
	}
	// Non-existent path.
	if err := probeDirWritable(filepath.Join(dir, "nope")); err == nil {
		t.Fatal("expected error for missing dir")
	}
	// A regular file is not a directory.
	f := filepath.Join(dir, "f")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := probeDirWritable(f); err == nil {
		t.Fatal("expected 'not a directory' error")
	}
}

// ─── port checks ────────────────────────────────────────────────────────────

func TestCheckControlPlanePort_Free(t *testing.T) {
	// :9001 may be free or in-use; we exercise the success path on a
	// disposable port by overriding the function's bound address would
	// require restructuring. Instead exercise the helper layer directly.
	free, err := tryBind("127.0.0.1:0")
	if !free || err != nil {
		t.Fatalf("tryBind on ephemeral port failed: %v / err=%v", free, err)
	}
}

// serveControlOnTCP puts a real ControlService behind a real TCP listener
// and returns its host:port — the transport the doctor's control-plane row
// actually probes (:9001), as opposed to the unix socket every other CLI
// command uses. The port stays HELD for the life of the test, which is
// what makes the row's tryBind fail with address-in-use and the probe the
// only thing that can attribute the holder.
func serveControlOnTCP(t *testing.T, h vibev1connect.ControlServiceHandler) string {
	t.Helper()
	_, handler := vibev1connect.NewControlServiceHandler(h)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return strings.TrimPrefix(ts.URL, "http://")
}

// doctorEnvAt wires the REAL defaultDaemonStatus at addr, so the tests
// below exercise the transport, the budget and the error classification
// rather than a stub's idea of them.
func doctorEnvAt(addr string) *doctorEnv {
	return &doctorEnv{statusFn: defaultDaemonStatus, daemonAddr: addr}
}

// TestCheckControlPlanePort_DaemonAnswers is the ordinary path, named
// first because none of the guards below may buy their honesty by
// breaking it.
func TestCheckControlPlanePort_DaemonAnswers(t *testing.T) {
	fake := newControlFake()
	fake.active = &vibev1.Status{Running: true, Ready: true, Profile: "code"}
	addr := serveControlOnTCP(t, fake)

	presence, profile := daemonPresenceUnknown, ""
	r := checkControlPlanePortAt(t.Context(), doctorEnvAt(addr), addr, &presence, &profile)
	if r.Status != statusOK {
		t.Fatalf("status = %v, want OK; msg=%q", r.Status, r.Message)
	}
	if presence != daemonPresenceRunning {
		t.Errorf("presence = %v, want running", presence)
	}
	if profile != "code" {
		t.Errorf("profile = %q, want code", profile)
	}
}

// TestCheckControlPlanePort_SlowDaemonIsNotAPortThief is the defect.
//
// The row gave statusFn one budget and turned EVERY error into
// "FAIL: in use by another process", then left the daemon flag false — so
// one unanswered probe produced three claims, two of them about a machine
// nobody had looked at: a port conflict on :9001, `daemon — not running`,
// and a stolen :9000. This is the diagnostic command, so the false
// definite costs more here than anywhere else in the CLI: it sends the
// operator hunting a conflict that does not exist.
//
// The rig drives a TIMEOUT rather than a refusal: a real ControlService on
// a real TCP listener, up and holding an active profile, answering Status
// slower than the budget. A refused connection proves nothing here — it is
// the case that was always handled, and it is asserted separately.
func TestCheckControlPlanePort_SlowDaemonIsNotAPortThief(t *testing.T) {
	fake := newControlFake()
	fake.active = &vibev1.Status{Running: true, Ready: true, Profile: "code"}
	fake.statusDelay = doctorPingBudget * 2
	addr := serveControlOnTCP(t, fake)

	presence, profile := daemonPresenceUnknown, ""
	r := checkControlPlanePortAt(t.Context(), doctorEnvAt(addr), addr, &presence, &profile)
	if r.Status == statusFail {
		t.Errorf("status = FAIL for a daemon that is UP and merely slow; msg=%q", r.Message)
	}
	if r.Status != statusUnknown {
		t.Fatalf("status = %v, want UNKNOWN — the probe established nothing; msg=%q", r.Status, r.Message)
	}
	if strings.Contains(r.Message, "another process") {
		t.Errorf("message names a process nobody looked at: %q", r.Message)
	}
	if !strings.Contains(r.Message, "no answer") {
		t.Errorf("message = %q, want it to name the unanswered probe rather than assert a holder", r.Message)
	}
	if presence != daemonPresenceUnknown {
		t.Errorf("presence = %v, want unknown — a spent budget is evidence of nothing, and the "+
			"daemon and proxy rows read this value", presence)
	}
}

// TestDoctorCascade_OneSlowProbeDoesNotProduceThreeClaims is the defect
// as an operator meets it: not one wrong row, but a report.
//
// The three rows are computed in three different functions and the middle
// one — a bool that had simply never been set — is what carried the wrong
// answer into the other two. So the assertion is on the report: a daemon
// that is UP, holding a model, and slow to answer must not produce a port
// conflict on :9001, a stopped daemon, and a stolen :9000. All three used
// to appear, and the operator's next hour goes to a `lsof -i :9000` that
// finds vibe's own proxy.
func TestDoctorCascade_OneSlowProbeDoesNotProduceThreeClaims(t *testing.T) {
	fake := newControlFake()
	fake.active = &vibev1.Status{Running: true, Ready: true, Profile: "code"}
	fake.statusDelay = doctorPingBudget * 2
	control := serveControlOnTCP(t, fake)

	// A second held port standing in for :9000 — the proxy the daemon
	// under test is serving on.
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxyLn.Close()

	presence, profile := daemonPresenceUnknown, ""
	rows := []checkResult{
		checkControlPlanePortAt(t.Context(), doctorEnvAt(control), control, &presence, &profile),
		checkProxyPortAt(proxyLn.Addr().String(), presence, false),
		daemonRow(presence, profile),
	}
	for _, r := range rows {
		if r.Status == statusFail {
			t.Errorf("[FAIL] %s — %s: a definite fault, from a probe that ran out of time", r.Name, r.Message)
		}
		if strings.Contains(r.Message, "another process") {
			t.Errorf("[%s] %s — %s: names a process nobody looked at", r.Status.tag(), r.Name, r.Message)
		}
		if r.Name == "daemon" && r.Message == "not running" {
			t.Errorf("the daemon row claims a stopped daemon that is up with %q resident", "code")
		}
	}
	if doctorOutcome(rows) == nil {
		t.Error("the report exited 0 — an incomplete report is not a clean one")
	}
	if got := ExitCode(doctorOutcome(rows)); got != doctorExitUnknown {
		t.Errorf("exit code = %d, want %d (the report is incomplete, not the box broken)", got, doctorExitUnknown)
	}
}

// TestCheckControlPlanePort_StillNamesAGenuineThief is the calibration
// half. The fix must not buy its honesty by going quiet on the case the
// row exists for: something IS listening on :9001 and it is not a vibe
// daemon. That stays FAIL, with the same words.
func TestCheckControlPlanePort_StillNamesAGenuineThief(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer ts.Close()
	addr := strings.TrimPrefix(ts.URL, "http://")

	presence, profile := daemonPresenceUnknown, ""
	r := checkControlPlanePortAt(t.Context(), doctorEnvAt(addr), addr, &presence, &profile)
	if r.Status != statusFail {
		t.Fatalf("status = %v, want FAIL — this is a real port conflict; msg=%q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "in use by another process") {
		t.Errorf("message = %q, want the conflict named", r.Message)
	}
	// And the presence stays DEFINITE here: the daemon binds :9001 at
	// startup and cannot start when it is taken, so a holder that does not
	// speak vibe's control plane means no daemon is serving. Going vague
	// about that would be the fix buying its honesty with a real claim.
	if presence != daemonPresenceStopped {
		t.Errorf("presence = %v, want stopped", presence)
	}
}

// TestCheckControlPlanePort_ARefusedCredentialIsNotAPortConflict is the
// third outcome, and the one `vibe fleet doctor` already models (C15): a
// cell that ANSWERS and rejects the key is serving, and reporting "no
// answer" sends the operator to the wrong box entirely. Here the same
// mistake sends them to a port conflict when the fix is a token — the
// exact shape of a bind_all daemon started with a credential this shell
// cannot resolve, which the row's own comment says it was written to
// avoid.
func TestCheckControlPlanePort_ARefusedCredentialIsNotAPortConflict(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer ts.Close()
	addr := strings.TrimPrefix(ts.URL, "http://")

	presence, profile := daemonPresenceUnknown, ""
	r := checkControlPlanePortAt(t.Context(), doctorEnvAt(addr), addr, &presence, &profile)
	if r.Status != statusUnknown {
		t.Fatalf("status = %v, want UNKNOWN; msg=%q", r.Status, r.Message)
	}
	if strings.Contains(r.Message, "another process") {
		t.Errorf("message = %q — the holder answered; what is wrong is the credential, not the port", r.Message)
	}
	if !strings.Contains(r.Message, "credential") || !strings.Contains(r.Message, "VIBE_TOKEN") {
		t.Errorf("message = %q, want the remedy named", r.Message)
	}
	if presence != daemonPresenceUnknown {
		t.Errorf("presence = %v, want unknown", presence)
	}
}

// A free port is the ONE piece of evidence that proves the daemon is not
// running: we held it ourselves. The probe must not even be attempted.
func TestCheckControlPlanePort_AFreePortIsTheOnlyProofOfAbsence(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	env := &doctorEnv{
		daemonAddr: addr,
		statusFn: func(context.Context, string) (string, error) {
			t.Fatal("a port we just bound ourselves was probed for a daemon")
			return "", nil
		},
	}
	presence, profile := daemonPresenceUnknown, ""
	r := checkControlPlanePortAt(t.Context(), env, addr, &presence, &profile)
	if r.Status != statusOK || r.Message != "free" {
		t.Fatalf("r = %+v, want OK/free", r)
	}
	if presence != daemonPresenceStopped {
		t.Errorf("presence = %v, want stopped", presence)
	}
}

// ─── the daemon row ─────────────────────────────────────────────────────────

// The cascade's second claim. `daemon — not running` is a definite
// statement about a process, and it was printed from a flag that stayed
// false whenever the probe failed to find anything out.
func TestDaemonRow_AnUnansweredProbeIsNotAStoppedDaemon(t *testing.T) {
	r := daemonRow(daemonPresenceUnknown, "")
	if r.Status != statusUnknown {
		t.Fatalf("status = %v, want UNKNOWN; msg=%q", r.Status, r.Message)
	}
	if strings.Contains(r.Message, "not running") && !strings.Contains(r.Message, "is a claim") {
		t.Errorf("message = %q, want no bare assertion that the daemon is stopped", r.Message)
	}
}

func TestDaemonRow_TheTwoAnswersItCanActuallyGive(t *testing.T) {
	stopped := daemonRow(daemonPresenceStopped, "")
	if stopped.Status != statusInfo || stopped.Message != "not running" {
		t.Errorf("stopped row = %+v, want INFO/not running", stopped)
	}
	running := daemonRow(daemonPresenceRunning, "code")
	if running.Status != statusOK || !strings.Contains(running.Message, "code") {
		t.Errorf("running row = %+v, want OK naming the profile", running)
	}
	idle := daemonRow(daemonPresenceRunning, "")
	if idle.Status != statusOK || !strings.Contains(idle.Message, "no active profile") {
		t.Errorf("idle row = %+v, want OK with no profile", idle)
	}
}

// ─── the exit status ────────────────────────────────────────────────────────

// UNKNOWN is worth a level rather than just a kinder sentence because a
// wrapper reads the STATUS: "this box is broken" and "I could not find
// out" are different facts, which is the whole distinction `vibe fleet
// doctor` has drawn since C13 and this command was collapsing.
func TestDoctorOutcome_AnIncompleteReportIsNotAPassAndNotAFailure(t *testing.T) {
	row := func(s checkStatus) checkResult { return checkResult{Name: "x", Status: s} }

	if err := doctorOutcome([]checkResult{row(statusOK), row(statusWarn), row(statusInfo)}); err != nil {
		t.Errorf("a clean report exits %v, want 0 — WARN must not become fatal", err)
	}
	unknown := doctorOutcome([]checkResult{row(statusOK), row(statusUnknown)})
	if got := ExitCode(unknown); got != doctorExitUnknown {
		t.Errorf("exit code = %d, want %d (the report is incomplete)", got, doctorExitUnknown)
	}
	// A FAIL outranks an UNKNOWN: something definite is wrong and that is
	// where the operator should be sent first.
	both := doctorOutcome([]checkResult{row(statusUnknown), row(statusFail)})
	if !errors.Is(both, errDoctorFailed) {
		t.Errorf("a report with a FAIL returned %v, want the FAIL sentinel", both)
	}
}

func TestCheckStatus_UnknownHasItsOwnHeading(t *testing.T) {
	// GR10: "not attempted" and "not possible" must never share a heading
	// with an answer. Nothing else may render as UNKN either.
	seen := map[string]checkStatus{}
	for _, s := range []checkStatus{statusOK, statusWarn, statusFail, statusInfo, statusUnknown} {
		if prev, dup := seen[s.tag()]; dup {
			t.Errorf("%v and %v share the heading %q", prev, s, s.tag())
		}
		seen[s.tag()] = s
	}
	if statusUnknown.tag() == "[????]" {
		t.Error("statusUnknown fell through to the default tag")
	}
}

// ─── the proxy row ──────────────────────────────────────────────────────────

func TestCheckProxyPort_DaemonHoldsIt(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, bindErr := tryBind(ln.Addr().String())
	if !isAddrInUse(bindErr) {
		t.Fatalf("expected address-in-use, got %v", bindErr)
	}
	r := checkProxyPortAt(ln.Addr().String(), daemonPresenceRunning, false)
	if r.Status != statusOK {
		t.Errorf("status = %v, want OK; msg=%q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "vibe daemon") {
		t.Errorf("message = %q, want daemon attribution", r.Message)
	}
}

func TestCheckProxyPortAt_InUseNoDaemon_Fails(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	r := checkProxyPortAt(ln.Addr().String(), daemonPresenceStopped, false)
	if r.Status != statusFail {
		t.Errorf("status = %v, want FAIL; msg=%q", r.Status, r.Message)
	}
}

// The cascade's third claim, and the most expensive one: :9000 is the
// port the model is served on, so "in use by another process" reads as
// "something has stolen your inference port". It was printed from the
// same unset flag — which, when :9001 does not answer, most likely means
// the holder of :9000 is vibe's own proxy.
func TestCheckProxyPortAt_AnUnattributedHolderIsNotAThief(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	r := checkProxyPortAt(ln.Addr().String(), daemonPresenceUnknown, false)
	if r.Status == statusFail {
		t.Errorf("status = FAIL from a :9001 probe that established nothing; msg=%q", r.Message)
	}
	if r.Status != statusUnknown {
		t.Fatalf("status = %v, want UNKNOWN; msg=%q", r.Status, r.Message)
	}
	if strings.Contains(r.Message, "another process") {
		t.Errorf("message = %q — nothing here identified the holder", r.Message)
	}
}

// disable_proxy inverts the check: an external router holding the port is
// the healthy state, not a conflict. It is decided before the daemon
// presence is consulted, so it must hold for all three.
func TestCheckProxyPortAt_DisabledExternalRouterHoldsIt(t *testing.T) {
	for _, presence := range []daemonPresence{daemonPresenceUnknown, daemonPresenceRunning, daemonPresenceStopped} {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		r := checkProxyPortAt(ln.Addr().String(), presence, true)
		_ = ln.Close()
		if r.Status != statusOK {
			t.Errorf("presence %v: status = %v, want OK; msg=%q", presence, r.Status, r.Message)
		}
		if !strings.Contains(r.Message, "external router") {
			t.Errorf("presence %v: message = %q, want external-router attribution", presence, r.Message)
		}
	}
}

func TestCheckProxyPortAt_DisabledButFree_Warns(t *testing.T) {
	// Grab a free port, then release it so the check sees it unbound.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	r := checkProxyPortAt(addr, daemonPresenceStopped, true)
	if r.Status != statusWarn {
		t.Errorf("status = %v, want WARN (router expected but absent); msg=%q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "disable_proxy") {
		t.Errorf("message = %q, want disable_proxy mention", r.Message)
	}
}

// setDoctorFixtureDirs points the XDG dirs at a fresh temp tree and
// returns the (created) backends + profiles dirs, so checks that read
// paths.BackendsDir()/paths.ProfilesDir() see a controlled inventory.
func setDoctorFixtureDirs(t *testing.T) (backendsDir, profilesDir string) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "c"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "s"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(tmp, "r"))
	backendsDir = paths.BackendsDir()
	profilesDir = paths.ProfilesDir()
	for _, d := range []string{backendsDir, profilesDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return backendsDir, profilesDir
}

// TestCheckCommonPorts_DerivedFromDisk asserts the probe list comes from
// the on-disk backend/profile inventory: a port declared in a backend
// yaml and held by a live listener must be reported in-use, labeled with
// the declaring file.
func TestCheckCommonPorts_DerivedFromDisk(t *testing.T) {
	backendsDir, _ := setDoctorFixtureDirs(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	backend := fmt.Sprintf(`name: embed
backend:
  http_server:
    image: ghcr.io/example/embed:latest
    port: %d
`, port)
	if err := os.WriteFile(filepath.Join(backendsDir, "embed.yaml"), []byte(backend), 0o644); err != nil {
		t.Fatal(err)
	}
	r := checkCommonPorts()
	if r.Name != "common ports" {
		t.Errorf("Name = %q, want 'common ports'", r.Name)
	}
	if strings.Contains(r.Message, "could not probe") {
		t.Errorf("a port held by this test's own listener was reported unprobeable: %q", r.Message)
	}
	if r.Status != statusInfo {
		t.Fatalf("status = %v, want INFO (declared port is bound); msg=%q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, fmt.Sprintf(":%d", port)) {
		t.Errorf("message missing bound port %d: %q", port, r.Message)
	}
	if !strings.Contains(r.Message, "backend embed") {
		t.Errorf("message missing declaring-file label: %q", r.Message)
	}
}

// The third call site of isAddrInUse, and the one that never asked it:
// `if ok, _ := tryBind(...); ok` discarded the error, so EVERY listen
// failure became "in use". A declared loopback port under 1024 fails with
// `bind: permission denied` as an ordinary user — measured — and this row,
// whose entire job is telling an operator which ports are taken, reported
// it as taken. Both sibling port checks already separate the two.
func TestCheckCommonPorts_APortWeCouldNotProbeIsNotReportedAsInUse(t *testing.T) {
	denied := &net.OpError{Op: "listen", Net: "tcp", Err: errors.New("permission denied")}
	ports := map[int][]string{
		80:   {"profile front"},
		9000: {"backend pi"},
		9999: {"profile spare"},
	}
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	r := checkCommonPortsWith(ports, func(addr string) (bool, error) {
		switch {
		case strings.HasSuffix(addr, ":80"):
			return false, denied
		case strings.HasSuffix(addr, ":9000"):
			// A genuine conflict, so the calibration half rides along:
			// the fix must not go quiet about ports that ARE taken.
			_, bindErr := tryBind(held.Addr().String())
			return false, bindErr
		default:
			return true, nil
		}
	})

	// The claim is made in the "in use:" clause, so that is the clause the
	// unprobeable port must be absent from.
	for _, clause := range strings.Split(r.Message, "; ") {
		if strings.HasPrefix(clause, "in use:") && strings.Contains(clause, ":80 (profile front)") {
			t.Fatalf("a port we were not allowed to bind is reported as in use: %q", r.Message)
		}
	}
	if !strings.Contains(r.Message, "could not probe") {
		t.Errorf("message = %q, want the unprobeable port called out as unprobeable", r.Message)
	}
	if !strings.Contains(r.Message, "permission denied") {
		t.Errorf("message = %q, want the reason the probe failed", r.Message)
	}
	if !strings.Contains(r.Message, "in use: :9000 (backend pi)") {
		t.Errorf("message = %q, want the REAL conflict still named", r.Message)
	}
	if r.Status != statusInfo {
		t.Errorf("status = %v, want INFO (this row never fails a box)", r.Status)
	}
}

// And the OK path must stay reachable, or the fix would have bought its
// honesty by never being able to say "all clear".
func TestCheckCommonPorts_AllFreeIsStillOK(t *testing.T) {
	r := checkCommonPortsWith(map[int][]string{8080: {"Open WebUI"}},
		func(string) (bool, error) { return true, nil })
	if r.Status != statusOK || !strings.Contains(r.Message, "8080 free") {
		t.Fatalf("r = %+v, want OK/8080 free", r)
	}
}

func TestLoopbackPort(t *testing.T) {
	cases := []struct {
		url  string
		want int
	}{
		{"http://127.0.0.1:8080/health", 8080},
		{"http://localhost:14001/", 14001},
		{"http://example.com:8080/", 0}, // non-loopback must not be probed
		{"http://127.0.0.1/", 0},        // no explicit port
		{"not a url", 0},
	}
	for _, c := range cases {
		if got := loopbackPort(c.url); got != c.want {
			t.Errorf("loopbackPort(%q) = %d, want %d", c.url, got, c.want)
		}
	}
}

func TestIsAddrInUse(t *testing.T) {
	if isAddrInUse(nil) {
		t.Fatal("nil should not be addr-in-use")
	}
	if !isAddrInUse(errors.New("listen tcp: bind: address already in use")) {
		t.Fatal("standard error text should match")
	}
	if isAddrInUse(errors.New("listen tcp: some other error")) {
		t.Fatal("unrelated error should not match")
	}
}

// ─── profiles ───────────────────────────────────────────────────────────────

func TestCheckProfilesAt_DirMissing(t *testing.T) {
	r := checkProfilesAt(filepath.Join(t.TempDir(), "nope"))
	if r.Status != statusWarn {
		t.Fatalf("status = %v, want WARN", r.Status)
	}
}

func TestCheckProfilesAt_EmptyDir(t *testing.T) {
	r := checkProfilesAt(t.TempDir())
	if r.Status != statusWarn {
		t.Fatalf("status = %v, want WARN", r.Status)
	}
}

func TestCheckProfilesAt_ValidAndInvalid(t *testing.T) {
	dir := t.TempDir()
	// Stub model file for the valid profile.
	model := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(model, []byte("stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	good := fmt.Sprintf(`name: testprof
backend:
  llama_server:
    path: %s
    alias: testalias
    context: 4096
frontend:
  kind: external
  write_file: /tmp/opencode.json
  template:
    foo: bar
`, model)
	if err := os.WriteFile(filepath.Join(dir, "good.yaml"), []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte("not: a: profile:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := checkProfilesAt(dir)
	if r.Status != statusWarn {
		t.Fatalf("status = %v, want WARN (mixed valid+invalid); msg=%q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "testprof") {
		t.Fatalf("message missing valid profile name: %q", r.Message)
	}
	if !strings.Contains(r.Message, "bad.yaml") {
		t.Fatalf("message missing invalid filename: %q", r.Message)
	}
}

func TestCheckProfilesAt_AllInvalid(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte("bogus: ::: bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := checkProfilesAt(dir)
	if r.Status != statusFail {
		t.Fatalf("status = %v, want FAIL", r.Status)
	}
}

// ─── backends ───────────────────────────────────────────────────────────────

// TestCheckBackends_NoneSkipped asserts the check disappears entirely for
// users who don't use the backends/ abstraction — no noise.
func TestCheckBackends_NoneSkipped(t *testing.T) {
	_, profilesDir := setDoctorFixtureDirs(t)
	if _, ok := checkBackends(profilesDir); ok {
		t.Fatal("expected ok=false when no backends are defined")
	}
}

// TestCheckBackends_InvalidAndUnreferenced covers the check's two reasons
// to exist: a broken backends/<name>.yaml (which otherwise only fails
// mid-pipeline) and a valid backend no profile references (capability-only
// target, invisible to checkProfilesAt).
func TestCheckBackends_InvalidAndUnreferenced(t *testing.T) {
	backendsDir, profilesDir := setDoctorFixtureDirs(t)
	good := `name: embed
backend:
  http_server:
    image: ghcr.io/example/embed:latest
    port: 18099
`
	if err := os.WriteFile(filepath.Join(backendsDir, "embed.yaml"), []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backendsDir, "bad.yaml"), []byte("bogus: ::: bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, ok := checkBackends(profilesDir)
	if !ok {
		t.Fatal("expected ok=true when backends exist")
	}
	if r.Status != statusWarn {
		t.Fatalf("status = %v, want WARN (invalid backend present); msg=%q", r.Status, r.Message)
	}
	for _, want := range []string{"embed", "bad", "capability-only"} {
		if !strings.Contains(r.Message, want) {
			t.Errorf("message missing %q: %q", want, r.Message)
		}
	}
}

// ─── docker ─────────────────────────────────────────────────────────────────

// TestCheckDocker_SkippedWithoutDockerProfiles asserts machines with no
// docker-using profiles see no output at all.
func TestCheckDocker_SkippedWithoutDockerProfiles(t *testing.T) {
	_, profilesDir := setDoctorFixtureDirs(t)
	env := &doctorEnv{lookPath: func(string) (string, error) {
		t.Fatal("lookPath should not be called when nothing needs docker")
		return "", nil
	}}
	if _, ok := checkDockerForProfiles(context.Background(), env, profilesDir); ok {
		t.Fatal("expected ok=false when nothing on disk needs docker")
	}
}

// TestCheckDocker_WarnWhenMissing asserts a WARN (never FAIL) naming the
// docker-needing definition when the docker CLI is absent.
func TestCheckDocker_WarnWhenMissing(t *testing.T) {
	backendsDir, profilesDir := setDoctorFixtureDirs(t)
	backend := `name: tts
backend:
  http_server:
    image: ghcr.io/example/tts:latest
    port: 18098
`
	if err := os.WriteFile(filepath.Join(backendsDir, "tts.yaml"), []byte(backend), 0o644); err != nil {
		t.Fatal(err)
	}
	env := &doctorEnv{lookPath: func(string) (string, error) { return "", errors.New("not found") }}
	r, ok := checkDockerForProfiles(context.Background(), env, profilesDir)
	if !ok {
		t.Fatal("expected ok=true when a docker-mode backend exists")
	}
	if r.Status != statusWarn {
		t.Fatalf("status = %v, want WARN", r.Status)
	}
	if !strings.Contains(r.Message, "backend tts") {
		t.Errorf("message missing the docker-needing definition: %q", r.Message)
	}
}

// TestCheckDocker_DaemonUnreachable asserts a present CLI whose daemon
// doesn't answer `docker info` still degrades to WARN with the error.
func TestCheckDocker_DaemonUnreachable(t *testing.T) {
	backendsDir, profilesDir := setDoctorFixtureDirs(t)
	backend := `name: tts
backend:
  http_server:
    image: ghcr.io/example/tts:latest
    port: 18098
`
	if err := os.WriteFile(filepath.Join(backendsDir, "tts.yaml"), []byte(backend), 0o644); err != nil {
		t.Fatal(err)
	}
	env := &doctorEnv{
		lookPath: func(string) (string, error) { return "/usr/bin/docker", nil },
		run: func(context.Context, string, ...string) ([]byte, error) {
			return nil, errors.New("cannot connect to the docker daemon")
		},
	}
	r, ok := checkDockerForProfiles(context.Background(), env, profilesDir)
	if !ok || r.Status != statusWarn {
		t.Fatalf("ok=%v status=%v, want ok=true WARN; msg=%q", ok, r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "docker daemon") {
		t.Errorf("message missing run error: %q", r.Message)
	}
}

// ─── mcps ───────────────────────────────────────────────────────────────────

func TestCheckMCPsAt_Missing(t *testing.T) {
	r := checkMCPsAt(filepath.Join(t.TempDir(), "nope"))
	if r.Status != statusInfo {
		t.Fatalf("status = %v, want INFO", r.Status)
	}
	if !strings.Contains(r.Message, "0 definitions") {
		t.Fatalf("message = %q", r.Message)
	}
}

func TestCheckMCPsAt_Counts(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.yaml", "b.yaml", "ignored.txt"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("name: x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := checkMCPsAt(dir)
	if r.Status != statusInfo {
		t.Fatalf("status = %v, want INFO", r.Status)
	}
	if !strings.Contains(r.Message, "2 definitions") {
		t.Fatalf("message = %q", r.Message)
	}
}

// ─── gpu ────────────────────────────────────────────────────────────────────

func TestCheckGPU_NoNvidiaSmi(t *testing.T) {
	env := &doctorEnv{lookPath: func(string) (string, error) { return "", errors.New("x") }}
	r := checkGPU(context.Background(), env)
	if r.Status != statusInfo {
		t.Fatalf("status = %v, want INFO", r.Status)
	}
}

func TestCheckGPU_Output(t *testing.T) {
	env := &doctorEnv{
		lookPath: func(string) (string, error) { return "/usr/bin/nvidia-smi", nil },
		run: func(context.Context, string, ...string) ([]byte, error) {
			return []byte("NVIDIA GeForce RTX 5090, 30447 MiB\n"), nil
		},
	}
	r := checkGPU(context.Background(), env)
	if r.Status != statusInfo {
		t.Fatalf("status = %v, want INFO", r.Status)
	}
	if !strings.Contains(r.Message, "RTX 5090") {
		t.Fatalf("message = %q", r.Message)
	}
}

func TestCheckGPU_RunFails(t *testing.T) {
	env := &doctorEnv{
		lookPath: func(string) (string, error) { return "/usr/bin/nvidia-smi", nil },
		run: func(context.Context, string, ...string) ([]byte, error) {
			return nil, errors.New("driver not loaded")
		},
	}
	r := checkGPU(context.Background(), env)
	if r.Status != statusWarn {
		t.Fatalf("status = %v, want WARN", r.Status)
	}
}

// ─── our budget vs the tool's failure ───────────────────────────────────────

// spentBudget returns a context whose deadline has already passed, so the
// derived per-check context is born expired and the check takes the
// timed-out branch without a test waiting three to ten real seconds for
// it. The stub runner still returns its canned error, which is exactly the
// production shape: exec.CommandContext kills the child and reports
// `signal: killed` — measured, errors.Is(err, context.DeadlineExceeded) is
// FALSE for that — so the error alone cannot tell these rows apart and the
// context is what has to be asked.
func spentBudget(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	t.Cleanup(cancel)
	return ctx
}

func killedRunner(context.Context, string, ...string) ([]byte, error) {
	return nil, errors.New("signal: killed")
}

// A driver in a bad state makes nvidia-smi HANG rather than exit — the
// single most recognisable GPU symptom there is — and it arrived as
// "nvidia-smi failed: signal: killed", naming our own kill as the tool's
// failure and hiding the one detail that identifies the fault.
func TestCheckGPU_AHungNvidiaSmiIsNotAFailedOne(t *testing.T) {
	env := &doctorEnv{
		lookPath: func(string) (string, error) { return "/usr/bin/nvidia-smi", nil },
		run:      killedRunner,
	}
	r := checkGPU(spentBudget(t), env)
	if r.Status != statusWarn {
		t.Fatalf("status = %v, want WARN; msg=%q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "did not finish within "+nvidiaSMIBudget.String()) {
		t.Errorf("message = %q, want our own budget named as the reason", r.Message)
	}
	if strings.Contains(r.Message, "signal: killed") {
		t.Errorf("message = %q — `signal: killed` is our kill, reported as the tool's exit", r.Message)
	}
}

// `docker info` on a busy host routinely takes longer than the budget with
// the daemon perfectly healthy, and the row guessed a cause anyway.
func TestCheckDockerForProfiles_OurTimeoutDoesNotClaimTheDaemonIsDown(t *testing.T) {
	backendsDir, profilesDir := setDoctorFixtureDirs(t)
	backend := `name: tts
backend:
  http_server:
    image: ghcr.io/example/tts:latest
    port: 18098
`
	if err := os.WriteFile(filepath.Join(backendsDir, "tts.yaml"), []byte(backend), 0o644); err != nil {
		t.Fatal(err)
	}
	env := &doctorEnv{
		lookPath: func(string) (string, error) { return "/usr/bin/docker", nil },
		run:      killedRunner,
	}
	r, ok := checkDockerForProfiles(spentBudget(t), env, profilesDir)
	if !ok {
		t.Fatal("a compose profile is on disk, so the row must be applicable — the assertions below would be vacuous")
	}
	if strings.Contains(r.Message, "daemon not running?") {
		t.Errorf("message = %q — that is a guess at a cause we have no evidence for", r.Message)
	}
	if !strings.Contains(r.Message, "did not finish within "+dockerInfoBudget.String()) {
		t.Errorf("message = %q, want our own budget named", r.Message)
	}
	if r.Status != statusWarn {
		t.Errorf("status = %v, want WARN — a slow docker must not fail the box", r.Status)
	}
}

func TestCheckLlamaVersion_OurTimeoutIsNotANonZeroExit(t *testing.T) {
	env := &doctorEnv{
		lookPath: func(string) (string, error) { return "/usr/bin/llama-server", nil },
		run:      killedRunner,
	}
	r, ok := checkLlamaVersion(spentBudget(t), env, []string{"profile coder"})
	if !ok {
		t.Fatal("the row must be applicable here")
	}
	if strings.Contains(r.Message, "exited non-zero") {
		t.Errorf("message = %q — a claim about the binary, made from our own deadline", r.Message)
	}
	if !strings.Contains(r.Message, "did not finish within "+llamaVersionBudget.String()) {
		t.Errorf("message = %q, want our own budget named", r.Message)
	}
}

// The worst of the four, because it manufactures an OK rather than a
// misleading WARN: the error was discarded outright and
// firstNonEmptyLine of a FAILED run went straight into "logged in as …".
func TestCheckHFAuth_AFailedWhoamiIsNotALogin(t *testing.T) {
	env := &doctorEnv{
		lookPath: func(string) (string, error) { return "/usr/bin/hf", nil },
		run: func(context.Context, string, ...string) ([]byte, error) {
			return []byte("Traceback (most recent call last):\n  File \"hf\", line 1\n"),
				errors.New("exit status 1")
		},
	}
	r := checkHFAuth(context.Background(), env)
	if r.Status == statusOK {
		t.Fatalf("an OK — the strongest claim in this report — was made out of an error message: %q", r.Message)
	}
	if strings.Contains(r.Message, "logged in as") {
		t.Errorf("message = %q, want no claim about who is logged in", r.Message)
	}
	if !strings.Contains(r.Message, "exit status 1") {
		t.Errorf("message = %q, want the failure reported", r.Message)
	}
}

func TestCheckHFAuth_OurTimeoutIsNotTheToolsFailure(t *testing.T) {
	env := &doctorEnv{
		lookPath: func(string) (string, error) { return "/usr/bin/hf", nil },
		run:      killedRunner,
	}
	r := checkHFAuth(spentBudget(t), env)
	if !strings.Contains(r.Message, "did not finish within "+hfAuthBudget.String()) {
		t.Errorf("message = %q, want our own budget named", r.Message)
	}
	if r.Status != statusWarn {
		t.Errorf("status = %v, want WARN", r.Status)
	}
}

// ─── helpers ────────────────────────────────────────────────────────────────

func TestFirstNonEmptyLine(t *testing.T) {
	if got := firstNonEmptyLine([]byte("\n\nhello\nworld\n")); got != "hello" {
		t.Fatalf("got %q", got)
	}
	if got := firstNonEmptyLine([]byte("")); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestNonEmptyLines(t *testing.T) {
	got := nonEmptyLines([]byte("a\n\nb\n c \n"))
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("got %v", got)
	}
}

func TestStatusTag(t *testing.T) {
	cases := []struct {
		s    checkStatus
		want string
	}{
		{statusOK, "[ OK ]"},
		{statusWarn, "[WARN]"},
		{statusFail, "[FAIL]"},
		{statusInfo, "[INFO]"},
	}
	for _, c := range cases {
		if got := c.s.tag(); got != c.want {
			t.Fatalf("tag(%d) = %q, want %q", c.s, got, c.want)
		}
	}
}

// ─── rsvg-convert (vision) ──────────────────────────────────────────────────

// TestCheckRSVG_NoMMProjProfiles asserts the check returns ok=false when
// no installed profile declares an mmproj path; we don't want a warning
// for users who only run text-only profiles.
func TestCheckRSVG_NoMMProjProfiles(t *testing.T) {
	dir := t.TempDir()
	// Text-only profile (no mmproj).
	model := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(model, []byte("stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	textProfile := fmt.Sprintf(`name: textonly
backend:
  llama_server:
    path: %s
    alias: t
    context: 1024
frontend:
  kind: external
  write_file: /tmp/x
  template:
    a: 1
`, model)
	if err := os.WriteFile(filepath.Join(dir, "text.yaml"), []byte(textProfile), 0o644); err != nil {
		t.Fatal(err)
	}
	env := &doctorEnv{lookPath: func(string) (string, error) { return "", errors.New("absent") }}
	_, ok := checkRSVGForVision(env, dir)
	if ok {
		t.Fatal("expected ok=false when no mmproj profile installed")
	}
}

// TestCheckRSVG_MissingWithMMProj asserts WARN when an mmproj profile
// exists but rsvg-convert is unavailable. This is the case the user
// explicitly requested coverage for.
func TestCheckRSVG_MissingWithMMProj(t *testing.T) {
	dir := t.TempDir()
	model := filepath.Join(dir, "model.gguf")
	mmproj := filepath.Join(dir, "mmproj.gguf")
	for _, p := range []string{model, mmproj} {
		if err := os.WriteFile(p, []byte("stub"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	visionProfile := fmt.Sprintf(`name: visionprof
backend:
  llama_server:
    path: %s
    mmproj: %s
    alias: v
    context: 1024
frontend:
  kind: external
  write_file: /tmp/x
  template:
    a: 1
`, model, mmproj)
	if err := os.WriteFile(filepath.Join(dir, "vision.yaml"), []byte(visionProfile), 0o644); err != nil {
		t.Fatal(err)
	}
	env := &doctorEnv{lookPath: func(name string) (string, error) {
		if name == "rsvg-convert" {
			return "", errors.New("not found")
		}
		t.Fatalf("unexpected lookup of %q", name)
		return "", nil
	}}
	r, ok := checkRSVGForVision(env, dir)
	if !ok {
		t.Fatal("expected ok=true when mmproj profile present")
	}
	if r.Status != statusWarn {
		t.Fatalf("status = %v, want WARN", r.Status)
	}
	if !strings.Contains(r.Message, "visionprof") {
		t.Errorf("message missing profile name: %q", r.Message)
	}
	if !strings.Contains(r.Message, "librsvg2-bin") {
		t.Errorf("message missing install hint: %q", r.Message)
	}
}

// TestCheckRSVG_PresentWithMMProj asserts OK when both rsvg-convert and
// an mmproj profile are present. Keeps the success path covered so a
// regression to "always warn" would fail.
func TestCheckRSVG_PresentWithMMProj(t *testing.T) {
	dir := t.TempDir()
	model := filepath.Join(dir, "model.gguf")
	mmproj := filepath.Join(dir, "mmproj.gguf")
	for _, p := range []string{model, mmproj} {
		if err := os.WriteFile(p, []byte("stub"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	visionProfile := fmt.Sprintf(`name: visionprof
backend:
  llama_server:
    path: %s
    mmproj: %s
    alias: v
    context: 1024
frontend:
  kind: external
  write_file: /tmp/x
  template:
    a: 1
`, model, mmproj)
	if err := os.WriteFile(filepath.Join(dir, "vision.yaml"), []byte(visionProfile), 0o644); err != nil {
		t.Fatal(err)
	}
	env := &doctorEnv{lookPath: func(name string) (string, error) {
		if name == "rsvg-convert" {
			return "/usr/bin/rsvg-convert", nil
		}
		return "", errors.New("absent")
	}}
	r, ok := checkRSVGForVision(env, dir)
	if !ok {
		t.Fatal("expected ok=true when mmproj profile present")
	}
	if r.Status != statusOK {
		t.Fatalf("status = %v, want OK; msg=%q", r.Status, r.Message)
	}
}

// TestCheckRSVG_MissingProfilesDir guards against a runtime panic when
// $XDG_CONFIG_HOME/vibe/profiles doesn't exist yet (first-run users).
func TestCheckRSVG_MissingProfilesDir(t *testing.T) {
	env := &doctorEnv{lookPath: func(string) (string, error) { return "", errors.New("absent") }}
	_, ok := checkRSVGForVision(env, filepath.Join(t.TempDir(), "nope"))
	if ok {
		t.Fatal("expected ok=false when profiles dir missing")
	}
}

// ─── end-to-end: doctorCmd registers and runs without panicking ─────────────

func TestRunChecks_Smoke(t *testing.T) {
	// Drive runChecks against a tightly stubbed env so the test doesn't
	// depend on host binaries or :9001.
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "c"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "s"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(tmp, "r"))
	env := &doctorEnv{
		lookPath: func(name string) (string, error) {
			return "", errors.New("missing: " + name)
		},
		run: func(context.Context, string, ...string) ([]byte, error) {
			return nil, errors.New("not invoked in test")
		},
		statusFn: func(context.Context, string) (string, error) {
			return "", errors.New("no daemon")
		},
		daemonAddr: "127.0.0.1:9001",
	}
	results := runChecks(context.Background(), env)
	if len(results) == 0 {
		t.Fatal("no results returned")
	}
	// Every result must have a Name; statuses must be one of the known
	// values.
	for _, r := range results {
		if r.Name == "" {
			t.Fatalf("result with empty name: %+v", r)
		}
		switch r.Status {
		case statusOK, statusWarn, statusFail, statusInfo, statusUnknown:
		default:
			t.Fatalf("unrecognised status in result %+v", r)
		}
	}
}
