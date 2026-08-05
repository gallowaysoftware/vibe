package swaptest_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The live target is gated on ENVIRONMENT, never on a build tag.
//
// This module contains zero //go:build lines, and that is the right choice
// here: `go vet` and golangci-lint skip tagged-out files by default, so a
// tagged file compiles nowhere in CI and rots until someone runs it. An
// env-gated file is built, vetted and linted on every run and merely skips.
//
// Two ways to supply a target:
//
//	LLAMA_SWAP_URL=http://127.0.0.1:9000            an instance you already run
//	LLAMA_SWAP_BIN=… LLAMA_SERVER_BIN=… LLAMA_GGUF=…  exec one for the test
//
// LLAMA_SWAP_MODEL names the model to drive (default "conformance").
//
// Pointing LLAMA_SWAP_URL at a production router is a bad idea: the suite
// drives completions and one deliberate refusal.

func liveTarget(t *testing.T) (target, bool) {
	t.Helper()
	model := os.Getenv("LLAMA_SWAP_MODEL")
	if model == "" {
		model = "conformance"
	}
	if url := os.Getenv("LLAMA_SWAP_URL"); url != "" {
		return target{name: "live/url", url: strings.TrimRight(url, "/"), model: model}, true
	}
	swapBin := os.Getenv("LLAMA_SWAP_BIN")
	serverBin := os.Getenv("LLAMA_SERVER_BIN")
	gguf := os.Getenv("LLAMA_GGUF")
	if swapBin == "" || serverBin == "" || gguf == "" {
		return target{}, false
	}
	return target{name: "live/exec", url: execLlamaSwap(t, swapBin, serverBin, gguf, model), model: model}, true
}

// execLlamaSwap starts a real llama-swap over a real llama-server and
// returns its base URL. ~20 lines of exec.CommandContext, which is the
// whole reason this module does not need a container runtime or a Docker
// API client in its module graph: llama-swap ships as a statically linked
// binary in a 13 MB tarball, and this repo already execs real binaries in
// six test files.
func execLlamaSwap(t *testing.T, swapBin, serverBin, gguf, model string) string {
	t.Helper()
	dir := t.TempDir()
	port := freePort(t)
	// ${PORT} resolves to llama-swap's fixed default (5800). Two of these
	// on one machine would collide, and the loser's llama-server exits 1
	// with "couldn't bind HTTP server socket" — which surfaces as a 500 and
	// an unexplained skip, not as a port conflict. Pin an ephemeral port and
	// declare the matching proxy so the instance is hermetic.
	upstream := freePort(t)

	cfg := fmt.Sprintf(`
healthCheckTimeout: 120
logLevel: info
store:
  path: %s
models:
  %s:
    proxy: http://127.0.0.1:%d
    cmd: >
      %s --model %s --host 127.0.0.1 --port %d
      --ctx-size 512 --n-gpu-layers 0 --no-warmup
    ttl: 300
`, filepath.Join(dir, "store.db"), model, upstream, serverBin, gguf, upstream)
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, swapBin, "-config", cfgPath, "-listen", fmt.Sprintf("127.0.0.1:%d", port))
	// SIGTERM, not the default SIGKILL. llama-swap owns the llama-server it
	// spawned; killed outright it never reaps it, and the orphan keeps
	// llama-server's port bound so the NEXT run's upstream exits 1 with
	// "couldn't bind HTTP server socket" — observed, and the sort of thing
	// that reads as a broken test rather than a leaked process.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	// -ngl 0 is NOT enough on a box with a GPU: llama.cpp still builds a
	// CUDA backend and ABORTS when VRAM is full. A hosted runner has no
	// GPU, so this only matters when someone runs the suite locally — which
	// is exactly when aborting would be most confusing. -1 rather than the
	// empty string: an empty value is easy to lose across a process
	// boundary, and this must not depend on that.
	cmd.Env = append(os.Environ(), "CUDA_VISIBLE_DEVICES=-1", "HIP_VISIBLE_DEVICES=-1")
	cmd.Stdout = &testWriter{t: t, prefix: "llama-swap"}
	cmd.Stderr = &testWriter{t: t, prefix: "llama-swap!"}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start llama-swap: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
	})

	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(60 * time.Second)
	up := false
	for time.Now().Before(deadline) {
		rctx, rcancel := context.WithTimeout(context.Background(), time.Second)
		req, _ := http.NewRequestWithContext(rctx, http.MethodGet, url+"/running", nil)
		resp, err := http.DefaultClient.Do(req)
		rcancel()
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				up = true
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !up {
		t.Fatalf("llama-swap did not answer /running on %s within 60s", url)
	}
	requireUpstreamServes(t, url, model)
	return url
}

// requireUpstreamServes proves the llama-server BEHIND llama-swap actually
// runs before any invariant is asserted against this target.
//
// This exists because the first green CI run was a lie. The workflow copied
// llama.cpp's shared objects with `find -type f`, which drops the versioned
// soname symlinks, so llama-server exited immediately and llama-swap
// answered every completion 500. Four of the five invariants passed anyway —
// they exercise routing, in-flight tracking and the activity log, none of
// which need an upstream — and the fifth SKIPPED, which reads exactly like
// coverage. A conformance job whose model never loaded must be RED.
func requireUpstreamServes(t *testing.T, url, model string) {
	t.Helper()
	body := fmt.Sprintf(`{"model":%q,"max_tokens":4,"messages":[{"role":"user","content":"hi"}]}`, model)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("the live target did not answer a completion: %v", err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the live target answered HTTP %d to a warm-up completion for %q: %s\n"+
			"llama-swap is running but its upstream is not. Check LLAMA_SERVER_BIN's shared "+
			"libraries (the ggml sonames need their symlinks) and that the port is free.",
			resp.StatusCode, model, strings.TrimSpace(string(payload)))
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

type testWriter struct {
	t      *testing.T
	prefix string
}

func (w *testWriter) Write(p []byte) (int, error) {
	w.t.Logf("[%s] %s", w.prefix, strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// TestRecordingIsCurrent fails when the live llama-swap is a version the
// checked-in recordings do not cover. It fires on the box where the upgrade
// happened, in front of the operator who can act — which the weekly drift
// job in CI cannot do.
func TestRecordingIsCurrent(t *testing.T) {
	bin := os.Getenv("LLAMA_SWAP_BIN")
	if bin == "" {
		if _, err := exec.LookPath("llama-swap"); err != nil {
			t.Skip("no llama-swap binary; set LLAMA_SWAP_BIN to check the recordings against it")
		}
		bin = "llama-swap"
	}
	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		t.Skipf("%s --version: %v", bin, err)
	}
	live := parseSwapVersion(string(out))
	if live == "" {
		t.Skipf("could not parse a version out of %q", string(out))
	}
	for _, w := range wiresWithRecordings(t) {
		if w == live {
			return
		}
	}
	t.Fatalf("llama-swap on this box is %s and internal/swaptest has recordings for %v. "+
		"Re-record: SWAPTEST_RECORD_URL=… SWAPTEST_RECORD_MODEL=<an already-resident model> "+
		"go test ./internal/swaptest -run TestRecord — and commit the new directory BESIDE the old ones.",
		live, wiresWithRecordings(t))
}
