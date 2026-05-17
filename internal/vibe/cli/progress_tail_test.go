package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// safeBuffer wraps bytes.Buffer with a mutex so a tail goroutine can
// Write to it while the test goroutine reads — bytes.Buffer is not
// safe for concurrent use.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func TestRenderDaemonEvent(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		{
			name: "vram ok",
			line: `{"time":"x","level":"INFO","msg":"vram pre-flight ok","profile":"chat","estimated_gib":28,"free_gib":29.6}`,
			want: "vram ok (29.6 GiB free, 28.0 GiB estimated)",
		},
		{
			name: "starting llama_server",
			line: `{"msg":"starting profile (llama_server)","profile":"chat"}`,
			want: "loading model into llama-server",
		},
		{
			name: "backend ready",
			line: `{"msg":"backend ready","addr":"http://127.0.0.1:1234","elapsed":2500000000}`,
			want: "backend ready in 2.5s",
		},
		{
			name: "compose up",
			line: `{"msg":"frontend compose: up","profile":"chat","project":"vibe-chat"}`,
			want: "starting compose stack (vibe-chat)",
		},
		{
			name: "wait_for ready",
			line: `{"msg":"frontend compose: wait_for ready","url":"http://127.0.0.1:8080/"}`,
			want: "frontend up: http://127.0.0.1:8080/",
		},
		{
			name: "managed starting",
			line: `{"msg":"frontend managed: starting","binary":"/home/x/opencode"}`,
			want: "starting /home/x/opencode",
		},
		{
			name: "unknown event silently dropped",
			line: `{"msg":"daemon ready"}`,
			want: "",
		},
		{
			name: "garbage line silently dropped",
			line: "not json",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			renderDaemonEvent(tc.line, &buf)
			got := strings.TrimSpace(buf.String())
			if tc.want == "" {
				if got != "" {
					t.Errorf("expected empty output, got %q", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("output %q does not contain %q", got, tc.want)
			}
		})
	}
}

// TestTailDaemonLog_TailsAppendedLines covers the live behavior:
// the tail opens a file, sits at EOF, and emits a friendly line as
// soon as a new daemon-log record is appended.
func TestTailDaemonLog_TailsAppendedLines(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "daemon.log")
	if err := os.WriteFile(logPath, []byte("pre-tail history\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	buf := &safeBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		tailDaemonLog(ctx, logPath, buf)
		close(done)
	}()

	// Give the goroutine a moment to open + seek to end.
	time.Sleep(150 * time.Millisecond)

	// Append a recognised event.
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"msg":"backend ready","elapsed":1500000000}` + "\n")
	_ = f.Close()

	// Wait for the tail to pick it up.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), "backend ready in 1.5s") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done

	if !strings.Contains(buf.String(), "backend ready in 1.5s") {
		t.Errorf("expected 'backend ready in 1.5s' in output, got %q", buf.String())
	}
	// We seeked to end on open so the pre-tail history line should
	// NOT have been rendered.
	if strings.Contains(buf.String(), "pre-tail") {
		t.Errorf("output should not include pre-tail history: %q", buf.String())
	}
}
