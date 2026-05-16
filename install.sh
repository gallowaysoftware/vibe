#!/bin/sh
# install.sh — one-shot installer for vibe + vamp.
#
# Usage:
#   curl -sSL https://raw.githubusercontent.com/gallowaysoftware/vibe/main/install.sh | sh
#
# Environment knobs:
#   VIBE_VERSION   Pin a specific tag (e.g. v1.2.3). Default: latest release.
#   INSTALL_DIR    Destination for the binaries. Default: $HOME/.local/bin.
#   VIBE_OS        Override detected OS (linux|darwin).
#   VIBE_ARCH      Override detected arch (amd64|arm64).
#   VIBE_REPO      Override the source repo (org/name). Default: gallowaysoftware/vibe.
#
# Flags:
#   --dry-run      Print the planned actions and exit; download nothing.
#   --force        Reinstall even if the target version is already present.
#   -h, --help     Show this message.
#
# Designed for plain POSIX /bin/sh — no bash-isms (no arrays, no [[ ]]).

set -eu
# Pipefail is bash/ksh; not POSIX. Probe it and turn it on opportunistically.
# shellcheck disable=SC3040
(set -o pipefail 2>/dev/null) && set -o pipefail || true

REPO="${VIBE_REPO:-gallowaysoftware/vibe}"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
VERSION="${VIBE_VERSION:-}"
DRY_RUN=0
FORCE=0

# -- argv parsing ----------------------------------------------------------
while [ "$#" -gt 0 ]; do
	case "$1" in
		--dry-run) DRY_RUN=1 ;;
		--force)   FORCE=1 ;;
		-h|--help)
			cat <<'EOF'
install.sh — one-shot installer for vibe + vamp.

Usage:
  curl -sSL https://raw.githubusercontent.com/gallowaysoftware/vibe/main/install.sh | sh

Environment knobs:
  VIBE_VERSION   Pin a specific tag (e.g. v1.2.3). Default: latest release.
  INSTALL_DIR    Destination for the binaries. Default: $HOME/.local/bin.
  VIBE_OS        Override detected OS (linux|darwin).
  VIBE_ARCH      Override detected arch (amd64|arm64).
  VIBE_REPO      Override the source repo (org/name). Default: gallowaysoftware/vibe.

Flags:
  --dry-run      Print the planned actions and exit; download nothing.
  --force        Reinstall even if the target version is already present.
  -h, --help     Show this message.
EOF
			exit 0
			;;
		*)
			printf 'install.sh: unknown argument: %s\n' "$1" >&2
			exit 2
			;;
	esac
	shift
done

# -- pretty-printing helpers ----------------------------------------------
info()  { printf '[install] %s\n' "$*"; }
warn()  { printf '[install] WARN: %s\n' "$*" >&2; }
fatal() { printf '[install] ERROR: %s\n' "$*" >&2; exit 1; }

# -- detect OS / arch ------------------------------------------------------
detect_os() {
	if [ -n "${VIBE_OS:-}" ]; then printf '%s\n' "$VIBE_OS"; return; fi
	uname_s=$(uname -s 2>/dev/null || printf 'unknown')
	case "$uname_s" in
		Linux)  printf 'linux'  ;;
		Darwin) printf 'darwin' ;;
		*) fatal "unsupported OS: $uname_s (only linux and darwin are released)" ;;
	esac
}

detect_arch() {
	if [ -n "${VIBE_ARCH:-}" ]; then printf '%s\n' "$VIBE_ARCH"; return; fi
	uname_m=$(uname -m 2>/dev/null || printf 'unknown')
	case "$uname_m" in
		x86_64|amd64)  printf 'amd64' ;;
		aarch64|arm64) printf 'arm64' ;;
		*) fatal "unsupported architecture: $uname_m (only amd64 and arm64 are released)" ;;
	esac
}

OS=$(detect_os)
ARCH=$(detect_arch)

# -- pick a downloader ----------------------------------------------------
DL=""
if command -v curl >/dev/null 2>&1; then
	DL="curl"
elif command -v wget >/dev/null 2>&1; then
	DL="wget"
else
	fatal "need either curl or wget on PATH"
fi

http_get() {
	# $1 = URL, stdout = body. Fails noisily.
	url=$1
	if [ "$DL" = "curl" ]; then
		curl --fail --silent --show-error --location "$url"
	else
		wget --quiet -O - "$url"
	fi
}

http_download() {
	# $1 = URL, $2 = destination path.
	url=$1; dest=$2
	if [ "$DL" = "curl" ]; then
		curl --fail --silent --show-error --location --output "$dest" "$url"
	else
		wget --quiet -O "$dest" "$url"
	fi
}

# -- resolve the version ---------------------------------------------------
if [ -z "$VERSION" ]; then
	info "resolving latest release from github.com/$REPO ..."
	api_url="https://api.github.com/repos/$REPO/releases/latest"
	# Pull the "tag_name" line and unwrap the value with sed; no jq dependency.
	VERSION=$(http_get "$api_url" \
		| sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
		| head -n 1)
	if [ -z "$VERSION" ]; then
		fatal "could not parse latest tag from $api_url (rate-limited? offline?)"
	fi
fi

