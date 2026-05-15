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
- **MCP definitions**: one YAML file per Model Context Protocol server, dropped into `~/.config/vibe/mcp/` (e.g. `datadog.yaml`, `jira.yaml`). Profiles compose them by listing names: `frontend.mcps: [datadog, jira]`. Vibe injects a top-level `mcp` map into the rendered frontend config. Secrets stay in env vars (the frontend resolves `${env:...}` references); profiles never name them inline.
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

## Multi-profile example

`vamp` is a sibling tool in this repo that drives `vibe` to run a
multi-stage pipeline where each stage can use a different profile (and
therefore a different model). See
[`examples/multi-profile-pipeline/`](examples/multi-profile-pipeline/)
for a runnable two-stage demo: a small 7B profile drafts an outline,
then `vibe` swaps to a larger profile to expand it. Both
`profiles/fast.example.yaml` and `profiles/code.example.yaml` are
referenced by that example.

## License

MIT — see [LICENSE](LICENSE).
