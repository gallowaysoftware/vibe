package vamp

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
)

// traversalFixture builds a run dir with a secret one level ABOVE it, so
// the only way to name the secret from inside the run dir is to climb
// out. Returns the run dir and the secret's basename.
//
// Same construction as lessonEscapeFixture next door and for the same
// reason: a target that is a sibling of the root cannot be reached by
// accident, so a test that reaches it has proved the climb and not
// merely a path join.
func traversalFixture(t *testing.T) (runDir, secretName string) {
	t.Helper()
	base := t.TempDir()
	runDir = filepath.Join(base, "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "id_rsa"), []byte("-----BEGIN PRIVATE KEY-----\nSECRET\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// One file INSIDE the run dir, so every case below can also show the
	// legitimate read still works and the guard is not just "refuse
	// everything".
	if err := os.WriteFile(filepath.Join(runDir, "inside.txt"), []byte("INSIDE"), 0o644); err != nil {
		t.Fatal(err)
	}
	return runDir, "id_rsa"
}

// TestReadHelpers_RefuseATraversalOutOfTheDirectoryTheyWereGiven pins the
// READ half of the containment rule this executor already applies to
// writes.
//
// The asymmetry is the point. A stage's rendered `output:` goes through
// ensureUnderRunDir and a lesson name goes through lessonImageDir, but
// readFile / readFiles / readFilesOrEmpty / readFileBatch / enumerateDirs
// and joinPath accepted a ".." freely — on the same executor, fed from
// the same place (a prior stage's output is whatever the model wrote).
// `{{ readFile (printf "%s/../id_rsa" .runDir) }}` returned a private key
// into the rendered prompt.
//
// What this does NOT claim, so nobody later "completes" it into a
// confinement that breaks real pipelines: an ABSOLUTE path is still
// legal, because reading a user's on-disk corpus is these helpers'
// documented job. See ensureNoTraversal.
func TestReadHelpers_RefuseATraversalOutOfTheDirectoryTheyWereGiven(t *testing.T) {
	runDir, secret := traversalFixture(t)
	// Built by concatenation, NOT filepath.Join: Join cleans, so it would
	// resolve the ".." here and hand the helpers a path with nothing left
	// to refuse — the same reason joinPath itself needed a guard. This is
	// the literal string a template's `printf "%s/../x"` produces.
	sep := string(filepath.Separator)
	escape := runDir + sep + ".." + sep + secret
	escapeGlob := runDir + sep + ".." + sep + "*_rsa"

	t.Run("readFile", func(t *testing.T) {
		got, err := readFileTemplate(escape)
		assertTraversalRefused(t, got, err)
		if in, err := readFileTemplate(filepath.Join(runDir, "inside.txt")); err != nil || in != "INSIDE" {
			t.Errorf("legitimate read regressed: %q %v", in, err)
		}
	})
	t.Run("readFiles", func(t *testing.T) {
		got, err := readFilesTemplate(escapeGlob)
		assertTraversalRefused(t, got, err)
		if in, err := readFilesTemplate(filepath.Join(runDir, "*.txt")); err != nil || !strings.Contains(in, "INSIDE") {
			t.Errorf("legitimate glob regressed: %q %v", in, err)
		}
	})
	t.Run("readFilesOrEmpty", func(t *testing.T) {
		// The permissive contract is about a glob that legitimately
		// matches nothing. An escape must not be folded into that silence.
		got, err := readFilesOrEmptyTemplate(escapeGlob)
		assertTraversalRefused(t, got, err)
		if in, err := readFilesOrEmptyTemplate(filepath.Join(runDir, "nope*")); err != nil || in != "" {
			t.Errorf("permissive no-match contract regressed: %q %v", in, err)
		}
	})
	t.Run("readFileBatch", func(t *testing.T) {
		got, err := readLessonsTemplate(escapeGlob, 1, 1)
		assertTraversalRefused(t, got, err)
	})
	t.Run("enumerateDirs", func(t *testing.T) {
		got, err := enumerateLessonsTemplate(runDir + sep + ".." + sep + "*")
		assertTraversalRefused(t, got, err)
	})
	t.Run("joinPath", func(t *testing.T) {
		// joinPath is the natural composer for the helpers above, and
		// filepath.Join RESOLVES ".." rather than rejecting it — so an
		// unguarded joinPath would hand readFile a perfectly clean path
		// that points outside the run dir, defeating every guard above.
		got, err := joinPathTemplate(runDir, "../../etc/passwd")
		assertTraversalRefused(t, got, err)
		if p, err := joinPathTemplate(runDir, "sub", "x.txt"); err != nil || p != filepath.Join(runDir, "sub", "x.txt") {
			t.Errorf("legitimate join regressed: %q %v", p, err)
		}
	})

	// And through a rendered template, which is the surface a pipeline
	// actually uses — a guard that only fires on a direct Go call is a
	// guard the pipeline never meets.
	t.Run("through a rendered template", func(t *testing.T) {
		tmpl, err := template.New("t").Funcs(templateFuncs()).Parse(`{{ readFile (printf "%s/../id_rsa" .runDir) }}`)
		if err != nil {
			t.Fatal(err)
		}
		var sb strings.Builder
		err = tmpl.Execute(&sb, map[string]any{"runDir": runDir})
		if err == nil {
			t.Fatalf("template read a file outside the run dir: %q", sb.String())
		}
		if strings.Contains(sb.String(), "PRIVATE KEY") {
			t.Errorf("the key reached the prompt: %q", sb.String())
		}
		if !strings.Contains(err.Error(), `climbs out of its own prefix`) {
			t.Errorf("want a traversal refusal, got %v", err)
		}
	})
}

func assertTraversalRefused(t *testing.T, got string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a refusal, got %q", got)
	}
	if !errors.Is(err, errTemplatePathTraversal) {
		t.Errorf("want errTemplatePathTraversal, got %v", err)
	}
	if strings.Contains(got, "SECRET") || strings.Contains(got, "PRIVATE KEY") {
		t.Errorf("leaked out-of-tree content: %q", got)
	}
}

