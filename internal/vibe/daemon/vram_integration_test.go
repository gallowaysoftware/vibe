package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/vram"
	"github.com/gallowaysoftware/vibe/internal/vibeclient"
)

// vramProfile returns a llama-server profile YAML that declares an
// estimated_vram_gb so the daemon's pre-flight check is exercised.
func vramProfile(name, modelPath string, estimatedGiB float64) string {
	return fmt.Sprintf(`name: %s
description: %s vram integration profile
backend:
  llama_server:
    path: %s
    alias: fake
    context: 1024
    parallel: 1
frontend:
  kind: external
  write_file: ${VIBE_STATE_DIR}/frontend/%s/sidecar.json
  env:
    STUB_CONFIG: ${WRITE_FILE}
  template:
    api: ${VIBE_API}
    alias: ${MODEL_ALIAS}
estimated_vram_gb: %g
`, name, name, modelPath, name, estimatedGiB)
}

// TestDaemon_VRAMCheck_Sufficient: profile fits within probed free VRAM, so
// Start proceeds normally.
func TestDaemon_VRAMCheck_Sufficient(t *testing.T) {
	setupXDG(t)
	stub := stubModel(t)
	writeProfile(t, "code", vramProfile("code", stub, 22.0))

	d := makeDaemon(t)
	// Plenty of headroom.
	d.SetVRAMProbe(func(context.Context) (float64, error) { return 26.0, nil })

	client, _ := startDaemon(t, d)

	startCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := client.Start(startCtx, "code")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _, _ = client.Stop(context.Background()) })
	if res.Status == nil || !res.Status.Running || !res.Status.Ready {
		t.Fatalf("status after Start = %+v; want running+ready", res.Status)
	}
}

// TestDaemon_VRAMCheck_Tight: an estimate above free memory but within the
// machine's capacity is a warning, not a refusal — free memory is largely
// reclaimable, so blocking here produced false negatives. Start proceeds.
func TestDaemon_VRAMCheck_Tight(t *testing.T) {
	setupXDG(t)
	stub := stubModel(t)
	writeProfile(t, "code", vramProfile("code", stub, 22.0))

	d := makeDaemon(t)
	d.SetVRAMProbe(func(context.Context) (float64, error) { return 14.3, nil })

	client, _ := startDaemon(t, d)

	startCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := client.Start(startCtx, "code")
	if err != nil {
		t.Fatalf("Start should proceed on a tight estimate, got: %v", err)
	}
	if res.Status == nil || !res.Status.Running {
		t.Fatalf("status after Start = %+v; want running", res.Status)
	}
	t.Cleanup(func() { _, _ = client.Stop(context.Background()) })
}

// TestDaemon_VRAMCheck_ExceedsCapacity: an estimate larger than the whole
// machine is the one case that still fails closed — the profile must NOT be
// active and no supervisor child should have been launched.
func TestDaemon_VRAMCheck_ExceedsCapacity(t *testing.T) {
	setupXDG(t)
	stub := stubModel(t)
	// Beyond any real host, so the capacity probe rejects it whichever
	// branch (nvidia-smi / darwin sysctl) it takes.
	writeProfile(t, "code", vramProfile("code", stub, 10*1024*1024))

	d := makeDaemon(t)
	d.SetVRAMProbe(func(context.Context) (float64, error) { return 1.0, nil })

	client, _ := startDaemon(t, d)

	startCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := client.Start(startCtx, "code")
	if err == nil {
		t.Fatalf("Start: expected error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "failed_precondition") && !strings.Contains(msg, "FailedPrecondition") {
		t.Errorf("err code = %v; want FailedPrecondition", err)
	}
	// The actionable text from daemon.go's message, including the escape
	// hatch — a user staring at this needs to know it can be overridden.
	for _, want := range []string{`"code"`, "in total", "--no-vram-check"} {
		if !strings.Contains(msg, want) {
			t.Errorf("err message %q missing %q", msg, want)
		}
	}

	// Profile must NOT be active.
	s, err := client.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if s.Running {
		t.Errorf("daemon reports running after VRAM failure: %+v", s)
	}
	if s.Profile != "" {
		t.Errorf("Profile = %q, want empty after VRAM failure", s.Profile)
	}
	if s.Pid != 0 {
		t.Errorf("Pid = %d, want 0 (no supervisor child should have been spawned)", s.Pid)
	}
	if s.BackendAddr != "" {
		t.Errorf("BackendAddr = %q, want empty (no backend)", s.BackendAddr)
	}
}

// TestDaemon_VRAMCheck_NoProbe: when the probe reports ErrNoGPUInfo (e.g.
// nvidia-smi missing on a CPU/AMD host), Start must proceed.
func TestDaemon_VRAMCheck_NoProbe(t *testing.T) {
	setupXDG(t)
	stub := stubModel(t)
	writeProfile(t, "code", vramProfile("code", stub, 22.0))

	d := makeDaemon(t)
	var called atomic.Int32
	d.SetVRAMProbe(func(context.Context) (float64, error) {
		called.Add(1)
		return 0, vram.ErrNoGPUInfo
	})

	client, _ := startDaemon(t, d)

	startCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := client.Start(startCtx, "code")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _, _ = client.Stop(context.Background()) })
	if !res.Status.Running || !res.Status.Ready {
		t.Errorf("status = %+v; want running+ready", res.Status)
	}
	if called.Load() != 1 {
		t.Errorf("probe called %d times, want 1", called.Load())
	}
}

// TestDaemon_VRAMCheck_Bypass: --no-vram-check must skip the probe entirely
// even when it would otherwise reject the profile.
func TestDaemon_VRAMCheck_Bypass(t *testing.T) {
	setupXDG(t)
	stub := stubModel(t)
	writeProfile(t, "code", vramProfile("code", stub, 22.0))

	d := makeDaemon(t)
	var called atomic.Int32
	d.SetVRAMProbe(func(context.Context) (float64, error) {
		called.Add(1)
		// Would reject if the check ran.
		return 1.0, nil
	})

	client, _ := startDaemon(t, d)

	startCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := client.StartWithOptions(startCtx, "code", vibeclient.StartOptions{NoVRAMCheck: true})
	if err != nil {
		t.Fatalf("Start (with bypass): %v", err)
	}
	t.Cleanup(func() { _, _ = client.Stop(context.Background()) })
	if !res.Status.Running || !res.Status.Ready {
		t.Errorf("status = %+v; want running+ready", res.Status)
	}
	if called.Load() != 0 {
		t.Errorf("probe was called %d times; --no-vram-check should skip it", called.Load())
	}
}

// TestDaemon_VRAMCheck_NoEstimate: profiles without estimated_vram_gb must
// never invoke the probe (back-compat for older profiles).
func TestDaemon_VRAMCheck_NoEstimate(t *testing.T) {
	setupXDG(t)
	stub := stubModel(t)
	// externalProfile from the sibling file has no estimated_vram_gb.
	writeProfile(t, "code", externalProfile("code", stub))

	d := makeDaemon(t)
	var called atomic.Int32
	d.SetVRAMProbe(func(context.Context) (float64, error) {
		called.Add(1)
		return 0, errors.New("probe should not be called")
	})

	client, _ := startDaemon(t, d)

	startCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.Start(startCtx, "code"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _, _ = client.Stop(context.Background()) })
	if called.Load() != 0 {
		t.Errorf("probe called %d times for profile with no estimate", called.Load())
	}
}
