# TODO

## Installation path

Need a single command that gets a new user from "nothing" to "vibe start
works" without manual diagnostic. Likely covers:

- Binary install (script or `go install`)
- `hf` (huggingface_hub) presence check; instructions when missing
- `hf auth login` advisory for gated repos
- llama-server binary presence/version check
- First-time XDG dir creation + example profile drop-in
- Maybe a `vibe doctor` subcommand that runs all the above as diagnostics

## MCP composition for profiles

The frontend template currently bakes everything (including MCP servers and
their secrets) into the profile. Two problems with that:

1. MCP definitions are repeated across profiles even though the same MCP
   (Jira, Datadog, Linear, GitHub) is used in many.
2. MCP context isn't free; you want different MCP sets in different
   environments (code vs debug vs research) so you only pay for what you
   need.

Sketch: a separate top-level config (e.g. `$XDG_CONFIG_HOME/vibe/mcp/*.yaml`,
one file per MCP) defines each MCP's command/env/etc. Profiles list which
MCPs to include:

```yaml
# ~/.config/vibe/mcp/datadog.yaml
name: datadog
type: local
command: [npx, -y, "@us-all/datadog-mcp"]
environment:
  DD_APP_KEY: ${env:DD_APP_KEY}
  DD_API_KEY: ${env:DD_API_KEY}
```

```yaml
# profile (excerpt)
frontend:
  mcps: [datadog, jira]
  template:
    # ... vibe injects mcp.<name> blocks into the rendered config
```

Secrets stay in env or a dedicated secrets file; profiles never name them
inline. Composition + reuse without duplication.
