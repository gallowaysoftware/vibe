package frontend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
)

// composeDriver brings up (and tears down) a docker-compose stack as the
// frontend for a profile.
//
// The command runner and the HTTP probe are injectable so unit tests can
// assert the argv we'd hand to `docker compose` and the URLs we'd poll
// without spawning docker or making real network calls.
type composeDriver struct {
	// runCommand executes a docker-compose command. Tests stub this out;
	// production uses execCommand which shells out for real.
	runCommand func(ctx context.Context, name string, args []string, env []string) error

	waitPoller
}

// defaultCompose returns a composeDriver configured for production: it
// shells out to `docker` and polls URLs with net/http.
func defaultCompose() *composeDriver {
	return &composeDriver{
		runCommand: execCommand,
		waitPoller: waitPoller{
			driver:         "compose",
			probeURL:       httpProbe,
			now:            time.Now,
			defaultTimeout: 60 * time.Second,
			pollInterval:   500 * time.Millisecond,
		},
	}
}

// execCommand runs `name args...` with env appended to the current
// environment and streams stdout/stderr to the daemon's standard handles.
// On non-zero exit, the last ~4 KiB of stderr is captured into a
// teeReader-style buffer and wrapped into the returned error so the CLI
// sees the real "failed to connect to podman socket" / "no such image"
// detail instead of an opaque `exit status 1`.
//
// cmd.Env is ALWAYS set (even with an empty env slice) so the child
// process gets a clean, predictable environment. Without this, Go leaves
// cmd.Env as nil and the child inherits the process env implicitly —
// which is fine until profile env vars need to be injected, at which
// point a partial env list can shadow inherited shell vars.
func execCommand(ctx context.Context, name string, args []string, env []string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = os.Stdout
	stderrTail := &tailBuffer{max: 4096}
	cmd.Stderr = io.MultiWriter(os.Stderr, stderrTail)
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderrTail.String()); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// tailBuffer keeps only the last `max` bytes written to it. We don't want
// to balloon the daemon's memory with the full stdout of a compose pull
// (can be hundreds of MB of layer-progress lines), but the LAST few KB
// is almost always where the real failure message lives.
type tailBuffer struct {
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.buf) }

// httpProbe issues a single GET to url and returns the response status code.
// Any transport error is returned to the caller, who decides whether to
// retry.
func httpProbe(ctx context.Context, url string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// Activate runs `docker compose up -d [services...]` and polls each
// wait_for entry until it returns 2xx or its timeout elapses. On success it
// returns a Result whose teardown invokes `docker compose down`.
func (c *composeDriver) Activate(reqCtx context.Context, p *profile.Profile, ctx profile.ExpandContext) (*Result, error) {
	composeFile, err := profile.ExpandPathString(p.Frontend.ComposeFile, ctx)
	if err != nil {
		return nil, fmt.Errorf("expand compose_file: %w", err)
	}
	if _, err := os.Stat(composeFile); err != nil {
		return nil, fmt.Errorf("compose_file %s: %w", composeFile, err)
	}

	projectName := p.Frontend.ProjectName
	if projectName == "" {
		projectName = "vibe-" + p.Name
	}

	env, err := p.ExpandEnv(ctx)
	if err != nil {
		return nil, fmt.Errorf("expand env: %w", err)
	}
	envSlice := envMapToSlice(env)

	// Pre-flight: tear down any stale containers from a previous run.
	// Without this, a daemon restart or profile switch can leave old
	// containers behind, and `compose up -d` fails with "container name
	// already in use". Best-effort — a failure here doesn't abort the
	// start, we just log a warning and continue (the up will handle it
	// by recreating if needed).
	downArgs := composeDownArgs(composeFile, projectName)
	if err := c.runCommand(reqCtx, "docker", downArgs, envSlice); err != nil {
		slog.Warn("frontend compose: stale teardown failed (continuing)",
			"profile", p.Name, "err", err)
	}

	upArgs := composeUpArgs(composeFile, projectName, p.Frontend.Services)
	slog.Info("frontend compose: up",
		"profile", p.Name,
		"compose_file", composeFile,
		"project", projectName,
	)
	if err := c.runCommand(reqCtx, "docker", upArgs, envSlice); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("docker not found on $PATH; install Docker (https://docs.docker.com/get-docker/) to run a docker-compose frontend")
		}
		return nil, fmt.Errorf("docker compose up: %w", err)
	}
	slog.Info("frontend compose: up done", "profile", p.Name, "project", projectName)

	if len(p.Frontend.WaitFor) > 0 {
		urls := make([]string, len(p.Frontend.WaitFor))
		for i, w := range p.Frontend.WaitFor {
			urls[i] = w.URL
		}
		slog.Info("frontend compose: waiting for ready", "profile", p.Name, "urls", urls)
	}
	if err := c.waitForReady(reqCtx, p.Frontend.WaitFor); err != nil {
		// Best-effort teardown: bring the stack back down so the caller
		// isn't left with a half-up stack on failure.
		downCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = c.runCommand(downCtx, "docker", composeDownArgs(composeFile, projectName), envSlice)
		return nil, err
	}

	teardown := func(tdCtx context.Context) error {
		return c.runCommand(tdCtx, "docker", composeDownArgs(composeFile, projectName), envSlice)
	}

	return &Result{
		WroteFile:       composeFile,
		RestartRequired: p.Frontend.RestartRequired,
		Env:             env,
		Kind:            profile.FrontendDockerCompose,
		teardown:        teardown,
	}, nil
}

