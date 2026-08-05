package swaptest_test

import (
	"encoding/json"
	"regexp"
	"sort"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/swaptest"
)

func parseSwapVersion(s string) string {
	m := regexp.MustCompile(`version:\s*(v?\d+)`).FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	return m[1]
}

func wiresWithRecordings(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, w := range swaptest.Wires() {
		if _, err := swaptest.LoadRecording(w); err == nil {
			out = append(out, w.String())
		}
	}
	return out
}

// TestEveryWireHasARecording keeps the two halves of the package from
// drifting apart: a Wire the double can speak but nothing recorded is a
// fake with no evidence behind it.
func TestEveryWireHasARecording(t *testing.T) {
	for _, w := range swaptest.Wires() {
		rec, err := swaptest.LoadRecording(w)
		if err != nil {
			t.Fatalf("%s has no RECORDED stamp: %v", w, err)
		}
		if rec.Version == "" || rec.At == "" || rec.CapturedFrom == "" {
			t.Errorf("%s RECORDED is missing provenance: %+v", w, rec)
		}
		if rec.Synthesized() && rec.Caveat == "" {
			t.Errorf("%s was synthesized from upstream source and carries no caveat; a fixture nobody captured must say so where it is read", w)
		}
	}
}

// TestFixtureShapeMatchesGenerated is the mechanism that keeps the double
// honest. The recordings are raw wire bytes; the double GENERATES frames,
// because a replay cannot answer a request a test just made. This asserts
// the generator emits the recorded SHAPE — same envelope keys, same double
// encoding, same inner key set at every level — so the two cannot drift
// without a failure.
//
// It compares key sets, never bytes: ids, timestamps and model names differ
// by construction, and a byte golden over those would flap and be silenced.
func TestFixtureShapeMatchesGenerated(t *testing.T) {
	for _, w := range swaptest.Wires() {
		t.Run(w.String(), func(t *testing.T) {
			recorded, err := swaptest.RecordedFrames(w, "events-request.sse")
			if err != nil {
				t.Fatalf("read recording: %v", err)
			}
			byType := map[string][]map[string]bool{}
			for _, f := range recorded {
				byType[f.Type] = append(byType[f.Type], keySet(t, f.Inner))
			}
			if len(byType["inflight"]) == 0 {
				t.Fatal("the recording carries no inflight frame; there is nothing to compare against")
			}

			cell := swaptest.NewCell(t, swaptest.WithWire(w))
			gen := captureFrames(t, cell, func() {
				cell.Request("qwen3.6-27b", "/v1/chat/completions", swaptest.Tokens{Input: 10, Output: 8}, 0)
			})
			genByType := map[string][]map[string]bool{}
			for _, f := range gen {
				genByType[f.Type] = append(genByType[f.Type], keySet(t, f.Inner))
			}

			for _, typ := range []string{"inflight", "activity"} {
				want := unionOfShapes(byType[typ])
				got := unionOfShapes(genByType[typ])
				if len(got) == 0 {
					t.Fatalf("the double emitted no %q frame; the recording has %d", typ, len(byType[typ]))
				}
				if !sameKeys(want, got) {
					t.Errorf("%s %q inner keys: recorded %v, generated %v — the double no longer resembles the recording",
						w, typ, sortedKeys(want), sortedKeys(got))
				}
			}
		})
	}
}

// TestRecordedFramesAreDoubleEncoded pins the shape that has caught out
// every hand-rolled fake in this module: the SSE data field is a JSON
// OBJECT whose "data" member is a JSON STRING containing JSON. A fake that
// nests a plain object there is silently ignored by the production parser,
// so the test passes without exercising anything.
func TestRecordedFramesAreDoubleEncoded(t *testing.T) {
	for _, w := range swaptest.Wires() {
		frames, err := swaptest.RecordedFrames(w, "events-connect.sse")
		if err != nil {
			t.Fatalf("%s: %v", w, err)
		}
		if len(frames) == 0 {
			t.Fatalf("%s: empty connect recording", w)
		}
		for i, f := range frames {
			var any map[string]json.RawMessage
			var arr []json.RawMessage
			if json.Unmarshal([]byte(f.Inner), &any) != nil && json.Unmarshal([]byte(f.Inner), &arr) != nil {
				t.Errorf("%s frame %d (%s): inner payload is not JSON: %q", w, i, f.Type, f.Inner)
			}
		}
		types := make([]string, 0, len(frames))
		for _, f := range frames {
			types = append(types, f.Type)
		}
		// Recorded live on v239: logData, logData, modelStatus, inflight,
		// the whole burst inside ~198ms of the connect.
		if types[len(types)-1] != "inflight" {
			t.Errorf("%s connect burst ends with %q, not the current-state inflight snapshot: %v", w, types[len(types)-1], types)
		}
	}
}

func keySet(t *testing.T, inner string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(inner), &obj); err != nil {
		return out
	}
	for k, v := range obj {
		out[k] = true
		// One level down is where the version difference lives: v239 nests
		// the entries under "requests", v247 under "request".
		var entries []map[string]json.RawMessage
		var entry map[string]json.RawMessage
		switch {
		case json.Unmarshal(v, &entries) == nil && len(entries) > 0:
			for ek := range entries[0] {
				out[k+"."+ek] = true
			}
		case json.Unmarshal(v, &entry) == nil:
			for ek := range entry {
				out[k+"."+ek] = true
			}
		}
	}
	return out
}

func unionOfShapes(shapes []map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, s := range shapes {
		for k := range s {
			out[k] = true
		}
	}
	return out
}

func sameKeys(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
