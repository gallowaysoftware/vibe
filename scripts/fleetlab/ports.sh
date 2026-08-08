#!/usr/bin/env bash
# ports.sh — every port scripts/fleetlab binds, derived from ONE knob.
#
# Sourced by lab.sh and by gl.sh (and therefore by every gate rig), so
# there is exactly one port table in this directory. A rig that keeps its
# own copy points at whichever llama-swap used to be there — or, once a
# second lab instance exists, at ANOTHER AGENT'S llama-swap.
#
#   FLEETLAB_PORT_BASE   the base of this instance's 200-port block
#                        (default 9600 — today's values, exactly)
#
# Two windows follow from the base:
#
#   listen window     [base, base+199]        cells, host probes, fleetd
#   upstream window   [base-3620, +39]        the llama-server children
#
# The upstream window keeps its historical distance from the listen
# window so that the DEFAULT base reproduces the ports every existing
# gate script hardcodes (cells 9640-9643, probes 9651-9653, fleetd
# 9720-9724, upstreams 5980-6019). Move the base and both windows move
# together.
#
# THE COLLISION RULE. A base must be a multiple of 200. Two instances
# whose bases differ therefore differ by at least 200, which makes both
# windows disjoint — and that disjointness is what `lab.sh down`'s sweep
# is anchored on for the llama-server children, which carry no lab path
# on their command line. Concurrent agents: pick a base nobody else is
# using (10200, 10400, 10600 …), check it with `ss -ltn`, and never run
# on the default while somebody else might be.
#
# It also refuses a base whose windows would cover a port this box's
# PRODUCTION fleet or scripts/upgrade/ritual.sh owns. That guard is not
# decoration: `down` kills what its patterns match, and the production
# llama-swap on :9000 holds a resident model and real traffic.

LAB_PORT_BASE=${FLEETLAB_PORT_BASE:-9600}

# The block geometry. Every offset below is relative to LAB_PORT_BASE (or
# to LAB_UPSTREAM_BASE); nothing downstream may hardcode a port.
LAB_PORT_SPAN=200
LAB_UPSTREAM_SPAN=40
LAB_UPSTREAM_OFFSET=3620   # 9600 - 5980

lab_ports_die() { printf '\033[31m[fail]\033[0m fleetlab ports: %s\n' "$*" >&2; exit 2; }

# Reserved: "lo hi label". Checked against BOTH of this instance's
# windows. The first two entries are the production fleet on this box;
# the last two are scripts/upgrade/ritual.sh, which drives a lab instance
# itself and must not be able to pick a base that eats its own ports.
LAB_RESERVED_RANGES=(
  "9000 9001 the production llama-swap (:9000) and vibe daemon (:9001)"
  "5800 5809 the production llama-swap's upstream range"
  "9810 9819 scripts/upgrade/ritual.sh's own listeners"
  "6100 6139 scripts/upgrade/ritual.sh's own upstream range"
)

lab_ports_overlap() { # lo1 hi1 lo2 hi2 -> 0 if the ranges intersect
  local a=$1 b=$2 c=$3 d=$4
  (( a <= d && c <= b ))
}

lab_ports_check() {
  # `down`'s upstream sweep reads ps and filters with awk. Missing either
  # one does not fail — it reaps NOTHING, quietly, and leaves a previous
  # crash's llama-servers holding this instance's ports while every log
  # line says the lab came down cleanly. Refuse instead.
  local t
  for t in ps awk pgrep; do
    command -v "$t" >/dev/null 2>&1 ||
      lab_ports_die "$t is required — \`down\`'s sweep cannot identify this instance's processes without it"
  done

  [[ $LAB_PORT_BASE =~ ^[0-9]+$ ]] ||
    lab_ports_die "FLEETLAB_PORT_BASE must be a whole number (got '$LAB_PORT_BASE')"
  (( LAB_PORT_BASE % LAB_PORT_SPAN == 0 )) ||
    lab_ports_die "FLEETLAB_PORT_BASE must be a multiple of $LAB_PORT_SPAN (got $LAB_PORT_BASE) — that is what keeps two instances' windows disjoint"
  (( LAB_PORT_BASE - LAB_UPSTREAM_OFFSET >= 1024 )) ||
    lab_ports_die "FLEETLAB_PORT_BASE $LAB_PORT_BASE puts the upstream window below 1024"
  (( LAB_PORT_BASE + LAB_PORT_SPAN - 1 <= 65535 )) ||
    lab_ports_die "FLEETLAB_PORT_BASE $LAB_PORT_BASE puts the listen window above 65535"

  local r lo hi label
  for r in "${LAB_RESERVED_RANGES[@]}"; do
    read -r lo hi label <<<"$r"
    if lab_ports_overlap "$LAB_PORT_BASE" "$((LAB_PORT_BASE + LAB_PORT_SPAN - 1))" "$lo" "$hi"; then
      lab_ports_die "base $LAB_PORT_BASE listens on $LAB_PORT_BASE-$((LAB_PORT_BASE + LAB_PORT_SPAN - 1)), which covers $lo-$hi: $label"
    fi
    if lab_ports_overlap "$((LAB_PORT_BASE - LAB_UPSTREAM_OFFSET))" \
                         "$((LAB_PORT_BASE - LAB_UPSTREAM_OFFSET + LAB_UPSTREAM_SPAN - 1))" "$lo" "$hi"; then
      lab_ports_die "base $LAB_PORT_BASE puts upstreams on $((LAB_PORT_BASE - LAB_UPSTREAM_OFFSET))-$((LAB_PORT_BASE - LAB_UPSTREAM_OFFSET + LAB_UPSTREAM_SPAN - 1)), which covers $lo-$hi: $label"
    fi
  done
}

