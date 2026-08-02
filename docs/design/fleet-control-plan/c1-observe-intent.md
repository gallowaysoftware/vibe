# C1 — Observe + intent: fleetd, the cells registry, `vibe cell`, MCP

Status: EXECUTED (2026-08-02). All five gates passed; fleetd is deployed
and serving in the reference fleet (compose instance on the front host,
private repo).

Gate results:

1. **Automated: PASS.** `fleetapi` covers multi-cell registry from
   config, the one-element default (route-absence + field-absence
   guards), intent CRUD + the full display-state table (incl. the
   host-down-but-cell-up proof-of-life row), host-probe vs cell-probe
   disagreement, intent/last-seen persistence across Server recreation,
   and the dual-shape catalog parse (below). Daemon integration covers
   the fleet_registry role (multi-cell state, intent + /mcp behind the
   bearer boundary) and its absence (no /mcp, no intent route — the
   regression gate).
2. **Manual: PASS.** A sighted-then-vanished test cell showed OFF/AWAY
   with its pre-vanish last_seen across three fleetd restarts; host up
   + cell down + no intent showed `DRAINED?`.
3. **await: PASS.** `vibe cell await test-cell --up` parked through a
   fleetd restart (connection-refused → retrying) and unblocked on the
   cell's return.
4. **MCP: PASS.** curl'd initialize / tools-list / tools-call against
   the live fleetd: `fleet_status` returned the derived table;
   `warm_model(qwen2.5-coder-7b)` JIT-loaded through the front (ready
   in 8s); `warm_model(bge-embed)` rejected with the typed embed-class
   error and fired no request; `unload_model` evicted (verified via
   `/running`). The token-visibility mitigations are live-proven too:
   "token CREATED (new)" WARN fired on first boot, and a tokenless curl
   showed up as `auth rejections: 1` in `vibe cell status`.
5. **Regression: PASS** (daemon tests above).

Implementation notes beyond the doc's letter:

- **`model_classes` in hosts.yaml** is the warm_model class source. The
  doc requires rejecting embed/rerank ids "with a typed error naming
  the class" but names no mechanism; backend defs have no reliable
  class marker (embed is an `extra_args` convention), so the class
  lives in config where membership lives.
- **Dual-shape catalog parse.** A vibe-daemon-proxy cell (the roaming
  laptop before C3) answers `/v1/models` with OpenAI `data[]` AND the
  Ollama passthrough `models[]`, and 404s `/running`. With no
  `/running` to merge, `data[]`'s default "stopped" is a lie;
  llama.cpp only advertises what it has loaded, so when `/running`
  fails and the Ollama shape is present, entries report `ready`.
  llama-swap cells are unaffected (their transient `/running` failures
  carry no Ollama shape).
- **fleetd deploy note:** the reference compose mounts config read-only
  at `/config/vibe` — the daemon's `EnsureDirs` then needs
  `profiles/`, `backends/`, `mcp/` pre-created in that dir (empty dirs,
  host side). Recorded in deploy/fleetd/README.md.

## Goal

One always-on place that answers "what is every cell doing, and why" —
queryable by humans (`vibe cell status`), scripts (`vibe cell await`),
and conversational agents (MCP `fleet_status`). Plus the intent store,
so "reclaimed for gaming, back at 23:00" finally has somewhere to live.

## Design

### 1. `hosts.yaml` gains a `cells:` section

New file (or section — fleet.md §4.1's `hosts:` inventory is a separate
pending design; do not implement it, just leave room) at
`$XDG_CONFIG_HOME/vibe/hosts.yaml`:

```yaml
fleetd_url: "http://front.lan:9001"   # where the CLI finds fleetd (see §4)
cells:
  front:      { url: "http://front.lan:9000",  class: always_on }
  gpu-cell:   { url: "http://gpu.lan:9000",    class: opportunistic,
                host_probe: "gpu.lan:22",
                daemon_url: "http://gpu.lan:9001",
                token_file: "~/.config/vibe/tokens/gpu-cell" }
  laptop:     { url: "http://laptop.lan:9000", class: roaming,
                host_probe: "laptop.lan:22" }
  heavy-cell: { url: "http://spark1.lan:9000", class: always_on }
  # utility-cell: { url: "http://utility.lan:9100", class: always_on }
```

