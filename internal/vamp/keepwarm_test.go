package vamp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// warmRecorder captures heartbeat invocations for deterministic assertions.
type warmRecorder struct {
	mu    sync.Mutex
	calls []keepWarmKey
	err   error // returned from every warm call
	fired chan struct{}
}

func newWarmRecorder(err error) *warmRecorder {
	return &warmRecorder{err: err, fired: make(chan struct{}, 64)}
}

func (r *warmRecorder) warm(_ context.Context, baseURL, model string) error {
	r.mu.Lock()
	r.calls = append(r.calls, keepWarmKey{baseURL, model})
	r.mu.Unlock()
	select {
	case r.fired <- struct{}{}:
	default: // never block the sweep goroutine on a full buffer
	}
	return r.err
}

func (r *warmRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *warmRecorder) waitForCalls(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case <-r.fired:
		case <-deadline:
			t.Fatalf("heartbeat did not fire %d time(s) within 2s (got %d)", n, r.count())
		}
	}
}

func TestKeepWarm_FiresAfterIdleInterval(t *testing.T) {
	rec := newWarmRecorder(nil)
	kw := newKeepWarm(50*time.Millisecond, func(string, ...any) {})
	kw.warm = rec.warm
	kw.touch("http://127.0.0.1:9000/v1", "qwen") // as the inference client reports it
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	kw.start(ctx)
	defer kw.stopAndWait()

	rec.waitForCalls(t, 1)
	rec.mu.Lock()
	got := rec.calls[0]
	rec.mu.Unlock()
	// The /v1 suffix must be normalized away: WarmModel appends /v1 itself.
	want := keepWarmKey{"http://127.0.0.1:9000", "qwen"}
	if got != want {
		t.Errorf("heartbeat endpoint = %+v, want %+v", got, want)
	}
}

func TestKeepWarm_RecentUseSuppressesHeartbeat(t *testing.T) {
	rec := newWarmRecorder(nil)
	kw := newKeepWarm(time.Hour, func(string, ...any) {})
	kw.tick = 10 * time.Millisecond // sweep often; interval keeps it idle-gated
	kw.warm = rec.warm
	kw.touch("http://x", "m")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	kw.start(ctx)
	defer kw.stopAndWait()

	time.Sleep(150 * time.Millisecond)
	if got := rec.count(); got != 0 {
		t.Errorf("heartbeat fired %d time(s) despite recent use", got)
	}
}

// TestKeepWarm_FailureIsLoggedNotFatal: a failing heartbeat logs and the
// loop keeps running (the endpoint stays tracked and fires again after the
// next idle interval).
func TestKeepWarm_FailureIsLoggedNotFatal(t *testing.T) {
	rec := newWarmRecorder(errors.New("upstream fell over"))
	var logMu sync.Mutex
	var logs []string
	kw := newKeepWarm(30*time.Millisecond, func(format string, args ...any) {
		logMu.Lock()
		logs = append(logs, fmt.Sprintf(format, args...))
		logMu.Unlock()
	})
	kw.warm = rec.warm
	kw.touch("http://x", "m")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	kw.start(ctx)
	defer kw.stopAndWait()

	rec.waitForCalls(t, 2) // >= 2 proves the sweep survived the first failure
	logMu.Lock()
	joined := strings.Join(logs, "\n")
	logMu.Unlock()
	if !strings.Contains(joined, "non-fatal") || !strings.Contains(joined, "upstream fell over") {
		t.Errorf("expected non-fatal heartbeat failure log, got:\n%s", joined)
	}
}

func TestKeepWarm_ForgetAllStopsHeartbeats(t *testing.T) {
	rec := newWarmRecorder(nil)
	kw := newKeepWarm(30*time.Millisecond, func(string, ...any) {})
	kw.warm = rec.warm
	kw.touch("http://x", "m")
	kw.forgetAll()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	kw.start(ctx)
	defer kw.stopAndWait()

	time.Sleep(120 * time.Millisecond)
	if got := rec.count(); got != 0 {
		t.Errorf("heartbeat fired %d time(s) after forgetAll", got)
	}
}

func TestKeepWarm_NilIsNoOp(t *testing.T) {
	var kw *keepWarm
	kw.touch("http://x", "m")
	kw.forgetAll()
	kw.start(context.Background())
	kw.stopAndWait() // must not panic or block
	if kw := newKeepWarm(0, func(string, ...any) {}); kw != nil {
		t.Error("newKeepWarm(0) should return nil (disabled)")
	}
}

func TestKeepWarmSetting_YAML(t *testing.T) {
	cases := []struct {
		name         string
		yaml         string
		wantInterval time.Duration // via EffectiveInterval; 0 = disabled
	}{
		{"duration override", `keep_warm: 45m`, 45 * time.Minute},
		{"disabled", `keep_warm: false`, 0},
		{"explicit default", `keep_warm: true`, defaultKeepWarmInterval},
		{"unset", `name: x`, defaultKeepWarmInterval},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var p struct {
				Name     string          `yaml:"name"`
				KeepWarm KeepWarmSetting `yaml:"keep_warm,omitempty"`
			}
			if err := yaml.Unmarshal([]byte(tc.yaml), &p); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := p.KeepWarm.EffectiveInterval(); got != tc.wantInterval {
				t.Errorf("EffectiveInterval = %s, want %s", got, tc.wantInterval)
			}
			// Round-trip: marshal + re-unmarshal preserves the setting.
			out, err := yaml.Marshal(p)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var p2 struct {
				Name     string          `yaml:"name"`
				KeepWarm KeepWarmSetting `yaml:"keep_warm,omitempty"`
			}
			if err := yaml.Unmarshal(out, &p2); err != nil {
				t.Fatalf("re-unmarshal %q: %v", out, err)
			}
			if got := p2.KeepWarm.EffectiveInterval(); got != tc.wantInterval {
				t.Errorf("round-trip EffectiveInterval = %s, want %s (yaml: %q)", got, tc.wantInterval, out)
			}
			if tc.name == "unset" && strings.Contains(string(out), "keep_warm") {
				t.Errorf("unset keep_warm must be omitted from marshal, got %q", out)
			}
		})
	}
	var bad KeepWarmSetting
	if err := yaml.Unmarshal([]byte(`banana`), &bad); err == nil {
		t.Error("expected error for non-duration, non-bool scalar")
	}
}
