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
the code refuses to create. Per-phase results are in each phase doc's
gate table; the harness is named there as `local multi-cell harness`.

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
- A browser (C12 L1's hallway test — a phone is convenient, not
  required).
- Wall-clock duration: 24 h for C8 L4's cap and C7a's soak, a week of
  real traffic for C7b's plausibility gate. Nothing physical blocks
  these; the harness can run them unattended.

## Adding a gate

Write it as a standalone script beside `lab.sh` that sources nothing
from it except `./lab.sh env`, drives the fleet through the CLI or the
HTTP API, and prints raw evidence rather than a verdict. The gate
transcripts from the 2026-08-05 run were all of that shape, and the
reason is C13's rule in miniature: a rig that prints PASS is a rig that
can print PASS while wrong. Print what happened; let the reader judge.

## Requirements

`go`, `jq`, `socat`, `python3`, a llama-swap v239+ binary, a llama-server
binary, and three small GGUFs (one chat + two embedding) named by
`CHAT_GGUF` / `BGE_LARGE` / `BGE_M3`. Everything else the script writes
itself.
