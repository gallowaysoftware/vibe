package cli

// The gates on install.sh — the script this project asks strangers to pipe
// into `sh`, and the one shell file in the repo that nothing in CI has ever
// run. They live in package cli because both claims are about THIS package's
// output: `vibe --version` is rendered here (root.go's version template over
// internal/buildinfo), and install.sh parses it.
//
// The script is never executed by these tests. It downloads release archives
// into $HOME/.local/bin and ends in `exec vibe doctor`; a test that ran it
// would install over the operator's own binaries.

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func installScript(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "..", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("install.sh is missing: %v", err)
	}
	return string(b)
}

// TestInstallScriptIdempotencyMatchesWhatAReleasedBinaryPrints pins the two
// ends of install.sh's idempotency check against each other, because they had
// silently disagreed since the check was written.
//
// goreleaser injects `{{ .Version }}` into internal/buildinfo, and that
// template strips the leading `v` — so a binary from tag v1.2.3 answers
// `--version` with `1.2.3 (sha, date)`, while the script's $VERSION is the
// tag. `[ "$existing" = "$VERSION" ]` therefore never matched: every run
// re-downloaded, and `--force`, whose only job is to override this check, was
// inert.
//
// The binary is built with goreleaser-equivalent ldflags rather than
// asserting against buildinfo.String() directly, because the thing that has
// to be true is what the SHIPPED command prints — the version template lives
// in root.go and could put a "vibe version " prefix in front of it tomorrow.
func TestInstallScriptIdempotencyMatchesWhatAReleasedBinaryPrints(t *testing.T) {
	const tag = "v1.2.3"
	bare := strings.TrimPrefix(tag, "v") // goreleaser's {{ .Version }}

	bin := filepath.Join(t.TempDir(), "vibe")
	const pkg = "github.com/gallowaysoftware/vibe/internal/buildinfo"
	build := exec.CommandContext(t.Context(), "go", "build",
		"-ldflags", "-X "+pkg+".Version="+bare+" -X "+pkg+".Commit=abc1234 -X "+pkg+".Date=2026-01-02",
		"-o", bin, "../../../cmd/vibe")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build vibe: %v\n%s", err, out)
	}
	raw, err := exec.CommandContext(t.Context(), bin, "--version").Output()
	if err != nil {
		t.Fatalf("vibe --version: %v", err)
	}

	// Exactly what the script does: `head -n 1 | awk '{print $1}'`.
	firstLine, _, _ := strings.Cut(string(raw), "\n")
	existing := strings.Fields(firstLine)[0]
	if existing != bare {
		t.Fatalf("a goreleaser-built vibe prints %q as its first field; install.sh compares that against "+
			"VERSION_NUM (%q). If this changed, the idempotency check silently stopped firing again.", existing, bare)
	}

	// And the script must compare against the bare version. Read the
	// comparison out of the file rather than trusting the sentence above:
	// the empirical half proves what the binary says, this proves what the
	// script listens for, and only together do they prove the check can fire.
	var compare string
	sc := bufio.NewScanner(strings.NewReader(installScript(t)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "#") || !strings.Contains(line, "$VERSION") {
			continue
		}
		if strings.Contains(line, "existing") && strings.Contains(line, "=") && strings.HasPrefix(line, "if ") {
			compare = line
			break
		}
	}
	if compare == "" {
		t.Fatal("install.sh has no idempotency comparison against the installed binary's version at all")
	}
	if !strings.Contains(compare, `"$VERSION_NUM"`) {
		t.Errorf("the idempotency check reads %q. It must compare against $VERSION_NUM (the bare `1.2.3` a "+
			"released binary prints), not $VERSION (the `v1.2.3` tag) — against the tag it can never match, "+
			"which re-downloads on every run and makes --force inert.", compare)
	}
}

