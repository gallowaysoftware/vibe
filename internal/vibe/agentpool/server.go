package agentpool

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
	"gopkg.in/yaml.v3"
)

// maxBody bounds a buffered request. Unlike the proxy's 2 MiB routing peek,
// the pool needs the whole body: the affinity key reads the opening
// messages, and a dispatch to a down member is retried on another, which
// means sending the body twice. Agent prompts run to a few hundred KiB at
// the 131K-token contexts the members serve.
const maxBody = 32 << 20

// maxWeight bounds slots and weight so owners*slots*weight comparisons stay
// far from overflow; real members differ in speed by single-digit ratios.
const maxWeight = 1000

// Config is the agentpool configuration file.
type Config struct {
	// Listen is the address to serve on, e.g. ":9010".
	Listen string `yaml:"listen"`
	// Upstream is the front every member is reached through, e.g.
	// "http://front.example:9000". The pool picks the model id; the front
	// still decides which cell serves it.
	Upstream string `yaml:"upstream"`
	// AffinityTTL is how long an idle conversation keeps its member for its
	// own next request.
	AffinityTTL profile.Duration `yaml:"affinity_ttl"`
	// ConversationIdle is how long after its last request a conversation
	// stops owning its member's slot against new conversations. An agent is
	// between requests whenever a tool runs, so this should outlast a typical
	// test run.
	ConversationIdle profile.Duration `yaml:"conversation_idle"`
	// DownCooldown is how long a member is skipped after a failed dispatch.
	DownCooldown profile.Duration `yaml:"down_cooldown"`
	// OverflowPatience bounds how long a new conversation waits for a faster
	// busy member before taking a free slower one.
	OverflowPatience profile.Duration `yaml:"overflow_patience"`
	// QueueTimeout bounds how long a request waits for a slot.
	QueueTimeout profile.Duration `yaml:"queue_timeout"`
	Pools        []PoolConfig     `yaml:"pools"`
}

// PoolConfig is one virtual model id.
type PoolConfig struct {
	ID string `yaml:"id"`
	// Context is the context window /v1/models advertises. It should be the
	// smallest of the members', since a conversation may land on any.
	Context int            `yaml:"context"`
	Members []MemberConfig `yaml:"members"`
}

// MemberConfig is one member model and how many concurrent requests it takes.
type MemberConfig struct {
	Model string `yaml:"model"`
	Slots int    `yaml:"slots"`
	// Weight is the member's relative speed, 1 to maxWeight; unset means 1.
	// Once every slot is owned, conversations split in proportion to it.
	Weight int `yaml:"weight"`
}

// LoadConfig reads and validates a config file, filling defaults.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if c.Listen == "" {
		c.Listen = ":9010"
	}
	if c.AffinityTTL == 0 {
		c.AffinityTTL = profile.Duration(2 * time.Hour)
	}
	if c.ConversationIdle == 0 {
		c.ConversationIdle = profile.Duration(2 * time.Minute)
	}
	if c.OverflowPatience == 0 {
		c.OverflowPatience = profile.Duration(30 * time.Second)
	}
	if c.DownCooldown == 0 {
		c.DownCooldown = profile.Duration(30 * time.Second)
	}
	if c.QueueTimeout == 0 {
		c.QueueTimeout = profile.Duration(15 * time.Minute)
	}
	if c.Upstream == "" {
		return Config{}, fmt.Errorf("%s: upstream is required", path)
	}
	if len(c.Pools) == 0 {
		return Config{}, fmt.Errorf("%s: no pools", path)
	}
	seen := map[string]bool{}
	for pi := range c.Pools {
		p := &c.Pools[pi]
		if p.ID == "" || seen[p.ID] {
			return Config{}, fmt.Errorf("%s: pool id %q is empty or repeated", path, p.ID)
		}
		seen[p.ID] = true
		if len(p.Members) == 0 {
			return Config{}, fmt.Errorf("%s: pool %q has no members", path, p.ID)
		}
		for mi := range p.Members {
			m := &p.Members[mi]
			if m.Weight == 0 {
				m.Weight = 1
			}
			if m.Model == "" || m.Slots < 1 || m.Slots > maxWeight || m.Weight < 1 || m.Weight > maxWeight {
				return Config{}, fmt.Errorf("%s: pool %q: member needs a model, and slots and weight in 1..%d", path, p.ID, maxWeight)
			}
		}
	}
	return c, nil
}

