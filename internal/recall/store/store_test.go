package store

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestStore returns a store on a real git repo in a temp dir — the
// git interaction IS the logic here, so no fake.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s := New(t.TempDir())
	if err := s.EnsureRepo(); err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}
	return s
}

func commitCount(t *testing.T, s *Store) int {
	t.Helper()
	out, err := s.git("rev-list", "--count", "HEAD")
	if err != nil {
		t.Fatalf("rev-list: %v", err)
	}
	n := 0
	for _, r := range strings.TrimSpace(out) {
		n = n*10 + int(r-'0')
	}
	return n
}

func TestEnsureRepoIdempotent(t *testing.T) {
	s := newTestStore(t)
	if err := s.EnsureRepo(); err != nil {
		t.Fatalf("second EnsureRepo: %v", err)
	}
	if got := commitCount(t, s); got != 1 {
		t.Errorf("commits after double init = %d, want 1", got)
	}
}

func TestCreateExpertAndScaffold(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateExpert("x4", "X4 Foundations campaign companion"); err != nil {
		t.Fatalf("CreateExpert: %v", err)
	}
	if err := s.CreateExpert("x4", "again"); err == nil {
		t.Fatal("duplicate CreateExpert did not error")
	}

	files, err := s.ListFiles("x4")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	want := []string{"profile.md", "goals.md", "decisions.md", "log.md"}
	if len(files) != len(want) {
		t.Fatalf("files = %v, want %v", files, want)
	}

	experts, err := s.ListExperts()
	if err != nil {
		t.Fatalf("ListExperts: %v", err)
	}
	if len(experts) != 1 || experts[0] != "x4" {
		t.Errorf("experts = %v, want [x4]", experts)
	}
}

func TestExpertNameValidation(t *testing.T) {
	s := newTestStore(t)
	cases := []string{"../escape", "X4", "a/b", "", strings.Repeat("a", 80)}
	for _, name := range cases {
		if err := s.CreateExpert(name, "d"); err == nil {
			t.Errorf("CreateExpert(%q) accepted invalid name", name)
		}
	}
}

func TestReadFileValidation(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateExpert("x4", "d"); err != nil {
		t.Fatalf("CreateExpert: %v", err)
	}
	for _, f := range []string{"../../etc/passwd", "notes/../profile.md", "other.md", "notes/UP.md"} {
		if _, err := s.ReadFile("x4", f); err == nil {
			t.Errorf("ReadFile(%q) accepted invalid path", f)
		}
	}
	if _, err := s.ReadFile("x4", "notes/missing.md"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing note error = %v, want ErrNotFound", err)
	}
}

func TestRecordDecisionRequiresWhy(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateExpert("x4", "d"); err != nil {
		t.Fatalf("CreateExpert: %v", err)
	}
	if err := s.RecordDecision("x4", "build a wharf", " "); err == nil {
		t.Fatal("decision without why accepted")
	}
	if err := s.RecordDecision("x4", "build a wharf in Segaris", "closer to Terran economy"); err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	content, err := s.ReadFile("x4", "decisions.md")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(content, "build a wharf in Segaris") || !strings.Contains(content, "**Why:** closer to Terran economy") {
		t.Errorf("decisions.md missing entry:\n%s", content)
	}
}

