package cache

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCanonicalJSON_KeyOrderDeterministic(t *testing.T) {
	// Two maps with the same contents in different insertion orders MUST
	// canonicalise to identical bytes — otherwise cache keys derived from
	// "sorted params JSON" would be sensitive to YAML decode ordering, which
	// is not stable across Go versions.
	a := map[string]any{"b": 2, "a": 1, "c": map[string]any{"y": "y", "x": "x"}}
	b := map[string]any{"c": map[string]any{"x": "x", "y": "y"}, "a": 1, "b": 2}
	ab, err := CanonicalJSON(a)
	if err != nil {
		t.Fatal(err)
	}
	bb, err := CanonicalJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ab, bb) {
		t.Fatalf("canonical JSON mismatch:\n a=%s\n b=%s", ab, bb)
	}
	// Spot-check the sort: keys should appear in lexicographic order.
	want := `{"a":1,"b":2,"c":{"x":"x","y":"y"}}`
	if string(ab) != want {
		t.Fatalf("canonical JSON = %s, want %s", ab, want)
	}
}

func TestHashStrings_StableAcrossRuns(t *testing.T) {
	h1 := HashStrings("text", "cap-a", "hello", "model-x")
	h2 := HashStrings("text", "cap-a", "hello", "model-x")
	if h1 != h2 {
		t.Fatalf("HashStrings not deterministic: %s != %s", h1, h2)
	}
	if len(h1) != 64 {
		t.Fatalf("expected 64-char sha256 hex, got %d", len(h1))
	}
	// Different stage type -> different hash, even with the same parts.
	h3 := HashStrings("audio", "cap-a", "hello", "model-x")
	if h3 == h1 {
		t.Fatalf("stage type collision: %s == %s", h1, h3)
	}
}

func TestStore_PutGet_TextRoundTrip(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hash := HashStrings("text", "x", "hello", "model")
	// Cache miss before Put.
	got, err := s.Get(hash)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("expected miss before put, got entry %+v", got)
	}
	if err := s.Put(PutInput{
		Hash:         hash,
		StageType:    "text",
		Text:         "hello world",
		SourceRunDir: "/tmp/run-x",
	}); err != nil {
		t.Fatal(err)
	}
	got, err = s.Get(hash)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatalf("expected hit after put")
	}
	if got.IsBinary {
		t.Fatalf("text entry returned with IsBinary=true")
	}
	if string(got.Output) != "hello world" {
		t.Fatalf("output = %q", got.Output)
	}
	if got.Meta.StageType != "text" {
		t.Fatalf("meta.stage_type = %q", got.Meta.StageType)
	}
	if got.Meta.SourceRunDir != "/tmp/run-x" {
		t.Fatalf("meta.source_run_dir = %q", got.Meta.SourceRunDir)
	}
	if got.Meta.Size != int64(len("hello world")) {
		t.Fatalf("meta.size = %d, want %d", got.Meta.Size, len("hello world"))
	}
}

func TestStore_PutGet_BinaryRoundTrip(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hash := HashStrings("comfyui", "imagery", "workflow-x")
	payload := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A} // PNG signature
	if err := s.Put(PutInput{
		Hash:      hash,
		StageType: "comfyui",
		Bytes:     payload,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(hash)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected hit")
	}
	if !got.IsBinary {
		t.Fatal("binary entry returned with IsBinary=false")
	}
	if !bytes.Equal(got.Output, payload) {
		t.Fatalf("payload mismatch")
	}
}

func TestStore_AtomicRename_NoTmpLeftover(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	hash := HashStrings("text", "x", "y", "z")
	if err := s.Put(PutInput{Hash: hash, StageType: "text", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	// No .tmp sibling should remain after a successful Put — the rename
	// either moved or removed it. Walk the entry dir and assert the file
	// list matches exactly {output.txt, meta.json}.
	dir := filepath.Join(root, "sha256", hash[:2], hash)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"output.txt": true, "meta.json": true}
	for _, e := range entries {
		if !want[e.Name()] {
			t.Errorf("unexpected entry %q in cache dir", e.Name())
		}
		delete(want, e.Name())
	}
	if len(want) != 0 {
		t.Errorf("missing entries: %v", want)
	}
}

func TestStore_PutGet_CorruptedMetaTreatedAsMiss(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	hash := HashStrings("text", "x", "y", "z")
	if err := s.Put(PutInput{Hash: hash, StageType: "text", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "sha256", hash[:2], hash)
	// Truncate meta.json to an invalid JSON snippet.
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(hash)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatal("expected miss on corrupted meta.json")
	}
}

func TestStore_Stat_AndSize(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		hash := HashStrings("text", "x", "y", "z", string(rune('0'+i)))
		if err := s.Put(PutInput{Hash: hash, StageType: "text", Text: "abc"}); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := s.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(stats))
	}
	total, count, err := s.Size()
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("count = %d", count)
	}
	if total != 9 {
		t.Fatalf("total = %d (each text is 3 bytes, 3 entries)", total)
	}
}

func TestStore_Prune_ByAge(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	young := HashStrings("text", "young", "1", "2")
	old := HashStrings("text", "old", "1", "2")
	if err := s.Put(PutInput{Hash: young, StageType: "text", Text: "y"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(PutInput{Hash: old, StageType: "text", Text: "o"}); err != nil {
		t.Fatal(err)
	}
	// Backdate the `old` entry by editing its meta.json's created_at AND its
	// output mtime. Prune consults the on-disk mtime via Stat (which prefers
	// the touched mtime), so we have to push both into the past.
	oldDir := filepath.Join(root, "sha256", old[:2], old)
	past := time.Now().Add(-48 * time.Hour)
	mb, err := json.MarshalIndent(Meta{
		StageType: "text", CreatedAt: past, Size: 1,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldDir, "meta.json"), mb, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(oldDir, "output.txt"), past, past); err != nil {
		t.Fatal(err)
	}
	removed, bytesGone, err := s.Prune(24 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if bytesGone != 1 {
		t.Fatalf("bytesGone = %d, want 1", bytesGone)
	}
	// young should still be there.
	if got, _ := s.Get(young); got == nil {
		t.Fatal("young entry pruned by mistake")
	}
	if got, _ := s.Get(old); got != nil {
		t.Fatal("old entry survived prune")
	}
}

func TestStore_Clean(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		h := HashStrings("text", "x", "y", "z", string(rune('0'+i)))
		if err := s.Put(PutInput{Hash: h, StageType: "text", Text: "abc"}); err != nil {
			t.Fatal(err)
		}
	}
	count, bytesGone, err := s.Clean()
	if err != nil {
		t.Fatal(err)
	}
	if count != 4 {
		t.Fatalf("count = %d", count)
	}
	if bytesGone != 12 {
		t.Fatalf("bytesGone = %d", bytesGone)
	}
	stats, err := s.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 0 {
		t.Fatalf("stats not empty after clean: %d", len(stats))
	}
}

func TestStore_Put_RejectsAmbiguousInput(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Both text and bytes set: error.
	if err := s.Put(PutInput{Hash: "abc", Text: "x", Bytes: []byte{1}}); err == nil {
		t.Fatal("expected error when both Text and Bytes set")
	}
	// Neither set: also error.
	if err := s.Put(PutInput{Hash: "abc"}); err == nil {
		t.Fatal("expected error when neither Text nor Bytes set")
	}
}

func TestHashFile_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "in")
	if err := os.WriteFile(path, []byte("the quick brown fox"), 0o644); err != nil {
		t.Fatal(err)
	}
	h1, err := HashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := HashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("hash unstable: %s != %s", h1, h2)
	}
}
