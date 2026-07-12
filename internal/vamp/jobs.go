package vamp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// PidFileName is the basename of the per-run "alive" marker that
// `vamp run --detach` writes inside the run dir. Its presence + an alive
// process at the recorded pid is the signal that a background `vamp`
// worker is still executing the pipeline. The file is removed by the
// worker on clean exit; consumers must therefore treat "file present but
// process gone" as a crashed-worker state, not a live job.
const PidFileName = "vamp.pid"

// LogFileName is the basename of the per-run combined log that
// `vamp run --detach` redirects the worker's stdout/stderr to. `vamp logs`
// reads (and optionally tails) this file.
const LogFileName = "vamp.log"

// JobState is the lifecycle bucket a job currently sits in. It is derived
// from a combination of the on-disk PidFile presence, whether that pid is
// still alive, and the pipeline.json record's status field.
type JobState string

const (
	// JobStateRunning means PidFile exists and the recorded pid is alive.
	JobStateRunning JobState = "running"
	// JobStateFinished means the worker exited cleanly and pipeline.json
	// has a terminal status ("ok", "error", "partial", or "canceled").
	JobStateFinished JobState = "finished"
	// JobStateCrashed means PidFile exists but the recorded pid is no
	// longer alive AND pipeline.json hasn't been written (or is missing
	// the status field). The worker died before flushing its terminal
	// record; the run dir is effectively orphaned.
	JobStateCrashed JobState = "crashed"
	// JobStateUnknown is the catch-all for run dirs that contain neither
	// a PidFile nor a pipeline.json — typically a foreground `vamp run`
	// pre-detach. We surface them so `jobs ls` is a strict superset of
	// `runs ls`'s pipeline-launched entries; downstream tooling decides
	// whether to filter.
	JobStateUnknown JobState = "unknown"
)

// JobSummary is the per-job view returned by ListJobs and FindJobByPrefix.
// It composes RunSummary (which already encapsulates the pipeline.json /
// basename / mtime fallback hierarchy used by `runs ls`) with the daemon-
// mode-specific fields: process state, pid, and a precomputed log path.
//
// We deliberately embed RunSummary rather than re-deriving its fields here
// so the listing columns shared with `runs ls` (timestamp, name, stages,
// duration, size, status) stay in lockstep with the existing run-history
// formatter.
type JobSummary struct {
	RunSummary
	// State is the lifecycle bucket; see JobState constants for semantics.
	State JobState
	// PID is the recorded worker pid (zero when no PidFile is present).
	// Even when State is Finished/Crashed the value is retained so `jobs
	// show` can report the historical worker pid.
	PID int
	// LogPath is the absolute path to vamp.log inside the run dir. Always
	// set (even if the file doesn't exist yet) so callers can hand it
	// straight to TailLogs.
	LogPath string
}

// ListJobs enumerates every run dir under runsDir and returns the
// daemon-mode view (alive vs finished vs crashed) for each. Unlike
// ListRuns, ListJobs filters down to dirs that have *at least one* of:
// a PidFile (i.e. a `vamp run --detach` was started), a vamp.log file
// (i.e. a detached worker has begun writing output), or a pipeline.json
// (i.e. the run completed at least once). That filter keeps the listing
// focused on jobs the daemon-mode tooling actually owns and avoids
// surfacing legacy/foreground-only run dirs as bogus "unknown" rows.
//
// Sorted newest-first by RunSummary.Timestamp, matching ListRuns.
func ListJobs(runsDir string) ([]JobSummary, error) {
	entries, err := os.ReadDir(runsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read runs dir %s: %w", runsDir, err)
	}
	out := make([]JobSummary, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(runsDir, e.Name())
		// Cheap pre-filter: only dirs that contain at least one
		// daemon-mode marker are considered "jobs". This keeps `jobs
		// ls` from getting flooded with pre-daemon-mode foreground
		// runs.
		hasPid := fileExists(filepath.Join(dir, PidFileName))
		hasLog := fileExists(filepath.Join(dir, LogFileName))
		hasJSON := fileExists(filepath.Join(dir, "pipeline.json"))
		if !hasPid && !hasLog && !hasJSON {
			continue
		}
		js, err := summarizeJob(dir)
		if err != nil {
			continue
		}
		out = append(out, js)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Timestamp.After(out[j].Timestamp)
	})
	return out, nil
}

