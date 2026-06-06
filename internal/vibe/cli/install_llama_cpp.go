package cli

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// llamaInstallMethod is the user's chosen install path. The character matches
// the menu prompt ([d]istro / [r]elease tarball / [s]ource build / cancel).
type llamaInstallMethod string

const (
	llamaMethodDistro  llamaInstallMethod = "d"
	llamaMethodRelease llamaInstallMethod = "r"
	llamaMethodSource  llamaInstallMethod = "s"
	llamaMethodCancel  llamaInstallMethod = "c"
)

// llamaInputter prompts the user with the install-method menu and returns
// the chosen method. The default reads a line from stdin and matches the
// first character against d/r/s/c (case-insensitive). Tests stub it.
type llamaInputter func(prompt string) llamaInstallMethod

// llamaInstallerEnv bundles externals so tests can swap them out. Pattern
// mirrors install_comfyui.go's installerEnv: a commandRunner, a confirm
// prompt, an http.Client, an os-release reader, etc.
type llamaInstallerEnv struct {
	// home is the user's home directory. Tests set this to a tmp dir.
	home string
	// runCmd produces commands; tests return a stub.
	runCmd commandRunner
	// chooseMethod prompts the user for d/r/s/c; tests stub it.
	chooseMethod llamaInputter
	// httpClient performs GitHub API + tarball downloads. Tests stub it.
	httpClient *http.Client
	// readOSRelease returns the contents of /etc/os-release (or its
	// override location). Tests provide a synthetic stream.
	readOSRelease func() ([]byte, error)
	// stdout streams progress + tool output.
	stdout io.Writer
	// stderr is for surface errors.
	stderr io.Writer
	// lookPath resolves binaries on $PATH (apt-get, pacman, dnf, llama-server).
	lookPath func(string) (string, error)
	// getEnv returns environment variables ($GITHUB_TOKEN, $PATH).
	getEnv func(string) string
	// yes short-circuits the method-choice prompt to release-tarball (the
	// most-broadly-applicable path for automation).
	yes bool
	// cuda is set when --install-cuda is passed; prefers a CUDA-flavoured
	// release asset and warns when none exists for Linux.
	cuda bool
	// forcedMethod, when non-empty, skips the interactive prompt and
	// pins the install method. Set by the CLI's --method flag.
	forcedMethod llamaInstallMethod
	// update bypasses the "symlink already present" short-circuit so
	// the install path actually refreshes an existing installation
	// (re-fetches latest release / re-pulls + rebuilds source).
	update bool
}

// defaultLlamaInstallerEnv constructs an env with real implementations of
// every external. Tests should NOT use this — they should hand-build an env
// with stubs.
func defaultLlamaInstallerEnv(stdout, stderr io.Writer, yes, cuda, update bool) *llamaInstallerEnv {
	home, _ := os.UserHomeDir()
	return &llamaInstallerEnv{
		home:          home,
		runCmd:        exec.Command,
		chooseMethod:  promptLlamaMethod,
		httpClient:    &http.Client{Timeout: 60 * time.Second},
		readOSRelease: func() ([]byte, error) { return os.ReadFile("/etc/os-release") },
		stdout:        stdout,
		stderr:        stderr,
		lookPath:      exec.LookPath,
		getEnv:        os.Getenv,
		yes:           yes,
		cuda:          cuda,
		update:        update,
	}
}

// promptLlamaMethod reads a line from stdin and maps it to a method. Any
// unrecognised input becomes "cancel" so we err on the side of safe.
func promptLlamaMethod(prompt string) llamaInstallMethod {
	fmt.Print(prompt)
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return llamaMethodCancel
	}
	line = strings.TrimSpace(strings.ToLower(line))
	if line == "" {
		return llamaMethodCancel
	}
	switch line[0] {
	case 'd':
		return llamaMethodDistro
	case 'r':
		return llamaMethodRelease
	case 's':
		return llamaMethodSource
	default:
		return llamaMethodCancel
	}
}