// TestEnumerateDirs_NestedGlobKeepsTheParentSegment.
//
// enumerateDirs returned filepath.Base, which is the last segment and not
// "relative" to anything. Over a module-organised curriculum
// (root/*/Lesson_*) four real lessons came back as
// ["Lesson_1","Lesson_2","Lesson_1","Lesson_2"] — parent lost, leaf
// duplicated — none of which resolves under the root. The image fan-out
// that consumes them then returned `[]` with a nil error and the foreach
// logged "no items to run": a green pipeline that did nothing.
//
// The flat case is asserted alongside because it is the case the doc
// example uses and the one the fix must not disturb.
func TestEnumerateDirs_NestedGlobKeepsTheParentSegment(t *testing.T) {
	root := t.TempDir()
	for _, mod := range []string{"Module_1", "Module_2"} {
		for _, les := range []string{"Lesson_1", "Lesson_2"} {
			d := filepath.Join(root, mod, les)
			if err := os.MkdirAll(filepath.Join(d, "images"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(d, "lesson.md"), []byte("# x"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(d, "images", "fig.png"), []byte(mod+les), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	got, err := enumerateLessonsTemplate(filepath.Join(root, "*", "Lesson_*"))
	if err != nil {
		t.Fatal(err)
	}
	want := `["Module_1/Lesson_1","Module_1/Lesson_2","Module_2/Lesson_1","Module_2/Lesson_2"]`
	if got != want {
		t.Errorf("nested glob:\n got %s\nwant %s", got, want)
	}

	// The end-to-end consequence, which is the reason the name shape
	// matters at all: the names must resolve back under the root.
	pairs, err := enumerateImagePairsTemplate(root, got)
	if err != nil {
		t.Fatalf("nested lesson names did not resolve: %v", err)
	}
	for _, mod := range []string{"Module_1", "Module_2"} {
		if !strings.Contains(pairs, mod) {
			t.Errorf("fan-out lost %s: %s", mod, pairs)
		}
	}
	if n := strings.Count(pairs, `"image":"fig.png"`); n != 4 {
		t.Errorf("want 4 images enumerated, got %d: %s", n, pairs)
	}

	// A flat glob still names its own last segment — the doc's example
	// shape, and the case the old filepath.Base got right.
	flat, err := enumerateLessonsTemplate(filepath.Join(root, "Module_1", "Lesson_*"))
	if err != nil {
		t.Fatal(err)
	}
	if flat != `["Lesson_1","Lesson_2"]` {
		t.Errorf("flat glob regressed: %s", flat)
	}
}

// TestLessonHelpers_RefuseALessonThatNamesNothing pins the RECEIVING half
// of the rule enumerateDirs states on the sending side ("a glob matching
// nothing is an error, because a stale root is indistinguishable from a
// curriculum with no lessons once the result is []").
//
// The lessons array need not come from enumerateDirs at all — the
// documented binding is .stages.list_lessons.output, i.e. a model's own
// JSON — so `[]` and `["Lesson_9999"]` both used to yield a well-formed
// empty fan-out with a nil error.
//
// The line this test walks, deliberately: a lesson that EXISTS and has no
// images/ is still zero units of work, correctly expressed, and stays a
// nil error. That case is pinned by
// TestEnumerateImagePairs_NoImagesIsEmptyArrayNotNull and must not move.
func TestLessonHelpers_RefuseALessonThatNamesNothing(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Lesson_1"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("empty array", func(t *testing.T) {
		for _, in := range []string{`[]`, "  [ ] "} {
			if got, err := enumerateImagePairsTemplate(root, in); err == nil {
				t.Errorf("enumerateImagePairs(%q) = %s, want a refusal", in, got)
			}
			if got, err := enumerateUniqueImagesTemplate(root, in); err == nil {
				t.Errorf("enumerateUniqueImages(%q) = %s, want a refusal", in, got)
			}
		}
	})

	t.Run("names a lesson that is not there", func(t *testing.T) {
		const in = `["Lesson_9999","Lesson_8888"]`
		got, err := enumerateImagePairsTemplate(root, in)
		if err == nil {
			t.Errorf("enumerateImagePairs = %s, want a refusal", got)
		} else if !errors.Is(err, errLessonNotFound) {
			t.Errorf("want errLessonNotFound, got %v", err)
		}
		got, err = enumerateUniqueImagesTemplate(root, in)
		if err == nil {
			t.Errorf("enumerateUniqueImages = %s, want a refusal", got)
		} else if !errors.Is(err, errLessonNotFound) {
			t.Errorf("want errLessonNotFound, got %v", err)
		}
		// The per-lesson helper: "" is a legitimate answer for a real
		// lesson, so only the hallucinated name is a fault.
		if _, err := imageDescriptionsForLessonTemplate(t.TempDir(), root, "Lesson_9999"); !errors.Is(err, errLessonNotFound) {
			t.Errorf("imageDescriptionsFor: want errLessonNotFound, got %v", err)
		}
	})

	t.Run("a real lesson with no images is still zero work, not a fault", func(t *testing.T) {
		got, err := enumerateImagePairsTemplate(root, `["Lesson_1"]`)
		if err != nil {
			t.Fatalf("a real lesson with no images/ must not be a fault: %v", err)
		}
		if got != "[]" {
			t.Errorf("got %q, want []", got)
		}
		if _, err := imageDescriptionsForLessonTemplate(t.TempDir(), root, "Lesson_1"); err != nil {
			t.Errorf("a real lesson with no images/ must not be a fault: %v", err)
		}
	})
}

// TestExtractSVGText_AdjacentTspansAreSeparated.
//
// The helper's stated job is to hand a vision model GROUND TRUTH for
// every number the SVG author put on the page, because the rasterised
// 896x896 tile may mis-render small type. Sibling <tspan>s — how Inkscape
// and matplotlib emit any wrapped or stacked label — were concatenated
// with nothing between them, so 12 and 34 arrived as "1234": a confident
// value that appears nowhere in the diagram, which is strictly worse than
// omitting it.
func TestExtractSVGText_AdjacentTspansAreSeparated(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	cases := []struct {
		name string
		svg  string
		want string
	}{
		{
			"stacked numbers",
			`<svg><text><tspan>12</tspan><tspan>34</tspan></text></svg>`,
			"12 34",
		},
		{
			"wrapped label",
			`<svg><text><tspan>Total</tspan><tspan>Revenue</tspan></text></svg>`,
			"Total Revenue",
		},
		{
			"a tspan after bare text",
			`<svg><text>Total<tspan>2026</tspan></text></svg>`,
			"Total 2026",
		},
		{
			// The separator must not double up where the author already
			// wrote one, or every label acquires drift.
			"author's own spacing survives",
			`<svg><text><tspan>Total </tspan><tspan>Revenue</tspan></text></svg>`,
			"Total Revenue",
		},
		{
			"a plain label is untouched",
			`<svg><text>plain label</text></svg>`,
			"plain label",
		},
		{
			"separate text elements still join with a pipe",
			`<svg><text>a</text><text>b</text></svg>`,
			"a | b",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractSVGTextTemplate(write(strings.ReplaceAll(tc.name, " ", "_")+".svg", tc.svg))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
