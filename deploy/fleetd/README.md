# fleetd — the fleet control plane (infra)

fleetd is the always-on control-plane deployment from
`docs/design/fleet-control.md`: the one place that answers "what is
every cell doing, and why" — for humans (`vibe cell status`), scripts
(`vibe cell await`), and conversational agents (MCP `fleet_status`).

**fleetd is a deployment, not a binary.** An ordinary vibe daemon with
no profiles, `disable_proxy: true`, `bind_all: true`,
`fleet_registry: true`, a bearer token, and a `hosts.yaml` cells section
IS fleetd. It is read-and-request-only: if it dies, inference is
unaffected and `vibe cell status` degrades to direct cell probes
(invariant 4).

## Files

- `Dockerfile` — alpine + the release-shaped `vibe` binary. Build the
  binary into this directory first (`deploy/fleetd/vibe` is gitignored):
  `CGO_ENABLED=0 go build -trimpath -o deploy/fleetd/vibe ./cmd/vibe`,
  then `docker build -t vibe-fleetd deploy/fleetd`.
- `docker-compose.yaml` — single-service stack, macvlan networking like
  `deploy/front`, plus the state contract (below).
- `.env.example` — copy to `.env`; every `REPLACE-` marker is required.

## Config dir (`FLEETD_CONFIG_DIR`)

The compose mounts this read-only at `/config/vibe`, so the daemon's
startup `EnsureDirs` cannot create its subdirectories — pre-create them
(empty) on the host once:

```sh
mkdir -p "$FLEETD_CONFIG_DIR"/{"profiles","backends","mcp"}
```

`config.yaml`:

```yaml
# fleetd serves no inference: the proxy stays off, the control plane
# binds the LAN behind its bearer token, and the fleet role activates
# (multi-cell registry, intent store, announce endpoint, /mcp facade,
# GET /ui/fleet).
disable_proxy: true
bind_all: true
fleet_registry: true

# C3: the front's rendered llama-swap config, as seen from THIS
# container. Requires an extra rw mount of the front's -watch-config
# directory (the reference compose above does not ship one — add it
# when you turn the render loop on). Only fleetd writes there: two
# writers to one watched file is what the atomic-write contract
# forbids.
fleet:
  front_config: /front-config/config.yaml

# C4: restore the cell's default model after the operator's swap goes
# request-idle. Keyed on activity, never on a timer.
warm_targets:
  - cell: gpu-cell
    model: default-chat
    restore_after_idle: 30m

# C4: cron-fired warms, evaluated in the container's TZ (below).
warm_schedule:
  - cron: "30 6 * * *"
    model: default-chat
```

**Timezone.** `warm_schedule` entries evaluate in the container's `TZ`,
and an alpine base has no tzdata — set `TZ` in `.env` AND keep tzdata in
the image (the reference `Dockerfile` installs it for exactly this).
A wrong zone is not silent: every schedule's resolved `next_fire` shows
in `fleet_status` and on the fleet page, which is how the C4 gate caught
a UTC container firing "06:30" at 23:30 local.