# Strip a leading 'v' for the filename portion of the goreleaser tarball
# (goreleaser by default doesn't include the 'v' in archive names).
VERSION_NUM=$(printf '%s' "$VERSION" | sed 's/^v//')

ARCHIVE="vibe_${VERSION_NUM}_${OS}_${ARCH}.tar.gz"
URL="https://github.com/$REPO/releases/download/$VERSION/$ARCHIVE"
CHECKSUM_URL="https://github.com/$REPO/releases/download/$VERSION/checksums.txt"

info "version  : $VERSION"
info "platform : $OS/$ARCH"
info "archive  : $ARCHIVE"
info "url      : $URL"
info "install  : $INSTALL_DIR"

# -- idempotency check -----------------------------------------------------
if [ "$FORCE" -eq 0 ] && [ -x "$INSTALL_DIR/vibe" ]; then
	# `vibe --version` is set via cobra's built-in flag (see internal/buildinfo).
	# Failures are tolerated — older binaries may not support --version.
	existing=$("$INSTALL_DIR/vibe" --version 2>/dev/null | head -n 1 | awk '{print $1}' || true)
	if [ -n "$existing" ] && [ "$existing" = "$VERSION" ]; then
		info "vibe $VERSION is already installed at $INSTALL_DIR/vibe — skipping download (use --force to override)."
		[ "$DRY_RUN" -eq 1 ] && exit 0
		# Still run doctor so the user gets the diagnostic.
		exec "$INSTALL_DIR/vibe" doctor
	fi
fi

if [ "$DRY_RUN" -eq 1 ]; then
	info "dry-run: would download $URL"
	info "dry-run: would download $CHECKSUM_URL"
	info "dry-run: would extract vibe and vamp into $INSTALL_DIR"
	info "dry-run: would run $INSTALL_DIR/vibe doctor"
	exit 0
fi

# -- download + extract ---------------------------------------------------
mkdir -p "$INSTALL_DIR" || fatal "could not create $INSTALL_DIR"

tmpdir=$(mktemp -d 2>/dev/null || mktemp -d -t vibe-install)
[ -d "$tmpdir" ] || fatal "could not create temp directory"
trap 'rm -rf "$tmpdir"' EXIT INT TERM

archive_path="$tmpdir/$ARCHIVE"
checksum_path="$tmpdir/checksums.txt"

info "downloading $ARCHIVE ..."
http_download "$URL" "$archive_path" || fatal "failed to download $URL"

info "downloading checksums.txt ..."
if http_download "$CHECKSUM_URL" "$checksum_path"; then
	if command -v sha256sum >/dev/null 2>&1; then
		expected=$(grep " $ARCHIVE\$" "$checksum_path" | awk '{print $1}' || true)
		if [ -n "$expected" ]; then
			actual=$(sha256sum "$archive_path" | awk '{print $1}')
			if [ "$expected" != "$actual" ]; then
				fatal "sha256 mismatch for $ARCHIVE (expected $expected, got $actual)"
			fi
			info "sha256 verified."
		else
			warn "no checksum line for $ARCHIVE in checksums.txt — skipping verification."
		fi
	else
		warn "sha256sum not available — skipping checksum verification."
	fi
else
	warn "could not download checksums.txt — skipping checksum verification."
fi

info "extracting into $INSTALL_DIR ..."
extract_dir="$tmpdir/extract"
mkdir -p "$extract_dir"
tar -xzf "$archive_path" -C "$extract_dir" || fatal "tar extraction failed"

found_any=0
for bin in vibe vamp; do
	src=""
	# Goreleaser places binaries at the root of the archive; tolerate one
	# level of nesting in case the layout changes.
	if [ -f "$extract_dir/$bin" ]; then
		src="$extract_dir/$bin"
	else
		# shellcheck disable=SC2231
		for candidate in "$extract_dir"/*/"$bin"; do
			[ -f "$candidate" ] || continue
			src=$candidate
			break
		done
	fi
	if [ -z "$src" ]; then
		warn "$bin not found in archive (expected at top level of $ARCHIVE)"
		continue
	fi
	install -m 0755 "$src" "$INSTALL_DIR/$bin" 2>/dev/null \
		|| { cp "$src" "$INSTALL_DIR/$bin" && chmod 0755 "$INSTALL_DIR/$bin"; }
	info "installed $INSTALL_DIR/$bin"
	found_any=1
done

[ "$found_any" -eq 1 ] || fatal "no binaries were installed from $ARCHIVE"

# -- PATH hint -------------------------------------------------------------
case ":${PATH:-}:" in
	*:"$INSTALL_DIR":*) : ;;
	*)
		warn "$INSTALL_DIR is not on your PATH."
		warn "Add this to your shell profile:"
		warn "  export PATH=\"$INSTALL_DIR:\$PATH\""
		;;
esac

# -- run doctor ------------------------------------------------------------
info "running '$INSTALL_DIR/vibe doctor' so you can see what else to set up ..."
printf -- '----- vibe doctor -----\n'
# Use exec so the doctor's exit code is the script's exit code. doctor exits
# non-zero on FAIL; that's information the operator wants to surface.
exec "$INSTALL_DIR/vibe" doctor
