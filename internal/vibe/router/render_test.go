package router

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
)

const testBinary = "/opt/llama/bin/llama-server"

// parsedConfig is the decode target for rendered output; only fields the
// tests assert on.
type parsedConfig struct {
	HealthCheckTimeout   int  `yaml:"healthCheckTimeout"`
	SendLoadingState     bool `yaml:"sendLoadingState"`
	StartPort            int  `yaml:"startPort"`
	IncludeAliasesInList bool `yaml:"includeAliasesInList"`
	Models               map[string]struct {
		Cmd      string   `yaml:"cmd"`
		Aliases  []string `yaml:"aliases"`
		TTL      *int     `yaml:"ttl"`
		Unlisted bool     `yaml:"unlisted"`
	} `yaml:"models"`
	Peers map[string]struct {
		Proxy  string   `yaml:"proxy"`
		APIKey string   `yaml:"apiKey"`
		Models []string `yaml:"models"`
	} `yaml:"peers"`
	Hooks struct {
		OnStartup struct {
			Preload []string `yaml:"preload"`
		} `yaml:"on_startup"`
	} `yaml:"hooks"`
}

func renderTestdata(t *testing.T) (string, []*profile.BackendDef, parsedConfig) {
	t.Helper()
	defs, err := LoadDefs("testdata/backends")
	if err != nil {
		t.Fatalf("LoadDefs: %v", err)
	}
	out, err := Render(defs, Options{LlamaServerBinary: testBinary})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var cfg parsedConfig
	if err := yaml.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("rendered output is not YAML: %v\n%s", err, out)
	}
	return out, defs, cfg
}