// installLlamaCpp walks the install path. It branches on the chosen method,
// then unconditionally verifies `llama-server --version` at the end.
func installLlamaCpp(env *llamaInstallerEnv) error {
	// ── Step 1: detect distro from /etc/os-release. ─────────────────────
	distro := detectDistroID(env)
	if distro == "" {
		fmt.Fprintln(env.stdout, "[info] could not detect distro from /etc/os-release; distro path disabled")
	} else {
		fmt.Fprintf(env.stdout, "[info] detected distro: %s\n", distro)
	}

	// ── Step 2: choose install method. ──────────────────────────────────
	// Precedence: forcedMethod (--method flag) > --yes default > prompt.
	var method llamaInstallMethod
	switch {
	case env.forcedMethod != "":
		method = env.forcedMethod
	case env.yes:
		method = llamaMethodRelease
	default:
		menu := "Install llama.cpp (llama-server)? Choose method: [d]istro / [r]elease tarball / [s]ource build / [c]ancel: "
		method = env.chooseMethod(menu)
	}
	if method == llamaMethodCancel {
		fmt.Fprintln(env.stdout, "Aborted by user.")
		return errors.New("install: user declined")
	}
	// If the user picked distro but we couldn't detect one, force release.
	if method == llamaMethodDistro && distro == "" {
		fmt.Fprintln(env.stdout, "[warn] distro unknown; falling back to release tarball")
		method = llamaMethodRelease
	}

	// ── Step 3: dispatch. ───────────────────────────────────────────────
	switch method {
	case llamaMethodDistro:
		if err := installLlamaCppDistro(env, distro); err != nil {
			// Distro path "falls through to tarball" on any "not
			// available" signal. Other errors propagate.
			if errors.Is(err, errLlamaPkgUnavailable) {
				fmt.Fprintln(env.stdout, "[info] distro package not available; falling back to release tarball")
				if rerr := installLlamaCppRelease(env); rerr != nil {
					return rerr
				}
			} else {
				return err
			}
		}
	case llamaMethodRelease:
		if err := installLlamaCppRelease(env); err != nil {
			return err
		}
	case llamaMethodSource:
		if err := installLlamaCppSource(env); err != nil {
			return err
		}
	}

	// ── Step 4: verify. ─────────────────────────────────────────────────
	verifyLlamaInstall(env)
	return nil
}

// detectDistroID parses /etc/os-release and returns the lower-case ID field
// (e.g. "ubuntu", "debian", "arch", "fedora", "cachyos"). If the file is
// unreadable or malformed, returns "".
//
// Also folds ID_LIKE so Arch derivatives map to "arch", Debian derivatives
// to "debian", RHEL derivatives to "fedora".
func detectDistroID(env *llamaInstallerEnv) string {
	data, err := env.readOSRelease()
	if err != nil {
		return ""
	}
	var id, idLike string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"`)
		switch k {
		case "ID":
			id = strings.ToLower(v)
		case "ID_LIKE":
			idLike = strings.ToLower(v)
		}
	}
	// Canonicalise via ID first, then ID_LIKE as fallback.
	switch id {
	case "ubuntu", "debian", "arch", "fedora":
		return id
	}
	// Fold via ID_LIKE; the field may contain space-separated values.
	for _, tok := range strings.Fields(idLike) {
		switch tok {
		case "ubuntu", "debian":
			return "debian"
		case "arch":
			return "arch"
		case "fedora", "rhel":
			return "fedora"
		}
	}
	// Fall back to whatever ID is (might still be useful in messages).
	return id
}

// errLlamaPkgUnavailable is returned by the distro path when we know the
// package isn't installable on this system. The dispatcher uses it as a
// signal to fall through to the tarball path.
var errLlamaPkgUnavailable = errors.New("llama-cpp: package not available via distro")