(Cell names follow the established reference set —
`deploy/front/front-config.example.yaml` and topology.md §1: `front`,
`gpu-cell`, `heavy-cell`, `utility-cell`, plus `laptop` for the
roaming role.)

- `url` — the cell's llama-swap base (probed via `/running`,
  `/v1/models`, exactly as fleetapi does today).
- `class` — `always_on | opportunistic | roaming` (design doc §4);
  drives display/alarm semantics now, catalog policy in C3.
- `host_probe` — optional `host:port` for a plain TCP dial
  distinguishing "host up, cell down" from "host down".
- `daemon_url` — optional; that cell's vibe daemon control plane
  (used from C2 for remote drain/resume; harmless now).
- `token_file` — optional; path to that cell daemon's bearer token
  (every daemon generates its own — see C2 §1 for resolution order).
  The path is config; the token value never enters a repo.
- `fleetd_url` (top-level) — where CLI commands find fleetd.
- **The cell named `front` is required** when a `cells:` section
  exists (warm-path and render logic address it by name); fail
  loudly at load if it's missing.

Parsing lives in a small `internal/vibe/fleetcfg` package (path
resolution via `internal/vibe/paths` — note that package is *pure
path* helpers; actual config loading currently lives in
`internal/vibe/daemon`). Keep it dependency-free (yaml.v3 only) so
both the daemon and CLI can load it. This file is the **single source
of cell membership** — C2's front render reads the same file; do not
introduce a second list anywhere.

**Presence of `hosts.yaml` does not make a daemon fleetd.** The CLI
reads it on any box (for the degraded fallback and for `daemon_url`
lookups). A daemon only activates the multi-cell registry, intent
store, and MCP facade when its config sets `fleet_registry: true` —
explicit role, not file-sniffing.

### 2. fleetapi: cells-from-config + host probe + intent

`internal/vibe/fleetapi` today (verified): `New(cells []Cell,
historyPath string, daemonInfo func() DaemonInfo)`, one-element slice
constructed in `daemon.go` (`{Name: "front", URL: 127.0.0.1:ProxyPort}`),
`snapshotCell` probing `/running` + `/v1/models`, an SSE watcher with
cellUp/cellDown transitions, start-duration history. The registry is
additive by design.

Changes:

- `Cell` gains `Class` and `HostProbe` fields. When `hosts.yaml` has a
  `cells:` section, the daemon builds the slice from it; otherwise the
  current one-element default stands (zero behavior change for
  existing deployments — test this).
- `snapshotCell` adds the host-level TCP probe (~2s timeout) when
  `HostProbe` is set; the snapshot distinguishes
  `host_reachable`/`cell_reachable`.
- The watcher gains per-cell `last_seen` timestamps recorded on
  up/down transitions (today it keeps only a boolean up/down map),
  **persisted** to `$XDG_STATE_HOME/vibe/fleet/last-seen.json` beside
  the start-history — after a fleetd restart, a cell that was already
  off must still show its last sighting rather than "unknown".
  Included in `/api/fleet/state`.
- **Intent store**: `$XDG_STATE_HOME/vibe/fleet/intent.json` (the
  state-dir convention fleet.md already names), guarded by the
  server's existing mutex discipline. `POST /api/fleet/intent` body:
  `{"cell": "...", "state": "drained"|"serving", "reason": "...",
  "eta": "..."}` — `"serving"` deletes the entry for
  never-announced cells; from C3, for announcing cells it stores a
  resolvable resume REQUEST (dropped when the cell echoes serving
  newer — desired_intent could otherwise never carry a resume).
  Registered on the same mux via `Register`, behind the same auth as
  the rest of `:9001`. Malformed cell names (not in the registry) →
  400.
- `/api/fleet/state` output gains per-cell `class`, `intent`,
  `last_seen`, and the **derived display state** (design doc §4 table,
  computed at read time — SERVING / DRAINED / `DRAINED?` / OFF /
  OFF/AWAY / INCONSISTENT).

### 3. fleetd is a deployment, not a binary

