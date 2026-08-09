package modeltry

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/swaptest"
	"github.com/gallowaysoftware/vibe/internal/vibe/benchreplay"
	"github.com/stretchr/testify/require"
)

// fleet-control C25, the half of U3 and U5 that lives on this side of the
// seam: the ordering the trial journal enforces, and the journal itself as
// a leak surface.
//
// The journal matters more here than anywhere else in the phase. It is a
// FILE, `vibe model try status` re-prints it forever, and it is the one
// place a replay's output is persisted — so "the Report cannot carry a
// body" is a claim this package is entitled to, and this file is where it
// is checked against a real journal on a real disk.

// c25Marker is the synthetic "private prompt". Every fragment of it that
// requireNoC25Marker searches for has to be a string that CANNOT occur in
// a journal by coincidence, which is why the door code carries a suffix.
//
// It read `the door code is 7781` until 2026-08-09, and the bare `"7781"`
// in the fragment list matched a journal that had leaked nothing: the
// document is full of numbers — durations in nanoseconds, Unix stamps,
// tok/s floats — and one of them contained those four digits about 1.5%
// of the time (measured: 3 failures in 200 runs, always this fragment).
// A leak assertion that fires on a coincidence is worse than useless; it
// is a guard people learn to re-run, which is how a real red one gets
// waved through.
const c25Marker = "PRIVATE-PROMPT-4d21ba the door code is 7781-4d21ba and my mother's maiden name is"

// c25Rig drives the real sequence to `applied` with both models resident,
// which is the state Measure requires and the state a harvest is too late
// for.
func c25Rig(t *testing.T) (*rig, *Runner, *Trial) {
	t.Helper()
	r := smallRig(t)
	run := r.runner()
	ctx := t.Context()
	tr, def, err := run.Plan(ctx, planReq())
	require.NoError(t, err)
	require.NoError(t, run.Fetch(ctx, tr, nil))
	require.NoError(t, run.Stage(tr, def))
	return r, run, tr
}

func c25Apply(t *testing.T, r *rig, run *Runner, tr *Trial) {
	t.Helper()
	// swaptest does not model llama-swap's JIT load, so the post-warm
	// state a real cell reaches is set here — the same substitution
	// TestMeasureProbesBothSidesAndSaysWhatItIsNot makes, for the same
	// reason (C8's cardinal rule refuses a probe of a non-resident model).
	r.swap.SetModelState(tr.Def, "ready")
	r.swap.SetModelState(incName, "ready")
	_, err := run.Apply(t.Context(), tr)
	require.NoError(t, err)
}

// ── U5: the harvest happens before the apply ────────────────────────────

// TestHarvestIsRefusedOnceTheTrialIsApplied is the ordering, as a
// mechanism rather than a convention.
//
// The apply writes the cell's llama-swap config; `-watch-config` does not
// mutate a running server, it builds a new one, and the new one allocates
// a fresh EMPTY capture buffer. So a harvest after the apply is not late,
// it is impossible — and a run that did it anyway would replay an empty
// buffer, which is a score with no evidence behind it.
func TestHarvestIsRefusedOnceTheTrialIsApplied(t *testing.T) {
	r, run, tr := c25Rig(t)
	ctx := t.Context()

	// Before the apply: a capture exists and the harvest takes it.
	id := r.swap.Request(incName, "/v1/chat/completions",
		swaptest.Tokens{Input: 100, Output: 40, Cache: 0, Draft: -1, DraftAcc: -1}, 0)
	r.swap.SetCapture(id, syntheticCapture())

	set, err := run.Harvest(ctx, tr, benchreplay.Options{})
	require.NoError(t, err)
	require.Equal(t, 1, set.Len())

	// After the apply: refused by name, with the reload named as the cause.
	c25Apply(t, r, run, tr)
	require.Equal(t, StateApplied, tr.State)
	_, err = run.Harvest(ctx, tr, benchreplay.Options{})
	require.ErrorIs(t, err, benchreplay.ErrAlreadyApplied)
	require.Contains(t, err.Error(), "capture buffer")

	// And from `measured` too — a re-run of a finished trial must not
	// harvest either.
	tr.State = StateMeasured
	_, err = run.Harvest(ctx, tr, benchreplay.Options{})
	require.ErrorIs(t, err, benchreplay.ErrAlreadyApplied)
}

// TestMeasureCannotObtainASampleOfItsOwn is the type-level half of the
// same rule. Measure takes the sample as a PARAMETER, and
// benchreplay.Harvest is the only producer of one, so there is no path
// from inside the measurement to a buffer the apply has already emptied.
// A trial that reaches Measure without a sample measures throughput and
// says nothing about tool calls.
func TestMeasureCannotObtainASampleOfItsOwn(t *testing.T) {
	r, run, tr := c25Rig(t)
	c25Apply(t, r, run, tr)

	cmp, err := run.Measure(t.Context(), tr, nil)
	require.NoError(t, err)
	require.Nil(t, cmp.Replay)
	require.Empty(t, cmp.ReplayNote, "no replay was ASKED for, so there is nothing to explain")
	require.Greater(t, cmp.Incumbent.Value, 0.0, "the throughput comparison still happens")
	require.Greater(t, cmp.Trial.Value, 0.0)

	// The C18 caveat that C25 exists to fill is still printed when there
	// is no replay, and only then.
	joined := strings.Join(cmp.Caveats, " ")
	require.Contains(t, joined, "Throughput is not quality")
}

// ── U3 (journal half): a marker in a capture reaches no file ────────────

