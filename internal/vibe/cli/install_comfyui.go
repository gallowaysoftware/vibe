package cli

import (
	"bufio"
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
)

// comfyuiExampleProfile is the canonical ComfyUI example profile shipped in
// the repo at `profiles/comfyui.example.yaml`. We embed it so the installer
// doesn't need to locate the source tree at runtime — the binary is fully
// self-contained.
//
//go:embed comfyui.example.yaml
var comfyuiExampleProfile string

// commandRunner produces an *exec.Cmd ready to be configured (stdout/stderr,
// stdin, env) by the caller and run. Tests stub this to return an *exec.Cmd
// that invokes /bin/true (or false) so we never reach git/pip/hf for real.
type commandRunner func(name string, args ...string) *exec.Cmd

// inputter prompts the user and returns true on a yes answer. The single
// argument is the prompt text (without trailing newline). The default
// implementation reads a line from stdin; tests stub it.
type inputter func(prompt string) bool

// installerEnv bundles everything install_comfyui touches so tests can swap
// the externals out.
type installerEnv struct {
	// home is the user's home directory. Tests set this to a tmp dir.
	home string
	// configHome is $XDG_CONFIG_HOME/vibe (or the resolved default).
	configHome string
	// runCmd produces commands; tests return a stub that resolves to a
	// no-op binary.
	runCmd commandRunner
	// confirm prompts the user; tests stub it to a constant.
	confirm inputter
	// stdout is where streaming subprocess output and progress lines go.
	stdout io.Writer
	// stderr is for surface errors.
	stderr io.Writer
	// lookPath resolves binaries on $PATH (e.g. python3, hf, git).
	lookPath func(string) (string, error)
	// yes short-circuits all confirmation prompts to true.
	yes bool
}

func defaultInstallerEnv(stdout, stderr io.Writer, yes bool) *installerEnv {
	home, _ := os.UserHomeDir()
	return &installerEnv{
		home:       home,
		configHome: paths.ConfigHome(),
		runCmd:     exec.Command,
		confirm:    promptYesNo,
		stdout:     stdout,
		stderr:     stderr,
		lookPath:   exec.LookPath,
		yes:        yes,
	}
}

// promptYesNo reads a line from stdin and returns true iff the trimmed
// answer starts with y or Y. Empty/anything-else is treated as no.
func promptYesNo(prompt string) bool {
	fmt.Print(prompt)
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return false
	}
	line = strings.TrimSpace(line)
	return len(line) > 0 && (line[0] == 'y' || line[0] == 'Y')
}

