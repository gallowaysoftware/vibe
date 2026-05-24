package profile

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/gallowaysoftware/vibe/internal/vibe/supervisor"
)

// LlamaServerSpec returns the LaunchSpec for a llama-server-backed profile.
// `binary` is the path / $PATH name to exec (e.g. "llama-server" or an
// absolute path from daemon config). `port` is the localhost port the child
// should listen on; the daemon picks it ahead of time so it can also point
// the proxy at the resulting addr.
func LlamaServerSpec(p *Profile, binary string, port int) (supervisor.LaunchSpec, error) {
	if p == nil || p.Backend.LlamaServer == nil {
		return supervisor.LaunchSpec{}, errors.New("LlamaServerSpec: profile has no llama_server backend")
	}
	if binary == "" {
		binary = "llama-server"
	}
	m := p.Backend.LlamaServer
	args := []string{
		"--model", m.Path,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(port),
		"--alias", m.Alias,
		"--ctx-size", strconv.Itoa(m.Context),
		"--parallel", strconv.Itoa(m.Parallel),
	}
	if m.GPULayers > 0 {
		args = append(args, "--n-gpu-layers", strconv.Itoa(m.GPULayers))
	}
	if m.FlashAttn {
		args = append(args, "--flash-attn", "on")
	}
	if m.CacheTypeK != "" {
		args = append(args, "--cache-type-k", m.CacheTypeK)
	}
	if m.CacheTypeV != "" {
		args = append(args, "--cache-type-v", m.CacheTypeV)
	}
	if m.Jinja {
		args = append(args, "--jinja")
	}
	if m.MMProj != "" {
		args = append(args, "--mmproj", m.MMProj)
	}
	args = append(args, m.ExtraArgs...)

	return supervisor.LaunchSpec{
		Binary:    binary,
		Args:      args,
		HealthURL: fmt.Sprintf("http://127.0.0.1:%d/health", port),
	}, nil
}

// HTTPServerSpec returns the LaunchSpec for an http_server-backed profile.
// Two modes:
//
//   - **docker mode** (Backend.Image set): synthesises a `docker run --rm`
//     invocation with the configured Port published, Volumes mapped, Env
//     forwarded as -e flags, optional --gpus all, and the image's CMD
//     overridden by Args when non-empty. `--name vibe-<profile-name>` is
//     attached so a stuck container can be inspected / killed by name; we
//     also tear it down explicitly on stop in case docker's SIGINT
//     handling doesn't fire cleanly.
//
//   - **binary mode** (Backend.Binary set): straightforward exec with Args
//
//   - Env, identical to the existing llama_server / comfyui pattern.
//
// HealthURL defaults to http://127.0.0.1:Port/health, overridable via
// Backend.HealthPath.
//
// `profileName` is used in docker mode to name the container so a future
// stop / restart targets the exact instance. Pass p.Name from the caller.
func HTTPServerSpec(p *Profile, profileName string) (supervisor.LaunchSpec, error) {
	if p == nil || p.Backend.HTTPServer == nil {
		return supervisor.LaunchSpec{}, errors.New("HTTPServerSpec: profile has no http_server backend")
	}
	h := p.Backend.HTTPServer
	if h.Port <= 0 {
		return supervisor.LaunchSpec{}, errors.New("HTTPServerSpec: backend.http_server.port must be > 0")
	}
	healthPath := h.HealthPath
	if healthPath == "" {
		healthPath = "/health"
	}
	if healthPath[0] != '/' {
		healthPath = "/" + healthPath
	}
	healthURL := fmt.Sprintf("http://127.0.0.1:%d%s", h.Port, healthPath)

	// Deterministic env-flag order so the resulting argv is stable
	// across map-iteration random walks (helps diffability in logs +
	// makes the supervisor's startup line reproducible).
	envKeys := make([]string, 0, len(h.Env))
	for k := range h.Env {
		envKeys = append(envKeys, k)
	}
	sortStrings(envKeys)

	if h.Image != "" {
		containerPort := h.ContainerPort
		if containerPort == 0 {
			containerPort = h.Port
		}
		args := []string{
			"run", "--rm",
			"--name", "vibe-" + profileName,
			"-p", fmt.Sprintf("127.0.0.1:%d:%d", h.Port, containerPort),
		}
		if h.GPU {
			args = append(args, "--gpus", "all")
		}
		for _, v := range h.Volumes {
			args = append(args, "-v", v)
		}
		for _, k := range envKeys {
			args = append(args, "-e", k+"="+h.Env[k])
		}
		args = append(args, h.Image)
		args = append(args, h.Args...)
		return supervisor.LaunchSpec{
			Binary:    "docker",
			Args:      args,
			HealthURL: healthURL,
		}, nil
	}

	// Binary mode.
	var env []string
	for _, k := range envKeys {
		env = append(env, k+"="+h.Env[k])
	}
	return supervisor.LaunchSpec{
		Binary:    h.Binary,
		Args:      h.Args,
		Env:       env,
		HealthURL: healthURL,
	}, nil
}

