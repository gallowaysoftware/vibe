package profile

import (
	"reflect"
	"strings"
	"testing"
)

func TestLlamaServerSpec_Full(t *testing.T) {
	p := &Profile{
		Backend: Backend{LlamaServer: &LlamaServerBackend{
			Path:       "/m/qwen.gguf",
			Alias:      "qwen",
			Context:    8192,
			Parallel:   2,
			GPULayers:  99,
			FlashAttn:  true,
			CacheTypeK: "q8_0",
			CacheTypeV: "q8_0",
			Jinja:      true,
			ExtraArgs:  []string{"--no-mmap"},
		}},
	}
	spec, err := LlamaServerSpec(p, "llama-server", 8080)
	if err != nil {
		t.Fatalf("LlamaServerSpec: %v", err)
	}
	if spec.Binary != "llama-server" {
		t.Errorf("binary = %q", spec.Binary)
	}
	wantArgs := []string{
		"--model", "/m/qwen.gguf",
		"--host", "127.0.0.1",
		"--port", "8080",
		"--alias", "qwen",
		"--ctx-size", "8192",
		"--parallel", "2",
		"--n-gpu-layers", "99",
		"--flash-attn", "on",
		"--cache-type-k", "q8_0",
		"--cache-type-v", "q8_0",
		"--jinja",
		"--no-mmap",
	}
	if !reflect.DeepEqual(spec.Args, wantArgs) {
		t.Errorf("args:\n got  %v\n want %v", spec.Args, wantArgs)
	}
	if spec.HealthURL != "http://127.0.0.1:8080/health" {
		t.Errorf("health_url = %q", spec.HealthURL)
	}
}

func TestLlamaServerSpec_OmitsZeroValueFlags(t *testing.T) {
	p := &Profile{
		Backend: Backend{LlamaServer: &LlamaServerBackend{
			Path:     "/m/q.gguf",
			Alias:    "q",
			Context:  4096,
			Parallel: 1,
		}},
	}
	spec, err := LlamaServerSpec(p, "", 9999)
	if err != nil {
		t.Fatalf("LlamaServerSpec: %v", err)
	}
	if spec.Binary != "llama-server" {
		t.Errorf("binary = %q (empty should default to llama-server)", spec.Binary)
	}
	mustNotContain := []string{"--flash-attn", "--jinja", "--cache-type-k", "--cache-type-v", "--n-gpu-layers"}
	for _, bad := range mustNotContain {
		for _, a := range spec.Args {
			if a == bad {
				t.Errorf("unexpected flag %s in %v", bad, spec.Args)
			}
		}
	}
}

func TestLlamaServerSpec_WrongBackendErrors(t *testing.T) {
	p := &Profile{Backend: Backend{}}
	if _, err := LlamaServerSpec(p, "llama-server", 1); err == nil {
		t.Errorf("expected error when llama_server is nil")
	}
}

func TestComfyUISpec_Defaults(t *testing.T) {
	p := &Profile{
		Backend: Backend{ComfyUI: &ComfyUIBackend{
			Dir: "/opt/ComfyUI",
		}},
	}
	spec, err := ComfyUISpec(p, 8188)
	if err != nil {
		t.Fatalf("ComfyUISpec: %v", err)
	}
	if spec.Binary != "python3" {
		t.Errorf("binary = %q, want python3", spec.Binary)
	}
	if spec.Workdir != "/opt/ComfyUI" {
		t.Errorf("workdir = %q", spec.Workdir)
	}
	wantArgs := []string{"main.py", "--listen", "127.0.0.1", "--port", "8188"}
	if !reflect.DeepEqual(spec.Args, wantArgs) {
		t.Errorf("args = %v, want %v", spec.Args, wantArgs)
	}
	if spec.HealthURL != "http://127.0.0.1:8188/system_stats" {
		t.Errorf("health_url = %q", spec.HealthURL)
	}
}

func TestComfyUISpec_CustomPythonAndListenAndExtras(t *testing.T) {
	p := &Profile{
		Backend: Backend{ComfyUI: &ComfyUIBackend{
			Dir:       "/opt/ComfyUI",
			Python:    "/usr/bin/python3.11",
			Listen:    "0.0.0.0",
			ExtraArgs: []string{"--lowvram", "--disable-xformers"},
		}},
	}
	spec, err := ComfyUISpec(p, 9000)
	if err != nil {
		t.Fatalf("ComfyUISpec: %v", err)
	}
	if spec.Binary != "/usr/bin/python3.11" {
		t.Errorf("binary = %q", spec.Binary)
	}
	wantArgs := []string{"main.py", "--listen", "0.0.0.0", "--port", "9000", "--lowvram", "--disable-xformers"}
	if !reflect.DeepEqual(spec.Args, wantArgs) {
		t.Errorf("args = %v, want %v", spec.Args, wantArgs)
	}
	// Health URL is on 127.0.0.1 regardless of listen address — the
	// supervisor talks to the child via loopback.
	if !strings.HasPrefix(spec.HealthURL, "http://127.0.0.1:9000") {
		t.Errorf("health_url = %q, want loopback", spec.HealthURL)
	}
}

func TestComfyUISpec_WrongBackendErrors(t *testing.T) {
	p := &Profile{Backend: Backend{}}
	if _, err := ComfyUISpec(p, 1); err == nil {
		t.Errorf("expected error when comfyui is nil")
	}
}
