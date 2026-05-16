package vamp

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeRunDir creates a synthetic run dir layout under root for use in
// tests. rec, when non-nil, is marshaled to pipeline.json. When rec is
// nil the dir is left without a pipeline.json so the test exercises the
// fallback code paths (basename timestamp / mtime span).
//
// files maps relative paths -> content; intermediate directories are
// created automatically.
func writeRunDir(t *testing.T, root, base string, rec *RunRecord, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(root, base)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if rec != nil {
		data, err := json.MarshalIndent(rec, "", "  ")
		if err != nil {
			t.Fatalf("marshal rec: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "pipeline.json"), data, 0o644); err != nil {
			t.Fatalf("write pipeline.json: %v", err)
		}
	}
	for rel, content := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir parent of %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return dir
}

func TestSummarizeRun_WithPipelineJSON(t *testing.T) {
	root := t.TempDir()
	start := time.Date(2026, 5, 15, 12, 0, 0, 0, time.Local)
	end := start.Add(2*time.Minute + 30*time.Second)
	rec := &RunRecord{
		Name:      "demo",
		StartTime: start,
		EndTime:   end,
		Status:    "ok",
		Stages: []StageRecord{
			{ID: "a", DurationMS: 100, Status: "ok"},
			{ID: "b", DurationMS: 200, Status: "ok"},
		},
	}
	dir := writeRunDir(t, root, "2026-05-15T12-00-00_demo", rec, map[string]string{
		"out.md": "hello",
	})
	s, err := SummarizeRun(dir)
	if err != nil {
		t.Fatalf("SummarizeRun: %v", err)
	}
	if !s.HasRecord {
		t.Errorf("HasRecord = false, want true")
	}
	if s.Name != "demo" {
		t.Errorf("Name = %q, want demo", s.Name)
	}
	if s.NumStages != 2 {
		t.Errorf("NumStages = %d, want 2", s.NumStages)
	}
	if s.Duration != 2*time.Minute+30*time.Second {
		t.Errorf("Duration = %s, want 2m30s", s.Duration)
	}
	if s.Status != "ok" {
		t.Errorf("Status = %q, want ok", s.Status)
	}
	if !s.Timestamp.Equal(start) {
		t.Errorf("Timestamp = %s, want %s", s.Timestamp, start)
	}
	if s.SizeBytes < 5 {
		t.Errorf("SizeBytes = %d, want >= 5 (out.md + pipeline.json)", s.SizeBytes)
	}
}

func TestSummarizeRun_FallbacksWhenNoPipelineJSON(t *testing.T) {
	root := t.TempDir()
	dir := writeRunDir(t, root, "2026-05-15T08-30-15_legacy", nil, map[string]string{
		"inputs.json": "{}",
		"out.md":      "hi",
	})
	s, err := SummarizeRun(dir)
	if err != nil {
		t.Fatalf("SummarizeRun: %v", err)
	}
	if s.HasRecord {
		t.Errorf("HasRecord = true, want false (no pipeline.json)")
	}
	if s.Name != "legacy" {
		t.Errorf("Name = %q, want legacy (parsed from basename)", s.Name)
	}
	if s.NumStages != 0 {
		t.Errorf("NumStages = %d, want 0", s.NumStages)
	}
	if s.Status != "unknown" {
		t.Errorf("Status = %q, want unknown", s.Status)
	}
	want := time.Date(2026, 5, 15, 8, 30, 15, 0, time.Local)
	if !s.Timestamp.Equal(want) {
		t.Errorf("Timestamp = %s, want %s (from basename)", s.Timestamp, want)
	}
}

func TestSummarizeRun_UnparseableBasenameFallsBackToMtime(t *testing.T) {
	root := t.TempDir()
	dir := writeRunDir(t, root, "garbage_name_no_timestamp", nil, map[string]string{
		"x": "y",
	})
	s, err := SummarizeRun(dir)
	if err != nil {
		t.Fatalf("SummarizeRun: %v", err)
	}
	if s.Timestamp.IsZero() {
		t.Errorf("Timestamp is zero; expected mtime fallback")
	}
	if s.Name != "" {
		t.Errorf("Name = %q, want empty (unparseable basename)", s.Name)
	}
}

func TestListRuns_SortsNewestFirstAndSkipsFiles(t *testing.T) {
	root := t.TempDir()
	// Three runs, in deliberately-shuffled basename order.
	writeRunDir(t, root, "2026-05-15T10-00-00_a", &RunRecord{
		Name: "a", StartTime: time.Date(2026, 5, 15, 10, 0, 0, 0, time.Local),
	}, nil)
	writeRunDir(t, root, "2026-05-15T12-00-00_c", &RunRecord{
		Name: "c", StartTime: time.Date(2026, 5, 15, 12, 0, 0, 0, time.Local),
	}, nil)
	writeRunDir(t, root, "2026-05-15T11-00-00_b", &RunRecord{
		Name: "b", StartTime: time.Date(2026, 5, 15, 11, 0, 0, 0, time.Local),
	}, nil)
	// Stray file at the root: must be skipped (only dirs are runs).
	if err := os.WriteFile(filepath.Join(root, "stray.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	runs, err := ListRuns(root)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("len(runs) = %d, want 3", len(runs))
	}
	gotOrder := []string{runs[0].Name, runs[1].Name, runs[2].Name}
	wantOrder := []string{"c", "b", "a"}
	for i := range gotOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Errorf("runs[%d].Name = %q, want %q", i, gotOrder[i], wantOrder[i])
		}
	}
}

