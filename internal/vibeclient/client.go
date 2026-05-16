// Package vibeclient is the typed Go SDK for vibe's Connect/RPC control
// plane. It is scoped to profile lifecycle (start/stop/status); inference is
// OpenAI-compatible HTTP at Status.ProxyAddr, which callers hit directly.
package vibeclient

import (
	"context"
	"net/http"
	"os"

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
func New(baseURL string) *Client {
	return NewWithHTTPClient(baseURL, http.DefaultClient)
}

// NewWithHTTPClient is for callers who need to control the transport — e.g.
// the vibe CLI, which dials a unix socket instead of TCP.
func NewWithHTTPClient(baseURL string, hc *http.Client) *Client {
	if baseURL == "" {
		baseURL = os.Getenv("VIBE_API")
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		rpc: vibev1connect.NewControlServiceClient(hc, baseURL),
	}
}

func (c *Client) Status(ctx context.Context) (*vibev1.Status, error) {
	resp, err := c.rpc.Status(ctx, connect.NewRequest(&vibev1.StatusRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Status, nil
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
// heuristic).
type StartOptions struct {
	NoVRAMCheck bool
}

// StartWithOptions is the same as Start but exposes optional flags. Existing
// callers can continue using Start; new callers that need bypass behavior
// should use this.
func (c *Client) StartWithOptions(ctx context.Context, profile string, opts StartOptions) (*StartResult, error) {
	resp, err := c.rpc.Start(ctx, connect.NewRequest(&vibev1.StartRequest{
		Profile:     profile,
		NoVramCheck: opts.NoVRAMCheck,
	}))
	if err != nil {
		return nil, err
	}
	return &StartResult{Status: resp.Msg.Status, Frontend: resp.Msg.Frontend}, nil
}

func (c *Client) Stop(ctx context.Context) (*vibev1.Status, error) {
	resp, err := c.rpc.Stop(ctx, connect.NewRequest(&vibev1.StopRequest{}))
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
	resp, err := c.rpc.Logs(ctx, connect.NewRequest(&vibev1.LogsRequest{}))
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

// EnsureActive returns immediately if `profile` is already the active, ready
// profile. Otherwise it stops any active profile and starts `profile`.
func (c *Client) EnsureActive(ctx context.Context, profile string) (*vibev1.Status, error) {
	s, err := c.Status(ctx)
	if err != nil {
		return nil, err
	}
	if s.Running && s.Ready && s.Profile == profile {
		return s, nil
	}
	if s.Running {
		if _, err := c.Stop(ctx); err != nil {
			return nil, err
		}
	}
	r, err := c.Start(ctx, profile)
	if err != nil {
		return nil, err
	}
	return r.Status, nil
}
