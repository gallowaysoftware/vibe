# riff — persistent chat/research (application)

riff is topology.md's UC1: a persistent Open WebUI + SearXNG, the
phone-installable replacement for the Claude app. It is an *application*
stack — a plain client of the hum front (`deploy/hum`), deployed and
upgraded independently of it. Model selection, lazy loading, and sharing
warm models with other clients all come from hum; riff just points at it.

## Files

- `docker-compose.yaml` — OWUI + SearXNG. House networking pattern: the
  webui sits on the `br0` macvlan with its own static LAN IP (`RIFF_IPV4`,
  default 172.16.3.212) for NPM to proxy to; SearXNG stays on an internal
  bridge with no LAN exposure. Needs an `.env` with `HUM_URL` (the front's
  macvlan address, e.g. `http://172.16.3.211:9000` — NOT the unraid host
  IP, which macvlan containers cannot reach), `WEBUI_SECRET_KEY`
  (`openssl rand -hex 32`), and optionally `RIFF_IPV4`/`RIFF_PUBLIC_URL`/
  `SUBKB_URL` (the subkb MCP host, default `http://172.16.3.214:8091`).

## Bring-up

1. Create the `.env`; `compose up`.
2. NPM proxy host: `chat.<domain>` → `http://<RIFF_IPV4>:8080`, with the
   Authelia forward-auth snippet **plus header stripping** (below).
3. Open `chat.<domain>` on the phone, log in via Authelia, browser menu →
   "Add to Home Screen" — OWUI is a PWA; that tile is the Claude-app
   replacement.
4. In a chat, the model dropdown is hum's live catalog. Picking a cold
   model just works (JIT + loading-state hold); a model on a down cell
   errors immediately and honestly — pick one that's up.

## Reverse-proxy advanced config

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

## Knowledge experts (subkb)

The compose env registers the subkb MCP server
(`gallowaysoftware/subreddit-knowledge-aggregator`, running at
`SUBKB_URL` on the same br0 macvlan) as a native MCP tool server via
`TOOL_SERVER_CONNECTIONS` — it must live in env, not the admin UI,
because persistent config is off (UI-added servers revert on recreate).

Each domain expert is then a **workspace model** (Workspace → Models),
which is DB state and does persist:

1. Base model: a `-tools` def from hum's catalog (`qwen3.6-27b-tools` —
   the tool-loop rule below), native function calling.
2. System prompt: declare the domain, pin the collection — "always call
   search_knowledge with collection=x4" — and set norms (cite permalinks,
   prefer newer version_tag results over older advice).
3. Tools: attach the subkb tools to the model.

One MCP server serves every expert; the collection pin is what
differentiates them. Curated canon (patch notes, manuals) goes into the
corpus with `kbctl add-doc` on the subkb side — NOT into OWUI's own
knowledge/RAG store, which would fork retrieval (different embedder, no
temporal supersession) and hide the docs from other MCP clients.

### Memory (recall)

Persistent per-expert memory is provided by the `recall` service
(github.com/gallowaysoftware/recall) — both an MCP tool server (add it to
`TOOL_SERVER_CONNECTIONS` alongside subkb) and an Open WebUI inlet filter
that injects a memory digest at chat start. See recall's
`integrations/openwebui/` for the filter and setup.

## Notes

- `ENABLE_PERSISTENT_CONFIG=False` makes the compose env the source of
  truth for OWUI settings (otherwise the DB masks env changes after first
  boot — the recurring gotcha). Per-user prefs still persist in the DB.
- Web search is SearXNG with the web-loader bypassed (the playwright
  loader's "no sources" bug — same fix as the localmodel chat profile).
- Tool-loop rule applies here as everywhere: if you wire MCP tools into
  this OWUI against a Qwen model, address the `-tools` def
  (`qwen3.6-27b-tools`), not the base alias.
- riff is deliberately NOT one of the vibe-profile distillery stacks —
  those are episodic tuned appliances on localmodel; riff is the
  always-on general instance with its own webui.db.
