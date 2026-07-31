package vamp

import "testing"

// TestTTSNormalize_HyphenedAlphanumeric is the regression test for
// the heard-aloud bug that motivated this whole subsystem: Kokoro
// reading "F-150" as "eff minus one fifty".
func TestTTSNormalize_HyphenedAlphanumeric(t *testing.T) {
	got, err := ttsNormalizeTemplate("Drive the F-150 to the lot", "")
	if err != nil {
		t.Fatal(err)
	}
	want := "Drive the F 150 to the lot"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestTTSNormalize_DiOverD0(t *testing.T) {
	got, err := ttsNormalizeTemplate("compute the di/d0 ratio", "")
	if err != nil {
		t.Fatal(err)
	}
	want := "compute the d i over d 0 ratio"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestTTSNormalize_TempAndPercent(t *testing.T) {
	got, err := ttsNormalizeTemplate("heat to 70°C and reach 50% conversion", "")
	if err != nil {
		t.Fatal(err)
	}
	want := "heat to 70 degrees C and reach 50 percent conversion"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestTTSNormalize_UnitsExpandedAcronymsUntouched(t *testing.T) {
	// Unit abbreviations expand (default rules); bare domain acronyms
	// pass through untouched — per-domain spell-outs belong in the
	// owning pipeline's tts_rules.yaml, not the shared defaults.
	got, err := ttsNormalizeTemplate("dose 5 mg/L of LAA", "")
	if err != nil {
		t.Fatal(err)
	}
	want := "dose 5 milligrams per litre of LAA"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestTTSNormalize_DoesntTouchLessonHyphens(t *testing.T) {
	// Lesson 10 - Cereal Wort Production should NOT be rewritten —
	// the hyphen has spaces around it; Kokoro reads the space and
	// the rule's regex anchors require no-space-before-the-hyphen.
	got, err := ttsNormalizeTemplate("Lesson 10 - Cereal Wort Production", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Lesson 10 - Cereal Wort Production" {
		t.Errorf("hyphen-with-spaces should be left alone; got %q", got)
	}
}

func TestTTSNormalize_FractionToOver(t *testing.T) {
	got, err := ttsNormalizeTemplate("split the batch 1/2 and 3/4", "")
	if err != nil {
		t.Fatal(err)
	}
	want := "split the batch 1 over 2 and 3 over 4"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}
