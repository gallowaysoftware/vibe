package daemon

import (
	"strings"
	"testing"
)

// TestLoadProfileByName_RejectsTraversal pins the daemon's control-plane
// hardening against path-traversal in the profile-name field. A
// remote-but-token-authenticated caller submitting a Start RPC with a
// profile name like "../foo" must NOT be able to read arbitrary YAML
// files off the host — even though the bearer-token gate keeps random
// internet traffic out, the daemon should refuse a syntactically
// invalid name regardless of where the request came from.
func TestLoadProfileByName_RejectsTraversal(t *testing.T) {
	cases := []string{
		"../etc-passwd",
		"foo/bar",
		"foo bar",
		"foo.yaml",
		"foo$",
		"",
		"a\nb",
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := loadProfileByName(name)
			if err == nil {
				t.Fatalf("loadProfileByName(%q): expected validation error", name)
			}
			if !strings.Contains(err.Error(), "invalid") {
				t.Fatalf("loadProfileByName(%q): error %q must mention validation, not just missing file", name, err.Error())
			}
		})
	}
}
