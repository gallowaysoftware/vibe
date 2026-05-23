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
	"os/exec"
	"path/filepath"
	"regexp"
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

// serviceInstance bundles the per-service state for one running
// service-mode profile. Services run alongside each other and
// alongside the active profile; each has its own supervisor so a
// crash + auto-respawn in one service doesn't perturb the others.
type serviceInstance struct {
	profile   *profile.Profile
	sup       *supervisor.Supervisor
	startTime time.Time
	port      int
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
	// services is the map of running service-mode profiles, keyed by
	// profile name. Empty in the legacy single-active-profile case.
	// Each entry has its own supervisor so crashes don't cross-
	// contaminate. The map itself is protected by mu; entries are
	// otherwise immutable once stored (callers read pointers, never
	// mutate).
	services map[string]*serviceInstance

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
		services:    map[string]*serviceInstance{},
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
	// Clear d.active BEFORE stopping the supervisor — same rationale
	// as stopActive does for a per-profile stop. The
	// watchBackendForRespawn goroutine wakes the instant the
	// supervisor's `stopped` channel closes; if d.active still names
	// the about-to-be-killed profile, the watcher fires a respawn
	// against the shutting-down process and we spawn an orphan
	// backend during shutdown.
	d.active = nil
	d.frontend = nil
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
	return connect.NewResponse(&vibev1.StatusResponse{
		Status:   d.protoStatus(),
		Services: d.servicesStatus(),
	}), nil
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

	p, err := loadProfileByName(profileName)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}

	startCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Service-mode profiles run as background sidecars: their own
	// supervisor instance, no proxy / frontend wiring, no contention
	// for the daemon's single "active" slot. Branch off the active
	// path here so the existing code below stays untouched.
	if p.ResolvedMode() == profile.ModeService {
		return d.startService(startCtx, p)
	}

	d.mu.Lock()
	if d.active != nil {
		name := d.active.Name
		d.mu.Unlock()
		return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("profile %q is already running; stop first", name))
	}
	d.mu.Unlock()

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

	// Dispatch by backend kind to build the launch spec + pick a port.
	spec, port, err := d.buildLaunchSpec(p)
	if err != nil {
		return nil, err
	}

	if err := d.sup.Start(startCtx, spec, port); err != nil {
		slog.Error("supervisor start failed", "profile", p.Name, "err", err)
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("start backend: %w", err))
	}

	st := d.sup.Status()

	// Wire the proxy for llama_server and http_server backends — both
	// front a generic HTTP API that vamp / external tools reach through
	// the proxy. ComfyUI is reached directly via Status.BackendAddr
	// (workflow API is one big POST, no streaming, no profile-managed
	// state worth proxying) and ships its own UI so there's no frontend.
	var fr *frontend.Result
	if p.Backend.LlamaServer != nil || p.Backend.HTTPServer != nil {
		backendURL, err := url.Parse(st.Addr)
		if err != nil {
			_ = d.sup.Stop(context.Background())
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("parse backend url: %w", err))
		}
		d.prx.SetBackend(backendURL)
	}
	// Frontend activation is llama-server-only: only those profiles have
	// a separate UI/client process to launch.
	if p.Backend.LlamaServer != nil {
		if p.Frontend.Kind != "" {
			// Pre-create the per-profile frontend state dir so docker-compose
			// bind mounts (and any other path the profile points inside it)
			// don't fail with a "no such directory" race the first time a
			// profile activates. Cheap idempotent mkdir.
			stateDir := paths.FrontendStateDir(p.Name)
			if err := os.MkdirAll(stateDir, 0o755); err != nil {
				_ = d.sup.Stop(context.Background())
				d.prx.SetBackend(nil)
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create frontend state dir %s: %w", stateDir, err))
			}

			vibeAPI := fmt.Sprintf("http://127.0.0.1:%d/v1", d.cfg.ProxyPort)
			ectx := profile.ExpandContext{
				VibeAPI:      vibeAPI,
				ModelAlias:   p.Backend.LlamaServer.Alias,
				ModelContext: p.Backend.LlamaServer.Context,
				VibeStateDir: paths.StateHome(),
			}
			if req.Msg.Foreground {
				fr, err = frontend.ActivateForeground(startCtx, p, ectx)
			} else {
				fr, err = frontend.ActivateWithContext(startCtx, p, ectx)
			}
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

	// Auto-respawn watcher: when the supervised process exits mid-life
	// (after reaching Ready) without an operator-initiated Stop, the
	// backend probably crashed (e.g. llama-server SIGABRT from a flaky
	// CUDA kernel mid-foreach). Without this, the daemon stays in
	// "Running but !Ready" forever and every vamp retry returns 502.
	// We try up to maxBackendRespawns within respawnWindow; on the last
	// failed restart we clear d.active so the next vamp EnsureActive
	// goes through a clean start path.
	go d.watchBackendForRespawn(p.Name, spec, port)

	resp := &vibev1.StartResponse{Status: d.protoStatus()}
	if fr != nil {
		slog.Info("profile started", "profile", p.Name, "backend", st.Addr, "wrote", fr.WroteFile)
		resp.Frontend = &vibev1.FrontendInfo{
			WroteFile:       fr.WroteFile,
			RestartRequired: fr.RestartRequired,
			EnvVars:         fr.Env,
			Kind:            p.Frontend.Kind,
			Url:             p.Frontend.BrowserURL(),
		}
	} else {
		slog.Info("profile started", "profile", p.Name, "backend", st.Addr)
	}
	return connect.NewResponse(resp), nil
}

const (
	// maxBackendRespawns is the cap on automatic restarts of a crashed
	// backend within respawnWindow. We size it for a worst-case 4-5 h
	// pipeline (cibd-distilling Module 3 vision phase) where the
	// llama.cpp flash-attn pool-VMM kernel SIGABRTs every ~3-4 min on
	// the current 595.71.05 / kernel 7.0.9 combo. 60 respawns / 30 min
	// = roughly one respawn every 30 s before the budget trips, which
	// still catches a deterministic-on-every-restart crash (would burn
	// the budget in seconds) while surviving the flaky CUDA hiccup
	// case. Lower this once the upstream FA kernel is fixed.
	maxBackendRespawns = 60
	respawnWindow      = 30 * time.Minute
)

// watchBackendForRespawn watches the active supervisor for unexpected
// exits and re-launches the same spec on the same port if the operator
// hasn't manually stopped the profile. Exits on:
//   - clean Stop (d.active cleared)
//   - profile change (d.active.Name != name)
//   - respawn budget exhausted
func (d *Daemon) watchBackendForRespawn(name string, spec supervisor.LaunchSpec, port int) {
	respawns := 0
	windowStart := time.Now()
	for {
		stopped := d.sup.Stopped()
		if stopped == nil {
			return
		}
		<-stopped

		d.mu.Lock()
		stillActive := d.active != nil && d.active.Name == name
		d.mu.Unlock()
		if !stillActive {
			// Operator-initiated Stop or profile switch: nothing to do.
			return
		}
		if time.Since(windowStart) > respawnWindow {
			respawns = 0
			windowStart = time.Now()
		}
		if respawns >= maxBackendRespawns {
			slog.Error("backend respawn budget exhausted; giving up",
				"profile", name, "respawns", respawns, "window", respawnWindow)
			d.mu.Lock()
			d.active = nil
			d.mu.Unlock()
			d.prx.SetBackend(nil)
			return
		}
		respawns++
		slog.Warn("backend exited unexpectedly; auto-respawning",
			"profile", name, "attempt", respawns, "of", maxBackendRespawns)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		err := d.sup.Start(ctx, spec, port)
		cancel()
		if err != nil {
			slog.Error("backend respawn failed", "profile", name, "attempt", respawns, "err", err)
			// A failed Start leaves supervisor in StateExited with its
			// `stopped` channel already closed; the next loop iteration
			// would unblock instantly and we'd burn the entire respawn
			// budget in <1 s on a deterministic config error
			// (typo in model path, missing binary, etc.). Sleep a beat
			// so the operator has time to spot the recurring failure
			// in the log before the budget trips.
			time.Sleep(2 * time.Second)
			continue
		}
		// Re-wire the proxy at the same backend URL — Supervisor.Start
		// reuses the same port (we pass it in), so the URL is identical
		// to the original, but SetBackend is cheap and avoids any
		// staleness if the proxy was cleared during a transient.
		backendURL, perr := url.Parse(d.sup.Status().Addr)
		if perr == nil && backendURL != nil {
			d.prx.SetBackend(backendURL)
		}
		slog.Info("backend respawned", "profile", name, "addr", d.sup.Status().Addr)
	}
}

func (d *Daemon) Stop(ctx context.Context, req *connect.Request[vibev1.StopRequest]) (*connect.Response[vibev1.StopResponse], error) {
	if !d.startMu.TryLock() {
		return nil, connect.NewError(connect.CodeAborted, errors.New("another start/stop is in progress"))
	}
	defer d.startMu.Unlock()

	name := strings.TrimSpace(req.Msg.GetProfile())
	switch {
	case name == "" || (d.active != nil && d.active.Name == name):
		// Empty or active-name targets the active profile (legacy
		// behavior). The active-name path lets callers be explicit
		// without having to know which mode the profile is in.
		if err := d.stopActive(ctx); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	case name == "all":
		// Best-effort stop everything. The first error gets surfaced
		// but every entry is attempted so a partial failure doesn't
		// leave half the stack up. Active goes last so a service stop
		// failure doesn't dangle the frontend.
		var firstErr error
		d.mu.Lock()
		names := make([]string, 0, len(d.services))
		for n := range d.services {
			names = append(names, n)
		}
		d.mu.Unlock()
		for _, n := range names {
			if err := d.stopService(ctx, n); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if err := d.stopActive(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
		if firstErr != nil {
			return nil, connect.NewError(connect.CodeInternal, firstErr)
		}
	default:
		// Anything else is a service name.
		if err := d.stopService(ctx, name); err != nil {
			if errors.Is(err, errServiceNotFound) {
				return nil, connect.NewError(connect.CodeNotFound, err)
			}
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	}
	return connect.NewResponse(&vibev1.StopResponse{Status: d.protoStatus()}), nil
}

// errServiceNotFound is returned by stopService when the named
// service isn't registered. Stop converts this to CodeNotFound so
// CLI callers can distinguish "wrong name" from "stop genuinely
// failed".
var errServiceNotFound = errors.New("service not found")

// stopService tears down a single service-mode profile. The entry
// is removed from d.services BEFORE stopping the supervisor so the
// per-service respawn watcher sees no registration and bails out
// cleanly (same pattern stopActive uses for d.active).
func (d *Daemon) stopService(ctx context.Context, name string) error {
	d.mu.Lock()
	svc, ok := d.services[name]
	if !ok {
		d.mu.Unlock()
		return fmt.Errorf("%w: %q", errServiceNotFound, name)
	}
	delete(d.services, name)
	d.mu.Unlock()

	slog.Info("stopping service", "profile", name)
	if err := svc.sup.Stop(ctx); err != nil {
		return fmt.Errorf("stop service %q: %w", name, err)
	}
	return nil
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
	// http_server backends bring their own model artefacts (baked into the
	// image or mounted via volumes — kokoro-fastapi pulls weights at first
	// run into the kokoro-models volume). Nothing for vibe to pull.
	if p.Backend.HTTPServer != nil {
		return stream.Send(&vibev1.PullProgress{
			Phase:   vibev1.PullProgress_PHASE_DONE,
			Message: "no model file to pull (http_server backends manage their own artefacts)",
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

	if err := stream.Send(&vibev1.PullProgress{Phase: vibev1.PullProgress_PHASE_RESOLVING}); err != nil {
		return err
	}

	// pullOne fetches a single repo file into dest, streaming download
	// progress over the same RPC stream. Returns the file's final size
	// on disk and whether the download actually transferred bytes (vs.
	// short-circuiting on a cached copy).
	pullOne := func(file, dest string) (int64, bool, error) {
		spec := hfdownload.Spec{
			Repo:     m.Huggingface.Repo,
			File:     file,
			Revision: m.Huggingface.Revision,
		}
		slog.Info("pulling model file", "profile", p.Name, "repo", spec.Repo, "file", spec.File, "dest", dest)
		var bytesFlowed bool
		progress := func(downloaded, total int64) {
			bytesFlowed = true
			_ = stream.Send(&vibev1.PullProgress{
				Phase:           vibev1.PullProgress_PHASE_DOWNLOADING,
				DownloadedBytes: downloaded,
				TotalBytes:      total,
			})
		}
		if err := hfdownload.Download(ctx, spec, dest, progress); err != nil {
			return 0, false, err
		}
		var size int64
		if info, statErr := os.Stat(dest); statErr == nil {
			size = info.Size()
		}
		return size, bytesFlowed, nil
	}

	modelSize, modelFlowed, err := pullOne(m.Huggingface.File, m.Path)
	if err != nil {
		slog.Error("download failed", "profile", p.Name, "err", err)
		return connect.NewError(connect.CodeInternal, fmt.Errorf("download model: %w", err))
	}

	var mmprojSize int64
	mmprojFlowed := false
	if m.Huggingface.MMProjFile != "" {
		size, flowed, err := pullOne(m.Huggingface.MMProjFile, m.MMProj)
		if err != nil {
			slog.Error("mmproj download failed", "profile", p.Name, "err", err)
			return connect.NewError(connect.CodeInternal, fmt.Errorf("download mmproj: %w", err))
		}
		mmprojSize = size
		mmprojFlowed = flowed
	}

	total := modelSize + mmprojSize
	msg := "complete"
	if !modelFlowed && !mmprojFlowed {
		msg = "already cached"
	}
	slog.Info("pull done", "profile", p.Name, "model", m.Path, "model_size", modelSize, "mmproj", m.MMProj, "mmproj_size", mmprojSize, "flowed", modelFlowed || mmprojFlowed)
	return stream.Send(&vibev1.PullProgress{
		Phase:           vibev1.PullProgress_PHASE_DONE,
		DownloadedBytes: total,
		TotalBytes:      total,
		Message:         msg,
	})
}

// ─── Internals ──────────────────────────────────────────────────────────────

func (d *Daemon) stopActive(ctx context.Context) error {
	d.mu.Lock()
	active := d.active
	fr := d.frontend
	// Clear d.active up-front so the watchBackendForRespawn goroutine
	// — which fires the moment the supervisor exits — sees no active
	// profile and bails out instead of respawning the backend right
	// out from under our Stop call. Without this, an EnsureActive
	// switch (Stop A, then Start B) races: the watcher restarts A on
	// the same port before Start B can grab the supervisor, and the
	// switch fails with "supervisor: already running".
	d.active = nil
	d.frontend = nil
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
		// Surface the llama_server `parallel` value so clients (notably vamp)
		// can cap their own foreach concurrency at the actual back-pressure
		// point. ComfyUI-backed profiles leave Parallel at its zero value:
		// the parallel concept is llama-server-specific.
		if ls := d.active.Backend.LlamaServer; ls != nil {
			s.Parallel = int32(ls.Parallel)
		}
	}
	if d.frontend != nil {
		s.FrontendEnv = d.frontend.Env
	}
	return s
}

// servicesStatus snapshots every running service into a list of
// vibev1.Status entries — one per service. Sorted by name so CLI
// output is stable across calls. Returned with the daemon lock NOT
// held; caller must not race against starts/stops. Status RPC is
// the only caller today.
func (d *Daemon) servicesStatus() []*vibev1.Status {
	d.mu.Lock()
	names := make([]string, 0, len(d.services))
	for n := range d.services {
		names = append(names, n)
	}
	d.mu.Unlock()
	sort.Strings(names)

	out := make([]*vibev1.Status, 0, len(names))
	for _, n := range names {
		d.mu.Lock()
		svc, ok := d.services[n]
		d.mu.Unlock()
		if !ok {
			continue // raced with a Stop; skip
		}
		st := svc.sup.Status()
		s := &vibev1.Status{
			Running:     true,
			Ready:       st.State == supervisor.StateReady,
			Profile:     svc.profile.Name,
			StartedAt:   timestamppb.New(svc.startTime),
			BackendAddr: st.Addr,
			// ProxyAddr intentionally empty: services don't go through
			// the daemon's reverse proxy. Callers reach them by their
			// published port (BackendAddr).
			Pid: int32(st.PID),
		}
		out = append(out, s)
	}
	return out
}

// firstNonEmpty returns the first non-"" string from its arguments.
// Used by the http_server start logging path so the same slog field can
// surface either the docker image or the bare-binary path without two
// separate log lines per mode.
func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// validProfileName matches the same shape `vibe profile init` enforces:
// alphanumerics, underscore, and hyphen only. Rejecting "/" / "." /
// non-printable characters here means the daemon's control plane can't
// be tricked into reading arbitrary YAML files via path traversal in
// a Start/Pull/Status RPC's profile-name field.
var validProfileName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func loadProfileByName(name string) (*profile.Profile, error) {
	if !validProfileName.MatchString(name) {
		return nil, fmt.Errorf("profile name %q is invalid (allowed: [a-zA-Z0-9_-]+)", name)
	}
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

// buildLaunchSpec dispatches a Profile to its backend-specific spec
// builder. Returns the supervisor.LaunchSpec ready to hand to
// Supervisor.Start, the host port the proxy / external callers should
// target, and a Connect-coded error on failure. Shared between
// active-mode (the main Start path) and service-mode (startService)
// so both paths construct identical specs from the same profile.
func (d *Daemon) buildLaunchSpec(p *profile.Profile) (supervisor.LaunchSpec, int, error) {
	var (
		spec supervisor.LaunchSpec
		port int
		err  error
	)
	switch {
	case p.Backend.LlamaServer != nil:
		// Honor the profile's pinned port if set — service-mode
		// profiles that pipelines reach by well-known address need
		// the port to survive daemon restarts. Otherwise fall back
		// to PickFreePort (the legacy behavior for active profiles).
		if p.Backend.LlamaServer.Port > 0 {
			port = p.Backend.LlamaServer.Port
		} else {
			port, err = supervisor.PickFreePort()
			if err != nil {
				return spec, 0, connect.NewError(connect.CodeInternal, fmt.Errorf("pick port: %w", err))
			}
		}
		llamaBin := d.cfg.LlamaBinary
		if p.Backend.LlamaServer.Binary != "" {
			llamaBin = p.Backend.LlamaServer.Binary
		}
		spec, err = profile.LlamaServerSpec(p, llamaBin, port)
		if err != nil {
			return spec, 0, connect.NewError(connect.CodeInternal, err)
		}
		slog.Info("starting profile (llama_server)",
			"profile", p.Name, "mode", p.ResolvedMode(),
			"alias", p.Backend.LlamaServer.Alias,
			"model_file", filepath.Base(p.Backend.LlamaServer.Path),
			"context", p.Backend.LlamaServer.Context, "port", port,
			"binary", llamaBin)
	case p.Backend.ComfyUI != nil:
		port = p.Backend.ComfyUI.Port
		if port == 0 {
			port, err = supervisor.PickFreePort()
			if err != nil {
				return spec, 0, connect.NewError(connect.CodeInternal, fmt.Errorf("pick port: %w", err))
			}
		}
		spec, err = profile.ComfyUISpec(p, port)
		if err != nil {
			return spec, 0, connect.NewError(connect.CodeInternal, err)
		}
		slog.Info("starting profile (comfyui)",
			"profile", p.Name, "mode", p.ResolvedMode(),
			"dir", p.Backend.ComfyUI.Dir, "port", port)
	case p.Backend.HTTPServer != nil:
		port = p.Backend.HTTPServer.Port
		spec, err = profile.HTTPServerSpec(p, p.Name)
		if err != nil {
			return spec, 0, connect.NewError(connect.CodeInternal, err)
		}
		mode := "docker"
		if p.Backend.HTTPServer.Image == "" {
			mode = "binary"
		}
		// Same stale-container cleanup the original inline switch did:
		// if a previous run left a `vibe-<name>` container behind, a
		// fresh `docker run --name` collision aborts the start.
		if mode == "docker" {
			rm := exec.Command("docker", "rm", "-f", "vibe-"+p.Name)
			rm.Stdout = nil
			rm.Stderr = nil
			_ = rm.Run()
		}
		slog.Info("starting profile (http_server)",
			"profile", p.Name, "mode", p.ResolvedMode(),
			"docker_or_binary", mode,
			"image_or_binary", firstNonEmpty(p.Backend.HTTPServer.Image, p.Backend.HTTPServer.Binary),
			"port", port)
	default:
		return spec, 0, connect.NewError(connect.CodeFailedPrecondition,
			errors.New("profile has no backend (set backend.llama_server, backend.comfyui, or backend.http_server)"))
	}
	return spec, port, nil
}

// startService runs a service-mode profile alongside the daemon's
// other services and (optionally) an active profile. Each service
// gets its own supervisor instance so a crash + auto-respawn in one
// service doesn't disturb the others.
//
// Service-mode profiles bypass:
//   - the d.active "already running" check (services don't compete for the active slot),
//   - the VRAM pre-flight (services are typically CPU-bound; if a service genuinely needs GPU, the caller bears responsibility for sizing),
//   - proxy backend wiring (callers reach services via the published port directly),
//   - frontend activation (a sidecar has no UI to launch).
//
// Returns a StartResponse populated from the per-service supervisor
// status; the StatusResponse's `services` list will pick up the new
// entry on subsequent Status calls.
func (d *Daemon) startService(ctx context.Context, p *profile.Profile) (*connect.Response[vibev1.StartResponse], error) {
	d.mu.Lock()
	if _, exists := d.services[p.Name]; exists {
		d.mu.Unlock()
		return nil, connect.NewError(connect.CodeAlreadyExists,
			fmt.Errorf("service %q is already running; stop it first with `vibe stop %s`", p.Name, p.Name))
	}
	d.mu.Unlock()

	spec, port, err := d.buildLaunchSpec(p)
	if err != nil {
		return nil, err
	}

	sup := supervisor.New()
	if err := sup.Start(ctx, spec, port); err != nil {
		slog.Error("service supervisor start failed", "profile", p.Name, "err", err)
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("start service: %w", err))
	}

	svc := &serviceInstance{
		profile:   p,
		sup:       sup,
		startTime: time.Now(),
		port:      port,
	}
	d.mu.Lock()
	d.services[p.Name] = svc
	d.mu.Unlock()

	// Service-specific auto-respawn watcher: same shape as the active
	// watcher, but parameterised on the service's own supervisor +
	// its slot in d.services. Crash in one service doesn't tear down
	// the others (each has its own supervisor; the active is on yet
	// a third).
	go d.watchServiceForRespawn(p.Name, sup, spec, port)

	// Return the SERVICE's status, not the active-profile status —
	// d.protoStatus() would surface the active slot (empty in this
	// case) and confuse the CLI. The Profile / BackendAddr fields
	// in StartResponse are how `vibe start` decides what to print.
	st := sup.Status()
	resp := &vibev1.StartResponse{Status: &vibev1.Status{
		Running:     true,
		Ready:       st.State == supervisor.StateReady,
		Profile:     p.Name,
		StartedAt:   timestamppb.New(svc.startTime),
		BackendAddr: st.Addr,
		// ProxyAddr left empty: services don't go through the proxy.
		Pid: int32(st.PID),
	}}
	slog.Info("service started", "profile", p.Name, "backend", st.Addr)
	return connect.NewResponse(resp), nil
}

// watchServiceForRespawn parallels watchBackendForRespawn but is
// scoped to a single service's supervisor and slot in d.services.
// Exits when the service is stopped by name (entry removed) or when
// the respawn budget is exhausted (entry cleared).
func (d *Daemon) watchServiceForRespawn(name string, sup *supervisor.Supervisor, spec supervisor.LaunchSpec, port int) {
	respawns := 0
	windowStart := time.Now()
	for {
		stopped := sup.Stopped()
		if stopped == nil {
			return
		}
		<-stopped

		d.mu.Lock()
		_, stillRegistered := d.services[name]
		d.mu.Unlock()
		if !stillRegistered {
			return // operator-initiated stop
		}
		if time.Since(windowStart) > respawnWindow {
			respawns = 0
			windowStart = time.Now()
		}
		if respawns >= maxBackendRespawns {
			slog.Error("service respawn budget exhausted; deregistering",
				"profile", name, "respawns", respawns, "window", respawnWindow)
			d.mu.Lock()
			delete(d.services, name)
			d.mu.Unlock()
			return
		}
		respawns++
		slog.Warn("service exited unexpectedly; auto-respawning",
			"profile", name, "attempt", respawns)
		startCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		if err := sup.Start(startCtx, spec, port); err != nil {
			slog.Error("service respawn failed", "profile", name, "err", err)
			d.mu.Lock()
			delete(d.services, name)
			d.mu.Unlock()
			cancel()
			return
		}
		cancel()
	}
}
