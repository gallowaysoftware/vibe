package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// stubRun describes a single expected subprocess invocation. The installer
// matches incoming calls in declaration order. Anything mismatched fails the
// test loudly.
type stubRun struct {
	// match is run against the command name + args; returning true means
	// this stub handles the call.
	match func(name string, args []string) bool
	// onCall is invoked when match succeeds. It receives the produced
	// *exec.Cmd so the stub can inspect or replace its behavior.
	onCall func(cmd *exec.Cmd) *exec.Cmd
	// label is printed in failure messages.
	label string
	// hit is set when the stub fires.
	hit bool
}

// fakeRunner builds a commandRunner that walks `expected` in order and
// returns a stub command from the first matching entry. The stub commands
// resolve to /bin/true or /bin/false as configured per-step.
func fakeRunner(t *testing.T, expected []*stubRun) commandRunner {
	t.Helper()
	return func(name string, args ...string) *exec.Cmd {
		for _, e := range expected {
			if e.hit {
				continue
			}
			if e.match(name, args) {
				e.hit = true
				base := exec.Command(trueBin)
				if e.onCall != nil {
					return e.onCall(base)
				}
				return base
			}
		}
		t.Errorf("unexpected exec: %s %v", name, args)
		// Return /bin/false so the test fails fast in the installer too.
		return exec.Command(falseBin)
	}
}

// matchPrefix matches when name matches and the first len(args) of the
// invocation match prefixArgs exactly.
func matchPrefix(wantName string, prefixArgs ...string) func(name string, args []string) bool {
	return func(name string, args []string) bool {
		if name != wantName {
			return false
		}
		if len(args) < len(prefixArgs) {
			return false
		}
		for i, want := range prefixArgs {
			if args[i] != want {
				return false
			}
		}
		return true
	}
}

// newTestEnv constructs an installerEnv pointed at a tmp HOME, with the
// supplied stubs for runCmd and confirm. yes=true short-circuits prompts.
func newTestEnv(t *testing.T, stubs []*stubRun, confirm inputter, yes bool) (*installerEnv, *bytes.Buffer) {
	t.Helper()
	home := t.TempDir()
	cfgHome := t.TempDir()
	var out bytes.Buffer
	env := &installerEnv{
		home:       home,
		configHome: cfgHome,
		runCmd:     fakeRunner(t, stubs),
		confirm:    confirm,
		stdout:     &out,
		stderr:     &out,
		lookPath: func(s string) (string, error) {
			return "/usr/bin/" + s, nil
		},
		yes: yes,
	}
	return env, &out
}

// alwaysYes is a confirm stub.
func alwaysYes(string) bool { return true }
func alwaysNo(string) bool  { return false }