// summarizeJob builds a JobSummary for a single run dir. It reuses
// SummarizeRun so the embedded RunSummary's fields populate the same way
// `runs ls` does, then overlays the daemon-mode state derived from the
// PidFile.
func summarizeJob(dir string) (JobSummary, error) {
	rs, err := SummarizeRun(dir)
	if err != nil {
		return JobSummary{}, err
	}
	js := JobSummary{
		RunSummary: rs,
		LogPath:    filepath.Join(dir, LogFileName),
		State:      JobStateUnknown,
	}
	pid, pidErr := readPidFile(filepath.Join(dir, PidFileName))
	switch {
	case pidErr == nil && IsAlive(pid):
		js.PID = pid
		js.State = JobStateRunning
	case pidErr == nil:
		// PidFile present but pid dead. Two paths:
		//   - pipeline.json with a terminal status → clean exit, the
		//     PidFile is stale (e.g. crash AFTER terminal write before
		//     the defer cleanup ran). Treat as Finished.
		//   - otherwise → Crashed (worker died mid-run).
		js.PID = pid
		if rs.HasRecord && rs.Status != "" && rs.Status != "unknown" {
			js.State = JobStateFinished
		} else {
			js.State = JobStateCrashed
		}
	case errors.Is(pidErr, fs.ErrNotExist):
		// No PidFile. If pipeline.json has a terminal status, the
		// worker finished and cleaned up after itself.
		if rs.HasRecord && rs.Status != "" && rs.Status != "unknown" {
			js.State = JobStateFinished
		}
	default:
		// Unreadable PidFile (permission, garbage contents). We don't
		// have enough signal to claim Running; if pipeline.json says
		// the run finished, prefer Finished.
		if rs.HasRecord && rs.Status != "" && rs.Status != "unknown" {
			js.State = JobStateFinished
		}
	}
	return js, nil
}

// FindJobByPrefix is the job-flavoured sibling of FindRunByPrefix. The
// matching grammar is identical: a path-shaped id is taken verbatim,
// otherwise the basename is prefix-matched under runsDir.
func FindJobByPrefix(runsDir, id string) (JobSummary, error) {
	if id == "" {
		return JobSummary{}, fmt.Errorf("empty job id")
	}
	if strings.ContainsRune(id, os.PathSeparator) {
		abs, err := filepath.Abs(id)
		if err != nil {
			return JobSummary{}, err
		}
		return summarizeJob(abs)
	}
	all, err := ListJobs(runsDir)
	if err != nil {
		return JobSummary{}, err
	}
	var matches []JobSummary
	for _, j := range all {
		if strings.HasPrefix(j.ID, id) {
			matches = append(matches, j)
		}
	}
	switch len(matches) {
	case 0:
		return JobSummary{}, &NotFoundError{ID: id, RunsDir: runsDir}
	case 1:
		return matches[0], nil
	default:
		cands := make([]string, len(matches))
		for i, m := range matches {
			cands[i] = m.ID
		}
		return JobSummary{}, &AmbiguousError{ID: id, Candidates: cands}
	}
}

// IsAlive reports whether the given pid refers to a live process. On Unix
// this is implemented via `kill(pid, 0)`: a zero signal performs the
// permission/existence check without actually delivering a signal. The
// caveats are:
//   - pid <= 0 is treated as not alive (matches `man 2 kill`'s group-send
//     semantics, which we never want here).
//   - ESRCH ("no such process") → not alive.
//   - EPERM means the process exists but we can't signal it. We still
//     return true: existence is what callers actually care about, not
//     signal authority. This matters when a vamp worker is owned by a
//     different uid than the inspector (e.g. root-owned cron run vs.
//     user `jobs ls`).
func IsAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EPERM) {
		return true
	}
	return false
}

// CancelJob sends SIGTERM to the worker process recorded in the job's
// PidFile. Behaviour:
//   - State must be JobStateRunning; cancelling a finished/crashed/unknown
//     job returns ErrJobNotRunning.
//   - SIGTERM is the right signal: vamp's RunE wires signal.NotifyContext
//     to cancel the executor's context on SIGTERM/SIGINT, and the
//     executor's deferred writePipelineJSON renders that as
//     status="canceled" in the run record.
//   - We do NOT remove the PidFile here. The worker's deferred cleanup is
//     responsible for that; if it crashes before reaching the defer, the
//     next `jobs ls` will surface JobStateCrashed.
func CancelJob(j JobSummary) error {
	if j.State != JobStateRunning {
		return ErrJobNotRunning
	}
	if j.PID <= 0 {
		return ErrJobNotRunning
	}
	p, err := os.FindProcess(j.PID)
	if err != nil {
		return fmt.Errorf("locate process %d: %w", j.PID, err)
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal pid %d: %w", j.PID, err)
	}
	return nil
}

// ErrJobNotRunning is returned by CancelJob when the job's state isn't
// JobStateRunning (e.g. it already finished, crashed, or never had a
// recorded pid). The CLI distinguishes this from a real signal failure so
// it can print a friendly message rather than a wrapped errno.
var ErrJobNotRunning = errors.New("job is not running")

// readPidFile parses a vamp.pid file. The file is a single integer (no
// trailing newline required; we trim whitespace). Errors are surfaced
// distinctly so summarizeJob can branch on fs.ErrNotExist vs malformed
// contents — they mean different things for state derivation.
func readPidFile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return 0, fmt.Errorf("empty pid file %s", path)
	}
	pid, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("parse pid file %s: %w", path, err)
	}
	if pid <= 0 {
		return 0, fmt.Errorf("non-positive pid %d in %s", pid, path)
	}
	return pid, nil
}

