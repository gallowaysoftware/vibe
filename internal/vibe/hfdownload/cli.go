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
	"strings"
	"time"
)

// hfCLIName is the modern huggingface_hub command. The older `huggingface-cli`
// is deprecated and refuses to work in recent huggingface_hub versions.
const hfCLIName = "hf"

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

	args := []string{"download", spec.Repo, spec.File, "--local-dir", targetDir}
	if spec.Revision != "" {
		args = append(args, "--revision", spec.Revision)
	}
	cmd := exec.CommandContext(ctx, hfBin, args...)
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
				if n := largestFileSize(targetDir); n > 0 {
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

	if waitErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = waitErr.Error()
		}
		return fmt.Errorf("hf download %s/%s: %s", spec.Repo, spec.File, msg)
	}

	// hf places the file at <local_dir>/<file>. If the profile's destPath has a
	// different basename, rename it.
	if hfWritePath != destPath {
		if err := os.Rename(hfWritePath, destPath); err != nil {
			return fmt.Errorf("rename %s -> %s: %w", hfWritePath, destPath, err)
		}
	}

	if info, err := os.Stat(destPath); err == nil {
		size := info.Size()
		report(progress, size, size)
	}
	return nil
}

// largestFileSize returns the size of the largest regular file under root
// (recursively), or 0 if the walk finds nothing. Used by the hf-CLI polling
// path to track in-flight `.incomplete` files that the modern hf client
// writes alongside the final destination filename. Walk errors are best-
// effort: missing-directory at start-of-download is normal, and the next
// tick will retry.
func largestFileSize(root string) int64 {
	var max int64
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// Skip unreadable entries instead of bailing — partially-
			// created subdirs are normal mid-download.
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if info.Size() > max {
			max = info.Size()
		}
		return nil
	})
	return max
}
