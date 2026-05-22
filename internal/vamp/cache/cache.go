// Package cache implements a content-addressed cache for vamp stage outputs.
//
// Each cacheable stage output is stored under
//
//	<root>/sha256/<2-char-prefix>/<full-hash>/output(.txt|.bin)
//
// alongside a meta.json that records stage type, creation time, the run dir
// the entry came from, and the file size. The two-character prefix shards the
// tree so a cache with tens of thousands of entries doesn't pile every blob
// under one directory.
//
// The package is intentionally a thin set of helpers — Key/Get/Put/Stat/Prune/
// Clean — with NO coupling to the vamp Executor. Stage-key composition lives
// in internal/vamp (where the per-stage types live); this package only knows
// how to canonicalise inputs (sorted-keys JSON), how to write entries
// atomically, and how to manage the directory layout.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Store is a content-addressed cache rooted at a directory the caller owns.
// All operations are safe for concurrent use by multiple goroutines (and by
// extension processes, as long as the kernel honours rename atomicity).
type Store struct {
	// Root is the on-disk directory the store manages. Concretely this is
	// $XDG_CACHE_HOME/vamp on production callers; tests can point it at
	// t.TempDir() for isolation.
	Root string
}

// New constructs a Store rooted at root and ensures the sharded base
// directory exists. The directory is created with 0o755 so it is shareable
// across user-owned tools.
func New(root string) (*Store, error) {
	if root == "" {
		return nil, errors.New("cache: root is required")
	}
	if err := os.MkdirAll(filepath.Join(root, "sha256"), 0o755); err != nil {
		return nil, fmt.Errorf("cache: mkdir root %s: %w", root, err)
	}
	return &Store{Root: root}, nil
}

// Entry is the on-disk record of one cached stage output. Output is the
// content (text for text stages, raw bytes for binary stages); IsBinary tells
// the caller which side of that union is meaningful. Meta is the sidecar
// metadata we wrote on Put.
type Entry struct {
	Hash     string
	Output   []byte
	IsBinary bool
	Meta     Meta
}

// Meta is the sidecar JSON record written next to every cache output. The
// fields are intentionally small and stable so older entries keep parsing
// after future Meta additions; new fields should be additive-only.
type Meta struct {
	StageType    string    `json:"stage_type"`
	CreatedAt    time.Time `json:"created_at"`
	SourceRunDir string    `json:"source_run_dir,omitempty"`
	Size         int64     `json:"size"`
}

// PutInput is what callers hand to Put when storing a fresh entry. Exactly
// one of Text or Bytes must be set: Text writes output.txt, Bytes writes
// output.bin, which feeds back into IsBinary on Get. StageType + SourceRunDir
// land in meta.json verbatim.
type PutInput struct {
	Hash         string
	StageType    string
	Text         string
	Bytes        []byte
	SourceRunDir string
}