// installLlamaCppDistro implements the [d] path. We never run sudo
// ourselves; we print the install command for the user. The function only
// queries package metadata (apt-cache, pacman -Si, dnf info) to verify the
// package exists; it does NOT install it.
func installLlamaCppDistro(env *llamaInstallerEnv, distro string) error {
	ctx := context.Background()
	switch distro {
	case "ubuntu", "debian":
		// Phase 1 reality: there is no `llama-cpp` package in the
		// standard Ubuntu/Debian repos. We could query apt-cache, but
		// the spec is explicit: treat as "not available, falling back
		// to tarball". We still document the probe for forward
		// compatibility.
		fmt.Fprintln(env.stdout, "[info] no `llama-cpp` package in standard Ubuntu/Debian repos (Phase 1)")
		return errLlamaPkgUnavailable
	case "arch":
		if _, err := env.lookPath("pacman"); err != nil {
			fmt.Fprintln(env.stdout, "[warn] pacman not on $PATH; cannot query Arch package")
			return errLlamaPkgUnavailable
		}
		// `pacman -Si llama.cpp` returns 0 iff the package exists in
		// the configured repos. We capture stderr so it doesn't leak
		// to the user on the not-found path.
		fmt.Fprintln(env.stdout, "[run ] pacman -Si llama.cpp (probing repo metadata)")
		cmd := env.runCmd("pacman", "-Si", "llama.cpp")
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if err := cmd.Run(); err != nil {
			fmt.Fprintln(env.stdout, "[info] pacman: llama.cpp not found in configured repos")
			return errLlamaPkgUnavailable
		}
		fmt.Fprintln(env.stdout, "[info] llama.cpp is available via pacman. Run:")
		fmt.Fprintln(env.stdout, "    sudo pacman -S llama.cpp")
		fmt.Fprintln(env.stdout, "(vibe does not invoke sudo for you; run the command above and re-run `vibe doctor` to verify.)")
		_ = ctx
		return nil
	case "fedora":
		if _, err := env.lookPath("dnf"); err != nil {
			fmt.Fprintln(env.stdout, "[warn] dnf not on $PATH; cannot query Fedora package")
			return errLlamaPkgUnavailable
		}
		fmt.Fprintln(env.stdout, "[run ] dnf info llama.cpp (probing repo metadata)")
		cmd := env.runCmd("dnf", "info", "llama.cpp")
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if err := cmd.Run(); err != nil {
			fmt.Fprintln(env.stdout, "[info] dnf: llama.cpp not found in configured repos")
			return errLlamaPkgUnavailable
		}
		fmt.Fprintln(env.stdout, "[info] llama.cpp is available via dnf. Run:")
		fmt.Fprintln(env.stdout, "    sudo dnf install llama.cpp")
		fmt.Fprintln(env.stdout, "(vibe does not invoke sudo for you; run the command above and re-run `vibe doctor` to verify.)")
		return nil
	default:
		fmt.Fprintf(env.stdout, "[info] distro %q has no built-in handler; falling back to release tarball\n", distro)
		return errLlamaPkgUnavailable
	}
}

// llamaGitHubRelease is the slice of the GitHub release JSON we care about.
// Anything else in the response is silently ignored by encoding/json.
type llamaGitHubRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name        string `json:"name"`
		DownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

