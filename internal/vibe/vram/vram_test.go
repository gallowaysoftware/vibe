package vram

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestParseFreeGiB(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantGiB float64
		wantErr bool
	}{
		{
			name:    "single gpu",
			in:      "14336\n",
			wantGiB: 14.0,
		},
		{
			name:    "multiple gpus pick max",
			in:      "10240\n22528\n5120\n",
			wantGiB: 22.0,
		},
		{
			name:    "tolerates MiB suffix",
			in:      "  22528 MiB \n",
			wantGiB: 22.0,
		},
		{
			name:    "skips blank lines",
			in:      "\n\n14336\n\n",
			wantGiB: 14.0,
		},
		{
			name:    "skips unparseable lines",
			in:      "garbage\n14336\nmore garbage\n",
			wantGiB: 14.0,
		},
		{
			name:    "empty output is error",
			in:      "",
			wantErr: true,
		},
		{
			name:    "all-garbage output is error",
			in:      "Driver Version: 550.54\nCUDA Version: 12.4\n",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseFreeGiB([]byte(tc.in))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("got %.2f, want error", got)
				}
				if !errors.Is(err, ErrNoGPUInfo) {
					t.Errorf("err = %v, want ErrNoGPUInfo", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.wantGiB {
				t.Errorf("got %.4f GiB, want %.4f GiB", got, tc.wantGiB)
			}
		})
	}
}

func TestCheck_NoEstimate(t *testing.T) {
	// estimatedGiB <= 0: short-circuit, never call probe.
	called := false
	probe := func(context.Context) (float64, error) {
		called = true
		return 0, nil
	}
	res := Check(context.Background(), probe, 0, DefaultSlopGiB)
	if !res.OK {
		t.Errorf("OK = false, want true when estimate is zero")
	}
	if called {
		t.Errorf("probe was called for zero estimate")
	}
}

func TestCheck_Sufficient(t *testing.T) {
	probe := func(context.Context) (float64, error) { return 26.0, nil }
	res := Check(context.Background(), probe, 22.0, DefaultSlopGiB)
	if !res.OK {
		t.Errorf("OK = false, want true (free=26 >= est=22)")
	}
	if res.Skipped {
		t.Errorf("Skipped = true, want false")
	}
	if res.FreeGiB != 26.0 || res.EstimatedGiB != 22.0 {
		t.Errorf("res = %+v", res)
	}
}

func TestCheck_SlopRescue(t *testing.T) {
	// Free is just barely below the estimate, but the slop covers it.
	probe := func(context.Context) (float64, error) { return 21.7, nil }
	res := Check(context.Background(), probe, 22.0, DefaultSlopGiB)
	if !res.OK {
		t.Errorf("OK = false, want true (free=21.7 + slop=0.5 >= est=22)")
	}
}

// Short of free memory is a warning, not a refusal: free memory is mostly
// reclaimable cache on a unified-memory host and can be freed by another
// tenant exiting on a discrete GPU, so the start proceeds loudly.
func TestCheck_TightWarnsButProceeds(t *testing.T) {
	probe := func(context.Context) (float64, error) { return 14.3, nil }
	// Capacity is pinned, not read from the host: on a 16 GiB CI runner a
	// 22 GiB estimate really does exceed capacity, so leaving it ambient
	// made this assertion depend on the machine.
	capacity := func(context.Context) (float64, error) { return 64.0, nil }
	res := CheckWith(context.Background(), probe, capacity, 22.0, DefaultSlopGiB)
	if !res.OK {
		t.Errorf("OK = false, want true — being short of free memory must not block")
	}
	if !res.Warn {
		t.Errorf("Warn = false, want true (free=14.3 << est=22)")
	}
	if !strings.Contains(res.Message, "14.3") || !strings.Contains(res.Message, "22.0") {
		t.Errorf("message %q missing expected numbers", res.Message)
	}
}

// The one hard stop: an estimate larger than the machine's whole capacity
// cannot come good no matter what is evicted.
func TestCheck_ExceedsCapacityFails(t *testing.T) {
	probe := func(context.Context) (float64, error) { return 1.0, nil }
	capacity := func(context.Context) (float64, error) { return 64.0, nil }
	res := CheckWith(context.Background(), probe, capacity, 128.0, DefaultSlopGiB)
	if res.OK {
		t.Errorf("OK = true, want false for an estimate beyond total capacity")
	}
	if res.Warn {
		t.Errorf("Warn = true, want false — this is a hard failure, not a warning")
	}
	if !strings.Contains(res.Message, "in total") {
		t.Errorf("message %q should explain it exceeds total capacity", res.Message)
	}
}

func TestCheck_NoGPUInfo(t *testing.T) {
	probe := func(context.Context) (float64, error) { return 0, ErrNoGPUInfo }
	res := Check(context.Background(), probe, 22.0, DefaultSlopGiB)
	if !res.OK {
		t.Errorf("OK = false, want true (Skipped path)")
	}
	if !res.Skipped {
		t.Errorf("Skipped = false, want true")
	}
}

func TestCheck_OtherProbeError(t *testing.T) {
	// A non-ErrNoGPUInfo error is still treated as skip-and-warn so we
	// never block users on a flaky tool.
	boom := errors.New("nvidia-smi exploded")
	probe := func(context.Context) (float64, error) { return 0, boom }
	res := Check(context.Background(), probe, 22.0, DefaultSlopGiB)
	if !res.OK || !res.Skipped {
		t.Errorf("res = %+v, want OK+Skipped", res)
	}
	if !strings.Contains(res.Message, "nvidia-smi exploded") {
		t.Errorf("message = %q, want underlying error preserved", res.Message)
	}
}
