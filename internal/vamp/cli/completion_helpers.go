package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vamp"
)

// completePipelineFiles is a cobra ValidArgsFunction that suggests pipeline
// YAML files for `vamp run` and `vamp validate`.
//
// The completer surfaces two sources:
//  1. *.yaml files under $XDG_CONFIG_HOME/vamp/pipelines/ — as full paths
//     so the user can hit Tab and run a config-home pipeline by name
//     without leaving the path computation to the shell.
//  2. *.yaml files in the current working directory (mirrors the
//     `./pipeline.yaml` / `./*.yaml` convention from the task brief),
//     prefixed with "./" so they sort distinctly from XDG hits.
//
// We return cobra.ShellCompDirective(0) — the default — so the shell ALSO
// does its own filename completion, which keeps `vamp run path/to/<TAB>`
// useful for pipelines that live outside both well-known locations.
//
// We do NOT attempt to LoadPipeline-validate each candidate the way the
// vibe profile completer does: pipelines reference workflow files / prompt
// files relative to the pipeline's directory, so validation here would
// silently drop genuinely-runnable pipelines whose context lives elsewhere.
func completePipelineFiles(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		// `run` / `validate` are ExactArgs(1); past the slot, do nothing.
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return pipelineCandidates(vamp.PipelinesDir(), "."), cobra.ShellCompDirective(0)
}

// pipelineCandidates returns the union of:
//   - full paths of *.yaml files under xdgDir
//   - relative paths of *.yaml files in localDir (prefixed with "./")
//
// Returned list is deduplicated and sorted. Missing directories yield an
// empty slice from that source rather than an error — completion is
// advisory.
func pipelineCandidates(xdgDir, localDir string) []string {
	seen := map[string]struct{}{}
	add := func(s string) {
		if s == "" {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
	}

	if entries, err := os.ReadDir(xdgDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
				continue
			}
			add(filepath.Join(xdgDir, e.Name()))
		}
	}
	if entries, err := os.ReadDir(localDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
				continue
			}
			add("./" + e.Name())
		}
	}

	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// completeRunIDs is a cobra ValidArgsFunction that suggests run-dir
// basenames (newest first) for the commands that take an <id-or-prefix>:
// `runs show`, `runs cancel`, `logs`, `confirm`, `diff`, `cancel`. It
// keeps the run-targeting commands tab-completable from the same source,
// so the id printed by `vamp run --detach` can be completed back without
// the user hunting through $XDG_STATE_HOME.
func completeRunIDs(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	runs, err := vamp.ListRuns(vamp.RunsDir())
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	ids := make([]string, 0, len(runs))
	for _, r := range runs {
		ids = append(ids, r.ID)
	}
	return ids, cobra.ShellCompDirectiveNoFileComp
}