// TestRender_GoldenLiveEquivalent renders fixtures equivalent to the live A1
// hand-written config's four models + Anthropic peer and asserts the
// semantically load-bearing content: selection, aliases, ttl seconds,
// flag-for-flag cmds, and the peer stanza.
func TestRender_GoldenLiveEquivalent(t *testing.T) {
	out, defs, cfg := renderTestdata(t)

	if !strings.HasPrefix(out, "# rendered by vibe router render — do not edit; source: ~/.config/vibe/backends/\n") {
		t.Errorf("missing header comment, got prefix %q", out[:min(len(out), 90)])
	}
	if cfg.HealthCheckTimeout != 600 {
		t.Errorf("healthCheckTimeout = %d, want 600 (qwen2.5-coder-7b start_timeout 10m)", cfg.HealthCheckTimeout)
	}
	if !cfg.SendLoadingState {
		t.Error("sendLoadingState must be true (clients stall on byte-silent loads)")
	}
	if cfg.StartPort != 5800 {
		t.Errorf("startPort = %d, want 5800", cfg.StartPort)
	}
	if !cfg.IncludeAliasesInList {
		t.Error("includeAliasesInList must be true (external readiness checks read the catalog)")
	}

	wantModels := []string{"gemma-4-31b-mm", "qwen2.5-coder-7b", "qwen3.6-27b", "qwen3.6-27b-tools"}
	if len(cfg.Models) != len(wantModels) {
		t.Errorf("models = %v, want exactly %v", keys(cfg.Models), wantModels)
	}
	for _, name := range wantModels {
		if _, ok := cfg.Models[name]; !ok {
			t.Errorf("model %q missing from rendered config", name)
		}
	}
	// Non-external and service-mode defs stay vibe-managed.
	for _, name := range []string{"gemma-4-31b", "searxng", "anthropic"} {
		if _, ok := cfg.Models[name]; ok {
			t.Errorf("def %q must not be rendered as a model", name)
		}
	}

	wantAliases := map[string][]string{
		"qwen3.6-27b":       {"qwen3.6-27b-mtp-q6_k"}, // alias_owner beats the -tools claim
		"qwen3.6-27b-tools": nil,                      // lost the contested alias
		"gemma-4-31b-mm":    {"gemma-4-31b-it"},
		"qwen2.5-coder-7b":  nil, // alias == def name; the models: key already serves it
	}
	wantTTL := map[string]int{
		"qwen3.6-27b":       7200, // default 2h: no lifecycle block
		"qwen3.6-27b-tools": 7200,
		"gemma-4-31b-mm":    3600,
		"qwen2.5-coder-7b":  1800,
	}
	for name, m := range cfg.Models {
		if got, want := m.Aliases, wantAliases[name]; !equalSlices(got, want) {
			t.Errorf("model %s aliases = %v, want %v", name, got, want)
		}
		if m.TTL == nil {
			t.Errorf("model %s: ttl must always be emitted", name)
		} else if *m.TTL != wantTTL[name] {
			t.Errorf("model %s ttl = %d, want %d", name, *m.TTL, wantTTL[name])
		}
		if m.Unlisted {
			t.Errorf("model %s unexpectedly unlisted", name)
		}
	}

	// cmd must be exactly what LlamaServerSpec renders, with the port
	// replaced by llama-swap's ${PORT} macro.
	for _, def := range defs {
		if !def.Backend.External || def.Backend.LlamaServer == nil {
			continue
		}
		spec, err := profile.LlamaServerSpec(&profile.Profile{Name: def.Name, Backend: def.Backend}, testBinary, 0)
		if err != nil {
			t.Fatalf("LlamaServerSpec(%s): %v", def.Name, err)
		}
		want := append([]string{spec.Binary}, spec.Args...)
		for i := 0; i+1 < len(want); i++ {
			if want[i] == "--port" {
				want[i+1] = "${PORT}"
			}
		}
		got := strings.Fields(cfg.Models[def.Name].Cmd)
		if !equalSlices(got, want) {
			t.Errorf("model %s cmd tokens:\n got %q\nwant %q", def.Name, got, want)
		}
	}

	// Anchor a few tokens against the A1 golden config directly, so a
	// regression in LlamaServerSpec itself can't self-certify.
	qwenCmd := cfg.Models["qwen3.6-27b"].Cmd
	for _, tok := range []string{
		"--host 127.0.0.1", "--port ${PORT}", "--alias qwen3.6-27b-mtp-q6_k",
		"--ctx-size 131072", "--cache-type-k q8_0", "--jinja",
		"--spec-type draft-mtp", "--spec-draft-n-max 5", "--spec-draft-p-min 0.75",
	} {
		if !strings.Contains(strings.ReplaceAll(qwenCmd, "\n", " "), tok) {
			t.Errorf("qwen3.6-27b cmd missing %q:\n%s", tok, qwenCmd)
		}
	}
	toolsCmd := strings.ReplaceAll(cfg.Models["qwen3.6-27b-tools"].Cmd, "\n", " ")
	if !strings.Contains(toolsCmd, "--reasoning off") {
		t.Errorf("qwen3.6-27b-tools cmd missing --reasoning off:\n%s", toolsCmd)
	}
	gemmaCmd := strings.ReplaceAll(cfg.Models["gemma-4-31b-mm"].Cmd, "\n", " ")
	for _, tok := range []string{"--mmproj", "--model-draft", "--spec-type draft-mtp", "--spec-draft-n-max 4"} {
		if !strings.Contains(gemmaCmd, tok) {
			t.Errorf("gemma-4-31b-mm cmd missing %q:\n%s", tok, gemmaCmd)
		}
	}

	peer, ok := cfg.Peers["anthropic"]
	if !ok {
		t.Fatalf("peers.anthropic missing; peers = %v", keys(cfg.Peers))
	}
	if peer.Proxy != "https://api.anthropic.com" {
		t.Errorf("peer proxy = %q", peer.Proxy)
	}
	if peer.APIKey != "${env.ANTHROPIC_API_KEY}" {
		t.Errorf("peer apiKey = %q, want ${env.ANTHROPIC_API_KEY} (no literal keys in rendered config)", peer.APIKey)
	}
	if !equalSlices(peer.Models, []string{"claude-opus-4-8", "claude-sonnet-5"}) {
		t.Errorf("peer models = %v", peer.Models)
	}
}

func TestRender_Deterministic(t *testing.T) {
	a, _, _ := renderTestdata(t)
	b, _, _ := renderTestdata(t)
	if a != b {
		t.Error("two renders of the same defs differ (diff-based drift detection needs byte-stable output)")
	}
}

func llamaDef(name, alias string) *profile.BackendDef {
	return &profile.BackendDef{
		Name: name,
		Backend: profile.Backend{
			External: true,
			LlamaServer: &profile.LlamaServerBackend{
				Path:     "/models/" + name + ".gguf",
				Alias:    alias,
				Context:  4096,
				Parallel: 1,
			},
		},
	}
}

