# front — the fleet front (infra)

The always-on llama-swap front from `docs/design/topology.md`: the stable
`:9000` every client in the fleet — chat apps, coding harnesses, vamp —
points at, forever. Peers-only config: it owns no models and no GPU (the
`:cpu` image is a pure Go proxy); the GPU cells it federates own
JIT/TTL/swap. Cells appearing or disappearing changes the catalog, never
this address.

Run it on the box that is never off. This stack is infrastructure;
applications that consume it (chat stacks, self-hosted apps, dashboards)
live in their own stacks and treat the published `:9000` like any other
client would.

That same host is also the natural home for the fleet's **model
library** — a bulk share that downloads happen into once. Cells do NOT
load from it directly (a 26 GB LAN read per JIT load would wreck
cold-start times); `vibe model ensure` (fleet.md P4) rsyncs cell copies
onto local NVMe with the share as the preferred source.

This is a **reference stack**: it runs as-is once `.env` is filled, and
the values are meant to be replaced rather than inherited.

## Files

- `docker-compose.yaml` — the single-service stack. The example
  networking gives the front its own LAN IP on an external macvlan
  network, so it is a real host on the network with no port NAT.
- `front-config.example.yaml` — the peers-only llama-swap config. Copy
  it into the directory you set as `FRONT_CONFIG_DIR` as `config.yaml`
  and fill in real cell addresses and model lists — the bring-up seed
  only; once fleetd runs with `fleet.front_config` pointing here, it
  owns this file. The whole directory is mounted (not the file) so
  `-watch-config` sees atomic-rename (tmp+rename) writes — a re-render
  reloads the catalog with no container restart and no outage.
- `.env.example` — copy to `.env`; every `REPLACE-` marker is required.
  `FRONT_IMAGE` arrives already digest-pinned — see below.

## Bring-up order

1. Copy and edit the front config; create the `.env`.
2. `docker compose up -d`. Sanity check:
   `curl http://<FRONT_IPV4>:9000/v1/models` should list every peer's
   models. **Run it from another machine** — with macvlan, the Docker
   host itself cannot reach the container's IP.
3. On each cell, make its llama-swap listen on the LAN
   (`-listen 0.0.0.0:9000`) rather than loopback, then re-render and
   restart it (`vibe router render && systemctl --user restart
   llama-swap`).
4. Repoint clients at `http://<FRONT_IPV4>:9000/v1`.

If a cloud peer is enabled on the front, remove the equivalent def from
any cell that also had one — otherwise the same model ids appear twice
in the catalog.

Keep `:9000` on the LAN/VPN unless there is a real need to expose it;
the API surface has no auth of its own. If a peer's network is not
otherwise gated, give the peer an `apiKey`.

## Operating levers

- **Evict a model from a cell:**
  `curl -X POST http://<cell>:9000/api/models/unload/<model-id>` — or
  the unload button in the cell's UI. The next request for it JIT-loads
  again; eviction is how you force a VRAM reshuffle without touching
  the unit.
- **Warm a model:** any request naming it — JIT *is* the start verb.
  A 1-token chat completion is the cheap way:
  `curl http://<front>:9000/v1/chat/completions -H 'Content-Type: application/json' \
  -d '{"model": "<id>", "max_tokens": 1, "messages": [{"role": "user", "content": "hi"}]}'`
- **Per-cell dashboard:** `http://<cell>:9000/ui` — live model states,
  load/unload buttons, logs, metrics. **Never expose `/ui` beyond the
  LAN**: verified on v239, `/health` answers without credentials even
  when `apiKeys` is set (and deployments like this reference stack run
  default-allow with no keys at all). External exposure goes through
  the reverse-proxy auth layer only.
- **Reload the front config:** fleetd re-renders and writes
  `config.yaml` in `FRONT_CONFIG_DIR` atomically; `-watch-config` picks
  it up in place — no restart, and the new catalog is live within
  seconds. Verified on v239: in-flight streams keep flowing on the old
  server while it drains, but the drain grace is a **hardcoded 30s** — a
  stream still running 30s after the reload is force-closed (clean EOF,
  never corrupted bytes). `docker restart` is only needed for flag or
  image changes.

## The image is digest-pinned, and moving the pin is a procedure

`FRONT_IMAGE` ships as `<repo>:<tag>@sha256:<digest>` in both
`docker-compose.yaml` and `.env.example`. The tag half is for humans; the
digest half is what docker resolves. That means `docker compose pull` on
this host **cannot** change which llama-swap the fleet runs.

That is not caution for its own sake. On 2026-08-05 the floating `:cpu`
tag was found serving v247 against a fleet gated on v239: v240+ replaced
the `/api/events` in-flight wire (`requests` array →
`{"operation":"upsert","request":…}` / `{"operation":"remove","id":…}`,
`requests` omitempty), vibe counted the absent array as **zero in
flight**, and a reported zero is what disarms `drain --wait`, C14's
suspend, C8's probe guard and both warm loops. The parser is fixed and
both wires are now gated (`internal/swaptest`), but the trigger — a
routine pull — is a discipline problem, not a code problem.

**To move the pin**, run the ritual rather than editing this file:

```
scripts/upgrade/ritual.sh preflight <version>   # resolve + report the candidate
scripts/upgrade/ritual.sh canary   <version>    # conformance + a real 4-cell fleet
scripts/upgrade/ritual.sh gate     <version>    # the six-client SSE cold-start gate
scripts/upgrade/ritual.sh pin      <version>    # print the .env line to paste
```

`scripts/upgrade/README.md` says what each step catches and which parts a
human still has to watch. `vibe fleet doctor` reports an unpinned
deployment as `front.image_pin`, and the llama-swap version each cell is
actually running as `versions.llama_swap` — declaration and observation,
because a pin that was never applied looks exactly like a pin that was.

## Notes

- The per-peer `models:` lists are DERIVED, not maintained: fleetd
  renders them from the backend defs plus the presence table and writes
  this file on every membership transition (C2 built the renderer, C3
  made presence its trigger). Adding a backend def on a cell is the
  whole edit — the front's catalog follows on the cell's next announce.
  Hand edits are an emergency tool only: the next render overwrites
  them, and `-watch-config` means even that costs no outage.
- The behavioural claims above (`-watch-config`'s 30s drain, `/health`
  answering unauthenticated, SIGTERM's stream handling) were verified on
  the pinned build. They are upstream *behaviour*, so they are only as
  durable as the pin and the ritual that moves it.
- Cold models "just work" through the front: it relays the owning cell's
  loading state to the client while the model JIT-loads (proven
  unbuffered through two hops, router-lifecycle §17).
