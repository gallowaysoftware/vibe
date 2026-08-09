package cli

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// activateCmdInMemory mounts the `activate` subcommand on every
// vamp-built pipeline binary. The user runs `<pipeline> activate`
// and vibe brings up everything the pipeline needs:
//
//  1. The active-mode profile declared by p.RequireProfile (the
//     LLM-bearing slot fronted by the proxy at :9000).
//  2. Every service-mode profile declared by p.RequireService —
//     extracted from the setup_hint string if it matches the
//     `vibe start <name>` shape.
//
// Already-running profiles are skipped. Each declared service URL
// is health-polled after start so the user knows whether the
// service actually came up, not just whether the start command
// returned 0.
//
// Setup hints that don't match `vibe start ...` are surfaced
// verbatim with a "run this yourself" footer — keeps the command
// safe to add to any pipeline regardless of how its services come
// up today.
func activateCmdInMemory(factory PipelineFactory) *cobra.Command {
	var skipActive bool
	cmd := &cobra.Command{
		Use:   "activate",
		Short: "Start every vibe profile this pipeline needs (active + services).",
		Long: `activate brings up the active-mode and service-mode vibe profiles
this pipeline requires, then health-checks each declared service URL.

Reads:
  - p.RequireProfile     → the active-mode LLM profile to start.
  - p.RequireService      → service-mode profiles, derived from each
                            service's setup_hint when it has the shape
                            "vibe start <name>" (anything else is
                            surfaced as a manual instruction).

Idempotent: profiles already running are reported as such and
skipped. After all starts, polls each service URL until ready
(or a 30s timeout, then fails with the failing service named).

--skip-active brings up only service-mode profiles. Useful when
the GPU is already busy with something else and you just need the
CPU sidecars running.
`,
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := factory()
			if err != nil {
				return err
			}
			req := p.Requirements()

			fmt.Fprintln(cmd.OutOrStdout(), "pipeline:", req.Pipeline)
			if req.Description != "" {
				fmt.Fprintln(cmd.OutOrStdout(), "  ", req.Description)
			}
			fmt.Fprintln(cmd.OutOrStdout())

			// Step 1: active-mode profile.
			switch {
			case skipActive:
				fmt.Fprintln(cmd.OutOrStdout(), "active-mode profile SKIPPED (--skip-active)")
			case req.Profile != "":
				if err := vibeStartContext(cmd.Context(), cmd.OutOrStdout(), req.Profile, true); err != nil {
					return err
				}
			default:
				fmt.Fprintln(cmd.OutOrStdout(), "no active-mode profile declared — skipping (pipeline's text stages will require one to be running externally)")
			}

			// Step 2: service-mode profiles, derived from setup hints.
			for _, svc := range req.Services {
				name := parseVibeStartHint(svc.SetupHint)
				if name == "" {
					fmt.Fprintf(cmd.OutOrStdout(),
						"%s — no parseable `vibe start ...` setup hint; run manually:\n    %s\n",
						svc.Name, svc.SetupHint)
					continue
				}
				if err := vibeStartContext(cmd.Context(), cmd.OutOrStdout(), name, false); err != nil {
					return err
				}
			}

			// Step 3: health-check declared service URLs.
			fmt.Fprintln(cmd.OutOrStdout())
			fmt.Fprintln(cmd.OutOrStdout(), "health-checking declared services…")
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			var bad []string
			for _, svc := range req.Services {
				if svc.URL == "" {
					continue
				}
				if err := waitForURL(ctx, svc.URL); err != nil {
					bad = append(bad, fmt.Sprintf("%s (%s): %v", svc.Name, svc.URL, err))
					continue
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  ✓ %s — %s\n", svc.Name, svc.URL)
			}
			if len(bad) > 0 {
				return fmt.Errorf("not ready: %s", strings.Join(bad, "; "))
			}
			fmt.Fprintln(cmd.OutOrStdout())
			fmt.Fprintln(cmd.OutOrStdout(), "ready — run the pipeline now")
			return nil
		},
	}
	cmd.Flags().BoolVar(&skipActive, "skip-active", false, "Bring up only service-mode profiles; skip the active-mode LLM profile.")
	return cmd
}

// vibeStartHintRE matches a `vibe start <profile>` setup hint
// regardless of surrounding whitespace + ignoring trailing flags.
// Captures group 1 is the profile name.
var vibeStartHintRE = regexp.MustCompile(`(?m)^\s*vibe\s+start\s+([A-Za-z0-9_-]+)`)

func parseVibeStartHint(hint string) string {
	m := vibeStartHintRE.FindStringSubmatch(hint)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// vibeStartTimeout bounds one `vibe start`. Starting a profile means the
// daemon loading a model, which on a cold page cache is tens of seconds
// for a large GGUF — so the number is generous. It exists because the
// call had NO bound: a daemon wedged mid-start (the interesting failure,
// as opposed to a refused connection, which returns instantly) left
// `<pipeline> activate` sitting at a blank prompt with nothing to
// report and nothing to cancel.
const vibeStartTimeout = 5 * time.Minute

// vibeStartContext shells out to `vibe start <name>`. The vibe daemon
// distinguishes active-mode from service-mode internally via the
// profile YAML's `mode:` field — we just say "start it" and let
// the daemon route. The active arg is informational for the log
// prefix.
//
// It takes a ctx so the operator's ctrl-C reaches the child, and bounds
// it regardless: the caller's context on the `activate` path has no
// deadline of its own.
func vibeStartContext(ctx context.Context, w cobraOutputWriter, name string, active bool) error {
	tag := "service"
	if active {
		tag = "active"
	}
	ctx, cancel := context.WithTimeout(ctx, vibeStartTimeout)
	defer cancel()
	c := exec.CommandContext(ctx, "vibe", "start", name)
	// A zero WaitDelay is documented as "Wait blocks indefinitely on the
	// pipes", which would put the unbounded wait straight back after the
	// deadline had done its job. CombinedOutput holds both pipes, so this
	// is exactly the path that needs it. (Same rule as
	// vamp.subprocessKillGrace; a different package, so a literal rather
	// than a shared constant nothing else here would use.)
	c.WaitDelay = 2 * time.Second
	out, err := c.CombinedOutput()
	body := strings.TrimSpace(string(out))
	if ctx.Err() != nil {
		return fmt.Errorf("vibe start %s: no answer within %s (is the vibe daemon wedged? try `vibe ps`)\n%s", name, vibeStartTimeout, body)
	}
	// "already_exists" / "is already running" comes back from the
	// daemon when the profile holds the slot already — treat as a
	// no-op success.
	if err != nil {
		if strings.Contains(body, "already running") || strings.Contains(body, "AlreadyExists") {
			fmt.Fprintf(w, "%s (%s) — already running, skipping\n", name, tag)
			return nil
		}
		return fmt.Errorf("vibe start %s: %w\n%s", name, err, body)
	}
	fmt.Fprintf(w, "%s (%s) — started\n", name, tag)
	return nil
}

// cobraOutputWriter is the narrow interface activateCmd needs for
// its progress prints. cobra commands expose this via cmd.OutOrStdout().
type cobraOutputWriter interface {
	Write([]byte) (int, error)
}

// waitForURL polls url until it returns any HTTP response or ctx
// expires. Any 2xx-4xx is good enough — we care that the server
// is listening, not that the specific endpoint is correct.
func waitForURL(ctx context.Context, url string) error {
	client := &http.Client{Timeout: 3 * time.Second}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for %s", url)
		case <-ticker.C:
		}
	}
}
