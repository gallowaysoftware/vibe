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
	"strconv"
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

// newRewritingBackend is newBackend plus a response filter that puts the
// client-facing alias back into the "model" field. Completion responses
// echo the id the upstream knows itself by, which for mlx_lm.server is an
// absolute path — so without this every client (and every hum user
// downstream of llama-swap, which does not rewrite responses either) sees
// a filesystem path where it asked for a model name.
func newRewritingBackend(u *url.URL, rw *modelRewrite) *backend {
	be := newBackend(u)
	needle := []byte(`"model": "` + rw.upstream + `"`)
	compact := []byte(`"model":"` + rw.upstream + `"`)
	replNeedle := []byte(`"model": "` + rw.alias + `"`)
	replCompact := []byte(`"model":"` + rw.alias + `"`)
	be.rp.ModifyResponse = func(resp *http.Response) error {
		ct := resp.Header.Get("Content-Type")
		if !strings.Contains(ct, "json") && !strings.Contains(ct, "event-stream") {
			return nil
		}
		// Content-Length can't survive a substitution of a different
		// length; dropping it makes the response chunked, which every
		// OpenAI client already handles (streaming responses are chunked).
		resp.Header.Del("Content-Length")
		resp.ContentLength = -1
		resp.Body = &replacingReader{
			src: resp.Body,
			// Both spellings: encoding/json in the upstream may or may not
			// put a space after the colon, and matching the whole
			// `"model": "<id>"` token rather than the bare path means
			// model output that happens to mention the path is untouched.
			pairs: [][2][]byte{{needle, replNeedle}, {compact, replCompact}},
		}
		return nil
	}
	return be
}

// replacingReader substitutes fixed byte sequences in a stream without
// buffering the whole body — required because completion responses stream
// as SSE and must keep flowing token by token. It retains up to one
// needle's length between reads so a match spanning a chunk boundary is
// still found.
type replacingReader struct {
	src   io.ReadCloser
	pairs [][2][]byte
	buf   []byte
	eof   bool
}

func (r *replacingReader) Read(p []byte) (int, error) {
	for {
		if len(r.buf) > 0 {
			// Hold back a needle-length tail unless the source is done, so
			// a match split across reads isn't missed.
			emit := len(r.buf)
			if !r.eof {
				if keep := r.maxNeedle() - 1; emit > keep {
					emit -= keep
				} else {
					emit = 0
				}
			}
			if emit > 0 {
				n := copy(p, r.buf[:emit])
				r.buf = r.buf[n:]
				return n, nil
			}
		}
		if r.eof {
			return 0, io.EOF
		}
		chunk := make([]byte, 32<<10)
		n, err := r.src.Read(chunk)
		if n > 0 {
			r.buf = append(r.buf, chunk[:n]...)
			for _, pr := range r.pairs {
				r.buf = bytes.ReplaceAll(r.buf, pr[0], pr[1])
			}
		}
		if err != nil {
			r.eof = true
			if err != io.EOF {
				// Emit whatever survived, then surface the error next call.
				if len(r.buf) > 0 {
					n := copy(p, r.buf)
					r.buf = r.buf[n:]
					return n, nil
				}
				return 0, err
			}
		}
	}
}

func (r *replacingReader) maxNeedle() int {
	m := 0
	for _, pr := range r.pairs {
		if len(pr[0]) > m {
			m = len(pr[0])
		}
	}
	return m
}

func (r *replacingReader) Close() error { return r.src.Close() }

// modelRewrite translates between the client-facing model id and the id an
// upstream insists on. Needed by mlx_lm.server, which advertises the
// filesystem path it was launched with and treats any other `model` value
// as a model to download from HuggingFace — so a client sending a friendly
// alias gets an HF 401 instead of a completion.
type modelRewrite struct {
	alias    string // what clients send and see
	upstream string // what the upstream requires
}

type Proxy struct {
	addr string

	mu      sync.RWMutex
	def     *backend            // active-profile upstream; nil => 503
	routes  map[string]*backend // model alias -> service upstream
	rewrite *modelRewrite       // default upstream's id translation, if any

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
	// Order-independent with SetModelRewrite: whichever lands second
	// rebuilds the default backend with both pieces applied.
	if p.rewrite != nil {
		p.def = newRewritingBackend(u, p.rewrite)
		return
	}
	p.def = newBackend(u)
}