No new program. A vibe daemon with no profiles, `disable_proxy: true`,
`bind_all: true`, `fleet_registry: true`, a bearer token, and a
`hosts.yaml` cells section IS fleetd. Add a short "fleetd" section to `deploy/` docs (reference
compose in `deploy/fleetd/` mirroring `deploy/front`'s
reference-stack conventions: macvlan example, `.env` contract, no real
values). The container needs the vibe binary image/artifact the
private repo already uses for cells.

**State contract (containers get recreated; write this into the
reference compose, not tribal memory):** the files that MUST survive
container recreation are the bearer token file, `intent.json`,
`leases.json` (C2), `last-seen.json`, and the start-history — one
volume mount covering the config and state dirs, marked required in
the compose. The daemon's `LoadOrCreateToken` silently mints a fresh
token when the file is missing — after an unmounted recreate, every
future announcer would 401 quietly. Mitigations, required in C1:
log **"token CREATED (new)"** vs "token loaded" distinctly at
startup, and surface rejected-auth counts as a fleetd status field
so a token mismatch is visible from `vibe cell status`, not buried
in cell-side logs.

### 4. CLI: `vibe cell status | await`

New `internal/vibe/cli/cmd_cell.go`:

- `vibe cell status` — GET `<fleetd>/api/fleet/state`, render the
  derived table (one row per cell: display state, resident models,
  intent reason/eta, last-seen; deep-link line to each cell's `/ui`).
  The fleetd address resolves: `--api` flag → `$VIBE_API` →
  `hosts.yaml fleetd_url` → local daemon (vibeclient's existing
  default). Falls back to probing cells directly from `hosts.yaml`
  when fleetd is unreachable (degraded mode, clearly labeled).
- `vibe cell await <cell> [--up|--down] [--timeout 0]` — poll
  `/api/fleet/state` (default 5s interval); exit 0 when the condition
  holds, non-zero on timeout. `--up` means **cell reachable** (its
  llama-swap answers `/running` or `/v1/models`); `--down` is the
  negation; default `--up`. Intent is deliberately not consulted
  (routing truth rule). This is the "run the overnight batch when the
  GPU box returns" primitive:
  `vibe cell await gpu-cell --up && ./overnight-batch.sh`.

### 5. MCP facade: `internal/vibe/fleetmcp`

Clone the wire pattern from `internal/vibe/search/mcp.go` (320 lines;
`handleMCP` / `dispatchRPC` / `mcpTools` / `callTool` — plain HTTP
JSON-RPC, no SDK). Mount at `/mcp` on the daemon mux behind the
existing bearer auth. C1 tools:

- `fleet_status()` → same JSON the CLI renders, plus start-ETA
  estimates from history.
- `warm_model(model)` → `POST /v1/chat/completions` with
  `max_tokens: 1` through the front (`cells["front"].url`) —
  fire-and-forget: return immediately with the ETA from history, do
  not block on model load. Non-chat-class models (embed/rerank) are
  rejected with a typed error naming the class — warming those means
  their pinned cell is misconfigured, not that a JIT poke is needed.
- `unload_model(cell, model)` → `POST {cell.url}/api/models/unload/{model}`.

Registration for agent harnesses uses the existing MCP-spec passthrough
(`$XDG_CONFIG_HOME/vibe/mcp/<name>.yaml`, `internal/vibe/mcp`) — a
reference spec in the docs, house values private.

## Acceptance gates

1. `go test ./internal/vibe/fleetapi/...` covers: multi-cell registry
   from config, one-element default preserved, intent CRUD + the six
   derived display states (table-driven), host-probe vs cell-probe
   disagreement.
2. Manual: with one cell powered off, `vibe cell status` shows
   OFF/AWAY with a plausible `last_seen`; with a cell's llama-swap
   stopped but host up and no intent recorded, shows `DRAINED?`.
3. `vibe cell await <cell> --up` parked in a shell unblocks within one
   poll interval of the cell returning.
4. MCP: `curl` the initialize / tools-list / tools-call sequence
   against `/mcp` (mirror the search MCP's test approach);
   `fleet_status` returns the derived table; `unload_model` against a
   test cell evicts (verify via `/running`).
5. A daemon with no `cells:` section behaves exactly as before
   (regression gate).

## Out of scope

Drain/resume (C2 — requires proto + per-cell config), any front-config
rendering, announce, any web page. **Never act on `DRAINED?`** — it is
a display state only.
