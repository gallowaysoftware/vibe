package observed

import (
	"encoding/json"
	"testing"
)

func TestZeroValueIsUnknown(t *testing.T) {
	var v Value[int]
	if v.IsKnown() {
		t.Fatal("the zero value reported a measurement; the whole type is that it does not")
	}
	if n, ok := v.Observed(); ok || n != 0 {
		t.Fatalf("Observed() = (%d, %v), want (0, false)", n, ok)
	}
	if got := v.OrElse(-1); got != -1 {
		t.Fatalf("OrElse(-1) = %d on an unknown", got)
	}
	if got := v.String(); got != "unknown" {
		t.Fatalf("String() = %q, want %q — a status line that renders an unknown as 0 is the original defect", got, "unknown")
	}
}

// TestAKnownZeroIsNotAnUnknown is the distinction the fleet keeps losing:
// llama-swap reporting zero in-flight requests is a positive claim of
// idleness, and no evidence at all is not.
func TestAKnownZeroIsNotAnUnknown(t *testing.T) {
	known := Known(0)
	if !known.IsKnown() {
		t.Fatal("a measured zero read as no measurement")
	}
	if n, ok := known.Observed(); !ok || n != 0 {
		t.Fatalf("Observed() = (%d, %v), want (0, true)", n, ok)
	}
	if got := known.OrElse(99); got != 0 {
		t.Fatalf("OrElse overrode a real measurement: %d", got)
	}
	var unknown Value[int]
	if known == unknown {
		t.Fatal("a measured zero compares equal to an unmeasured one")
	}
}

func TestJSONSpellsAbsenceAsNull(t *testing.T) {
	var unknown Value[int]
	b, err := json.Marshal(struct {
		N Value[int] `json:"n"`
	}{unknown})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"n":null}` {
		t.Fatalf("marshalled %s, want a null — a state document that spells absence 0 hands every reader the same false measurement", b)
	}
	b, err = json.Marshal(struct {
		N Value[int] `json:"n"`
	}{Known(3)})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"n":3}` {
		t.Fatalf("marshalled %s, want {\"n\":3}", b)
	}

	var back struct {
		N Value[int] `json:"n"`
	}
	if err := json.Unmarshal([]byte(`{"n":null}`), &back); err != nil {
		t.Fatal(err)
	}
	if back.N.IsKnown() {
		t.Fatal("null round-tripped into a measurement")
	}
	if err := json.Unmarshal([]byte(`{"n":7}`), &back); err != nil {
		t.Fatal(err)
	}
	if n, ok := back.N.Observed(); !ok || n != 7 {
		t.Fatalf("Observed() = (%d, %v) after unmarshal, want (7, true)", n, ok)
	}
	if err := json.Unmarshal([]byte(`{}`), &back); err != nil {
		t.Fatal(err)
	}
	if !back.N.IsKnown() {
		t.Fatal("an absent field cleared a value that was already set; UnmarshalJSON is not called for one, so this asserts the shape of the contract")
	}
}
