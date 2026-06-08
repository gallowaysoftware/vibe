package hfdownload

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func newFakeHF(t *testing.T, body []byte) (*httptest.Server, Spec) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "/test-org/test-repo/resolve/main/model.gguf"
		if r.URL.Path != want {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("Accept-Ranges", "bytes")
		if rng := r.Header.Get("Range"); rng != "" {
			var start int64
			fmt.Sscanf(rng, "bytes=%d-", &start)
			if start >= int64(len(body)) {
				http.Error(w, "range past end", http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(body)-1, len(body)))
			w.Header().Set("Content-Length", strconv.Itoa(len(body)-int(start)))
			w.WriteHeader(http.StatusPartialContent)
			if r.Method != http.MethodHead {
				w.Write(body[start:])
			}
			return
		}
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			w.Write(body)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, Spec{Repo: "test-org/test-repo", File: "model.gguf"}
}

func TestHeadSize(t *testing.T) {
	srv, spec := newFakeHF(t, bytes.Repeat([]byte("a"), 1234))
	n, err := headSize(context.Background(), srv.Client(), spec, Endpoint(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1234 {
		t.Errorf("size = %d, want 1234", n)
	}
}

func TestDownload_FreshFile(t *testing.T) {
	body := bytes.Repeat([]byte("X"), 4096)
	srv, spec := newFakeHF(t, body)
	dest := filepath.Join(t.TempDir(), "subdir", "model.gguf") // tests MkdirAll

	var lastDownloaded, lastTotal int64
	err := download(context.Background(), srv.Client(), spec, dest, func(d, total int64) {
		lastDownloaded, lastTotal = d, total
	}, Endpoint(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("downloaded body mismatch (lengths got %d want %d)", len(got), len(body))
	}
	if lastDownloaded != int64(len(body)) || lastTotal != int64(len(body)) {
		t.Errorf("final progress = %d/%d, want %d/%d", lastDownloaded, lastTotal, len(body), len(body))
	}
}

func TestDownload_SkipsWhenLocalMatchesSize(t *testing.T) {
	body := bytes.Repeat([]byte("X"), 4096)
	called := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if r.Method == http.MethodGet {
			t.Errorf("unexpected GET; should skip download")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	spec := Spec{Repo: "test-org/test-repo", File: "model.gguf"}

	dest := filepath.Join(t.TempDir(), "model.gguf")
	if err := os.WriteFile(dest, body, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := download(context.Background(), srv.Client(), spec, dest, nil, Endpoint(srv.URL)); err != nil {
		t.Fatal(err)
	}
	if called == 0 {
		t.Errorf("HEAD should have been called")
	}
}

func TestDownload_ResumesFromPartial(t *testing.T) {
	body := bytes.Repeat([]byte("Y"), 10*1024)
	srv, spec := newFakeHF(t, body)
	dest := filepath.Join(t.TempDir(), "model.gguf")

	// Pre-populate with first half.
	half := len(body) / 2
	if err := os.WriteFile(dest, body[:half], 0o644); err != nil {
		t.Fatal(err)
	}

	if err := download(context.Background(), srv.Client(), spec, dest, nil, Endpoint(srv.URL)); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("resumed download mismatch (got len %d, want %d)", len(got), len(body))
	}
}

func TestDownload_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gated", http.StatusUnauthorized)
	}))
	defer srv.Close()
	spec := Spec{Repo: "test-org/test-repo", File: "model.gguf"}
	dest := filepath.Join(t.TempDir(), "model.gguf")
	err := download(context.Background(), srv.Client(), spec, dest, nil, Endpoint(srv.URL))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("err = %v", err)
	}
}

func TestSpec_URL(t *testing.T) {
	cases := []struct {
		spec Spec
		want string
	}{
		{Spec{Repo: "a/b", File: "c.gguf"}, "https://huggingface.co/a/b/resolve/main/c.gguf"},
		{Spec{Repo: "a/b", File: "c.gguf", Revision: "v1.0"}, "https://huggingface.co/a/b/resolve/v1.0/c.gguf"},
	}
	for _, tc := range cases {
		if got := tc.spec.URL(); got != tc.want {
			t.Errorf("URL = %q, want %q", got, tc.want)
		}
	}
}

// silence "imported and not used" if we trim later
var _ = io.Copy

// TestLargestNewFileSize covers the in-flight polling path used by the hf-CLI
// downloader: while `hf` writes to a `.incomplete` sibling we must find THAT
// file (largest NEW file in the tree), not the final filename (which doesn't
// exist yet) and crucially not a pre-existing unrelated file — the target dir is
// shared, so a model's mmproj/drafter download sits beside the much larger main
// weights, which must be ignored.
func TestLargestNewFileSize(t *testing.T) {
	dir := t.TempDir()
	if got := largestNewFileSize(dir, nil); got != 0 {
		t.Errorf("empty dir: got %d, want 0", got)
	}
	if got := largestNewFileSize(filepath.Join(dir, "missing"), nil); got != 0 {
		t.Errorf("missing dir: got %d, want 0 (walk should be tolerant)", got)
	}
	// A pre-existing large file (e.g. the main model weights) that this download
	// must NOT report as progress.
	preexisting := bytes.Repeat([]byte("m"), 1<<16)
	if err := os.WriteFile(filepath.Join(dir, "model.gguf"), preexisting, 0o644); err != nil {
		t.Fatal(err)
	}
	baseline := fileSizes(dir)

	// With only the pre-existing file, nothing new → 0.
	if got := largestNewFileSize(dir, baseline); got != 0 {
		t.Errorf("only pre-existing files: got %d, want 0", got)
	}

	// Now an in-flight nested .incomplete blob (smaller than the pre-existing
	// model) — it must be found despite not being the largest file in the tree.
	sub := filepath.Join(dir, "nested")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	blob := bytes.Repeat([]byte("a"), 4096)
	if err := os.WriteFile(filepath.Join(sub, "drafter.gguf.incomplete"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := largestNewFileSize(dir, baseline); got != int64(len(blob)) {
		t.Errorf("got %d, want %d (should find the NEW nested blob, ignoring the bigger pre-existing model)", got, len(blob))
	}
}
