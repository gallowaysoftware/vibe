#!/bin/sh
# install_test.sh — plain-POSIX assertions for ../install.sh.
#
# Run from the repo root:
#   sh internal/install_test.sh
#
# We intentionally do NOT use bats; many dev machines and the GH runner
# don't have it. The install script's surface area is small, so a handful
# of `--dry-run` checks driven by environment-variable knobs is enough.
#
# Each test stubs the things the script would otherwise touch:
#   * uname    — via VIBE_OS / VIBE_ARCH env knobs the script honours.
#   * network  — via --dry-run, which short-circuits before any HTTP call.
#   * vibe bin — via INSTALL_DIR pointing at a temp dir we control.

set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
INSTALL_SH="$SCRIPT_DIR/install.sh"

[ -x "$INSTALL_SH" ] || { printf 'install.sh not executable at %s\n' "$INSTALL_SH" >&2; exit 1; }

PASS=0
FAIL=0
TMPROOT=$(mktemp -d -t vibe-install-test.XXXXXX)
trap 'rm -rf "$TMPROOT"' EXIT INT TERM

pass() { PASS=$((PASS + 1)); printf '  PASS %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n' "$1"; printf '       %s\n' "$2"; }

assert_contains() {
	# $1 = haystack, $2 = needle, $3 = test name
	case "$1" in
		*"$2"*) pass "$3" ;;
		*) fail "$3" "expected to contain: $2" ;;
	esac
}

assert_not_contains() {
	case "$1" in
		*"$2"*) fail "$3" "expected to NOT contain: $2" ;;
		*) pass "$3" ;;
	esac
}

# ---------------------------------------------------------------------------
# Test 1: --dry-run prints the planned URL/path without making network calls.
#         We pin VIBE_VERSION so the script doesn't hit the GitHub API.
# ---------------------------------------------------------------------------
printf '\n[test] dry-run on linux/amd64 with a pinned version\n'
out=$(
	VIBE_VERSION=v1.2.3 \
	VIBE_OS=linux \
	VIBE_ARCH=amd64 \
	INSTALL_DIR="$TMPROOT/bin1" \
	"$INSTALL_SH" --dry-run 2>&1
) || { fail "dry-run exit" "$out"; }

assert_contains "$out" "vibe_1.2.3_linux_amd64.tar.gz"          "linux/amd64 archive name"
assert_contains "$out" "releases/download/v1.2.3"                "linux/amd64 download URL"
assert_contains "$out" "$TMPROOT/bin1"                            "linux/amd64 install dir echoed"
assert_contains "$out" "would download"                           "linux/amd64 dry-run guard fires"
assert_not_contains "$out" "extracting into"                      "linux/amd64 dry-run skips real extract"

# ---------------------------------------------------------------------------
# Test 2: OS/arch detection — Darwin / arm64 produces the right URL.
# ---------------------------------------------------------------------------
printf '\n[test] dry-run on darwin/arm64\n'
out=$(
	VIBE_VERSION=v0.5.0 \
	VIBE_OS=darwin \
	VIBE_ARCH=arm64 \
	INSTALL_DIR="$TMPROOT/bin2" \
	"$INSTALL_SH" --dry-run 2>&1
) || { fail "darwin dry-run exit" "$out"; }

assert_contains "$out" "vibe_0.5.0_darwin_arm64.tar.gz"          "darwin/arm64 archive name"
assert_contains "$out" "https://github.com/gallowaysoftware/vibe/releases/download/v0.5.0/vibe_0.5.0_darwin_arm64.tar.gz" "darwin/arm64 full URL"

