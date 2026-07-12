package cli

import (
	"os"
	"path/filepath"
	"slices"
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
// basenames (newest first) for the commands that take a single
// <id-or-prefix>: `runs show`, `runs cancel`, `logs`, `cancel`. It
// keeps the run-targeting commands tab-completable from the same source,
// so the id printed by `vamp run --detach` can be completed back without
// the user hunting through $XDG_STATE_HOME.
var completeRunIDs = completeRunIDsUpTo(0)

// completeRunIDsUpTo builds a run-id completer that fills every argument
// slot up to maxIndex (0-based). `diff` takes two run ids, so it uses
// maxIndex 1; everything else sticks with completeRunIDs. Ids already
// consumed by earlier slots are dropped — diffing a run against itself
// is meaningless.
func completeRunIDsUpTo(maxIndex int) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
		if len(args) > maxIndex {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		runs, err := vamp.ListRuns(vamp.RunsDir())
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		ids := make([]string, 0, len(runs))
		for _, r := range runs {
			if slices.Contains(args, r.ID) {
				continue
			}
			ids = append(ids, r.ID)
		}
		return ids, cobra.ShellCompDirectiveNoFileComp
	}
}

// completeRenderArgs completes `vamp render <pipeline.yaml> <stage_id>`.
// Slot 0 mirrors completePipelineFiles; slot 1 loads the pipeline the
// user just named and offers its stage ids — the render loop otherwise
// retypes the same long stage id every iteration. Completion is
// advisory, so an unloadable pipeline degrades to no suggestions.
func completeRenderArgs(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	switch len(args) {
	case 0:
		return pipelineCandidates(vamp.PipelinesDir(), "."), cobra.ShellCompDirective(0)
	case 1:
		p, err := vamp.LoadPipeline(args[0])
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return stageIDs(p, ""), cobra.ShellCompDirectiveNoFileComp
	default:
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
}

// completeConfirmArgs completes `vamp confirm <id-or-prefix> <stage-id>`.
// Slot 0 is a run id; slot 1 resolves the run and offers the confirm-stage
// ids from its pipeline.yaml.snapshot. We parse the snapshot rather than
// pipeline.json because the executor only writes pipeline.json at run end
// — it never exists while a confirm stage is still blocking. Only
// `type: confirm` stages are offered since only those consume a response
// file; a typo'd id would silently write a response that never fires.
func completeConfirmArgs(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	switch len(args) {
	case 0:
		return completeRunIDs(cmd, args, toComplete)
	case 1:
		r, err := vamp.FindRunByPrefix(vamp.RunsDir(), args[0])
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		p, err := vamp.LoadPipeline(filepath.Join(r.Path, "pipeline.yaml.snapshot"))
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return stageIDs(p, vamp.StageTypeConfirm), cobra.ShellCompDirectiveNoFileComp
	default:
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
}

// stageIDs lists a pipeline's stage ids in declaration order, optionally
// filtered to one stage type ("" means all).
func stageIDs(p *vamp.Pipeline, only vamp.StageType) []string {
	ids := make([]string, 0, len(p.Stages))
	for i := range p.Stages {
		if only != "" && p.Stages[i].Type != only {
			continue
		}
		ids = append(ids, p.Stages[i].ID)
	}
	return ids
}