func TestDigestComposition(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateExpert("x4", "Terran playthrough"); err != nil {
		t.Fatalf("CreateExpert: %v", err)
	}
	if err := s.ReplaceFile("x4", "goals.md", "# Goals\n\n- self-sufficient Terran economy"); err != nil {
		t.Fatalf("ReplaceFile: %v", err)
	}
	for i, d := range []string{"one", "two", "three", "four", "five", "six"} {
		if err := s.RecordDecision("x4", "decision "+d, "why "+d); err != nil {
			t.Fatalf("decision %d: %v", i, err)
		}
	}
	if err := s.AppendSessionLog("x4", "Captured first Xenon K."); err != nil {
		t.Fatalf("AppendSessionLog: %v", err)
	}
	if err := s.SetSessionContext("x4", "tonight: boarding ops practice"); err != nil {
		t.Fatalf("SetSessionContext: %v", err)
	}

	digest, err := s.Digest("x4")
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	for _, want := range []string{
		"# Memory digest: x4",
		"tonight: boarding ops practice",
		"self-sufficient Terran economy",
		"decision six",
		"Captured first Xenon K.",
	} {
		if !strings.Contains(digest, want) {
			t.Errorf("digest missing %q:\n%s", want, digest)
		}
	}
	// Only the last 5 decisions survive the digest cut.
	if strings.Contains(digest, "decision one") {
		t.Error("digest includes decision beyond the recent-5 window")
	}
}

func TestSessionContextTTL(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateExpert("x4", "d"); err != nil {
		t.Fatalf("CreateExpert: %v", err)
	}
	if err := s.SetSessionContext("x4", "boarding tonight"); err != nil {
		t.Fatalf("SetSessionContext: %v", err)
	}
	got, err := s.SessionContext("x4")
	if err != nil || got != "boarding tonight" {
		t.Fatalf("SessionContext = %q, %v", got, err)
	}
	// Age the clock past the TTL: stale scratch must not leak.
	s.now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	got, err = s.SessionContext("x4")
	if err != nil || got != "" {
		t.Errorf("expired SessionContext = %q, %v; want empty", got, err)
	}
}

func TestSearch(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateExpert("x4", "d"); err != nil {
		t.Fatalf("CreateExpert: %v", err)
	}
	if err := s.RecordNote("x4", "boarding", "Use three-star marines above 40% hull."); err != nil {
		t.Fatalf("RecordNote: %v", err)
	}
	hits, err := s.Search("x4", "MARINES")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].File != "notes/boarding.md" {
		t.Fatalf("hits = %+v, want one hit in notes/boarding.md", hits)
	}
	if _, err := s.Search("x4", "  "); err == nil {
		t.Error("empty query accepted")
	}
}

func TestExternalEditsCheckpointedSeparately(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateExpert("x4", "d"); err != nil {
		t.Fatalf("CreateExpert: %v", err)
	}
	before := commitCount(t, s)

	// A human edits goals.md directly (no tool involved)...
	goals := filepath.Join(s.root, "x4", "goals.md")
	if err := os.WriteFile(goals, []byte("# Goals\n\n- edited by hand\n"), 0o644); err != nil {
		t.Fatalf("hand edit: %v", err)
	}
	// ...then a tool write happens. The hand edit must land in its own
	// checkpoint commit, not be absorbed into the tool's commit.
	if err := s.RecordDecision("x4", "some decision", "some why"); err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	if got := commitCount(t, s); got != before+2 {
		t.Errorf("commits = %d, want %d (checkpoint + tool write)", got, before+2)
	}
	out, err := s.git("log", "--format=%s", "-n", "2")
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if !strings.Contains(out, "checkpoint external edits") {
		t.Errorf("log missing checkpoint commit:\n%s", out)
	}
}

func TestNoOpWriteMakesNoCommit(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateExpert("x4", "d"); err != nil {
		t.Fatalf("CreateExpert: %v", err)
	}
	if err := s.ReplaceFile("x4", "goals.md", "# Goals\n\n- same"); err != nil {
		t.Fatalf("first replace: %v", err)
	}
	before := commitCount(t, s)
	if err := s.ReplaceFile("x4", "goals.md", "# Goals\n\n- same"); err != nil {
		t.Fatalf("identical replace: %v", err)
	}
	if got := commitCount(t, s); got != before {
		t.Errorf("identical write created a commit (%d -> %d)", before, got)
	}
}

func TestMain(m *testing.M) {
	if _, err := exec.LookPath("git"); err != nil {
		os.Exit(0) // environment without git: nothing here is testable
	}
	os.Exit(m.Run())
}
