package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
)

// startProgressTail follows the daemon's slog JSON log file and renders
// human-friendly progress lines for the events emitted during a profile
// start. It runs until the returned cancel func is called — `vibe start`
// kicks it off just before the Start RPC and cancels it once the RPC
// returns (either successfully or with an error).
//
// The tail starts at the END of the log file (so we don't replay history)
// and reads line-buffered JSON records. Each record is matched against a
// small allow-list of msg values; unknown events are silently dropped.
// We deliberately avoid streaming raw daemon log lines to the user — the
// JSON shape is noisy and contains internal details (file paths, pids).
//
// Polls at 200ms — fast enough to feel live, slow enough to not show up
// as CPU usage. The poll loop tolerates log rotation / truncation by
// seeking back to the new end whenever it sees a smaller file size.
func startProgressTail(out io.Writer) (cancel func()) {
	logPath := filepath.Join(paths.StateHome(), "daemon.log")
	// If the log file doesn't exist (fresh daemon, never logged), we
	// still want the tail to attach when it shows up — but we don't
	// want to spam errors meanwhile. Open lazily inside the goroutine.
	ctx, cancelFn := context.WithCancel(context.Background())
	go tailDaemonLog(ctx, logPath, out)
	return cancelFn
}

// tailDaemonLog is the worker for startProgressTail. Separated so tests
// can drive it against a temp file without going through paths.LogDir.
func tailDaemonLog(ctx context.Context, logPath string, out io.Writer) {
	var f *os.File
	defer func() {
		if f != nil {
			_ = f.Close()
		}
	}()

	// Open + seek-to-end. Retry on miss; the daemon may be just-spawned
	// and the file might lag by a few ms.
	for {
		if ctx.Err() != nil {
			return
		}
		var err error
		f, err = os.Open(logPath)
		if err == nil {
			_, _ = f.Seek(0, io.SeekEnd)
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}

	reader := bufio.NewReader(f)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		// Drain whatever lines are available right now, then wait.
		for {
			line, err := reader.ReadString('\n')
			if err == io.EOF {
				if line != "" {
					// Partial line — push it back. Seek back so the
					// next read sees the same bytes once they're
					// complete. We seek by len(line) negative.
					_, _ = f.Seek(-int64(len(line)), io.SeekCurrent)
				}
				break
			}
			if err != nil {
				return
			}
			renderDaemonEvent(strings.TrimRight(line, "\n"), out)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Loop.
		}
	}
}

// renderDaemonEvent decodes a JSON daemon-log line and prints a friendly
// progress line for known msg values. Anything we don't recognise is
// silently dropped — the goal is high-signal progress, not log tailing.
func renderDaemonEvent(line string, out io.Writer) {
	type event struct {
		Msg          string  `json:"msg"`
		Profile      string  `json:"profile"`
		EstimatedGiB float64 `json:"estimated_gib"`
		FreeGiB      float64 `json:"free_gib"`
		Binary       string  `json:"binary"`
		Addr         string  `json:"addr"`
		Elapsed      int64   `json:"elapsed"` // nanoseconds
		URL          string  `json:"url"`
		Project      string  `json:"project"`
		Alias        string  `json:"alias"`
		ModelFile    string  `json:"model_file"`
	}
	var e event
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		return
	}
	switch e.Msg {
	case "vram pre-flight ok":
		fmt.Fprintf(out, "  vram ok (%.1f GiB free, %.1f GiB estimated)\n", e.FreeGiB, e.EstimatedGiB)
	case "vram pre-flight skipped":
		fmt.Fprintln(out, "  vram check skipped (probe unavailable)")
	case "starting profile (llama_server)":
		switch {
		case e.Alias != "" && e.ModelFile != "":
			fmt.Fprintf(out, "  loading %s (%s) into llama-server...\n", e.Alias, e.ModelFile)
		case e.Alias != "":
			fmt.Fprintf(out, "  loading %s into llama-server...\n", e.Alias)
		default:
			fmt.Fprintln(out, "  loading model into llama-server...")
		}
	case "starting profile (comfyui)":
		fmt.Fprintln(out, "  starting ComfyUI...")
	case "backend ready":
		fmt.Fprintf(out, "  backend ready in %.1fs\n", float64(e.Elapsed)/1e9)
	case "frontend compose: up":
		fmt.Fprintf(out, "  starting compose stack (%s)...\n", e.Project)
	case "frontend compose: up done":
		fmt.Fprintln(out, "  compose containers up")
	case "frontend compose: waiting for ready":
		fmt.Fprintln(out, "  waiting for frontend to answer health checks...")
	case "frontend compose: wait_for ready":
		fmt.Fprintf(out, "  frontend up: %s\n", e.URL)
	case "frontend managed: starting":
		fmt.Fprintf(out, "  starting %s...\n", e.Binary)
	}
}