`hosts.yaml` (the single source of cell membership — `vibe cell`, the
MCP facade, and C2's front render all read this one file):

```yaml
fleetd_url: "http://<FLEETD_IPV4>:9001"
cells:
  front:      { url: "http://<front-host>:9000",  class: always_on }
  gpu-cell:   { url: "http://<gpu-host>:9000",    class: opportunistic,
                host_probe: "<gpu-host>:22",
                daemon_url: "http://<gpu-host>:9001",
                token_file: "~/.config/vibe/tokens/gpu-cell" }
  laptop:     { url: "http://<laptop-host>:9000", class: roaming,
                host_probe: "<laptop-host>:22" }
# Optional: pin non-chat model ids to their class so warm_model refuses
# to JIT-poke them (warming an embedder with a chat completion loads it
# for nothing).
# model_classes:
#   bge-embed: embed
```

- `class` — `always_on | opportunistic | roaming`: display/alarm
  semantics now, catalog policy in C3.
- `host_probe` — optional `host:port` TCP dial distinguishing "host up,
  cell down" (`DRAINED?`) from "host down" (`OFF/AWAY`).
- `daemon_url` / `token_file` — used from C2 for remote drain/resume;
  harmless before. The token FILE path is config; token values never
  enter a repo.
- The cell named `front` is required when `cells:` exists.

### Savings config (C7b, optional)

The savings screen (`/ui/fleet#savings`, `GET /api/fleet/savings`, the
`fleet_savings` MCP tool) prices the usage ledger. Without this block it
renders tokens and no money — which is the correct output for a fleet
that has declared no equivalence.

```yaml
cells:
  gpu-cell:
    url: "http://<gpu-host>:9000"
    class: opportunistic
    # Declared wattage only. nvidia_smi / ha_entity are named future
    # values and fail validation today — this repo ships no power
    # sampler. Idle and busy are billed separately.
    power: { source: declared, watts_idle: 100, watts_busy: 400 }
    # EXAMPLE NUMBER. The real one lives in the private fleet repo.
    # Convention for dual-use hardware: the upgrade delta over a
    # gaming-adequate card. capital_note is REQUIRED and renders beside
    # the payback bar; no capital_cost means no payback bar at all.
    capital_cost: 2100
    capital_note: "example: dual-use GPU, upgrade delta over a gaming-adequate card"

pricing:
  electricity_price_per_kwh: 0.15          # example rate
  models:
    # twin: the SAME open-weight model as a real host spells it. The
    # headline is the median across the hosts that serve it. Naming a
    # frontier model here instead moves the answer about 72x.
    qwen3.6-27b: { twin: "Qwen/Qwen3-Coder-30B-A3B-Instruct" }
    # counterfactual scales by the tier the work would really have run
    # at: interactive (default, 1.0) | batch (0.5) | free (0.0).
    nightly-sweeper: { twin: "Qwen/Qwen3-32B", counterfactual: batch }
    # priced_as: an exact price-table id, for cloud_peer ids whose
    # actual spend fleetd reconstructs from the front's activity log.
    claude-opus-5: { priced_as: "claude-opus-5" }
  # Optional second line. There is deliberately NO default: a frontier
  # comparable is a claim about work you would actually have paid for,
  # and it will not render without a written rationale.
  # frontier:
  #   model: "<a frontier model id>"
  #   rationale: "why this workload would really have gone there"
```

Refresh the vendored price table with `vibe fleet prices vendor` from a
checkout on a networked box (`vibe fleet prices show <model>` prints what
the table says today). The daemon never fetches anything: the page has to
load on a LAN with no internet.

## State contract

Containers get recreated; these files MUST survive — one rw mount for
the state dir, which the compose marks required:

| file | why it matters |
|---|---|
| `token` | A recreate over an unmounted state dir silently mints a FRESH bearer token and every client 401s. The startup log says **"token CREATED (new)"** vs "token loaded" distinctly, and rejected-auth counts surface in `/api/fleet/state` and `vibe cell status` — check there first when clients 401. |
| `intent.json` | Declared cell intent ("drained for gaming, eta 23:00"). Losing it turns a deliberate drain into a `DRAINED?` mystery. |
| `last-seen.json` | Absent cells' last sightings. |
| `start-history.json` | Cold-start ETAs for `warm_model` and the UI. |
| `leases.json` (C2) | Advisory consumer leases. |
| `usage.jsonl` (C7a) | The token ledger, append-only. Losing it loses the fleet's whole accounting history — cells announce CUMULATIVE totals, so a fresh ledger starts the running total over rather than back-filling. C7b's payback bars are computed from this file. |
| `front-cloud-usage.json` (C7b) | fleetd's cursor for the front's `cloud_peer` traffic. Losing it re-ingests whatever the front's activity log still holds, which double-counts that window's actual cloud spend. |

The **backends defs mount** (`/config/vibe/backends`) is not state, but
it is load-bearing the same way: fleetd renders the front config from
it, and an EMPTY mount would otherwise render a peerless config over a
good one. fleetd refuses that render with a loud error rather than
writing it — if `fleet_status` shows `front_renders` frozen and the log
says "refusing to render an empty front config", the defs mount is
wrong.

## Consuming it

- CLI: `vibe cell status`, `vibe cell await gpu-cell --up` — the fleetd
  address resolves `--api` → `$VIBE_API` → `hosts.yaml fleetd_url` →
  local daemon, with a labeled degraded fallback to direct cell probes.
- Page: `GET http://<FLEETD_IPV4>:9001/ui/fleet` — the derived-state
  table, live off the SSE stream, with drain/resume/wake/unload/warm
  buttons that all POST `/mcp`. One security-relevant fact: this exact
  path is the ONE bearer-exempt route (GET only, exact match), because a
  browser cannot 401-and-then-prompt. The page carries no fleet data —
  every byte of state still requires the token, which it stores in
  `localStorage`.
- MCP: `POST http://<FLEETD_IPV4>:9001/mcp` (bearer) speaks
  initialize / tools-list / tools-call. Tools: `fleet_status`,
  `warm_model`, `unload_model`, `drain_cell`, `resume_cell`,
  `wake_cell`, `render_front` (dry-run only — fleetd's presence-driven
  render loop owns the write path). `drain_cell`/`resume_cell` reach a
  cell through its `daemon_url` + `token_file`; without those they fall
  back to recording desired intent for the cell to pick up on its next
  announce. Registration for agent harnesses uses
  vibe's MCP-spec passthrough (`$XDG_CONFIG_HOME/vibe/mcp/<name>.yaml`),
  e.g. `fleet.yaml`:

  ```yaml
  type: http
  url: http://<FLEETD_IPV4>:9001/mcp
  headers:
    Authorization: Bearer <fleetd-token>
  ```

  (The spec is a pass-through map — shape it to what the harness
  expects. House token values live in the private repo.)

Keep `:9001` on the LAN/VPN like everything else on the control plane;
external exposure only behind the reverse-proxy auth layer. The bearer
token rides plaintext HTTP inside that perimeter (the house posture —
same as the per-cell daemons); do not point `fleetd_url` at anything
you would not hand the token to, and keep `hosts.yaml` itself
user-writable only: any process that can rewrite it can redirect where
`vibe cell` sends your token.

**The token is every cell's voice** (fleet-control §6): announce
authenticates the connection, never the cell name. A stolen token can
announce as any registered cell — fake SERVING for a dead box, prune a
roaming cell's catalog entries (fake `withdrawing`), or cancel pending
drains (forged newer echo, bounded by the ingest skew clamp). Same
blast radius as the token's other powers (drain/resume/unload via
MCP); distribute it like cell-root. Per-cell credentials are a
futures item.