# ---------------------------------------------------------------------------
# Test 3: idempotency — when an existing vibe reports the target version,
#         the script skips the download. We fake `vibe` with a tiny shell
#         script that echoes the version.
#
#         The fake prints what a RELEASED binary prints: goreleaser injects
#         `{{ .Version }}` (the tag with its `v` stripped) into
#         internal/buildinfo, and buildinfo.String() appends " (commit, date)".
#         install.sh takes the first field of the first line and compares it
#         against VERSION_NUM. It used to compare against the tag, so this
#         check never fired for any real install — see
#         TestInstallScriptIdempotencyMatchesWhatAReleasedBinaryPrints, which
#         pins the same claim against a genuinely goreleaser-built binary.
# ---------------------------------------------------------------------------
printf '\n[test] idempotency: existing matching install is skipped\n'
mkdir -p "$TMPROOT/bin3"
cat >"$TMPROOT/bin3/vibe" <<'EOF'
#!/bin/sh
case "$1" in
	--version) printf '9.9.9 (abc1234, 2026-01-02)\n' ;;
	doctor)    printf 'fake-doctor-ok\n' ;;
	*)         exit 0 ;;
esac
EOF
chmod +x "$TMPROOT/bin3/vibe"

out=$(
	VIBE_VERSION=v9.9.9 \
	VIBE_OS=linux \
	VIBE_ARCH=amd64 \
	INSTALL_DIR="$TMPROOT/bin3" \
	"$INSTALL_SH" --dry-run 2>&1
) || { fail "idempotency dry-run exit" "$out"; }
# This used to be `|| true`, on the theory that the match path execs
# `vibe doctor`. It does not under --dry-run: the script exits 0 one line
# ABOVE the exec (install.sh's `[ "$DRY_RUN" -eq 1 ] && exit 0`). So the
# `|| true` masked nothing today and would have masked any regression that
# made this path exit non-zero tomorrow.

assert_contains "$out" "already installed"                       "idempotency banner printed"
assert_not_contains "$out" "would download"                      "idempotency suppresses download plan"

# ---------------------------------------------------------------------------
# Test 4: --force re-downloads even when the version matches.
# ---------------------------------------------------------------------------
printf '\n[test] --force overrides idempotency\n'
out=$(
	VIBE_VERSION=v9.9.9 \
	VIBE_OS=linux \
	VIBE_ARCH=amd64 \
	INSTALL_DIR="$TMPROOT/bin3" \
	"$INSTALL_SH" --dry-run --force 2>&1
) || { fail "force dry-run exit" "$out"; }

assert_contains "$out" "would download"                          "force ignores existing install"
assert_not_contains "$out" "already installed"                   "force suppresses idempotency banner"

# ---------------------------------------------------------------------------
# Test 5: unsupported arch errors clearly.
# ---------------------------------------------------------------------------
printf '\n[test] unsupported arch fails clearly\n'
if out=$(
	VIBE_VERSION=v1.0.0 \
	VIBE_OS=linux \
	VIBE_ARCH=riscv64 \
	INSTALL_DIR="$TMPROOT/bin4" \
	"$INSTALL_SH" --dry-run 2>&1
); then
	# VIBE_ARCH bypasses the uname-derived detection, so the script will
	# happily build a URL for "riscv64". That's fine — the supported-arch
	# check fires only on the uname path. Make sure the URL is constructed.
	assert_contains "$out" "vibe_1.0.0_linux_riscv64.tar.gz"     "VIBE_ARCH override is honoured verbatim"
else
	# The else branch used to call pass("VIBE_ARCH override didn't reject"),
	# which made this test unfailable: install.sh could honour the override,
	# reject the override, or fall over, and all three were green. It is one
	# assertion with two outcomes — detect_arch() returns $VIBE_ARCH verbatim
	# before any supported-arch check, so a non-zero exit here means that
	# stopped being true.
	fail "VIBE_ARCH override is honoured verbatim" "the script exited non-zero instead of building a riscv64 URL: $out"
fi

# Real uname-driven rejection: clear the override so detection runs.
real_arch=$(uname -m 2>/dev/null || true)
case "$real_arch" in
	x86_64|amd64|aarch64|arm64) : ;;
	*)
		out=$(
			VIBE_VERSION=v1.0.0 VIBE_OS=linux INSTALL_DIR="$TMPROOT/bin5" \
			"$INSTALL_SH" --dry-run 2>&1
		) && { fail "unsupported arch should exit non-zero" "$out"; } || {
			assert_contains "$out" "unsupported architecture" "uname-driven arch rejection"
		}
		;;
esac

# ---------------------------------------------------------------------------
printf '\n[summary] %d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ] || exit 1
