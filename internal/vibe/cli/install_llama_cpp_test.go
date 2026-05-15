package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ─── helpers ────────────────────────────────────────────────────────────────

// roundTripperFunc is a single-handler http.RoundTripper. Tests build one to
// stand in for the GitHub API + download server so we never hit real network.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// fakeReleaseJSON builds a minimal GitHub release JSON document with the
// supplied asset names. Asset download URLs point at a sentinel host the
// test transport intercepts.
func fakeReleaseJSON(tag string, assetNames []string) string {
	type asset struct {
		Name        string `json:"name"`
		DownloadURL string `json:"browser_download_url"`
	}
	type rel struct {
		TagName string  `json:"tag_name"`
		Assets  []asset `json:"assets"`
	}
	r := rel{TagName: tag}
	for _, n := range assetNames {
		r.Assets = append(r.Assets, asset{
			Name:        n,
			DownloadURL: "https://fake.example.com/" + n,
		})
	}
	b, _ := json.Marshal(r)
	return string(b)
}

// makeTarGzWithBinary builds an in-memory tar.gz containing a regular file
// at `internalPath` with the bytes "fake-binary\n" and mode 0o755. Used by
// the release-path test to satisfy findExecutableUnder.
func makeTarGzWithBinary(t *testing.T, internalPath string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("fake-binary\n")
	hdr := &tar.Header{
		Name:     internalPath,
		Mode:     0o755,
		Size:     int64(len(body)),
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// newLlamaTestEnv constructs a llamaInstallerEnv with a tmp HOME, a fake
// HTTP client routed through `rt`, a runner that records calls into
// `*calls`, and the supplied os-release contents + method choice. Returns
// the env plus the captured stdout buffer.
func newLlamaTestEnv(t *testing.T, opts llamaTestOpts) (*llamaInstallerEnv, *bytes.Buffer, *[]string) {
	t.Helper()
	home := t.TempDir()
	var stdout bytes.Buffer
	var calls []string
	rt := opts.transport
	if rt == nil {
		rt = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("unexpected HTTP call: %s %s", req.Method, req.URL)
		})
	}
	env := &llamaInstallerEnv{
		home: home,
		runCmd: func(name string, args ...string) *exec.Cmd {
			calls = append(calls, name+" "+strings.Join(args, " "))
			if opts.runHandler != nil {
				return opts.runHandler(name, args)
			}
			return exec.Command("/bin/true")
		},
		chooseMethod: func(string) llamaInstallMethod { return opts.method },
		httpClient:   &http.Client{Transport: rt},
		readOSRelease: func() ([]byte, error) {
			if opts.osRelease == "" {
				return nil, errors.New("not present")
			}
			return []byte(opts.osRelease), nil
		},
		stdout: &stdout,
		stderr: &stdout,
		lookPath: func(name string) (string, error) {
			if opts.lookPath != nil {
				return opts.lookPath(name)
			}
			return "/usr/bin/" + name, nil
		},
		getEnv: func(k string) string {
			if opts.env != nil {
				return opts.env[k]
			}
			return ""
		},
		yes:  opts.yes,
		cuda: opts.cuda,
	}
	return env, &stdout, &calls
}

type llamaTestOpts struct {
	osRelease  string
	method     llamaInstallMethod
	yes        bool
	cuda       bool
	transport  http.RoundTripper
	runHandler func(name string, args []string) *exec.Cmd
	lookPath   func(name string) (string, error)
	env        map[string]string
}

// ─── detectDistroID ────────────────────────────────────────────────────────

func TestDetectDistroID_Ubuntu(t *testing.T) {
	env, _, _ := newLlamaTestEnv(t, llamaTestOpts{
		osRelease: `ID=ubuntu` + "\n" + `ID_LIKE=debian` + "\n",
	})
	if got := detectDistroID(env); got != "ubuntu" {
		t.Errorf("got %q, want ubuntu", got)
	}
}

func TestDetectDistroID_Debian(t *testing.T) {
	env, _, _ := newLlamaTestEnv(t, llamaTestOpts{
		osRelease: `ID=debian` + "\n",
	})
	if got := detectDistroID(env); got != "debian" {
		t.Errorf("got %q, want debian", got)
	}
}

func TestDetectDistroID_Arch(t *testing.T) {
	env, _, _ := newLlamaTestEnv(t, llamaTestOpts{
		osRelease: `ID=arch` + "\n",
	})
	if got := detectDistroID(env); got != "arch" {
		t.Errorf("got %q, want arch", got)
	}
}

