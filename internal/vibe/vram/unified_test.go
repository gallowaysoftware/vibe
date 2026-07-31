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
