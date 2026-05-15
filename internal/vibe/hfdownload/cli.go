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
				if info, err := os.Stat(hfWritePath); err == nil {
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