// WritePidFile writes pid to <runDir>/vamp.pid atomically. The worker
// process calls this from its detach entry point; the foreground (parent)
// spawner does not — we want the pid file to reflect the actual long-
// lived process so callers' kill -0 / SIGTERM target the right pid.
func WritePidFile(runDir string, pid int) error {
	path := filepath.Join(runDir, PidFileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		return fmt.Errorf("write pid file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename pid file: %w", err)
	}
	return nil
}

// RemovePidFile deletes <runDir>/vamp.pid. The worker calls this from its
// terminal defer so `IsAlive` based on the pid stops returning true even
// if some other process happens to recycle the pid. Best-effort: an
// ENOENT or permission error is logged by the caller but never propagated.
func RemovePidFile(runDir string) error {
	return os.Remove(filepath.Join(runDir, PidFileName))
}

// fileExists is a small Stat wrapper that returns true iff path exists
// AND is not a directory. We don't care about the file's mode beyond
// "is it a regular file (or symlink to one)"; an unreadable file still
// counts as existing for the purpose of the daemon-mode filter.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

// TailLogs streams the log at path to w. When follow is false this is a
// straight `cat` — the file is opened, copied to w, closed. When follow
// is true we seek to end, then poll for new bytes at the configured
// interval, exiting when either:
//   - ctx is cancelled, or
//   - stopWhen returns true at the end of a poll cycle (intended use:
//     check whether the worker pid is still alive; once it's gone, drain
//     any remaining bytes and return).
//
// pollInterval defaults to 200ms when zero — matches `tail -f`'s default
// sleep interval and is short enough to feel live without burning CPU.
//
// The "drain after stopWhen" semantics matter: a worker may write its
// final timing-table rows and then exit; we want those bytes in the
// output even though the process is already gone by the time the next
// poll fires. After stopWhen says stop, we do one more read to suck up
// the tail.
func TailLogs(ctx context.Context, w io.Writer, path string, follow bool, pollInterval time.Duration, stopWhen func() bool) error {
	if !follow {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(w, f)
		return err
	}
	if pollInterval <= 0 {
		pollInterval = 200 * time.Millisecond
	}
	// Open with retry: the log file may not exist yet when the user runs
	// `vamp logs <id> -f` immediately after `--detach` returns and the
	// worker hasn't created vamp.log yet. Poll until it appears, gated
	// by ctx cancellation.
	var f *os.File
	for {
		var err error
		f, err = os.Open(path)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
	defer f.Close()
	// Stream existing content first (so `-f` shows what was already
	// written, like `tail -f -n +1`). After that, switch to polling.
	// io.Copy returns nil at EOF on a regular file, leaving the file
	// position at end-of-file — exactly where we want subsequent
	// poll-mode reads to resume.
	if _, err := io.Copy(w, f); err != nil {
		return err
	}
	buf := make([]byte, 4096)
	for {
		// Drain whatever has been appended since the last cycle. A
		// regular file returns io.EOF when the read position has
		// caught up; we treat that as "wait for more" rather than a
		// terminal error.
		for {
			n, rerr := f.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return werr
				}
			}
			if rerr == io.EOF || (n == 0 && rerr == nil) {
				break
			}
			if rerr != nil {
				return rerr
			}
		}
		if stopWhen != nil && stopWhen() {
			// One final drain to capture any tail bytes flushed
			// between our last read and the stopWhen check. The
			// worker may have written its terminal timing table
			// after we last read and before we observed the pid
			// going away.
			for {
				n, rerr := f.Read(buf)
				if n > 0 {
					if _, werr := w.Write(buf[:n]); werr != nil {
						return werr
					}
				}
				if rerr == io.EOF || (n == 0 && rerr == nil) {
					break
				}
				if rerr != nil {
					return rerr
				}
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// JobStateFor derives the JobState from an already-summarized run plus a
// cheap pid-file read — without re-walking the run dir's file tree the way
// summarizeJob (via SummarizeRun) does. `vamp runs ls` calls
// this once per run it already listed, so the listing stays O(runs) stats
// instead of O(files) on every invocation. The state matrix matches
// summarizeJob exactly; keep the two in lockstep.
func JobStateFor(rs RunSummary) JobState {
	terminal := rs.HasRecord && rs.Status != "" && rs.Status != "unknown"
	pid, pidErr := readPidFile(filepath.Join(rs.Path, PidFileName))
	switch {
	case pidErr == nil && IsAlive(pid):
		return JobStateRunning
	case pidErr == nil:
		if terminal {
			return JobStateFinished
		}
		return JobStateCrashed
	default:
		// No pid file, or an unreadable one: a terminal record means the
		// worker finished and cleaned up; otherwise we can't claim more
		// than unknown.
		if terminal {
			return JobStateFinished
		}
		return JobStateUnknown
	}
}
