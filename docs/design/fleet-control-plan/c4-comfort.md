# C4 — Comfort: warm targets, warm schedules, the fleet page

Status: PLANNED (2026-08-02). Scope: ~300 lines — the heavy cell's
default-model policy, scheduled warming (topology.md §3 item 5), and
the one-page web UI (topology.md §3 item 4).

## Goal

The heavy cell restores its wide-utility default model without fighting
the operator's swaps; models can be pre-warmed on a schedule; and the
fleet gets its single pane of glass — one static page over the
substrate built in C1–C3.

## Design

### 1. Warm targets (restore-after-idle)

fleetd config:

```yaml
warm_targets:
  - cell: heavy
    model: dsv4flash
    restore_after_idle: 30m
```

Semantics — **restore keys on the swapped-in model going request-idle,
never on a clock**:

- If the target model is resident: do nothing.
- If another model is resident (the operator swapped): watch that
  cell's activity (in-flight counts / last-request timestamps from
  presence + the cell's llama-swap metrics). Only when the resident
  model has served **no requests for `restore_after_idle`** does
  fleetd issue a warm command for the target (piggybacked or a 1-token
  request), which evicts the idle swap via the cell's normal matrix.
- Any request to the swapped-in model resets the idle window. The
  design-panel-rejected alternative (pin/keep-warm on a timer) re-warms
  the default *while the operator's chosen model is in use* and evicts
  it mid-session — do not build that, even as an option.
- The cell's own TTL still applies underneath; warm-target is fleetd
  policy layered on top, and it must tolerate the cell being drained
  or absent (skip silently; note in status).

### 2. Warm schedules

```yaml
warm_schedule:
  - cron: "30 6 * * *"
    model: dsv4flash
```

A minimal cron evaluator in fleetd (stdlib time math over the five
standard fields is acceptable; a well-known tiny cron parser dep is
not — stdlib first, and the need is minute-granularity at best) firing
`warm_model`. This moves scheduled warming out of loose host crontabs
into fleet config, where `--check`/git see it. Two rules:

- **Scheduled warms reuse the warm-target guard.** A warm through the
  front makes llama-swap's matrix *evict* — an unconditional 06:30
  warm landing mid-overnight-batch on the heavy cell starts exactly
  the eviction fight §1 exists to prevent. Skip (and note in status)
  when any model on the target cell is non-idle or holds an active
  lease. This is also the first mechanical consumer the lease store
  gets.
- **Timezone is declared, not inherited.** fleetd runs in a container
  that defaults to UTC; `"30 6 * * *"` evaluated in UTC warms at
  ~23:30 local. Set `TZ` in the `deploy/fleetd` `.env` contract, and
  have `fleet_status` print each schedule's resolved next-fire
  timestamp so a wrong zone is visible at a glance rather than at a
  cold 6:30 a.m.

### 3. The fleet page

One static HTML file, embedded via `embed.FS` (house convention),
served by fleetd at `GET /ui/fleet` (path chosen to avoid any
collision with llama-swap's `/ui` on cells):

- Renders the derived-state table live: EventSource on
  `/api/fleet/events`, initial fill from `/api/fleet/state`. Per cell:
  display state (SERVING / DRAINED + reason/eta / OFF/AWAY +
  last-seen / …), class badge, resident models with states, leases,
  fingerprint warnings.
- Thin action buttons — drain / resume / wake / unload / warm — POSTing
  to the same endpoints the MCP tools use. No new mutation surface:
  if a button needs an endpoint the MCP facade doesn't have, the
  facade is incomplete, fix that first.
- Deep links to each cell's llama-swap `/ui` (the model-level detail
  view stays there deliberately).
- Auth: the page and its API calls sit behind the daemon's bearer
  auth; the page prompts once and keeps the token in localStorage.
  LAN posture applies; external exposure only behind the house
  reverse-proxy auth (same rule as everything else on `:9001`).
- No framework, no build step, no external assets (it must load on a
  LAN with no internet). Target: one file, a few hundred lines.

## Acceptance gates

1. **Warm-target gate:** on a test cell, warm the non-default model,
   drive requests to it past the idle window's start (window resets),
   stop; after `restore_after_idle` of true idleness the default is
   resident again. Assert the default was *never* warmed while the
   swap was active.
2. **Schedule gate:** a `warm_schedule` entry fires within a minute of
   its cron time and the model is resident before the first organic
   request (verify via events history).
3. **Page gate:** with the page open, drain a cell from the CLI — the
   row updates via SSE without reload; press Resume on the page — the
   cell returns; all buttons round-trip through existing endpoints
   (verify no new server routes beyond static serving).
4. Unit tests: idle-window state machine (reset on request, skip on
   absent/drained cell), cron next-fire math (DST boundary included).

## Out of scope

Metrics dashboards (llama-swap's `/ui` + Prometheus endpoints already
exist), multi-user auth, anything mobile-app-shaped, the v2 throughput
probe (still reserved).