func TestRender_AliasCollision(t *testing.T) {
	t.Run("no owner is an error naming both defs", func(t *testing.T) {
		_, err := Render([]*profile.BackendDef{llamaDef("base", "shared"), llamaDef("variant", "shared")}, Options{})
		if err == nil {
			t.Fatal("want collision error")
		}
		for _, want := range []string{`alias "shared"`, "base", "variant", "alias_owner"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q missing %q", err, want)
			}
		}
	})

	t.Run("exactly one owner keeps the alias", func(t *testing.T) {
		base := llamaDef("base", "shared")
		base.Router = &profile.RouterOpts{AliasOwner: true}
		out, err := Render([]*profile.BackendDef{base, llamaDef("variant", "shared")}, Options{})
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		var cfg parsedConfig
		if err := yaml.Unmarshal([]byte(out), &cfg); err != nil {
			t.Fatal(err)
		}
		if got := cfg.Models["base"].Aliases; !equalSlices(got, []string{"shared"}) {
			t.Errorf("owner aliases = %v, want [shared]", got)
		}
		if got := cfg.Models["variant"].Aliases; len(got) != 0 {
			t.Errorf("non-owner aliases = %v, want none", got)
		}
	})

	t.Run("two owners is an error", func(t *testing.T) {
		a := llamaDef("base", "shared")
		a.Router = &profile.RouterOpts{AliasOwner: true}
		b := llamaDef("variant", "shared")
		b.Router = &profile.RouterOpts{AliasOwner: true}
		_, err := Render([]*profile.BackendDef{a, b}, Options{})
		if err == nil || !strings.Contains(err.Error(), "multiple owners") {
			t.Fatalf("want multiple-owners error, got %v", err)
		}
		for _, want := range []string{"base", "variant"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q missing def %q", err, want)
			}
		}
	})

	t.Run("alias colliding with a def name is an error", func(t *testing.T) {
		_, err := Render([]*profile.BackendDef{llamaDef("base", "other"), llamaDef("other", "other-alias")}, Options{})
		if err == nil {
			t.Fatal("want def-name collision error")
		}
		for _, want := range []string{"base", `"other"`, "canonical"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q missing %q", err, want)
			}
		}
	})
}

func TestRender_ExplicitRouterAliasesUnlistedPreload(t *testing.T) {
	ttl := profile.Duration(0)
	def := llamaDef("pinned", "pinned-alias")
	def.Router = &profile.RouterOpts{Aliases: []string{"fast", "cheap"}, Unlisted: true}
	def.Lifecycle = &profile.Lifecycle{TTL: &ttl, Preload: true}
	out, err := Render([]*profile.BackendDef{def}, Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var cfg parsedConfig
	if err := yaml.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatal(err)
	}
	m := cfg.Models["pinned"]
	// Explicit router.aliases replace the llama_server alias default.
	if !equalSlices(m.Aliases, []string{"fast", "cheap"}) {
		t.Errorf("aliases = %v, want [fast cheap]", m.Aliases)
	}
	if m.TTL == nil || *m.TTL != 0 {
		t.Errorf("ttl = %v, want explicit 0 (never unload)", m.TTL)
	}
	if !m.Unlisted {
		t.Error("unlisted not rendered")
	}
	if !equalSlices(cfg.Hooks.OnStartup.Preload, []string{"pinned"}) {
		t.Errorf("preload = %v, want [pinned]", cfg.Hooks.OnStartup.Preload)
	}
}

func TestRender_ExternalTabbyRejected(t *testing.T) {
	def := &profile.BackendDef{
		Name: "pi-tabby",
		Backend: profile.Backend{
			External: true,
			TabbyAPI: &profile.TabbyAPIBackend{ModelDir: "/m/x", Alias: "x", Context: 4096, Port: 5000, Venv: "/v", Repo: "/r"},
		},
	}
	_, err := Render([]*profile.BackendDef{def}, Options{})
	if err == nil || !strings.Contains(err.Error(), "tabby_api") {
		t.Fatalf("want external-tabby rejection, got %v", err)
	}
}

func TestUnifiedDiff(t *testing.T) {
	if d := UnifiedDiff("a", "b", "same\n", "same\n"); d != "" {
		t.Errorf("equal texts must yield empty diff, got %q", d)
	}
	d := UnifiedDiff("old", "new", "one\ntwo\nthree\n", "one\ntwo changed\nthree\n")
	for _, want := range []string{"--- old", "+++ new", "-two", "+two changed", "@@ -1,3 +1,3 @@"} {
		if !strings.Contains(d, want) {
			t.Errorf("diff missing %q:\n%s", want, d)
		}
	}
	// Far-apart changes split into separate hunks.
	long := strings.Repeat("ctx\n", 20)
	d = UnifiedDiff("old", "new", "start\n"+long+"end\n", "START\n"+long+"END\n")
	if got := strings.Count(d, "@@"); got != 4 { // two hunks, "@@" twice per header
		t.Errorf("want 2 hunks for far-apart changes, got %d markers:\n%s", got, d)
	}
}