// composeUpArgs returns the argv slice (minus the leading "docker" program
// name) for bringing the stack up. Services, when set, are passed as
// positional arguments after "up -d".
func composeUpArgs(composeFile, projectName string, services []string) []string {
	args := []string{"compose", "-f", composeFile, "-p", projectName, "up", "-d"}
	args = append(args, services...)
	return args
}

// composeDownArgs returns the argv slice for tearing the stack down. We
// intentionally don't pass per-service names here — `down` should reap the
// entire project so its networks/volumes get cleaned up.
func composeDownArgs(composeFile, projectName string) []string {
	return []string{"compose", "-f", composeFile, "-p", projectName, "down"}
}

// envMapToSlice flattens a string→string env map into the KEY=VAL format
// exec.Cmd.Env expects. Keys are sorted so the result is deterministic for
// tests and logs.
func envMapToSlice(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

// waitPoller is the wait_for readiness loop shared by the compose and
// managed drivers via embedding. probeURL and now are injectable so tests
// can fake network probes and time without either driver spawning anything.
type waitPoller struct {
	// driver names the embedding driver in log lines ("compose" |
	// "managed") so a ready log still says which lifecycle produced it.
	driver string

	// probeURL issues a single GET against url and returns the response
	// status code. The default uses net/http (httpProbe).
	probeURL func(ctx context.Context, url string) (int, error)

	// now is wired so tests can advance time deterministically; defaults
	// to time.Now.
	now func() time.Time

	// defaultTimeout is applied when a wait_for entry has no explicit
	// timeout. 60s matches the spec's example.
	defaultTimeout time.Duration

	// pollInterval is the gap between wait_for polls. Small, but tests
	// can override if they need to spin faster.
	pollInterval time.Duration
}

// waitForReady polls each wait_for URL until it returns 2xx or its timeout
// (or reqCtx) expires. URLs are polled sequentially: the spec is small,
// real frontends typically expose only one or two endpoints, and
// sequential polling keeps the error message unambiguous about which URL
// failed.
func (w *waitPoller) waitForReady(reqCtx context.Context, urls []profile.WaitForURL) error {
	for _, u := range urls {
		timeout := u.Timeout
		if timeout <= 0 {
			timeout = w.defaultTimeout
		}
		if err := w.pollURL(reqCtx, u.URL, timeout); err != nil {
			return err
		}
	}
	return nil
}

// pollURL hits url at w.pollInterval cadence until it answers with a 2xx
// status, or deadline / reqCtx fires. Non-2xx responses and transport
// errors are treated equivalently — the service may still be starting up.
func (w *waitPoller) pollURL(reqCtx context.Context, url string, timeout time.Duration) error {
	deadline := w.now().Add(timeout)
	var lastStatus int
	var lastErr error
	for {
		if err := reqCtx.Err(); err != nil {
			return fmt.Errorf("wait_for %s: %w", url, err)
		}
		// Per-poll context with the smaller of the remaining wait_for budget
		// and a 5s ceiling, so a stuck request can't outlast the deadline.
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return waitErr(url, lastStatus, lastErr)
		}
		probeCtx, cancel := context.WithTimeout(reqCtx, min(remaining, 5*time.Second))
		status, err := w.probeURL(probeCtx, url)
		cancel()
		if err == nil && status >= 200 && status < 300 {
			slog.Info("frontend "+w.driver+": wait_for ready", "url", url)
			return nil
		}
		lastStatus = status
		lastErr = err
		if w.now().After(deadline) {
			return waitErr(url, lastStatus, lastErr)
		}
		// Sleep with cancellation awareness.
		select {
		case <-reqCtx.Done():
			return fmt.Errorf("wait_for %s: %w", url, reqCtx.Err())
		case <-time.After(w.pollInterval):
		}
	}
}

// waitErr formats a wait_for-failed error including the most recent
// observation so logs are actionable.
func waitErr(url string, status int, lastErr error) error {
	if lastErr != nil {
		return fmt.Errorf("wait_for %s: timeout (last err: %v)", url, lastErr)
	}
	if status != 0 {
		return fmt.Errorf("wait_for %s: timeout (last status: %d)", url, status)
	}
	return fmt.Errorf("wait_for %s: timeout", url)
}
