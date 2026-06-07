package cli

import (
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vamp"
)

// lintCmd is the standalone-YAML counterpart of lintCmdInMemory. It loads
// a pipeline file off disk and runs the same checks against the parsed
// pipeline value.
func lintCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:               "lint <pipeline.yaml>",
		Short:             "Surface non-fatal pipeline-author problems on top of validate.",
		Long:              lintCmdLong,
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
			return LintPipeline(p, cmd.OutOrStdout())
		},
	}
	return cmd
}

// lintCmdInMemory mounts `lint` on every Go-DSL pipeline binary. The pipeline
// is already in memory (no YAML to re-parse); the checks are identical.
func lintCmdInMemory(factory PipelineFactory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "lint",
		Short: "Surface non-fatal pipeline-author problems on top of validate.",
		Long:  lintCmdLong,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := factory()
			if err != nil {
				return err
			}
			return LintPipeline(p, cmd.OutOrStdout())
		},
	}
	return cmd
}

const lintCmdLong = `lint runs after validate and surfaces non-fatal mistakes
the author may have missed. Findings are advisory — exit 0 even when warnings
fire — so pipelines that knowingly break a rule keep running.

Checks today:
  1. Webhook URL → matching RequireService. A webhook stage that POSTs to
     a 127.0.0.1:* URL with no matching p.RequireService(...) means the
     pipeline can't be auto-activated or doctored. Skipped when the URL
     contains a Go template (we can't statically resolve {{ … }}).

  2. JSON output_format → retry on invalid_output. A text stage with
     output_format: json should declare a Retry policy that includes
     "invalid_output" in RetryOn, otherwise a single malformed-JSON
     emission from the model fails the whole pipeline without a retry
     pass to clean up. (Transient errors retry regardless.)

  3. Trivial Retry blocks. A Retry block with MaxAttempts < 2 is a no-op;
     either the field is wrong or the whole block should be dropped.

  4. Capability used but undocumented. A stage references a capability
     that isn't in p.CapabilityModelHints, so doctor can't sanity-check
     the resolved profile against expected min_params / min_context.
`

// LintPipeline runs every advisory check against an in-memory pipeline and
// prints a per-finding line plus a summary footer. Always returns nil today
// (warnings only); future hard-rule checks may switch to a non-nil return so
// CI gates can refuse to merge violations.
func LintPipeline(p *vamp.Pipeline, w io.Writer) error {
	var findings []string
	findings = append(findings, lintWebhookServices(p)...)
	findings = append(findings, lintJSONRetryPolicy(p)...)
	findings = append(findings, lintTrivialRetry(p)...)
	findings = append(findings, lintMissingCapabilityHint(p)...)

	if len(findings) == 0 {
		fmt.Fprintf(w, "lint: %s — clean (%d stage(s) checked)\n", p.Name, len(p.Stages))
		return nil
	}
	sort.Strings(findings)
	for _, f := range findings {
		fmt.Fprintf(w, "  [WARN] %s\n", f)
	}
	fmt.Fprintf(w, "lint: %s — %d warning(s) on %d stage(s)\n", p.Name, len(findings), len(p.Stages))
	return nil
}

// lintWebhookServices returns one finding per webhook stage that POSTs to a
// loopback host:port no RequireService URL covers. Template-bearing URLs
// (any "{{" substring) are skipped — we can't predict what they render to.
func lintWebhookServices(p *vamp.Pipeline) []string {
	// Index host:port across declared services so the per-stage check is
	// O(1) per webhook.
	declared := make(map[string]string)
	for _, svc := range p.RequiredServices {
		if svc.URL == "" {
			continue
		}
		if hp, ok := hostPortOf(svc.URL); ok {
			declared[hp] = svc.Name
		}
	}

	var out []string
	for i := range p.Stages {
		s := &p.Stages[i]
		if s.Type != vamp.StageTypeWebhook {
			continue
		}
		if s.URL == "" || strings.Contains(s.URL, "{{") {
			continue
		}
		hp, ok := hostPortOf(s.URL)
		if !ok {
			continue
		}
		if !isLoopback(hp) {
			// Public / LAN services aren't expected to be declared as
			// vibe-managed RequireService entries.
			continue
		}
		if _, found := declared[hp]; !found {
			out = append(out, fmt.Sprintf(
				"stage %s: webhook URL %s targets %s but no RequireService matches — declare it via p.RequireService(...) so `<pipeline> doctor` + `activate` can preflight it",
				s.ID, s.URL, hp))
		}
	}
	return out
}

