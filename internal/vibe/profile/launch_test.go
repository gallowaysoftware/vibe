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

func TestLlamaServerSpec_MMProj(t *testing.T) {
	p := &Profile{
		Backend: Backend{LlamaServer: &LlamaServerBackend{
			Path:     "/m/gemma.gguf",
			MMProj:   "/m/mmproj.gguf",
			Alias:    "gemma",
			Context:  8192,
			Parallel: 1,
		}},
	}
	spec, err := LlamaServerSpec(p, "llama-server", 8080)
	if err != nil {
		t.Fatalf("LlamaServerSpec: %v", err)
	}
	var sawMMProj bool
	for i, a := range spec.Args {
		if a == "--mmproj" {
			sawMMProj = true
			if i+1 >= len(spec.Args) || spec.Args[i+1] != "/m/mmproj.gguf" {
				t.Errorf("--mmproj value: got %v, want /m/mmproj.gguf", spec.Args[i+1:])
			}
		}
	}
	if !sawMMProj {
		t.Errorf("expected --mmproj flag, got args: %v", spec.Args)
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
	mustNotContain := []string{"--flash-attn", "--jinja", "--cache-type-k", "--cache-type-v", "--n-gpu-layers", "--mmproj"}
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

// TestHTTPServerSpec_DockerMode covers the docker run argv synthesis: image,
// host:container port publish (loopback-scoped), GPU passthrough, sorted
// env, deterministic volume order. A failing assertion here means a profile
// using engine: kokoro would not spawn the same docker invocation a user
// would write by hand.
func TestHTTPServerSpec_DockerMode(t *testing.T) {
	p := &Profile{
		Name: "tts_kokoro",
		Backend: Backend{HTTPServer: &HTTPServerBackend{
			Image:         "ghcr.io/remsky/kokoro-fastapi-gpu:latest",
			Port:          8880,
			ContainerPort: 8880,
			Volumes:       []string{"/host/cache:/data"},
			Env:           map[string]string{"HF_HOME": "/data/hf", "DEBUG": "1"},
			GPU:           true,
			HealthPath:    "/health",
		}},
	}
	spec, err := HTTPServerSpec(p, p.Name)
	if err != nil {
		t.Fatalf("HTTPServerSpec: %v", err)
	}
	if spec.Binary != "docker" {
		t.Errorf("Binary = %q, want docker", spec.Binary)
	}
	got := strings.Join(spec.Args, " ")
	for _, want := range []string{
		"run --rm",
		"--name vibe-tts_kokoro",
		"-p 127.0.0.1:8880:8880",
		"--gpus all",
		"-v /host/cache:/data",
		"-e DEBUG=1",
		"-e HF_HOME=/data/hf",
		"ghcr.io/remsky/kokoro-fastapi-gpu:latest",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("docker argv missing %q\nargv: %s", want, got)
		}
	}
	if spec.HealthURL != "http://127.0.0.1:8880/health" {
		t.Errorf("HealthURL = %q", spec.HealthURL)
	}
	// Env-flag order must be sorted (DEBUG before HF_HOME alphabetically).
	if strings.Index(got, "-e DEBUG=") > strings.Index(got, "-e HF_HOME=") {
		t.Errorf("env-flag order is not sorted: %s", got)
	}
}

// TestHTTPServerSpec_BinaryMode covers the bare-process path: Binary + Args
// + sorted env, no docker.
func TestHTTPServerSpec_BinaryMode(t *testing.T) {
	p := &Profile{
		Name: "embedding_server",
		Backend: Backend{HTTPServer: &HTTPServerBackend{
			Binary: "/usr/local/bin/text-embeddings-inference",
			Args:   []string{"--model-id", "BAAI/bge-m3", "--port", "8090"},
			Env:    map[string]string{"RUST_LOG": "info"},
			Port:   8090,
		}},
	}
	spec, err := HTTPServerSpec(p, p.Name)
	if err != nil {
		t.Fatalf("HTTPServerSpec: %v", err)
	}
	if spec.Binary != "/usr/local/bin/text-embeddings-inference" {
		t.Errorf("Binary = %q", spec.Binary)
	}
	if strings.Join(spec.Args, " ") != "--model-id BAAI/bge-m3 --port 8090" {
		t.Errorf("Args = %v", spec.Args)
	}
	if len(spec.Env) != 1 || spec.Env[0] != "RUST_LOG=info" {
		t.Errorf("Env = %v", spec.Env)
	}
	if spec.HealthURL != "http://127.0.0.1:8090/health" {
		t.Errorf("HealthURL = %q", spec.HealthURL)
	}
}

func TestHTTPServerSpec_CustomHealthPath(t *testing.T) {
	p := &Profile{
		Name: "x",
		Backend: Backend{HTTPServer: &HTTPServerBackend{
			Image:      "foo:latest",
			Port:       9000,
			HealthPath: "v1/status",
		}},
	}
	spec, err := HTTPServerSpec(p, p.Name)
	if err != nil {
		t.Fatal(err)
	}
	if spec.HealthURL != "http://127.0.0.1:9000/v1/status" {
		t.Errorf("HealthURL = %q (leading slash should be added)", spec.HealthURL)
	}
}

func TestHTTPServerSpec_WrongBackendErrors(t *testing.T) {
	p := &Profile{Backend: Backend{}}
	if _, err := HTTPServerSpec(p, "x"); err == nil {
		t.Errorf("expected error when http_server is nil")
	}
}

func TestValidateHTTPServer_ImageBinaryXOR(t *testing.T) {
	cases := []struct {
		name    string
		h       HTTPServerBackend
		wantErr string
	}{
		{name: "neither", h: HTTPServerBackend{Port: 1}, wantErr: "image (docker mode) or binary"},
		{name: "both", h: HTTPServerBackend{Image: "i", Binary: "b", Port: 1}, wantErr: "mutually exclusive"},
		{name: "port_zero", h: HTTPServerBackend{Image: "i", Port: 0}, wantErr: "port must be > 0"},
		{name: "volumes_in_binary_mode", h: HTTPServerBackend{Binary: "b", Port: 1, Volumes: []string{"a:b"}}, wantErr: "volumes is only valid in docker mode"},
		{name: "gpu_in_binary_mode", h: HTTPServerBackend{Binary: "b", Port: 1, GPU: true}, wantErr: "gpu is only valid in docker mode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateHTTPServer(&tc.h)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}
