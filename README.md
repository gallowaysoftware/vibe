# vibe

A task-oriented launcher for local AI inference. Think `docker compose` for local AI workflows: define a profile that bundles a model configuration with a frontend, and one command brings up everything for a task.

```
vibe start code        # llama-server + opencode wired up for coding
vibe start research    # different model, different context, different frontend
vibe ps                # what's running
vibe stop              # tear it all down
```

## Why

Today running local AI looks like:

1. Launch `llama-server` with the right flags (model, context, parallel, GPU layers, cache types, jinja, ...).
2. Launch the frontend separately and configure it to match.
3. Hope the frontend's context size agrees with what llama-server actually loaded.
4. Repeat all of the above whenever you switch tasks.

`vibe` collapses that into one command and a versioned YAML profile.

## Architecture

- **Profile**: the unit of configuration. A YAML file bundling a model spec, a frontend integration, and template variables that wire them together.
- **Daemon**: supervises `llama-server` (and, later, docker-compose stacks). Exposes a control plane over a Unix socket.
- **Proxy**: reverse-proxies frontends to the active llama-server so swapping models doesn't require reconfiguring the frontend.
- **CLI**: `start`, `stop`, `ps`, `logs`, `list`.

## Status

Phase 1 in progress: profile schema, llama-server supervision, proxy, CLI, opencode integration. Single-host, local-only.

Not yet:

- docker-compose frontends (Perplexica, Open WebUI)
- managed-binary frontends
- TUI dashboard
- VRAM enforcement
- Remote (LAN) access from a laptop

## License

MIT — see [LICENSE](LICENSE).
