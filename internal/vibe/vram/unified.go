package vram

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// AppleUnifiedProbe reports how much memory a model could actually claim on
// an Apple-silicon host. It exists because the nvidia-smi probe returns
// ErrNoGPUInfo there, which downgrades the pre-flight to a warning — the
// wrong answer on a machine whose GPU budget is a slice of the same RAM the
// OS, the browser, and any service-mode sidecar are already using.
//
// **What it measures, and why not the Metal ceiling.** The hard cap is
// Metal's recommendedMaxWorkingSetSize, but reading that requires linking
// Metal, and vibe is a cgo-free build. Rather than hardcode a fraction of
// hw.memsize (the default ratio is not documented and is not a round number
// in practice), this probe measures *available* memory from vm_stat. On a
// unified-memory box that is the constraint that actually bites: a 22 GiB
// model does not fail because it exceeded a ratio, it fails because Chrome
// and an embedding server were already holding the pages.
//
// When iogpu.wired_limit_mb is set explicitly the result is additionally
// capped by it, since an operator who set that ceiling meant it.
func AppleUnifiedProbe(ctx context.Context) (float64, error) {
	if runtime.GOOS != "darwin" {
		return 0, ErrNoGPUInfo
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	out, err := exec.CommandContext(cctx, "vm_stat").Output()
	if err != nil {
		return 0, ErrNoGPUInfo
	}
	availGiB, err := parseVMStatAvailableGiB(out)
	if err != nil {
		return 0, err
	}

	// A non-zero iogpu.wired_limit_mb is a deliberate operator ceiling; 0
	// means "macOS default", which this probe cannot read and must ignore.
	if limit, err := sysctlInt(cctx, "iogpu.wired_limit_mb"); err == nil && limit > 0 {
		if capGiB := float64(limit) / 1024.0; capGiB < availGiB {
			return capGiB, nil
		}
	}
	return availGiB, nil
}

// parseVMStatAvailableGiB sums the reclaimable page classes from vm_stat
// output. Free + inactive + speculative + purgeable is the same
// approximation Activity Monitor's "available" uses: inactive and
// speculative pages are file-backed or read-ahead and are evicted on
// demand rather than swapped.
func parseVMStatAvailableGiB(out []byte) (float64, error) {
	pageSize := int64(4096)
	counts := map[string]int64{}

	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "Mach Virtual Memory Statistics") {
			// "... (page size of 16384 bytes)" — 16K on Apple silicon, 4K
			// on Intel, so it must be read rather than assumed.
			if i := strings.Index(line, "page size of "); i >= 0 {
				rest := line[i+len("page size of "):]
				if f := strings.Fields(rest); len(f) > 0 {
					if n, err := strconv.ParseInt(f[0], 10, 64); err == nil && n > 0 {
						pageSize = n
					}
				}
			}
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(value), "."))
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			continue
		}
		counts[strings.TrimSpace(key)] = n
	}

	free, haveFree := counts["Pages free"]
	if !haveFree {
		return 0, ErrNoGPUInfo
	}
	pages := free + counts["Pages inactive"] + counts["Pages speculative"] + counts["Pages purgeable"]
	return float64(pages*pageSize) / (1 << 30), nil
}

func sysctlInt(ctx context.Context, name string) (int64, error) {
	out, err := exec.CommandContext(ctx, "sysctl", "-n", name).Output()
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
}

// DefaultProbe picks the right probe for the host: nvidia-smi where there
// is an NVIDIA GPU (the workstation and the Sparks), unified-memory
// accounting on Apple silicon. Falling back rather than choosing by GOOS
// keeps a CUDA box with an odd OS working, and keeps the "no info, warn and
// proceed" behaviour for anything neither probe understands.
func DefaultProbe(ctx context.Context) (float64, error) {
	free, err := NvidiaSMIProbe(ctx)
	if err == nil {
		return free, nil
	}
	return AppleUnifiedProbe(ctx)
}

// FormatFreeGiB renders a probe result for human-facing output.
func FormatFreeGiB(v float64) string { return fmt.Sprintf("%.1f GiB", v) }

// capacityGiB reports the largest a single model could ever be on this
// host, used to separate "tight right now" from "cannot ever fit". Returns
// ErrNoGPUInfo when capacity can't be established, which leaves the check
// at a warning — the conservative direction, since a wrong capacity would
// block a start that would have worked.
func capacityGiB(ctx context.Context) (float64, error) {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	if total, err := nvidiaTotalGiB(cctx); err == nil {
		return total, nil
	}
	// A CPU Linux host still has a knowable ceiling: system RAM. Without
	// this the hard stop silently degraded to a warning everywhere except
	// darwin and NVIDIA hosts, which is exactly where it went unnoticed.
	//
	// On a Linux box with a non-NVIDIA GPU this overstates the ceiling,
	// because that card's VRAM is separate from system RAM. That is the
	// safe direction to be wrong in: too large a capacity only ever
	// downgrades a hard stop to a warning, whereas too small a one would
	// refuse a load that fits. For the same reason there is deliberately no
	// Linux *free* probe — reporting system RAM as free VRAM on a discrete
	// GPU would be wrong in the unsafe direction.
	if runtime.GOOS == "linux" {
		if raw, err := os.ReadFile("/proc/meminfo"); err == nil {
			if total, err := parseMemTotalGiB(raw); err == nil {
				return total, nil
			}
		}
		return 0, ErrNoGPUInfo
	}
	if runtime.GOOS != "darwin" {
		return 0, ErrNoGPUInfo
	}
	// An explicit iogpu ceiling is the real cap when set: the GPU cannot
	// wire past it no matter how much RAM is idle.
	if limit, err := sysctlInt(cctx, "iogpu.wired_limit_mb"); err == nil && limit > 0 {
		return float64(limit) / 1024.0, nil
	}
	// Otherwise total RAM. The true cap is Metal's working-set limit, some
	// fraction below this, so a model between the two is reported as merely
	// tight rather than impossible — deliberately: guessing that fraction
	// wrong would hard-block loads that do fit.
	memBytes, err := sysctlInt(cctx, "hw.memsize")
	if err != nil || memBytes <= 0 {
		return 0, ErrNoGPUInfo
	}
	return float64(memBytes) / (1 << 30), nil
}

// parseMemTotalGiB pulls MemTotal out of /proc/meminfo, whose line reads
// "MemTotal:       32797156 kB". The unit column is always kB on Linux, so
// it is checked rather than parsed.
func parseMemTotalGiB(raw []byte) (float64, error) {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), ":")
		if !ok || key != "MemTotal" {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) < 2 || fields[1] != "kB" {
			return 0, ErrNoGPUInfo
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || kb <= 0 {
			return 0, ErrNoGPUInfo
		}
		return float64(kb) / (1 << 20), nil
	}
	return 0, ErrNoGPUInfo
}

func nvidiaTotalGiB(ctx context.Context) (float64, error) {
	if _, err := exec.LookPath("nvidia-smi"); err != nil {
		return 0, ErrNoGPUInfo
	}
	out, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=memory.total", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return 0, ErrNoGPUInfo
	}
	// parseFreeGiB takes the max across GPUs, which is the right capacity
	// for a model that has to fit on one card.
	return parseFreeGiB(out)
}
