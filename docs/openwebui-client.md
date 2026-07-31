# Running an Open WebUI client against a vibe front

Open WebUI is the most common way to put a human (and a phone) in front
of a vibe fleet: point it at the front's `:9000`, and its model dropdown
becomes the live catalog — cold models just work via JIT loading, and a
model on a down cell fails immediately and honestly.

This is a pattern doc, not a stack. The essentials fit in a handful of
environment variables, and the four gotchas below are the ones that cost
real debugging time.

## The wiring

```yaml
environment:
  OPENAI_API_BASE_URL: http://<front-host>:9000/v1
  OPENAI_API_KEY: anything-nonempty     # the front has no auth of its own
  WEBUI_SECRET_KEY: <openssl rand -hex 32>
  ENABLE_PERSISTENT_CONFIG: "False"
```

## Gotcha 1 — persistent config masks your compose env

Open WebUI's PersistentConfig layer copies settings into its database on
first boot and then *ignores* the environment on subsequent starts. You
edit compose, restart, and nothing changes.

`ENABLE_PERSISTENT_CONFIG: "False"` makes the environment authoritative
again. Per-user preferences still live in the DB; only the settings that
have env equivalents are governed by compose.

The corollary is easy to miss: with persistent config off, **anything
you click into the admin UI that has an env equivalent reverts on the
next recreate.** Tool servers, search settings, and model defaults have
to be declared in compose, not clicked. Workspace models, prompts, and
per-user settings are DB entities with no env equivalent, so those do
persist — set them in the UI.

## Gotcha 2 — pin the session secret

`WEBUI_SECRET_KEY` defaults to a value generated at startup, so every
container recreation invalidates existing sessions and logs everyone
out. Generate it once and set it explicitly.

## Gotcha 3 — trusted-header SSO is a forgery risk unless the proxy strips

If you front Open WebUI with an authenticating reverse proxy
(Authelia and friends), it can trust a header instead of running its own
login:

```yaml
WEBUI_AUTH_TRUSTED_EMAIL_HEADER: Remote-Email
WEBUI_AUTH_TRUSTED_NAME_HEADER: Remote-Name
```

Whoever sends `Remote-Email` **is** that user. The proxy must therefore
blank any client-supplied value before setting its own from the auth
response — otherwise anyone who can reach the proxy can log in as
anybody by adding a header:

```nginx
# strip whatever the client sent
proxy_set_header Remote-Email "";
proxy_set_header Remote-Name  "";

# ... your forward-auth block ..., then:
auth_request_set $email $upstream_http_remote_email;
auth_request_set $name  $upstream_http_remote_name;
proxy_set_header Remote-Email $email;
proxy_set_header Remote-Name  $name;
```

If the app is only reachable on a LAN/VPN, this matters less — but the
day it goes behind a public hostname, this block is what stands between
your chat history and the internet.

## Gotcha 4 — tool-loop clients need the tool-flavored model def

A chat that calls tools runs several sequential completions per turn.
Two rules follow from that:

- Use the catalog's tool-capable def (for Qwen-family models, the
  `-tools` variant that emits visible-content tool calls), not the base
  alias.
- Keep the tiers swap-safe. If a turn alternates between models that
  evict each other on one cell, every turn becomes a swap storm. Within
  a cell, map tiers to the same model or to a co-resident group.

## MCP tool servers

Current Open WebUI speaks MCP (Streamable HTTP) natively — no `mcpo`
shim. Because of gotcha 1, declare servers in the environment rather
than the admin UI:

```yaml
TOOL_SERVER_CONNECTIONS: >-
  [{"type":"mcp",
    "url":"http://<mcp-host>:<port>/mcp",
    "auth_type":"none",
    "info":{"id":"myserver","name":"My Server","description":"..."},
    "config":{"enable":true,"access_control":null}}]
```

Attach the resulting tools to a workspace model (a DB entity, so the
UI is the right place) and enable native function calling on it.

## Web search

SearXNG on an internal network, with the web loader bypassed — the
Playwright loader yields "no sources" in this configuration:

```yaml
ENABLE_WEB_SEARCH: "True"
WEB_SEARCH_ENGINE: searxng
SEARXNG_QUERY_URL: http://<searxng>:8080/search?q=<query>
BYPASS_WEB_SEARCH_WEB_LOADER: "True"
```

## Networking note

If the webui gets its own LAN IP via a macvlan network, remember that
macvlan containers cannot reach ports published on their own Docker
host — the front needs its own macvlan address too, and sanity checks
have to run from a different machine.
