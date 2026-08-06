# fleetlab — a real multi-CELL fleet on one box

`lab.sh` stands up a genuine vibe fleet — four real llama-swap v239
processes, a real fleetd, both announcer shapes — entirely on localhost,
so the fleet-control live gates can be *run* instead of asserted.

Its reason to exist is a mistake the plan made for eleven phases. Every
phase doc from C5 to C14 marked its live gates "NOT RUN — needs the real
fleet", and that was mostly wrong: almost none of them need a *second
box*. They need a second **cell** — a distinct llama-swap with its own
config, its own class in `hosts.yaml` and its own announce loop. That is
a process, not a machine. The gates that genuinely need metal are a
short, nameable list (see below), and everything else was blocked on an
assumption rather than on hardware.

## What it builds

| cell | port | class | models | announcer |
|---|---|---|---|---|
| `front` | 9640 | `always_on` | peers-only (rendered) | none — it is fleetd's `-watch-config` render target |
| `alpha` | 9641 | `always_on` | `lab-chat` (chat), `lab-embed-a` (`fingerprint: strict`) | slim (`vibe fleet announce`) |
| `bravo` | 9642 | `opportunistic` | `lab-embed-b` | a full `vibe daemon` cell — `cell_cmds`, drain/resume/suspend, in-daemon announce |
| `charlie` | 9643 | `roaming` | `lab-embed-c` | slim |

