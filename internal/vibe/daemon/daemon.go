// Package daemon is the long-running supervisor process. It owns the
// llama-server supervisor and the reverse proxy, and exposes a Connect/RPC
// control plane on both a unix socket (for the local CLI) and a TCP listener
// on 127.0.0.1 (for vibeclient/vamp).
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gopkg.in/yaml.v3"

	"github.com/gallowaysoftware/vibe/internal/vibe/frontend"
	"github.com/gallowaysoftware/vibe/internal/vibe/hfdownload"
	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
	"github.com/gallowaysoftware/vibe/internal/vibe/proxy"
	"github.com/gallowaysoftware/vibe/internal/vibe/supervisor"
	"github.com/gallowaysoftware/vibe/internal/vibe/vram"
	vibev1 "github.com/gallowaysoftware/vibe/proto/vibe/v1"
	"github.com/gallowaysoftware/vibe/proto/vibe/v1/vibev1connect"
)

const (
	defaultProxyPort = 9000
	defaultHTTPAddr  = "127.0.0.1:9001"
	defaultHTTPPort  = "9001"
)

type Config struct {
	ProxyPort   int    `yaml:"proxy_port,omitempty"`
	HTTPAddr    string `yaml:"http_addr,omitempty"`    // empty → "127.0.0.1:9001"
	LlamaBinary string `yaml:"llama_binary,omitempty"` // empty → "llama-server" from $PATH
	// BindAll, when true, switches the TCP listener from 127.0.0.1 to
	// 0.0.0.0 so a remote machine on the LAN can reach the control plane.
	// Bearer-token auth is enforced on the TCP listener regardless. If the
	// user sets HTTPAddr explicitly (e.g. "0.0.0.0:9001"), that value wins
	// and BindAll is ignored.
	BindAll bool `yaml:"bind_all,omitempty"`
}

// LoadConfig reads the global vibe config; missing file is not an error.
func LoadConfig() (Config, error) {
	c := Config{ProxyPort: defaultProxyPort, HTTPAddr: defaultHTTPAddr}
	data, err := os.ReadFile(paths.ConfigFile())
	if errors.Is(err, os.ErrNotExist) {
		return c.resolveHTTPAddr(), nil
	}
	if err != nil {
		return c, err
	}
	// Don't default HTTPAddr before unmarshal: we need to tell "user didn't
	// set it" from "user set it to the default" so BindAll can take effect
	// only when HTTPAddr is unset.
	c.HTTPAddr = ""
	if err := yaml.Unmarshal(data, &c); err != nil {
		return c, err
	}
	if c.ProxyPort == 0 {
		c.ProxyPort = defaultProxyPort
	}
	return c.resolveHTTPAddr(), nil
}

// resolveHTTPAddr fills in HTTPAddr from BindAll when the user didn't set
// HTTPAddr explicitly. An explicit HTTPAddr always wins.
func (c Config) resolveHTTPAddr() Config {
	if c.HTTPAddr != "" {
		return c
	}
	if c.BindAll {
		c.HTTPAddr = "0.0.0.0:" + defaultHTTPPort
	} else {
		c.HTTPAddr = defaultHTTPAddr
	}
	return c
}

type Daemon struct {
	cfg Config
	sup *supervisor.Supervisor
	prx *proxy.Proxy

	// nvidiaSMI is the VRAM probe used by Start for its pre-flight check.
	// Tests inject a stub here; production wires vram.NvidiaSMIProbe via New.
	nvidiaSMI vram.Probe
	// vramSlopGiB is added to free VRAM before comparing to the profile's
	// estimate, absorbing the inherent fuzziness of those numbers. Defaults
	// to vram.DefaultSlopGiB in New.
	vramSlopGiB float64

	// startMu serializes start/stop operations against each other.
	startMu sync.Mutex

	mu        sync.Mutex
	active    *profile.Profile
	startTime time.Time
	frontend  *frontend.Result

	shutdown     chan struct{}
	shutdownOnce sync.Once
}

// Compile-time check: Daemon implements the Connect service.
var _ vibev1connect.ControlServiceHandler = (*Daemon)(nil)

