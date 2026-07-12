// Package hfdownload downloads model files from HuggingFace using the public
// resolve URL pattern. Supports the $HF_TOKEN env var for gated models and
// resume-after-interruption via HTTP Range. Phase 1 is single-file only.
package hfdownload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Spec identifies a file on HuggingFace.
type Spec struct {
	Repo     string // e.g. "Qwen/Qwen3.6-27B-GGUF"
	File     string // e.g. "Qwen_Qwen3.6-27B-Q6_K.gguf"
	Revision string // optional, default "main"
}

// ProgressFunc is called with (downloaded, total) bytes. total is -1 if HF
// didn't return a Content-Length. Callbacks are best-effort; the downloader
// throttles to roughly once every 250ms.
type ProgressFunc func(downloaded, total int64)

// Endpoint allows tests to point at a fake HF. Defaults to https://huggingface.co.
type Endpoint string

func (e Endpoint) String() string {
	if e == "" {
		return "https://huggingface.co"
	}
	return string(e)
}

// HeadSize returns the file size as advertised by HuggingFace, in bytes.
// Returns -1 if the server didn't include Content-Length.
func HeadSize(ctx context.Context, spec Spec) (int64, error) {
	return headSize(ctx, http.DefaultClient, spec, Endpoint(""))
}

func headSize(ctx context.Context, hc *http.Client, spec Spec, ep Endpoint) (int64, error) {
	u := ep.String() + spec.path()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
	if err != nil {
		return 0, err
	}
	addAuth(req, ep)
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return 0, fmt.Errorf("HEAD %s: %s", u, resp.Status)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		n, err := strconv.ParseInt(cl, 10, 64)
		if err != nil {
			return -1, nil
		}
		return n, nil
	}
	return -1, nil
}

// Download fetches spec to destPath. If `hf` (huggingface_hub CLI) is on
// $PATH, vibe shells out to it so gated repos work via the user's stored
// `hf auth login` credentials. Otherwise we fall back to a direct HF resolve
// URL, which works for public repos and reads $HF_TOKEN for gated ones.
//
// In both modes: if destPath already exists at the size HuggingFace reports
// for the file, this returns immediately. Parent directories are created.
// Progress is reported periodically; the final call is guaranteed.
func Download(ctx context.Context, spec Spec, destPath string, progress ProgressFunc) error {
	// First: if the file exists locally, try to verify against HF metadata.
	// If HEAD succeeds and sizes match, we're done. If HEAD fails (e.g.
	// gated repo without auth), trust the local file rather than pretending
	// we need to redownload something we can't even check. Trusting is
	// sound because destPath only ever holds completed downloads: the
	// direct path stages to a .partial sibling and renames on completion,
	// and the hf CLI writes .incomplete files with the same rename-at-end
	// invariant. Cached paths deliberately do NOT call progress — the
	// caller distinguishes "cached" vs "downloaded" by whether bytes flowed.
	if info, err := os.Stat(destPath); err == nil && info.Size() > 0 {
		total, herr := HeadSize(ctx, spec)
		switch {
		case herr == nil && total > 0 && info.Size() == total:
			return nil
		case herr != nil:
			return nil
		}
		// File exists with wrong size; fall through to redownload.
	}

	if hfBin, ok := LookupHFCLI(); ok {
		return downloadViaHFCLI(ctx, hfBin, spec, destPath, progress)
	}
	return download(ctx, http.DefaultClient, spec, destPath, progress, Endpoint(""))
}

func download(ctx context.Context, hc *http.Client, spec Spec, destPath string, progress ProgressFunc, ep Endpoint) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("mkdir for %s: %w", destPath, err)
	}

	total, err := headSize(ctx, hc, spec, ep)
	if err != nil {
		return fmt.Errorf("head: %w", err)
	}

	if info, err := os.Stat(destPath); err == nil {
		if total > 0 && info.Size() == total {
			report(progress, total, total)
			return nil
		}
		// Wrong size at destPath: redownload into the staging file and let
		// the final rename replace it.
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	// Stage into a .partial sibling and rename onto destPath only after the
	// size check passes, so an interrupted pull never leaves a truncated
	// file where Download's HEAD-failed branch would trust it as complete.
	partialPath := destPath + ".partial"
	var resumeFrom int64
	if info, err := os.Stat(partialPath); err == nil {
		switch {
		case total > 0 && info.Size() == total:
			if err := os.Rename(partialPath, destPath); err != nil {
				return fmt.Errorf("promote partial: %w", err)
			}
			report(progress, total, total)
			return nil
		case total > 0 && info.Size() > total:
			// Local is somehow bigger than remote — assume corruption, start over.
			if err := os.Remove(partialPath); err != nil {
				return fmt.Errorf("remove oversized partial: %w", err)
			}
		case total > 0 && info.Size() < total:
			resumeFrom = info.Size()
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	u := ep.String() + spec.path()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	addAuth(req, ep)
	if resumeFrom > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", resumeFrom))
	}

	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("GET %s: %s: %s", u, resp.Status, string(body))
	}
	// If the server didn't honor Range (returned 200 instead of 206), restart.
	if resumeFrom > 0 && resp.StatusCode == http.StatusOK {
		resumeFrom = 0
		_ = os.Remove(partialPath)
	}

	flag := os.O_CREATE | os.O_WRONLY
	if resumeFrom > 0 {
		flag |= os.O_APPEND
	} else {
		flag |= os.O_TRUNC
	}
	f, err := os.OpenFile(partialPath, flag, 0o644)
	if err != nil {
		return err
	}

	downloaded := resumeFrom
	report(progress, downloaded, total)

	r := &reportingReader{r: resp.Body, downloaded: &downloaded, total: total, progress: progress, lastReport: time.Now()}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	report(progress, downloaded, total) // ensure a final callback at completion
	if total > 0 && downloaded != total {
		// Leave the partial in place so the next attempt resumes from here.
		return fmt.Errorf("downloaded %d, expected %d", downloaded, total)
	}
	if err := os.Rename(partialPath, destPath); err != nil {
		return fmt.Errorf("promote partial: %w", err)
	}
	return nil
}

func (s Spec) path() string {
	rev := s.Revision
	if rev == "" {
		rev = "main"
	}
	return fmt.Sprintf("/%s/resolve/%s/%s", s.Repo, rev, s.File)
}

// addAuth attaches the gated-model bearer token only when the request host
// matches the configured (trusted) endpoint host. Gating on host means a
// future endpoint override or an HF open-redirect can't forward the token
// cross-host.
func addAuth(req *http.Request, ep Endpoint) {
	tok := os.Getenv("HF_TOKEN")
	if tok == "" {
		return
	}
	trusted, err := url.Parse(ep.String())
	if err != nil || req.URL.Host != trusted.Host {
		return
	}
	req.Header.Set("Authorization", "Bearer "+tok)
}

func report(f ProgressFunc, downloaded, total int64) {
	if f != nil {
		f(downloaded, total)
	}
}

type reportingReader struct {
	r          io.Reader
	downloaded *int64
	total      int64
	progress   ProgressFunc
	lastReport time.Time
}

func (rr *reportingReader) Read(p []byte) (int, error) {
	n, err := rr.r.Read(p)
	if n > 0 {
		*rr.downloaded += int64(n)
		if rr.progress != nil && time.Since(rr.lastReport) >= 250*time.Millisecond {
			rr.progress(*rr.downloaded, rr.total)
			rr.lastReport = time.Now()
		}
	}
	return n, err
}
