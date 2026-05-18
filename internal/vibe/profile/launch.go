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
