# C2 — Actuate: drain/resume, wake, and the rendered front

Status: PLANNED (2026-08-02). Scope: ~450 lines — two Connect RPCs +
proto, `vibe cell drain|resume|wake`, advisory leases, and
`vibe router render --cell front` (topology.md §3 build item 1).

## Goal

Fleet state changes become verbs instead of YAML edits: reclaim the
GPU box with one command (and get it back deterministically), wake a
sleeping box explicitly, and make the front's peer list a *rendered
artifact* — the hand-maintained peer YAML retires.

## Design

### 1. Proto: `CellDrain` / `CellResume`

`proto/vibe/v1/control.proto` gains:

```proto
rpc CellDrain(CellDrainRequest) returns (CellDrainResponse) {}
rpc CellResume(CellResumeRequest) returns (CellResumeResponse) {}
// CellDrainRequest: reason, eta (both optional strings)
// CellDrainResponse: report — in-flight request count at drain time,
//   resident models, active leases (see §3) — the "pre-drain report"
```

Regenerate with `buf generate`. These act on **the daemon's own local
cell** — remote reach comes from calling a *remote* daemon via the
existing vibeclient machinery, not from new routing.

**Token topology (decide here, not in code review):** every daemon
generates its own bearer token; the shipped client is single-target
(`$VIBE_API`/`$VIBE_TOKEN` or the local token file). Multi-cell reach
therefore needs per-cell credentials: `cells.<name>.token_file` in
`hosts.yaml` (a *path* — values never enter a repo). Resolution order
for both the CLI and fleetd when addressing cell X:
`$VIBE_TOKEN` (explicit override) → `cells.X.token_file` → local
token file (correct only when X is the local box). Distributing the
token files to the operator box / fleetd container is private-repo
provisioning.

**The `fleet:` daemon-config block starts here** (C3's announce loop
reuses it unchanged):

```yaml
fleet:
  cell: gpu-cell                        # this box's cell name
  registry_url: "http://front.lan:9001" # fleetd
  token_file: "~/.config/vibe/tokens/fleetd"
```

A cell daemon uses it to POST intent (drain/resume side effects) and,
from C3, to announce.

### 2. Daemon: `cell_cmds` config + drain semantics

Daemon config gains:

```yaml
cell_cmds:
  drain:  "systemctl --user stop llama-swap"
  resume: "systemctl --user start llama-swap"
```

The per-box process regime stays; the **verb** unifies (friction pain
1's honest v1 answer). Semantics:

- `CellDrain`: gather the pre-drain report — resident models and
  states from local llama-swap `/running`, leases fetched from fleetd
  (`fleet.registry_url`, when configured), in-flight counts from
  llama-swap `/api/metrics` **when that endpoint answers; the field
  is optional** (`/running` carries no request counts — do not
  invent them). Then run `cell_cmds.drain`. Draining via unit stop
  lets llama-swap run its documented drain (WaitGroup over in-flight
  → `cmdStop` → 5s grace → kill, router-lifecycle.md §2) — so stop
  the unit and let it drain; never SIGKILL the unit yourself. The
  cell unit's stop timeout (`TimeoutStopSec` / launchd
  `ExitTimeOut`) **must exceed the longest expected generation**;
  that value is private-repo config, the requirement is stated here.
  Whether SIGTERM-to-llama-swap waits for in-flight HTTP requests on
  the pinned version is an assumption drain gate 2 exists to verify.
  **Do not implement drain as unload-all**: an unloaded model
  JIT-reloads on the next stray request, which is exactly wrong
  mid-game. (Unload-all only becomes a valid drain mode once the
  front render is intent-aware — C3.)
- `CellResume`: run `cell_cmds.resume`, then clear intent. Models
  return by JIT on next request — resume does not preload.
- **Errors:** `cell_cmds` unset → Connect `FailedPrecondition`
  ("this daemon has no cell verbs configured"). Commands run via
  `sh -c` with a 60s timeout; non-zero exit or timeout → Connect
  `Unavailable` carrying captured stderr, and **no intent write**.
  Intent writer ownership: fleetd-invoked drains write intent at
  fleetd after the RPC succeeds; locally-invoked drains write it
  from the cell daemon, best-effort. Never both.

### 3. Advisory leases (consumer safety)

fleetd's intent store grows a sibling: advisory leases, keyed by
`(cell, model, holder)` with last-write-wins per key.
`POST /api/fleet/lease` `{cell, model, holder, note, ttl}` (`ttl` a
Go duration string, e.g. `"2h"`; re-POST to refresh) and
`DELETE /api/fleet/lease` with the same three-field key in the body.
Storage: `$XDG_STATE_HOME/vibe/fleet/leases.json`. Leases are
**advisory only**: they appear in the pre-drain report
("batch-ingest holds embed-large: mid-batch, 2.1M rows left") and in
`fleet_status`; they never block anything. A batch consumer that
takes a lease before a long run turns "did I just strand a 19-hour
job?" into a visible answer at drain time. TTL-expired leases vanish
(a crashed consumer must not haunt the fleet).