func New(cfg Config) *Daemon {
	return &Daemon{
		cfg:         cfg,
		sup:         supervisor.New(),
		prx:         proxy.New(fmt.Sprintf("127.0.0.1:%d", cfg.ProxyPort)),
		nvidiaSMI:   vram.NvidiaSMIProbe,
		vramSlopGiB: vram.DefaultSlopGiB,
		shutdown:    make(chan struct{}),
	}
}

// SetVRAMProbe overrides the VRAM-free probe. Used by tests to avoid shelling
// out to nvidia-smi.
func (d *Daemon) SetVRAMProbe(p vram.Probe) {
	d.nvidiaSMI = p
}

// Run brings up the proxy and both control-plane listeners (unix + TCP),
// blocking until ctx is canceled or a Shutdown RPC fires.
func (d *Daemon) Run(ctx context.Context) error {
	if err := paths.EnsureDirs(); err != nil {
		return fmt.Errorf("ensure dirs: %w", err)
	}
	if err := ensureSingleInstance(); err != nil {
		return err
	}

	sockPath := paths.Socket()
	_ = os.Remove(sockPath)
	unixLn, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("listen unix %s: %w", sockPath, err)
	}
	defer os.Remove(sockPath)
	if err := os.Chmod(sockPath, 0o600); err != nil {
		return err
	}

	httpLn, err := net.Listen("tcp", d.cfg.HTTPAddr)
	if err != nil {
		unixLn.Close()
		return fmt.Errorf("listen tcp %s: %w", d.cfg.HTTPAddr, err)
	}

	if err := writePIDFile(); err != nil {
		return err
	}
	defer os.Remove(paths.PIDFile())

	if err := d.prx.Start(); err != nil {
		return fmt.Errorf("start proxy: %w", err)
	}
	defer d.prx.Stop(context.Background())

	// Generate or load the bearer token before binding the TCP server.
	// The unix socket reuses the same Connect handler but skips token
	// validation (0600 socket perms are the auth boundary there).
	token, err := LoadOrCreateToken()
	if err != nil {
		unixLn.Close()
		httpLn.Close()
		return fmt.Errorf("load token: %w", err)
	}

	mux := http.NewServeMux()
	mountPath, connectHandler := vibev1connect.NewControlServiceHandler(d)
	mux.Handle(mountPath, connectHandler)

	unixSrv := &http.Server{Handler: mux}
	// The TCP server gets a bearer-auth wrapper around the same mux. Two
	// separate http.Server instances (one per listener) is the cleanest
	// way to express "only TCP requires auth" without leaking the
	// distinction into per-RPC interceptors.
	httpSrv := &http.Server{Handler: bearerAuthMiddleware(token, mux)}
	errCh := make(chan error, 2)
	serve := func(srv *http.Server, ln net.Listener) {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}
	go serve(unixSrv, unixLn)
	go serve(httpSrv, httpLn)

	slog.Info("daemon ready",
		"unix_socket", sockPath,
		"http_addr", d.cfg.HTTPAddr,
		"proxy_addr", fmt.Sprintf("127.0.0.1:%d", d.cfg.ProxyPort),
		"llama_binary", d.cfg.LlamaBinary,
		"token_file", paths.TokenFile())

	var shutReason string
	select {
	case <-ctx.Done():
		shutReason = "context canceled"
	case <-d.shutdown:
		shutReason = "shutdown rpc"
	case err := <-errCh:
		slog.Error("listener failed", "err", err)
		return err
	}
	slog.Info("daemon shutting down", "reason", shutReason)
	shCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Tear down the active frontend (if any) first; otherwise a
	// docker-compose stack outlives the daemon and keeps serving stale
	// requests at the (now-dead) proxy.
	d.mu.Lock()
	fr := d.frontend
	d.mu.Unlock()
	if fr != nil {
		if err := frontend.Deactivate(shCtx, fr); err != nil {
			slog.Warn("frontend deactivate on shutdown failed", "err", err)
		}
	}
	// Stop the supervisor unconditionally; if a start is mid-flight the
	// child exists and SIGINT will unblock waitReady.
	_ = d.sup.Stop(shCtx)
	d.prx.SetBackend(nil)
	_ = unixSrv.Shutdown(shCtx)
	_ = httpSrv.Shutdown(shCtx)
	return nil
}

// ─── Connect handler methods ────────────────────────────────────────────────