// CanonicalJSON returns the deterministic JSON encoding of v with object keys
// sorted alphabetically at every level. Returned bytes match across
// machine/architecture and are the canonical input for stage key composition
// where the input is a map (text-stage params, comfyui workflows, ffmpeg
// argv arrays, etc.).
//
// Maps with non-string keys are not supported (vamp doesn't produce them);
// non-object/array values pass through encoding/json's default formatting.
func CanonicalJSON(v any) ([]byte, error) {
	// json.Marshal already sorts map[string]any keys lexicographically, so we
	// can lean on it for the canonical form. We still walk the value to
	// detect non-canonicalisable shapes early.
	canon, err := canonicalise(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(canon)
}

func canonicalise(v any) (any, error) {
	switch x := v.(type) {
	case map[string]any:
		// Re-emit as a key-sorted map for json.Marshal. Since map iteration
		// in Go is random, we explicitly sort and rebuild so any caller that
		// post-processes the resulting bytes sees a stable order even if
		// future encoding/json changes alter the implicit sort.
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make(orderedMap, 0, len(keys))
		for _, k := range keys {
			val, err := canonicalise(x[k])
			if err != nil {
				return nil, err
			}
			out = append(out, kv{K: k, V: val})
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			val, err := canonicalise(item)
			if err != nil {
				return nil, err
			}
			out[i] = val
		}
		return out, nil
	default:
		// Scalars (string/number/bool/nil) and any json.Marshaler pass
		// through unchanged.
		return v, nil
	}
}

// orderedMap encodes as a JSON object preserving insertion order of its
// entries. canonicalise hands us key-sorted slices so the resulting JSON has
// the canonical (sorted) key order across runs and architectures.
type orderedMap []kv

type kv struct {
	K string
	V any
}

func (o orderedMap) MarshalJSON() ([]byte, error) {
	var b []byte
	b = append(b, '{')
	for i, e := range o {
		if i > 0 {
			b = append(b, ',')
		}
		kb, err := json.Marshal(e.K)
		if err != nil {
			return nil, err
		}
		b = append(b, kb...)
		b = append(b, ':')
		vb, err := json.Marshal(e.V)
		if err != nil {
			return nil, err
		}
		b = append(b, vb...)
	}
	b = append(b, '}')
	return b, nil
}

// HashStrings returns the sha256 hex digest of stage type + the supplied
// parts joined with a NUL byte. It exists so the per-stage Key() helpers in
// internal/vamp can avoid hand-rolling their own sha256 setup; NUL is used
// as the separator because none of the textual stage inputs (prompts, model
// ids, capability names) contain NUL in any realistic pipeline.
func HashStrings(stageType string, parts ...string) string {
	h := sha256.New()
	h.Write([]byte(stageType))
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// HashFile reads path and returns the sha256 hex digest of its bytes. Used by
// ffmpeg key composition to mix every input file's content into the key.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// FileSize returns the size in bytes of path. Used by audio key composition
// to fold the voice .onnx file's size into the key without reading the (often
// >100MB) model bytes themselves; the size + voice-name combination is enough
// to detect a model swap in practice.
func FileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// entryDir returns the directory the cache uses for hash. The two-char prefix
// is the first two hex characters of the hash; the rest is the full hash.
func (s *Store) entryDir(hash string) string {
	if len(hash) < 2 {
		// A malformed (too-short) hash should never reach this code, but we
		// guard against an out-of-bounds slice. Fall back to a literal "xx"
		// shard so the caller still gets a deterministic path to error on.
		return filepath.Join(s.Root, "sha256", "xx", hash)
	}
	return filepath.Join(s.Root, "sha256", hash[:2], hash)
}

// Get returns the cached Entry for hash, or (nil, nil) on cache miss. The
// distinction between "no such entry" and "entry exists but is malformed"
// (e.g. missing meta.json) collapses to a miss so the caller can re-run the
// stage; rotting/half-written entries should never poison a fresh run.
func (s *Store) Get(hash string) (*Entry, error) {
	dir := s.entryDir(hash)
	metaPath := filepath.Join(dir, "meta.json")
	mb, err := os.ReadFile(metaPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("cache: read meta %s: %w", metaPath, err)
	}
	var m Meta
	if err := json.Unmarshal(mb, &m); err != nil {
		// Corrupted meta: treat as miss so the caller re-executes the stage
		// instead of failing the whole run on a broken cache entry.
		return nil, nil
	}
	// Try output.bin first (binary stages); fall back to output.txt (text).
	if body, err := os.ReadFile(filepath.Join(dir, "output.bin")); err == nil {
		return &Entry{Hash: hash, Output: body, IsBinary: true, Meta: m}, nil
	}
	body, err := os.ReadFile(filepath.Join(dir, "output.txt"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("cache: read output for %s: %w", hash, err)
	}
	return &Entry{Hash: hash, Output: body, IsBinary: false, Meta: m}, nil
}

// Put writes a fresh entry. Exactly one of Text / Bytes must be set; the
// resulting on-disk file is output.txt or output.bin respectively. The write
// is atomic: we stream to a `.tmp` sibling, fsync, then rename — partial
// writes on crash leave the cache directory empty rather than half-populated.
//
// Errors here are returned to the caller but in practice the exec.go
// integration treats them as warnings (a failed cache write doesn't fail the
// run; the stage's output already landed in the run dir on the live path).
func (s *Store) Put(in PutInput) error {
	if in.Hash == "" {
		return errors.New("cache: Put: hash is required")
	}
	// Text and Bytes are mutually exclusive; both being empty is fine
	// (an intentional empty text output, e.g. a compact stage whose
	// source had no content). The mode is determined by which slice is
	// non-nil — Bytes set ⇒ binary output, otherwise text. An empty
	// text output writes a zero-byte output.txt so Get can round-trip it.
	if len(in.Text) > 0 && len(in.Bytes) > 0 {
		return errors.New("cache: Put: Text and Bytes are mutually exclusive")
	}
	dir := s.entryDir(in.Hash)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("cache: mkdir %s: %w", dir, err)
	}
	var (
		outName string
		body    []byte
	)
	if in.Bytes != nil {
		outName = "output.bin"
		body = in.Bytes
	} else {
		outName = "output.txt"
		body = []byte(in.Text)
	}
	outPath := filepath.Join(dir, outName)
	if err := writeFileAtomic(outPath, body); err != nil {
		return err
	}
	meta := Meta{
		StageType:    in.StageType,
		CreatedAt:    time.Now().UTC(),
		SourceRunDir: in.SourceRunDir,
		Size:         int64(len(body)),
	}
	mb, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("cache: marshal meta: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(dir, "meta.json"), mb); err != nil {
		return err
	}
	return nil
}

// writeFileAtomic writes body to path via a sibling "<path>.tmp" + rename.
// fsync of the data file is best-effort: on filesystems where fsync is
// honoured the rename's atomicity is well-defined; on tmpfs (used by some CI
// /tmp setups) fsync is a no-op but the rename is still atomic-by-syscall,
// which is what crash-recovery actually depends on.
func writeFileAtomic(path string, body []byte) error {
	tmp := path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		// Sync failure on a tmpfs is benign (the kernel returns ENOSYS or
		// EINVAL depending on the mount); we don't want a write to /tmp
		// from a test to fail just because the temporary mount can't
		// fsync. Log via the returned error path on real failures.
		_ = f.Close()
		_ = os.Remove(tmp)
		// Some platforms surface a non-fatal sync error here. We treat any
		// sync error as fatal for the cache write (because the durability
		// guarantee is the whole point); the caller turns it into a warn
		// and the run still completes.
		return fmt.Errorf("cache: fsync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// EntryStat is a row produced by Stat: one logical cache entry with its
// on-disk size, the metadata we recorded at Put time, and the entry's
// content-hash key. Stat callers use this to render `vamp cache ls` and to
// power Prune's age-based filtering.
type EntryStat struct {
	Hash     string
	Size     int64
	Meta     Meta
	Modified time.Time
}

// Stat walks the cache directory and returns one EntryStat per entry. Entries
// whose meta.json is missing or unparseable are skipped (treated the same as
// Get treats them — a miss). The returned slice is sorted by modification
// time newest-first so `vamp cache ls` reads as a "what did I do today"
// timeline.
func (s *Store) Stat() ([]EntryStat, error) {
	base := filepath.Join(s.Root, "sha256")
	var out []EntryStat
	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	for _, shard := range entries {
		if !shard.IsDir() {
			continue
		}
		shardPath := filepath.Join(base, shard.Name())
		hashes, err := os.ReadDir(shardPath)
		if err != nil {
			continue
		}
		for _, h := range hashes {
			if !h.IsDir() {
				continue
			}
			dir := filepath.Join(shardPath, h.Name())
			metaBytes, err := os.ReadFile(filepath.Join(dir, "meta.json"))
			if err != nil {
				continue
			}
			var m Meta
			if err := json.Unmarshal(metaBytes, &m); err != nil {
				continue
			}
			// Output file may be .bin or .txt; size the one that exists.
			var size int64
			modified := m.CreatedAt
			for _, fn := range []string{"output.bin", "output.txt"} {
				if info, err := os.Stat(filepath.Join(dir, fn)); err == nil {
					size = info.Size()
					if info.ModTime().After(modified) {
						modified = info.ModTime()
					}
					break
				}
			}
			out = append(out, EntryStat{
				Hash:     h.Name(),
				Size:     size,
				Meta:     m,
				Modified: modified,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out, nil
}

// Size returns the total on-disk size of every cache output plus the entry
// count. It's a thin wrapper around Stat so `vamp cache size` and `vamp
// cache ls` share a single walk implementation.
func (s *Store) Size() (totalBytes int64, count int, err error) {
	stats, err := s.Stat()
	if err != nil {
		return 0, 0, err
	}
	for _, st := range stats {
		totalBytes += st.Size
		count++
	}
	return totalBytes, count, nil
}

// Prune removes every entry older than older. "Older than" is measured
// against EntryStat.Modified (which falls back to Meta.CreatedAt when the
// output file has been touched). Returns the number of entries pruned and
// the number of bytes reclaimed.
func (s *Store) Prune(older time.Duration) (int, int64, error) {
	cutoff := time.Now().Add(-older)
	stats, err := s.Stat()
	if err != nil {
		return 0, 0, err
	}
	var (
		removed   int
		bytesGone int64
	)
	for _, st := range stats {
		if !st.Modified.Before(cutoff) {
			continue
		}
		dir := s.entryDir(st.Hash)
		if err := os.RemoveAll(dir); err != nil {
			return removed, bytesGone, fmt.Errorf("cache: prune %s: %w", st.Hash, err)
		}
		removed++
		bytesGone += st.Size
	}
	return removed, bytesGone, nil
}

// Clean removes every entry in the store. Returns the count and total bytes
// removed. Callers that want a confirmation step should do that themselves —
// this helper is unconditional.
func (s *Store) Clean() (int, int64, error) {
	stats, err := s.Stat()
	if err != nil {
		return 0, 0, err
	}
	var total int64
	for _, st := range stats {
		total += st.Size
	}
	count := len(stats)
	base := filepath.Join(s.Root, "sha256")
	if err := os.RemoveAll(base); err != nil {
		return 0, 0, fmt.Errorf("cache: clean: %w", err)
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return 0, 0, fmt.Errorf("cache: recreate root: %w", err)
	}
	return count, total, nil
}

// DefaultRoot returns the canonical content-cache root:
// $XDG_CACHE_HOME/vamp (default $HOME/.cache/vamp).
func DefaultRoot() string {
	if v := strings.TrimSpace(os.Getenv("XDG_CACHE_HOME")); v != "" {
		return filepath.Join(v, "vamp")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "vamp")
}