// lintJSONRetryPolicy returns one finding per text stage that emits JSON
// without a Retry policy that covers "invalid_output". A malformed-JSON
// emission today fails the run with no retry pass.
func lintJSONRetryPolicy(p *vamp.Pipeline) []string {
	var out []string
	for i := range p.Stages {
		s := &p.Stages[i]
		if s.Type != vamp.StageTypeText || s.OutputFormat != "json" {
			continue
		}
		if s.Retry != nil && containsString(s.Retry.RetryOn, "invalid_output") {
			continue
		}
		out = append(out, fmt.Sprintf(
			"stage %s: output_format: json but RetryOn does not include \"invalid_output\" — a malformed JSON emission will fail the run without retrying",
			s.ID))
	}
	return out
}

// lintTrivialRetry returns one finding per stage whose Retry block sets
// MaxAttempts < 2 (a "retry" of one attempt is a no-op). Either the field
// is wrong or the whole block should be dropped — flag for the author to
// pick.
func lintTrivialRetry(p *vamp.Pipeline) []string {
	var out []string
	for i := range p.Stages {
		s := &p.Stages[i]
		if s.Retry == nil {
			continue
		}
		if s.Retry.MaxAttempts >= 2 {
			continue
		}
		out = append(out, fmt.Sprintf(
			"stage %s: Retry.MaxAttempts=%d — a retry block with <2 attempts is a no-op; drop the block or bump the count",
			s.ID, s.Retry.MaxAttempts))
	}
	return out
}

// lintMissingCapabilityHint returns one finding per distinct capability
// declared on a stage but absent from p.CapabilityModelHints. Hints are
// optional, but their absence means `<pipeline> doctor` can't refuse a
// model that obviously can't satisfy the prompts (too small, too short a
// context); for any non-trivial pipeline they're worth filling in.
//
// Deduped on capability name so a pipeline with 10 text stages on one
// capability emits a single finding, not 10.
func lintMissingCapabilityHint(p *vamp.Pipeline) []string {
	used := map[string]string{} // capability → first stage id seen
	for i := range p.Stages {
		s := &p.Stages[i]
		if s.Capability == "" {
			continue
		}
		if _, ok := used[s.Capability]; !ok {
			used[s.Capability] = s.ID
		}
	}
	var out []string
	for cap, firstStage := range used {
		if _, ok := p.CapabilityModelHints[cap]; ok {
			continue
		}
		out = append(out, fmt.Sprintf(
			"capability %q (used by stage %s, …): no CapabilityModelHints entry — `<pipeline> doctor` can't sanity-check the resolved profile against expected min_params / min_context",
			cap, firstStage))
	}
	return out
}

// hostPortOf parses raw as a URL and returns "host:port" with the default
// port filled in when the URL omits one. ok=false means the input wasn't a
// parseable URL (caller skips it).
func hostPortOf(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", false
	}
	host, port := u.Hostname(), u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "http":
			port = "80"
		case "https":
			port = "443"
		default:
			return "", false
		}
	}
	return host + ":" + port, true
}

// isLoopback reports whether host:port targets the local machine. Covers the
// names a developer typically types (127.0.0.1, localhost, ::1) — anything
// else is treated as a remote address the linter doesn't second-guess.
func isLoopback(hostPort string) bool {
	host := hostPort
	if i := strings.LastIndex(hostPort, ":"); i > 0 {
		host = hostPort[:i]
	}
	switch host {
	case "127.0.0.1", "localhost", "::1", "[::1]":
		return true
	}
	return false
}

func containsString(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
