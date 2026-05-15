// Package daemon is the long-running supervisor process. It owns the
// llama-server supervisor and the reverse proxy, and exposes a JSON-over-HTTP
// control plane on a unix socket for the CLI.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	"gopkg.in/yaml.v3"

	"github.com/gallowaysoftware/vibe/internal/frontend"
	"github.com/gallowaysoftware/vibe/internal/ipc"
	"github.com/gallowaysoftware/vibe/internal/paths"
	"github.com/gallowaysoftware/vibe/internal/profile"
	"github.com/gallowaysoftware/vibe/internal/proxy"
	"github.com/gallowaysoftware/vibe/internal/supervisor"
)

const defaultProxyPort = 9000

type Config struct {
	ProxyPort   int    `yaml:"proxy_port,omitempty"`
	LlamaBinary string `yaml:"llama_binary,omitempty"` // empty → "llama-server" from $PATH
}

// LoadConfig reads the global vibe config; missing file is not an error.
func LoadConfig() (Config, error) {
	c := Config{ProxyPort: defaultProxyPort}
	data, err := os.ReadFile(paths.ConfigFile())
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := yaml.Unmarshal(data, &c); err != nil {
		return c, err
	}
	if c.ProxyPort == 0 {
		c.ProxyPort = defaultProxyPort
	}
	return c, nil
}

type Daemon struct {
	cfg Config
	sup *supervisor.Supervisor
	prx *proxy.Proxy

	// startMu serializes start/stop operations against each other.
	startMu sync.Mutex

	mu        sync.Mutex
	active    *profile.Profile
	startTime time.Time
	frontend  *frontend.Result

	shutdown     chan struct{}
	shutdownOnce sync.Once
}

func New(cfg Config) *Daemon {
	return &Daemon{
		cfg:      cfg,
		sup:      supervisor.New(cfg.LlamaBinary),
		prx:      proxy.New(fmt.Sprintf("127.0.0.1:%d", cfg.ProxyPort)),
		shutdown: make(chan struct{}),
	}
}

// Run brings up the proxy and the IPC server, blocking until ctx is canceled.
// Cleans up the socket and PID file on exit.
func (d *Daemon) Run(ctx context.Context) error {
	if err := paths.EnsureDirs(); err != nil {
		return fmt.Errorf("ensure dirs: %w", err)
	}
	if err := ensureSingleInstance(); err != nil {
		return err
	}

	sockPath := paths.Socket()
	_ = os.Remove(sockPath) // stale socket from a hard kill
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("listen unix %s: %w", sockPath, err)
	}
	defer os.Remove(sockPath)
	if err := os.Chmod(sockPath, 0o600); err != nil {
		return err
	}

	if err := writePIDFile(); err != nil {
		return err
	}
	defer os.Remove(paths.PIDFile())

	if err := d.prx.Start(); err != nil {
		return fmt.Errorf("start proxy: %w", err)
	}
	defer d.prx.Stop(context.Background())

	srv := &http.Server{Handler: d.handler()}
	errCh := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
	case <-d.shutdown:
	case err := <-errCh:
		return err
	}
	shCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Stop the supervisor unconditionally; if a start is mid-flight the
	// child process exists and SIGINT will unblock waitReady on the other side.
	_ = d.sup.Stop(shCtx)
	d.prx.SetBackend(nil)
	_ = srv.Shutdown(shCtx)
	return nil
}

func (d *Daemon) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /start", d.handleStart)
	mux.HandleFunc("POST /stop", d.handleStop)
	mux.HandleFunc("POST /shutdown", d.handleShutdown)
	mux.HandleFunc("GET /status", d.handleStatus)
	mux.HandleFunc("GET /list", d.handleList)
	mux.HandleFunc("GET /logs", d.handleLogs)
	return mux
}

func (d *Daemon) handleShutdown(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "shutting down"})
	// Trigger after the response flushes so the client doesn't see EOF.
	go func() {
		time.Sleep(50 * time.Millisecond)
		d.shutdownOnce.Do(func() { close(d.shutdown) })
	}()
}

