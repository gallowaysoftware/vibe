# riff — persistent chat/research (application)

riff is topology.md's UC1: a persistent Open WebUI + SearXNG, the
phone-installable replacement for the Claude app. It is an *application*
stack — a plain client of the hum front (`deploy/hum`), deployed and
upgraded independently of it. Model selection, lazy loading, and sharing
warm models with other clients all come from hum; riff just points at it.

## Files

- `docker-compose.yaml` — OWUI + SearXNG. Needs an `.env` with `HUM_URL`
  (the front's address, e.g. `http://192.168.1.10:9000`) and
  `WEBUI_SECRET_KEY` (`openssl rand -hex 32`).

## Bring-up

1. Create the `.env`; `compose up`.
2. Reverse-proxy host: `chat.<domain>` → `http://<host>:8091`, with the
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
