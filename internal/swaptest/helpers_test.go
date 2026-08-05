package swaptest_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/swaptest"
)

// captureFrames opens one /api/events stream on the cell, runs drive, and
// returns every frame that arrived after the connect burst.
func captureFrames(t *testing.T, c *swaptest.Cell, drive func()) []swaptest.RecordedFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.URL()+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // closed by the reader goroutine below (bodyclose can't see across the goroutine)
	if err != nil {
		t.Fatalf("GET /api/events: %v", err)
	}
	body := resp.Body
	defer func() { _ = body.Close() }()

	frames := make(chan swaptest.RecordedFrame, 64)
	go func() {
		defer close(frames)
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 0, 64<<10), 32<<20)
		var data []string
		for sc.Scan() {
			line := sc.Text()
			if line != "" {
				if rest, ok := strings.CutPrefix(line, "data:"); ok {
					data = append(data, strings.TrimPrefix(rest, " "))
				}
				continue
			}
			payload := strings.Join(data, "\n")
			data = nil
			var env struct {
				Type string `json:"type"`
				Data string `json:"data"`
			}
			if json.Unmarshal([]byte(payload), &env) != nil {
				continue
			}
			frames <- swaptest.RecordedFrame{Type: env.Type, Inner: env.Data}
		}
	}()

	// Drain the connect burst before driving, so the caller sees only what
	// its own action produced. The burst ENDS at its inflight snapshot, so
	// drain until that frame rather than counting frames: the count differs
	// per wire (v247 adds uiConfig and profileChanged) and a fixed count
	// leaves the snapshot in the caller's sample, where it silently widens
	// whatever shape the caller is about to compare.
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatal("stream closed during the connect burst")
			}
			if f.Type != "inflight" {
				continue
			}
		case <-time.After(5 * time.Second):
			t.Fatal("connect burst did not complete")
		}
		break
	}

	drive()

	var out []swaptest.RecordedFrame
	deadline := time.After(3 * time.Second)
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				return out
			}
			out = append(out, f)
			if f.Type == "inflight" && (strings.Contains(f.Inner, `"requests":[]`) || strings.Contains(f.Inner, `"operation":"remove"`)) {
				return out
			}
		case <-deadline:
			return out
		}
	}
}
