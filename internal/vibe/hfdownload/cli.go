package hfdownload

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// hfCLIName is the modern huggingface_hub command. The older `huggingface-cli`
// is deprecated and refuses to work in recent huggingface_hub versions.
const hfCLIName = "hf"

// hfXetHighPerfMinRAMGiB is the RAM floor below which we do NOT auto-enable Xet
// high-performance mode. HF recommends it for high-bandwidth boxes (~64GB+); on
// smaller machines the bumped concurrency/buffers can DEGRADE throughput, so we
// gate on it. Vibe hosts are GPU workstations, which clear this comfortably.
const hfXetHighPerfMinRAMGiB = 48

// hfDownloadEnv builds the environment for the `hf` download subprocess. It
// inherits the daemon env (so HF_TOKEN / stored `hf auth` creds and any operator
// HF_XET_* tuning flow through), and on a box with enough RAM opts into Xet
// high-performance transfer.
//
// Xet itself — parallel content-defined chunking with global dedup — is the fast
// path for large GGUFs (observed ~60+ MB/s vs ~1-2 MB/s on the throttled legacy
// HTTP path) and is ON BY DEFAULT when the `hf_xet` package is present (bundled
// with huggingface_hub >= 0.32). HF_XET_HIGH_PERFORMANCE additionally raises
// concurrency/buffer bounds and is only worthwhile on high-RAM machines, so we
// gate it and never override an operator who set it explicitly. The deprecated
// HF_HUB_ENABLE_HF_TRANSFER is a no-op in current huggingface_hub and is
// intentionally NOT set.
func hfDownloadEnv() []string {
	env := os.Environ()
	if _, set := os.LookupEnv("HF_XET_HIGH_PERFORMANCE"); !set && systemRAMGiB() >= hfXetHighPerfMinRAMGiB {
		env = append(env, "HF_XET_HIGH_PERFORMANCE=1")
	}
	return env
}

// systemRAMGiB returns total system memory in GiB on Linux, or 0 if it can't be
// determined (so high-performance mode stays off by default off-Linux / on error).
func systemRAMGiB() float64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			if kb, err := strconv.ParseFloat(fields[1], 64); err == nil {
				return kb / 1024 / 1024
			}
		}
	}
	return 0
}

// LookupHFCLI returns the absolute path to the `hf` binary if it's on $PATH.
func LookupHFCLI() (string, bool) {
	path, err := exec.LookPath(hfCLIName)
	if err != nil {
		return "", false
	}
	return path, true
}

