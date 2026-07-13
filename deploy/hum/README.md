# hum — the fleet front (infra)

hum is the always-on llama-swap front from `docs/design/topology.md`: the
stable `:9000` every client in the fleet — chat apps, coding harnesses,
toqui, vamp — points at, forever. Peers-only config: it owns no models and
no GPU (the `:cpu` image is a pure Go proxy); the GPU cells it federates
own JIT/TTL/swap. Cells appearing or disappearing changes the catalog,
never this address.

This stack is infrastructure. Applications that consume it (the riff chat
stack, toqui, the future fleet dashboard) live in their own stacks and
treat hum's published `:9000` like any other client would.

The same host should also carry the fleet's **model library** — a bulk
`models` share that downloads happen into once. Cells do NOT load from it
directly (a 26GB LAN read per JIT load would wreck cold-start times);
`vibe model ensure` (fleet.md P4) rsyncs cell copies onto local NVMe with
the share as the preferred source.

## Files

- `docker-compose.yaml` — the single-service stack. House networking
  pattern: the front sits on the `br0` macvlan with its own static LAN IP
  (`HUM_IPV4`, default 172.16.3.211) — a real host on the network, no
  port NAT. Needs an `.env` with `ANTHROPIC_API_KEY` (and `HUM_IPV4` to
  override the default).
- `front-config.example.yaml` — the peers-only llama-swap config; copy to
  `<appdata>/hum/front/config.yaml` and fill in real cell addresses/model
  lists.

## Bring-up order

1. Copy + edit the front config; create the `.env`.
2. `compose up`. Sanity: `curl http://<HUM_IPV4>:9000/v1/models` should
   list every peer model (claude ids immediately; cell ids once cells are
   reachable). Run it from another LAN box — macvlan isolation means the
   unraid host itself cannot reach this IP.
3. On localmodel, flip its llama-swap unit to `-listen 0.0.0.0:9000` and
   remove its `anthropic` peer from `~/.config/vibe/router-extras.yaml`
   (hum owns cloud peers now — otherwise claude ids appear twice), then
   `vibe router render && systemctl --user restart llama-swap`.
4. Repoint clients at `http://<HUM_IPV4>:9000/v1` as they come up for
   changes (riff via its `HUM_URL`; coding harnesses; toqui).

Keep `:9000` LAN/VPN-only (no NPM proxy host for it) unless a real need
appears; the API surface has no auth of its own.

## Notes

- The per-peer `models:` lists in the config are explicit, so adding a
  backend def on a cell means adding its id here too. That duplication is
  why `vibe router render --cell front` (topology.md §3 item 1) is the
  first build item — once it lands, vibe renders + pushes this config and
  hand-editing stops.
- Cold models "just work" through hum: it relays the owning cell's
  loading state to the client while the model JIT-loads (proven unbuffered
  through two hops, router-lifecycle §17).
