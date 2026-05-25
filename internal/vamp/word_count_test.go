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