lab_ports_check

LAB_UPSTREAM_BASE=$((LAB_PORT_BASE - LAB_UPSTREAM_OFFSET))
LAB_PORT_LO=$LAB_PORT_BASE
LAB_PORT_HI=$((LAB_PORT_BASE + LAB_PORT_SPAN - 1))
LAB_UPSTREAM_LO=$LAB_UPSTREAM_BASE
LAB_UPSTREAM_HI=$((LAB_UPSTREAM_BASE + LAB_UPSTREAM_SPAN - 1))

# name:port:class:startPort:hostprobe — the one cell table.
CELL_LIST=(
  "front:$((LAB_PORT_BASE + 40)):always_on:$((LAB_UPSTREAM_BASE + 0)):"
  "alpha:$((LAB_PORT_BASE + 41)):always_on:$((LAB_UPSTREAM_BASE + 10)):$((LAB_PORT_BASE + 51))"
  "bravo:$((LAB_PORT_BASE + 42)):opportunistic:$((LAB_UPSTREAM_BASE + 20)):$((LAB_PORT_BASE + 52))"
  "charlie:$((LAB_PORT_BASE + 43)):roaming:$((LAB_UPSTREAM_BASE + 30)):$((LAB_PORT_BASE + 53))"
)

LAB_PROXY_PORT=$((LAB_PORT_BASE + 120))        # fleetd proxy_port (disabled)
LAB_FLEETD_PORT=$((LAB_PORT_BASE + 121))       # fleetd control plane
LAB_BRAVO_DAEMON_PORT=$((LAB_PORT_BASE + 123)) # bravo's own cell daemon
LAB_NOTIFY_PORT=$((LAB_PORT_BASE + 124))       # the C9 webhook sink

FLEETD_URL=http://127.0.0.1:$LAB_FLEETD_PORT
BRAVO_DAEMON_URL=http://127.0.0.1:$LAB_BRAVO_DAEMON_PORT

cell_field() { local want=$1 idx=$2 e; for e in "${CELL_LIST[@]}"; do
  IFS=: read -r n p c s h <<<"$e"; [[ $n == "$want" ]] || continue
  case $idx in n) echo "$n";; p) echo "$p";; c) echo "$c";; s) echo "$s";; h) echo "$h";; esac; return; done; }
cell_port()  { cell_field "$1" p; }
cell_class() { cell_field "$1" c; }
cell_sport() { cell_field "$1" s; }
cell_probe() { cell_field "$1" h; }
model_cells() { echo "alpha bravo charlie"; }
all_cells()   { echo "front alpha bravo charlie"; }
# bravo runs a FULL vibe cell daemon (in-daemon announce loop + cell_cmds,
# so C2 drain/resume and C14 suspend are exercisable); alpha and charlie run
# the slim `vibe fleet announce` loop. Both announcer modes in one fleet.
slim_cells()  { echo "alpha charlie"; }

# lab_upstream_pids LO HI — the pids of every llama-server whose --port is
# inside [LO, HI].
#
# This replaces a regex over a range constant shared by every instance
# (`--port (59[89][0-9]|60[01][0-9])`), which is what made a second lab's
# `down` entitled to kill the first's upstreams. The llama-server children
# carry no lab path on their command line — llama-swap builds their argv
# from the rendered config, and rewriting it would change what `vibe fleet
# doctor`'s parity check re-renders — so the derived window IS their
# identity, and lab_ports_check's multiple-of-200 rule is what makes the
# windows disjoint.
#
# `[l]lama-server` rather than `llama-server`: the awk program's own argv
# is in `ps` output, and a literal would match it.
lab_upstream_pids() {
  local lo=$1 hi=$2
  ps -eww -o pid=,args= 2>/dev/null | awk -v lo="$lo" -v hi="$hi" '
    /[l]lama-server/ {
      for (i = 2; i <= NF; i++) {
        p = ""
        if ($i == "--port" && i < NF) p = $(i + 1)
        else if ($i ~ /^--port=[0-9]+$/) { p = $i; sub(/^--port=/, "", p) }
        if (p ~ /^[0-9]+$/ && p + 0 >= lo && p + 0 <= hi) { print $1; break }
      }
    }'
}

# lab_ports_table — the derived table, one field per line. `lab.sh ports`
# prints it; the Go regression test reads it, which is how "the default
# base reproduces today's values exactly" is a checked claim rather than a
# comment.
lab_ports_table() {
  local e n p c s h
  echo "port_base $LAB_PORT_BASE"
  echo "upstream_base $LAB_UPSTREAM_BASE"
  echo "listen_window $LAB_PORT_LO-$LAB_PORT_HI"
  echo "upstream_window $LAB_UPSTREAM_LO-$LAB_UPSTREAM_HI"
  for e in "${CELL_LIST[@]}"; do
    IFS=: read -r n p c s h <<<"$e"
    echo "cell $n $p $c $s ${h:--}"
  done
  echo "proxy $LAB_PROXY_PORT"
  echo "fleetd $LAB_FLEETD_PORT"
  echo "bravo_daemon $LAB_BRAVO_DAEMON_PORT"
  echo "notify $LAB_NOTIFY_PORT"
}
