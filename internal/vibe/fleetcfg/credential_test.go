package fleetcfg

// C13: the two credential precedences, in one place. Both orders are
// correct for their context (C6's NIT-E), and both are now testable
// without standing up either caller — which is the point: `vibe fleet
// doctor` reports the credential the ACTUATION path resolves, so a
// second implementation would test its own code.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func credFixture(t *testing.T) (*File, string) {
	t.Helper()
	dir := t.TempDir()
	tok := filepath.Join(dir, "cell-token")
	if err := os.WriteFile(tok, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &File{Cells: map[string]Cell{
		FrontCell:  {URL: "http://127.0.0.1:1", Class: ClassAlwaysOn},
		"with":     {URL: "http://127.0.0.1:1", Class: ClassOpportunistic, TokenFile: tok},
		"without":  {URL: "http://127.0.0.1:1", Class: ClassOpportunistic},
		"missing":  {URL: "http://127.0.0.1:1", Class: ClassOpportunistic, TokenFile: filepath.Join(dir, "nope")},
		"emptyfil": {URL: "http://127.0.0.1:1", Class: ClassOpportunistic, TokenFile: writeEmpty(t, dir)},
	}}
	return f, tok
}

func writeEmpty(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "empty-token")
	if err := os.WriteFile(p, []byte("\n  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestCellCredential_PreferenceDecidesOnlyWhenBothExist: the two orders
// differ in exactly one case, and pinning that keeps a future
// "simplification" from collapsing them.
func TestCellCredential_PreferenceDecidesOnlyWhenBothExist(t *testing.T) {
	f, _ := credFixture(t)
	local := func() string { return "from-local" }

	for _, tc := range []struct {
		name       string
		cell, env  string
		pref       CredentialPreference
		wantToken  string
		wantKind   string
		sourceHint string
	}{
		{"fleetd: the file wins over the env", "with", "from-env", PreferCellFile, "from-file", CredCellFile, "token_file"},
		{"cli: the env wins over the file", "with", "from-env", PreferEnv, "from-env", CredEnv, "$VIBE_TOKEN"},
		{"fleetd: no file, the env is the fallback", "without", "from-env", PreferCellFile, "from-env", CredEnv, "$VIBE_TOKEN"},
		{"cli: no file, the env is the answer", "without", "from-env", PreferEnv, "from-env", CredEnv, "$VIBE_TOKEN"},
		{"fleetd: no file, no env, the local token", "without", "", PreferCellFile, "from-local", CredLocal, "own control-plane token"},
		{"cli: no env, the file", "with", "", PreferEnv, "from-file", CredCellFile, "token_file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := f.CellCredential(tc.cell, tc.env, tc.pref, local)
			if err != nil {
				t.Fatalf("CellCredential: %v", err)
			}
			if got.Token != tc.wantToken || got.Kind != tc.wantKind {
				t.Errorf("token/kind = %q/%q, want %q/%q", got.Token, got.Kind, tc.wantToken, tc.wantKind)
			}
			if !strings.Contains(got.Source, tc.sourceHint) {
				t.Errorf("source = %q, want it to name %q", got.Source, tc.sourceHint)
			}
			if strings.Contains(got.Source, got.Token) && got.Kind == CredCellFile {
				t.Errorf("source %q leaks the token value", got.Source)
			}
		})
	}
}

// TestCellCredential_BrokenFileIsAnErrorNotAFallthrough: both failures
// otherwise turn a typo into an opaque 401 from a remote box, which is
// the swallow C6's MIN-P fixed for the CLI. The empty-file rung is new
// in C13 and is the same failure with a different spelling.
func TestCellCredential_BrokenFileIsAnErrorNotAFallthrough(t *testing.T) {
	f, _ := credFixture(t)
	local := func() string { return "from-local" }
	for _, cell := range []string{"missing", "emptyfil"} {
		if _, err := f.CellCredential(cell, "from-env", PreferCellFile, local); err == nil ||
			!strings.Contains(err.Error(), "token_file") {
			t.Errorf("%s: err = %v, want a named token_file failure", cell, err)
		}
	}
	if _, err := f.CellCredential("nosuchcell", "", PreferCellFile, local); err == nil {
		t.Error("unknown cell resolved a credential")
	}
	var nilFile *File
	if _, err := nilFile.CellCredential("with", "", PreferCellFile, local); err == nil {
		t.Error("a nil registry resolved a credential")
	}
}