// installLlamaCppRelease implements the [r] path: fetch latest release JSON,
// pick a Linux x86_64 asset, download + extract to ~/.local/share/vibe/llama-cpp/,
// symlink llama-server into ~/.local/bin/llama-server.
func installLlamaCppRelease(env *llamaInstallerEnv) error {
	installRoot := filepath.Join(env.home, ".local", "share", "vibe", "llama-cpp")
	binDir := filepath.Join(env.home, ".local", "bin")
	symlink := filepath.Join(binDir, "llama-server")

	// Idempotency: if the symlink already points to a real file, skip —
	// unless --update was passed, in which case the caller wants us to
	// fetch the latest release and refresh the install regardless.
	if target, err := os.Readlink(symlink); err == nil {
		if _, statErr := os.Stat(target); statErr == nil {
			if !env.update {
				fmt.Fprintf(env.stdout, "[skip] symlink: %s -> %s already present (pass --update to refresh)\n", symlink, target)
				pathWarning(env, binDir)
				return nil
			}
			fmt.Fprintf(env.stdout, "[info] --update: existing install at %s will be refreshed\n", target)
		}
	}

	// Fetch latest release metadata.
	release, err := fetchLatestLlamaRelease(env)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.stdout, "[info] latest release: %s (%d assets)\n", release.TagName, len(release.Assets))

	asset := pickLlamaAsset(release, env.cuda, runtime.GOOS, runtime.GOARCH)
	if asset.Name == "" {
		if env.cuda {
			return errors.New("no CUDA-flavoured Linux x86_64 asset found; build from source with -DGGML_CUDA=ON (try `vibe doctor --install llama-cpp` and pick [s])")
		}
		return fmt.Errorf("no llama.cpp release asset found for %s/%s; build from source or install via your OS package manager", runtime.GOOS, runtime.GOARCH)
	}
	fmt.Fprintf(env.stdout, "[info] picked asset: %s\n", asset.Name)

	// Download + extract.
	if err := os.MkdirAll(installRoot, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", installRoot, err)
	}
	archivePath := filepath.Join(installRoot, asset.Name)
	if err := downloadFile(env, asset.DownloadURL, archivePath); err != nil {
		return fmt.Errorf("download %s: %w", asset.Name, err)
	}
	extractDir := filepath.Join(installRoot, release.TagName)
	if err := os.MkdirAll(extractDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", extractDir, err)
	}
	fmt.Fprintf(env.stdout, "[run ] extract %s -> %s\n", asset.Name, extractDir)
	if err := extractArchive(archivePath, extractDir); err != nil {
		return fmt.Errorf("extract: %w", err)
	}

	// Locate the llama-server binary inside the extracted tree. Releases
	// commonly nest it under `build/bin/` or just `bin/`. Walk and pick
	// the first match.
	serverPath, err := findExecutableUnder(extractDir, "llama-server")
	if err != nil {
		return fmt.Errorf("locate llama-server in extracted tree: %w", err)
	}
	// Ensure the binary is executable (zip preserves mode but tar.gz
	// extraction respects header bits; we set 0755 defensively).
	if err := os.Chmod(serverPath, 0o755); err != nil {
		// Non-fatal; surface as a warning.
		fmt.Fprintf(env.stdout, "[warn] chmod %s: %v\n", serverPath, err)
	}

	// Drop the symlink. Remove any stale one first so this is idempotent.
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", binDir, err)
	}
	_ = os.Remove(symlink) // ignore "not exist" — we recreate unconditionally
	if err := os.Symlink(serverPath, symlink); err != nil {
		return fmt.Errorf("symlink %s -> %s: %w", symlink, serverPath, err)
	}
	fmt.Fprintf(env.stdout, "[ok  ] symlink %s -> %s\n", symlink, serverPath)
	pathWarning(env, binDir)
	return nil
}

// pathWarning warns if binDir isn't on $PATH. Skipping it would mean the
// just-installed llama-server is technically present but not reachable by
// the rest of vibe (`vibe doctor` looks it up via $PATH).
func pathWarning(env *llamaInstallerEnv, binDir string) {
	path := env.getEnv("PATH")
	for _, p := range strings.Split(path, string(os.PathListSeparator)) {
		if p == binDir {
			return
		}
	}
	fmt.Fprintf(env.stdout, "[warn] %s is not on $PATH. Add this to your shell rc:\n", binDir)
	fmt.Fprintf(env.stdout, "    export PATH=\"%s:$PATH\"\n", binDir)
}

// fetchLatestLlamaRelease GETs the GitHub releases endpoint and decodes the
// response. Handles 403 (rate-limit) with a $GITHUB_TOKEN hint.
func fetchLatestLlamaRelease(env *llamaInstallerEnv) (*llamaGitHubRelease, error) {
	const url = "https://api.github.com/repos/ggerganov/llama.cpp/releases/latest"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if token := env.getEnv("GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := env.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read GitHub response: %w", err)
	}
	if resp.StatusCode == http.StatusForbidden {
		hint := "GitHub API returned 403 (likely unauthenticated rate-limit). Set $GITHUB_TOKEN to a personal access token and retry."
		return nil, errors.New(strings.TrimRight(hint, ".\n"))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var release llamaGitHubRelease
	if err := json.Unmarshal(body, &release); err != nil {
		return nil, fmt.Errorf("decode GitHub release JSON: %w", err)
	}
	return &release, nil
}