// SetModelRewrite makes the default upstream addressable by alias: inbound
// requests naming alias are rewritten to upstreamID before forwarding, and
// upstreamID is rewritten back to alias in /v1/models responses. Passing
// empty strings clears it. Set alongside SetBackend when the active profile
// is one of the backends that cannot be told its own model name.
func (p *Proxy) SetModelRewrite(alias, upstreamID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if alias == "" || upstreamID == "" || alias == upstreamID {
		p.rewrite = nil
		if p.def != nil {
			p.def = newBackend(p.def.url)
		}
		return
	}
	p.rewrite = &modelRewrite{alias: alias, upstream: upstreamID}
	if p.def != nil {
		p.def = newRewritingBackend(p.def.url, p.rewrite)
	}
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
	rw := p.rewrite
	p.mu.RUnlock()

	// Fast path: no model routes registered → behave exactly like the
	// original single-upstream proxy (no body inspection, zero overhead).
	// A rewrite still costs a body pass, but only when one is configured.
	if nroutes == 0 {
		if def == nil {
			http.Error(w, "vibe: no profile active", http.StatusServiceUnavailable)
			return
		}
		if rw != nil {
			if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/v1/models") {
				if p.serveRewrittenModels(w, r, def, rw) {
					return
				}
			}
			rewriteRequestModel(r, rw)
		}
		def.rp.ServeHTTP(w, r)
		return
	}

	// With routes present, aggregate /v1/models so clients see the active
	// model AND every routed service model. Best-effort: any failure
	// falls through to the default upstream's own /v1/models.
	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/v1/models") {
		if p.serveAggregatedModels(w, r, def, rw) {
			return
		}
	}

	be := p.pick(r, def)
	if be == nil {
		http.Error(w, "vibe: no profile active", http.StatusServiceUnavailable)
		return
	}
	// The rewrite belongs to the default upstream only — a routed service
	// backend advertises its own alias and must receive it unchanged.
	if rw != nil && be == def {
		rewriteRequestModel(r, rw)
	}
	be.rp.ServeHTTP(w, r)
}

// rewriteRequestModel replaces a top-level JSON "model" equal to rw.alias
// with rw.upstream, restoring the body either way. A body that hits
// maxPeekBody may be truncated, so it is passed through untouched rather
// than re-serialised from a partial read.
func rewriteRequestModel(r *http.Request, rw *modelRewrite) {
	if r.Method != http.MethodPost || r.Body == nil {
		return
	}
	orig := r.Body
	buf, err := io.ReadAll(io.LimitReader(orig, maxPeekBody))
	restore := func() {
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(buf), orig))
	}
	if err != nil || len(buf) == maxPeekBody {
		restore()
		return
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(buf, &doc) != nil {
		restore()
		return
	}
	var model string
	if json.Unmarshal(doc["model"], &model) != nil || model != rw.alias {
		restore()
		return
	}
	repl, err := json.Marshal(rw.upstream)
	if err != nil {
		restore()
		return
	}
	doc["model"] = repl
	out, err := json.Marshal(doc)
	if err != nil {
		restore()
		return
	}
	// ContentLength must track the new body: the reverse proxy forwards it
	// verbatim, and a stale value truncates the request upstream.
	r.Body = io.NopCloser(bytes.NewReader(out))
	r.ContentLength = int64(len(out))
	r.Header.Set("Content-Length", strconv.Itoa(len(out)))
}

// rewriteModelIDs swaps id == from for to in an OpenAI /v1/models payload,
// leaving every other field of each entry untouched. Returns nil when the
// body isn't the expected shape so callers can fall back to passthrough.
func rewriteModelIDs(body []byte, from, to string) []byte {
	var ml struct {
		Object string                       `json:"object"`
		Data   []map[string]json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &ml) != nil {
		return nil
	}
	repl, err := json.Marshal(to)
	if err != nil {
		return nil
	}
	for _, entry := range ml.Data {
		var id string
		if json.Unmarshal(entry["id"], &id) == nil && id == from {
			entry["id"] = repl
		}
	}
	out, err := json.Marshal(ml)
	if err != nil {
		return nil
	}
	return out
}

// serveRewrittenModels answers /v1/models from the single default upstream
// with the upstream's id swapped for the client-facing alias, so discovery
// returns the same name the client is expected to send back.
func (p *Proxy) serveRewrittenModels(w http.ResponseWriter, r *http.Request, def *backend, rw *modelRewrite) bool {
	body, err := fetchModels(r.Context(), def.url)
	if err != nil {
		return false
	}
	out := rewriteModelIDs(body, rw.upstream, rw.alias)
	if out == nil {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
	return true
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

// serveAggregatedModels merges /v1/models from the default upstream (if
// any) and every routed upstream into one list, deduped by model id.
// Returns false (writing nothing) on failure so the caller can fall back
// to the default-upstream path (which 503s when there is no default).
func (p *Proxy) serveAggregatedModels(w http.ResponseWriter, r *http.Request, def *backend, rw *modelRewrite) bool {
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
		// The rewrite describes the default upstream only, so the swap is
		// scoped to it: a routed service that happens to serve the same id
		// keeps its own name.
		if rw != nil && be == def {
			if swapped := rewriteModelIDs(body, rw.upstream, rw.alias); swapped != nil {
				body = swapped
			}
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

	// When a default upstream exists it must succeed (it's the source of
	// truth); a routed service that's mid-restart is skipped rather than
	// fatal. With no default (service-mode backends only, no active
	// profile), at least one routed upstream must answer — never serve an
	// empty list that would hide models a restarted service will re-offer.
	if def != nil {
		if err := add(def); err != nil {
			return false
		}
	}
	ok := def != nil
	for _, be := range routes {
		if add(be) == nil {
			ok = true
		}
	}
	if !ok {
		return false
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