func TestDetectDistroID_CachyOSFoldsToArch(t *testing.T) {
	// CachyOS has ID=cachyos but ID_LIKE=arch. We should canonicalise to
	// `arch` so the dispatcher hits the pacman branch.
	env, _, _ := newLlamaTestEnv(t, llamaTestOpts{
		osRelease: `ID=cachyos` + "\n" + `ID_LIKE=arch` + "\n",
	})
	if got := detectDistroID(env); got != "arch" {
		t.Errorf("got %q, want arch (via ID_LIKE)", got)
	}
}

func TestDetectDistroID_Fedora(t *testing.T) {
	env, _, _ := newLlamaTestEnv(t, llamaTestOpts{
		osRelease: `ID=fedora` + "\n",
	})
	if got := detectDistroID(env); got != "fedora" {
		t.Errorf("got %q, want fedora", got)
	}
}

func TestDetectDistroID_Missing(t *testing.T) {
	env, _, _ := newLlamaTestEnv(t, llamaTestOpts{})
	if got := detectDistroID(env); got != "" {
		t.Errorf("got %q, want empty (no os-release)", got)
	}
}

func TestDetectDistroID_QuotedValues(t *testing.T) {
	env, _, _ := newLlamaTestEnv(t, llamaTestOpts{
		osRelease: `ID="fedora"` + "\n",
	})
	if got := detectDistroID(env); got != "fedora" {
		t.Errorf("got %q, want fedora", got)
	}
}

// ─── pickLlamaLinuxAsset ───────────────────────────────────────────────────

func TestPickLlamaLinuxAsset_GenericUbuntuX64(t *testing.T) {
	// The realistic set of assets shipped by the upstream project.
	rel := &llamaGitHubRelease{TagName: "b9172"}
	for _, n := range []string{
		"llama-b9172-bin-ubuntu-x64.tar.gz",
		"llama-b9172-bin-ubuntu-vulkan-x64.tar.gz",
		"llama-b9172-bin-ubuntu-rocm-7.2-x64.tar.gz",
		"llama-b9172-bin-macos-x64.tar.gz",
		"llama-b9172-bin-ubuntu-arm64.tar.gz",
		"llama-b9172-bin-win-cuda-12.4-x64.zip",
		"cudart-llama-bin-win-cuda-12.4-x64.zip",
	} {
		rel.Assets = append(rel.Assets, struct {
			Name        string `json:"name"`
			DownloadURL string `json:"browser_download_url"`
		}{Name: n, DownloadURL: "https://x/" + n})
	}
	got := pickLlamaLinuxAsset(rel, false /* cuda */)
	if got.Name != "llama-b9172-bin-ubuntu-x64.tar.gz" {
		t.Errorf("got %q, want llama-b9172-bin-ubuntu-x64.tar.gz", got.Name)
	}
}

func TestPickLlamaLinuxAsset_CUDAWithNoLinuxCUDAFallsToPlain(t *testing.T) {
	// Reality (Phase 1): no Linux CUDA asset is published. When --cuda is
	// passed we should NOT return a Windows CUDA build; we should return
	// nothing (caller surfaces the "build from source" error).
	rel := &llamaGitHubRelease{TagName: "b9172"}
	for _, n := range []string{
		"llama-b9172-bin-ubuntu-x64.tar.gz",
		"llama-b9172-bin-win-cuda-12.4-x64.zip",
	} {
		rel.Assets = append(rel.Assets, struct {
			Name        string `json:"name"`
			DownloadURL string `json:"browser_download_url"`
		}{Name: n, DownloadURL: "https://x/" + n})
	}
	got := pickLlamaLinuxAsset(rel, true /* cuda */)
	if got.Name != "" {
		t.Errorf("got %q, want empty (no Linux CUDA asset exists)", got.Name)
	}
}

func TestPickLlamaLinuxAsset_CUDAWithLinuxCUDA(t *testing.T) {
	// Forward-looking: if upstream starts shipping a Linux CUDA tarball,
	// we should pick it preferentially over the plain Ubuntu build.
	rel := &llamaGitHubRelease{TagName: "b9999"}
	for _, n := range []string{
		"llama-b9999-bin-ubuntu-x64.tar.gz",
		"llama-b9999-bin-ubuntu-cuda-12.4-x64.tar.gz",
		"llama-b9999-bin-ubuntu-vulkan-x64.tar.gz",
	} {
		rel.Assets = append(rel.Assets, struct {
			Name        string `json:"name"`
			DownloadURL string `json:"browser_download_url"`
		}{Name: n, DownloadURL: "https://x/" + n})
	}
	got := pickLlamaLinuxAsset(rel, true)
	if got.Name != "llama-b9999-bin-ubuntu-cuda-12.4-x64.tar.gz" {
		t.Errorf("got %q, want CUDA variant", got.Name)
	}
}

