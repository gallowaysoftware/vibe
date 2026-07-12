package vamp

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// defaultKeepWarmInterval is how long an ensured LLM endpoint may sit unused
// mid-run before the lease heartbeat re-warms it. Sized under typical router
// idle-TTLs (30-45 min) with margin, so a 50-minute ComfyUI stage between
// LLM calls doesn't let llama-swap reap the model mid-pipeline.
const defaultKeepWarmInterval = 20 * time.Minute

// KeepWarmSetting is the pipeline-level `keep_warm:` knob. YAML forms:
//
//	keep_warm: 20m    # override the heartbeat interval
//	keep_warm: false  # disable the heartbeat for this pipeline
//	keep_warm: true   # explicit default (same as omitting the field)
//
// The zero value means "unset" — heartbeat on, default interval.
type KeepWarmSetting struct {
	Disabled bool
	Interval time.Duration // 0 = default
}

func (k *KeepWarmSetting) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("keep_warm: expected a duration (e.g. \"20m\") or false, got %v", value.Kind)
	}
	var b bool
	if err := value.Decode(&b); err == nil {
		k.Disabled = !b
		k.Interval = 0
		return nil
	}
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("keep_warm: %w", err)
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("keep_warm: %q is neither a duration nor a boolean: %w", s, err)
	}
	if d <= 0 {
		k.Disabled = true
		k.Interval = 0
		return nil
	}
	k.Disabled = false
	k.Interval = d
	return nil
}

func (k KeepWarmSetting) MarshalYAML() (any, error) {
	switch {
	case k.Disabled:
		return false, nil
	case k.Interval > 0:
		return k.Interval.String(), nil
	default:
		return nil, nil
	}
}

// IsZero lets yaml.v3's omitempty drop the field when unset, keeping run-dir
// pipeline snapshots byte-identical for pipelines that never mention it.
func (k KeepWarmSetting) IsZero() bool { return !k.Disabled && k.Interval == 0 }

// EffectiveInterval resolves the setting: 0 means disabled.
func (k KeepWarmSetting) EffectiveInterval() time.Duration {
	switch {
	case k.Disabled:
		return 0
	case k.Interval > 0:
		return k.Interval
	default:
		return defaultKeepWarmInterval
	}
}

type keepWarmCtxKey struct{}

// withKeepWarm threads the run's heartbeat manager through the stage
// context, same pattern as WithMetrics: the inference chokepoint reads it
// back without touching InferenceFunc's signature.
func withKeepWarm(ctx context.Context, kw *keepWarm) context.Context {
	return context.WithValue(ctx, keepWarmCtxKey{}, kw)
}

func keepWarmFrom(ctx context.Context) *keepWarm {
	kw, _ := ctx.Value(keepWarmCtxKey{}).(*keepWarm)
	return kw
}

type keepWarmKey struct{ baseURL, model string }

// keepWarm is the run-scoped lease heartbeat: every LLM endpoint that was
// Ensured during the run gets a 1-token streaming keep-warm request whenever
// it hasn't been used for interval, so a TTL-reaping router (llama-swap)
// doesn't unload it between distant pipeline stages. Heartbeat failures are
// logged and never fatal — the next real use re-warms via the normal ensure
// path.
type keepWarm struct {
	interval time.Duration
	tick     time.Duration
	warm     func(ctx context.Context, baseURL, model string) error
	logf     func(format string, args ...any)

	mu      sync.Mutex
	lastUse map[keepWarmKey]time.Time

	cancel context.CancelFunc
	done   chan struct{}
}

// newKeepWarm returns a manager sweeping at interval/8 (clamped to
// [10ms, 1m]) so a heartbeat fires within ~12% of the deadline without a
// busy loop. interval <= 0 (keep_warm: false) returns nil; every method on a
// nil *keepWarm is a no-op, so callers don't branch.
func newKeepWarm(interval time.Duration, logf func(format string, args ...any)) *keepWarm {
	if interval <= 0 {
		return nil
	}
	tick := interval / 8
	if tick > time.Minute {
		tick = time.Minute
	}
	if tick < 10*time.Millisecond {
		tick = 10 * time.Millisecond
	}
	return &keepWarm{
		interval: interval,
		tick:     tick,
		warm: func(ctx context.Context, baseURL, model string) error {
			return WarmModel(ctx, nil, baseURL, model)
		},
		logf:    logf,
		lastUse: make(map[keepWarmKey]time.Time),
	}
}

// start launches the sweep goroutine. Its lifetime is bounded by ctx and by
// stopAndWait; the executor defers stopAndWait so the goroutine can never
// outlive the run.
func (k *keepWarm) start(ctx context.Context) {
	if k == nil {
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	k.cancel = cancel
	k.done = make(chan struct{})
	go k.run(runCtx)
}

// stopAndWait cancels the sweep goroutine and blocks until it exits, so Run
// returns with zero heartbeat goroutines left behind.
func (k *keepWarm) stopAndWait() {
	if k == nil || k.cancel == nil {
		return
	}
	k.cancel()
	<-k.done
}

func (k *keepWarm) run(ctx context.Context) {
	defer close(k.done)
	ticker := time.NewTicker(k.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			k.sweep(ctx)
		}
	}
}

// touch records that the endpoint was just used (or Ensured). The base URL
// is normalized so the executor's proxy-root form and the inference client's
// "/v1"-suffixed form land on the same key.
func (k *keepWarm) touch(baseURL, model string) {
	if k == nil || baseURL == "" || model == "" {
		return
	}
	key := keepWarmKey{normalizeWarmBase(baseURL), model}
	k.mu.Lock()
	k.lastUse[key] = time.Now()
	k.mu.Unlock()
}

// forgetAll drops every tracked endpoint. Called when the run deliberately
// unloads the active profile (free_profile_after) — a heartbeat would
// otherwise reload the model the pipeline just paid to evict.
func (k *keepWarm) forgetAll() {
	if k == nil {
		return
	}
	k.mu.Lock()
	clear(k.lastUse)
	k.mu.Unlock()
}

// sweep warms every endpoint idle past interval. lastUse is bumped before
// the warm fires so a multi-minute JIT reload doesn't stack duplicate
// heartbeats behind it; success re-touches with the post-warm time.
func (k *keepWarm) sweep(ctx context.Context) {
	now := time.Now()
	k.mu.Lock()
	var due []keepWarmKey
	for key, last := range k.lastUse {
		if now.Sub(last) >= k.interval {
			due = append(due, key)
			k.lastUse[key] = now
		}
	}
	k.mu.Unlock()
	sort.Slice(due, func(i, j int) bool {
		if due[i].baseURL != due[j].baseURL {
			return due[i].baseURL < due[j].baseURL
		}
		return due[i].model < due[j].model
	})
	for _, key := range due {
		warmCtx, cancel := context.WithTimeout(ctx, warmAttemptTimeout)
		err := k.warm(warmCtx, key.baseURL, key.model)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			k.logf("keep_warm: %s: heartbeat failed: %v (non-fatal; next real use re-warms)", key.model, err)
			continue
		}
		k.touch(key.baseURL, key.model)
	}
}

func normalizeWarmBase(baseURL string) string {
	return strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")
}
