package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
)

// completeProfileNames is a cobra ValidArgsFunction that suggests profile
// names by scanning $XDG_CONFIG_HOME/vibe/profiles/ for valid *.yaml files.
//
// "Valid" means: loads + validates via profile.Load. Invalid files are
// silently dropped — completion is a UX nicety, not a diagnostic, and we
// don't want a typo in one profile to mask completion for the others. Use
// `vibe doctor` or `vibe list` to surface invalid profiles.
//
// Returns NoFileComp so the shell doesn't fall through to filename
// completion when no profile matches the prefix.
func completeProfileNames(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	// `start <profile>` and `pull <profile>` are ExactArgs(1); once one
	// arg is present the user is past the profile slot.
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names := listValidProfileNames(paths.ProfilesDir())
	return names, cobra.ShellCompDirectiveNoFileComp
}

// listValidProfileNames returns the basenames (minus .yaml) of every
// profile under dir that parses and validates. Exported through a small
// surface (rather than inlined into completeProfileNames) so the unit test
// can drive it directly without faking a *cobra.Command.
func listValidProfileNames(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}
		full := filepath.Join(dir, name)
		if _, err := profile.Load(full); err != nil {
			continue
		}
		out = append(out, strings.TrimSuffix(name, ".yaml"))
	}
	sort.Strings(out)
	return out
}