func TestPickLlamaLinuxAsset_NoLinuxAsset(t *testing.T) {
	rel := &llamaGitHubRelease{TagName: "b9172"}
	for _, n := range []string{"llama-b9172-bin-win-cuda-12.4-x64.zip"} {
		rel.Assets = append(rel.Assets, struct {
			Name        string `json:"name"`
			DownloadURL string `json:"browser_download_url"`
		}{Name: n, DownloadURL: "https://x/" + n})
	}
	got := pickLlamaLinuxAsset(rel, false)
	if got.Name != "" {
		t.Errorf("got %q, want empty", got.Name)
	}
}

// ─── fetchLatestLlamaRelease ───────────────────────────────────────────────

func TestFetchLatestLlamaRelease_OK(t *testing.T) {
	body := fakeReleaseJSON("b9172", []string{"llama-b9172-bin-ubuntu-x64.tar.gz"})
	rt := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if !strings.Contains(req.URL.String(), "api.github.com") {
			return nil, fmt.Errorf("unexpected URL: %s", req.URL)
		}
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})
	env, _, _ := newLlamaTestEnv(t, llamaTestOpts{transport: rt})
	r, err := fetchLatestLlamaRelease(env)
	if err != nil {
		t.Fatal(err)
	}
	if r.TagName != "b9172" {
		t.Errorf("tag = %q", r.TagName)
	}
	if len(r.Assets) != 1 {
		t.Errorf("assets = %d, want 1", len(r.Assets))
	}
}

func TestFetchLatestLlamaRelease_RateLimited(t *testing.T) {
	rt := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 403,
			Body:       io.NopCloser(strings.NewReader(`{"message":"rate limit exceeded"}`)),
			Header:     make(http.Header),
		}, nil
	})
	env, _, _ := newLlamaTestEnv(t, llamaTestOpts{transport: rt})
	_, err := fetchLatestLlamaRelease(env)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Errorf("error missing $GITHUB_TOKEN hint: %v", err)
	}
}

func TestFetchLatestLlamaRelease_SendsAuthHeader(t *testing.T) {
	var sawAuth string
	rt := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		sawAuth = req.Header.Get("Authorization")
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(fakeReleaseJSON("b1", nil))),
			Header:     make(http.Header),
		}, nil
	})
	env, _, _ := newLlamaTestEnv(t, llamaTestOpts{
		transport: rt,
		env:       map[string]string{"GITHUB_TOKEN": "ghp_secret"},
	})
	if _, err := fetchLatestLlamaRelease(env); err != nil {
		t.Fatal(err)
	}
	if sawAuth != "Bearer ghp_secret" {
		t.Errorf("Authorization header = %q, want Bearer ghp_secret", sawAuth)
	}
}

// ─── isPlainUbuntuX64Asset ─────────────────────────────────────────────────

