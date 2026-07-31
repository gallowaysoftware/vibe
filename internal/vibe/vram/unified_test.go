package vram

import (
	"context"
	"errors"
	"math"
	"runtime"
	"testing"
)

// Real `vm_stat` output from an M3 Pro, trimmed to the lines that matter
// plus a few that must be ignored. Note the 16384-byte page size: assuming
// the Intel 4096 would under-report available memory by 4x.
const m3ProVMStat = `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                               65536.
Pages active:                             757650.
Pages inactive:                           131072.
Pages speculative:                         32768.
Pages throttled:                               0.
Pages wired down:                         400000.
Pages purgeable:                           16384.
"Translation faults":                  123456789.
Pages stored in compressor:                12345.
`

func TestParseVMStatAvailable_UsesReportedPageSize(t *testing.T) {
	got, err := parseVMStatAvailableGiB([]byte(m3ProVMStat))
	if err != nil {
		t.Fatal(err)
	}
	// (65536 + 131072 + 32768 + 16384) pages * 16384 bytes = 3.75 GiB
	const want = 3.75
	if math.Abs(got-want) > 0.001 {
		t.Errorf("available = %.4f GiB, want %.4f", got, want)
	}
}

// A 4K page size (Intel Macs) must be honoured too — the constant is read
// from the header, never assumed.
func TestParseVMStatAvailable_IntelPageSize(t *testing.T) {
	out := `Mach Virtual Memory Statistics: (page size of 4096 bytes)
Pages free:                               262144.
Pages inactive:                                0.
Pages speculative:                             0.
Pages purgeable:                               0.
`
	got, err := parseVMStatAvailableGiB([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got-1.0) > 0.001 {
		t.Errorf("available = %.4f GiB, want 1.0", got)
	}
}

// Output that isn't vm_stat must degrade to ErrNoGPUInfo so Check() skips
// with a warning rather than blocking a start on a bad parse.
func TestParseVMStatAvailable_GarbageIsNoInfo(t *testing.T) {
	if _, err := parseVMStatAvailableGiB([]byte("command not found\n")); !errors.Is(err, ErrNoGPUInfo) {
		t.Errorf("err = %v, want ErrNoGPUInfo", err)
	}
}

// The probe must never claim to know free memory on a non-darwin host,
// where DefaultProbe should have stopped at nvidia-smi.
func TestAppleUnifiedProbe_NonDarwinIsNoInfo(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("darwin: covered by the live probe test")
	}
	if _, err := AppleUnifiedProbe(context.Background()); !errors.Is(err, ErrNoGPUInfo) {
		t.Errorf("err = %v, want ErrNoGPUInfo off darwin", err)
	}
}

// On a real Mac the probe must return a plausible number: enough to matter,
// less than any Apple silicon machine's total RAM.
func TestAppleUnifiedProbe_Live(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin only")
	}
	got, err := AppleUnifiedProbe(context.Background())
	if err != nil {
		t.Fatalf("probe failed on darwin: %v", err)
	}
	if got <= 0 || got > 1024 {
		t.Errorf("available = %v GiB, want a plausible value", got)
	}
}

// DefaultProbe must produce a usable answer on this host whichever branch
// it takes — that is the whole point of the fallback.
func TestDefaultProbe_ResolvesOnThisHost(t *testing.T) {
	got, err := DefaultProbe(context.Background())
	if err != nil {
		if errors.Is(err, ErrNoGPUInfo) {
			t.Skip("host has neither nvidia-smi nor darwin memory stats")
		}
		t.Fatal(err)
	}
	if got <= 0 {
		t.Errorf("free = %v, want > 0", got)
	}
}

// Real /proc/meminfo head from an ubuntu runner. The capacity probe must
// find MemTotal here: without a Linux source, the hard stop degraded to a
// warning on every non-darwin, non-NVIDIA host — which is what CI caught.
func TestParseMemTotal(t *testing.T) {
	raw := []byte(`MemTotal:       16373544 kB
MemFree:         2158372 kB
MemAvailable:   12043128 kB
Buffers:          312496 kB
`)
	got, err := parseMemTotalGiB(raw)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got-15.615) > 0.01 {
		t.Errorf("MemTotal = %.3f GiB, want ~15.615", got)
	}
}

func TestParseMemTotal_Malformed(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"no MemTotal", "MemFree: 100 kB\n"},
		{"unexpected unit", "MemTotal: 16373544 MB\n"},
		{"not a number", "MemTotal: lots kB\n"},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseMemTotalGiB([]byte(tc.raw)); !errors.Is(err, ErrNoGPUInfo) {
				t.Errorf("err = %v, want ErrNoGPUInfo", err)
			}
		})
	}
}

// Capacity must resolve on any host CI or a developer actually uses,
// otherwise the hard stop quietly stops existing there.
func TestCapacity_ResolvesOnThisHost(t *testing.T) {
	got, err := capacityGiB(context.Background())
	if err != nil {
		t.Fatalf("capacity unknown on %s: %v — the hard stop cannot fire here", runtime.GOOS, err)
	}
	if got <= 0 {
		t.Errorf("capacity = %v, want > 0", got)
	}
}