### 4. CLI: `vibe cell drain | resume | wake`

- `vibe cell drain [cell] [--reason X] [--eta Y]` — local daemon if no
  cell named, else that cell's `daemon_url` from `hosts.yaml`. Prints
  the pre-drain report and asks for confirmation when leases are
  active (`--yes` to skip).
- `vibe cell drain --until-exit -- <command>` — drain, exec the
  command, resume when it exits (deterministic GPU reclaim for a
  gaming session; the rejected alternative — GPU-idle polling — must
  not be built).
- `vibe cell resume [cell]`.
- `vibe cell wake <cell>` — WoL magic packet to `wake.mac` from
  `hosts.yaml` (`cells.<name>.wake: {mac: "..", broadcast: ".."}`),
  sent by fleetd (`POST /api/fleet/wake`) so it originates on the LAN
  segment. Always explicit; never triggered by a request. Validate
  L2 broadcast works from fleetd's network position (macvlan) — if
  not, fleetd shells to `cells.<name>.wake.cmd` as the per-cell
  fallback (per-cell because the broadcast workaround is
  per-target).

### 5. MCP tools

Add `drain_cell(cell, reason?, eta?)`, `resume_cell(cell)`,
`wake_cell(cell)`, `render_front(dry_run?)` to `fleetmcp`, thin
wrappers over the same paths the CLI uses. `drain_cell` returns the
pre-drain report so the agent can relay "heads up: canon holds a
lease" before confirming.

### 6. `vibe router render --cell front`

Extends the existing renderer (`internal/vibe/router/render.go`,
`vibe router render` in `cmd_router.go` with `--check/--stdout/--extras`
— all verified present):

- Backend defs gain an optional `cell: <name>` field (schema +
  validation in `internal/vibe/profile/backend_def.go`; unknown cell
  names are a validation error against `hosts.yaml`).
- `--cell front` renders a **peers-only** config: every def with a
  `cell:` whose cell is in `hosts.yaml` becomes an entry in that
  cell's `peers:` stanza (peer URL from `hosts.yaml`; `models:` list
  = def name **plus resolved aliases**, reusing the existing
  alias-winner logic — peers have no alias mechanism of their own,
  so an alias omitted here is an alias clients lose), plus
  `cloud_peer` defs exactly as the renderer emits them today. No
  local `models:` on the front.
- `--cell <gpu-cell>` renders that cell's local defs only.
- **Defaults, precisely:** bare `vibe router render` (no `--cell`)
  defaults `--cell` to the daemon config's `fleet.cell` when set and
  keeps writing the local `llamaSwapConfigPath()` — existing boxes
  keep their exact current behavior. When `fleet.cell` is unset and
  any def carries `cell:`, cell-annotated defs are **excluded with a
  warning** (never silently rendered into a local config). With an
  explicit `--cell` naming a *non-local* cell (the front case),
  `--out <path>` or `--stdout` is **required** — error otherwise;
  the local config path must never be overwritten with another
  cell's render.
- `--check` stays the drift gate; `--extras` still merges verbatim.
- The private repo points `--out` at the front's watched config
  directory; C0's `-watch-config` makes apply restart-free. The MCP
  `render_front` tool is **dry-run-only in C2** (returns the diff);
  the apply path from containerized fleetd arrives with C3's
  presence-driven render, which defines the mount contract.

## Acceptance gates

1. **Render-parity gate (do this first):** `vibe router render --cell
   front --check` against a `hosts.yaml` + defs describing the current
   live fleet reproduces the hand-maintained front config
   *semantically* (same peer set, same model **and alias** union per
   peer, same cloud peers — allow ordering/comment differences). Only
   after parity does authorship flip to the renderer.
2. **Drain gate:** with a long streaming request in flight to the
   target cell, `vibe cell drain` lets the stream complete, then
   requests for that cell's ids fail with the gateway-class errors
   consumers classify as `UPSTREAM_DOWN` (vamp's routererr shim);
   intent visible in `vibe cell status`; `resume` restores JIT
   service (one request round-trips) and clears intent. This gate is
   also the verification of the unit-stop-drains assumption (§2).
3. **`--until-exit` gate:** wrap a dummy long-running process; drain
   happens at start, resume fires on exit (including non-zero exit).
4. **Lease gate:** with an active lease, `drain` without `--yes`
   prints the lease and prompts; TTL expiry removes it.
5. Proto regeneration is clean (`buf generate` produces no drift
   beyond the new RPCs); full inner loop green.
6. Unit tests: render `--cell` assignment matrix (def with/without
   cell, unknown cell → validation error, non-local `--cell` without
   `--out`/`--stdout` → error, `fleet.cell` default), drain/resume
   RPC with a stubbed `cell_cmds` (assert order: report → cmd →
   intent write; missing `cell_cmds` → `FailedPrecondition`;
   non-zero exit → `Unavailable` + stderr + no intent write).

## Out of scope

Announce/heartbeats (C3), presence-derived rendering (C3 — this
phase's render is config-derived), warm targets (C4), any web page.