func TestIsPlainUbuntuX64Asset(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"llama-b9172-bin-ubuntu-x64.tar.gz", true},
		{"llama-b9172-bin-ubuntu-x64.zip", true},
		{"llama-b9172-bin-ubuntu-vulkan-x64.tar.gz", false},
		{"llama-b9172-bin-ubuntu-rocm-7.2-x64.tar.gz", false},
		{"llama-b9172-bin-ubuntu-cuda-12.4-x64.tar.gz", false},
		{"llama-b9172-bin-macos-x64.tar.gz", false},
		{"llama-b9172-bin-ubuntu-arm64.tar.gz", false},
	}
	for _, tc := range cases {
		got := isPlainUbuntuX64Asset(strings.ToLower(tc.name))
		if got != tc.want {
			t.Errorf("isPlainUbuntuX64Asset(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// ─── installLlamaCpp dispatch & branches ───────────────────────────────────

func TestInstallLlamaCpp_UserCancels(t *testing.T) {
	env, _, _ := newLlamaTestEnv(t, llamaTestOpts{
		osRelease: `ID=arch` + "\n",
		method:    llamaMethodCancel,
	})
	err := installLlamaCpp(env)
	if err == nil {
		t.Fatal("expected error when user cancels")
	}
	if !strings.Contains(err.Error(), "declined") {
		t.Errorf("err = %v", err)
	}
}

func TestInstallLlamaCpp_ArchDistroPathPrintsPacman(t *testing.T) {
	// pacman -Si llama.cpp succeeds (stub returns /bin/true).
	stub := func(name string, args []string) *exec.Cmd {
		if name == "pacman" && len(args) >= 2 && args[0] == "-Si" {
			return exec.Command("/bin/true")
		}
		// llama-server --version verify step
		if strings.HasSuffix(name, "llama-server") {
			return exec.Command("/bin/true")
		}
		return exec.Command("/bin/false")
	}
	env, out, _ := newLlamaTestEnv(t, llamaTestOpts{
		osRelease:  `ID=arch` + "\n",
		method:     llamaMethodDistro,
		runHandler: stub,
	})
	if err := installLlamaCpp(env); err != nil {
		t.Fatalf("install: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "sudo pacman -S llama.cpp") {
		t.Errorf("missing pacman install hint in output:\n%s", s)
	}
	// Critical: we must NOT have run sudo or pacman -S ourselves.
	if strings.Contains(s, "[run ] sudo") {
		t.Errorf("installer ran sudo on the user's behalf")
	}
}

func TestInstallLlamaCpp_ArchPackageNotAvailableFallsThrough(t *testing.T) {
	// pacman -Si llama.cpp exits non-zero (package not found). Then the
	// installer falls through to the tarball path, which fetches the
	// GitHub release JSON. We provide a release with a tar.gz containing
	// a `bin/llama-server`.
	pkgBytes := makeTarGzWithBinary(t, "build/bin/llama-server")
	rt := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.String(), "api.github.com") {
			body := fakeReleaseJSON("b9172", []string{"llama-b9172-bin-ubuntu-x64.tar.gz"})
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		}
		if strings.Contains(req.URL.String(), "fake.example.com") {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(pkgBytes)), Header: make(http.Header)}, nil
		}
		return nil, fmt.Errorf("unexpected URL: %s", req.URL)
	})
	stub := func(name string, args []string) *exec.Cmd {
		if name == "pacman" && len(args) >= 2 && args[0] == "-Si" {
			return exec.Command("/bin/false") // package not found
		}
		return exec.Command("/bin/true")
	}
	env, out, calls := newLlamaTestEnv(t, llamaTestOpts{
		osRelease:  `ID=arch` + "\n",
		method:     llamaMethodDistro,
		transport:  rt,
		runHandler: stub,
		env:        map[string]string{"PATH": filepath.Join(t.TempDir(), "bin")},
	})
	if err := installLlamaCpp(env); err != nil {
		t.Fatalf("install: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "falling back to release tarball") {
		t.Errorf("missing fallback message:\n%s", s)
	}
	// Pacman query DID happen.
	sawPacman := false
	for _, c := range *calls {
		if strings.HasPrefix(c, "pacman -Si") {
			sawPacman = true
		}
	}
	if !sawPacman {
		t.Error("expected `pacman -Si` to be queried")
	}
	// Symlink was created.
	symlink := filepath.Join(env.home, ".local", "bin", "llama-server")
	target, err := os.Readlink(symlink)
	if err != nil {
		t.Fatalf("symlink missing: %v", err)
	}
	if filepath.Base(target) != "llama-server" {
		t.Errorf("symlink target = %q, want a file named llama-server", target)
	}
}

func TestInstallLlamaCpp_UbuntuFallsThroughToTarball(t *testing.T) {
	// Phase 1: Ubuntu has no llama-cpp package. The installer should NOT
	// run apt-cache / apt-get itself; it should print the "not available"
	// message and fall through.
	pkgBytes := makeTarGzWithBinary(t, "bin/llama-server")
	rt := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.String(), "api.github.com") {
			body := fakeReleaseJSON("b9172", []string{"llama-b9172-bin-ubuntu-x64.tar.gz"})
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(pkgBytes)), Header: make(http.Header)}, nil
	})
	env, out, calls := newLlamaTestEnv(t, llamaTestOpts{
		osRelease: `ID=ubuntu` + "\n" + `ID_LIKE=debian` + "\n",
		method:    llamaMethodDistro,
		transport: rt,
	})
	if err := installLlamaCpp(env); err != nil {
		t.Fatalf("install: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "no `llama-cpp` package in standard Ubuntu/Debian repos") {
		t.Errorf("missing not-available message:\n%s", s)
	}
	if !strings.Contains(s, "falling back to release tarball") {
		t.Errorf("missing fallback message:\n%s", s)
	}
	// Critical: no apt-get / apt-cache invocation.
	for _, c := range *calls {
		if strings.HasPrefix(c, "apt-get") || strings.HasPrefix(c, "apt-cache") || strings.HasPrefix(c, "apt ") {
			t.Errorf("unexpected apt invocation: %s", c)
		}
	}
}