// downloadViaHFCLI runs `hf download <repo> <file> --local-dir <dir>` and
// reports progress by polling the destination file size while the subprocess
// runs. The CLI's stdout/stderr is captured so failures (e.g. not logged in
// for a gated repo) bubble up with the real error message.
func downloadViaHFCLI(ctx context.Context, hfBin string, spec Spec, destPath string, progress ProgressFunc) error {
	targetDir := filepath.Dir(destPath)
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return fmt.Errorf("mkdir for %s: %w", destPath, err)
	}

	// Try HEAD for the total size; with stored hf credentials it'll often
	// succeed for gated repos via HF_TOKEN, but if it doesn't, we just report
	// progress without a known total (total = -1).
	total, _ := HeadSize(ctx, spec)

	// Snapshot files already in the target dir so progress tracks only the NEW
	// download. The target dir is shared — a model's mmproj/drafter download into
	// the same dir as the (much larger) main weights — so reporting the dir's
	// largest file would show the pre-existing model, not this download.
	baseline := fileSizes(targetDir)

	args := []string{"download", spec.Repo, spec.File, "--local-dir", targetDir}
	if spec.Revision != "" {
		args = append(args, "--revision", spec.Revision)
	}
	cmd := exec.CommandContext(ctx, hfBin, args...)
	cmd.Env = hfDownloadEnv()
	// Capture stderr so error messages (e.g. "Not logged in") survive.
	var stderr bytes.Buffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr

	slog.Info("hf download", "bin", hfBin, "repo", spec.Repo, "file", spec.File, "local_dir", targetDir)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start hf: %w", err)
	}

	hfWritePath := filepath.Join(targetDir, spec.File)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				// hf writes to a `.incomplete` sibling (or a `.cache/`
				// subdir) until the download finishes, so stat'ing the
				// final filename returns "not exist" the entire time
				// and no progress is reported. Walk the target dir for
				// the largest file size instead — covers both the
				// in-flight `.incomplete` file and the final renamed
				// one without caring which layout `hf` is using.
				if n := largestNewFileSize(targetDir, baseline); n > 0 {
					report(progress, n, total)
				} else if info, err := os.Stat(hfWritePath); err == nil {
					report(progress, info.Size(), total)
				}
			}
		}
	}()

	waitErr := cmd.Wait()
	close(stop)
	<-done

	// Don't trust the exit code alone. The modern `hf` CLI can exit NON-ZERO with
	// a Python traceback even on a SUCCESSFUL download — a typer/click `Exit(0)`
	// goes uncaught on Python 3.14 ("click.exceptions.Exit: 0"), so the process
	// exits 1 after the bytes are safely on disk. hf only renames the blob to its
	// FINAL name on completion, so a present, correctly-sized final file means the
	// download succeeded regardless of exit code. Only surface the CLI error when
	// the file is actually missing or short.
	complete := func(path string) bool {
		info, err := os.Stat(path)
		if err != nil || info.Size() == 0 {
			return false
		}
		if total > 0 {
			return info.Size() == total
		}
		return true // final-named file present + non-empty, total unknown
	}
	if waitErr != nil && !complete(hfWritePath) && !complete(destPath) {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = waitErr.Error()
		}
		return fmt.Errorf("hf download %s/%s: %s", spec.Repo, spec.File, msg)
	}
	if waitErr != nil {
		slog.Warn("hf CLI exited non-zero but the file is complete — treating as success (likely the typer Exit(0) traceback on Python 3.14)",
			"repo", spec.Repo, "file", spec.File, "err", waitErr)
	}

	// hf places the file at <local_dir>/<file>. If the profile's destPath has a
	// different basename (or subdir, e.g. MTP/<file>), rename it. Skip when the
	// file is already at destPath (e.g. a prior partial run left it there).
	if hfWritePath != destPath {
		if _, err := os.Stat(hfWritePath); err == nil {
			if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
				return fmt.Errorf("mkdir for %s: %w", destPath, err)
			}
			if err := os.Rename(hfWritePath, destPath); err != nil {
				return fmt.Errorf("rename %s -> %s: %w", hfWritePath, destPath, err)
			}
		}
	}

	if info, err := os.Stat(destPath); err == nil {
		size := info.Size()
		report(progress, size, size)
	}
	return nil
}

// fileSizes returns a path->size map of every regular file under root, used to
// snapshot what's already present before a download so progress can ignore it.
// Best-effort: a missing root (download not started) yields an empty map.
func fileSizes(root string) map[string]int64 {
	m := map[string]int64{}
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		m[path] = info.Size()
		return nil
	})
	return m
}

// largestNewFileSize returns the size of the largest regular file under root
// that is NOT in baseline at the same size — i.e. a file this download created
// or grew. Used by the hf-CLI polling path to track the in-flight blob (and the
// final renamed file) while ignoring unrelated files already in the shared
// target dir (notably the main model weights sitting beside an mmproj/drafter
// download). Walk errors are best-effort: a missing dir at start-of-download is
// normal and the next tick retries.
func largestNewFileSize(root string, baseline map[string]int64) int64 {
	var max int64
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if bs, ok := baseline[path]; ok && bs == info.Size() {
			return nil // unchanged pre-existing file — not part of this download
		}
		if info.Size() > max {
			max = info.Size()
		}
		return nil
	})
	return max
}
