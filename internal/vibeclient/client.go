// Package vibeclient is the typed Go SDK for vibe's Connect/RPC control
// plane. It is scoped to profile lifecycle (start/stop/status); inference is
// OpenAI-compatible HTTP at Status.ProxyAddr, which callers hit directly.
package vibeclient

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"

	vibev1 "github.com/gallowaysoftware/vibe/proto/vibe/v1"
	"github.com/gallowaysoftware/vibe/proto/vibe/v1/vibev1connect"
)

const DefaultBaseURL = "http://127.0.0.1:9001"

type Client struct {
	rpc vibev1connect.ControlServiceClient
}

// New returns a Client targeting vibe's control plane. baseURL is the first
// non-empty of: argument, $VIBE_API, DefaultBaseURL.
//
// The bearer token (required for TCP control-plane RPCs; ignored on the
// unix-socket path) is discovered automatically — see ResolveToken. Use
// NewWithToken if you need to supply it explicitly.
func New(baseURL string) *Client {
	return NewWithToken(baseURL, ResolveToken())
}

// NewWithToken is like New but takes an explicit bearer token. Pass "" to
// skip the Authorization header entirely (appropriate for unix-socket
// clients).
func NewWithToken(baseURL, token string) *Client {
	return NewWithHTTPClient(baseURL, http.DefaultClient, token)
}