func TestInstallLlamaCpp_ReleasePathWithYes(t *testing.T) {
	// With --yes, no prompt; method defaults to release tarball.
	pkgBytes := makeTarGzWithBinary(t, "build/bin/llama-server")
	rt := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.String(), "api.github.com") {
			body := fakeReleaseJSON("b9172", []string{
				"llama-b9172-bin-ubuntu-x64.tar.gz",
				"llama-b9172-bin-ubuntu-vulkan-x64.tar.gz",
				"llama-b9172-bin-win-cuda-12.4-x64.zip",
			})
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		}
		// Confirm we hit the plain ubuntu-x64 asset, not a variant.
		if !strings.HasSuffix(req.URL.String(), "llama-b9172-bin-ubuntu-x64.tar.gz") {
			return nil, fmt.Errorf("unexpected download URL: %s", req.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(pkgBytes)), Header: make(http.Header)}, nil
	})
	env, out, _ := newLlamaTestEnv(t, llamaTestOpts{
		osRelease: `ID=ubuntu` + "\n",
		yes:       true,
		transport: rt,
		env: map[string]string{
			"PATH": filepath.Join(t.TempDir(), "elsewhere"),
		},
	})
	if err := installLlamaCpp(env); err != nil {
		t.Fatalf("install: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "picked asset: llama-b9172-bin-ubuntu-x64.tar.gz") {
		t.Errorf("wrong asset picked; out:\n%s", s)
	}
	// Symlink exists.
	if _, err := os.Lstat(filepath.Join(env.home, ".local", "bin", "llama-server")); err != nil {
		t.Errorf("symlink not created: %v", err)
	}
	// PATH warning fired because tmp PATH doesn't contain ~/.local/bin.
	if !strings.Contains(s, "is not on $PATH") {
		t.Errorf("missing PATH warning:\n%s", s)
	}
}

func TestInstallLlamaCpp_ReleasePathSymlinkAlreadyExists(t *testing.T) {
	// Pre-create the symlink pointing at a real file. The installer
	// should skip the entire release path. No HTTP calls should fire.
	home := t.TempDir()
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	realTarget := filepath.Join(home, "real-llama-server")
	if err := os.WriteFile(realTarget, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realTarget, filepath.Join(binDir, "llama-server")); err != nil {
		t.Fatal(err)
	}
	rt := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("network must not be touched; got %s", req.URL)
	})
	var stdout bytes.Buffer
	env := &llamaInstallerEnv{
		home:          home,
		runCmd:        func(name string, args ...string) *exec.Cmd { return exec.Command("/bin/true") },
		chooseMethod:  func(string) llamaInstallMethod { return llamaMethodRelease },
		httpClient:    &http.Client{Transport: rt},
		readOSRelease: func() ([]byte, error) { return []byte("ID=ubuntu\n"), nil },
		stdout:        &stdout,
		stderr:        &stdout,
		lookPath:      func(name string) (string, error) { return "/usr/bin/" + name, nil },
		getEnv:        func(string) string { return "" },
		yes:           true,
	}
	if err := installLlamaCpp(env); err != nil {
		t.Fatalf("install: %v", err)
	}
	if !strings.Contains(stdout.String(), "[skip] symlink") {
		t.Errorf("missing skip-symlink message:\n%s", stdout.String())
	}
}

func TestInstallLlamaCpp_ReleasePathRateLimited(t *testing.T) {
	rt := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 403,
			Body:       io.NopCloser(strings.NewReader(`{"message":"API rate limit exceeded"}`)),
			Header:     make(http.Header),
		}, nil
	})
	env, _, _ := newLlamaTestEnv(t, llamaTestOpts{
		osRelease: `ID=ubuntu` + "\n",
		yes:       true,
		transport: rt,
	})
	err := installLlamaCpp(env)
	if err == nil {
		t.Fatal("expected error from rate-limited release fetch")
	}
	if !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Errorf("missing $GITHUB_TOKEN hint: %v", err)
	}
}

