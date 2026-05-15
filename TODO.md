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

## ~~MCP composition for profiles~~ (done)

Implemented: one YAML file per MCP under `$XDG_CONFIG_HOME/vibe/mcp/`,
referenced from a profile via `frontend.mcps: [...]`. See
`profiles/mcp.example.yaml` and the Architecture section of the README.