func (d *Daemon) handleStart(w http.ResponseWriter, r *http.Request) {
	if !d.startMu.TryLock() {
		writeErr(w, http.StatusConflict, errors.New("another start/stop is in progress"))
		return
	}
	defer d.startMu.Unlock()

	var req ipc.StartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(req.Profile) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("profile name required"))
		return
	}

	d.mu.Lock()
	if d.active != nil {
		name := d.active.Name
		d.mu.Unlock()
		writeErr(w, http.StatusConflict, fmt.Errorf("profile %q is already running; stop first", name))
		return
	}
	d.mu.Unlock()

	p, err := loadProfileByName(req.Profile)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	// Long timeout for model loading on first start.
	startCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := d.sup.Start(startCtx, p); err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("start llama-server: %w", err))
		return
	}

	st := d.sup.Status()
	backendURL, err := url.Parse(st.Addr)
	if err != nil {
		_ = d.sup.Stop(context.Background())
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("parse backend url: %w", err))
		return
	}
	d.prx.SetBackend(backendURL)

	vibeAPI := fmt.Sprintf("http://127.0.0.1:%d/v1", d.cfg.ProxyPort)
	fr, err := frontend.Activate(p, profile.ExpandContext{
		VibeAPI:      vibeAPI,
		ModelAlias:   p.Model.Alias,
		ModelContext: p.Model.Context,
	})
	if err != nil {
		_ = d.sup.Stop(context.Background())
		d.prx.SetBackend(nil)
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("activate frontend: %w", err))
		return
	}

	d.mu.Lock()
	d.active = p
	d.startTime = time.Now()
	d.frontend = fr
	d.mu.Unlock()

	writeJSON(w, http.StatusOK, ipc.StartResponse{
		Status: d.status(),
		Frontend: &ipc.FrontendInfo{
			App:             p.Frontend.App,
			WroteFile:       fr.WroteFile,
			RestartRequired: fr.RestartRequired,
		},
	})
}

func (d *Daemon) handleStop(w http.ResponseWriter, r *http.Request) {
	if !d.startMu.TryLock() {
		writeErr(w, http.StatusConflict, errors.New("another start/stop is in progress"))
		return
	}
	defer d.startMu.Unlock()
	if err := d.stopActive(r.Context()); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, ipc.StopResponse{Status: d.status()})
}

func (d *Daemon) stopActive(ctx context.Context) error {
	d.mu.Lock()
	active := d.active
	d.mu.Unlock()
	if active == nil {
		return nil
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

func (d *Daemon) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, d.status())
}

func (d *Daemon) status() ipc.Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.sup.Status()
	s := ipc.Status{
		Running:     d.active != nil,
		Ready:       st.State == supervisor.StateReady,
		BackendAddr: st.Addr,
		ProxyAddr:   fmt.Sprintf("http://127.0.0.1:%d", d.cfg.ProxyPort),
		PID:         st.PID,
	}
	if d.active != nil {
		s.Profile = d.active.Name
		s.StartedAt = d.startTime
	}
	return s
}

func (d *Daemon) handleList(w http.ResponseWriter, r *http.Request) {
	dir := paths.ProfilesDir()
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	var out []ipc.ProfileSummary
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		p, err := profile.Load(path)
		if err != nil {
			continue
		}
		out = append(out, ipc.ProfileSummary{Name: p.Name, Description: p.Description, Path: path})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, ipc.ListResponse{Profiles: out})
}

func (d *Daemon) handleLogs(w http.ResponseWriter, r *http.Request) {
	// Phase 1: dump the current log buffer; follow mode lands later.
	lines := d.sup.Logs()
	w.Header().Set("Content-Type", "text/plain")
	for _, l := range lines {
		w.Write([]byte(l))
		w.Write([]byte("\n"))
	}
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

// ensureSingleInstance fails if another daemon is already running. A stale
// PID file (process gone) is silently cleaned up.
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

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, ipc.ErrorResponse{Error: err.Error()})
}
