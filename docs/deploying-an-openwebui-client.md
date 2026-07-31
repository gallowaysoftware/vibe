# Deploying an Open WebUI client against a vibe front

Lessons from running a persistent Open WebUI instance as a fleet client
(the reference stack lives in the operator's own infra repo; these are
the reusable rules).

## 1. Env is the source of truth — disable persistent config

Set `ENABLE_PERSISTENT_CONFIG=False` so compose env drives OWUI settings.
Otherwise the DB snapshot of first-boot config silently masks later env
changes — the recurring gotcha. Corollary: anything you'd normally click
into Admin Settings must instead be expressed as env, or it reverts on
the next recreate. Per-user prefs still persist in the DB.

## 2. Trusted-header SSO means the proxy MUST strip inbound headers

With `WEBUI_AUTH_TRUSTED_EMAIL_HEADER`, whoever sends that header IS that
user. The reverse proxy must overwrite it from the auth service's
response and never pass a client-supplied value through:

```nginx
proxy_set_header Remote-Email "";           # strip client value
auth_request_set $email $upstream_http_remote_email;
proxy_set_header Remote-Email $email;       # set from auth response
```

Direct LAN access to the container bypasses the proxy — that is the
trust model, not a bug; know it and scope the LAN accordingly.

## 3. Declare MCP tool servers in env, not the admin UI

Current OWUI resolves `type: "mcp"` (streamable HTTP) entries from
`TOOL_SERVER_CONNECTIONS` natively:

```yaml
TOOL_SERVER_CONNECTIONS: >-
  [{"type":"mcp","url":"http://<host>:<port>/mcp","auth_type":"none",
    "info":{"id":"<id>","name":"<name>","description":"..."},
    "config":{"enable":true,"access_control":null}}]
```

This is forced by rule 1 (UI-added tool servers revert on recreate) and
is better anyway: the tool wiring ships with the stack. Workspace
models, filter functions, and their per-model assignments are DB
entities — they persist and