// Server is the HTTP face of a set of pools.
type Server struct {
	draining     context.Context
	drain        context.CancelFunc
	upstream     string
	pools        map[string]*Pool
	order        []*Pool
	queueTimeout time.Duration
	client       *http.Client
	log          *slog.Logger
}

// NewServer builds a server from a validated config. transport may be nil.
func NewServer(c Config, transport http.RoundTripper, log *slog.Logger) *Server {
	if transport == nil {
		transport = &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     90 * time.Second,
			// Transparent gzip would put a decompressor between the member's
			// SSE frames and the client; pass the bytes as the front sends them.
			DisableCompression: true,
		}
	}
	if log == nil {
		log = slog.Default()
	}
	draining, drain := context.WithCancel(context.Background())
	s := &Server{
		draining:     draining,
		drain:        drain,
		upstream:     strings.TrimRight(c.Upstream, "/"),
		pools:        map[string]*Pool{},
		queueTimeout: time.Duration(c.QueueTimeout),
		// No client timeout: a completion streams for as long as the model
		// generates, and the caller's context already bounds it.
		client: &http.Client{Transport: transport},
		log:    log,
	}
	for _, pc := range c.Pools {
		specs := make([]MemberSpec, 0, len(pc.Members))
		for _, m := range pc.Members {
			specs = append(specs, MemberSpec(m))
		}
		p := NewPool(pc.ID, pc.Context, specs, Timing{
			TTL:      time.Duration(c.AffinityTTL),
			Idle:     time.Duration(c.ConversationIdle),
			Cooldown: time.Duration(c.DownCooldown),
			Patience: time.Duration(c.OverflowPatience),
		})
		s.pools[pc.ID] = p
		s.order = append(s.order, p)
	}
	return s
}

// Drain fails every queued request so a shutdown can finish. Requests
// already dispatched keep streaming.
func (s *Server) Drain() { s.drain() }

// ServeHTTP routes completions to pools and answers discovery locally.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		s.serveModels(w)
	case r.Method == http.MethodGet && r.URL.Path == "/status":
		s.serveStatus(w)
	case r.Method == http.MethodGet && r.URL.Path == "/healthz":
		w.WriteHeader(http.StatusOK)
	// Exact paths only: the request path is forwarded verbatim, and a front
	// 404 for a path it does not serve would otherwise read as a member
	// being down and take the whole pool out for a cooldown.
	case r.Method == http.MethodPost && (r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/v1/completions"):
		s.serveCompletion(w, r)
	default:
		writeError(w, http.StatusNotFound, "not_found", "agentpool serves only pool completions, /v1/models, /status and /healthz")
	}
}

type completionPeek struct {
	Model    string            `json:"model"`
	Messages []json.RawMessage `json:"messages"`
}

// requestBody is a completion parsed once: its top-level fields as raw bytes,
// so each dispatch attempt only swaps the model value.
type requestBody map[string]json.RawMessage

// withModel encodes the body with the member's model id. HTML escaping is
// off so prompts full of code are not rewritten to \u003c and friends.
func (b requestBody) withModel(model string) ([]byte, error) {
	m, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	b["model"] = m
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(map[string]json.RawMessage(b)); err != nil {
		return nil, err
	}
	return bytes.TrimRight(out.Bytes(), "\n"), nil
}

func (s *Server) serveCompletion(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "reading body: "+err.Error())
		return
	}
	if len(body) > maxBody {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body over 32 MiB")
		return
	}
	var peek completionPeek
	var fields requestBody
	if err := json.Unmarshal(body, &peek); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "body is not JSON: "+err.Error())
		return
	}
	if err := json.Unmarshal(body, &fields); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "body is not a JSON object: "+err.Error())
		return
	}
	pool, ok := s.pools[peek.Model]
	if !ok {
		writeError(w, http.StatusNotFound, "model_not_found", fmt.Sprintf("model %q is not a pool here", peek.Model))
		return
	}
	key := affinityKey(peek.Messages)

	ctx, cancel := context.WithTimeout(r.Context(), s.queueTimeout)
	defer cancel()
	// A shutdown fails queued requests at once: http.Server.Shutdown does not
	// cancel request contexts, so a waiter would hold the drain open until
	// its queue timeout. Requests already streaming are left to finish.
	stop := context.AfterFunc(s.draining, cancel)
	defer stop()
	// Each member gets at most one attempt per request; a retry exists to
	// step past a member that went away, not to hammer one that is failing.
	for attempt := 0; attempt < len(pool.members); attempt++ {
		lease, err := pool.Acquire(ctx, key)
		if err != nil {
			s.writeAcquireError(w, r, pool, err)
			return
		}
		out, err := fields.withModel(lease.Model())
		if err != nil {
			lease.Release()
			writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		resp, err := s.dispatch(r, out)
		if err != nil || memberUnavailable(resp.StatusCode) {
			status := "dial error"
			if err == nil {
				status = resp.Status
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
				resp.Body.Close()
			} else if r.Context().Err() != nil {
				lease.Release()
				return
			}
			s.log.Warn("agentpool: member unavailable, marking down", "pool", pool.ID, "member", lease.Model(), "status", status, "err", err)
			lease.ReleaseDown()
			continue
		}
		s.stream(w, resp, lease)
		return
	}
	w.Header().Set("Retry-After", "30")
	writeError(w, http.StatusServiceUnavailable, "pool_unavailable", fmt.Sprintf("pool %q: no member accepted the request", pool.ID))
}

