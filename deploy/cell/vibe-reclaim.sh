#!/bin/sh
# vibe-reclaim.sh — take this box for something else, and give it back.
#
# The declared reclaim path (fleet-control C24). It wraps ONE command in
# `vibe cell drain --until-exit`: the cell drains before the command
# starts and resumes when it exits — any exit, including a crash, a
# non-zero status, or a Ctrl-C that kills this wrapper.
#
#   Steam ▸ Properties ▸ Launch Options:
#       /usr/local/bin/vibe-reclaim.sh %command%
#   .desktop:
#       Exec=/usr/local/bin/vibe-reclaim.sh /usr/bin/blender %F
#   shell:
#       vibe-reclaim.sh ./long-render.sh
#
# It exists because the fleet's intent axis is only as good as its
# reach. A reclaim that bypasses the verb leaves the control plane
# guessing, and fifty guesses later nobody believes the column. This is
# the verb, placed where the reclaim actually happens: a launcher.
#
# Environment:
#   VIBE_RECLAIM_REASON   recorded as the drain reason (default: gaming)
#   VIBE_RECLAIM_ETA      recorded as the ETA, free text (default: none)
#   VIBE_BIN              path to the vibe binary (default: vibe on PATH)
#
# Exit status is the wrapped command's own — a launcher reads it.

set -eu

usage() {
	echo "usage: ${0##*/} <command> [args...]" >&2
	echo "       wraps the command in a declared drain/resume of this cell" >&2
	exit 64 # EX_USAGE
}

[ "$#" -gt 0 ] || usage

vibe_bin=${VIBE_BIN:-vibe}
reason=${VIBE_RECLAIM_REASON:-gaming}
eta=${VIBE_RECLAIM_ETA:-}

# `exec`, deliberately: the wrapper must not stay between the launcher
# and vibe. Steam and systemd both signal the process they started, and a
# forwarding shell in the middle is one more thing that can fail to
# forward — with `exec` the SIGINT/SIGTERM lands on the process that owns
# the deferred resume.
#
# --yes, also deliberately: a lease is ADVISORY (design §5), and a
# wrapper that can refuse to start the game because someone left a lease
# open is a wrapper that gets deleted from the launch options after the
# first Friday night. The pre-drain report still prints what was
# stranded, so the operator sees it in the launcher's log.
if [ -n "$eta" ]; then
	exec "$vibe_bin" cell drain --reason "$reason" --eta "$eta" --yes --until-exit -- "$@"
fi
exec "$vibe_bin" cell drain --reason "$reason" --yes --until-exit -- "$@"
