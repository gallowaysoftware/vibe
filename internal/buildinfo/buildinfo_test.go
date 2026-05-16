package buildinfo

import "testing"

func TestString_DefaultsToDev(t *testing.T) {
	// Restore globals at the end so we don't pollute other tests.
	origVersion, origCommit, origDate := Version, Commit, Date
	t.Cleanup(func() { Version, Commit, Date = origVersion, origCommit, origDate })

	Version, Commit, Date = "dev", "", ""
	if got, want := String(), "dev"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestString_VersionWithCommitAndDate(t *testing.T) {
	origVersion, origCommit, origDate := Version, Commit, Date
	t.Cleanup(func() { Version, Commit, Date = origVersion, origCommit, origDate })

	Version, Commit, Date = "v1.2.3", "abc1234", "2026-05-16"
	if got, want := String(), "v1.2.3 (abc1234, 2026-05-16)"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestString_CommitOnly(t *testing.T) {
	origVersion, origCommit, origDate := Version, Commit, Date
	t.Cleanup(func() { Version, Commit, Date = origVersion, origCommit, origDate })

	Version, Commit, Date = "v1.2.3", "abc1234", ""
	if got, want := String(), "v1.2.3 (abc1234)"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestString_DateOnly(t *testing.T) {
	origVersion, origCommit, origDate := Version, Commit, Date
	t.Cleanup(func() { Version, Commit, Date = origVersion, origCommit, origDate })

	Version, Commit, Date = "v1.2.3", "", "2026-05-16"
	if got, want := String(), "v1.2.3 (2026-05-16)"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
