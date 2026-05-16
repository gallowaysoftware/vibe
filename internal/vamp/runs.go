package vamp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// RunRecord is the on-disk pipeline.json the executor writes at run end.
// It is intentionally small and stable: `vamp runs ls` and future tooling
// read it directly. Best-effort — Run() won't fail if writing this file
// fails, so consumers must tolerate its absence (fall back to mtime span).
type RunRecord struct {
	Name      string        `json:"name"`
	StartTime time.Time     `json:"start_time"`
	EndTime   time.Time     `json:"end_time"`
	Status    string        `json:"status"` // "ok" | "error" | "partial"
	Stages    []StageRecord `json:"stages"`
}

// StageRecord is one row in RunRecord.Stages, written for every stage the
// executor attempted (including skipped/resumed and failed stages).
type StageRecord struct {
	ID         string `json:"id"`
	DurationMS int64  `json:"duration_ms"`
	Status     string `json:"status"` // "ok" | "error" | "skipped"
}

// RunSummary is the in-memory view of one run directory used for listings.
// It is derived from a combination of (a) the run dir's basename, (b) the
// optional pipeline.json record (preferred for accuracy), and (c) on-disk
// stat data (mtime span, total size). Every field has a sensible fallback
// when pipeline.json is absent so very old run dirs still appear in `runs ls`.
type RunSummary struct {
	// ID is the run directory's basename (e.g. "2026-05-15T20-12-55_image_batch_smoke").
	// This is the prefix-matchable identifier accepted by `runs show` /
	// `runs cleanup`.
	ID string
	// Path is the absolute path to the run directory.
	Path string
	// Name is the pipeline name; comes from pipeline.json when present,
	// otherwise the suffix of the basename after the timestamp (or empty).
	Name string
	// Timestamp is the run's start time. Pulled from pipeline.json when
	// available, otherwise parsed from the directory basename, otherwise
	// the directory's mtime.
	Timestamp time.Time
	// NumStages is the number of stage entries in pipeline.json, or 0 when
	// the record is absent.
	NumStages int
	// Duration is end_time - start_time from pipeline.json when both are
	// non-zero, otherwise the span between the oldest and newest mtime in
	// the run dir. Zero if the dir is empty.
	Duration time.Duration
	// Status is the pipeline-level status from pipeline.json, or "unknown"
	// when absent.
	Status string
	// SizeBytes is the total on-disk size of every regular file under the
	// run dir (recursive). Best-effort: an unreadable subtree is silently
	// skipped so a permission glitch on one file doesn't hide the row.
	SizeBytes int64
	// HasRecord is true iff pipeline.json was successfully read and parsed.
	// Surfaced so the formatter can mark legacy runs distinctly if it
	// wants to; currently unused at the CLI layer.
	HasRecord bool
}