func TestListRuns_MissingDirReturnsNil(t *testing.T) {
	runs, err := ListRuns(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if runs != nil {
		t.Errorf("runs = %v, want nil for missing dir", runs)
	}
}

func TestFindRunByPrefix(t *testing.T) {
	root := t.TempDir()
	writeRunDir(t, root, "2026-05-15T10-00-00_alpha", &RunRecord{
		Name: "alpha", StartTime: time.Date(2026, 5, 15, 10, 0, 0, 0, time.Local),
	}, nil)
	writeRunDir(t, root, "2026-05-15T11-00-00_alpha2", &RunRecord{
		Name: "alpha2", StartTime: time.Date(2026, 5, 15, 11, 0, 0, 0, time.Local),
	}, nil)
	writeRunDir(t, root, "2026-05-15T12-00-00_beta", &RunRecord{
		Name: "beta", StartTime: time.Date(2026, 5, 15, 12, 0, 0, 0, time.Local),
	}, nil)

	t.Run("unique prefix", func(t *testing.T) {
		r, err := FindRunByPrefix(root, "2026-05-15T12")
		if err != nil {
			t.Fatalf("FindRunByPrefix: %v", err)
		}
		if r.Name != "beta" {
			t.Errorf("Name = %q, want beta", r.Name)
		}
	})

	t.Run("ambiguous prefix", func(t *testing.T) {
		_, err := FindRunByPrefix(root, "2026-05-15T1")
		if err == nil {
			t.Fatal("expected error")
		}
		var amb *AmbiguousError
		if !errors.As(err, &amb) {
			t.Fatalf("err = %v, want *AmbiguousError", err)
		}
		if len(amb.Candidates) != 3 {
			t.Errorf("Candidates = %v, want 3 entries", amb.Candidates)
		}
	})

	t.Run("not found", func(t *testing.T) {
		_, err := FindRunByPrefix(root, "9999")
		if err == nil {
			t.Fatal("expected error")
		}
		var nf *NotFoundError
		if !errors.As(err, &nf) {
			t.Fatalf("err = %v, want *NotFoundError", err)
		}
	})

	t.Run("path argument bypasses prefix matching", func(t *testing.T) {
		path := filepath.Join(root, "2026-05-15T10-00-00_alpha")
		r, err := FindRunByPrefix(root, path)
		if err != nil {
			t.Fatalf("FindRunByPrefix: %v", err)
		}
		if r.Path != path {
			t.Errorf("Path = %q, want %q", r.Path, path)
		}
	})
}

func TestCleanupRuns(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	old := writeRunDir(t, root, "2026-04-01T10-00-00_old", &RunRecord{
		Name: "old", StartTime: now.Add(-30 * 24 * time.Hour),
	}, nil)
	mid := writeRunDir(t, root, "2026-05-10T10-00-00_mid", &RunRecord{
		Name: "mid", StartTime: now.Add(-10 * 24 * time.Hour),
	}, nil)
	_ = writeRunDir(t, root, "2026-05-15T10-00-00_fresh", &RunRecord{
		Name: "fresh", StartTime: now.Add(-1 * time.Hour),
	}, nil)

	cases := []struct {
		name        string
		olderThan   time.Duration
		dryRun      bool
		wantRemoved []string
		wantSkipped int
	}{
		{
			name:        "dry-run 7d only removes the 30d-old run",
			olderThan:   7 * 24 * time.Hour,
			dryRun:      true,
			wantRemoved: []string{old, mid},
			wantSkipped: 1, // fresh
		},
		{
			name:        "60d cutoff removes nothing",
			olderThan:   60 * 24 * time.Hour,
			dryRun:      true,
			wantRemoved: nil,
			wantSkipped: 3,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := CleanupRuns(root, tc.olderThan, tc.dryRun)
			if err != nil {
				t.Fatalf("CleanupRuns: %v", err)
			}
			if len(res.Removed) != len(tc.wantRemoved) {
				t.Errorf("Removed = %v, want %v", res.Removed, tc.wantRemoved)
			}
			for _, want := range tc.wantRemoved {
				found := false
				for _, got := range res.Removed {
					if got == want {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected %s in Removed, got %v", want, res.Removed)
				}
			}
			if len(res.Skipped) != tc.wantSkipped {
				t.Errorf("Skipped = %d, want %d", len(res.Skipped), tc.wantSkipped)
			}
		})
	}

	t.Run("real removal", func(t *testing.T) {
		// Fresh setup so the dry-run cases above don't interfere.
		root2 := t.TempDir()
		victim := writeRunDir(t, root2, "2025-01-01T10-00-00_ancient", &RunRecord{
			Name: "ancient", StartTime: now.Add(-365 * 24 * time.Hour),
		}, nil)
		keeper := writeRunDir(t, root2, "2026-05-15T10-00-00_recent", &RunRecord{
			Name: "recent", StartTime: now.Add(-1 * time.Hour),
		}, nil)
		res, err := CleanupRuns(root2, 7*24*time.Hour, false)
		if err != nil {
			t.Fatalf("CleanupRuns: %v", err)
		}
		if len(res.Removed) != 1 || res.Removed[0] != victim {
			t.Errorf("Removed = %v, want [%s]", res.Removed, victim)
		}
		if _, err := os.Stat(victim); !os.IsNotExist(err) {
			t.Errorf("victim still exists: stat err = %v", err)
		}
		if _, err := os.Stat(keeper); err != nil {
			t.Errorf("keeper should still exist: %v", err)
		}
	})

	t.Run("rejects non-positive duration", func(t *testing.T) {
		_, err := CleanupRuns(root, 0, true)
		if err == nil {
			t.Fatal("expected error for zero duration")
		}
		_, err = CleanupRuns(root, -time.Hour, true)
		if err == nil {
			t.Fatal("expected error for negative duration")
		}
	})

	t.Run("rejects suspicious runs dir", func(t *testing.T) {
		for _, suspicious := range []string{"", "/"} {
			_, err := CleanupRuns(suspicious, time.Hour, true)
			if err == nil {
				t.Errorf("CleanupRuns(%q): expected error", suspicious)
			}
		}
	})

	t.Run("missing runs dir is a no-op", func(t *testing.T) {
		res, err := CleanupRuns(filepath.Join(t.TempDir(), "nope"), time.Hour, false)
		if err != nil {
			t.Fatalf("CleanupRuns: %v", err)
		}
		if len(res.Removed) != 0 || len(res.Skipped) != 0 || len(res.Errors) != 0 {
			t.Errorf("expected empty CleanupResult, got %+v", res)
		}
	})
}

func TestFormatSize(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0B"},
		{1023, "1023B"},
		{1024, "1.0KiB"},
		{1024 * 1024, "1.0MiB"},
		{int64(1.5 * 1024 * 1024), "1.5MiB"},
		{int64(2 * 1024 * 1024 * 1024), "2.0GiB"},
	}
	for _, tc := range cases {
		got := FormatSize(tc.n)
		if got != tc.want {
			t.Errorf("FormatSize(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "-"},
		{-1, "-"},
		{500 * time.Millisecond, "500ms"},
		{5 * time.Second, "5s"},
		{2*time.Minute + 3*time.Second, "2m03s"},
		{time.Hour + 2*time.Minute + 3*time.Second, "1h02m03s"},
	}
	for _, tc := range cases {
		got := FormatDuration(tc.d)
		if got != tc.want {
			t.Errorf("FormatDuration(%s) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestFormatTimestamp_ZeroAndNonZero(t *testing.T) {
	if got := FormatTimestamp(time.Time{}); got != "-" {
		t.Errorf("FormatTimestamp(zero) = %q, want \"-\"", got)
	}
	now := time.Now().Local()
	got := FormatTimestamp(now)
	// Loose check: should contain a year-month-day-prefixed string in the
	// configured layout. Avoid pinning the full string because it depends
	// on the local timezone.
	if len(got) != len("2006-01-02 15:04:05") {
		t.Errorf("FormatTimestamp(%s) = %q, length unexpected", now, got)
	}
}

func TestParseTimestampFromBasename(t *testing.T) {
	fallback := time.Date(2000, 1, 1, 0, 0, 0, 0, time.Local)
	cases := []struct {
		base string
		want time.Time
	}{
		{"2026-05-15T12-34-56_demo", time.Date(2026, 5, 15, 12, 34, 56, 0, time.Local)},
		{"2026-05-15T12-34-56", time.Date(2026, 5, 15, 12, 34, 56, 0, time.Local)},
		{"garbage", fallback},
		{"", fallback},
	}
	for _, tc := range cases {
		got := parseTimestampFromBasename(tc.base, fallback)
		if !got.Equal(tc.want) {
			t.Errorf("parseTimestampFromBasename(%q) = %s, want %s", tc.base, got, tc.want)
		}
	}
}

func TestParseNameFromBasename(t *testing.T) {
	cases := map[string]string{
		"2026-05-15T12-34-56_demo":        "demo",
		"2026-05-15T12-34-56_image_batch": "image_batch",
		"2026-05-15T12-34-56":             "",
		"not-a-timestamp_anything":        "",
		"":                                "",
	}
	for base, want := range cases {
		got := parseNameFromBasename(base)
		if got != want {
			t.Errorf("parseNameFromBasename(%q) = %q, want %q", base, got, want)
		}
	}
}

// Ensure AmbiguousError lists candidates in its message so the CLI's
// default printf will render something useful even without unwrapping.
func TestAmbiguousErrorMessage(t *testing.T) {
	e := &AmbiguousError{ID: "x", Candidates: []string{"a", "b"}}
	msg := e.Error()
	if !strings.Contains(msg, "a") || !strings.Contains(msg, "b") {
		t.Errorf("Error() = %q, want candidates in message", msg)
	}
}