// TestInstallScriptVerifiesOnEveryPlatformAndRefusesWhatItCannotVerify.
//
// sha256sum is coreutils: stock macOS does not have it, and the whole
// verification block used to hang off `command -v sha256sum`, so every darwin
// install was unverified — with a warning, on a curl | sh path where a warning
// scrolls past.
//
// Two claims, both structural because the script must not be executed: that a
// hasher is found on macOS too, and that "we could not verify this" is refused
// rather than warned about. The mismatch path was already fatal and stays
// that way.
func TestInstallScriptVerifiesOnEveryPlatformAndRefusesWhatItCannotVerify(t *testing.T) {
	script := installScript(t)

	for _, want := range []string{
		"sha256sum ",           // linux / coreutils
		"shasum -a 256",        // stock macOS (perl)
		"openssl dgst -sha256", // the rest
	} {
		if !strings.Contains(script, want) {
			t.Errorf("install.sh never runs %q: a box without the other hashers installs unverified", want)
		}
	}

	// Each way verification can fail to happen goes through unverified(),
	// which is fatal unless the operator opted out by name.
	for _, want := range []string{
		"unverified \"checksums.txt carries no line for",
		"unverified \"no sha256 tool on this box",
		"unverified \"could not download checksums.txt",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("install.sh does not route %q through unverified(); an unverifiable archive must not be "+
				"installed on the strength of a warning nobody sees in a pipeline", want)
		}
	}
	if !strings.Contains(script, "VIBE_INSECURE_SKIP_CHECKSUM") {
		t.Error("there is no named opt-out: refusing with no way through leaves an operator editing the installer")
	}
	// And unverified() is FATAL, which is the entire claim above. Routing
	// the three call sites through a function proves nothing about what
	// the function does: change its one condition — `= "1"` to `!= "9"` is
	// enough — and every assertion here still passed while every
	// unverifiable archive installed with a warning, which is the exact
	// shape the block above says was removed. Measured: both install-script
	// tests green under that edit.
	body := shellFuncBody(t, script, "unverified")
	if !strings.Contains(body, `"${VIBE_INSECURE_SKIP_CHECKSUM:-0}" = "1"`) {
		t.Errorf("unverified() no longer gates its warn-and-continue path on the opt-out being exactly \"1\" "+
			"(defaulting to 0). An opt-out that reads a stray or absent value as ON is an installer with no "+
			"verification at all:\n%s", body)
	}
	if last := lastStatement(body); !strings.HasPrefix(last, "fatal ") {
		t.Errorf("unverified() ends with %q, not a fatal. The DEFAULT — no opt-out set — must refuse the "+
			"install; a warning on a `curl | sh` path is read by nobody:\n%s", last, body)
	}
	if !strings.Contains(script, `fatal "sha256 mismatch for`) {
		t.Error("a MISMATCH must stay fatal with no opt-out — that is the one case where something is known to be wrong")
	}
	// The shape that shipped: a skip path that warns and installs anyway.
	for _, gone := range []string{
		"skipping checksum verification",
		"skipping verification",
	} {
		if strings.Contains(script, gone) {
			t.Errorf("install.sh still says %q: an unverified install is refused, not announced", gone)
		}
	}
}

// shellFuncBody returns the lines between `name() {` and the closing brace
// at column 0. Deliberately naive: install.sh is one file this repo owns
// and every function in it is written that way, and a shell parser is a
// dependency this module is not taking for one assertion. It fails the
// test rather than returning "" if the shape it assumes is not there,
// because an extractor that silently returns nothing turns every
// assertion over its output into a vacuous pass.
func shellFuncBody(t *testing.T, script, name string) string {
	t.Helper()
	open := name + "() {\n"
	i := strings.Index(script, open)
	if i < 0 {
		t.Fatalf("install.sh has no %s() function; this guard is measuring nothing", name)
	}
	rest := script[i+len(open):]
	j := strings.Index(rest, "\n}\n")
	if j < 0 {
		t.Fatalf("%s() has no closing brace at column 0; the extractor's assumption no longer holds", name)
	}
	body := rest[:j]
	if strings.TrimSpace(body) == "" {
		t.Fatalf("%s() extracted as empty", name)
	}
	return body
}

// lastStatement is the final non-blank, non-comment line of a shell body,
// trimmed. It is what runs when every conditional above it declined.
func lastStatement(body string) string {
	lines := strings.Split(body, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		s := strings.TrimSpace(lines[i])
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		return s
	}
	return ""
}
