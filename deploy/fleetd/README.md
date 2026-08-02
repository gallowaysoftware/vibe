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
# (multi-cell registry, intent store, /mcp facade).
disable_proxy: true
bind_all: true
fleet_registry: true
```

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

## Consuming it

- CLI: `vibe cell status`, `vibe cell await gpu-cell --up` — the fleetd
  address resolves `--api` → `$VIBE_API` → `hosts.yaml fleetd_url` →
  local daemon, with a labeled degraded fallback to direct cell probes.
- MCP: `POST http://<FLEETD_IPV4>:9001/mcp` (bearer) speaks
  initialize / tools-list / tools-call; tools are `fleet_status`,
  `warm_model`, `unload_model`. Registration for agent harnesses uses
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
