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
  it to the path you set as `FRONT_CONFIG` and fill in real cell
  addresses and model lists.
- `.env.example` — copy to `.env`; every `REPLACE-` marker is required.

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

## Notes

- The per-peer `models:` lists are explicit, so adding a backend def on
  a cell means adding its id here too. That duplication is why
  `vibe router render --cell front` (topology.md §3 item 1) is the first
  build item — once it lands, vibe renders and pushes this config and
  hand-editing stops.
- Cold models "just work" through the front: it relays the owning cell's
  loading state to the client while the model JIT-loads (proven
  unbuffered through two hops, router-lifecycle §17).