// pickLlamaAsset dispatches to the per-platform picker based on goos/goarch.
// Today we ship release-tarball installs for linux/amd64 and darwin/arm64 +
// darwin/amd64; everything else returns an empty result so the caller can
// surface a "build from source / use your package manager" error.
//
// The cuda flag is only meaningful on linux/amd64 — macOS Metal builds are
// always GPU-accelerated, there's no separate "CUDA" variant to pick. Other
// platforms ignore the flag.
func pickLlamaAsset(release *llamaGitHubRelease, cuda bool, goos, goarch string) struct{ Name, DownloadURL string } {
	switch goos {
	case "linux":
		if goarch == "amd64" {
			return pickLlamaLinuxAsset(release, cuda)
		}
	case "darwin":
		return pickLlamaMacOSAsset(release, goarch)
	}
	return struct{ Name, DownloadURL string }{}
}

// pickLlamaMacOSAsset chooses the macOS release tarball matching goarch.
// llama.cpp upstream ships `llama-<ver>-bin-macos-arm64.tar.gz` (Apple
// Silicon, Metal-enabled) and `llama-<ver>-bin-macos-x64.tar.gz` (Intel Mac).
// The plain build is preferred over the `kleidiai` variant — kleidiai is an
// ARM CPU-only optimization path, but we want the Metal-default build for
// the unified-memory GPU use case.
func pickLlamaMacOSAsset(release *llamaGitHubRelease, goarch string) struct{ Name, DownloadURL string } {
	out := struct{ Name, DownloadURL string }{}
	var archToken string
	switch goarch {
	case "arm64":
		archToken = "arm64"
	case "amd64":
		archToken = "x64"
	default:
		return out
	}
	target := "bin-macos-" + archToken
	for _, a := range release.Assets {
		name := strings.ToLower(a.Name)
		if !strings.Contains(name, target) {
			continue
		}
		if !strings.HasSuffix(name, ".tar.gz") && !strings.HasSuffix(name, ".tgz") && !strings.HasSuffix(name, ".zip") {
			continue
		}
		// Skip alternative backends. `kleidiai` is a CPU-only ARM path; we
		// want the Metal-default plain build. Add new exclusions here if
		// upstream starts shipping more variants.
		if strings.Contains(name, "kleidiai") {
			continue
		}
		out.Name = a.Name
		out.DownloadURL = a.DownloadURL
		return out
	}
	return out
}

