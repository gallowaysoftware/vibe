package contentkit

import (
	"bytes"
	"strings"
	"testing"
)

func TestEnumerateTemplate(t *testing.T) {
	got := enumerateTemplate("polish", "shots", "idx",
		[]string{"image_prompt", "motion", "narration", "speaker", "voice_id"})

	// Behaviour-critical pieces (whitespace-insensitive): the array source, the
	// index stamp, and each field copied verbatim via toJSON — matching the
	// hand-rolled templates this replaces in worldsmith + brainrot.
	wants := []string{
		`{{- $items := index (parseJSON .stages.polish.output) "shots" -}}`,
		`{{- range $i, $it := $items -}}`,
		`"idx": {{ $i }}`,
		`"image_prompt": {{ toJSON (index $it "image_prompt") }}`,
		`"voice_id": {{ toJSON (index $it "voice_id") }}`,
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("template missing %q\n---\n%s", w, got)
		}
	}
	// Must be a single {"items": [ ... ]} wrapper.
	if !strings.HasPrefix(got, `{"items": [`) || !strings.HasSuffix(strings.TrimSpace(got), `] }`) {
		t.Errorf("template not wrapped as items array:\n%s", got)
	}
}

func TestEnumerateTemplateDefaults(t *testing.T) {
	// segments-style use (different array/index keys, different fields).
	got := enumerateTemplate("showrunner", "segments", "id", []string{"host", "voice_id", "text"})
	if !strings.Contains(got, `(parseJSON .stages.showrunner.output) "segments"`) {
		t.Errorf("wrong array source: %s", got)
	}
	if !strings.Contains(got, `"id": {{ $i }}`) {
		t.Errorf("wrong index key: %s", got)
	}
}

func TestReviewLoopAcceptRejectSkipQuit(t *testing.T) {
	items := []ReviewItem{
		{ID: "alpha", Title: "Alpha", Body: "a"},
		{ID: "beta", Title: "Beta", Body: "b"},
		{ID: "gamma", Title: "Gamma", Body: "c"},
		{ID: "delta", Title: "Delta", Body: "d"},
	}
	var accepted, discarded []string
	act := ReviewActions{
		Accept:  func(it ReviewItem) error { accepted = append(accepted, it.ID); return nil },
		Discard: func(it ReviewItem) error { discarded = append(discarded, it.ID); return nil },
	}
	// alpha accept, beta reject, gamma skip, then quit (delta never reached).
	in := strings.NewReader("a\nr\ns\nq\n")
	var out bytes.Buffer
	res, err := ReviewLoop(in, &out, items, act)
	if err != nil {
		t.Fatalf("ReviewLoop: %v", err)
	}
	if res.Accepted != 1 || res.Discarded != 1 || res.Skipped != 1 {
		t.Errorf("tally wrong: %+v", res)
	}
	if len(accepted) != 1 || accepted[0] != "alpha" {
		t.Errorf("accepted = %v, want [alpha]", accepted)
	}
	if len(discarded) != 1 || discarded[0] != "beta" {
		t.Errorf("discarded = %v, want [beta]", discarded)
	}
}

func TestReviewLoopEndOfInputLeavesStaged(t *testing.T) {
	items := []ReviewItem{{ID: "x", Title: "X", Body: "x"}}
	called := false
	act := ReviewActions{Accept: func(ReviewItem) error { called = true; return nil }}
	res, err := ReviewLoop(strings.NewReader(""), &bytes.Buffer{}, items, act)
	if err != nil {
		t.Fatal(err)
	}
	if called || res.Accepted != 0 {
		t.Errorf("EOF should leave items staged, got accepted=%d called=%v", res.Accepted, called)
	}
}
