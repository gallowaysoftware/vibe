package vamp

import "testing"

func TestWordCount_Basic(t *testing.T) {
	if got := wordCountTemplate("hello world"); got != 2 {
		t.Errorf("got %d, want 2", got)
	}
}

func TestWordCount_MultilineParagraphs(t *testing.T) {
	text := "First paragraph here.\n\nSecond paragraph with more words in it."
	if got := wordCountTemplate(text); got != 10 {
		t.Errorf("got %d, want 10", got)
	}
}

func TestWordCount_LeadingTrailingWhitespace(t *testing.T) {
	if got := wordCountTemplate("   spaced   words   "); got != 2 {
		t.Errorf("got %d, want 2", got)
	}
}

func TestWordCount_Empty(t *testing.T) {
	if got := wordCountTemplate(""); got != 0 {
		t.Errorf("got %d, want 0", got)
	}
	if got := wordCountTemplate("   \n\n   "); got != 0 {
		t.Errorf("whitespace-only: got %d, want 0", got)
	}
}

func TestSplitSentences_Short(t *testing.T) {
	// Under threshold → returned as one chunk.
	got := splitSentencesTemplate("She breathed in.", 300)
	if got != `["She breathed in."]` {
		t.Errorf("got %s", got)
	}
}

func TestSplitSentences_MultiSentence(t *testing.T) {
	// Greedy packs as many whole sentences as fit. First two combine
	// to 29 chars (under 30); third starts a new chunk.
	in := "She breathed in. She held it. She let it out."
	got := splitSentencesTemplate(in, 30)
	want := `["She breathed in. She held it.","She let it out."]`
	if got != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
}

func TestSplitSentences_GreedyPack(t *testing.T) {
	// Threshold large enough to pack 2 sentences per chunk.
	in := "One. Two. Three. Four."
	got := splitSentencesTemplate(in, 10)
	// "One. Two." = 9 chars, "Three." = 6, "Four." = 5
	// Greedy: pack One+Two (9), then Three+Four (11 over budget, so Three alone then Four).
	// Actually: "Three." = 6 + 1 + "Four." = 12, over 10 → split.
	want := `["One. Two.","Three.","Four."]`
	if got != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
}

// TestSplitSentences_SingleLongSentenceIsSubSplitToBudget records a
// deliberate behaviour change. This test used to assert the opposite —
// that a single 72-char sentence over a 30-char limit comes back as
// ONE chunk, on the argument that splitting mid-clause reads worse
// than one long TTS call. That argument holds for a sentence 10% over
// and collapses at 9x, which is what the helper actually produced on
// every input shape whose sentence boundaries the detector missed. A
// budget that only binds when detection succeeds is not a budget, and
// the caller discovered the overrun as a rejected request after the
// pipeline had run.
//
// The break is at a word boundary, not mid-word: splitToBudget's
// cascade prefers sentence, then clause, then word, and only cuts a
// word when there is no whitespace in the budget at all.
func TestSplitSentences_SingleLongSentenceIsSubSplitToBudget(t *testing.T) {
	in := "The room is a cube with rounded corners and a magnetic seal on the door."
	got := splitSentencesTemplate(in, 30)
	want := `["The room is a cube with","rounded corners and a","magnetic seal on the door."]`
	if got != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
}

func TestMulInt(t *testing.T) {
	cases := []struct {
		n    int
		m    float64
		want int
	}{
		{3637, 1.5, 5455},
		{3637, 2.0, 7274},
		{0, 1.5, 0},
		{100, 0.5, 50},
	}
	for _, c := range cases {
		if got := mulIntTemplate(c.n, c.m); got != c.want {
			t.Errorf("mulInt(%d, %g) = %d, want %d", c.n, c.m, got, c.want)
		}
	}
}
