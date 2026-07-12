package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vamp"
)

const doctorCmdLong = `doctor walks the pipeline's declared requirements (RequireProfile +
RequireService) and reports which are running, which are
unreachable, and which never had a profile of that name on the host.

Unlike activate, doctor never starts a profile — it's the
read-only "what's missing right now" check. Exit code is 0 when
everything's healthy and non-zero (with a one-line per-issue
report) otherwise.
`

// doctorCmd is the standalone-YAML counterpart of doctorCmdInMemory. It
// loads a pipeline file off disk and runs the same read-only requirement
// checks against the parsed pipeline value.
func doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "doctor <pipeline.yaml>",
		Short:             "Check that every vibe profile a pipeline needs is up and ready.",
		Long:              doctorCmdLong,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completePipelineFiles,
		RunE: func(cmd *cobra.Command, args []string) error {
			pipelinePath, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			p, err := vamp.LoadPipeline(pipelinePath)
			if err != nil {
				return err
			}
			return DoctorPipeline(cmd.Context(), p, cmd.Root().Use, cmd.OutOrStdout())
		},
	}
}

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
		Long:  doctorCmdLong,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := factory()
			if err != nil {
				return err
			}
			return DoctorPipeline(cmd.Context(), p, cmd.Root().Use, cmd.OutOrStdout())
		},
	}
}

// DoctorPipeline runs the read-only requirement checks against an in-memory
// pipeline and reports per-requirement status. Returns a non-nil error when
// anything is unmet so the caller's exit code can gate CI / shell scripts.
// rootUse names the binary in the "run `<root> activate`" hint.
func DoctorPipeline(ctx context.Context, p *vamp.Pipeline, rootUse string, w io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	req := p.Requirements()

	fmt.Fprintln(w, "pipeline:", req.Pipeline)
	if req.Description != "" {
		fmt.Fprintln(w, "  ", req.Description)
	}
	fmt.Fprintln(w)

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var issues []string

	// Active-mode profile check. We don't know its expected
	// address (the proxy port :9000 is shared across all
	// active profiles); rely on `vibe ps` for that. Just
	// note the requirement and the user.
	if req.Profile != "" {
		if running, _ := isActiveProfileRunning(ctx); running != "" {
			if running == req.Profile {
				fmt.Fprintf(w, "  ✓ active profile %s — running\n", req.Profile)
			} else {
				fmt.Fprintf(w, "  ✗ active profile %s — wrong profile is active (%s)\n", req.Profile, running)
				issues = append(issues, fmt.Sprintf("active profile %s not running (got %s)", req.Profile, running))
			}
		} else {
			fmt.Fprintf(w, "  ✗ active profile %s — not running\n", req.Profile)
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
			fmt.Fprintf(w, "  ✓ %s — %s\n", svc.Name, svc.URL)
		} else {
			fmt.Fprintf(w, "  ✗ %s — unreachable at %s\n", svc.Name, svc.URL)
			hint := svc.SetupHint
			if hint == "" {
				hint = "(no setup hint declared)"
			}
			fmt.Fprintf(w, "      try: %s\n", hint)
			issues = append(issues, fmt.Sprintf("%s unreachable", svc.Name))
		}
	}

	fmt.Fprintln(w)
	if len(issues) == 0 {
		fmt.Fprintln(w, "ready — every declared requirement is up")
		return nil
	}
	fmt.Fprintf(w, "missing %d requirement(s) — run `%s activate` to start them\n",
		len(issues), rootUse)
	// Make the exit-code non-zero so CI gates work.
	return fmt.Errorf("%d unmet requirement(s)", len(issues))
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