func (d *Daemon) Status(_ context.Context, _ *connect.Request[vibev1.StatusRequest]) (*connect.Response[vibev1.StatusResponse], error) {
	return connect.NewResponse(&vibev1.StatusResponse{Status: d.protoStatus()}), nil
}

func (d *Daemon) ListProfiles(_ context.Context, _ *connect.Request[vibev1.ListProfilesRequest]) (*connect.Response[vibev1.ListProfilesResponse], error) {
	dir := paths.ProfilesDir()
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	var out []*vibev1.Profile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		p, err := profile.Load(path)
		if err != nil {
			continue
		}
		out = append(out, &vibev1.Profile{Name: p.Name, Description: p.Description, Path: path})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return connect.NewResponse(&vibev1.ListProfilesResponse{Profiles: out}), nil
}

func (d *Daemon) Start(_ context.Context, req *connect.Request[vibev1.StartRequest]) (*connect.Response[vibev1.StartResponse], error) {
	if !d.startMu.TryLock() {
		return nil, connect.NewError(connect.CodeAborted, errors.New("another start/stop is in progress"))
	}
	defer d.startMu.Unlock()

	profileName := strings.TrimSpace(req.Msg.Profile)
	if profileName == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("profile name required"))
	}

	d.mu.Lock()
	if d.active != nil {
		name := d.active.Name
		d.mu.Unlock()
		return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("profile %q is already running; stop first", name))
	}
	d.mu.Unlock()

	p, err := loadProfileByName(profileName)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}

	startCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Pre-flight VRAM check. We only run this when the profile declares an
	// estimate (otherwise the older zero-VRAM profiles would all skip
	// silently). A missing nvidia-smi degrades to a warning so users on
	// CPU/AMD/Apple-Silicon hosts aren't blocked.
	if !req.Msg.NoVramCheck && p.EstimatedVRAMGB > 0 {
		res := vram.Check(startCtx, d.nvidiaSMI, p.EstimatedVRAMGB, d.vramSlopGiB)
		switch {
		case res.Skipped:
			slog.Warn("vram pre-flight skipped",
				"profile", p.Name,
				"estimated_gib", p.EstimatedVRAMGB,
				"reason", res.Message)
		case !res.OK:
			msg := fmt.Sprintf(
				"profile %q needs ~%.1f GiB free VRAM but only %.1f GiB is free.\nStop the current profile (`vibe stop`) or close other GPU users first.",
				p.Name, p.EstimatedVRAMGB, res.FreeGiB)
			slog.Warn("vram pre-flight failed",
				"profile", p.Name,
				"estimated_gib", p.EstimatedVRAMGB,
				"free_gib", res.FreeGiB)
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New(msg))
		default:
			slog.Info("vram pre-flight ok",
				"profile", p.Name,
				"estimated_gib", p.EstimatedVRAMGB,
				"free_gib", res.FreeGiB)
		}
	}

	// Dispatch by backend kind. Each branch fully populates `spec` and the
	// chosen `port`, then the shared tail starts the supervisor.
	var (
		spec supervisor.LaunchSpec
		port int
	)
	switch {
	case p.Backend.LlamaServer != nil:
		port, err = supervisor.PickFreePort()
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("pick port: %w", err))
		}
		spec, err = profile.LlamaServerSpec(p, d.cfg.LlamaBinary, port)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		slog.Info("starting profile (llama_server)",
			"profile", p.Name, "alias", p.Backend.LlamaServer.Alias,
			"context", p.Backend.LlamaServer.Context, "port", port)
	case p.Backend.ComfyUI != nil:
		port = p.Backend.ComfyUI.Port
		if port == 0 {
			port, err = supervisor.PickFreePort()
			if err != nil {
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("pick port: %w", err))
			}
		}
		spec, err = profile.ComfyUISpec(p, port)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		slog.Info("starting profile (comfyui)",
			"profile", p.Name, "dir", p.Backend.ComfyUI.Dir, "port", port)
	default:
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			errors.New("profile has no backend (set backend.llama_server or backend.comfyui)"))
	}

	if err := d.sup.Start(startCtx, spec, port); err != nil {
		slog.Error("supervisor start failed", "profile", p.Name, "err", err)
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("start backend: %w", err))
	}

	st := d.sup.Status()

	// Wire the proxy and frontend only for llama-server. ComfyUI is reached
	// directly via Status.BackendAddr by vamp / external tools, and ComfyUI
	// already ships its own UI so there's no frontend to activate.
	var fr *frontend.Result
	if p.Backend.LlamaServer != nil {
		backendURL, err := url.Parse(st.Addr)
		if err != nil {
			_ = d.sup.Stop(context.Background())
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("parse backend url: %w", err))
		}
		d.prx.SetBackend(backendURL)

		if p.Frontend.Kind != "" {
			vibeAPI := fmt.Sprintf("http://127.0.0.1:%d/v1", d.cfg.ProxyPort)
			fr, err = frontend.ActivateWithContext(startCtx, p, profile.ExpandContext{
				VibeAPI:      vibeAPI,
				ModelAlias:   p.Backend.LlamaServer.Alias,
				ModelContext: p.Backend.LlamaServer.Context,
				VibeStateDir: paths.StateHome(),
			})
			if err != nil {
				_ = d.sup.Stop(context.Background())
				d.prx.SetBackend(nil)
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("activate frontend: %w", err))
			}
		}
	}

	d.mu.Lock()
	d.active = p
	d.startTime = time.Now()
	d.frontend = fr
	d.mu.Unlock()

	resp := &vibev1.StartResponse{Status: d.protoStatus()}
	if fr != nil {
		slog.Info("profile started", "profile", p.Name, "backend", st.Addr, "wrote", fr.WroteFile)
		resp.Frontend = &vibev1.FrontendInfo{
			App:             p.Frontend.App,
			WroteFile:       fr.WroteFile,
			RestartRequired: fr.RestartRequired,
			EnvVars:         fr.Env,
		}
	} else {
		slog.Info("profile started", "profile", p.Name, "backend", st.Addr)
	}
	return connect.NewResponse(resp), nil
}

