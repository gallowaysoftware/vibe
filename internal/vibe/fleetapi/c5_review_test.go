package fleetapi

// The adversarial review pass over C4+C5 (ground rule 9, gate 9). Each
// test here pins a behaviour the review found asserted somewhere —
// a commit message, a comment, an addendum — but proven nowhere:
// mutating the production code back left the package green.

import (
	"bytes"
	"runtime/pprof"
	"strings"
	"testing"
	"time"
)

// TestModelSetChangedDetectsDuplicateIDs pins the set comparison. The
// C5 review commit replaced a length+subset check because [A,B] → [A,A]
// reported "unchanged" and the render trigger was dropped, but shipped
// no test: reverting the fix left the whole package green. Duplicate ids
// are a protocol violation, and announces are untrusted input by C3's
// own threat note — the two facts together are exactly why this must be
// a set compare.
func TestModelSetChangedDetectsDuplicateIDs(t *testing.T) {
	ms := func(ids ...string) []AnnounceModel {
		out := make([]AnnounceModel, 0, len(ids))
		for _, id := range ids {
			out = append(out, AnnounceModel{ID: id, State: "ready"})
		}
		return out
	}
	cases := []struct {
		name       string
		prev, next []AnnounceModel
		want       bool
	}{
		{"identical", ms("a", "b"), ms("a", "b"), false},
		{"reordered", ms("a", "b"), ms("b", "a"), false},
		{"added", ms("a"), ms("a", "b"), true},
		{"removed", ms("a", "b"), ms("a"), true},
		{"swapped", ms("a", "b"), ms("a", "c"), true},
		// The regression: same length, every next id present in prev,
		// yet b stopped serving.
		{"duplicate hides a drop", ms("a", "b"), ms("a", "a"), true},
		{"duplicate hides an add", ms("a", "a"), ms("a", "b"), true},
		{"duplicate on both sides", ms("a", "a"), ms("a", "a"), false},
		{"empty to empty", nil, nil, false},
		{"empty to one", nil, ms("a"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := modelSetChanged(tc.prev, tc.next); got != tc.want {
				t.Errorf("modelSetChanged(%v, %v) = %v, want %v", tc.prev, tc.next, got, tc.want)
			}
		})
	}
}

// TestScheduleLoopStartsNoGoroutineForAnUnfireableSpec is the mechanical
// half of CC-4. The existing test asserts Close() returns quickly and
// calls that proof the goroutine was never started — it is not:
// scheduleEntryLoop exits on s.done immediately, so Close() returns fast
// either way, and deleting the gate left that test green. The goroutine
// profile names the frame, so this one actually fails when the gate goes.
func TestScheduleLoopStartsNoGoroutineForAnUnfireableSpec(t *testing.T) {
	probe := &warmProbe{}
	s := newWarmServer(t, []Cell{{Name: "heavy", URL: "http://127.0.0.1:1", Class: "always_on"}})
	cellOf := func(string) (string, error) { return "heavy", nil }
	s.startScheduleLoopWithConfig([]WarmScheduleEntry{
		{Cron: "0 0 30 2 *", Model: "never"}, // parses; matches no instant
		{Cron: "nonsense", Model: "invalid"}, // does not parse
	}, cellOf, time.UTC, probe.warm, "http://front.test")

	if got := goroutineDump(t); strings.Contains(got, "scheduleEntryLoop") {
		t.Error("a schedule goroutine was started for a spec that can never fire")
	}

	// The control: a fireable spec DOES get one, so the assertion above
	// is not passing because the frame name is simply never present.
	s.startScheduleLoopWithConfig([]WarmScheduleEntry{
		{Cron: "* * * * *", Model: "fires"},
	}, cellOf, time.UTC, probe.warm, "http://front.test")
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(goroutineDump(t), "scheduleEntryLoop") {
		if time.Now().After(deadline) {
			t.Fatal("control: a fireable spec started no goroutine either — the probe is blind")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func goroutineDump(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	if err := pprof.Lookup("goroutine").WriteTo(&buf, 1); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// TestWarmCtxCancelIsIdempotent pins the C5 review's own first finding:
// context.CancelFunc is documented as safe to call more than once, and
// warmCtx's closure closes a channel — a second call panicked. Both call
// sites call it once today, which is exactly how this survives until
// someone adds a third.
func TestWarmCtxCancelIsIdempotent(t *testing.T) {
	s := newWarmServer(t, []Cell{{Name: "heavy", URL: "http://127.0.0.1:1"}})
	ctx, cancel := s.warmCtx(time.Minute)
	cancel()
	cancel() // must not panic
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("cancel did not cancel the context")
	}
}

// TestWarmCtxCancelsOnClose pins the link CC-2 added: the context must
// die with the server, or an s.wg goroutine blocked in a warm holds
// Close() for the full warmTimeout.
func TestWarmCtxCancelsOnClose(t *testing.T) {
	s := newWarmServer(t, []Cell{{Name: "heavy", URL: "http://127.0.0.1:1"}})
	ctx, cancel := s.warmCtx(time.Hour)
	defer cancel()
	s.Close()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("warm context outlived Close()")
	}
}
