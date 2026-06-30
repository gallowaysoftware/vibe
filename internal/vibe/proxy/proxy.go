// Package proxy is the reverse proxy that frontends point at instead of
// llama-server directly. Its default backend can be swapped at runtime so
// the listening port stays consistent across profile changes.
//
// On top of the single default upstream (the active profile's backend),
// the proxy can carry per-model routes: a request whose JSON "model"
// field matches a registered alias is forwarded to that alias's upstream
// instead of the default. This lets concurrently-running service-mode
// backends share the one proxy port and be selected by model id — see
// Daemon.startService.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
)

// maxPeekBody caps how much of a request body we buffer to read the
// "model" field for routing. Completion requests are small; this bounds
// the work (and memory) the router does on a request that happens to
// stream a large body through a model-routed proxy.
const maxPeekBody = 2 << 20 // 2 MiB

type backend struct {
	url *url.URL
	rp  *httputil.ReverseProxy
}

func newBackend(u *url.URL) *backend {
	rp := httputil.NewSingleHostReverseProxy(u)
	// Flush immediately so SSE streaming from llama-server isn't buffered.
	rp.FlushInterval = -1
	return &backend{url: u, rp: rp}
}

type Proxy struct {
	addr string

	mu     sync.RWMutex
	def    *backend            // active-profile upstream; nil => 503
	routes map[string]*backend // model alias -> service upstream

	srvMu   sync.Mutex
	srv     *http.Server
	started bool
}

// New returns a Proxy that will listen on addr (e.g. "127.0.0.1:9000").
func New(addr string) *Proxy {
	return &Proxy{addr: addr, routes: map[string]*backend{}}
}

// SetBackend points the default (active-profile) upstream at u. Passing
// nil clears it so unrouted requests get 503. Per-model routes added via
// AddRoute are independent and unaffected.
func (p *Proxy) SetBackend(u *url.URL) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if u == nil {
		p.def = nil
		return
	}
	p.def = newBackend(u)
}

// AddRoute registers a model alias that should be routed to u instead of
// the default backend. Used to expose concurrently-running service-mode
// backends on the same proxy port, selected by the request's "model".
func (p *Proxy) AddRoute(model string, u *url.URL) {
	if model == "" || u == nil {
		return
	}
	p.mu.Lock()
	p.routes[model] = newBackend(u)
	p.mu.Unlock()
}

// RemoveRoute deregisters a model alias previously added with AddRoute.
func (p *Proxy) RemoveRoute(model string) {
	p.mu.Lock()
	delete(p.routes, model)
	p.mu.Unlock()
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.RLock()
	def := p.def
	nroutes := len(p.routes)
	p.mu.RUnlock()

	// Fast path: no model routes registered → behave exactly like the
	// original single-upstream proxy (no body inspection, zero overhead).
	if nroutes == 0 {
		if def == nil {
			http.Error(w, "vibe: no profile active", http.StatusServiceUnavailable)
			return
		}
		def.rp.ServeHTTP(w, r)
		return
	}

	// With routes present, aggregate /v1/models so clients see the active
	// model AND every routed service model. Best-effort: any failure
	// falls through to the default upstream's own /v1/models.
	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/v1/models") {
		if p.serveAggregatedModels(w, r, def) {
			return
		}
	}

	be := p.pick(r, def)
	if be == nil {
		http.Error(w, "vibe: no profile active", http.StatusServiceUnavailable)
		return
	}
	be.rp.ServeHTTP(w, r)
}

// pick selects the upstream for r. A POST carrying a JSON "model" that
// matches a registered route goes to that route; everything else
// (including the active model) goes to the default backend.
func (p *Proxy) pick(r *http.Request, def *backend) *backend {
	if r.Method != http.MethodPost || r.Body == nil {
		return def
	}
	model := peekModel(r)
	if model == "" {
		return def
	}
	p.mu.RLock()
	be := p.routes[model]
	p.mu.RUnlock()
	if be != nil {
		return be
	}
	return def
}

// peekModel reads the request body (up to maxPeekBody) to extract the
// top-level "model" field, then restores the body so the upstream still
// receives the full, unmodified payload. Returns "" when the body isn't
// JSON with a model field.
func peekModel(r *http.Request) string {
	orig := r.Body
	buf, err := io.ReadAll(io.LimitReader(orig, maxPeekBody))
	// Restore the body regardless of outcome: prepend what we read back
	// in front of any unread remainder so the upstream sees everything.
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(buf), orig))
	if err != nil {
		return ""
	}
	var probe struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(buf, &probe) != nil {
		return ""
	}
	return probe.Model
}

// serveAggregatedModels merges /v1/models from the default upstream and
// every routed upstream into one list, deduped by model id. Returns
// false (writing nothing) on any failure so the caller can fall back to
// proxying /v1/models to the default upstream unchanged.
func (p *Proxy) serveAggregatedModels(w http.ResponseWriter, r *http.Request, def *backend) bool {
	if def == nil {
		return false
	}
	p.mu.RLock()
	routes := make([]*backend, 0, len(p.routes))
	for _, be := range p.routes {
		routes = append(routes, be)
	}
	p.mu.RUnlock()

	type modelList struct {
		Object string            `json:"object"`
		Data   []json.RawMessage `json:"data"`
	}
	merged := modelList{Object: "list"}
	seen := map[string]bool{}
	add := func(be *backend) error {
		body, err := fetchModels(r.Context(), be.url)
		if err != nil {
			return err
		}
		var ml modelList
		if err := json.Unmarshal(body, &ml); err != nil {
			return err
		}
		for _, raw := range ml.Data {
			var idObj struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(raw, &idObj)
			if idObj.ID != "" {
				if seen[idObj.ID] {
					continue
				}
				seen[idObj.ID] = true
			}
			merged.Data = append(merged.Data, raw)
		}
		return nil
	}

	// The default upstream must succeed (it's the source of truth); a
	// routed service that's mid-restart is skipped rather than fatal.
	if err := add(def); err != nil {
		return false
	}
	for _, be := range routes {
		_ = add(be)
	}

	out, err := json.Marshal(merged)
	if err != nil {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
	return true
}

func fetchModels(ctx context.Context, u *url.URL) ([]byte, error) {
	endpoint := *u
	endpoint.Path = "/v1/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models: upstream status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// Start binds the listener and serves in the background.
func (p *Proxy) Start() error {
	p.srvMu.Lock()
	defer p.srvMu.Unlock()
	if p.started {
		return errors.New("proxy: already started")
	}
	ln, err := net.Listen("tcp", p.addr)
	if err != nil {
		return err
	}
	p.srv = &http.Server{Handler: p}
	p.started = true
	go func() {
		// A post-bind Serve failure (accept-loop error other than the
		// expected close) is otherwise invisible: the daemon's errCh only
		// covers the control-plane servers, so frontend requests would fail
		// at the socket with no daemon-side explanation.
		if err := p.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("proxy serve failed", "addr", p.addr, "err", err)
		}
	}()
	return nil
}

func (p *Proxy) Stop(ctx context.Context) error {
	p.srvMu.Lock()
	srv := p.srv
	p.srvMu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}

func (p *Proxy) Addr() string { return p.addr }