func (d *Daemon) Stop(ctx context.Context, _ *connect.Request[vibev1.StopRequest]) (*connect.Response[vibev1.StopResponse], error) {
	if !d.startMu.TryLock() {
		return nil, connect.NewError(connect.CodeAborted, errors.New("another start/stop is in progress"))
	}
	defer d.startMu.Unlock()
	if err := d.stopActive(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&vibev1.StopResponse{Status: d.protoStatus()}), nil
}

func (d *Daemon) Shutdown(_ context.Context, _ *connect.Request[vibev1.ShutdownRequest]) (*connect.Response[vibev1.ShutdownResponse], error) {
	go func() {
		// Trigger after the response flushes.
		time.Sleep(50 * time.Millisecond)
		d.shutdownOnce.Do(func() { close(d.shutdown) })
	}()
	return connect.NewResponse(&vibev1.ShutdownResponse{}), nil
}

func (d *Daemon) Logs(_ context.Context, _ *connect.Request[vibev1.LogsRequest]) (*connect.Response[vibev1.LogsResponse], error) {
	return connect.NewResponse(&vibev1.LogsResponse{Lines: d.sup.Logs()}), nil
}

func (d *Daemon) Pull(ctx context.Context, req *connect.Request[vibev1.PullRequest], stream *connect.ServerStream[vibev1.PullProgress]) error {
	if !d.startMu.TryLock() {
		return connect.NewError(connect.CodeAborted, errors.New("another start/stop/pull is in progress"))
	}
	defer d.startMu.Unlock()

	name := strings.TrimSpace(req.Msg.Profile)
	if name == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("profile name required"))
	}
	p, err := loadProfileByName(name)
	if err != nil {
		return connect.NewError(connect.CodeNotFound, err)
	}
	// ComfyUI manages its own model assets (ComfyUI-Manager + manual placement
	// under models/); vibe has nothing to pull. Surface a clear DONE so the
	// CLI doesn't spin forever waiting for download phase messages.
	if p.Backend.ComfyUI != nil {
		return stream.Send(&vibev1.PullProgress{
			Phase:   vibev1.PullProgress_PHASE_DONE,
			Message: "no model file to pull (ComfyUI manages its own model assets)",
		})
	}
	if p.Backend.LlamaServer == nil {
		return connect.NewError(connect.CodeFailedPrecondition,
			errors.New("profile has no backend"))
	}
	m := p.Backend.LlamaServer
	if m.Huggingface == nil {
		return stream.Send(&vibev1.PullProgress{
			Phase:   vibev1.PullProgress_PHASE_DONE,
			Message: "no huggingface block; nothing to pull",
		})
	}

	spec := hfdownload.Spec{
		Repo:     m.Huggingface.Repo,
		File:     m.Huggingface.File,
		Revision: m.Huggingface.Revision,
	}
	if err := stream.Send(&vibev1.PullProgress{Phase: vibev1.PullProgress_PHASE_RESOLVING}); err != nil {
		return err
	}
	slog.Info("pulling model", "profile", p.Name, "repo", spec.Repo, "file", spec.File)

	// Track whether bytes actually flowed; hfdownload.Download skips the
	// progress callback when it short-circuits (local-and-cached path).
	var bytesFlowed bool
	progress := func(downloaded, total int64) {
		bytesFlowed = true
		_ = stream.Send(&vibev1.PullProgress{
			Phase:           vibev1.PullProgress_PHASE_DOWNLOADING,
			DownloadedBytes: downloaded,
			TotalBytes:      total,
		})
	}
	if err := hfdownload.Download(ctx, spec, m.Path, progress); err != nil {
		slog.Error("download failed", "profile", p.Name, "err", err)
		return connect.NewError(connect.CodeInternal, fmt.Errorf("download: %w", err))
	}

	var finalSize int64
	if info, err := os.Stat(m.Path); err == nil {
		finalSize = info.Size()
	}
	msg := "complete"
	if !bytesFlowed {
		msg = "already cached"
		slog.Info("model already cached", "profile", p.Name, "path", m.Path, "size", finalSize)
	} else {
		slog.Info("download complete", "profile", p.Name, "path", m.Path, "size", finalSize)
	}
	return stream.Send(&vibev1.PullProgress{
		Phase:           vibev1.PullProgress_PHASE_DONE,
		DownloadedBytes: finalSize,
		TotalBytes:      finalSize,
		Message:         msg,
	})
}

