# C0 — Quick wins: hot reload, autostart, discoverability

Status: EXECUTED (2026-08-02), gates 1+3 passed, gate 2 waived to a user
maintenance window with enablement evidence.

Gate results (live, against the reference fleet's front container on
the digest-pinned v239 image):

- **Mid-stream reload gate: PASS with a characterized limit.**
  `-watch-config` on v239 is poll-based (2s interval, config-file path
  only). Atomic tmp+rename in the watched dir triggers exactly one
  reload; writing the tmp file itself triggers none. On reload the new
  config is live for new requests immediately (catalog updated in <2
  poll intervals, container restarts=0), while the old server drains
  in-flight streams under a **hardcoded 30s shutdown timeout**
  (`llama-swap.go`, `const shutdownTimeout`). A stream ending inside
  the grace completed uncorrupted (123 chunks + terminal timings +
  `[DONE]`, reload fired +3s in); a stream still running at +30s was
  force-closed — clean EOF, no corrupt bytes, no panic (the client
  sees a truncated stream, never garbage). Membership edits are rare,
  so this is strictly better than the status-quo `docker restart`
  (instant kill of ALL streams + multi-second catalog outage) and the
  change was adopted. Long-stream kill-at-30s is recorded here as the
  known residual; a drain-timeout knob is upstream-contribution
  territory, not fleet config.
- **Reloads compose**: a second rename landing during the first
  reload's drain window was applied cleanly right after.
- **Env macros survive reloads** (`${env.<CLOUD>_API_KEY}` peer kept
  resolving across every reload).
- **Auth-exemption verification** (runbook line): on v239 with
  `apiKeys` set, `/health` answers 200 WITHOUT a key (exempt by
  design); `/ui/` and `/v1/models` return 401 without one. The earlier
  "UI and /health are auth-exempt" note was half right — only
  `/health` is. LAN-only posture stands regardless.
- **Reboot gate: WAIVED** (user decision 2026-08-02; rebooting the
  front host takes down the house, and both other cells were in active
  use). Enablement verified instead: the gpu cell's `llama-swap.service`
  enabled + new `vibe-daemon.service` user unit installed and active
  (the daemon had been running unmanaged); the front container's
  `restart: unless-stopped` + compose-manager autostart verified; the
  laptop cell gained a `vibe-daemon` LaunchAgent, installed and
  bootstrapped (RunAtLoad + KeepAlive; bare `vibe daemon` — sleep is
  pmset's job on AC, profile activation stays manual per the roaming
  class). Post-change, the laptop served a completion through the
  front. The live reboot check runs at the next natural reboot of each
  box.
- **Runbook lines: landed** in `deploy/front/README.md`
  ("Operating levers": unload / warm / per-cell UI / reload).

## Goal

Three outcomes: (1) editing the front's peer config no longer causes a
fleet-wide catalog outage; (2) every cell survives a reboot without a
human typing commands; (3) the levers that already exist (unload API,
per-cell `/ui`) are documented where an operator will find them at
2 a.m.

## Deliverables

### 1. Hot config reload on the front (`deploy/front`)

Current state (verified): `deploy/front/docker-compose.yaml` runs
`ghcr.io/mostlygeek/llama-swap:cpu` with
`command: ["-config", "/app/config.yaml", "-listen", ":9000"]` and a
**single-file read-only bind mount** of the config. Reload today means
`docker restart` — a brief outage for the whole fleet catalog.

Change:

- Mount the config's **parent directory**, not the file. fsnotify
  watchers miss atomic-rename writes through single-file bind mounts
  (editors and renderers write `tmp` + `rename`); a directory mount
  sees them. E.g. `- ${FRONT_CONFIG_DIR}:/app/config:ro` with
  `-config /app/config/config.yaml`.
- Add `-watch-config` to `command`.
- Update `deploy/front/README.md` and the `.env` contract
  (`FRONT_CONFIG` → `FRONT_CONFIG_DIR`) accordingly.
- While in the compose: add `logging:` bounds (`max-size`/`max-file`)
  — the json-file default is unbounded and this container runs
  forever on the box that also stores the ever-growing model library.

**Do not merge on faith.** `-watch-config` is documented upstream but
unverified on the `:cpu` image tag this deploy floats on. Gate below.
(The floating tag is itself a known deviation from router-lifecycle.md
§12's version-pin policy for cells; consider adding a digest pin to
`deploy/front` while here, so the gate's result stays meaningful.)

### 2. Reboot autonomy for every cell (private-repo work, tracked here)

Every serving process on every cell must come back after an unattended
reboot, under the process regime that cell already uses:

- systemd cells: units are `enabled` (verify, don't assume).
- launchd cells: a LaunchAgent per serving process. The reference
  fleet's laptop needed one for its serving stack — the embedder had
  one, the model server did not; the asymmetry was an accident.
- The front stack: `restart: unless-stopped` (already present).

Reference plist/unit sketches belong in the private fleet repo next to
the instance values; this repo only records the requirement.

### 3. Runbook lines (`deploy/front/README.md`)

Add an "Operating levers" section:

- Evict a model from a cell: `POST http://<cell>:9000/api/models/unload/<model-id>`
  (or the button in `http://<cell>:9000/ui`). This existed all along;
  a field session couldn't find it — write it down.
- Warm a model: any 1-token request naming it (JIT is the start verb).
- Per-cell dashboard: `http://<cell>:9000/ui` — live model states,
  logs, metrics. **Never expose `/ui` beyond the LAN**: llama-swap's
  UI and `/health` are auth-exempt (upstream docs, unverified here —
  verify while writing this runbook line); external exposure goes
  through the reverse-proxy auth layer only.

### 4. One canonical def source

Adopt the convention that each box's `$XDG_CONFIG_HOME/vibe/backends/`
is a checkout (symlink or clone) of the same def set, so serving flags
exist in exactly one place. This kills the copy-flag-blocks-by-hand
drift class (friction pain 4's mechanism) with zero code. Record the
convention in the private repo's ops docs; C2's render work assumes it.

## Acceptance gates

1. **Mid-stream reload gate** (the load-bearing one): start a request
   against a model with a long cold start so a slow-start SSE stream
   is in flight *through the front*; while it streams, rewrite the
   front config (add/remove a peer model); assert (a) the in-flight
   stream completes uncorrupted, (b) the catalog change is visible in
   `GET /v1/models` within seconds, (c) the container did not restart.
   Also verify the atomic-write pattern C3 will use: a `tmp` file
   created in the watched directory then renamed over the config must
   trigger exactly one reload (and the tmp filename itself must not).
   If this fails on the pinned image: revert to the restart flow
   (status quo — the design degrades, it doesn't break) and record the
   failure in this doc's Status line.
2. **Reboot gate**: reboot each cell host with no human input; every
   serving process is back and the front routes to it (verify with one
   request per cell through `:9000`).
3. `deploy/front/README.md` contains the unload/warm/ui runbook lines.

## Out of scope

Any Go changes; any new service; touching cell llama-swap configs.
