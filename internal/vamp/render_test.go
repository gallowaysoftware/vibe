package vamp

import (
	"errors"
	"testing"
)

// TestRenderStageOutputPath_RefusesAnEscapeFromTheRunDir.
//
// renderTemplate-for-output-paths has five callers. Four ran the result
// through ensureUnderRunDir — Executor.renderOutputPath (exec.go),
// dryRunState.renderOutputPath (dryrun.go) and the two in diff.go, one of
// which carries a ten-line comment naming this exact threat. This one did
// not, and `vamp render` feeds its result straight into os.ReadFile and
// then into the printed prompt.
//
// The file measured 100% line coverage before this test existed, and
// asserted nothing about what it should REFUSE. That is the calibration
// point worth keeping: coverage was never the missing thing.
func TestRenderStageOutputPath_RefusesAnEscapeFromTheRunDir(t *testing.T) {
	cases := []struct {
		name   string
		output string
		inputs map[string]string
	}{
		{"literal traversal", "../outside/secret.txt", nil},
		{"traversal via an input", "{{ .inputs.name }}", map[string]string{"name": "../../etc/passwd"}},
		{"absolute path", "/etc/hostname", nil},
		{"traversal in a subdirectory", "sub/../../escape.txt", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &Stage{ID: "dep", Output: tc.output}
			got, err := RenderStageOutputPath(st, tc.inputs, nil, t.TempDir())
			if err == nil {
				t.Fatalf("rendered an escaping output path: %q", got)
			}
			if !errors.Is(err, errOutputPathEscape) {
				t.Errorf("want errOutputPathEscape, got %v", err)
			}
			if got != "" {
				t.Errorf("a refused path must not also be returned: %q", got)
			}
		})
	}
}

// TestRenderStageOutputPath_AcceptsALegitimatePath keeps the guard from
// being satisfiable by refusing everything — the failure mode that would
// break `vamp render --run-dir` for every real pipeline.
func TestRenderStageOutputPath_AcceptsALegitimatePath(t *testing.T) {
	for _, tc := range []struct {
		output string
		want   string
		inputs map[string]string
	}{
		{"report.md", "report.md", nil},
		{"judge_{{ .inputs.i }}.json", "judge_3.json", map[string]string{"i": "3"}},
		{"sub/dir/out.txt", "sub/dir/out.txt", nil},
	} {
		st := &Stage{ID: "dep", Output: tc.output}
		got, err := RenderStageOutputPath(st, tc.inputs, nil, t.TempDir())
		if err != nil {
			t.Errorf("output %q refused: %v", tc.output, err)
			continue
		}
		if got != tc.want {
			t.Errorf("output %q = %q, want %q", tc.output, got, tc.want)
		}
	}
}