// pickLlamaLinuxAsset chooses a release asset for x86_64 Linux. Preference
// order:
//
//  1. If cuda is true, look for an asset matching `bin-ubuntu-` AND
//     `cuda` (or `cublas`) AND an x86_64 suffix. (As of Phase 1 the
//     upstream project doesn't publish Linux-CUDA binaries, so this is
//     forward-looking — we'll fall back to the non-CUDA Ubuntu x64 build
//     if no CUDA variant exists, and the caller surfaces a "may want
//     to build from source" warning later.)
//  2. Otherwise, the generic `llama-<ver>-bin-ubuntu-x64.tar.gz` (or .zip
//     for back-compat with the spec's wording).
//
// We're permissive on the file extension because the project has shipped
// both `.zip` (early days) and `.tar.gz` (current). We never pick assets
// containing `sycl`, `rocm`, `vulkan`, `openvino`, or `arm` unless they
// match the CUDA preference path.
func pickLlamaLinuxAsset(release *llamaGitHubRelease, cuda bool) struct{ Name, DownloadURL string } {
	out := struct{ Name, DownloadURL string }{}
	// Excluded substrings — windows builds, mac builds, non-x86_64
	// arches, alternative accelerator backends, etc.
	exclude := []string{
		"win-", "win.", "macos", "darwin",
		"arm64", "aarch64", "android",
		"s390x", "openvino", "openeuler",
	}
	// When CUDA isn't requested we also exclude other-accelerator builds.
	if !cuda {
		exclude = append(exclude, "rocm", "sycl", "vulkan", "hip", "opencl")
	}

	var (
		preferred    string
		preferredURL string
		fallback     string
		fallbackURL  string
	)
	for _, a := range release.Assets {
		name := strings.ToLower(a.Name)
		// Must be a Linux x86_64 binary build.
		if !strings.Contains(name, "bin-ubuntu-") {
			continue
		}
		if !strings.Contains(name, "x64") && !strings.Contains(name, "x86_64") && !strings.Contains(name, "amd64") {
			continue
		}
		// Skip non-archive miscellany.
		if !strings.HasSuffix(name, ".tar.gz") && !strings.HasSuffix(name, ".tgz") && !strings.HasSuffix(name, ".zip") {
			continue
		}
		skip := false
		for _, ex := range exclude {
			if strings.Contains(name, ex) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		if cuda && (strings.Contains(name, "cuda") || strings.Contains(name, "cublas")) {
			if preferred == "" {
				preferred = a.Name
				preferredURL = a.DownloadURL
			}
			continue
		}
		// Generic Linux x86_64 build (no accelerator suffix).
		if !cuda && fallback == "" {
			// Prefer the plain `bin-ubuntu-x64` over any other
			// remaining variant. We do this by checking that no
			// extra hyphenated qualifier appears between `ubuntu-`
			// and `x64`.
			if isPlainUbuntuX64Asset(name) {
				fallback = a.Name
				fallbackURL = a.DownloadURL
			} else if fallback == "" {
				// Hold onto any other Ubuntu x64 asset as a
				// last-resort, but only if a plain one
				// doesn't appear later.
				fallback = a.Name
				fallbackURL = a.DownloadURL
			}
		}
	}
	if preferred != "" {
		out.Name = preferred
		out.DownloadURL = preferredURL
		return out
	}
	if fallback != "" {
		out.Name = fallback
		out.DownloadURL = fallbackURL
	}
	return out
}

// isPlainUbuntuX64Asset returns true for `llama-<ver>-bin-ubuntu-x64.<ext>`
// — the no-accelerator generic Linux build — and false for variants like
// `bin-ubuntu-vulkan-x64.<ext>`.
func isPlainUbuntuX64Asset(name string) bool {
	// Find the `bin-ubuntu-` anchor.
	idx := strings.Index(name, "bin-ubuntu-")
	if idx < 0 {
		return false
	}
	// What follows should be exactly `x64.<ext>` (or `x86_64.<ext>`).
	rest := name[idx+len("bin-ubuntu-"):]
	for _, suffix := range []string{"x64.tar.gz", "x64.tgz", "x64.zip", "x86_64.tar.gz", "x86_64.tgz", "x86_64.zip", "amd64.tar.gz", "amd64.tgz", "amd64.zip"} {
		if rest == suffix {
			return true
		}
	}
	return false
}

// downloadFile streams the URL body to `dest`. Existing files at `dest` are
// overwritten (this is the "we are starting fresh" branch; idempotency at
// the higher level is handled via the symlink check).
func downloadFile(env *llamaInstallerEnv, url, dest string) error {
	fmt.Fprintf(env.stdout, "[run ] download %s -> %s\n", url, dest)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	resp, err := env.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return err
	}
	return nil
}

// extractArchive dispatches on file extension. Supports .tar.gz/.tgz and
// .zip; anything else is an error.
func extractArchive(archivePath, dest string) error {
	lower := strings.ToLower(archivePath)
	switch {
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return extractTarGz(archivePath, dest)
	case strings.HasSuffix(lower, ".zip"):
		return extractZip(archivePath, dest)
	default:
		return fmt.Errorf("unsupported archive format: %s", archivePath)
	}
}

