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
	vibev1 "github.com/gallowaysoftware/vibe/proto/vibe/v1"
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
//
// This is 4a4c5ea's warn-don't-block verdict, and it is still the default.
// TestDaemon_VRAMCheck_StrictRefusesInsufficientFree is the same condition
// for a caller that asked to be refused; the pair is the whole design.
func TestDaemon_VRAMCheck_Tight(t *testing.T) {
	setupXDG(t)
	stub := stubModel(t)
	writeProfile(t, "code", vramProfile("code", stub, 22.0))

	d := makeDaemon(t)
	d.SetVRAMProbe(func(context.Context) (float64, error) { return 14.3, nil })
	// Pinned: a 16 GiB CI runner really cannot fit 22 GiB, so an ambient
	// capacity probe turns this warn case into a refusal there.
	d.SetCapacityProbe(func(context.Context) (float64, error) { return 64.0, nil })

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
// machine is the one case that fails closed for EVERY caller — the profile
// must NOT be active and no supervisor child should have been launched.
//
// It also carries half the producer→classifier contract. The refusal is
// asserted through vibeclient.IsVRAMRejection against a REAL daemon over a
// real Connect socket, because the defect this replaces was precisely a
// classifier that had stopped matching its producer while both of its
// tests — each of which built the error it then asserted on — stayed
// green. Nothing here constructs a daemon error.
func TestDaemon_VRAMCheck_ExceedsCapacity(t *testing.T) {
	setupXDG(t)
	stub := stubModel(t)
	writeProfile(t, "code", vramProfile("code", stub, 128.0))

	d := makeDaemon(t)
	d.SetVRAMProbe(func(context.Context) (float64, error) { return 1.0, nil })
	d.SetCapacityProbe(func(context.Context) (float64, error) { return 64.0, nil })

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
	// Asserted against the daemon's REAL output; the classifier below is
	// what must never depend on it.
	for _, want := range []string{`"code"`, "in total", "--no-vram-check"} {
		if !strings.Contains(msg, want) {
			t.Errorf("err message %q missing %q", msg, want)
		}
	}

	// The contract vamp's candidate walk rides on.
	rej, ok := vibeclient.VRAMRejection(err)
	if !ok {
		t.Fatalf("vibeclient.VRAMRejection did not classify the daemon's own refusal: %v", err)
	}
	if got := rej.GetReason(); got != vibev1.StartRejection_REASON_VRAM_EXCEEDS_CAPACITY {
		t.Errorf("reason = %v, want REASON_VRAM_EXCEEDS_CAPACITY", got)
	}
	if rej.GetProfile() != "code" {
		t.Errorf("detail profile = %q, want %q", rej.GetProfile(), "code")
	}
	if rej.GetEstimatedGib() != 128.0 {
		t.Errorf("detail estimated_gib = %v, want 128", rej.GetEstimatedGib())
	}
	if rej.GetFreeGib() != 1.0 {
		t.Errorf("detail free_gib = %v, want 1", rej.GetFreeGib())
	}
	if rej.TotalGib == nil {
		t.Error("detail total_gib absent; the capacity probe answered, so the number that decided this must travel with it")
	} else if *rej.TotalGib != 64.0 {
		t.Errorf("detail total_gib = %v, want 64", *rej.TotalGib)
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

// TestDaemon_VRAMCheck_StrictRefusesInsufficientFree is the other half of
// the contract, and the rejection CLASS that 4a4c5ea removed and this
// change restores under an opt-in.
//
// 60 GiB wanted, 0.5 GiB free, 64 GiB capacity: it fits the machine, so
// the default caller gets a warning and a running profile (proved by
// TestDaemon_VRAMCheck_StrictIsOptInOnly, which differs from this test in
// exactly one field). A caller that says it can fall back is refused
// instead, with the typed reason that tells it so.
//
// This is the case vamp's candidate walk exists for — "something else is
// resident right now" — and between 4a4c5ea and this change it could not
// fire at all, which made candidates 2..N of every capability dead config.
func TestDaemon_VRAMCheck_StrictRefusesInsufficientFree(t *testing.T) {
	setupXDG(t)
	stub := stubModel(t)
	writeProfile(t, "code", vramProfile("code", stub, 60.0))

	d := makeDaemon(t)
	d.SetVRAMProbe(func(context.Context) (float64, error) { return 0.5, nil })
	d.SetCapacityProbe(func(context.Context) (float64, error) { return 64.0, nil })

	client, _ := startDaemon(t, d)

	startCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := client.StartWithOptions(startCtx, "code", vibeclient.StartOptions{StrictVRAM: true})
	if err == nil {
		t.Fatalf("Start with StrictVRAM: expected a refusal, got nil — the candidate walk has nothing to walk on")
	}

	rej, ok := vibeclient.VRAMRejection(err)
	if !ok {
		t.Fatalf("vibeclient.VRAMRejection did not classify the strict refusal: %v", err)
	}
	if got := rej.GetReason(); got != vibev1.StartRejection_REASON_VRAM_INSUFFICIENT_FREE {
		t.Errorf("reason = %v, want REASON_VRAM_INSUFFICIENT_FREE", got)
	}
	if rej.GetProfile() != "code" {
		t.Errorf("detail profile = %q, want %q", rej.GetProfile(), "code")
	}
	if rej.GetEstimatedGib() != 60.0 {
		t.Errorf("detail estimated_gib = %v, want 60", rej.GetEstimatedGib())
	}
	if rej.GetFreeGib() != 0.5 {
		t.Errorf("detail free_gib = %v, want 0.5", rej.GetFreeGib())
	}
	// Capacity did not decide this one, so it must be ABSENT rather than a
	// zero a reader would take for "nothing fits on this box".
	if rej.TotalGib != nil {
		t.Errorf("detail total_gib = %v, want absent: capacity was not the ground for this refusal", *rej.TotalGib)
	}

	// A refusal is an observation, not an actuator: nothing was started,
	// and nothing was evicted to make room either.
	s, err := client.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if s.Running || s.Profile != "" || s.Pid != 0 {
		t.Errorf("daemon state after a strict refusal = %+v; want nothing running", s)
	}
}

// TestDaemon_VRAMCheck_StrictIsOptInOnly: the same probe readings and the
// same profile as TestDaemon_VRAMCheck_StrictRefusesInsufficientFree, with
// StrictVRAM unset. It must START.
//
// The pair is what keeps this change from silently reverting 4a4c5ea:
// short-of-free is still a warning for everyone who did not ask, which is
// every human at `vibe start` and `vibe run`. Delete this test and the
// next agent cannot tell an opt-in from a restored hard failure.
func TestDaemon_VRAMCheck_StrictIsOptInOnly(t *testing.T) {
	setupXDG(t)
	stub := stubModel(t)
	writeProfile(t, "code", vramProfile("code", stub, 60.0))

	d := makeDaemon(t)
	d.SetVRAMProbe(func(context.Context) (float64, error) { return 0.5, nil })
	d.SetCapacityProbe(func(context.Context) (float64, error) { return 64.0, nil })

	client, _ := startDaemon(t, d)

	startCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := client.Start(startCtx, "code")
	if err != nil {
		t.Fatalf("Start without StrictVRAM must warn and proceed (4a4c5ea), got: %v", err)
	}
	t.Cleanup(func() { _, _ = client.Stop(context.Background()) })
	if res.Status == nil || !res.Status.Running {
		t.Fatalf("status after Start = %+v; want running", res.Status)
	}
}

// TestDaemon_VRAMCheck_ARefusalDoesNotBlockTheNextCandidate walks two
// candidates against ONE real daemon the way vamp's candidate walk does:
// the big one is asked for strictly and refused, and the smaller one must
// then actually start.
//
// The sequence is the point. Every other test here ends at the refusal,
// and a refusal that left the start mutex held, or left the active slot
// claimed by the profile it just declined, would pass all of them while
// making the fallback unreachable in production for a reason no unit test
// of either half could see. This is the seam vamp's Executor drives, with
// the real producer on the other end of it.
func TestDaemon_VRAMCheck_ARefusalDoesNotBlockTheNextCandidate(t *testing.T) {
	setupXDG(t)
	stub := stubModel(t)
	writeProfile(t, "big", vramProfile("big", stub, 60.0))
	writeProfile(t, "small", vramProfile("small", stub, 8.0))

	d := makeDaemon(t)
	// 12 GiB free of a 64 GiB machine: "big" fits the box but not the
	// moment (the resident-model case), "small" fits both.
	d.SetVRAMProbe(func(context.Context) (float64, error) { return 12.0, nil })
	d.SetCapacityProbe(func(context.Context) (float64, error) { return 64.0, nil })

	client, _ := startDaemon(t, d)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Candidate 1: asked strictly, because a smaller one is behind it.
	_, err := client.EnsureActiveWithOptions(ctx, "big", vibeclient.StartOptions{StrictVRAM: true})
	if err == nil {
		t.Fatal(`EnsureActive("big", strict): expected a refusal, got nil`)
	}
	if !vibeclient.IsVRAMRejection(err) {
		t.Fatalf("candidate 1's refusal is not classifiable, so a walk would abort here: %v", err)
	}

	// Candidate 2: the last one, asked the ordinary way. This is the
	// assertion the whole change exists to make true.
	st, err := client.EnsureActiveWithOptions(ctx, "small", vibeclient.StartOptions{})
	if err != nil {
		t.Fatalf(`EnsureActive("small") after a refused candidate: %v`, err)
	}
	t.Cleanup(func() { _, _ = client.Stop(context.Background()) })
	if st == nil || !st.Running || !st.Ready {
		t.Fatalf("candidate 2 status = %+v; want running+ready", st)
	}
	if st.Profile != "small" {
		t.Errorf("active profile = %q, want %q — the walk landed on the wrong candidate", st.Profile, "small")
	}
}

// TestDaemon_VRAMCheck_UnrelatedRefusalIsNotAVRAMRejection: a
// FailedPrecondition the daemon raises for some OTHER reason must not
// classify as a memory rejection, or vamp's walk would silently retry a
// broken config against a smaller model and report the wrong cause.
//
// The error is real, not constructed: an external-backend profile whose
// router is not up fails its catalog readiness check. External backends
// skip the VRAM pre-flight entirely, so this is the same Connect code
// arriving with no rejection detail on it — exactly what the old
// substring match could not tell apart once the wording drifted.
func TestDaemon_VRAMCheck_UnrelatedRefusalIsNotAVRAMRejection(t *testing.T) {
	setupXDG(t)
	stub := stubModel(t)
	writeProfile(t, "ext", externalRouterProfile("ext", stub, "some-alias"))

	// makeDaemon points ProxyPort at a free port nobody is listening on, so
	// the readiness check cannot reach a router.
	d := makeDaemon(t)
	client, _ := startDaemon(t, d)

	startCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := client.Start(startCtx, "ext")
	if err == nil {
		t.Fatal("Start against an absent router: expected a readiness failure, got nil")
	}
	if !strings.Contains(err.Error(), "failed_precondition") && !strings.Contains(err.Error(), "FailedPrecondition") {
		t.Fatalf("err = %v; this test is only meaningful if the daemon answers FailedPrecondition here", err)
	}
	if vibeclient.IsVRAMRejection(err) {
		t.Errorf("IsVRAMRejection classified an unrelated readiness failure as a memory rejection: %v", err)
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
// even when it would otherwise reject the profile — including when the
// caller also asked for a strict pre-flight. Skipping the probe is the
// stronger statement of the two, so it wins; a caller that sets both gets
// no check rather than the strictest one.
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
	res, err := client.StartWithOptions(startCtx, "code", vibeclient.StartOptions{NoVRAMCheck: true, StrictVRAM: true})
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
