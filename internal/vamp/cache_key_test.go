package vamp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestComputeStageCacheKey_WebhookBodyRenderedPerItem locks down that an
// opt-in cacheable webhook's inline body is template-rendered before hashing:
// two foreach items with different bindings must produce different keys, or
// item 0's cached response would be silently served for every later item.
func TestComputeStageCacheKey_WebhookBodyRenderedPerItem(t *testing.T) {
	optIn := true
	st := &Stage{
		ID:     "embed",
		Type:   StageTypeWebhook,
		URL:    "http://127.0.0.1:8080/v1/embeddings",
		Method: "POST",
		Cache:  &optIn,
		Body: map[string]any{
			"model": "bge-m3",
			"input": "{{ .item }}",
			"nested": map[string]any{
				"queries": []any{"{{ .item }}", 7},
			},
		},
		Foreach: &ForeachSpec{From: "queries", Var: "item"},
	}
	e := &Executor{RunDir: t.TempDir()}

	key0, err := e.computeStageCacheKey(st, "what is a barrel entry proof", 0)
	if err != nil {
		t.Fatalf("key for item 0: %v", err)
	}
	key0Again, err := e.computeStageCacheKey(st, "what is a barrel entry proof", 0)
	if err != nil {
		t.Fatalf("repeat key for item 0: %v", err)
	}
	key1, err := e.computeStageCacheKey(st, "how does loudnorm work", 1)
	if err != nil {
		t.Fatalf("key for item 1: %v", err)
	}
	if key0 == "" || key1 == "" {
		t.Fatal("expected non-empty keys for opt-in cacheable webhook")
	}
	if key0 != key0Again {
		t.Fatalf("same item produced different keys: %q vs %q", key0, key0Again)
	}
	if key0 == key1 {
		t.Fatalf("different rendered bodies produced the same key %q", key0)
	}
}

// TestHashStageImages_MissingImageFile locks down that a missing image_files
// entry folds in as a marker rather than collapsing the whole stage to the
// no-images key — otherwise a typo'd path would silently serve a stale
// text-only cache entry instead of surfacing the executor's error.
func TestHashStageImages_MissingImageFile(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "a.png")
	if err := os.WriteFile(present, []byte("png bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "b.png")

	e := &Executor{PipelineDir: dir, RunDir: dir}
	st := &Stage{ID: "vision", ImageFiles: []string{present, missing}}

	oneMissing, err := e.hashStageImages(st, nil)
	if err != nil {
		t.Fatalf("hashStageImages with missing file: %v", err)
	}
	if len(oneMissing) != 2 {
		t.Fatalf("expected 2 hash entries (1 real + 1 marker), got %v", oneMissing)
	}
	foundMarker := false
	for _, h := range oneMissing {
		if strings.HasPrefix(h, "missing:") {
			foundMarker = true
		}
	}
	if !foundMarker {
		t.Fatalf("expected a missing: marker in %v", oneMissing)
	}

	if err := os.WriteFile(missing, []byte("more png bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	bothPresent, err := e.hashStageImages(st, nil)
	if err != nil {
		t.Fatalf("hashStageImages with both present: %v", err)
	}

	keyFor := func(hashes []string) string {
		k, err := stageCacheKey(keyInput{StageType: StageTypeText, RenderedPrompt: "p", ImageHashes: hashes})
		if err != nil {
			t.Fatalf("stageCacheKey: %v", err)
		}
		return k
	}
	noImages := keyFor(nil)
	missingKey := keyFor(oneMissing)
	presentKey := keyFor(bothPresent)
	if missingKey == noImages {
		t.Fatal("missing image file collapsed to the no-images key")
	}
	if missingKey == presentKey {
		t.Fatal("missing image file produced the same key as both-present")
	}
}

// TestComputeStageCacheKey_ShortStretchAndBinary locks down that toggling
// short_stretch_video (freeze-frame vs time-stretch filtergraph) or a
// non-default binary changes the short key, while the default config keeps a
// key with no gated extras appended.
func TestComputeStageCacheKey_ShortStretchAndBinary(t *testing.T) {
	e := &Executor{RunDir: t.TempDir()}
	base := &Stage{ID: "short", Type: StageTypeShort, ScriptFile: "script.json", Output: "out.mp4"}

	keyOf := func(st *Stage) string {
		k, err := e.computeStageCacheKey(st, nil, 0)
		if err != nil {
			t.Fatalf("computeStageCacheKey: %v", err)
		}
		if k == "" {
			t.Fatal("expected non-empty short key")
		}
		return k
	}

	defaultKey := keyOf(base)

	stretched := *base
	stretched.ShortStretchVideo = true
	if keyOf(&stretched) == defaultKey {
		t.Fatal("short_stretch_video: true did not change the cache key")
	}

	customBinary := *base
	customBinary.Binary = "/opt/ffmpeg-git/ffmpeg"
	if keyOf(&customBinary) == defaultKey {
		t.Fatal("non-default short binary did not change the cache key")
	}

	mix := &Stage{ID: "mix", Type: StageTypeMix, ScriptFile: "script.json", Output: "out.m4b"}
	mixDefault := keyOf(mix)
	mixCustom := *mix
	mixCustom.Binary = "/opt/ffmpeg-git/ffmpeg"
	if keyOf(&mixCustom) == mixDefault {
		t.Fatal("non-default mix binary did not change the cache key")
	}
}

// TestStageCacheKey_KokoroCapability locks down that a capability-routed
// kokoro stage keys differently from a capability-less one hitting the same
// default URL: the capability resolves to a live endpoint the key can't see,
// so the capability name itself must discriminate.
func TestStageCacheKey_KokoroCapability(t *testing.T) {
	base := keyInput{
		StageType:    StageTypeAudio,
		AudioEngine:  "kokoro",
		AudioURL:     defaultKokoroURL,
		Voice:        "af_heart",
		RenderedText: "hello",
	}
	plain, err := stageCacheKey(base)
	if err != nil {
		t.Fatalf("stageCacheKey: %v", err)
	}
	routed := base
	routed.Capability = "tts"
	withCap, err := stageCacheKey(routed)
	if err != nil {
		t.Fatalf("stageCacheKey with capability: %v", err)
	}
	if plain == withCap {
		t.Fatal("capability-routed kokoro stage shares its key with the capability-less default")
	}
}
