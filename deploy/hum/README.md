# hum — the always-on half of the fleet

hum is the persistent stack from `docs/design/topology.md`: the llama-swap
front (the stable `:9000` every client points at, forever), a persistent
Open WebUI (UC1 — the phone chat app), and SearXNG for web search. It runs
on whatever docker host in the fleet is never off; the GPU cells it
federates come and go, hum doesn't. A reverse proxy + SSO layer (NPM +
Authelia in the reference deployment) is assumed to already exist on the
same host.

Everything is an upstream image — there is nothing to build. If hum ever
grows a built component (the fleet-dashboard UI is the likely first), that
image can build from this repo's CI or live in a host-side repo; decide
when it exists.

## Files

- `docker-compose.yaml` — the three-service stack (plain compose; works
  with unraid's Docker Compose Manager, plain `docker compose`, anything).
  Needs an `.env` with `ANTHROPIC_API_KEY` and `WEBUI_SECRET_KEY`
  (`openssl rand -hex 32`).
- `front-config.example.yaml` — the front's peers-only llama-swap config;
  copy to `<appdata>/hum/front/config.yaml` and fill in real cell
  addresses/model lists.

## Bring-up order

1. Copy + edit the front config; create the `.env`.
2. `compose up` the stack. Sanity: `curl http://<hum-host>:9000/v1/models`
   should list every peer model (claude ids immediately; cell ids once
   cells are reachable).
3. On localmodel, flip its llama-swap unit to `-listen 0.0.0.0:9000` and
   remove its `anthropic` peer from `~/.config/vibe/router-extras.yaml`
   (hum owns cloud peers now — otherwise claude ids appear twice), then
   `vibe router render && systemctl --user restart llama-swap`.
4. Reverse-proxy host: `chat.<domain>` → `http://<hum-host>:8091`, with
   the Authelia forward-auth snippet **plus header stripping** (below).
5. Open `chat.<domain>` on the phone, log in via Authelia, browser menu →
   "Add to Home Screen" — OWUI is a PWA; that tile is the Claude-app
   replacement.
6. Repoint clients at `http://<hum-host>:9000/v1` (OWUI in this stack
   already is; coding harnesses and toqui as they come up for changes).

## Reverse-proxy advanced config for the OWUI host

Trusted-header SSO means whoever sends `Remote-Email` IS that user, so the
proxy must overwrite it from Authelia's response and never pass a
client-supplied value through (nginx/NPM syntax):

```nginx
# strip anything the client sent
proxy_set_header Remote-Email "";
proxy_set_header Remote-Name  "";

# Authelia forward-auth (standard authelia location block), then:
auth_request_set $email $upstream_http_remote_email;
auth_request_set $name  $upstream_http_remote_name;
proxy_set_header Remote-Email $email;
proxy_set_header Remote-Name  $name;
```

Keep `:9000` LAN/VPN-only (no public proxy host for it) unless a real need
appears; the API surface has no auth of its own.

## Notes

- The front image is `:cpu` — it is a pure Go proxy; peers do the GPU work.
- **The host's `models` share is the fleet's model library** — the
  source-of-truth weights store that downloads happen into, once. Cells do
  NOT load from it directly (a 26GB read over the LAN per JIT load would
  wreck cold-start times); `vibe model ensure` (fleet.md P4) rsyncs cell
  copies onto local NVMe, and the share is the preferred `--from` source.
- `ENABLE_PERSISTENT_CONFIG=False` makes the compose env the source of
  truth for OWUI settings (otherwise the DB masks env changes after first
  boot — the recurring gotcha). Per-user prefs still persist in the DB.
- Cold models "just work" from the phone: pick any catalog model; the
  front relays the owning cell's loading state while it JIT-loads.
- toqui (or any self-hosted app) runs from its own compose on the same
  host; point it at hum with `AI_PROVIDER=openai`,
  `OPENAI_BASE_URL=http://<hum-host>:9000/v1`, and map
  `OPENAI_MODEL_FAST/SMART/BEST` per topology.md UC3 (swap-safe tiers).