// TestReplayScoresReachTheJournalAndCaptureTextDoesNot drives a real
// replay through the real Measure, with a capture whose every text field
// is a marker, and reads the JOURNAL back off disk the way a later
// process would.
func TestReplayScoresReachTheJournalAndCaptureTextDoesNot(t *testing.T) {
	r, run, tr := c25Rig(t)
	ctx := t.Context()

	for range 3 {
		id := r.swap.Request(incName, "/v1/chat/completions",
			swaptest.Tokens{Input: 100, Output: 40, Cache: 0, Draft: -1, DraftAcc: -1}, 0)
		r.swap.SetCapture(id, syntheticCapture())
	}
	set, err := run.Harvest(ctx, tr, benchreplay.Options{})
	require.NoError(t, err)
	require.Equal(t, 3, set.Len())

	c25Apply(t, r, run, tr)
	cmp, err := run.Measure(ctx, tr, set)
	require.NoError(t, err)

	// The replay HAPPENED — otherwise the leak assertions below would pass
	// because nothing was measured, which is the vacuous-gate failure this
	// plan has already shipped once.
	require.NotNil(t, cmp.Replay, "note: %q", cmp.ReplayNote)
	require.Equal(t, 3, cmp.Replay.Sample.Replayable)
	require.Equal(t, 3, cmp.Replay.Incumbent.Requests)
	require.Equal(t, 3, cmp.Replay.Candidate.Requests)
	require.Equal(t, testCell, cmp.Replay.Cell)

	// And both C8 probes still ran, from inside the replay's warm hook.
	require.Equal(t, incName, cmp.Incumbent.Model)
	require.Equal(t, tr.Def, cmp.Trial.Model)

	// The journal, read off disk the way `vibe model try status` reads it.
	raw, err := os.ReadFile(r.statePath)
	require.NoError(t, err)
	requireNoC25Marker(t, "the trial journal on disk", string(raw))

	var reread Trial
	require.NoError(t, json.Unmarshal(raw, &reread))
	require.NotNil(t, reread.Result)
	require.NotNil(t, reread.Result.Replay, "the SCORES are journalled; only the sample is not")
	require.Equal(t, 3, reread.Result.Replay.Sample.Replayable)

	// The rendered status, which is the other surface a journal reaches.
	var out strings.Builder
	reread.RenderStatus(&out)
	requireNoC25Marker(t, "vibe model try status", out.String())
	require.Contains(t, out.String(), "replay:")
}

// TestAFailedReplayIsNotedAndNeverSilent pins the other direction: a
// replay that was asked for and produced nothing must say so on the same
// screen as the throughput number, because an absent replay reads as a
// passing one otherwise.
func TestAFailedReplayIsNotedAndNeverSilent(t *testing.T) {
	r, run, tr := c25Rig(t)
	ctx := t.Context()
	id := r.swap.Request(incName, "/v1/chat/completions",
		swaptest.Tokens{Input: 10, Output: 4, Cache: 0, Draft: -1, DraftAcc: -1}, 0)
	r.swap.SetCapture(id, syntheticCapture())
	set, err := run.Harvest(ctx, tr, benchreplay.Options{})
	require.NoError(t, err)

	// Apply WITHOUT making the trial resident, so benchreplay's own
	// residency check refuses the candidate side.
	r.swap.SetModelState(incName, "ready")
	r.swap.SetModelState(tr.Def, "stopped")
	_, err = run.Apply(ctx, tr)
	require.NoError(t, err)
	tr.Live = true // the catalog half; the residency half is what is under test

	cmp, err := run.Measure(ctx, tr, set)
	require.NoError(t, err, "a failed replay must not cost the throughput comparison")
	require.Nil(t, cmp.Replay)
	require.Contains(t, cmp.ReplayNote, "no score")
	requireNoC25Marker(t, "the replay note", cmp.ReplayNote)

	var out strings.Builder
	cmp.Render(&out)
	require.Contains(t, out.String(), "replay:")
	require.Contains(t, strings.Join(cmp.Caveats, " "), "An absent replay is not a passing one")
}

// syntheticCapture is written BY HAND, here. Nothing in this repo may hold
// a real one: swaptest's recorder refuses the endpoint by name and
// TestNoFixtureContainsACapture walks the tree for the shape.
func syntheticCapture() swaptest.Capture {
	req := `{"model":"` + incName + `","temperature":0.7,"max_tokens":64,"stream":true,
	  "messages":[{"role":"system","content":"` + c25Marker + `"},
	              {"role":"user","content":"` + c25Marker + `"}],
	  "tools":[{"type":"function","function":{"name":"read_file","description":"` + c25Marker + `",
	    "parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}}]}`
	resp := "data: " + `{"choices":[{"index":0,"delta":{"role":"assistant","content":"` + c25Marker + `"}}]}` + "\n\n" +
		"data: " + `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"completion_tokens":9}}` + "\n\n" +
		"data: [DONE]\n\n"
	return swaptest.Capture{
		ReqPath:  "/v1/chat/completions",
		ReqBody:  []byte(req),
		RespBody: []byte(resp),
	}
}

func requireNoC25Marker(t *testing.T, where, body string) {
	t.Helper()
	// Every fragment here must be impossible to hit by coincidence — see
	// c25Marker. "7781-4d21ba" rather than "7781" for exactly that reason.
	for _, frag := range []string{c25Marker, "door code", "7781-4d21ba", "maiden name", "PRIVATE-PROMPT-4d21ba"} {
		if strings.Contains(body, frag) {
			t.Fatalf("%s contains %q — that is the verbatim text of a captured request, and this phase emits "+
				"only scores (fleet-control C25 §3)", where, frag)
		}
	}
}

var _ = context.Background