// touchFile creates `path` with `size` zero-bytes. Parent dirs are created.
func touchFile(t *testing.T, path string, size int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if size > 0 {
		if err := f.Truncate(size); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// ─── happy paths ────────────────────────────────────────────────────────────

func TestInstallComfyUI_FreshInstall_YesFlag(t *testing.T) {
	// All steps run: git clone, venv, pip install -r, hf download, profile drop.
	// (pip freeze short-circuit doesn't fire because venv didn't exist before.)
	stubs := []*stubRun{
		{label: "git clone", match: matchPrefix("git", "clone", "--depth", "1")},
		{label: "python3 -m venv", match: matchPrefix("python3", "-m", "venv")},
		{label: "pip install -r", match: func(name string, args []string) bool {
			return strings.HasSuffix(name, "/bin/pip") &&
				len(args) >= 2 && args[0] == "install" && args[1] == "-r"
		}},
		{label: "hf download", match: matchPrefix("hf", "download")},
	}
	env, out := newTestEnv(t, stubs, alwaysYes, true /* yes */)

	if err := installComfyUI(env); err != nil {
		t.Fatalf("installComfyUI: %v", err)
	}
	for _, s := range stubs {
		if !s.hit {
			t.Errorf("expected stub %q to fire", s.label)
		}
	}
	if !strings.Contains(out.String(), "Activate with `vibe start comfyui`") {
		t.Errorf("missing summary line; out=\n%s", out.String())
	}
	// Profile was dropped at expected path with substitution applied.
	profilePath := filepath.Join(env.configHome, "profiles", "comfyui.yaml")
	body, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("profile not written: %v", err)
	}
	wantPath := filepath.Join(env.home, "ComfyUI")
	if !strings.Contains(string(body), wantPath) {
		t.Errorf("profile missing substituted install path %q; body=\n%s", wantPath, body)
	}
	if strings.Contains(string(body), "~/ComfyUI") {
		t.Errorf("profile still contains unsubstituted ~/ComfyUI marker; body=\n%s", body)
	}
}

// ─── short-circuits ─────────────────────────────────────────────────────────

func TestInstallComfyUI_AllSatisfied_NoSubprocessRuns(t *testing.T) {
	// Pre-create main.py, .venv/bin/pip, model file, and profile. No
	// subprocess should fire because every step's post-condition holds —
	// except `pip freeze` (a heuristic check) which we stub to report
	// torch + transformers present.
	stubs := []*stubRun{
		{label: "pip freeze", match: func(name string, args []string) bool {
			return strings.HasSuffix(name, "/bin/pip") &&
				len(args) == 1 && args[0] == "freeze"
		}, onCall: func(cmd *exec.Cmd) *exec.Cmd {
			// Replace with an echo that writes the freeze-style output to stdout.
			r := exec.Command("/bin/sh", "-c", "echo 'torch==2.4.0'; echo 'transformers==4.45.0'")
			return r
		}},
	}
	env, out := newTestEnv(t, stubs, alwaysNo, true)
	installDir := filepath.Join(env.home, "ComfyUI")

	// Stage the filesystem.
	touchFile(t, filepath.Join(installDir, "main.py"), 0)
	touchFile(t, filepath.Join(installDir, ".venv", "bin", "pip"), 0)
	touchFile(t, filepath.Join(installDir, "models", "checkpoints", "sd_xl_turbo_1.0_fp16.safetensors"), 1<<30+1)
	profilePath := filepath.Join(env.configHome, "profiles", "comfyui.yaml")
	if err := os.MkdirAll(filepath.Dir(profilePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profilePath, []byte("name: comfyui\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := installComfyUI(env); err != nil {
		t.Fatalf("installComfyUI: %v", err)
	}
	if !stubs[0].hit {
		t.Errorf("expected pip freeze stub to fire")
	}
	s := out.String()
	for _, want := range []string{
		"[skip] clone",
		"[skip] venv",
		"[skip] pip install",
		"[skip] model",
		"[skip] profile",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in output:\n%s", want, s)
		}
	}
}

// ─── user declines initial confirmation ─────────────────────────────────────

func TestInstallComfyUI_UserDeclines(t *testing.T) {
	env, _ := newTestEnv(t, nil /* no stubs needed */, alwaysNo, false /* !yes */)
	err := installComfyUI(env)
	if err == nil {
		t.Fatal("expected error when user declines")
	}
	if !strings.Contains(err.Error(), "declined") {
		t.Errorf("error %q lacks 'declined'", err)
	}
}

// ─── pip install fails ──────────────────────────────────────────────────────

func TestInstallComfyUI_PipInstallFails(t *testing.T) {
	stubs := []*stubRun{
		{label: "git clone", match: matchPrefix("git", "clone", "--depth", "1")},
		{label: "python3 -m venv", match: matchPrefix("python3", "-m", "venv"),
			onCall: func(_ *exec.Cmd) *exec.Cmd {
				// Need to also create the venv/bin/pip so step 4 finds it
				// when it tries pip freeze. We do that via a sh -c that
				// touches the file too.
				return exec.Command(trueBin)
			}},
		// Note: pip freeze will fail because the pip binary doesn't really
		// exist on disk; the installer treats that as "not satisfied" and
		// proceeds to pip install. We make pip install fail.
		{label: "pip install -r", match: func(name string, args []string) bool {
			return strings.HasSuffix(name, "/bin/pip") &&
				len(args) >= 2 && args[0] == "install" && args[1] == "-r"
		}, onCall: func(_ *exec.Cmd) *exec.Cmd {
			return exec.Command(falseBin)
		}},
	}
	env, _ := newTestEnv(t, stubs, alwaysYes, true)

	err := installComfyUI(env)
	if err == nil {
		t.Fatal("expected pip install failure")
	}
	if !strings.Contains(err.Error(), "pip install") {
		t.Errorf("error doesn't mention pip install: %v", err)
	}
}

// ─── hf download fails ──────────────────────────────────────────────────────

func TestInstallComfyUI_HFDownloadFails(t *testing.T) {
	stubs := []*stubRun{
		{label: "git clone", match: matchPrefix("git", "clone", "--depth", "1")},
		{label: "python3 -m venv", match: matchPrefix("python3", "-m", "venv")},
		{label: "pip install -r", match: func(name string, args []string) bool {
			return strings.HasSuffix(name, "/bin/pip") &&
				len(args) >= 2 && args[0] == "install" && args[1] == "-r"
		}},
		{label: "hf download", match: matchPrefix("hf", "download"),
			onCall: func(_ *exec.Cmd) *exec.Cmd {
				return exec.Command(falseBin)
			}},
	}
	env, _ := newTestEnv(t, stubs, alwaysYes, true)

	err := installComfyUI(env)
	if err == nil {
		t.Fatal("expected hf download failure")
	}
	if !strings.Contains(err.Error(), "hf download") {
		t.Errorf("error doesn't mention hf download: %v", err)
	}
}

// ─── model download declined ────────────────────────────────────────────────

func TestInstallComfyUI_UserSkipsModelDownload(t *testing.T) {
	// User confirms the initial install but declines the model. (yes=false
	// is required to reach the per-step confirm prompts.)
	stubs := []*stubRun{
		{label: "git clone", match: matchPrefix("git", "clone", "--depth", "1")},
		{label: "python3 -m venv", match: matchPrefix("python3", "-m", "venv")},
		{label: "pip install -r", match: func(name string, args []string) bool {
			return strings.HasSuffix(name, "/bin/pip") &&
				len(args) >= 2 && args[0] == "install" && args[1] == "-r"
		}},
		// NOTE: no hf download stub — that step must NOT fire.
	}
	calls := 0
	confirm := func(prompt string) bool {
		calls++
		// 1st prompt: "Install ComfyUI...?"  -> yes
		// 2nd prompt: "Download SDXL-Turbo...?" -> no
		return calls == 1
	}
	env, out := newTestEnv(t, stubs, confirm, false /* !yes */)
	if err := installComfyUI(env); err != nil {
		t.Fatalf("installComfyUI: %v", err)
	}
	if !strings.Contains(out.String(), "model download declined by user") {
		t.Errorf("missing decline message; out=\n%s", out.String())
	}
	if calls != 2 {
		t.Errorf("expected exactly 2 confirm calls, got %d", calls)
	}
}

// ─── profile already exists (no overwrite) ──────────────────────────────────

func TestInstallComfyUI_ProfileExists_NotOverwritten(t *testing.T) {
	stubs := []*stubRun{
		{label: "git clone", match: matchPrefix("git", "clone", "--depth", "1")},
		{label: "python3 -m venv", match: matchPrefix("python3", "-m", "venv")},
		{label: "pip install -r", match: func(name string, args []string) bool {
			return strings.HasSuffix(name, "/bin/pip") &&
				len(args) >= 2 && args[0] == "install" && args[1] == "-r"
		}},
		{label: "hf download", match: matchPrefix("hf", "download")},
	}
	env, out := newTestEnv(t, stubs, alwaysYes, true)
	profilePath := filepath.Join(env.configHome, "profiles", "comfyui.yaml")
	if err := os.MkdirAll(filepath.Dir(profilePath), 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := "# user's bespoke comfyui profile\nname: comfyui\n"
	if err := os.WriteFile(profilePath, []byte(sentinel), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := installComfyUI(env); err != nil {
		t.Fatalf("installComfyUI: %v", err)
	}
	body, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != sentinel {
		t.Errorf("profile was overwritten; want %q got %q", sentinel, body)
	}
	if !strings.Contains(out.String(), "[skip] profile") {
		t.Errorf("missing skip-profile message; out=\n%s", out.String())
	}
}

// ─── pipFreezeHasComfyDeps ─────────────────────────────────────────────────

func TestPipFreezeHasComfyDeps_Present(t *testing.T) {
	tmp := t.TempDir()
	venvPip := filepath.Join(tmp, "pip")
	if err := os.WriteFile(venvPip, []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := &installerEnv{
		runCmd: func(name string, args ...string) *exec.Cmd {
			return exec.Command("/bin/sh", "-c", "echo 'numpy==2.0'; echo 'torch==2.4.0+cu121'; echo 'transformers==4.45.2'")
		},
	}
	ok, err := pipFreezeHasComfyDeps(env, venvPip)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Errorf("expected hasDeps=true with torch + transformers present")
	}
}

func TestPipFreezeHasComfyDeps_OnlyTorchvision(t *testing.T) {
	// Guards against the substring false-positive: torchvision must not
	// satisfy the "has torch" check.
	tmp := t.TempDir()
	venvPip := filepath.Join(tmp, "pip")
	if err := os.WriteFile(venvPip, []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := &installerEnv{
		runCmd: func(name string, args ...string) *exec.Cmd {
			return exec.Command("/bin/sh", "-c", "echo 'torchvision==0.19.0'; echo 'transformers==4.45.2'")
		},
	}
	ok, err := pipFreezeHasComfyDeps(env, venvPip)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Errorf("torchvision alone should not satisfy the heuristic")
	}
}

func TestPipFreezeHasComfyDeps_PipMissing(t *testing.T) {
	env := &installerEnv{
		runCmd: func(name string, args ...string) *exec.Cmd {
			return exec.Command(trueBin)
		},
	}
	_, err := pipFreezeHasComfyDeps(env, "/nonexistent/pip")
	if err == nil {
		t.Fatal("expected error when pip binary missing")
	}
}

// ─── hasLargeFile ──────────────────────────────────────────────────────────

func TestHasLargeFile(t *testing.T) {
	tmp := t.TempDir()
	small := filepath.Join(tmp, "small.bin")
	big := filepath.Join(tmp, "big.bin")
	touchFile(t, small, 100)
	touchFile(t, big, 2<<20)
	if hasLargeFile(small, 1<<20) {
		t.Error("small file should not satisfy 1MiB threshold")
	}
	if !hasLargeFile(big, 1<<20) {
		t.Error("2MiB file should satisfy 1MiB threshold")
	}
	if hasLargeFile(filepath.Join(tmp, "absent"), 1) {
		t.Error("absent file should not satisfy any threshold")
	}
	// Directory: not a regular file.
	if hasLargeFile(tmp, 0) {
		t.Error("directory should not satisfy hasLargeFile")
	}
}

// ─── dispatcher: unknown name fails ─────────────────────────────────────────

func TestRunInstall_UnknownComponent(t *testing.T) {
	// Pick a name that's not currently supported. Both `comfyui` and
	// `llama-cpp` are valid installers; `nonesuch` is not.
	cmd := doctorCmd()
	cmd.SetArgs([]string{"--install", "nonesuch"})
	var stderr bytes.Buffer
	cmd.SetOut(io.Discard)
	cmd.SetErr(&stderr)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for unknown component")
	}
	if !strings.Contains(err.Error(), "unknown component") {
		t.Errorf("wrong error: %v", err)
	}
}

// ─── embedded profile sanity ────────────────────────────────────────────────

func TestEmbeddedProfile_MatchesRepoCopy(t *testing.T) {
	// Guard against drift between profiles/comfyui.example.yaml (the
	// documented source-of-truth users browse on GitHub) and the embedded
	// copy in internal/vibe/cli/comfyui.example.yaml (what the installer
	// writes). They must stay byte-identical.
	repoPath := filepath.Join("..", "..", "..", "profiles", "comfyui.example.yaml")
	want, err := os.ReadFile(repoPath)
	if err != nil {
		t.Skipf("repo copy not readable from test cwd: %v", err)
	}
	if string(want) != comfyuiExampleProfile {
		t.Errorf("embedded profile drifted from %s; re-copy it into internal/vibe/cli/", repoPath)
	}
}

// ─── doctor without --install still runs diagnostics ────────────────────────

func TestDoctorCmd_NoInstallFlag_StillRunsChecks(t *testing.T) {
	// The non-install path of `vibe doctor` must still work. We won't drive
	// the full diagnostic here (cmd_doctor_test.go covers each check) — we
	// just confirm the command's RunE doesn't early-return into the install
	// path when --install is absent. A simple way: register the cobra
	// command, parse no flags, and verify it doesn't try to call the
	// installer (which would error against the real filesystem).
	cmd := doctorCmd()
	// Use a known-bogus XDG home so EnsureDirs writes to a tmp tree, not
	// the real ~/.config. The other checks may FAIL on a fresh tmp, but
	// the command should still return either nil or errDoctorFailed —
	// never our "unknown component" error.
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "c"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "s"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(tmp, "r"))
	cmd.SetArgs([]string{})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()
	if err != nil && !errors.Is(err, errDoctorFailed) {
		// A FAIL on a check would surface as errDoctorFailed; any other
		// error means something is structurally wrong.
		t.Fatalf("doctor without --install returned %v (want nil or errDoctorFailed)", err)
	}
}

// Sanity: the example yaml is non-empty after embedding.
func TestEmbeddedProfile_NonEmpty(t *testing.T) {
	if len(comfyuiExampleProfile) == 0 {
		t.Fatal("comfyuiExampleProfile is empty; go:embed broken?")
	}
	if !strings.Contains(comfyuiExampleProfile, "name: imagegen") {
		t.Errorf("embedded profile missing expected marker; got %d bytes", len(comfyuiExampleProfile))
	}
}

// Helper used in failure messages to dump the runner's command line.
func cmdLine(c *exec.Cmd) string {
	return fmt.Sprintf("%s %s", c.Path, strings.Join(c.Args[1:], " "))
}

var _ = cmdLine // referenced in future debug paths; keep export-safe linter happy.
