package profile

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// mlxProfileYAML builds a minimal valid mlx_server profile around the given
// model dir and venv, with `extra` spliced into the backend block.
func mlxProfileYAML(modelDir, venv, extra string) string {
	return `
name: mlx
backend:
  mlx_server:
    model_dir: ` + modelDir + `
    venv: ` + venv + `
    alias: qwen-mlx
    context: 65536
` + extra
}

func TestLoad_MLXServer_DefaultsAndExpansion(t *testing.T) {
	dir := t.TempDir()
	p, err := Load(writeProfile(t, mlxProfileYAML(dir, dir, "")))
	if err != nil {
		t.Fatal(err)
	}
	m := p.Backend.MLXServer
	if m == nil {
		t.Fatal("mlx_server backend is nil")
	}
	// Host defaults in normalize so the spec builder and the schema agree
	// on what an omitted bind address means.
	if m.Host != "127.0.0.1" {
		t.Errorf("host = %q, want the 127.0.0.1 default", m.Host)
	}
	if m.Alias != "qwen-mlx" || m.Context != 65536 {
		t.Errorf("alias/context = %q/%d", m.Alias, m.Context)
	}
}

func TestLoad_MLXServer_TildeExpanded(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	p, err := Load(writeProfile(t, mlxProfileYAML("~/models/mlx-snap", "~/.venvs/mlx", "    draft_model: ~/models/draft\n")))
	if err != nil {
		t.Fatal(err)
	}
	m := p.Backend.MLXServer
	for _, tc := range []struct{ field, got string }{
		{"model_dir", m.ModelDir},
		{"venv", m.Venv},
		{"draft_model", m.DraftModel},
	} {
		if !strings.HasPrefix(tc.got, home) {
			t.Errorf("%s = %q, want ~ expanded under %q", tc.field, tc.got, home)
		}
	}
}

// The alias is what reaches clients; a path there means someone worked
// around the /v1/models behaviour by hand and would break the router's
// useModelName rendering.
func TestLoad_MLXServer_RejectsPathAlias(t *testing.T) {
	dir := t.TempDir()
	yaml := `
name: mlx
backend:
  mlx_server:
    model_dir: ` + dir + `
    venv: ` + dir + `
    alias: ` + dir + `
    context: 4096
`
	_, err := Load(writeProfile(t, yaml))
	if err == nil {
		t.Fatal("expected a path-shaped alias to be rejected")
	}
	if !strings.Contains(err.Error(), "not a path") {
		t.Errorf("error should explain the alias/path split, got: %v", err)
	}
}

func TestLoad_MLXServer_RequiredFields(t *testing.T) {
	dir := t.TempDir()
	cases := []struct{ name, yaml, want string }{
		{
			name: "no model_dir or huggingface",
			yaml: "name: mlx\nbackend:\n  mlx_server:\n    venv: " + dir + "\n    alias: a\n    context: 4096\n",
			want: "model_dir or huggingface",
		},
		{
			name: "no venv",
			yaml: "name: mlx\nbackend:\n  mlx_server:\n    model_dir: " + dir + "\n    alias: a\n    context: 4096\n",
			want: "venv is required",
		},
		{
			name: "no context",
			yaml: "name: mlx\nbackend:\n  mlx_server:\n    model_dir: " + dir + "\n    venv: " + dir + "\n    alias: a\n",
			want: "context must be > 0",
		},
		{
			name: "draft tokens without draft model",
			yaml: mlxProfileYAML(dir, dir, "    num_draft_tokens: 3\n"),
			want: "requires draft_model",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeProfile(t, tc.yaml))
			if err == nil {
				t.Fatalf("expected an error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want mention of %q", err, tc.want)
			}
		})
	}
}

// An mlx_server profile may carry a frontend — that is the whole reason it
// is not modelled as an http_server backend.
func TestLoad_MLXServer_AllowsFrontend(t *testing.T) {
	dir := t.TempDir()
	bin := stubManagedBinary(t)
	yaml := mlxProfileYAML(dir, dir, "") + `
frontend:
  kind: managed
  binary: ` + bin + `
  write_files:
    - path: ` + filepath.Join(dir, "models.yml") + `
      template: {providers: {vibe-local: {baseUrl: "${VIBE_API}"}}}
`
	p, err := Load(writeProfile(t, yaml))
	if err != nil {
		t.Fatalf("mlx_server should accept a frontend block: %v", err)
	}
	if p.Frontend.Kind != FrontendManaged {
		t.Errorf("frontend.kind = %q", p.Frontend.Kind)
	}
}

func TestMLXServerSpec_Argv(t *testing.T) {
	dir := t.TempDir()
	p, err := Load(writeProfile(t, mlxProfileYAML(dir, dir,
		"    max_tokens: 32768\n    draft_model: "+dir+"\n    num_draft_tokens: 3\n    trust_remote_code: true\n    chat_template_args: {enable_thinking: false}\n    extra_args: [--prefill-step-size, \"512\"]\n")))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := MLXServerSpec(p, 8123)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "bin", "mlx_lm.server"); spec.Binary != want {
		t.Errorf("binary = %q, want %q", spec.Binary, want)
	}
	if spec.HealthURL != "http://127.0.0.1:8123/health" {
		t.Errorf("health = %q", spec.HealthURL)
	}
	// No --ctx-size and no --alias: mlx_lm.server has neither, and emitting
	// one would abort the launch.
	for _, banned := range []string{"--ctx-size", "--alias", "--n-gpu-layers"} {
		if slices.Contains(spec.Args, banned) {
			t.Errorf("argv must not contain %s: %v", banned, spec.Args)
		}
	}
	joined := strings.Join(spec.Args, " ")
	for _, want := range []string{
		"--model " + dir,
		"--host 127.0.0.1",
		"--port 8123",
		"--max-tokens 32768",
		"--draft-model " + dir,
		"--num-draft-tokens 3",
		`--chat-template-args {"enable_thinking":false}`,
		"--trust-remote-code",
		"--prefill-step-size 512",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv missing %q: %v", want, joined)
		}
	}
	// extra_args land last so a profile can always override an earlier flag.
	if spec.Args[len(spec.Args)-1] != "512" {
		t.Errorf("extra_args should be appended last, got %v", spec.Args)
	}
}

func TestMLXServerSpec_HostOverride(t *testing.T) {
	dir := t.TempDir()
	p, err := Load(writeProfile(t, mlxProfileYAML(dir, dir, "    host: 0.0.0.0\n")))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := MLXServerSpec(p, 9100)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(spec.Args, " "), "--host 0.0.0.0") {
		t.Errorf("host override not applied: %v", spec.Args)
	}
	// Health stays on loopback even when the server binds every interface.
	if spec.HealthURL != "http://127.0.0.1:9100/health" {
		t.Errorf("health = %q, want loopback", spec.HealthURL)
	}
}
