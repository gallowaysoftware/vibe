package vamp

import (
	"os"
	"path/filepath"
	"testing"
)

// The two halves of the cache contract, held together.
//
// stageCacheable is the ADVERTISEMENT — it decides whether the timing
// report prints `cache: hit/miss` for a stage at all, and it is the
// allow-list README and the JSON schema describe. computeStageCacheKey is
// the PERFORMANCE — an empty key means the runner does no Get and no Put.
// The two were switches over the same domain in different functions, and
// `pandoc` sat on the first from the day the type landed without ever
// appearing in the second: every pandoc stage reported `cache: miss` and
// re-ran a whole EPUB conversion on every run, while a comment in
// buildPandocArgs sorted its metadata keys "so the cache key (rendered
// argv) doesn't oscillate". Nobody was wrong about what the code should
// do; the two halves were just never made to answer to each other.

// cacheContractFixtures is one minimal, VALID stage per stage type.
//
// Valid is load-bearing: every fixture is run through Pipeline.Validate
// before it is used, so a fixture that has drifted into a shape vamp
// would reject cannot quietly stand in for the real thing. Files a key
// composer reads (comfyui's workflow) are written into dir.
func cacheContractFixtures(t *testing.T, dir string) map[StageType]*Stage {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "wf.json"), []byte(`{"3":{"inputs":{"seed":1}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return map[StageType]*Stage{
		StageTypeText:    {ID: "s", Type: StageTypeText, Prompt: "hello", Output: "s.txt", Capability: "chat"},
		StageTypeComfyUI: {ID: "s", Type: StageTypeComfyUI, Workflow: "wf.json", Parameters: map[string]string{"3.seed": "7"}, Output: "s.png", Capability: "image"},
		StageTypeAudio:   {ID: "s", Type: StageTypeAudio, Text: "hello", Voice: "en_GB-alba-medium", Output: "s.wav"},
		StageTypeFFmpeg:  {ID: "s", Type: StageTypeFFmpeg, FFmpegArgs: []string{"-i", "a.wav", "-y"}, Output: "s.mp3"},
		StageTypeYouTube: {ID: "s", Type: StageTypeYouTube, Video: "s.mp4", Title: "t", Description: "d", Output: "s.txt"},
		StageTypeWebhook: {ID: "s", Type: StageTypeWebhook, URL: "http://127.0.0.1:1/hook", Method: "POST", Body: map[string]any{"text": "done"}, Output: "s.txt"},
		StageTypeConfirm: {ID: "s", Type: StageTypeConfirm, Message: "ship it?", Output: "s.txt"},
		StageTypeRender:  {ID: "s", Type: StageTypeRender, Prompt: "rendered", Output: "s.txt"},
		StageTypeCompact: {ID: "s", Type: StageTypeCompact, Source: "long text", TargetChars: 100, Output: "s.txt", Capability: "chat"},
		StageTypePandoc:  {ID: "s", Type: StageTypePandoc, SourceFile: "book.md", PandocFrom: "markdown", PandocTo: "epub", Output: "s.epub"},
		StageTypeMix:     {ID: "s", Type: StageTypeMix, ScriptFile: "script.json", Output: "s.m4b"},
		StageTypeShort:   {ID: "s", Type: StageTypeShort, ScriptFile: "script.json", Output: "s.mp4"},
	}
}

// TestEveryStageTypeHasAnExecutor pins allStageTypes to the types vamp can
// actually run. Without it the list is a comment: a type could be added to
// the registry and left out of the list, and every guard that walks the
// list would silently stop covering it while still reporting green.
func TestEveryStageTypeHasAnExecutor(t *testing.T) {
	reg := (&Executor{}).newStageRegistry()
	listed := map[StageType]bool{}
	for _, ty := range allStageTypes {
		if listed[ty] {
			t.Fatalf("allStageTypes lists %q twice", ty)
		}
		listed[ty] = true
		if _, ok := reg[ty]; !ok {
			t.Errorf("allStageTypes lists %q, which has no executor: a stage of that type fails at run time with "+
				"\"no executor registered\", and every guard that walks this list covers a type that cannot run", ty)
		}
	}
	for ty := range reg {
		if !listed[ty] {
			t.Errorf("the executor registry can run %q, which allStageTypes does not list. Every per-type rule "+
				"derived from that list — the schema's `type` enum, the cacheable/keyable agreement below — "+
				"now silently excludes this type. Add it to allStageTypes.", ty)
		}
	}
}

// TestCacheableAndKeyableAgree is the guard that makes the pandoc defect
// unrepresentable rather than merely fixed: walk every stage type and
// assert the advertisement and the performance answer the same question.
//
// A thirteenth type added to one and forgotten in the other fails here. A
// type added to neither is caught by the fixture-coverage Fatal below (it
// has no entry) and by TestEveryStageTypeHasAnExecutor before that.
func TestCacheableAndKeyableAgree(t *testing.T) {
	dir := t.TempDir()
	fixtures := cacheContractFixtures(t, dir)
	keys := map[string]StageType{}

	for _, ty := range allStageTypes {
		st, ok := fixtures[ty]
		if !ok {
			t.Fatalf("no fixture for stage type %q. This is the guard's own coverage: a new stage type must be "+
				"given a minimal stage in cacheContractFixtures, or the agreement below is asserted over a "+
				"domain that no longer includes it.", ty)
		}
		t.Run(string(ty), func(t *testing.T) {
			p := &Pipeline{Name: "cache-contract", Stages: []Stage{*st}}
			if err := p.Validate(); err != nil {
				t.Fatalf("the fixture is not a valid stage (%v) — a shape vamp would reject proves nothing "+
					"about the shape it accepts", err)
			}
			e := &Executor{PipelineDir: dir, RunDir: dir, Pipeline: p}

			advertised := stageCacheable(st)
			key, err := e.computeStageCacheKey(st, nil, 0)
			if err != nil {
				t.Fatalf("computeStageCacheKey: %v", err)
			}
			switch {
			case advertised && key == "":
				t.Fatalf("stageCacheable says %q is cacheable and computeStageCacheKey has no branch for it, so "+
					"it returns the empty key: the runner does no Get and no Put, the timing report prints "+
					"`cache: miss` on every run forever, and the work is redone every time. This is exactly "+
					"the shape pandoc shipped in.", ty)
			case !advertised && key != "":
				t.Fatalf("computeStageCacheKey produced a key for %q, which stageCacheable says is NOT cacheable. "+
					"The runner gates on stageCacheable, so the key is unreachable today — and the day a "+
					"caller stops gating, a side effect gets replayed out of the cache.", ty)
			}
			if !advertised {
				return
			}
			// A key that is not reproducible is not a cache key: the same
			// stage over the same inputs would miss forever.
			again, err := e.computeStageCacheKey(st, nil, 0)
			if err != nil {
				t.Fatalf("second computeStageCacheKey: %v", err)
			}
			if again != key {
				t.Fatalf("%q keyed twice over identical inputs and produced %q then %q: nothing would ever hit", ty, key, again)
			}
			if other, clash := keys[key]; clash {
				t.Fatalf("%q and %q hash to the SAME key %q over unrelated inputs: one type's output would be "+
					"served as the other's", ty, other, key)
			}
			keys[key] = ty
		})
	}
}

// TestCacheableAndKeyableAgree_WebhookOptIn is the same agreement for the
// one type whose answer depends on the stage rather than only on its type.
// `cache: true` on a webhook must move BOTH halves, not just the badge.
func TestCacheableAndKeyableAgree_WebhookOptIn(t *testing.T) {
	dir := t.TempDir()
	st := *cacheContractFixtures(t, dir)[StageTypeWebhook]
	optIn := true
	st.Cache = &optIn
	e := &Executor{PipelineDir: dir, RunDir: dir}

	if !stageCacheable(&st) {
		t.Fatal("`cache: true` did not make the webhook cacheable")
	}
	key, err := e.computeStageCacheKey(&st, nil, 0)
	if err != nil {
		t.Fatalf("computeStageCacheKey: %v", err)
	}
	if key == "" {
		t.Fatal("an opt-in webhook advertises caching and computes no key: the opt-in is a no-op")
	}
}

// TestPandocCacheKeyDiscriminates: the pandoc key must move when anything
// buildPandocArgs reads moves. A key that ignored --to would serve the
// EPUB back when the stage was changed to ask for a PDF, which is worse
// than the no-caching it replaced.
func TestPandocCacheKeyDiscriminates(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "book.md")
	if err := os.WriteFile(src, []byte("# one"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := &Executor{PipelineDir: dir, RunDir: dir}
	base := &Stage{
		ID: "epub", Type: StageTypePandoc, SourceFile: "book.md",
		PandocFrom: "markdown", PandocTo: "epub", Output: "book.epub",
	}
	keyOf := func(st *Stage) string {
		t.Helper()
		k, err := e.computeStageCacheKey(st, nil, 0)
		if err != nil {
			t.Fatalf("computeStageCacheKey: %v", err)
		}
		if k == "" {
			t.Fatal("pandoc produced no cache key")
		}
		return k
	}
	baseKey := keyOf(base)

	// The source's BYTES, not its path: a regenerated markdown at the same
	// path is the commonest way a cached book goes stale.
	if err := os.WriteFile(src, []byte("# two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if keyOf(base) == baseKey {
		t.Fatal("rewriting the source markdown did not change the pandoc key: the cache would serve the previous book")
	}
	if err := os.WriteFile(src, []byte("# one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if keyOf(base) != baseKey {
		t.Fatal("restoring the source did not restore the key: the key is not content-addressed")
	}

	for _, c := range []struct {
		name   string
		mutate func(*Stage)
	}{
		{"pandoc_to", func(s *Stage) { s.PandocTo = "pdf"; s.Output = "book.pdf" }},
		{"pandoc_from", func(s *Stage) { s.PandocFrom = "gfm" }},
		{"pandoc_args", func(s *Stage) { s.PandocArgs = []string{"--toc"} }},
		{"pandoc_metadata", func(s *Stage) { s.PandocMetadata = map[string]string{"title": "Barrels"} }},
		{"output", func(s *Stage) { s.Output = "other.epub" }},
		{"binary", func(s *Stage) { s.Binary = "docker" }},
		{"source_file", func(s *Stage) { s.SourceFile = "elsewhere.md" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := *base
			c.mutate(&v)
			if keyOf(&v) == baseKey {
				t.Fatalf("changing %s did not change the pandoc cache key: the previous conversion would be "+
					"served for the new one", c.name)
			}
		})
	}
}