// installComfyUI walks the install path. Each step is idempotent and skips
// when the post-condition already holds. The function streams subprocess
// output (git clone, pip install, hf download) to env.stdout so the operator
// sees progress for slow steps.
func installComfyUI(env *installerEnv) error {
	installDir := filepath.Join(env.home, "ComfyUI")

	// ── Step 1: confirm. ────────────────────────────────────────────────
	if !env.yes {
		prompt := fmt.Sprintf("Install ComfyUI to %s? (~10GB of deps + model) [y/N] ", installDir)
		if !env.confirm(prompt) {
			fmt.Fprintln(env.stdout, "Aborted by user.")
			return errors.New("install: user declined")
		}
	}

	// ── Step 2: clone. ──────────────────────────────────────────────────
	mainPy := filepath.Join(installDir, "main.py")
	if _, err := os.Stat(mainPy); err == nil {
		fmt.Fprintf(env.stdout, "[skip] clone: %s already exists\n", mainPy)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", mainPy, err)
	} else {
		fmt.Fprintf(env.stdout, "[run ] git clone --depth 1 https://github.com/comfyanonymous/ComfyUI %s\n", installDir)
		cmd := env.runCmd("git", "clone", "--depth", "1",
			"https://github.com/comfyanonymous/ComfyUI", installDir)
		cmd.Stdout = env.stdout
		cmd.Stderr = env.stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git clone: %w", err)
		}
	}

	// ── Step 3: venv. ───────────────────────────────────────────────────
	venvDir := filepath.Join(installDir, ".venv")
	venvPip := filepath.Join(venvDir, "bin", "pip")
	if _, err := os.Stat(venvPip); err == nil {
		fmt.Fprintf(env.stdout, "[skip] venv: %s already exists\n", venvDir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", venvPip, err)
	} else {
		fmt.Fprintf(env.stdout, "[run ] python3 -m venv %s\n", venvDir)
		cmd := env.runCmd("python3", "-m", "venv", venvDir)
		cmd.Stdout = env.stdout
		cmd.Stderr = env.stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("python3 -m venv: %w", err)
		}
	}

	// ── Step 4: pip install -r requirements.txt. ────────────────────────
	// Heuristic: if `pip freeze` already lists torch and transformers,
	// consider it installed. Capture the freeze output so we can scan it;
	// don't stream this one to the user.
	if alreadyInstalled, err := pipFreezeHasComfyDeps(env, venvPip); err == nil && alreadyInstalled {
		fmt.Fprintf(env.stdout, "[skip] pip install: torch + transformers already in venv\n")
	} else {
		reqFile := filepath.Join(installDir, "requirements.txt")
		fmt.Fprintf(env.stdout, "[run ] %s install -r %s (slow; streaming pip output)\n", venvPip, reqFile)
		cmd := env.runCmd(venvPip, "install", "-r", reqFile)
		cmd.Stdout = env.stdout
		cmd.Stderr = env.stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("pip install -r requirements.txt: %w", err)
		}
	}

	// ── Step 5: optional model download. ────────────────────────────────
	modelDir := filepath.Join(installDir, "models", "checkpoints")
	modelPath := filepath.Join(modelDir, "sd_xl_turbo_1.0_fp16.safetensors")
	if hasLargeFile(modelPath, 1<<30 /* 1 GiB */) {
		fmt.Fprintf(env.stdout, "[skip] model: %s already present (>1 GiB)\n", modelPath)
	} else {
		download := env.yes
		if !env.yes {
			download = env.confirm("Download SDXL-Turbo (~7 GB) as default model? Skip if you've got your own. (y/N) ")
		}
		if download {
			if err := os.MkdirAll(modelDir, 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", modelDir, err)
			}
			fmt.Fprintf(env.stdout, "[run ] hf download stabilityai/sdxl-turbo sd_xl_turbo_1.0_fp16.safetensors --local-dir %s\n", modelDir)
			cmd := env.runCmd("hf", "download", "stabilityai/sdxl-turbo",
				"sd_xl_turbo_1.0_fp16.safetensors",
				"--local-dir", modelDir)
			cmd.Stdout = env.stdout
			cmd.Stderr = env.stderr
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("hf download: %w", err)
			}
		} else {
			fmt.Fprintf(env.stdout, "[skip] model download declined by user\n")
		}
	}

	// ── Step 6: drop default profile if absent. ─────────────────────────
	profilePath := filepath.Join(env.configHome, "profiles", "comfyui.yaml")
	if _, err := os.Stat(profilePath); err == nil {
		fmt.Fprintf(env.stdout, "[skip] profile: %s already exists\n", profilePath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", profilePath, err)
	} else {
		if err := os.MkdirAll(filepath.Dir(profilePath), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(profilePath), err)
		}
		// Substitute `~/ComfyUI` (the example default) with the resolved
		// install path. The example uses literal `~/ComfyUI`, which the
		// loader does expand, but rewriting it makes the dropped profile
		// portable and self-describing.
		body := strings.ReplaceAll(comfyuiExampleProfile, "~/ComfyUI", installDir)
		if err := os.WriteFile(profilePath, []byte(body), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", profilePath, err)
		}
		fmt.Fprintf(env.stdout, "[ok  ] profile written to %s\n", profilePath)
	}

	// ── Step 7: summary. ────────────────────────────────────────────────
	fmt.Fprintf(env.stdout, "\nComfyUI installed at %s. Activate with `vibe start comfyui`.\n", installDir)
	return nil
}

// pipFreezeHasComfyDeps runs `<venvPip> freeze` and returns true iff both
// torch and transformers appear in the output. We treat any error from pip
// itself as "not satisfied" — the caller will then run pip install, which
// is itself idempotent.
func pipFreezeHasComfyDeps(env *installerEnv, venvPip string) (bool, error) {
	if _, err := os.Stat(venvPip); err != nil {
		return false, err
	}
	var buf bytes.Buffer
	cmd := env.runCmd(venvPip, "freeze")
	cmd.Stdout = &buf
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return false, err
	}
	freeze := strings.ToLower(buf.String())
	hasTorch := false
	hasTransformers := false
	for _, line := range strings.Split(freeze, "\n") {
		// `pip freeze` emits `pkg==X.Y.Z`. We match on prefix to avoid
		// substring false positives (e.g. torchvision contains torch).
		switch {
		case strings.HasPrefix(line, "torch==") || strings.HasPrefix(line, "torch "):
			hasTorch = true
		case strings.HasPrefix(line, "transformers==") || strings.HasPrefix(line, "transformers "):
			hasTransformers = true
		}
	}
	return hasTorch && hasTransformers, nil
}

// hasLargeFile returns true iff `path` exists, is a regular file, and is at
// least `minSize` bytes. Used as a cheap "is the model present?" heuristic.
func hasLargeFile(path string, minSize int64) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if !info.Mode().IsRegular() {
		return false
	}
	return info.Size() >= minSize
}
