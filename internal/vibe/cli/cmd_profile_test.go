package cli

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
)

// stubProfileEnv points $XDG_CONFIG_HOME at a fresh tmp dir so each test
// owns its own profile directory. Returns the resolved profiles dir.
func stubProfileEnv(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(tmp, "runtime"))
	return filepath.Join(tmp, "config", "vibe", "profiles")
}

// writeModelStub creates a placeholder GGUF file and returns its path. Used
// to satisfy profile.validateLlamaServer's path-existence check after we
// fill in the REPLACE markers.
func writeModelStub(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(path, []byte("stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeComfyDirStub creates a directory containing main.py so
// validateComfyUI is satisfied.
func writeComfyDirStub(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("# fake\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeTabbyModelStub mints an empty directory; validateTabbyAPI only
// requires ModelDir be non-empty (file-existence checks happen at
// launch time, not load time, because EXL3 snapshots are populated
// lazily by `vibe pull`).
func writeTabbyModelStub(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// writeTabbyRepoStub mints a directory; validateTabbyAPI only requires
// Repo be non-empty. Existence-of-start.py is the launch concern, not
// the load concern.
func writeTabbyRepoStub(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// writeTabbyVenvStub mints a venv-shaped directory.
func writeTabbyVenvStub(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestProfileInit_Kinds drives `runProfileInit` for every embedded kind and
// asserts: (a) the file is dropped at <profiles-dir>/<name>.yaml, (b) the
// rendered content has the templated name substituted, (c) every kind
// carries at least one REPLACE marker so users know where to edit, and (d)
// the file loads via profile.Load after a small fixture fills in the
// REPLACE placeholders (skipped for `managed`, which the loader rejects
// pending Phase-N support).
func TestProfileInit_Kinds(t *testing.T) {
	// We discover kinds from the embed.FS directly so adding a new
	// template file automatically extends test coverage.
	kinds, err := validProfileKinds()
	if err != nil {
		t.Fatalf("validProfileKinds: %v", err)
	}
	if len(kinds) == 0 {
		t.Fatal("no embedded profile templates discovered")
	}

	for _, kind := range kinds {
		kind := kind
		t.Run(kind, func(t *testing.T) {
			profilesDir := stubProfileEnv(t)
			name := "test-" + strings.ReplaceAll(kind, "_", "-")

			var buf bytes.Buffer
			if err := runProfileInit(&buf, kind, name, "", false); err != nil {
				t.Fatalf("runProfileInit(%q): %v", kind, err)
			}

			dest := filepath.Join(profilesDir, name+".yaml")
			body, err := os.ReadFile(dest)
			if err != nil {
				t.Fatalf("read dropped file: %v", err)
			}

			// Templated name was substituted everywhere.
			if strings.Contains(string(body), profileNamePlaceholder) {
				t.Errorf("placeholder %q still present in rendered template",
					profileNamePlaceholder)
			}
			if !strings.Contains(string(body), "name: "+name) {
				t.Errorf("rendered template missing `name: %s`", name)
			}

			// At least one REPLACE marker (the whole point of the template).
			if !strings.Contains(string(body), "REPLACE") {
				t.Errorf("rendered template has no REPLACE marker; user has nothing to edit")
			}

			// Next-steps line printed.
			out := buf.String()
			if !strings.Contains(out, "Wrote "+dest) {
				t.Errorf("stdout missing `Wrote %s`; got %q", dest, out)
			}
			if !strings.Contains(out, "vibe start "+name) {
				t.Errorf("stdout missing next-step hint; got %q", out)
			}

			// Confirm the file loads after the user fills in REPLACE markers.
			// `managed` is intentionally skipped: profile.Validate rejects
			// frontend.kind=managed in Phase 1, so the template ships ahead
			// of the loader.
			if kind == "managed" {
				return
			}
			filled := fillReplacements(t, string(body), kind)
			fixture := filepath.Join(t.TempDir(), name+".yaml")
			if err := os.WriteFile(fixture, []byte(filled), 0o644); err != nil {
				t.Fatal(err)
			}
			p, err := profile.Load(fixture)
			if err != nil {
				t.Fatalf("profile.Load(%s) after filling REPLACE markers: %v\n--- yaml ---\n%s",
					fixture, err, filled)
			}
			if p.Name != name {
				t.Errorf("loaded profile name = %q, want %q", p.Name, name)
			}
		})
	}
}

// fillReplacements rewrites every REPLACE-marked field in the template so
// the result satisfies profile.Validate. We do string substitution rather
// than YAML editing because the templates are deliberately hand-edited
// during onboarding — testing against the literal markers is the closest
// approximation to what a user types.
func fillReplacements(t *testing.T, body, kind string) string {
	t.Helper()
	switch kind {
	case "llama-server":
		model := writeModelStub(t)
		body = strings.ReplaceAll(body, "~/models/REPLACE-model.gguf", model)
		body = strings.ReplaceAll(body, "REPLACE-model-alias", "test-alias")
	case "comfyui":
		dir := writeComfyDirStub(t)
		body = strings.ReplaceAll(body, "~/ComfyUI", dir)
	case "docker-compose":
		model := writeModelStub(t)
		body = strings.ReplaceAll(body, "~/models/REPLACE-model.gguf", model)
		body = strings.ReplaceAll(body, "REPLACE-model-alias", "test-alias")
		// wait_for.url's REPLACE-port: just give it a port number.
		body = strings.ReplaceAll(body, "REPLACE-port", "3000")
	case "http-service":
		// http-service template's REPLACE markers; the validator just
		// needs an image + a port, both already non-REPLACE in the
		// template, so substitution is largely a no-op.
		body = strings.ReplaceAll(body, "REPLACE-image", "ghcr.io/example/image:latest")
		body = strings.ReplaceAll(body, "REPLACE-port", "14002")
	case "llama-embed-service":
		// llama-embed-service ships with concrete values; the only
		// REPLACE-marked items are advisory ("REPLACE if you...").
		// No-op fill is enough for the loader to accept it.
	case "tabby-api":
		// The tabby-api template requires the model_dir basename to
		// match the alias (validator enforces this). We name both
		// "test-model".
		modelParent := writeTabbyModelStub(t)
		modelDir := filepath.Join(modelParent, "test-model")
		if err := os.MkdirAll(modelDir, 0o755); err != nil {
			t.Fatal(err)
		}
		repoDir := writeTabbyRepoStub(t)
		venvDir := writeTabbyVenvStub(t)
		body = strings.ReplaceAll(body, "~/models/exl3/REPLACE-model-name", modelDir)
		body = strings.ReplaceAll(body, "REPLACE-org/REPLACE-repo", "example/exl3-model")
		body = strings.ReplaceAll(body, "REPLACE-model-name", "test-model")
		body = strings.ReplaceAll(body, "~/.local/state/vibe/tabby-venv", venvDir)
		body = strings.ReplaceAll(body, "~/src/tabbyAPI", repoDir)
	default:
		t.Fatalf("fillReplacements: unknown kind %q (extend this switch)", kind)
	}
	return body
}

// TestProfileInit_DefaultsNameToKind verifies that omitting --name uses
// the kind as the profile name.
func TestProfileInit_DefaultsNameToKind(t *testing.T) {
	profilesDir := stubProfileEnv(t)
	// Drive the cobra command itself so we exercise the flag-default path,
	// not just runProfileInit.
	cmd := profileCmd()
	cmd.SetArgs([]string{"init", "comfyui"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	dest := filepath.Join(profilesDir, "comfyui.yaml")
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("expected default-name file %s: %v", dest, err)
	}
}

// TestProfileInit_RefusesOverwrite asserts the second invocation against
// the same name fails unless --force is passed.
func TestProfileInit_RefusesOverwrite(t *testing.T) {
	profilesDir := stubProfileEnv(t)

	// First run succeeds.
	if err := runProfileInit(io.Discard, "comfyui", "fixture", "", false); err != nil {
		t.Fatalf("first init: %v", err)
	}

	// Second run without --force errors out.
	err := runProfileInit(io.Discard, "comfyui", "fixture", "", false)
	if err == nil {
		t.Fatal("expected error on overwrite, got nil")
	}
	if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Errorf("wrong error: %v", err)
	}

	// The original file must be untouched. Re-running with --force
	// rewrites it; we don't assert on byte equality here (the template is
	// deterministic, but we cover content elsewhere). We just check the
	// file is still readable and parses as YAML.
	dest := filepath.Join(profilesDir, "fixture.yaml")
	body, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read after failed overwrite: %v", err)
	}
	if !strings.Contains(string(body), "name: fixture") {
		t.Errorf("post-failed-overwrite content lost templated name; got first line %q",
			strings.SplitN(string(body), "\n", 2)[0])
	}

	// --force succeeds and writes again.
	if err := runProfileInit(io.Discard, "comfyui", "fixture", "", true); err != nil {
		t.Fatalf("--force overwrite: %v", err)
	}
}

// TestProfileInit_UnknownKind asserts the CLI rejects unknown kinds with a
// helpful error listing the supported ones.
func TestProfileInit_UnknownKind(t *testing.T) {
	stubProfileEnv(t)
	err := runProfileInit(io.Discard, "nonesuch", "x", "", false)
	if err == nil {
		t.Fatal("expected error for unknown kind")
	}
	msg := err.Error()
	if !strings.Contains(msg, "unknown kind") {
		t.Errorf("error %q does not mention 'unknown kind'", msg)
	}
	// At least one known kind should appear in the error so the user
	// knows what to type instead.
	if !strings.Contains(msg, "llama-server") {
		t.Errorf("error %q does not list valid kinds", msg)
	}
}

// TestProfileInit_InvalidName rejects names that don't match the profile
// loader's regex. We catch this in the CLI so users see the error before
// any file is written.
func TestProfileInit_InvalidName(t *testing.T) {
	stubProfileEnv(t)
	for _, bad := range []string{"", "has spaces", "with/slash", "with.dot"} {
		err := runProfileInit(io.Discard, "comfyui", bad, "", false)
		if err == nil {
			t.Errorf("name %q accepted; expected rejection", bad)
		}
	}
}

// TestProfileInit_HFInjection asserts the --hf flag produces a
// huggingface: block under backend.llama_server and that the result still
// validates after the remaining REPLACE markers are filled.
func TestProfileInit_HFInjection(t *testing.T) {
	profilesDir := stubProfileEnv(t)

	if err := runProfileInit(io.Discard, "llama-server", "code",
		"Qwen/Qwen3-Coder-30B-A3B-Instruct-GGUF", false); err != nil {
		t.Fatalf("init with --hf: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(profilesDir, "code.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, "huggingface:") {
		t.Errorf("rendered yaml missing huggingface block:\n%s", got)
	}
	if !strings.Contains(got, "repo: Qwen/Qwen3-Coder-30B-A3B-Instruct-GGUF") {
		t.Errorf("rendered yaml missing repo line:\n%s", got)
	}

	// With repo:file form, no REPLACE remains in the huggingface block.
	if err := runProfileInit(io.Discard, "llama-server", "codef",
		"Qwen/Qwen2.5-Coder-7B-Instruct-GGUF:qwen2.5-coder-7b-instruct-q4_k_m.gguf",
		false); err != nil {
		t.Fatalf("init with --hf repo:file: %v", err)
	}
	body, err = os.ReadFile(filepath.Join(profilesDir, "codef.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	got = string(body)
	if !strings.Contains(got, "file: qwen2.5-coder-7b-instruct-q4_k_m.gguf") {
		t.Errorf("rendered yaml missing file line:\n%s", got)
	}

	// When huggingface.file is concrete, profile.Load accepts a missing
	// model path (pull is responsible for fetching it). Fill the
	// remaining alias REPLACE and confirm the loader is happy.
	filled := strings.ReplaceAll(got, "REPLACE-model-alias", "test-alias")
	// Leave path as ~/models/REPLACE-model.gguf — validate skips Stat
	// when huggingface is set.
	fixture := filepath.Join(t.TempDir(), "codef.yaml")
	if err := os.WriteFile(fixture, []byte(filled), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := profile.Load(fixture)
	if err != nil {
		t.Fatalf("profile.Load with huggingface: %v\n--- yaml ---\n%s", err, filled)
	}
	if p.Backend.LlamaServer == nil || p.Backend.LlamaServer.Huggingface == nil {
		t.Fatal("loaded profile missing huggingface block")
	}
	if p.Backend.LlamaServer.Huggingface.Repo != "Qwen/Qwen2.5-Coder-7B-Instruct-GGUF" {
		t.Errorf("huggingface.repo = %q", p.Backend.LlamaServer.Huggingface.Repo)
	}
}

// TestProfileInit_HFRejectedForNonLlamaServer guards against using --hf on
// a kind that has no huggingface block to fill.
func TestProfileInit_HFRejectedForNonLlamaServer(t *testing.T) {
	stubProfileEnv(t)
	err := runProfileInit(io.Discard, "comfyui", "x",
		"Qwen/Qwen3-Coder-30B-A3B-Instruct-GGUF", false)
	if err == nil {
		t.Fatal("expected error for --hf on comfyui")
	}
	if !strings.Contains(err.Error(), "--hf is only valid") {
		t.Errorf("wrong error: %v", err)
	}
}

// TestProfileInit_EmbeddedTemplatesNonEmpty sanity-checks the //go:embed.
func TestProfileInit_EmbeddedTemplatesNonEmpty(t *testing.T) {
	kinds, err := validProfileKinds()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range kinds {
		body, err := loadProfileTemplate(k)
		if err != nil {
			t.Errorf("loadProfileTemplate(%q): %v", k, err)
			continue
		}
		if len(body) == 0 {
			t.Errorf("template %q is empty", k)
		}
		if !strings.Contains(body, profileNamePlaceholder) {
			t.Errorf("template %q missing %s placeholder", k, profileNamePlaceholder)
		}
		if !strings.Contains(body, "REPLACE") {
			t.Errorf("template %q missing any REPLACE marker", k)
		}
	}
}

// TestProfileInit_TemplateFilesReadable confirms the embed includes every
// file under profile_templates/. A failure here usually means a new file
// was added without an updated //go:embed pattern.
func TestProfileInit_TemplateFilesReadable(t *testing.T) {
	got, err := fs.ReadDir(profileTemplates, "profile_templates")
	if err != nil {
		t.Fatalf("read embed FS: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("embed FS resolved profile_templates/ but it has no entries")
	}
}

// ─── `vibe profile new` ─────────────────────────────────────────────────────

// TestProfileNew_DefaultsLlamaServerExternal asserts that `vibe profile new
// <name>` with no flags drops a kind=llama-server + frontend=external
// template at <profiles-dir>/<name>.yaml. The defaults match what a
// first-time user is most likely to want (opencode-style external frontend
// against a llama-server backend).
func TestProfileNew_DefaultsLlamaServerExternal(t *testing.T) {
	profilesDir := stubProfileEnv(t)
	cmd := profileCmd()
	cmd.SetArgs([]string{"new", "myprof"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	dest := filepath.Join(profilesDir, "myprof.yaml")
	body, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dropped file: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, "name: myprof") {
		t.Errorf("rendered template missing `name: myprof`:\n%s", got)
	}
	if !strings.Contains(got, "llama_server:") {
		t.Errorf("rendered template missing llama_server backend (default --kind)")
	}
	if !strings.Contains(got, "kind: external") {
		t.Errorf("rendered template missing kind: external (default --frontend)")
	}
}

// TestProfileNew_KindComfyUI verifies the --kind=comfyui flag selects the
// ComfyUI starter template, irrespective of --frontend (since ComfyUI
// ships its own UI).
func TestProfileNew_KindComfyUI(t *testing.T) {
	profilesDir := stubProfileEnv(t)
	cmd := profileCmd()
	cmd.SetArgs([]string{"new", "imgs", "--kind", "comfyui"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(profilesDir, "imgs.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, "comfyui:") {
		t.Errorf("rendered template missing comfyui backend:\n%s", got)
	}
	// Walk the rendered file line-by-line and assert no line at column 0
	// declares `frontend:` — the only occurrences should be inside `#`
	// comment lines (the template's "no frontend block needed" prose).
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(strings.TrimRight(line, " \t"), "frontend:") {
			t.Errorf("comfyui template should not declare a top-level frontend block; offending line: %q", line)
		}
	}
}

// TestProfileNew_FrontendDockerCompose asserts --frontend=docker-compose
// selects the docker-compose template under kind=llama-server.
func TestProfileNew_FrontendDockerCompose(t *testing.T) {
	profilesDir := stubProfileEnv(t)
	cmd := profileCmd()
	cmd.SetArgs([]string{"new", "stack", "--frontend", "docker-compose"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(profilesDir, "stack.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "kind: docker-compose") {
		t.Errorf("rendered template missing docker-compose frontend kind")
	}
}

// TestProfileNew_RefusesOverwrite asserts the create-then-create flow
// without --force fails with a helpful message, and that --force fixes it.
func TestProfileNew_RefusesOverwrite(t *testing.T) {
	profilesDir := stubProfileEnv(t)

	first := profileCmd()
	first.SetArgs([]string{"new", "dup"})
	first.SetOut(io.Discard)
	first.SetErr(io.Discard)
	if err := first.Execute(); err != nil {
		t.Fatalf("first new: %v", err)
	}

	second := profileCmd()
	second.SetArgs([]string{"new", "dup"})
	second.SetOut(io.Discard)
	var errOut bytes.Buffer
	second.SetErr(&errOut)
	err := second.Execute()
	if err == nil {
		t.Fatal("expected error on overwrite without --force")
	}
	if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Errorf("wrong error: %v", err)
	}

	third := profileCmd()
	third.SetArgs([]string{"new", "dup", "--force"})
	third.SetOut(io.Discard)
	third.SetErr(io.Discard)
	if err := third.Execute(); err != nil {
		t.Fatalf("--force overwrite: %v", err)
	}
	// Sanity: file still exists.
	if _, err := os.Stat(filepath.Join(profilesDir, "dup.yaml")); err != nil {
		t.Fatalf("post-force file missing: %v", err)
	}
}

// TestProfileNew_UnknownKindRejected guards the enum validation on
// --kind, since cobra's default behavior is to accept any string for
// StringVar flags.
func TestProfileNew_UnknownKindRejected(t *testing.T) {
	stubProfileEnv(t)
	cmd := profileCmd()
	cmd.SetArgs([]string{"new", "x", "--kind", "stable-diffusion"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error on unknown --kind")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("wrong error: %v", err)
	}
}

// TestProfileNew_UnknownFrontendRejected guards the enum validation on
// --frontend for kind=llama-server.
func TestProfileNew_UnknownFrontendRejected(t *testing.T) {
	stubProfileEnv(t)
	cmd := profileCmd()
	cmd.SetArgs([]string{"new", "x", "--frontend", "headless"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error on unknown --frontend")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("wrong error: %v", err)
	}
}

// TestProfileNew_TabCompletionEnums asserts cobra's flag completion
// surfaces the enum values for --kind and --frontend, so users can
// discover the valid set via `<TAB>` without consulting --help.
func TestProfileNew_TabCompletionEnums(t *testing.T) {
	cmd := profileCmd()
	// Find the "new" subcommand on the parent. cobra wires completion
	// per-flag, so we exercise the registered functions directly.
	var newSub *cobra.Command
	for _, c := range cmd.Commands() {
		if c.Name() == "new" {
			newSub = c
			break
		}
	}
	if newSub == nil {
		t.Fatal("profileCmd missing `new` subcommand")
	}
	// Drive the registered completion functions for both flags. cobra
	// stores them keyed by flag name; pull them out via the public API.
	for _, flag := range []string{"kind", "frontend"} {
		fn, ok := newSub.GetFlagCompletionFunc(flag)
		if !ok || fn == nil {
			t.Errorf("flag %q missing a completion function", flag)
			continue
		}
		vals, _ := fn(newSub, nil, "")
		if len(vals) == 0 {
			t.Errorf("flag %q completion returned no values", flag)
		}
	}
}

// TestProfileCmd_NoArgs runs `vibe profile` with no subcommand. cobra's
// default help should kick in (no error).
func TestProfileCmd_NoArgs(t *testing.T) {
	cmd := profileCmd()
	cmd.SetArgs(nil)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		// cobra returns nil when Help() succeeds; if Help itself fails
		// we want to know.
		if !errors.Is(err, io.EOF) {
			t.Fatalf("execute: %v", err)
		}
	}
}