func (s *Server) dispatch(r *http.Request, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, s.upstream+r.URL.Path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for _, h := range []string{"Authorization", "Content-Type", "Accept", "User-Agent"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	req.ContentLength = int64(len(body))
	return s.client.Do(req)
}

// stream copies the upstream response through, flushing every read so an
// SSE completion reaches the client as the model produces it.
func (s *Server) stream(w http.ResponseWriter, resp *http.Response, lease *Lease) {
	defer lease.Release()
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		if k == "Content-Length" || k == "Connection" || k == "Transfer-Encoding" {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-Agent-Pool-Member", lease.Model())
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	// Headers go out now: a member still prefilling a long prompt may not
	// produce a byte for a minute, and the client should know it was accepted.
	if flusher != nil {
		flusher.Flush()
	}
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func (s *Server) writeAcquireError(w http.ResponseWriter, r *http.Request, pool *Pool, err error) {
	switch {
	case errors.Is(err, ErrNoMembers):
		w.Header().Set("Retry-After", "30")
		writeError(w, http.StatusServiceUnavailable, "pool_unavailable", fmt.Sprintf("pool %q: every member is down", pool.ID))
	case r.Context().Err() != nil:
		// The client went away while queued; nobody is left to answer.
	case s.draining.Err() != nil:
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "pool_draining", "agentpool is shutting down")
	default:
		w.Header().Set("Retry-After", "30")
		writeError(w, http.StatusServiceUnavailable, "pool_busy", fmt.Sprintf("pool %q: no slot freed within %s", pool.ID, s.queueTimeout))
	}
}

// memberUnavailable reports whether a front response means the member cannot
// serve right now, as opposed to the request being bad. The front answers a
// model on a down or withdrawn cell with 404 ("no router for requested
// model") and an unreachable one with 502/503.
func memberUnavailable(code int) bool {
	return code == http.StatusNotFound || code == http.StatusBadGateway || code == http.StatusServiceUnavailable
}

// affinityKey identifies a conversation by its opening: the first system
// message and the first user message. Both are fixed for the life of an
// agent's conversation while everything after them grows, so every request
// in it hashes the same. Messages are re-marshalled so field order and
// whitespace differences in the client's JSON do not split one conversation.
func affinityKey(msgs []json.RawMessage) string {
	var system, user json.RawMessage
	for _, raw := range msgs {
		var m struct {
			Role string `json:"role"`
		}
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		if m.Role == "system" && system == nil {
			system = raw
		}
		if m.Role == "user" && user == nil {
			user = raw
		}
		if system != nil && user != nil {
			break
		}
	}
	if user == nil {
		return ""
	}
	h := sha256.New()
	for _, raw := range []json.RawMessage{system, user} {
		var v any
		if raw != nil && json.Unmarshal(raw, &v) == nil {
			canon, _ := json.Marshal(v)
			h.Write(canon)
		}
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (s *Server) serveModels(w http.ResponseWriter) {
	type entry struct {
		ID            string `json:"id"`
		Object        string `json:"object"`
		OwnedBy       string `json:"owned_by"`
		ContextLength int    `json:"context_length,omitempty"`
	}
	out := struct {
		Object string  `json:"object"`
		Data   []entry `json:"data"`
	}{Object: "list", Data: []entry{}}
	for _, p := range s.order {
		out.Data = append(out.Data, entry{ID: p.ID, Object: "model", OwnedBy: "agentpool", ContextLength: p.Context})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) serveStatus(w http.ResponseWriter) {
	out := make([]Status, 0, len(s.order))
	for _, p := range s.order {
		out = append(out, p.Status())
	}
	writeJSON(w, http.StatusOK, out)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, typ, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": code}})
}