// sortStrings is the local stable sort used by HTTPServerSpec for env-flag
// ordering. Avoids pulling in sort throughout the package for one site.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// TabbyAPISpec returns the LaunchSpec for a tabbyAPI-backed profile.
//
// argv: <venv>/bin/python start.py --model-dir <ModelDir> --port <Port>
//       --max-seq-len <Context> [--cache-mode <CacheMode>]
//       [--draft-model-dir <DraftModelDir>] [extra_args...]
//       (run from <Repo> as workdir)
//
// HealthURL: /health — tabbyAPI exposes a small JSON readiness endpoint
// once the model has finished loading. Health-check returns 503 during
// load and 200 once ready, so the supervisor waiting on a 2xx is the
// right gate even for slow EXL3 loads (~30-90s for a 32B).
//
// Note: tabbyAPI's start.py prefers a config file, but every knob we
// expose also has a CLI flag. We use the CLI form because it stays
// pure-stateless: no temp config files to write or clean up, the
// argv is fully reproducible from the profile YAML alone.
func TabbyAPISpec(p *Profile) (supervisor.LaunchSpec, error) {
	if p == nil || p.Backend.TabbyAPI == nil {
		return supervisor.LaunchSpec{}, errors.New("TabbyAPISpec: profile has no tabby_api backend")
	}
	t := p.Backend.TabbyAPI
	if t.Port <= 0 {
		return supervisor.LaunchSpec{}, errors.New("TabbyAPISpec: backend.tabby_api.port must be > 0")
	}
	args := []string{
		"start.py",
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(t.Port),
		"--model-dir", t.ModelDir,
		"--max-seq-len", strconv.Itoa(t.Context),
		"--model-alias", t.Alias,
	}
	if t.CacheMode != "" {
		args = append(args, "--cache-mode", t.CacheMode)
	}
	if t.DraftModelDir != "" {
		args = append(args, "--draft-model-dir", t.DraftModelDir)
	}
	args = append(args, t.ExtraArgs...)

	python := filepath.Join(t.Venv, "bin", "python")
	return supervisor.LaunchSpec{
		Binary:    python,
		Args:      args,
		Workdir:   filepath.Clean(t.Repo),
		HealthURL: fmt.Sprintf("http://127.0.0.1:%d/health", t.Port),
	}, nil
}

// ComfyUISpec returns the LaunchSpec for a ComfyUI-backed profile.
//
// argv: python main.py --listen <addr> --port <port> [extra_args...]
//
// HealthURL: /system_stats — a small JSON endpoint ComfyUI exposes once the
// app is fully initialised. It returns 200 with system info; we just care
// about the status code.
func ComfyUISpec(p *Profile, port int) (supervisor.LaunchSpec, error) {
	if p == nil || p.Backend.ComfyUI == nil {
		return supervisor.LaunchSpec{}, errors.New("ComfyUISpec: profile has no comfyui backend")
	}
	c := p.Backend.ComfyUI
	if c.Dir == "" {
		return supervisor.LaunchSpec{}, errors.New("ComfyUISpec: backend.comfyui.dir is required")
	}
	python := c.Python
	if python == "" {
		python = "python3"
	}
	listen := c.Listen
	if listen == "" {
		listen = "127.0.0.1"
	}
	args := []string{
		"main.py",
		"--listen", listen,
		"--port", strconv.Itoa(port),
	}
	args = append(args, c.ExtraArgs...)

	// Health URL always points at localhost: the proxy/daemon side only
	// reaches the child via 127.0.0.1 regardless of the --listen choice.
	return supervisor.LaunchSpec{
		Binary:    python,
		Args:      args,
		Workdir:   filepath.Clean(c.Dir),
		HealthURL: fmt.Sprintf("http://127.0.0.1:%d/system_stats", port),
	}, nil
}