// extractTarGz unpacks a gzipped tar into dest. Refuses entries whose
// resolved path escapes `dest` (zip-slip / tar-slip).
func extractTarGz(archivePath, dest string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	absDest, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dest, hdr.Name)
		absTarget, err := filepath.Abs(target)
		if err != nil {
			return err
		}
		// Prevent tar-slip.
		if !strings.HasPrefix(absTarget, absDest+string(os.PathSeparator)) && absTarget != absDest {
			return fmt.Errorf("tar entry escapes dest: %s", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		case tar.TypeSymlink:
			// Skip symlinks inside the archive; they shouldn't be
			// needed for the binary release and complicate the
			// escape check.
			continue
		default:
			// Skip other types (char/block/fifo).
			continue
		}
	}
}

// extractZip unpacks a zip into dest with the same escape guard as
// extractTarGz.
func extractZip(archivePath, dest string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer zr.Close()
	absDest, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	for _, zf := range zr.File {
		target := filepath.Join(dest, zf.Name)
		absTarget, err := filepath.Abs(target)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(absTarget, absDest+string(os.PathSeparator)) && absTarget != absDest {
			return fmt.Errorf("zip entry escapes dest: %s", zf.Name)
		}
		if zf.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		mode := zf.Mode().Perm()
		if mode == 0 {
			mode = 0o644
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			rc.Close()
			return err
		}
		if _, err := io.Copy(out, rc); err != nil {
			rc.Close()
			out.Close()
			return err
		}
		rc.Close()
		out.Close()
	}
	return nil
}

// findExecutableUnder walks `root` and returns the first regular file whose
// basename equals `name`. Errors if none found.
func findExecutableUnder(root, name string) (string, error) {
	var found string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if filepath.Base(p) == name && info.Mode().IsRegular() {
			found = p
			return filepath.SkipDir // short-circuit to the basename; sibling files in the same dir are skipped
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("not found: %s under %s", name, root)
	}
	return found, nil
}

