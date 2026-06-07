// Package proxy is the reverse proxy that frontends point at instead of
// llama-server directly. Its backend can be swapped at runtime so the
// listening port stays consistent across profile changes.
package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
)

type Proxy struct {
	addr    string
	backend atomic.Pointer[httputil.ReverseProxy]

	mu      sync.Mutex
	srv     *http.Server
	started bool
}

// New returns a Proxy that will listen on addr (e.g. "127.0.0.1:9000").
func New(addr string) *Proxy {
	return &Proxy{addr: addr}
}

// SetBackend points the proxy at u. Passing nil clears the backend so the
// proxy returns 503 to all requests.
func (p *Proxy) SetBackend(u *url.URL) {
	if u == nil {
		p.backend.Store(nil)
		return
	}
	rp := httputil.NewSingleHostReverseProxy(u)
	// Flush immediately so SSE streaming from llama-server isn't buffered.
	rp.FlushInterval = -1
	p.backend.Store(rp)
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rp := p.backend.Load()
	if rp == nil {
		http.Error(w, "vibe: no profile active", http.StatusServiceUnavailable)
		return
	}
	rp.ServeHTTP(w, r)
}

// Start binds the listener and serves in the background.
func (p *Proxy) Start() error {
	p.mu.Lock()
	defer p.mu.Unlock()
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
	p.mu.Lock()
	srv := p.srv
	p.mu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}

func (p *Proxy) Addr() string { return p.addr }
