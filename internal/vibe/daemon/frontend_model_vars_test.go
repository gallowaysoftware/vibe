package daemon

// frontendModelVars is a dispatch over backend kinds, and "a dispatch that
// forgot one kind" is the defect class this repo keeps re-finding. A kind
// left out here does not fail loudly: it renders an empty model id (or a
// context window of 0) into a harness config, which surfaces as the harness
// silently using its own paid default. So the table below walks EVERY member
// of the discriminated union, and the last test fails when a seventh appears.

import (
	"reflect"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
)

func TestFrontendModelVars(t *testing.T) {
	tests := []struct {
		name      string
		backend   profile.Backend
		ref       string
		profName  string
		external  bool
		wantAlias string
		wantCtx   int
	}{{
		name:      "llama_server uses its alias",
		backend:   profile.Backend{LlamaServer: &profile.LlamaServerBackend{Alias: "qwen3.6-27b", Context: 131072}},
		profName:  "code",
		wantAlias: "qwen3.6-27b",
		wantCtx:   131072,
	}, {
		// external llama_server routes by DEF NAME, not alias: the router
		// keys models that way and an alias may be shared across variants.
		name:      "external llama_server uses the backend def name",
		backend:   profile.Backend{External: true, LlamaServer: &profile.LlamaServerBackend{Alias: "qwen3.6-27b", Context: 131072}},
		ref:       "qwen3.6-27b-tools",
		profName:  "code",
		external:  true,
		wantAlias: "qwen3.6-27b-tools",
		wantCtx:   131072,
	}, {
		name:      "tabby_api uses its alias",
		backend:   profile.Backend{TabbyAPI: &profile.TabbyAPIBackend{Alias: "qwen3.6-27b-exl3", Context: 65536}},
		profName:  "pi",
		wantAlias: "qwen3.6-27b-exl3",
		wantCtx:   65536,
	}, {
		name:      "mlx_server uses the friendly alias, not the model dir",
		backend:   profile.Backend{MLXServer: &profile.MLXServerBackend{Alias: "qwen3.6-mlx", ModelDir: "/models/qwen", Context: 32768}},
		profName:  "mac",
		wantAlias: "qwen3.6-mlx",
		wantCtx:   32768,
	}, {
		// The case this whole change exists for.
		name: "single-model cloud_peer resolves an unambiguous alias",
		backend: profile.Backend{CloudPeer: &profile.CloudPeerBackend{
			BaseURL: "https://api.moonshot.ai", Models: []string{"kimi-k3"}, Context: 1048576,
		}},
		profName:  "omp",
		external:  true,
		wantAlias: "kimi-k3",
		wantCtx:   1048576,
	}, {
		// A peer's ids ARE the router's, so the def-name fallback must NOT
		// win here — returning "omp" would render a model id the router has
		// never heard of, which 404s at the harness's first request.
		name: "multi-model cloud_peer leaves the alias unset rather than guessing",
		backend: profile.Backend{CloudPeer: &profile.CloudPeerBackend{
			BaseURL: "https://api.anthropic.com",
			Models:  []string{"claude-opus-5", "claude-sonnet-5"},
			Context: 200000,
		}},
		profName:  "omp",
		ref:       "anthropic",
		external:  true,
		wantAlias: "",
		wantCtx:   200000,
	}, {
		// context: is optional on a peer. 0 must stay 0 so it drops out of
		// the expansion map instead of rendering a zero-size window.
		name: "cloud_peer without context leaves the window unset",
		backend: profile.Backend{CloudPeer: &profile.CloudPeerBackend{
			BaseURL: "https://api.moonshot.ai", Models: []string{"kimi-k3"},
		}},
		profName:  "omp",
		external:  true,
		wantAlias: "kimi-k3",
		wantCtx:   0,
	}, {
		// comfyui and http_server never render a frontend config
		// (validateFrontend rejects a frontend block on both), so there is
		// no template to expand a model id into. Both must stay unset rather
		// than picking up the def-name fallback.
		name:     "comfyui has no model vars",
		backend:  profile.Backend{ComfyUI: &profile.ComfyUIBackend{Dir: "/opt/comfy"}},
		profName: "comfyui",
	}, {
		name:     "http_server has no model vars",
		backend:  profile.Backend{HTTPServer: &profile.HTTPServerBackend{Image: "kokoro:latest"}},
		profName: "tts",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &profile.Profile{Name: tc.profName, BackendRef: tc.ref, Backend: tc.backend}
			alias, ctxLen := frontendModelVars(p, tc.external)
			if alias != tc.wantAlias {
				t.Errorf("alias = %q, want %q", alias, tc.wantAlias)
			}
			if ctxLen != tc.wantCtx {
				t.Errorf("context = %d, want %d", ctxLen, tc.wantCtx)
			}
		})
	}
}

// The union is a discriminated one, so its arms are enumerable. Pinning the
// count here is what turns "someone added a backend kind and forgot this
// dispatch" from a silent empty model id into a failing test naming the file
// to edit. Update the expected set only alongside frontendModelVars itself.
func TestFrontendModelVarsCoversEveryBackendKind(t *testing.T) {
	covered := map[string]bool{
		"LlamaServer": true, "TabbyAPI": true, "MLXServer": true,
		"CloudPeer": true, "ComfyUI": true, "HTTPServer": true,
	}
	bt := reflect.TypeOf(profile.Backend{})
	var missing []string
	for i := range bt.NumField() {
		f := bt.Field(i)
		// The union arms are the pointer-to-struct fields; External and the
		// rest are scalars describing the chosen arm, not arms themselves.
		if f.Type.Kind() == reflect.Pointer && !covered[f.Name] {
			missing = append(missing, f.Name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("backend kind(s) %v are not accounted for in frontendModelVars "+
			"(internal/vibe/daemon/daemon.go); an unhandled kind renders an empty "+
			"${MODEL_ALIAS} into a harness config", missing)
	}
}