// runDirTimestampRE matches the leading "YYYY-MM-DDTHH-MM-SS" portion of a
// run directory basename. We don't try to validate the full grammar (the
// run dir may legitimately have arbitrary suffix characters after the
// underscore — pipeline name, etc.); we only want to lift the timestamp.
var runDirTimestampRE = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2})(?:_(.*))?$`)

// runDirLayout is the timestamp format used by `cmd_run.go` when minting
// new run-dir basenames. Centralized here so parser and writer stay in lockstep.
const runDirLayout = "2006-01-02T15-04-05"

// ListRuns reads every immediate subdirectory of runsDir and returns one
// RunSummary per dir, sorted newest-first by Timestamp. Non-directory
// entries are ignored. A missing runsDir returns (nil, nil) — the caller
// is expected to surface a "no runs yet" message rather than treating
// absence as an error (the dir is created lazily on first run).
//
// Per-dir errors (e.g. unreadable pipeline.json, permission denied on a
// subtree) degrade gracefully into the RunSummary defaults so one bad
// run dir doesn't poison the whole listing.
func ListRuns(runsDir string) ([]RunSummary, error) {
	entries, err := os.ReadDir(runsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read runs dir %s: %w", runsDir, err)
	}
	out := make([]RunSummary, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		s, err := SummarizeRun(filepath.Join(runsDir, e.Name()))
		if err != nil {
			// Skip rather than abort: the summarizer only returns a
			// non-nil error for outright broken state (e.g. the dir
			// vanished between ReadDir and Stat). The common cases —
			// missing pipeline.json, partially-written runs — all
			// produce a populated RunSummary with HasRecord=false.
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Timestamp.After(out[j].Timestamp)
	})
	return out, nil
}

// SummarizeRun inspects a single run directory and returns a RunSummary.
// Best-effort across the board: it will not error on a missing pipeline.json,
// an unreadable file inside the tree, or a basename that doesn't match the
// timestamp layout; each fallback is documented inline.
func SummarizeRun(path string) (RunSummary, error) {
	info, err := os.Stat(path)
	if err != nil {
		return RunSummary{}, fmt.Errorf("stat run dir %s: %w", path, err)
	}
	if !info.IsDir() {
		return RunSummary{}, fmt.Errorf("%s is not a directory", path)
	}
	s := RunSummary{
		ID:     filepath.Base(path),
		Path:   path,
		Status: "unknown",
	}
	// Try pipeline.json first; it's the canonical record.
	recPath := filepath.Join(path, "pipeline.json")
	if data, rerr := os.ReadFile(recPath); rerr == nil {
		var rec RunRecord
		if jerr := json.Unmarshal(data, &rec); jerr == nil {
			s.HasRecord = true
			s.Name = rec.Name
			s.Status = rec.Status
			s.NumStages = len(rec.Stages)
			if !rec.StartTime.IsZero() {
				s.Timestamp = rec.StartTime
			}
			if !rec.StartTime.IsZero() && !rec.EndTime.IsZero() {
				if d := rec.EndTime.Sub(rec.StartTime); d > 0 {
					s.Duration = d
				}
			}
		}
	}
	// Fill in fallbacks for fields pipeline.json didn't populate.
	if s.Timestamp.IsZero() {
		s.Timestamp = parseTimestampFromBasename(s.ID, info.ModTime())
	}
	if s.Name == "" {
		s.Name = parseNameFromBasename(s.ID)
	}
	// mtime span + total size walk the tree once. We absorb walk errors
	// silently so a permission glitch on one file doesn't lose the row;
	// the worst case is an undercount of size and a tighter mtime span.
	span, total := walkRunDir(path)
	s.SizeBytes = total
	if s.Duration == 0 && span > 0 {
		s.Duration = span
	}
	return s, nil
}

// walkRunDir returns (mtime span, total size in bytes) over every regular
// file beneath path. The mtime span is max(mtime) - min(mtime); when only
// one file is present (or all files share an mtime) it is zero, which the
// caller treats as "duration unknown" and the formatter renders as "-".
func walkRunDir(path string) (time.Duration, int64) {
	var (
		oldest time.Time
		newest time.Time
		total  int64
		any    bool
	)
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// Skip unreadable subtrees rather than aborting the walk.
			// Returning nil here continues the walk past the offender.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		total += info.Size()
		mt := info.ModTime()
		if !any {
			oldest = mt
			newest = mt
			any = true
		} else {
			if mt.Before(oldest) {
				oldest = mt
			}
			if mt.After(newest) {
				newest = mt
			}
		}
		return nil
	})
	if !any {
		return 0, 0
	}
	span := newest.Sub(oldest)
	if span < 0 {
		span = 0
	}
	return span, total
}

// parseTimestampFromBasename pulls the leading timestamp out of a run-dir
// basename. The expected layout is "YYYY-MM-DDTHH-MM-SS_<pipeline-name>"
// (see cmd_run.go's RunsDir / time.Format call). Unrecognized basenames
// fall back to the directory's mtime so cleanup-by-age still has a usable
// number rather than the Unix epoch.
func parseTimestampFromBasename(base string, fallback time.Time) time.Time {
	m := runDirTimestampRE.FindStringSubmatch(base)
	if m == nil {
		return fallback
	}
	// The run dir was minted with time.Now().Local().Format(runDirLayout),
	// so parse in local time to match. Anything that fails to parse falls
	// back to mtime; we don't want a malformed timestamp to silently sort
	// to the epoch.
	t, err := time.ParseInLocation(runDirLayout, m[1], time.Local)
	if err != nil {
		return fallback
	}
	return t
}

// parseNameFromBasename returns the suffix after the underscore separator
// in a basename like "2026-05-15T20-12-55_image_batch_smoke". Unrecognized
// basenames return "" so the formatter can show a placeholder.
func parseNameFromBasename(base string) string {
	m := runDirTimestampRE.FindStringSubmatch(base)
	if m == nil {
		return ""
	}
	return m[2]
}

// FindRunByPrefix looks up a run directory in runsDir by basename prefix.
// Behaviour:
//   - If id is an absolute or relative path that resolves to an existing
//     directory, it is returned verbatim (after Stat-ing it). This lets the
//     user pass a full run-dir path, useful for shell completion and for
//     run dirs outside the standard runsDir.
//   - Otherwise id is matched as a prefix against the basenames of every
//     immediate subdir of runsDir. Exactly one match: return its summary.
//     Zero matches: ErrRunNotFound. Multiple matches: ErrRunAmbiguous,
//     with the candidate IDs accessible via AmbiguousError.Candidates.
//
// Callers that want to render their own messages can use errors.As to
// pull out *AmbiguousError; the default Error() form lists candidates
// for direct printing.
func FindRunByPrefix(runsDir, id string) (RunSummary, error) {
	if id == "" {
		return RunSummary{}, fmt.Errorf("empty run id")
	}
	// Path-shaped argument: take it verbatim. We treat anything containing
	// a path separator as a path, since basenames don't include them.
	if strings.ContainsRune(id, os.PathSeparator) {
		abs, err := filepath.Abs(id)
		if err != nil {
			return RunSummary{}, err
		}
		return SummarizeRun(abs)
	}
	all, err := ListRuns(runsDir)
	if err != nil {
		return RunSummary{}, err
	}
	var matches []RunSummary
	for _, r := range all {
		if strings.HasPrefix(r.ID, id) {
			matches = append(matches, r)
		}
	}
	switch len(matches) {
	case 0:
		return RunSummary{}, &NotFoundError{ID: id, RunsDir: runsDir}
	case 1:
		return matches[0], nil
	default:
		cands := make([]string, len(matches))
		for i, m := range matches {
			cands[i] = m.ID
		}
		return RunSummary{}, &AmbiguousError{ID: id, Candidates: cands}
	}
}

// NotFoundError is returned by FindRunByPrefix when no run dir matches
// the prefix. RunsDir is included in the message so the user knows
// where we looked.
type NotFoundError struct {
	ID      string
	RunsDir string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("no run matches prefix %q under %s", e.ID, e.RunsDir)
}

// AmbiguousError is returned by FindRunByPrefix when more than one run dir
// matches the supplied prefix. Candidates lists every matching basename
// so the CLI can print them and bail.
type AmbiguousError struct {
	ID         string
	Candidates []string
}

func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("prefix %q matches %d runs: %s", e.ID, len(e.Candidates), strings.Join(e.Candidates, ", "))
}

// CleanupResult is returned by CleanupRuns and captures which run dirs were
// (or would have been, under DryRun) removed. The CLI uses Removed/Skipped
// to drive its summary line; Errors carries non-fatal per-dir RemoveAll
// failures so the user knows a manual cleanup is needed.
type CleanupResult struct {
	// Removed is the list of run dir paths that were deleted (or, under
	// DryRun, that would have been deleted). Sorted oldest-first so the
	// log reads naturally.
	Removed []string
	// Skipped is every run dir younger than the cutoff. The CLI prints
	// just the count, not the IDs.
	Skipped []string
	// Errors holds (run path, removal error) tuples for run dirs that
	// failed to delete. The cleanup is best-effort: one failure doesn't
	// stop the rest, so Removed/Errors may both be non-empty.
	Errors []CleanupError
}

// CleanupError pairs a run path with the RemoveAll error that prevented
// its deletion.
type CleanupError struct {
	Path string
	Err  error
}

func (c CleanupError) Error() string { return fmt.Sprintf("%s: %v", c.Path, c.Err) }

// CleanupRuns removes every run dir under runsDir whose Timestamp is older
// than now-olderThan. When dryRun is true the candidates are returned in
// CleanupResult.Removed without any filesystem changes.
//
// The "age" used here is RunSummary.Timestamp, which comes from pipeline.json
// when present and falls back to the basename timestamp / mtime. That makes
// cleanup well-defined for both new and legacy run dirs.
//
// Safety net: we refuse to act when runsDir is "", "/", or doesn't resolve
// to an existing directory. The intent is purely defensive — RemoveAll on
// "/" is catastrophic and we'd rather error than trust a misconfigured
// $XDG_STATE_HOME. The caller's runsDir is normally vamp.RunsDir(), which
// always nests two levels deep, so this only bites when someone wires
// CleanupRuns into a script with a bare env var.
func CleanupRuns(runsDir string, olderThan time.Duration, dryRun bool) (CleanupResult, error) {
	var res CleanupResult
	if olderThan <= 0 {
		return res, fmt.Errorf("--older-than must be positive (got %s)", olderThan)
	}
	clean := filepath.Clean(runsDir)
	if clean == "" || clean == "." || clean == "/" {
		return res, fmt.Errorf("refusing to clean up suspicious runs dir %q", runsDir)
	}
	info, err := os.Stat(clean)
	if errors.Is(err, fs.ErrNotExist) {
		// Nothing to do; surface the empty result so the CLI can print
		// a friendly "no runs to clean" rather than aborting.
		return res, nil
	}
	if err != nil {
		return res, fmt.Errorf("stat runs dir: %w", err)
	}
	if !info.IsDir() {
		return res, fmt.Errorf("runs dir %s is not a directory", clean)
	}
	all, err := ListRuns(clean)
	if err != nil {
		return res, err
	}
	cutoff := time.Now().Add(-olderThan)
	// Walk oldest-first so the Removed slice reads naturally in log output.
	// ListRuns sorts newest-first, so reverse-iterate.
	for i := len(all) - 1; i >= 0; i-- {
		r := all[i]
		if !r.Timestamp.Before(cutoff) {
			res.Skipped = append(res.Skipped, r.Path)
			continue
		}
		if dryRun {
			res.Removed = append(res.Removed, r.Path)
			continue
		}
		if rmErr := os.RemoveAll(r.Path); rmErr != nil {
			res.Errors = append(res.Errors, CleanupError{Path: r.Path, Err: rmErr})
			continue
		}
		res.Removed = append(res.Removed, r.Path)
	}
	return res, nil
}

// FormatSize renders a byte count using base-1024 binary units (KiB/MiB/GiB).
// We pick units so the rendered number is in [1.0, 1024.0) and print one
// decimal place for non-byte units to keep the column compact. Byte counts
// stay integers.
func FormatSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	suffixes := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	if exp >= len(suffixes) {
		exp = len(suffixes) - 1
	}
	return fmt.Sprintf("%.1f%s", float64(n)/float64(div), suffixes[exp])
}

// FormatDuration renders a duration as a compact human string. We deliberately
// avoid time.Duration.String()'s output for short runs (e.g. "1m3.4s") in
// favor of fixed-width "1m03s" so the column lines up. Zero durations render
// as "-" so the listing visually distinguishes "we don't know" from "0s".
func FormatDuration(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	total := int64(d.Seconds())
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// FormatTimestamp renders a run start time in local-time "YYYY-MM-DD HH:MM:SS"
// form. We choose space-separated rather than ISO-T so the column stays
// human-scannable without monospace tricks. Zero times render as "-".
func FormatTimestamp(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}