Plus: a per-cell `host_probe` TCP listener (9651–9653) independent of the
llama-swap port, so "the box answers but is not announcing" is a real
state; a webhook sink on 9724 standing in for ntfy; a persistent
llama-swap activity store per cell (C7a §0's `store: {path:}`); and
`hosts.yaml` carrying three classes, `model_classes`, `power`,
`capital_cost` and `pricing`.

Three classes in one fleet is the point: C3's render policy, C9's alarm
column and C13's `roaming.announcer` check all branch on class, and a
one-class fleet cannot tell you whether the branch works.

```
FLEETLAB_DIR=/tmp/fleetlab ./lab.sh up      # build, render, start, wait healthy
                           ./lab.sh status  # processes + a fleetd state summary
                           ./lab.sh prove   # four end-to-end proofs, raw output
                           ./lab.sh env     # env for driving the vibe CLI by hand
                           ./lab.sh down    # idempotent, works after a crash
```

## Why it cannot touch production

Both halves matter, and both are load-bearing rather than tidy:

- **Isolation.** Every process gets `XDG_CONFIG_HOME`, `XDG_STATE_HOME`
  and `XDG_RUNTIME_DIR` under `$FLEETLAB_DIR`. A lab fleetd cannot read
  or write `~/.config/vibe` or `~/.local/state/vibe`, so it cannot see
  the real `hosts.yaml`, mint into the real token file, or drain a real
  cell. Ports are 9600–9799 and upstreams 5980–6019, clear of a
  production llama-swap on `:9000` with upstreams at 5800+.
- **`CUDA_VISIBLE_DEVICES=""` on every cell.** Not tidiness. With
  `--n-gpu-layers 0` llama.cpp still builds a CUDA backend and reserves
  scheduler buffers, which aborts inside `libggml-cuda` when the card is
  already full. Hiding the device makes the lab genuinely CPU-only —
  which is what makes it safe to run beside a box whose GPU is holding
  production models.

`sweep` (called by `down`) anchors every kill pattern on a lab-only path
or a lab-only upstream port range, so a crashed run cleans up after
itself without ever matching a production process.

## What it proved, and what it cannot

Run 2026-08-05 across three lab instances. It moved 40+ gates from
"NOT RUN (live)" to a watched result, and surfaced five product bugs
that no unit test had — the ledger's epoch double-count, the warm path's
missing model-class guard, the doctor's dirty-checkout inversion, the
guest allowlist's decoded-path comparison, and a documented fleet state
the code refuses to create. A second pass the same day
([C17](../../docs/design/fleet-control-plan/c17-gate-closure.md)) ran the
thirteen gates that had been recorded as "needs metal" but had simply
never been attempted, moved fourteen rows, and found one more product
defect (a cell's probe specs and announced fingerprints are frozen at
announcer start, so `-watch-config` hot reload does not rebind the C8
baseline key) plus one documented figure that was wrong by 3x. Per-phase
results are in each phase doc's gate table; the harness is named there as
`local multi-cell harness`.

Two limits to state every time it is used:

1. **CPU models are not GPU models.** A 7B q4 on eight threads loads in
   ~25 s and decodes at ~16 tok/s. Every *edge* the control plane cares
   about is real — ready transitions, inflight frames, idle windows,
   activity rows, TTL evictions — but nothing here exercises a 6–10
   minute cold start, VRAM pressure, an eviction that costs real money,
   or a `degraded` verdict caused by spill rather than by a deliberate
   `SIGSTOP` throttle. Where a gate's claim is about *magnitude*, the lab
   verifies the mechanism and the magnitude is still owed.
2. **One box is not a fleet.** Nothing here crosses a network. No SSH,
   no TLS, no WoL, no suspend/resume, no laptop that leaves the
   building, no clock skew between hosts.

### The gates that still need metal

- A real suspend/resume cycle and a wattmeter (C14 L1).
- A magic packet on a real NIC to a powered-off box, and the BIOS switch
  that arms it (C14 L2, L5; C13 L4's wake half).
- A laptop that physically leaves and rejoins the LAN (C3's ungraceful
  vanish against a real roaming box; the lab substitutes SIGKILL).
- A GPU under real VRAM pressure (C8 L2's spill-induced degradation, as
  opposed to an induced throttle; C10 13a's 6–10 minute cold start).
- ~~A browser (C12 L1's hallway test)~~ — closed 2026-08-05 by
  `marionette.py`, which drives a headless Firefox. A phone was always
  convenience; the remaining delta is screen size.
- Wall-clock duration: 24 h for C8 L4's scheduling half and C7a's soak, a
  week of real traffic for C7b's plausibility gate. Nothing physical
  blocks these; the harness can run them unattended.

## The gate scripts

One script per gate, beside `lab.sh`. Each sources `gl.sh` (the scratch
XDG triple + `state`/`mcp` helpers), drives a running lab, prints raw
evidence, and cleans up whatever config it changed. Bring the lab up
first (`FLEETLAB_DIR=/tmp/fleetlab ./lab.sh up`) and pass the same
`FLEETLAB_DIR`.

| script | gate | runtime |
|---|---|---|
| `gate-c7a-partials.sh` | C7a 2 + 5 — front-collected rows into the fold; a poke that is not one token; probe traffic as self-traffic | ~3 min |
| `gate-c8-l4.sh` | C8 L4 — the 96/day cap at its boundary (the 24 h window is seeded on the cell's state file; the script says so) | ~5 min |
| `gate-c8-l5.sh` | C8 L5 — embed probe baseline on a bge cell | ~25 min (5-minute cell-side cooldown) |
| `gate-c8-l5-staleflags.sh` | C8 L5's flag-change half — currently a **FAIL**; isolates the cause | ~15 min |
| `gate-c9-14a.sh` | C9 14a — a real ntfy topic accepts the payload (needs outbound network) | ~1 min |
| `gate-c9-14d.sh` | C9 14d — a def edited on the front, not the cell → drift alarm, once, then resolve | ~9 min |
| `gate-c10-13d.sh` | C10 13d — two shells, the lease handshake | ~4 min |
| `gate-c11-l2.sh` | C11 L2 — a hold is not a pin (drops a def's TTL to 45 s) | ~7 min |
| `gate-c11-l3.sh` | C11 L3 — a hold reaches the warm-schedule guard | ~7 min |
| `gate-c12-l3.sh` | C12 L3 — guest token rotation across a fleetd restart | ~1 min |
| `gate-c13-parity.sh` | C13's `defs.parity` — the dirty-and-diverged level, after #36 | ~3 min |
| `gate-c14-l3.sh` | C14 L3 — a real request defers the declared suspend, then it fires | ~12 min |
| `gate-c14-l4.sh` | C14 L4 — a lease defers the suspend until `max_defer` abandons it | ~10 min |
| `marionette.py` | C12 L1's DOM half — drives headless Firefox over Marionette | ~1 min |

`marionette.py` needs `firefox` on `$PATH`; nothing else here does.
**Five** of these change fleetd config and restart it (`gate-c9-14a.sh`,
`gate-c9-14d.sh`, `gate-c11-l3.sh`, `gate-c14-l3.sh`, `gate-c14-l4.sh`) —
they back the file up to the same `config.yaml.c17bak` and restore it, so
do not run two of them at once against the same lab, and do not start one
while a previous run's backup is still on disk.

## Adding a gate

Write it as a standalone script beside `lab.sh` that sources nothing
from it except `gl.sh` (or `./lab.sh env`), drives the fleet through the
CLI or the HTTP API, and prints raw evidence rather than a verdict. The
gate transcripts from the 2026-08-05 runs were all of that shape, and the
reason is C13's rule in miniature: a rig that prints PASS is a rig that
can print PASS while wrong. Print what happened; let the reader judge.

Four habits worth copying, learned the hard way on 2026-08-05 — the first
three the same way twice, because C17's own first cut shipped six rigs
that broke rule 1 (see its adversarial-review addendum).

1. **Read the field the answer is actually in.** `/api/fleet/usage`
   returns `buckets`, not `rows`; the warm block's key is `schedule`; an
   activity row nests `tokens`; the announced `flags_sha256` is on
   `.cells[].presence.models[]`, and `.cells[].models[]` has no such field;
   `.cells[]` has no `commands` — the piggyback queue rides the announce
   RESPONSE and is drained on delivery, so it is not readable from state at
   all. A wrong `jq` path prints `null` or an empty array, which reads
   exactly like the feature not firing. **Check every evidence expression
   against a state document you have actually looked at**, because a rig
   whose columns are structurally constant is worse than no rig: it prints
   a healthy-looking zero under a heading that promises a measurement.
2. **fleetd's log is `state/fleetd/vibe/daemon.log`.** `logs/fleetd.log`
   holds only pre-handler stdout, i.e. nothing. `gl.sh` exports `$DLOG`.
3. **A grep is not a control check.** `/warm/i` over `document.body.innerText`
   matches the footer's warm-target summary whether or not the warm row is
   rendered; test the element (`#warmrow`), and name the field after what
   it interrogates.
4. **Compute cron minutes in the FLEET timezone** (`fleet.timezone`), not
   the shell's, or the declared minute arrives an hour late and the gate
   looks broken.

If a rig re-renders a cell config, use `gl.sh`'s `render_cell` rather than
open-coding `router render` + the startPort `sed`: the renderer hardcodes
`startPort: 5800`, which is **production's** upstream range on this box, and
`render_cell` fails loudly if the rewrite did not apply. `lab.sh` has
always checked this; the first cut of the C17 rigs did not.

## Requirements

`go`, `jq`, `socat`, `python3`, a llama-swap v239+ binary, a llama-server
binary, and three small GGUFs (one chat + two embedding) named by
`CHAT_GGUF` / `BGE_LARGE` / `BGE_M3`. Everything else the script writes
itself. `firefox` for `marionette.py`.
