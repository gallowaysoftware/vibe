package cli

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// doctorCmdInMemory is the read-only counterpart of activate: it
// inspects what the pipeline needs and reports what's missing,
// without starting anything. Useful before a long run when the
// operator wants to confirm the environment without committing
// to bring services up. Exits non-zero when anything is missing
// so CI / shell scripts can use it as a gate.
func doctorCmdInMemory(factory PipelineFactory) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check that every vibe profile this pipeline needs is up and ready.",
		Long: `doctor walks the pipeline's declared requirements (RequireProfile +
RequireService) and reports which are running, which are
unreachable, and which never had a profile of that name on the host.

Unlike activate, doctor never starts a profile — it's the
read-only "what's missing right now" check. Exit code is 0 when
everything's healthy and non-zero (with a one-line per-issue
report) otherwise.
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

			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()

			var issues []string

			// Active-mode profile check. We don't know its expected
			// address (the proxy port :9000 is shared across all
			// active profiles); rely on `vibe ps` for that. Just
			// note the requirement and the user.
			if req.Profile != "" {
				if running, _ := isActiveProfileRunning(ctx); running != "" {
					if running == req.Profile {
						fmt.Fprintf(cmd.OutOrStdout(), "  ✓ active profile %s — running\n", req.Profile)
					} else {
						fmt.Fprintf(cmd.OutOrStdout(), "  ✗ active profile %s — wrong profile is active (%s)\n", req.Profile, running)
						issues = append(issues, fmt.Sprintf("active profile %s not running (got %s)", req.Profile, running))
					}
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "  ✗ active profile %s — not running\n", req.Profile)
					issues = append(issues, fmt.Sprintf("active profile %s not running", req.Profile))
				}
			}

			// Service URL probes. Anything 2xx-4xx is "up"; 5xx +
			// connection errors are "down."
			for _, svc := range req.Services {
				if svc.URL == "" {
					continue
				}
				ok := probeURL(ctx, svc.URL)
				if ok {
					fmt.Fprintf(cmd.OutOrStdout(), "  ✓ %s — %s\n", svc.Name, svc.URL)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "  ✗ %s — unreachable at %s\n", svc.Name, svc.URL)
					hint := svc.SetupHint
					if hint == "" {
						hint = "(no setup hint declared)"
					}
					fmt.Fprintf(cmd.OutOrStdout(), "      try: %s\n", hint)
					issues = append(issues, fmt.Sprintf("%s unreachable", svc.Name))
				}
			}

			fmt.Fprintln(cmd.OutOrStdout())
			if len(issues) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "ready — every declared requirement is up")
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "missing %d requirement(s) — run `%s activate` to start them\n",
				len(issues), cmd.Root().Use)
			// Make the exit-code non-zero so CI gates work.
			return fmt.Errorf("%d unmet requirement(s)", len(issues))
		},
	}
}

// isActiveProfileRunning shells out to `vibe ps` and parses the
// "active: <name>" line. Returns "" + nil when no active profile.
// Best-effort — a parse miss returns ("", nil), letting doctor
// fall back to the not-running branch.
func isActiveProfileRunning(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "vibe", "ps").Output()
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		// Format: "active:   <name> (ready|starting)"
		if strings.HasPrefix(strings.TrimSpace(line), "active:") {
			rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "active:"))
			// Strip the trailing " (status)" by truncating at first space.
			if sp := strings.IndexByte(rest, ' '); sp > 0 {
				return rest[:sp], nil
			}
			return rest, nil
		}
	}
	return "", nil
}

// probeURL does a single GET with a tight timeout. Any non-5xx
// response (including 401, 404) counts as "the server is up" —
// we're not testing the endpoint, we're testing that something
// answers.
func probeURL(ctx context.Context, url string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode < 500
}