// installLlamaCppSource clones llama.cpp from upstream main, cmakes it,
// builds with --parallel, and symlinks build/bin/llama-server into
// ~/.local/bin/. This is the path users take when they want features
// that haven't shipped in a release tag yet (e.g. MTP support).
//
// Pre-flight verifies git + cmake + a C++ compiler exist; with cuda=true
// nvcc is also required. The build itself streams stdout/stderr to the
// user so they see compile progress — a fresh build is 5-15 min on
// modern hardware.
//
// Idempotent: rerunning re-pulls from origin (git fetch + reset --hard
// origin/main), rebuilds incrementally, refreshes the symlink. The
// "wipe and re-clone" path isn't worth the disk write for routine
// updates.
func installLlamaCppSource(env *llamaInstallerEnv) error {
	// ── Pre-flight: required toolchain. ─────────────────────────────────
	required := []string{"git", "cmake"}
	if env.cuda {
		required = append(required, "nvcc")
	}
	for _, bin := range required {
		if _, err := env.lookPath(bin); err != nil {
			return fmt.Errorf("source build needs %s on $PATH (install it and retry)", bin)
		}
	}
	// At least one C++ compiler. cmake will discover it but we want to
	// fail fast with a helpful message rather than 30s into the cmake
	// configure step.
	var cxxFound string
	for _, bin := range []string{"c++", "g++", "clang++"} {
		if _, err := env.lookPath(bin); err == nil {
			cxxFound = bin
			break
		}
	}
	if cxxFound == "" {
		return errors.New("source build needs a C++ compiler (c++, g++, or clang++) on $PATH")
	}
	fmt.Fprintf(env.stdout, "[info] toolchain: git, cmake, %s present\n", cxxFound)

	// ── Clone or update. ────────────────────────────────────────────────
	srcRoot := filepath.Join(env.home, ".local", "share", "vibe", "llama-cpp-source")
	repoDir := filepath.Join(srcRoot, "llama.cpp")
	binDir := filepath.Join(env.home, ".local", "bin")
	symlink := filepath.Join(binDir, "llama-server")

	if err := os.MkdirAll(srcRoot, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", srcRoot, err)
	}

	if _, err := os.Stat(filepath.Join(repoDir, ".git")); err == nil {
		fmt.Fprintf(env.stdout, "[run ] git -C %s fetch origin\n", repoDir)
		fetch := env.runCmd("git", "-C", repoDir, "fetch", "origin")
		fetch.Stdout = env.stdout
		fetch.Stderr = env.stderr
		if err := fetch.Run(); err != nil {
			return fmt.Errorf("git fetch: %w", err)
		}
		fmt.Fprintf(env.stdout, "[run ] git -C %s reset --hard origin/master\n", repoDir)
		reset := env.runCmd("git", "-C", repoDir, "reset", "--hard", "origin/master")
		reset.Stdout = env.stdout
		reset.Stderr = env.stderr
		if err := reset.Run(); err != nil {
			return fmt.Errorf("git reset: %w", err)
		}
	} else {
		fmt.Fprintf(env.stdout, "[run ] git clone https://github.com/ggerganov/llama.cpp %s\n", repoDir)
		clone := env.runCmd("git", "clone", "https://github.com/ggerganov/llama.cpp", repoDir)
		clone.Stdout = env.stdout
		clone.Stderr = env.stderr
		if err := clone.Run(); err != nil {
			return fmt.Errorf("git clone: %w", err)
		}
	}

	// ── Configure. ──────────────────────────────────────────────────────
	cfgArgs := []string{"-B", "build"}
	if env.cuda {
		cfgArgs = append(cfgArgs, "-DGGML_CUDA=ON")
	}
	fmt.Fprintf(env.stdout, "[run ] cmake %s (in %s)\n", strings.Join(cfgArgs, " "), repoDir)
	cfg := env.runCmd("cmake", cfgArgs...)
	cfg.Dir = repoDir
	cfg.Stdout = env.stdout
	cfg.Stderr = env.stderr
	if err := cfg.Run(); err != nil {
		return fmt.Errorf("cmake configure: %w", err)
	}

	// ── Build. ──────────────────────────────────────────────────────────
	// --parallel without a value asks cmake to pick a sensible job count
	// based on $CMAKE_BUILD_PARALLEL_LEVEL or the host's nproc. Release
	// config matters for ggml hot-path optimization.
	buildArgs := []string{"--build", "build", "--config", "Release", "--parallel", "--target", "llama-server"}
	fmt.Fprintf(env.stdout, "[run ] cmake %s   (this takes 5-15 min on a fresh checkout)\n", strings.Join(buildArgs, " "))
	build := env.runCmd("cmake", buildArgs...)
	build.Dir = repoDir
	build.Stdout = env.stdout
	build.Stderr = env.stderr
	if err := build.Run(); err != nil {
		return fmt.Errorf("cmake build: %w", err)
	}

	// ── Locate the built binary and refresh the symlink. ────────────────
	serverPath, err := findExecutableUnder(filepath.Join(repoDir, "build"), "llama-server")
	if err != nil {
		return fmt.Errorf("locate llama-server after build: %w", err)
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", binDir, err)
	}
	_ = os.Remove(symlink)
	if err := os.Symlink(serverPath, symlink); err != nil {
		return fmt.Errorf("symlink %s -> %s: %w", symlink, serverPath, err)
	}
	fmt.Fprintf(env.stdout, "[ok  ] symlink %s -> %s\n", symlink, serverPath)
	pathWarning(env, binDir)
	return nil
}

// verifyLlamaInstall runs `llama-server --version` and reports whatever it
// produces. Doesn't fail the install if the verify step errors — the user
// might need to refresh $PATH (cached shell hash) or open a new shell.
func verifyLlamaInstall(env *llamaInstallerEnv) {
	path, err := env.lookPath("llama-server")
	if err != nil {
		fmt.Fprintln(env.stdout, "[warn] llama-server still not on $PATH after install. Open a new shell or `hash -r` to refresh.")
		return
	}
	fmt.Fprintf(env.stdout, "[run ] %s --version\n", path)
	cmd := env.runCmd(path, "--version")
	cmd.Stdout = env.stdout
	cmd.Stderr = env.stdout
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(env.stdout, "[warn] `llama-server --version` exited non-zero: %v (some builds write to stderr; output above may still be the version)\n", err)
		return
	}
	fmt.Fprintln(env.stdout, "[ok  ] llama-server install verified")
}
