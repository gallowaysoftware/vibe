// Package vram provides a pre-flight VRAM check for profile activation.
//
// The default probe shells out to `nvidia-smi --query-gpu=memory.free
// --format=csv,noheader,nounits` and returns the maximum per-GPU free MiB
// converted to GiB. The daemon uses this to refuse activation when a profile
// declares it needs more VRAM than is available — saving users from confusing
// OOM kills mid-load.
//
// Callers inject a Probe function so the daemon can be tested without a real
// nvidia-smi on the host. ErrNoGPUInfo signals "no usable GPU info" (binary
// missing, CPU-only box, etc.) — the daemon treats that as a warning and
// proceeds, since not every user has an Nvidia GPU.
package vram

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// DefaultSlopGiB is the buffer added to free VRAM before comparing against the
// profile's estimate. Estimates are inherently fuzzy (different drivers,
// fragmentation, residual allocations), and llama-server is forgiving with a
// little headroom — half a gibibyte is the smallest slop that meaningfully
// suppresses false positives in practice.
const DefaultSlopGiB = 0.5

// ErrNoGPUInfo is returned by a Probe when no usable free-memory readings
// could be obtained (nvidia-smi missing, exec failed, unparseable output).
// The daemon downgrades this to a warning rather than a hard fail so users
// on CPU/AMD/Apple-Silicon hosts aren't blocked.
var ErrNoGPUInfo = errors.New("no GPU free-memory info available")

// Probe returns the maximum free VRAM (in GiB) across all GPUs, or
// ErrNoGPUInfo when none can be determined. Today vibe runs one profile at a
// time on one GPU, so we surface the single best-case number; per-GPU
// planning is a future concern.
type Probe func(ctx context.Context) (freeGiB float64, err error)

// NvidiaSMIProbe runs `nvidia-smi --query-gpu=memory.free
// --format=csv,noheader,nounits` and returns the per-GPU max. Each value is
// MiB; we convert to GiB by dividing by 1024.
func NvidiaSMIProbe(ctx context.Context) (float64, error) {
	if _, err := exec.LookPath("nvidia-smi"); err != nil {
		return 0, ErrNoGPUInfo
	}
	// Bound the call: nvidia-smi has been known to hang on broken setups.
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "nvidia-smi",
		"--query-gpu=memory.free",
		"--format=csv,noheader,nounits")
	out, err := cmd.Output()
	if err != nil {
		return 0, ErrNoGPUInfo
	}
	return parseFreeGiB(out)
}

// parseFreeGiB returns the max free GiB across all parsed lines. An empty or
// fully-unparseable output yields ErrNoGPUInfo.
func parseFreeGiB(out []byte) (float64, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	var maxMiB int64 = -1
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		// nounits should mean a bare integer, but be lenient about a stray
		// "MiB" suffix or whitespace just in case.
		line = strings.TrimSuffix(line, "MiB")
		line = strings.TrimSpace(line)
		mib, err := strconv.ParseInt(line, 10, 64)
		if err != nil {
			continue
		}
		if mib > maxMiB {
			maxMiB = mib
		}
	}
	if maxMiB < 0 {
		return 0, ErrNoGPUInfo
	}
	return float64(maxMiB) / 1024.0, nil
}

// CheckResult is the outcome of a pre-flight check. The daemon consults
// these fields to either proceed, refuse, or warn-and-proceed.
type CheckResult struct {
	// OK is true when the profile's estimate fits within free VRAM + slop,
	// OR when the probe couldn't determine free VRAM (Skipped=true).
	OK bool
	// Skipped is true when the probe returned ErrNoGPUInfo. Callers should
	// log a warning and proceed.
	Skipped bool
	// FreeGiB is the probed max free VRAM in GiB. Zero when Skipped.
	FreeGiB float64
	// EstimatedGiB echoes the profile's estimate, for logging.
	EstimatedGiB float64
	// Message is human-readable detail (empty when OK and not skipped).
	Message string
}

// Check runs probe and compares its result to estimatedGiB. slopGiB is added
// to free before the comparison.
//
// When the probe returns ErrNoGPUInfo, Check returns OK=true / Skipped=true:
// the daemon will log a warning and proceed (CPU/AMD-friendly).
func Check(ctx context.Context, probe Probe, estimatedGiB, slopGiB float64) CheckResult {
	if estimatedGiB <= 0 {
		// Nothing to check.
		return CheckResult{OK: true, EstimatedGiB: estimatedGiB}
	}
	free, err := probe(ctx)
	if errors.Is(err, ErrNoGPUInfo) {
		return CheckResult{OK: true, Skipped: true, EstimatedGiB: estimatedGiB}
	}
	if err != nil {
		// Any other error is also treated as "no info" — better to proceed
		// than to block on a flaky tool. The daemon will log the underlying
		// error via the message.
		return CheckResult{
			OK:           true,
			Skipped:      true,
			EstimatedGiB: estimatedGiB,
			Message:      err.Error(),
		}
	}
	res := CheckResult{
		FreeGiB:      free,
		EstimatedGiB: estimatedGiB,
	}
	if free+slopGiB >= estimatedGiB {
		res.OK = true
		return res
	}
	res.OK = false
	res.Message = fmt.Sprintf("needs ~%.1f GiB free VRAM but only %.1f GiB is free",
		estimatedGiB, free)
	return res
}