// ─── Internals ──────────────────────────────────────────────────────────────

func (d *Daemon) stopActive(ctx context.Context) error {
	d.mu.Lock()
	active := d.active
	fr := d.frontend
	d.mu.Unlock()
	if active == nil {
		return nil
	}
	slog.Info("stopping profile", "profile", active.Name)
	// Tear down the frontend first. For docker-compose this issues
	// `docker compose down`, which may make requests against the proxy on
	// its way out — better to fail those requests cleanly than to surface
	// 502s from a half-stopped proxy/supervisor.
	if fr != nil {
		if err := frontend.Deactivate(ctx, fr); err != nil {
			// Log and continue: leaving a stack up is bad, but failing to
			// stop the supervisor is worse. The user can still `docker
			// compose down` by hand if needed.
			slog.Warn("frontend deactivate failed", "profile", active.Name, "err", err)
		}
	}
	if err := d.sup.Stop(ctx); err != nil {
		return err
	}
	d.prx.SetBackend(nil)
	d.mu.Lock()
	d.active = nil
	d.frontend = nil
	d.mu.Unlock()
	return nil
}

func (d *Daemon) protoStatus() *vibev1.Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.sup.Status()
	s := &vibev1.Status{
		Running:     d.active != nil,
		Ready:       st.State == supervisor.StateReady,
		BackendAddr: st.Addr,
		ProxyAddr:   fmt.Sprintf("http://127.0.0.1:%d", d.cfg.ProxyPort),
		Pid:         int32(st.PID),
	}
	if d.active != nil {
		s.Profile = d.active.Name
		s.StartedAt = timestamppb.New(d.startTime)
	}
	if d.frontend != nil {
		s.FrontendEnv = d.frontend.Env
	}
	return s
}

func loadProfileByName(name string) (*profile.Profile, error) {
	path := filepath.Join(paths.ProfilesDir(), name+".yaml")
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("profile %q not found at %s", name, path)
	}
	return profile.Load(path)
}

func writePIDFile() error {
	return os.WriteFile(paths.PIDFile(), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644)
}

func ensureSingleInstance() error {
	data, err := os.ReadFile(paths.PIDFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		_ = os.Remove(paths.PIDFile())
		return nil
	}
	if err := syscall.Kill(pid, 0); err == nil {
		return fmt.Errorf("vibe daemon already running (pid %d)", pid)
	}
	_ = os.Remove(paths.PIDFile())
	return nil
}