func TestRenderToFile_WriteAndDrift(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := tmp + "/llama-swap/config.yaml"

	out, err := RenderToFile("testdata/backends", cfgPath, Options{LlamaServerBinary: testBinary}, true)
	if err != nil {
		t.Fatalf("first RenderToFile: %v", err)
	}
	if !out.Changed {
		t.Error("first render against a missing file must report Changed")
	}

	// Re-render: no drift.
	out, err = RenderToFile("testdata/backends", cfgPath, Options{LlamaServerBinary: testBinary}, false)
	if err != nil {
		t.Fatalf("second RenderToFile: %v", err)
	}
	if out.Changed {
		t.Errorf("no drift expected after a write; diff:\n%s", out.Diff)
	}

	// Change the render input (different binary) -> drift detected, and
	// write=false must leave the file untouched.
	out, err = RenderToFile("testdata/backends", cfgPath, Options{LlamaServerBinary: "/elsewhere/llama-server"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Changed {
		t.Fatal("expected drift after input change")
	}
	if !strings.Contains(out.Diff, "/elsewhere/llama-server") || !strings.Contains(out.Diff, "/opt/llama/bin/llama-server") {
		t.Errorf("diff should show both sides of the cmd change:\n%s", out.Diff)
	}
	after, err := RenderToFile("testdata/backends", cfgPath, Options{LlamaServerBinary: testBinary}, false)
	if err != nil {
		t.Fatal(err)
	}
	if after.Changed {
		t.Error("check mode (write=false) must not modify the file")
	}
}

func TestDefaultTTLIsTwoHours(t *testing.T) {
	if DefaultTTL != 2*time.Hour {
		t.Errorf("DefaultTTL = %v", DefaultTTL)
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRender_ExtrasMerge(t *testing.T) {
	dir := t.TempDir()
	extras := filepath.Join(dir, "extras.yaml")
	if err := os.WriteFile(extras, []byte(`models:
  slowmodel:
    cmd: sh -c "exec /tmp/slowmodel --port 18099"
    proxy: http://127.0.0.1:18099
    checkEndpoint: /health
    ttl: 300
peers:
  sim-cell:
    proxy: http://127.0.0.1:9101
    models: [fastfake]
routing:
  router:
    use: group
`), 0o644); err != nil {
		t.Fatal(err)
	}

	defs, err := LoadDefs("testdata/backends")
	if err != nil {
		t.Fatal(err)
	}
	out, err := Render(defs, Options{LlamaServerBinary: "/usr/bin/llama-server", ExtrasPath: extras})
	if err != nil {
		t.Fatalf("render with extras: %v", err)
	}
	var cfg map[string]any
	if err := yaml.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("reparse: %v", err)
	}
	models := cfg["models"].(map[string]any)
	if _, ok := models["slowmodel"]; !ok {
		t.Error("extras model slowmodel missing from merge")
	}
	if _, ok := models["qwen3.6-27b"]; !ok {
		t.Error("rendered model lost in merge")
	}
	peers := cfg["peers"].(map[string]any)
	if _, ok := peers["sim-cell"]; !ok {
		t.Error("extras peer sim-cell missing")
	}
	if _, ok := peers["anthropic"]; !ok {
		t.Error("rendered peer anthropic lost in merge")
	}
	if _, ok := cfg["routing"]; !ok {
		t.Error("extras routing section missing")
	}

	// A collision with a rendered def must fail loudly, naming the key.
	if err := os.WriteFile(extras, []byte("models:\n  qwen3.6-27b:\n    cmd: echo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Render(defs, Options{LlamaServerBinary: "/usr/bin/llama-server", ExtrasPath: extras}); err == nil || !strings.Contains(err.Error(), "qwen3.6-27b") {
		t.Errorf("collision error = %v, want mention of qwen3.6-27b", err)
	}

	// Missing extras file is fine.
	if _, err := Render(defs, Options{LlamaServerBinary: "/usr/bin/llama-server", ExtrasPath: filepath.Join(dir, "absent.yaml")}); err != nil {
		t.Errorf("missing extras file should be a no-op, got %v", err)
	}
}

func TestRender_ComfyUITenant(t *testing.T) {
	dir := t.TempDir()
	comfy := t.TempDir()
	if err := os.WriteFile(filepath.Join(comfy, "main.py"), []byte("# stub\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	def := `name: comfyui
backend:
  external: true
  comfyui:
    dir: ` + comfy + `
    python: /venv/bin/python
    port: 8188
lifecycle:
  ttl: 30m
`
	if err := os.WriteFile(filepath.Join(dir, "comfyui.yaml"), []byte(def), 0o644); err != nil {
		t.Fatal(err)
	}
	defs, err := LoadDefs(dir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Render(defs, Options{LlamaServerBinary: "/usr/bin/llama-server"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var cfg struct {
		Models map[string]*swapModel `yaml:"models"`
	}
	if err := yaml.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatal(err)
	}
	m := cfg.Models["comfyui"]
	if m == nil {
		t.Fatal("comfyui model entry missing")
	}
	wantCmd := `sh -c "cd ` + comfy + ` && exec /venv/bin/python main.py --listen 127.0.0.1 --port 8188"`
	if m.Cmd != wantCmd {
		t.Errorf("cmd = %q, want %q", m.Cmd, wantCmd)
	}
	if m.Proxy != "http://127.0.0.1:8188" || m.CheckEndpoint != "/system_stats" || m.TTL != 1800 {
		t.Errorf("proxy/check/ttl = %q %q %d, want fixed-port proxy, /system_stats, 1800", m.Proxy, m.CheckEndpoint, m.TTL)
	}

	// A random-port comfyui def can't be proxied — must fail loudly.
	if err := os.WriteFile(filepath.Join(dir, "comfyui.yaml"), []byte(`name: comfyui
backend:
  external: true
  comfyui: {dir: `+comfy+`, port: 0}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	defs, err = LoadDefs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Render(defs, Options{}); err == nil || !strings.Contains(err.Error(), "port") {
		t.Errorf("random-port comfyui render = %v, want port error", err)
	}
}

// TestRender_Capabilities covers the block llama-swap turns into
// architecture.input_modalities and context_length on /v1/models. A client
// that has to guess these instead gets them from the model id, which is the
// real upstream model rather than the one this def serves — a vision-capable
// checkpoint run without an mmproj then receives images and answers 500.
func TestRender_Capabilities(t *testing.T) {
	_, _, _ = renderTestdata(t)
	defs, err := LoadDefs("testdata/backends")
	if err != nil {
		t.Fatalf("LoadDefs: %v", err)
	}
	out, err := Render(defs, Options{LlamaServerBinary: testBinary})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var cfg struct {
		Models map[string]*swapModel `yaml:"models"`
	}
	if err := yaml.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("rendered output is not YAML: %v", err)
	}

	for _, tc := range []struct {
		model   string
		wantIn  []string
		wantCtx int
	}{
		{"gemma-4-31b-mm", []string{"text", "image"}, 57344},
		{"qwen3.6-27b", []string{"text"}, 131072},
	} {
		m := cfg.Models[tc.model]
		if m == nil {
			t.Fatalf("%s: model entry missing", tc.model)
		}
		if m.Capabilities == nil {
			t.Fatalf("%s: no capabilities block", tc.model)
		}
		if !equalSlices(m.Capabilities.In, tc.wantIn) {
			t.Errorf("%s: in = %v, want %v", tc.model, m.Capabilities.In, tc.wantIn)
		}
		if !equalSlices(m.Capabilities.Out, []string{"text"}) {
			t.Errorf("%s: out = %v, want [text]", tc.model, m.Capabilities.Out)
		}
		if m.Capabilities.Context != tc.wantCtx {
			t.Errorf("%s: context = %d, want %d (the def's own ctx-size)", tc.model, m.Capabilities.Context, tc.wantCtx)
		}
	}

	t.Run("embedding servers are excluded, not described", func(t *testing.T) {
		embed := llamaDef("bge-embed", "bge-embed")
		embed.Backend.LlamaServer.Parallel = 16
		embed.Backend.LlamaServer.ExtraArgs = []string{"--embeddings", "--pooling", "mean"}
		out, err := Render([]*profile.BackendDef{embed}, Options{LlamaServerBinary: testBinary})
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		var cfg struct {
			Models map[string]*swapModel `yaml:"models"`
		}
		if err := yaml.Unmarshal([]byte(out), &cfg); err != nil {
			t.Fatal(err)
		}
		// context is the total budget across --parallel slots, so publishing
		// it as a context window overstates the per-request limit 16-fold.
		if c := cfg.Models["bge-embed"].Capabilities; c != nil {
			t.Errorf("capabilities = %+v, want none for an embedding server", c)
		}
	})
}