// NewWithHTTPClient is for callers who need to control the transport — e.g.
// the vibe CLI, which dials a unix socket instead of TCP. The token is
// passed as a separate argument because it's a Connect-level concern (an
// interceptor on the rpc client), independent of how the bytes get to the
// daemon. Unix-socket callers should pass "" for token.
func NewWithHTTPClient(baseURL string, hc *http.Client, token string) *Client {
	if baseURL == "" {
		baseURL = os.Getenv("VIBE_API")
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	var opts []connect.ClientOption
	if token != "" {
		opts = append(opts, connect.WithInterceptors(bearerAuthInterceptor(token)))
	}
	return &Client{
		rpc: vibev1connect.NewControlServiceClient(hc, baseURL, opts...),
	}
}

// ResolveToken returns the bearer token to use for TCP requests. Priority:
//  1. $VIBE_TOKEN (the documented way to inject one on a remote laptop).
//  2. $XDG_STATE_HOME/vibe/token (works without setup on the same machine).
//  3. "" — no token available; callers will get 401 from a remote daemon.
//
// Returning "" intentionally lets unix-socket callers work seamlessly: they
// pass the token through but the daemon doesn't check it on the unix
// listener.
func ResolveToken() string {
	if v := strings.TrimSpace(os.Getenv("VIBE_TOKEN")); v != "" {
		return v
	}
	data, err := os.ReadFile(tokenFilePath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// tokenFilePath duplicates a tiny piece of paths.TokenFile here so the
// vibeclient package stays free of internal/vibe/paths (the SDK is consumed
// by vamp and could one day live in a separate repo).
func tokenFilePath() string {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		stateHome = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(stateHome, "vibe", "token")
}

// bearerAuthInterceptor injects `Authorization: Bearer <token>` on every
// outgoing RPC (unary + streaming).
func bearerAuthInterceptor(token string) connect.Interceptor {
	header := "Bearer " + token
	return &authInterceptor{header: header}
}

type authInterceptor struct {
	header string
}

func (a *authInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("Authorization", a.header)
		return next(ctx, req)
	}
}

func (a *authInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("Authorization", a.header)
		return conn
	}
}

func (a *authInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	// Client-only interceptor; the handler side does nothing.
	return next
}

func (c *Client) Status(ctx context.Context) (*vibev1.Status, error) {
	resp, err := c.rpc.Status(ctx, connect.NewRequest(&vibev1.StatusRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Status, nil
}

// StatusFull returns the active-profile Status plus every running
// service. Used by `vibe ps` to render the whole picture; callers
// that only care about the active profile can keep using Status().
func (c *Client) StatusFull(ctx context.Context) (active *vibev1.Status, services []*vibev1.Status, err error) {
	resp, err := c.rpc.Status(ctx, connect.NewRequest(&vibev1.StatusRequest{}))
	if err != nil {
		return nil, nil, err
	}
	return resp.Msg.Status, resp.Msg.Services, nil
}

func (c *Client) ListProfiles(ctx context.Context) ([]*vibev1.Profile, error) {
	resp, err := c.rpc.ListProfiles(ctx, connect.NewRequest(&vibev1.ListProfilesRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Profiles, nil
}

type StartResult struct {
	Status   *vibev1.Status
	Frontend *vibev1.FrontendInfo
}

func (c *Client) Start(ctx context.Context, profile string) (*StartResult, error) {
	return c.StartWithOptions(ctx, profile, StartOptions{})
}

// StartOptions tunes a Start call. NoVRAMCheck bypasses the daemon's
// pre-flight VRAM check (for users who know their setup better than the
// heuristic). Foreground asks the daemon to render the frontend's config
// + env but not spawn the binary; used by `vibe run` which exec's the
// frontend in its own terminal.
type StartOptions struct {
	NoVRAMCheck bool
	Foreground  bool
}

// StartWithOptions is the same as Start but exposes optional flags. Existing
// callers can continue using Start; new callers that need bypass behavior
// should use this.
func (c *Client) StartWithOptions(ctx context.Context, profile string, opts StartOptions) (*StartResult, error) {
	resp, err := c.rpc.Start(ctx, connect.NewRequest(&vibev1.StartRequest{
		Profile:     profile,
		NoVramCheck: opts.NoVRAMCheck,
		Foreground:  opts.Foreground,
	}))
	if err != nil {
		return nil, err
	}
	return &StartResult{Status: resp.Msg.Status, Frontend: resp.Msg.Frontend}, nil
}

// StartBackend activates a named backend definition (backends/<name>.yaml) as
// the active model server with no frontend. The active identity is the backend
// name. Used by vamp capability resolution so a pipeline targets a backend
// rather than a frontend-bearing profile.
func (c *Client) StartBackend(ctx context.Context, backend string) (*StartResult, error) {
	resp, err := c.rpc.Start(ctx, connect.NewRequest(&vibev1.StartRequest{Backend: backend}))
	if err != nil {
		return nil, err
	}
	return &StartResult{Status: resp.Msg.Status, Frontend: resp.Msg.Frontend}, nil
}

func (c *Client) Stop(ctx context.Context) (*vibev1.Status, error) {
	return c.StopByName(ctx, "")
}

// StopByName stops a specific profile or service. Empty name targets
// the active profile (legacy Stop behavior). The special value "all"
// stops the active profile and every running service. Anything else
// is treated as a service name.
func (c *Client) StopByName(ctx context.Context, name string) (*vibev1.Status, error) {
	resp, err := c.rpc.Stop(ctx, connect.NewRequest(&vibev1.StopRequest{Profile: name}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Status, nil
}

func (c *Client) Shutdown(ctx context.Context) error {
	_, err := c.rpc.Shutdown(ctx, connect.NewRequest(&vibev1.ShutdownRequest{}))
	return err
}

func (c *Client) Logs(ctx context.Context) ([]string, error) {
	return c.LogsForProfile(ctx, "")
}

// LogsForProfile returns logs from a specific profile's supervisor.
// Empty or "active" → the active profile (legacy behaviour); any
// other name → a running service profile.
func (c *Client) LogsForProfile(ctx context.Context, name string) ([]string, error) {
	resp, err := c.rpc.Logs(ctx, connect.NewRequest(&vibev1.LogsRequest{Profile: name}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Lines, nil
}

// PullStream is a forward-iterator over PullProgress messages.
type PullStream struct {
	s *connect.ServerStreamForClient[vibev1.PullProgress]
}

func (s *PullStream) Receive() bool             { return s.s.Receive() }
func (s *PullStream) Msg() *vibev1.PullProgress { return s.s.Msg() }
func (s *PullStream) Err() error                { return s.s.Err() }
func (s *PullStream) Close() error              { return s.s.Close() }

// Pull starts a server-streaming Pull RPC. The caller iterates with Receive()
// and reads each message via Msg().
func (c *Client) Pull(ctx context.Context, profile string) (*PullStream, error) {
	stream, err := c.rpc.Pull(ctx, connect.NewRequest(&vibev1.PullRequest{Profile: profile}))
	if err != nil {
		return nil, err
	}
	return &PullStream{s: stream}, nil
}

// IsVRAMRejection reports whether err is the daemon's pre-flight VRAM
// rejection (i.e. "this profile needs more VRAM than is currently free").
//
// We identify it by the Connect status code AND a substring match on the
// daemon's well-known message body. The dual check keeps unrelated
// FailedPrecondition errors (e.g. "no profile is active") from being
// misclassified as VRAM rejections, which would otherwise cause vamp's
// candidate-fallback loop to swallow legitimate errors. We control both
// ends of the wire so a string match is acceptable; if the daemon message
// changes, IsVRAMRejection moves in lockstep.
func IsVRAMRejection(err error) bool {
	if err == nil {
		return false
	}
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return false
	}
	if ce.Code() != connect.CodeFailedPrecondition {
		return false
	}
	return strings.Contains(ce.Message(), "free VRAM")
}

// IsNotFound reports whether err is a Connect NotFound — e.g. EnsureBackendActive
// against a name that has no backends/<name>.yaml. vamp uses it to fall back to
// treating a capability target as a profile name (backward compat for
// capabilities.yaml entries that still name profiles rather than backends).
func IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return false
	}
	return ce.Code() == connect.CodeNotFound
}

// EnsureActive ensures `profile` is up and ready, regardless of whether
// it's defined as active-mode or service-mode in its YAML.
//
// Resolution order:
//
//  1. If `profile` is already the active, ready profile → return it.
//  2. If `profile` is already running as a service (e.g. tts_kokoro,
//     searxng, embed_bge_large) → return that service's status. This
//     is what makes vamp's capability resolver work transparently
//     against service-mode sidecars: a stage can declare
//     Capability("tts") and the runner will activate "tts_kokoro"
//     without caring whether it's a single-slot active LLM or a
//     long-running sidecar.
//  3. Otherwise → stop the current active (if any) and start
//     `profile` as the new active. (The daemon rejects starting a
//     service-mode profile as active with a clear error, which is
//     what we want for misconfigured pipelines.)
//
// Before this routing existed, every episodic-pipeline episode failed because
// cast_aria's Capability("tts") resolved to "tts_kokoro" and step 3
// would unconditionally Stop+Start, only to have the daemon reject
// the Start with already_exists.
func (c *Client) EnsureActive(ctx context.Context, profile string) (*vibev1.Status, error) {
	return c.ensureActive(ctx, profile, func(ctx context.Context) (*StartResult, error) {
		return c.Start(ctx, profile)
	})
}

// EnsureBackendActive is EnsureActive's backend twin: it ensures the named
// backend is the active model server (no frontend), swapping out whatever is
// currently active if needed. The active identity is the backend name, so a
// backend already active — or already running as a service-mode sidecar — is
// reused without a needless stop/start.
func (c *Client) EnsureBackendActive(ctx context.Context, backend string) (*vibev1.Status, error) {
	return c.ensureActive(ctx, backend, func(ctx context.Context) (*StartResult, error) {
		return c.StartBackend(ctx, backend)
	})
}

// ensureActive holds the shared resolution: reuse the active or a service if
// its name matches, otherwise stop the current active and start anew via the
// supplied start func. `name` is matched against Status.Profile, which is the
// profile name for a profile activation and the backend name for a backend one.
func (c *Client) ensureActive(ctx context.Context, name string, start func(context.Context) (*StartResult, error)) (*vibev1.Status, error) {
	resp, err := c.rpc.Status(ctx, connect.NewRequest(&vibev1.StatusRequest{}))
	if err != nil {
		return nil, err
	}
	active := resp.Msg.Status
	if active != nil && active.Running && active.Ready && active.Profile == name {
		return active, nil
	}
	for _, svc := range resp.Msg.Services {
		if svc != nil && svc.Profile == name && svc.Ready {
			return svc, nil
		}
	}
	if active != nil && active.Running {
		if _, err := c.Stop(ctx); err != nil {
			return nil, err
		}
	}
	r, err := start(ctx)
	if err != nil {
		return nil, err
	}
	return r.Status, nil
}