func TestInstallLlamaCpp_SourcePathPrintsOnly(t *testing.T) {
	env, out, calls := newLlamaTestEnv(t, llamaTestOpts{
		osRelease: `ID=ubuntu` + "\n",
		method:    llamaMethodSource,
	})
	if err := installLlamaCpp(env); err != nil {
		t.Fatalf("install: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "git clone https://github.com/ggerganov/llama.cpp") {
		t.Errorf("missing canonical git clone command:\n%s", s)
	}
	if !strings.Contains(s, "cmake -B build -DGGML_CUDA=ON") {
		t.Errorf("missing canonical cmake command:\n%s", s)
	}
	// Source path doesn't run subprocesses (no verify step either).
	if len(*calls) != 0 {
		t.Errorf("source path ran subprocesses: %v", *calls)
	}
}

func TestInstallLlamaCpp_SourcePathWithCUDAFlagStillPrintsCUDA(t *testing.T) {
	env, out, _ := newLlamaTestEnv(t, llamaTestOpts{
		osRelease: `ID=ubuntu` + "\n",
		method:    llamaMethodSource,
		cuda:      true,
	})
	if err := installLlamaCpp(env); err != nil {
		t.Fatalf("install: %v", err)
	}
	if !strings.Contains(out.String(), "-DGGML_CUDA=ON") {
		t.Errorf("missing CUDA flag in source build:\n%s", out.String())
	}
}

func TestInstallLlamaCpp_DistroUnknownForcesReleaseTarball(t *testing.T) {
	// User picks `d` but we couldn't detect their distro. We should
	// force them onto the release tarball path with a warning.
	pkgBytes := makeTarGzWithBinary(t, "bin/llama-server")
	rt := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.String(), "api.github.com") {
			body := fakeReleaseJSON("b9172", []string{"llama-b9172-bin-ubuntu-x64.tar.gz"})
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(pkgBytes)), Header: make(http.Header)}, nil
	})
	env, out, _ := newLlamaTestEnv(t, llamaTestOpts{
		// osRelease empty -> detectDistroID returns ""
		method:    llamaMethodDistro,
		transport: rt,
	})
	if err := installLlamaCpp(env); err != nil {
		t.Fatalf("install: %v", err)
	}
	if !strings.Contains(out.String(), "distro unknown; falling back to release tarball") {
		t.Errorf("missing distro-unknown warning:\n%s", out.String())
	}
}

// ─── extractTarGz / extractZip safety ──────────────────────────────────────

func TestExtractTarGz_BlocksTarSlip(t *testing.T) {
	// Build a malicious tarball with an entry that escapes via ../.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("evil")
	if err := tw.WriteHeader(&tar.Header{
		Name: "../escape", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()

	tmp := t.TempDir()
	archivePath := filepath.Join(tmp, "evil.tar.gz")
	if err := os.WriteFile(archivePath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(tmp, "extract")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	err := extractTarGz(archivePath, dest)
	if err == nil {
		t.Fatal("expected tar-slip refusal")
	}
	if !strings.Contains(err.Error(), "escapes dest") {
		t.Errorf("wrong refusal: %v", err)
	}
}

// ─── findExecutableUnder ───────────────────────────────────────────────────

func TestFindExecutableUnder_FindsNested(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "build", "bin", "llama-server")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := findExecutableUnder(root, "llama-server")
	if err != nil {
		t.Fatal(err)
	}
	if got != target {
		t.Errorf("got %q, want %q", got, target)
	}
}

func TestFindExecutableUnder_NotFound(t *testing.T) {
	root := t.TempDir()
	_, err := findExecutableUnder(root, "llama-server")
	if err == nil {
		t.Fatal("expected not-found error")
	}
}

// ─── doctorCmd dispatcher integration ──────────────────────────────────────

func TestDoctorCmd_InstallLlamaCpp_Recognised(t *testing.T) {
	// We can't actually run installLlamaCpp through the real cobra path
	// (it would hit the network), but we can prove the dispatch routing
	// works by checking that `--install llama-cpp --yes` returns
	// something other than the "unknown component" error. Hitting the
	// network with no transport set will fail, but the error message
	// won't say "unknown component" — which is the regression we're
	// guarding against.
	cmd := doctorCmd()
	cmd.SetArgs([]string{"--install", "llama-cpp", "--yes"})
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	// We can't easily inject env into the cobra command, so just observe
	// that the error is NOT the dispatcher's "unknown component" error.
	// If it errors elsewhere (network, etc.) that's fine — the routing
	// itself succeeded.
	err := cmd.Execute()
	if err != nil && strings.Contains(err.Error(), "unknown component") {
		t.Errorf("dispatcher rejected llama-cpp as unknown: %v", err)
	}
}
