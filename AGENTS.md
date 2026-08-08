# AGENTS.md

Operating notes for agents (Claude Code, Aider, Codex, Cursor, etc.) working
in this repo. The user-facing model lives in `README.md`; this file
captures the conventions and invariants an agent needs to make changes
that fit.

## Repo at a glance

Two binaries from one Go module (`github.com/gallowaysoftware/vibe`):

- **`vibe`** (`cmd/vibe`, `internal/vibe/`): task launcher. One YAML
  profile activates a backend (`llama_server` | `comfyui` | `http_server` |
  `tabby_api`) and an optional frontend (`external` | `docker-compose` |
  `managed`).
  The daemon owns a Connect/protobuf control plane on a unix socket
  plus optional `127.0.0.1:9001` (bearer-token-authed). The supervisor
  auto-respawns a backend that exits unexpectedly mid-life (up to 60
  restarts per 30 min) so a flaky CUDA kernel mid-foreach doesn't kill
  a long pipeline — see `internal/vibe/daemon/daemon.go:watchBackendForRespawn`.
- **`vamp`** (`cmd/vamp`, `internal/vamp/`): pipeline orchestrator that
  drives `vibe`. A YAML pipeline declares stages (`text`, `comfyui`,
  `audio`, `ffmpeg`, `youtube`, `webhook`, `confirm`, `render`,
  `compact`, `pandoc`, `mix`, `short`) with a DAG of inputs;
  capability → backend mapping (profile-name fallback) lives in
  `$XDG_CONFIG_HOME/vamp/capabilities.yaml`.

**`render` stage type.** Pure template → text without LLM invocation.
Does not activate a vibe profile. Use for deterministic data
transformation: enumerating directories, building JSON arrays, etc.
Validated in `pipeline.go:Validate`, executor in
`vision_executor.go:renderExecutor`.

Generated code: `proto/vibe/v1/control.pb.go` and
`proto/vibe/v1/vibev1connect/`. Regenerate with `buf generate` (see
`buf.gen.yaml`).

## Inner loop

```
go build ./...
go vet ./...
go test -race ./...
gofmt -l .          # CI fails if this prints anything
go mod tidy         # CI fails if this dirties go.mod/go.sum
golangci-lint run   # CI runs this too (bodyclose, staticcheck, …) — vet alone is NOT enough
```

The CI workflow (`.github/workflows/ci.yml`) gates exactly these. Run
them before pushing. (2026-07-12: a push failed CI on golangci-lint
findings that vet+gofmt missed — the linter is part of the gate, not
optional.)

**`golangci-lint`'s cache is per-USER, not per-checkout**, and this repo
is worked in `.claude/worktrees/*` several at a time. A run can therefore
report a finding whose path names a SIBLING worktree — code that is not
in your tree and that your change cannot have caused. `golangci-lint
cache clean` is the fix; a `.golangci.yml` exclude is not, because the
path is real and it is the cache that is shared. Note the asymmetry:
`go list ./...` does NOT cross worktrees (each has its own `go.mod`), so
the `go` half of the loop never shows this and the linter half does.

`go test -race ./...` is ~15s on a workstation. That number is an asset —
it is why agents here run the whole gate before every push — so anything
that pushes the blocking job past ~3 min is not worth whatever fidelity it
buys. The llama-swap conformance work lives in SEPARATE CI jobs for
exactly that reason.

## Test doubles and upstream contracts

**`internal/swaptest` is the one llama-swap double.** Stdlib only, no new
module, `NewCell(t, WithWire(…))` serving `/running`, `/v1/models`,
`/api/events`, `/api/metrics/activity` and the completion paths. New tests
that need a llama-swap use it; do not hand-roll a fifty-first `httptest`
stand-in.

- **Fail toward "no evidence", never toward "confirmed idle".** A parser
  reading an upstream contract must map a shape it does not recognise to
  *unknown*, never to a valid-looking value. `trackInFlight` is the worked
  example: an unrecognised inflight `operation` sets `inFlightSeen=false`,
  which the eight busy guards (drain, suspend, probe, both warm loops, the
  pre-drain report) already render as a refusal — because a *reported zero*
  is a positive claim of idleness that no guard can question. This is the
  only mechanism that protects against drift nobody noticed, and it is a
  property of the production code, not of a test.
  - The same shape one layer up: `usagemeter.PollAndSnapshot` returns the
    PREVIOUS cumulative totals whether the poll refused, timed out or
    succeeded. It is right about the value — those totals are still true
    — and it is also **exactly what an idle cell returns**, so "this cell
    is idle" and "this cell's collector has been timing out for six
    hours" are byte-identical on fleetd's side. `Collector.Health()` is
    the separator (`LastPoll` UNKNOWN until a poll completes, staleness
    measured from construction, WARN once the totals have been frozen
    5 min, rate-limited, with an all-clear); a durability failure counts
    as staleness too. A surface that renders usage and not freshness is
    rendering a number it cannot date.
- **The in-flight wire is version-dependent and the difference is not
  cosmetic.** v239 sends the full request list on every edge; v240+ sends
  operation-tagged DELTAS (`snapshot` / `upsert` / `remove`) with
  `requests` omitempty and absent, so `len(requests)` reads a request
  STARTING as an idle cell. Verified 2026-08-05 by running the fold against
  real v239 and real v247 binaries: pre-fix code passed v239 and failed
  v247. The fold is a SET keyed by request id, and it keeps the model per
  id because a v247 remove edge names only an id — without that memory C4's
  per-model activity never stamps and every `restore_after_idle` window
  reads as never-active.
- **A count belongs to the connection that produced it** — `clearInFlight`
  on stream drop. llama-swap re-seeds a fresh `/api/events` connection with
  a current-state inflight snapshot inside ~200ms (measured), so keeping the
  old count buys nothing and can strand a busy verdict forever.
- **Fixtures are RAW RECORDED BYTES** (`internal/swaptest/fixtures/<ver>/`),
  captured by `TestRecord` and checked in verbatim, never re-encoded
  production structs — a fixture that round-trips through the same json tags
  the decoder reads cancels out a tag typo, and a field the production
  struct omits (`resp_content_type`, `error_msg`, `has_capture`) is
  structurally invisible. logData payloads are the one redaction (they are
  the operator's log history); the byte lengths are kept in `RECORDED`.
  Every fixture set carries provenance and a `caveat` if it was not captured
  from a running binary.
- **Re-recording is triggered by bumping a pin.** `.github/workflows/ci.yml`
  pins the conformance matrix; whoever bumps it runs `TestRecord` and
  commits the new `fixtures/<version>/` BESIDE the old ones. Old recordings
  are kept, not replaced: a heterogeneous fleet is the normal state, so
  "both wires must pass" is the requirement, not "newest wins".
  `TestRecordingIsCurrent` fires on the box where the upgrade happened.
- **`TestSwapContract` is ONE assertion list run against every fake wire AND
  a real binary** (env-gated on `LLAMA_SWAP_BIN`/`LLAMA_SERVER_BIN`/
  `LLAMA_GGUF`, never a build tag — this module has zero `//go:build` lines
  and a tagged-out file is skipped by vet and the linter, so it rots
  uncompiled). Assert SEMANTICS, never bytes: v247 added five fields to the
  in-flight entry and two new frame types to the connect burst, all
  harmless, and a byte golden would have flapped on every one.
- **The double may not INVENT a field's value, and a conformance invariant
  may not `t.Skip`.** Two ways a green suite lies, both found by review on
  the pass that introduced the double. It logged `resp_content_type:
  text/event-stream` on every chat row whether or not the client asked to
  stream, so "streaming rows carry tokens" was satisfied by a row nobody
  streamed — and correcting the lie turned that invariant into a green
  SKIP. If a field's value is not something the double actually models,
  model it (`swaptest` honours `stream`, because a real v247 logs
  `application/json; charset=utf-8` for the same completion unstreamed);
  and a target that fails to produce the row an invariant is about must be
  RED, not skipped. Assertions on the double's *own* output — the `-1`
  sentinel, the wire shape — are required of `fake/` targets and merely
  noted on live ones: llama.cpp may legitimately not report a counter, but
  the double has no excuse.
- **Two upstream BEHAVIOURS are gated, not just the wire**
  (`internal/swaptest/behaviour_test.go`, C16). The loading-state
  keepalive (a streaming completion at a still-loading model must be
  delivering parseable delta frames within 2 s, and the same stream must
  go on to carry the model's own tokens) and SIGTERM's stream handling
  (`/api/events` closes at once; in-flight *inference* streams are NOT
  cancelled; anything still running at the 30 s grace is force-closed).
  Both are env-gated on `LLAMA_SWAP_BIN` because a signal is not a wire,
  and both use an in-test HTTP upstream so the question stays about
  llama-swap rather than about llama.cpp. The grace window is bounded on
  both sides on purpose — either direction changes what `TimeoutStopSec`
  has to be.
- **`GET /api/version` is an upstream contract like any other** (C16).
  `fleetapi.readSwapVersion` is the ONE reader, verified against real
  v239 and v247 binaries and replayed by `TestSwapContract` I6 on both
  fake wires. It **rejects rather than truncates**: a truncated version
  is a guess that becomes its own matrix key, and an unprintable one
  makes fleetd's `clean` refuse the cell's WHOLE announce — presence,
  intent echo, usage and probes with it. An upstream dependency the
  conformance suite does not replay is the hole C16 exists to close.
- **A shell command in a test is a shell-dependent test.** `/bin/sh` is
  bash on the workstation and dash in CI, and the difference is not
  cosmetic: bash exec-optimises `sh -c '<one word>'` while dash forks, so
  a one-word command exercises a DIFFERENT NUMBER OF PROCESSES in the two
  places. Any test whose subject is a process tree must fork explicitly
  (`sleep 10 & echo $! >pid; wait`) rather than trusting the shell to do
  it. This cost two CI rounds, once in `fleetapi` and once in `daemon`,
  each time as a bound that passed locally and hung on the runner. Two
  corollaries learned the same way:
  - **`/dev/tty`-dependent shell features differ too.** `set -m` looks
    like the portable way to give a background job its own process group
    and is not: dash's `setjobctl` opens `/dev/tty` and declines
    **silently** when there is no controlling terminal, which under
    `go test` is always, while bash sets the group anyway. Use `setsid`.
  - **A command with no pipe-backed output has no pipe to block on.**
    With `Stdout`/`Stderr` left nil, `exec` wires the command to
    `/dev/null`, `Wait` has no copy to wait for, the whole
    kill-does-not-reach-the-grandchild defect disappears, and every test
    passes for the wrong reason. Attach real pipes (`CombinedOutput`, or
    set both), and assert a FLOOR as well as a ceiling — the floor is
    what catches this.
- **The local rigs stay.** `scripts/fleetlab` and
  `scripts/smoke/llama-swap/` are not made redundant by any of this — CI has
  no GPU, no real models, no SSH, no wattmeter, and its numbers would be
  different numbers measuring a different thing. Run them: before merging
  anything that touches drain / suspend / warm / sleep-schedule / probe
  semantics (the eight `InFlight` call sites); whenever the weekly
  `drift` workflow goes red; after any llama-swap version bump on a real
  cell, BEFORE the fleet is left unattended overnight with sleep schedules
  armed; before a release; and when adding a new cell CLASS. **A green CI
  suite is never evidence that a phase doc's gate passed** — those
  transcripts are real hardware runs.
  - `scripts/fleetlab/gate-*.sh` are the per-gate rigs (one per phase
    gate, sourcing `gl.sh`); `marionette.py` drives a headless Firefox
    for the one gate that needs a DOM. They print raw evidence, never a
    verdict — **a rig that prints PASS is a rig that can print PASS
    while wrong.** C17's own review found six evidence lines that were
    structurally constant (`null`, `0`, `"none"` regardless of what the
    fleet was doing) under headings promising a measurement, with three
    flipped gate rows citing one of them. So: every column in a rig must
    be watched to MOVE when the fleet moves, no rig may act outside
    `$FLEETLAB_DIR` (`git -C`, never a bare `cd`), and no rig may print
    a credential. **Running the rig is not the same as reading it** —
    C18 found two defects, one a blocker, only by executing the rig it
    had already committed.
  - `vibe router render` hardcodes `startPort: 5800`, which is
    **production's** upstream range on this box. Every rig that
    re-renders a cell config goes through `gl.sh`'s `render_cell`, which
    FAILS if the port rewrite did not apply. Do not open-code a render
    in a rig.
  - **A lab instance is `FLEETLAB_DIR` + `FLEETLAB_PORT_BASE`, both,
    always** (C23). The dir separates the configs; the base separates the
    ports; `down` sweeps on both. Bases are multiples of 200 (refused
    otherwise — that is what makes two instances' listen AND upstream
    windows disjoint), so take one nobody else holds, check it with
    `ss -ltn` first, say which one you took, and never run `down` on the
    default while another agent is working. Pass the same pair to the
    gate rigs: they source `gl.sh`, which derives the same table, and a
    rig run without the base drives whichever lab holds the default
    block. `ports.sh` refuses outright a base whose listen or upstream
    window would cover `:9000`, `:9001`, production's 5800-5809 or
    `ritual.sh`'s ranges — base 12600 looks harmless and puts upstreams
    on 8980-9019. Do not defeat that guard.
  - **`internal/fleetlab` is a second Go package that runs real
    subprocesses and binds real ports** — the regression that keeps one
    lab's `down` out of another's. It skips itself when `bash`, `ps`,
    `pgrep` or `awk` is missing, and its negative control (both instances
    on one base, the sweep reaching across) is required: a sweep that
    kills nothing reads exactly like a sweep that is correctly scoped.

## Conventions agents tend to violate

- **Stdlib first.** Reach for stdlib before adding a dep. Current
  third-party set is small and intentional (cobra, yaml.v3, bubbletea,
  lipgloss, connectrpc, protobuf, isatty); justify any addition.
- **Modern Go.** `log/slog` for logging (not `log`), `errors.Join` /
  `errors.Is` / `errors.As`, `any` over `interface{}`, `embed.FS` for
  bundled assets. Go 1.26+ — `go.mod`'s `go` directive is the floor.
- **No emojis** in code, comments, commit messages, or generated docs
  unless the user explicitly asks.
- **Comments explain WHY, not WHAT.** Identifiers carry the what.
  Prefer no comment to a comment that restates the code. When you do
  comment, justify the surprising choice or the hidden constraint.
- **Don't narrate the current task.** No `// added for issue #123`,
  `// used by X`, or `// removed Y`. Those rot.
- **No documentation files unless asked.** Don't create README.md or
  similar in subdirectories without a request.

## vibe profile schema rules

- Backend is a **discriminated union by sub-block presence** — exactly
  one of `backend.llama_server`, `backend.comfyui`,
  `backend.http_server`, or `backend.tabby_api` must be set. We
  deliberately do NOT use a `kind:` field; the sub-block IS the
  discriminator. If you add a fifth backend, follow the same pattern.
- **`http_server` backend.** Wraps any HTTP-serving inference engine
  (TTS daemons, embedding servers, third-party inference). Two
  modes, mutually exclusive: docker (`image:` + optional `volumes`,
  `gpu`, `container_port`) or bare binary (`binary:`). Common
  fields: `port` (required, daemon proxies here), `args`, `env`,
  `health_path` (defaults to `/health`). The daemon synthesizes a
  `docker run --rm --name vibe-<profile> -p 127.0.0.1:N:M ...`
  invocation in docker mode. Used today for Kokoro-FastAPI TTS
  serving the audio stage's `capability: tts` capability. Frontend
  block is rejected — the HTTP server IS the deliverable.
  - **`bind`** is the host interface the docker publish binds to (left
    side of `-p <bind>:<port>:<container_port>`), default `127.0.0.1`.
    Set `0.0.0.0` for a service the rest of the fleet consumes; without
    it the container is unreachable from any other host. Docker mode
    only — binary mode leaves the listen address to the process, and
    setting it there is a validation error rather than a no-op.
    Readiness still probes `127.0.0.1`: the daemon is checking the
    process it launched, not the address clients use.
- **Retrieval plane (`internal/vibe/search`, `cmd/vibe-search`).** One
  service beside the router serving the other two things every harness
  needs: web search and page extraction, so no client holds a search
  key. `GET /search` speaks the **SearXNG** JSON contract — an
  impersonation, because that is the one search endpoint harnesses let
  you redirect (every other provider hardcodes its host), and speaking
  it keeps the client's NATIVE search path. `POST /mcp` exposes
  `fetch_url`, because page fetch has NO redirectable endpoint
  anywhere; that is the deliberate exception to keeping shared infra out
  of MCP. Fetch is tiered: free static extraction first, paid extractor
  only when a LARGE document yields almost no text (a JS shell) or the
  GET is blocked — page SIZE is the discriminator, since a short page is
  legitimately short. A failed escalation is reported, never silently
  downgraded to the thin result.
  - **`--mcp-expose-search`** also serves `web_search` over `/mcp`. Off by
    default because a client with a redirectable search endpoint should
    keep its native path (richer rendering, its own provider chain). Turn
    it on for harnesses that have no such endpoint — opencode's
    `websearch` accepts only `exa` or `parallel` via
    `OPENCODE_WEBSEARCH_PROVIDER`, so MCP is its only route to the plane.
- **`search_url` (`~/.config/vibe/config.yaml`)** backs `${VIBE_SEARCH}`
  in frontend templates and env, so one profile points a harness at
  models, search, and fetch together. Client-facing only — nothing in
  the daemon dials it. Unset is not the same as empty: `${VIBE_SEARCH}`
  is left out of the expansion map entirely so a profile referencing it
  fails to activate with a message naming `search_url`, instead of
  rendering an empty URL into a harness config.
- **`${MODEL_ALIAS}` / `${MODEL_CONTEXT}` follow the same rule.** They
  used to be always-known (every backend declared both), so rendering the
  zero value was harmless. A `cloud_peer` need not declare either, and
  `""` / `0` reaching a harness config means an unroutable model id or a
  context window of zero — so both drop out of the expansion map when
  unset and a template referencing one fails by name. `optionalVars` in
  `expand.go` carries the per-variable hint; add new optional vars there.
  Both are resolved in ONE place, `frontendModelVars` in
  `internal/vibe/daemon/daemon.go` — a dispatch over backend kinds, whose
  test walks every arm of the union because forgetting one renders an
  empty model id rather than failing.
- **Fleet control plane (fleet-control C1+).** `docs/design/fleet-control.md`
  is the design; the phase plan (C0–C21, all merged) is
  `docs/design/fleet-control-plan/`. The pieces an agent must not break:
  - `internal/vibe/fleetcfg` parses `$XDG_CONFIG_HOME/vibe/hosts.yaml`
    (cells registry + `fleetd_url` + optional `model_classes`) — the
    **single source of cell membership**; never introduce a second cell
    list. yaml.v3 only, no daemon imports, so CLI and daemon both load
    it. A `cells:` section requires a cell named `front`.
  - The daemon becomes **fleetd** only via `fleet_registry: true` in
    config.yaml (explicit role, never file-sniffing): multi-cell
    fleetapi registry, intent store (`POST /api/fleet/intent`), and the
    `internal/vibe/fleetmcp` facade at `/mcp` activate. Without it the
    daemon keeps the one-element front-cell registry and none of those
    routes — that regression is test-gated.
  - State axes (design §4): availability is OBSERVED, intent is
    DECLARED (`$XDG_STATE_HOME/vibe/fleet/intent.json`), residency is
    llama-swap-owned. The derived display states (SERVING / DRAINED /
    `DRAINED?` / OFF / OFF/AWAY / OFF/AWAY? / INCONSISTENT) are computed
    at read time in `fleetapi/display.go`. **Never act on `DRAINED?`**
    or inferred intent — display states are for humans.
  - Token visibility (fleetd runs containerized): the daemon logs
    "token CREATED (new)" vs "token loaded" at startup, and bearer 401s
    count into `/api/fleet/state`'s `daemon.auth_rejected` — a
    stale-token client must be visible as a number.
  - `vibe cell status|await` resolves fleetd via `--api` → `$VIBE_API`
    → `hosts.yaml fleetd_url` → local daemon, with a labeled degraded
    fallback to direct cell probes. `deploy/fleetd/` is the reference
    stack (state-dir volume is REQUIRED — see its README's state
    contract).
- **Fleet actuation (fleet-control C2).** The pieces an agent must not
  break:
  - `CellDrain`/`CellResume` RPCs act on the daemon's OWN cell via
    `cell_cmds.drain|resume` (config.yaml) — remote reach is calling a
    remote daemon, never routing. Errors: `FailedPrecondition` (no
    cell_cmds), `Unavailable`+stderr (command failed),
    `DeadlineExceeded` (quiescence wait expired, drain NOT run).
  - **llama-swap's SIGTERM path does NOT cancel in-flight INFERENCE
    streams** — corrected by C16, and this bullet asserted the opposite
    until 2026-08-05. Measured on real v239 and v247:
    `CloseStreams()` closes the EVENT streams (`/api/events` drops in
    ~1 ms) while inference streams keep flowing and are force-closed at
    a hardcoded **30 s** — the same grace a `-watch-config` reload gets,
    not a contrast with it. Gated in
    `internal/swaptest/behaviour_test.go` (B2), bounded on BOTH sides on
    purpose: either direction changes what `TimeoutStopSec` has to be.
    `vibe cell drain --wait <dur>` / `CellDrainRequest.wait_seconds` is
    unaffected and still required — a generation longer than the grace
    is truncated either way, which is what C2's ~39 s live gate actually
    observed. It is the quiescence wait that lets generations finish
    BEFORE the unit stops, driven by fleetapi's inflight SSE tracking.
    Unit `TimeoutStopSec` must exceed llama-swap's 30 s internal cap.
  - **One intent writer per invocation path, transport-distinguished**:
    TCP-arriving RPCs are fleetd-driven (fleetd writes intent after
    success, `fleetapi.SetIntent`); unix-socket ones are local (the
    cell daemon posts to `fleet.registry_url` best-effort). A failed
    drain never records intent.
  - **Advisory leases** (`POST/DELETE /api/fleet/lease`,
    `GET /api/fleet/leases`): keyed (cell, model, holder), Go-duration
    TTL, lazy expiry at read — they appear in the pre-drain report and
    fleet_status, never block anything.
  - **Render `cell:` rules** (`vibe router render --cell <name>`):
    front renders peers-only (models = def name + alias union,
    proxy from hosts.yaml); unassigned LLM defs are excluded with a
    warning; unknown cell names are render errors. cloud_peer follows
    cell: too — unassigned renders everywhere, assigned renders on its
    cell (front render only when front-assigned). Non-local `--cell`
    requires `--out`/`--stdout`. **`cell:` set ⇒ os.Stat validation
    gated OFF** (fleet.md §4.2's `host:` rule) — the canonical def
    checkout loads on every box; a def's paths are its cell's
    business.
  - MCP tools (fleetmcp): fleet_status, warm_model, unload_model,
    drain_cell, resume_cell, wake_cell, render_front (dry-run only
    until C3's apply path).
- **Fleet presence (fleet-control C3).** Cells dial OUT; fleetd never
  needs an inbound port. The pieces an agent must not break:
  - `POST /api/fleet/announce` (fleetapi/announce.go) is the
    registration endpoint: `"v": 1` required, unknown fields tolerated
    (version skew is guaranteed). Presence derives availability +
    last_seen; probes are the fallback for never-announced cells.
    Staleness is `3×interval + 5s` from fleetd-side `received_at`
    only — seq is a per-boot hint, cell clocks are never consulted.
  - **A cell cannot announce itself into existence.** hosts.yaml stays
    the single source of membership: an announce naming a cell the
    registry does not carry is refused `400 unknown cell`, so
    commissioning is daemon/announcer + `hosts.yaml` entry + registry
    URL — all three. What C3 retires is the inbound PORT, not the
    membership record, and **"announce-only" everywhere in these docs
    means fleetd cannot DIAL the cell** (no reachable inbound port, no
    `daemon_url`), never that the cell is missing from hosts.yaml.
    Several docs said the latter until 2026-08-05; they were describing
    a state that has never been reachable. Do not loosen the check to
    make them true — an announce accepting an unknown NAME is a
    fleet-wide write from an unauthenticated one, and the fleet token
    authenticates the connection, never the cell (design §6, below).
  - **The conflict rule**: registry intent is a REQUEST until the
    cell echoes it; a NEWER echo resolves it either way (complied or
    human override); older echo gets desired_intent handed back. The
    cell-side mirror (fleetannounce) executes only newer requests,
    **stamps intent only on a successful verb** (a failed/missing verb
    keeps the request pending — a false ack once let a lie resolve),
    and re-stamps already-in-state requests (ghost livelock). The
    daemon skips its C2-era intent POST when announcing (the echo IS
    the record). Split-brain always resolves toward the box. Echo
    `since` is clamped to now+2min at ingest — the one place a cell
    clock is consulted.
    `"serving"` on an announcing cell stores a resolvable resume
    request; on never-announced cells it deletes (C1 semantics).
  - **Availability honors evidence over declaration**: a probe that
    just answered stands over a drained echo (INCONSISTENT nags); the
    echo decides only when probes can't reach the cell.
  - **Announce-side model truth**: `gatherModels` = defs ∩ the cell's
    own llama-swap catalog (a multi-cell box must not leak defs across
    cells); defless catalog ids announce hashless + log-once.
    Fingerprints cover spec-rendered kinds only (llama_server,
    mlx_server).
  - **flags_sha256 canonicalization** (router/fingerprint.go): drop
    argv[0] and --port, NORMALIZE home-anchored paths to `~` (fleetd
    runs root, cells run users — tilde expansion otherwise false-
    mismatches every def), sort flag groups, join `\x00`. Weights-path
    swaps must still mismatch. Enforcement binds to the def's HOME
    cell (a cross-cell announce can't yank a strict def); unassigned
    defs skip enforcement.
  - **Presence-derived render** (fleetapi/render_loop.go): roaming
    prunes on stale/withdrawn, always_on/opportunistic hold; re-add
    needs `MinHealthyStreak` consecutive fresh announces (default 3);
    renders cap at 1/min coalesced, write only on change, cold-start
    hold until full wave or ~50s. `front_renders` in fleet_status is
    the flap-storm counter. Strict fingerprint mismatches exclude +
    event; advisory events only.
  - **fleet.front_config is the render mount contract** (daemon
    config): fleetd writes the front's watched config dir atomically;
    -watch-config applies. MCP drain/resume fall back to
    desired-intent when a cell has no daemon_url.
  - `vibe fleet announce` is the slim announcer (cells without a full
    daemon); the daemon's own loop is internal/vibe/daemon/announce.go
    — same fleetannounce.Client both ways.
  - **The fleet token is every cell's voice** (design §6 threat note):
    announce authenticates the connection, never the cell name — a
    forged announce can fake SERVING, prune a roaming catalog, or
    cancel pending drains. Distribute tokens like cell-root; per-cell
    credentials are a futures item.
- **Fleet comfort (fleet-control C4).** The pieces an agent must not
  break:
  - **Warm targets** (`warm_targets:` in config.yaml,
    fleetapi/warmtarget.go) restore the default ONLY after the
    swapped-in model goes request-idle (per-model activity from the
    inflight SSE stream, fleetd-side clock) — NEVER on a timer
    (pin/keep-warm evicts the operator's model mid-session; stays
    unbuilt). Empty-restore requires a time-based grace ≥ one announce
    interval (presence is heartbeat-stale; a swap mid-cold-start reads
    as "nothing resident"). Absent/drained cells skip silently, noted
    in fleet_status's `warm` block.
  - **Four ways the warm policy reaches that rejected behaviour**, each
    now guarded and test-pinned (C5) — do not undo any of them:
    *drain* is checked before presence AND probes via
    `effectiveIntent` (a drained cell announces an empty model list by
    design, which the nothing-resident branch reads as "restore");
    *in-flight* blocks the restore (`InFlight(cell) > 0`) and
    `trackInFlight` stamps activity on the completion edge as well as
    the start, or one generation longer than the window reads as idle;
    *unknown activity* measures idleness from the fleetd process's own
    start (`Server.started`) — never a fabricated floor — and only where
    fleetd actually WATCHES the cell (`observesActivity`: an inflight
    frame ever seen, or the events stream open now); with no observation
    channel the target is skipped, because otherwise fleetd's uptime
    becomes the clock the rule forbids. The status names the missing
    evidence either way; *`swapIdleFor` returns the real idle*
    (shortest across residents, unbounded above) so a window over an
    hour is not silently inert.
  - **Warm schedules** (`warm_schedule:`, fleetapi/warmsched.go): a
    minimal 5-field cron evaluator (stdlib, minute granularity, DST
    wall-clock semantics) firing warm through the front, with the
    eviction-fight guard. TZ is the environment's declared zone (the
    reference Dockerfile carries tzdata); every schedule's resolved
    `next_fire` shows in fleet_status so a wrong zone is visible.
    - **Vixie dom/dow, exactly**: both fields restricted ⇒ OR; either
      one a star ⇒ AND. "Star" is TEXTUAL (the raw field's first byte,
      like cronie's `entry.c`), so `*/2` is a star and `1-31` is not —
      never derive it from set cardinality. `dow=7` is Sunday, folded
      at parse time (`time.Weekday()` never returns 7). Names (`sun`,
      `jan`) are unsupported. Fall-back DST fires the repeated minute
      twice; that is documented and pinned, not "fixed" silently.
    - **A guard that cannot be EVALUATED is a skip**: `CellOfModel`
      returns an error so a `LoadDefs` failure (one malformed YAML in
      the backends dir) skips instead of firing unguarded, and an
      unreported in-flight count is not a zero one. Resolved-but-no-cell
      (a front-only alias) still fires, labelled `unguarded` in the
      status.
    - Warms run under `warmCtx`, whose cancellation is linked to
      `s.done` — both warm loops call `warmFn` synchronously from
      `s.wg` goroutines, so an unlinked timeout blocks `Close()`.
  - **The fleet page** (fleetapi/fleet.html via embed.FS at
    `GET /ui/fleet`): static, framework-free, bearer-exempt as a static
    asset ONLY (the ONE middleware exemption — exact-match, GET-only, on
    the escaped path, evaluated before mux path-cleaning, boundary
    test-pinned in daemon/fleet_registry_test.go and
    daemon/authpath_test.go; do NOT widen it to a prefix match,
    `path.Clean` or a decoded path). SSE (`/api/fleet/events`) drives
    debounced state
    refreshes; action buttons POST `/mcp` tools/call — never add
    mutation routes for it; if a button needs something new, the MCP
    facade is what's incomplete. `esc()` is the TEXT escaper and
    `attr()` the attribute one — they stay separate because esc()'s
    output also feeds `textContent`.
  - **`model_classes` guards EVERY warm producer, at both ends**
    (`fleetcfg.File.WarmClassRefusal` — the ONE sentence, used by
    fleetmcp's `warm_model`, both C4 loops, C14's post-wake warms and the
    daemon's config load). Every warm in the fleet is `warmViaFront`, a
    CHAT completion; hosts.yaml pinning an id to a non-chat class is the
    declaration that it must not receive one. Until the 2026-08-05 live
    gate only `warm_model` honoured it, and a `warm_schedule` fired five
    500-ing chat completions at an embed-class id — then queued them to
    the cell, because a 500 is a DELIVERY failure. So the refusal holds
    at WIRING (a `skipped` status row and no goroutine; a refused
    schedule carries no `next_fire`, and `warm.policy` reports its NOTE
    rather than "no resolved next fire", which names a cron field that is
    fine) and at FIRE time (`restore`, `evalScheduleEntry`, `wakeWarm`)
    and in `queueWarm`. Do not "fix" a refused embed target by adding an
    embed warm body: the right verb per class is a phase, and an embed
    warm is a fully METERED request on C7a's `embed` basis, which has no
    `poke_req` equivalent.
    - **The test is "does it answer a chat completion", not "is the class
      string `chat`"** (`fleetcfg.chatCapableClasses` = `chat` +
      `vision`). A multimodal model is llama-server plus `--mmproj`: same
      `/v1/chat/completions`, image as a content part, warmed by the same
      1-token request. Four of the five producers are automated policy,
      so a FALSE refusal is not a command failing in front of an operator
      — it is a declared target that silently never fires and a
      `warm.policy` yellow forever. `embed`/`rerank`/`stt`/`tts` each
      answer their own route, and `classify` names llama.cpp's
      sequence-classification family; a small model used FOR
      classification but served on the chat route is class `chat`
      (listing it documents ownership and gates nothing).
  - **Model-set changes are render triggers** (recordAnnounce): a cell
    that starts/stops serving a model re-renders like a membership
    transition.
  - **The render loop treats announces as untrusted input** (C5, in
    C3-authored code): `applyFingerprints` skips defs that are neither
    llama_server nor mlx_server before calling `router.ModelCmd`, and
    `ModelCmd` returns an error instead of dereferencing; a
    per-model verification failure warns and CONTINUES, because an
    aborted pass leaves `p.Announcing` uncleared and freezes prune,
    re-add and enforcement fleet-wide. `renderPass` refuses to render
    when ZERO defs loaded and a non-empty front config exists — the
    guard is INPUT-side, since a peerless render is legitimate when
    every def is unassigned or every roaming cell is pruned.
- **Fleet substrate repair (fleet-control C6).** Landed against merged
  C1–C5 code; each of these is a promise the substrate broke, so do not
  undo them:
  - **Staleness retires the ANNOUNCE, never the probe** (presence.go's
    not-fresh branch): a cell whose announcer died while llama-swap
    keeps serving stays `reachable`, or `vibe cell await --up` parks
    forever against a working cell. The withdraw's
    `HostReachable=false` is gated on the probe failing too.
  - **A complied drain becomes the RECORD, not a deletion**
    (recordAnnounce): a newer echo that AGREES re-stores the intent with
    its reason/ETA and `Since == echo.Since` exactly — that equality is
    what keeps it from reading as a pending request — and `decorate`
    carries reason/ETA through the echo override. Deleting it erased
    axis 2's headline feature one heartbeat after every ack. A diverging
    echo still clears the request.
  - **Every intent writer clones → persists → swaps.** A failed write
    must leave memory unresolved so the next heartbeat retries. But a
    DISABLED store is not a failed write: the RESOLUTION writers (the
    announce conflict rule, `pruneStaleServingRequest`) go through
    `persistIntents`, which no-ops when `intentPath == ""` — with no file
    there is nothing for memory to diverge from, and gating on a persist
    that can never succeed stops the C3 conflict rule dead (a resume at
    the box would stay pending forever, and C4's warm loops read
    `s.intents` regardless). `setIntent` still calls `saveIntents`
    directly: an operator POST to a disabled store must fail loudly.
  - **Fingerprint drift is a render trigger**
    (`modelFingerprintChanged`): enforcement only runs inside a render
    pass, so a steady-state `flags_sha256` change with a stable id set
    raised nothing. Compare the HASH only — `State` flips constantly.
  - **`flags_sha256` home-folding is box-independent**
    (router/fingerprint.go `normalizeHome`): local `$HOME`,
    `/home/<user>/`, `/root/`, `/Users/<user>/`. It fails OPEN by
    design — a false mismatch yanks a working model.
  - **Piggyback commands are at-least-once**, retired only by an
    announce with a higher seq; `QueueCommand` validates the model
    against what the cell ANNOUNCED. A seq reset (cell reboot)
    redelivers rather than pinning the slot.
  - **`CellDrainResponse.wait_status`** reports `not_requested` /
    `waited` / `skipped_no_inflight_data`, rendered by the CLI and the
    MCP tool: an operator who asked for quiescence and silently got a
    stream-cancelling drain must be able to see it. The local cell key
    is `Daemon.localCellKey()`, not the literal `"front"` — on a
    fleetd-role box that literal reads another cell's counter.
  - **`Client.Withdraw`** is the `withdrawing` producer (daemon
    shutdown + `vibe fleet announce`). It does NOT persist: a
    `withdrawing` echo read back at next boot would lie or erase a live
    drain. **Stop the loop before withdrawing** — the daemon holds the
    loop's own cancel + done channel (`Daemon.withdrawAnnounce`) because
    the shutdown-RPC path never cancels `ctx`, and a heartbeat still in
    flight lands after the goodbye and resurrects the cell. `seq` is
    mutex-owned for the same reason.
  - **The piggyback fallback is for DELIVERY failures.** Three
    producers, one rule. `unload_model` (fleetmcp) queues on a transport
    error or a 5xx from the cell's admin port; the warm-target restore
    and the warm-schedule fire (`fleetapi.queueWarm`) queue a `warm`
    when the front cannot deliver, or when the cell has **no front
    route at all** — the front's peers are rendered from `hosts.yaml`
    and every registry cell carries a `url`, so "no front route" means
    the cell is ABSENT from the registry (`fleetapi.noFrontRoute` says
    so; it read "announce-only" until 2026-08-05, naming a state the
    announce endpoint forbids). In
    every case a **4xx is the far side answering** and stays an error:
    telling an agent a refused verb is "queued for the next announce"
    is worse than failing. That decision needs a status, so
    `warmViaFront` returns a typed `*warmHTTPError` — do not collapse
    it back into a formatted string. A QUEUED warm never stamps
    `last_restore`/`last_fire`; those are records of a warm that
    happened.
  - **`writeAtomic` forces the front config's read bits back on**
    (`perm | 0o044`): every fleetd deployed before C6 left a 0600 file,
    and inheriting that mode would keep the bug alive on exactly the
    boxes that have it. Operator widening still survives.
  - `vibe cell await` fails fast on an unknown cell (`errUnknownCell`)
    and keeps retrying transport errors; `--timeout 0` stays the
    overnight-batch idiom.
  - `model_classes` has a closed vocabulary (`fleetcfg.ModelClasses`);
    the warm guard skips the CHAT-CAPABLE classes (`chat`, `vision`) and
    still refuses the rest — see C4's `WarmClassRefusal` note.
    hosts.yaml tolerates fleet.md's top-level `hosts:` inventory as an
    inert key — `KnownFields(true)` stays on.
- **Usage ledger (fleet-control C7a).** Tokens per cell, per model, per
  day — RAW COUNTS ONLY, no prices anywhere (C7b prices them; storing
  counts is what lets a price change re-price the whole history). The
  pieces an agent must not break:
  - **`internal/vibe/usagemeter` is CELL-side by necessity, not by
    taste.** llama-swap keys each activity row on
    `RealModelName(requested)`; the front's rendered config is
    peers-only, so that resolves nothing there and the front records
    whatever string the client typed. Only the cell resolves
    `qwen3.6-27b-tools` → `qwen3.6-27b`. Never turn this into a fleetd
    pull.
  - **Token semantics branch on `req_path`, never on backend kind.** On
    chat-family paths llama.cpp's `input_tokens` is `timings.prompt_n` =
    **cache-miss only**, so billable input is `input + max(cache, 0)`;
    on `/v1/embeddings` and the rerank family it is the OpenAI `usage`
    prompt figure with cache already included, so adding cache
    double-counts (1.8x-5x). `-1` is llama-swap's not-reported sentinel
    for `cache_tokens`, `draft_tokens` and `draft_acc_tokens` — always
    clamp. **mlx needs no branch**: it answers chat paths from `usage`
    and reports no cache, so the chat arithmetic degenerates correctly.
    The chosen branch is recorded per row as `basis`, because llama-swap
    stores nothing that says which parser won.
  - **Three corrections, all VISIBLE counters**: `poke_req` (chat rows
    with `output_tokens <= 1` — C4's warm loops issue real metered
    1-token completions and can outnumber human requests), `err_req`
    (non-200), `unmeasured_req` (200 with no tokens: mlx streaming and
    every cancelled stream). None is summed as zero, and `unmeasured` is
    **never** estimated from `duration_ms × tokens_per_second` — that
    field is -1 or 0 on exactly those rows.
  - **Day buckets use an explicit `*time.Location`** (`fleet.timezone`,
    `Config.FleetLocation()`). `Truncate(24*time.Hour)` rounds against
    absolute time and lands on UTC midnight regardless of the value's
    Location, silently; a grep test over the whole module forbids it.
  - **An announce is untrusted input on this path too** (C3/C5's posture;
    the fleet token is every cell's voice). The ledger is APPEND-ONLY, so
    `fold` hardens at ingest and a wrong value can never be corrected:
    counters are clamped non-negative on the CUMULATIVE total (clamping
    the delta would leave a poisoned cursor), and the two bases fleetd
    writes itself — `resident` and `cell` — are closed to cells, because a
    cell-announced `resident` row keys onto the exact bucket residency
    seconds land on. Unknown bases stay welcome (that's a C7b pricing
    question); one bad entry never costs the announce its other rows.
  - **No double count is a whitelist**: only announce-carried totals
    enter the ledger, and `fleetcfg.FrontCell` is skipped structurally
    by name at fold time. Totals on the wire are CUMULATIVE, so a
    missed heartbeat loses nothing and a duplicate announce adds
    nothing; the cursor is keyed `(cell, model, basis, epoch)` WITHOUT
    the day, or the first fold after local midnight rebills the cell's
    whole lifetime. `epoch` changes when the cell's activity ids restart
    (`max_id < cursor`) and starts a new row rather than flatlining the
    cell.
  - **The cursor carries an ANCHOR, and a contradicted window is never
    folded.** The epoch rule above only answers the DOWNWARD jump; a
    store restored or swapped for one whose ids sit ABOVE the cursor
    presents identically to a cell that served a lot while nobody was
    reading, and the 2026-08-05 live gate watched every counter double
    into the append-only ledger. So the state file records the id,
    timestamp, model, `req_path` and status of the row the cursor points
    at, and `Poll` refuses the whole window on either contradiction: the
    row now AT the cursor id is a different request, or a row above the
    cursor was recorded BEFORE the anchor (llama-swap stamps
    `ts_created` at request COMPLETION and inserts then, so id order and
    timestamp order agree within one store; `maxRowClockSkew` is for a
    backwards clock step, not request overlap). A break adopts the new
    head, folds nothing, adds the refused rows to `lost_rows` and names
    the evidence — under-count and say so, never silently double-count.
    It does NOT mint an epoch: the counters are the cell's LIFETIME
    totals and must stay monotone. An UNREACHABLE anchor is deliberately
    not a break — on an in-memory ring it ages out legitimately, and this
    tests CONTRADICTED continuity, not unproven continuity (the same
    reason a row-count cross-check against `/api/metrics/stats` was
    rejected: a ring's aged-out span and a swapped store's id hole are
    numerically identical). The two checks are a LADDER, not a
    conjunction: a row still sitting at the cursor id that matches the
    anchor PROVES the log's identity, so the clock scan is skipped —
    running it anyway let one backwards clock step (the thing
    `maxRowClockSkew` exists to absorb) discard a window of real traffic
    from a settled log. Two identity changes it does NOT catch, named in
    the code: a store copied from a busier box whose rows all postdate
    the anchor, and two llama-swap instances sharing one `store.path`.
  - Storage is append-only JSONL (`paths.UsageLedgerFile()`), coalesced
    in memory, flushed on a 60s ticker and at shutdown, compacted at
    start via tmp+rename. Deliberately NOT `history.go`'s
    rewrite-on-every-record: fleetd folds an announce per cell per 15s.
    **Compaction rewrites the file from memory, so a DEGRADED read (open
    error, aborted scan) skips it** — otherwise a transient read error
    deletes whatever didn't parse. Unparseable LINES still compact away;
    that is the cleanup, and it is why JSONL was chosen.
  - `GET /api/fleet/usage` is fleetd-only like `/mcp` and
    `/api/fleet/intent`, and belongs in
    `daemon/fleet_registry_test.go:TestDaemon_FleetRegistryOff_NoMCP`'s
    probe list with them — add every new fleetd route there.
  - `internal/vibe/proxy` is **not** instrumented and must not be: cells
    run `disable_proxy: true`, so a flawless tee there would measure ~0%
    of fleet tokens. Requires `store: {path: …}` in each cell's
    llama-swap extras (private fleet repo) or the activity log is a
    1000-row in-memory ring.
- **Savings screen (fleet-control C7b).** C7a's counts, priced.
  `internal/vibe/prices` (the vendored table + the arithmetic),
  `fleetapi/savings.go` (`Savings(ctx, window)`,
  `GET /api/fleet/savings`, fleetmcp's `fleet_savings`), and the
  `#savings` view inside the same `fleet.html`. Two levers decide
  whether the number is honest and both are load-bearing forever:
  - **Equivalence is the same open-weight model RENTED**, priced at the
    median across hosts that serve it (`open_weights == true`) and
    rendered as a range. Pricing a 27B local model as a frontier model
    moves the answer ~72x. The frontier comparable exists only as a
    config-declared line that **requires** a written `rationale`
    rendered beside it; **this repo ships no default frontier mapping**
    and a test greps for one.
  - **Prompt tokens are priced fresh + cache-read, separately, always**,
    from C7a's measurement (~5x). The direction inverts by endpoint,
    which is what `basis` is for: chat/cloud bill `in_fresh + in_cached`,
    embed/other bill `in_fresh` alone.
  - **A rate of exactly 0 means UNKNOWN, never free**, and an unpriced
    model keeps its tokens while leaving the money column. Every money
    field on the report is a POINTER: an unmeasured cell renders an em
    dash plus a reason and is excluded from the totals, so `$0` renders
    only for a genuine measured zero. No `capital_cost` → no payback bar
    at all; under 14 covered days → "too early to project"; a hopeless
    rate → ">10 years at this rate". The screen is allowed to be
    unflattering — one that can only render triumph will.
  - **The price table is vendored, dated and embedded**
    (`prices.json`, models.dev + LiteLLM, both MIT, commits recorded).
    Refresh with `vibe fleet prices vendor` on a networked box; CI never
    has network. Snapshots are append-only (base + overlays) and a day is
    priced at the newest snapshot on or before it — re-pricing history at
    today's rates would erase money that genuinely was not spent. A
    cross-source disagreement past 2x DROPS the row and fails the run
    unless `--max-disagreements` records a reviewed count.
  - **Energy is declared, never sampled** (`power: {source: declared}`
    per cell; `nvidia_smi`/`ha_entity` are named future values that fail
    validation today). Idle and busy are billed separately because C4's
    warm targets deliberately increase the idle term. Four rules the
    review pass had to restore, all in the same direction:
    **electricity is a COST, not a saving**, so a cell with no measured
    requests still bills its watts (a box holding a warm target resident
    all day IS the case §4 was written for, and the payback strip was
    already charging them); the energy denominator is the **cell-level**
    residency row, falling back to the **MAX** across per-model rows,
    never the sum; a **partial** power term (some cells declared, some
    not) is stated on the page, because it is the one place this screen
    errs LARGE; and the note names the actual missing field — an unset
    `pricing.electricity_price_per_kwh` blanks every cell that declared
    wattage perfectly well.
  - **The front never gets a payback bar.** C7a folds no token rows for
    it, so its numerator is defined to be zero; a `capital_cost` on the
    front becomes a note, not a bar that reads "0% of $N" forever. Same
    structural exclusion as the savings table.
  - **The page adds no route**: `#savings` is a hash-routed view, because
    `/ui/fleet/savings` would force C5's exact-match bearer exemption to
    widen. Bar widths via `el.style.width`, never an interpolated
    `style="…"`. No action buttons on that view, no external asset.
  - **Price the ledger through `Table.Resolver()`, never `At()` per day.**
    Payback is lifetime, so every window walks the whole history, and
    resolving the vendored table costs ~3 ms; per-day resolution made the
    endpoint cost ~1.3 s at 400 days and grow forever. Days inside one
    snapshot generation resolve identically, so the cache is exact.
  - Two additive C7a ledger changes belong to this phase: a **cell-level
    residency row** (`model: ""`) as the energy denominator, and the
    fleetd-reserved **`cloud` basis** on the front cell, fed by
    `daemon/cloudspend.go` tailing the FRONT's activity log for
    `cloud_peer` ids only — actual spend beside notional savings, the
    induced-demand control. A defs read that fails skips the poll
    entirely rather than folding unfiltered.
- **Throughput probes (fleet-control C8).** `internal/vibe/modelprobe`
  (cell-side prober + rolling baselines), `fleetapi/probe.go` (scheduler,
  shared guard, status block), the typed `AnnounceModel.Probe` block, the
  `probe` piggyback verb, and fleetmcp's `probe_model`. Answers friction
  pain 2 (llama-server degrades 10-100x while `/health` stays green). The
  rules that keep a measurement from becoming an actuator:
  - **A probe never loads a model.** It runs ONLY against a model the
    cell's own `/running` reports `ready`, checked on localhost
    immediately before the request; a cold model is REFUSED with a note
    to `warm_model` first. There is no force flag anywhere on this path,
    and there must not be one — that is why the prober is cell-side at
    all (a probe through the front is a request like any other, and
    llama-swap JIT-starts whatever it is asked for, which is exactly the
    eviction C4's warm policy exists to prevent).
  - **`degraded` is a per-MODEL health state and a fourth thing** beside
    the three ownership axes. It rides `ModelState.Probe` and nothing
    else: never `CellSnapshot.Reachable`/`.Display`/`.Intent`, never the
    front render's exclusion path (that stays fingerprint-only), never a
    warm/unload/drain trigger. The remediation runbook is human:
    probe → `unload_model` → probe. Test-pinned through the REAL snapshot
    path in `fleetapi/c8_test.go`. fleet_status's `probe.degraded`
    roll-up answers "is anything slow RIGHT NOW", so it reads FRESH
    announces only (C6's staleness rule applied to the one field that is
    pure evidence); the model row keeps the verdict either way.
  - **fleetd ASKS, the cell MEASURES.** Requests travel on C3's piggyback
    queue (so announce-only cells are probeable), guarded by C4's guard
    set verbatim — drained / stale / busy / **unreported** in-flight /
    leased / not-announced-ready / the front cell — with every skip named
    in fleet_status's `probe` block. One `probeGuard` holds all of them
    and serves both producers (the scheduler and the MCP verb) so they
    cannot drift; the daemon's config filter and the MCP verb ALSO refuse
    the front, loudly, but the shared guard is what makes that a rule
    rather than two coincidences (peers-only config ⇒ a probe there
    measures a peer through the front).
  - **The budget is explicit and enforced on the CELL**, because the
    piggyback queue is at-least-once: 5-minute per-model cooldown keyed
    on the last ATTEMPT (not the last result — refusals carry the last
    measurement forward), a rolling 96/day cap counting attempts, and
    single-flight. `probe_targets:` in the fleetd config is declared and
    empty by default; the daemon clamps its interval to 5 min.
  - **The baseline is CELL-side** (`paths.CellProbeFile()`), keyed
    `(model, flags_sha256, metric)` so a def edit starts a fresh baseline
    instead of reporting a config change as a regression, and scored as
    a median over ≤20 samples. Under 5 samples the verdict is `unknown`,
    never `degraded`; the announced `samples`/`baseline_at` describe the
    window BEHIND that verdict (the sample just taken is excluded from
    both, exactly as it is from `baseline_p50`), so `samples: 5` never
    rides `unknown`. **A degraded sample never enters the window** (or a
    real regression washes out in ~11 probes and the status goes green
    while the box is slow); `rebaseline: true` is the explicit escape
    hatch. It reuses `history.go`'s SHAPE (small window, rewrite on
    record) and deliberately not C7a's JSONL — records here are
    minutes-to-hours apart.
  - **A cell's probe specs and announced fingerprints are FROZEN at
    announcer start** (`modelprobe.Hooks` gets a defs slice loaded once,
    in `daemon/announce.go` and `cli/cmd_fleet.go`). fleetd re-reads defs
    every render pass; a cell never does. Under C0's `-watch-config` hot
    reload a def edit therefore takes effect in llama-swap while the
    baseline key and the announced `flags_sha256` still describe the old
    argv — so the new configuration's samples are scored against, and
    folded into, the OLD configuration's baseline, and a hot-applied edit
    at the cell raises a spurious fingerprint mismatch (and, for a strict
    def, a front-render exclusion). Restarting the announcer is the only
    thing that rebinds either. Watched end to end on the harness
    2026-08-05 (`scripts/fleetlab/gate-c8-l5-staleflags.sh`, C17 finding
    1): 100 s and six announces after the edit the cell still announced
    the old hash while fleetd logged the mismatch and dropped the def
    from the front. **This is an open defect, not a documented
    behaviour** — C8's L5 flag-change half FAILS.
  - `Prober.Start` returns immediately and probes on its own goroutine:
    the heartbeat is the cell's only evidence of life, and a synchronous
    probe would mark a cell stale for being slow to prove it was slow.
  - C8 adds **no HTTP route** (verdicts ride `/api/fleet/state`, the verb
    rides `/mcp`), so the fleetd route list and C5's exact-match bearer
    exemption are untouched.
- **hold_model (fleet-control C11).** The operator's declaration that
  fleetd must not act on its own warm policy for a cell until it
  expires — the evaluation-afternoon verb. `fleetapi/hold.go`,
  `fleetmcp/hold.go` (`hold_model` / `release_hold`), `vibe cell hold`.
  No new HTTP route, no proto change, no cell-side code.
  - **A hold IS a lease** (`Lease.Hold`), in the ONE lease store: the
    (cell, model, holder) key, TTL-at-read expiry, the atomic file, the
    pre-drain report and `cells[].leases` are all requirements of a hold
    that C2 already built. A second store would have duplicated every
    one of them. The holder is the reserved literal `hold` and the
    endpoint enforces that pairing BOTH ways — a hold under another
    holder is invisible to `release_hold`, and a plain lease squatting
    on the reserved holder would be deleted by someone else's release.
    Not the intent store: axis 2's vocabulary is cell state the CELL
    echoes, and no cell can echo a hold.
  - **What it suppresses**: the C4 warm-target restore (the new check),
    and — inherited, no new code — scheduled warms and C8 probes, which
    already skip on an active lease. What it does NOT touch: routing,
    the render, the catalog, availability/intent, `warm_model` /
    `unload_model` / `drain_cell` / `resume_cell` (an operator asking is
    not fleetd guessing), and llama-swap's TTL.
  - **A hold is not a pin**, and every surface says so. Residency
    belongs to llama-swap; the hold guarantees only that FLEETD will not
    cause the eviction. Do not "fix" this by writing a cell's TTL.
  - **Warm targets skip on a HOLD, not on any lease** (schedules keep
    skipping on any). The restore is already evidence-gated, so a
    working batch is covered by the in-flight/idle guards; what a hold
    adds is the case where the evidence is right and the conclusion is
    wrong. Widening it would let a forgotten 168h lease disable the warm
    policy for a week.
  - **The hold holds at BOTH ends of the warm path**: the loops check it
    when they decide, and `drainCommands` drops queued `warm` verbs for a
    held cell (`dropHeldWarmsLocked`) — the piggyback queue is
    at-least-once, so a restore queued one tick before the hold would
    otherwise land on the next announce and evict the held model. `warm`
    is the ONLY verb dropped: every queued warm comes from `queueWarm`
    (fleetd's own policy), while `unload` is an operator's verb and
    `probe` can be one. Use `holdOnLocked` there — `drainCommands`
    already holds `s.mu`.
  - **Surfaces with no hold flag key on the RESERVED HOLDER.** The
    pre-drain RPC report (`vibev1.LeaseView`) carries no `hold` field and
    must not grow one; `Holder == fleetapi.HoldHolder` is the
    deterministic test, and both renderers (`cli.printDrainReport`,
    `fleetmcp.leaseLine`) use it. `DELETE /api/fleet/lease` returns
    `existed` beside `status` so a release never claims work it did not
    do; `fleetapi.HoldLeft` is the ONE remaining-time string.
  - **Ladder: drained > held > swap-credential > stale > unreachable >
    policy** (the credential rung added by C15). Every rung skips, so the
    order decides only the reason an operator reads — which is why it is
    written down here and in the phase doc.
  - **Bounded and self-expiring**: default 4h, max 24h (tighter than the
    lease store's 168h, because a hold disables a configured policy). A
    re-issue refreshes the same key. There is no unbounded hold.
  - **A SKIP is not an observation of emptiness** (`setWarmState` clears
    `emptySince` on `skipped`): every skip returns before residency is
    read, so banked emptiness let the first tick after a hold or a drain
    ended fire the empty-restore against a mid-cold-start model — the
    live race C4's grace window exists to prevent.
- **Alarm notifications (fleet-control C9).** `internal/vibe/fleetnotify`
  (the dwell/dedup/rate policy engine + the webhook sink, zero fleet
  imports), `fleetapi/notify.go` (conditions, the away/home scope,
  `POST /api/fleet/notify/{scope,send}`, the `notify` status block),
  `daemon/notify.go` (config), fleetmcp's `fleet_notify_scope` /
  `fleet_notify_test`, `vibe fleet notify`, `vibe cell await --notify`.
  - **It is a state DIFFER, not an event bridge**, and that is not a
    style choice: `fleet.fingerprintMismatch` fires exactly ONCE per
    drift (renderPass runs on triggers; a steady wrong hash triggers
    nothing) and drain-with-lease has no event at all. Conditions are
    read off `Server.Snapshot` — the same document the page and
    `vibe cell status` render — so the pager and the page cannot
    disagree. Do not "fix" this by subscribing to the hub.
  - **The default policy is the design §4 class table's alarm column and
    nothing else**: always_on absence, persistent fingerprint drift,
    drain-with-active-lease. `model_degraded` is implemented and OFF
    (C8's verdict has a false-positive tail; the table does not list
    it). A notifier that fires on everything is one the operator learns
    to swipe away — that is a worse failure than no notifier.
  - **Declared intent may SUPPRESS an alarm; inferred intent does
    nothing.** A cell absent with a declared drain (DRAINED, OFF) is
    explained and does not page; absent with NO entry is `DRAINED?` and
    pages. That reads the intent store's emptiness as a fact, never as a
    guess about what the operator meant, and it actuates nothing.
    INCONSISTENT is a nag, not an alarm.
  - **Persistence needs a set, not an event**: `renderPass` rebuilds
    `Server.fpMismatch` every pass, preserving `FirstSeen`. Without
    `fleet.front_config` there are no passes, so the status reports
    `fingerprint_source: unavailable (…)` rather than letting a silent
    zero read as "no drift". The set takes **FRESH announces only**
    (`!p.Stale && !p.Withdrawn`): `Presence.Announcing` means "has ever
    announced" and survives both, so a powered-off hold-class cell would
    otherwise keep its last hash in the set and page about drift on a box
    serving nothing — including an `opportunistic` one, whose absence the
    class table says must never alarm. Same rule as C8's
    `probe.degraded` roll-up. The mismatch EVENT and the strict render
    exclusion are C3's and keep their own (stale-tolerant) semantics.
  - **The drain_with_lease alarm is a lease renderer**, so it keys on
    `fleetapi.HoldHolder` like `cli.printDrainReport` and
    `fleetmcp.leaseLine`: a C11 hold is a policy override the drain
    overrides, not an advisory note about running work.
  - **The dwell clock is monotonic** (`notifyNow`), and the tracker
    normalises to UTC on the way OUT (`stamp`). `time.Now().UTC()` strips
    the monotonic reading, and every dwell plus the token bucket is a
    `Sub` — a wall-clock step would move a threshold in either direction
    on the one subsystem whose job is not being silent. `away` stays
    wall-clock; it is a declaration about a date.
  - **Explicit sends go through `validateExplicit`**, because there are
    two producers (the HTTP route and `fleet_notify_test`) and only the
    route used to bound anything.
  - **Coalescing is three rules**: dwell on BOTH edges (a cell flapping
    faster than its dwell notifies zero times), an active key never
    re-fires, and a token bucket that DEFERS rather than drops.
  - **`notify.scope` (away/home) is its own file**
    (`paths.NotifyScopeFile()`), never a key in `intent.json` — every
    reader of that store treats a key as a cell that announces and
    echoes. Away gates DELIVERY only: alarms still fire, stay visible in
    `fleet_status` with a suppressed count, and coming home sends one
    digest. An explicit send (`notify test`, `await --notify`) is never
    gated by away.
  - **The webhook URL is a credential** (an ntfy topic URL is
    bearer-equivalent). `*url.Error.Error()` embeds the full URL, so the
    sink unwraps it AND scrubs; the status shows `Redact(url)` only.
    Both guards are individually mutation-pinned — do not delete either
    because "the other one covers it".
  - Delivery runs on one worker behind a bounded queue: `Enqueue` never
    blocks, and every request and backoff is bound to a context
    cancelled by `s.done` (C4's `warmCtx` rule). A 4xx is the far side
    answering and is never retried (C6's piggyback rule).
- **await extensions (fleet-control C10).** `vibe cell await` grew
  `--model <id> --ready`, `--idle <dur>`, `--unleased` and `--lease`
  (`cli/cmd_cell_await.go` + `awaitCell`), fed by
  `CellSnapshot.Activity` (`fleetapi/activity.go`). No new HTTP route,
  no new MCP tool, no new store — every condition is a read of an axis
  something else already owns.
  - **Missing evidence is never idleness.** `--idle` unblocks only on a
    computed window; where fleetd has no LIVE `/api/events` stream to
    the cell it keeps waiting and prints why. This is C4/C5's rule (a
    cell fleetd doesn't watch makes fleetd's own uptime the idle clock)
    with a bigger blast radius: a wrong warm restore costs a model load,
    a wrong `--idle` launches a 19-hour batch into a box in use. There
    is no `--assume-idle` and there must not be one.
  - **The window is floored at `cellUpSince`, not process start**: the
    watcher's connect instant per cell, cleared on drop. fleetd may not
    claim silence it was not connected to observe, and the reconnect is
    exactly when a running generation is invisible. `lastInFlightFrame`
    is the cell-level activity stamp — every inflight frame is an EDGE,
    so ANY frame is activity, which C4's per-model map cannot say once
    llama-swap TTL-unloads the model somebody just used.
  - **Idle is cell-scoped even with `--model`** (the contended resource
    is the GPU), a reported in-flight count > 0 outranks any window, and
    an unreported count is still not a zero one. A count whose frame
    PREDATES the current connection is neither: the remove edge was lost
    with the stream, so it is reported as missing evidence (no window),
    not as a live busy count that never resolves.
  - **Every condition is judged against ONE snapshot** — "ready at
    03:00 and idle at 03:05" is not "ready and idle". **The snapshot is
    the only verdict**: a transition frame triggers an immediate re-poll
    (which is what preserves C3's sub-second unblock) and never returns
    on its own. `fleet.cellStale` means the ANNOUNCER died, and C6 keeps
    such a cell `Reachable`, so taking it as a verdict reported a false
    `--down` for a cell still serving. Conversely a frame that is NOT a
    transition for this cell is not a reason to re-poll at all: fleetd
    forwards every upstream llama-swap payload from every cell, and
    `/api/fleet/state` is an uncached probe round of the whole fleet.
  - **Any evidence gap fleetd qualifies is printed on the SUCCESS line
    too**, not only while waiting. The gap that matters is "the stream
    is live and no inflight frame has ever arrived": that is the one
    reading of `--idle` resting on the cell's silence rather than on an
    observed edge.
  - **C6's fail-fast rule extends to model ids**: an id absent from a
    REACHABLE cell's NON-EMPTY catalog is a typo and errors even under
    `--timeout 0`; an unreachable cell or an empty catalog (a drained
    cell announces one by design) keeps retrying, as does any transport
    error. `--ready` against `front` is refused outright — peers-only,
    C8's probeGuard reason.
  - **Leases stay advisory**: `--unleased` is the WAITER opting to
    respect other holders (its own holder is ignored, so a crashed run
    can't deadlock on its own residue) and `--lease` claims one on
    success; a refused claim fails the command, because a batch that
    runs undeclared is invisible to the pre-drain report. A C11 hold is
    one of those holders and is named as `held: <model>` (key on the
    reserved holder, C11's rule); `--idle` alone does NOT consult holds,
    because a hold suspends what FLEETD initiates and an operator's
    batch is not fleetd guessing.
  - **Every refusal fleetd would issue is issued BEFORE the wait**
    (`validateAwaitFlags`): C11's reserved `hold` holder,
    `fleetapi.MaxLeaseTTL`, control characters in
    `--model`/`--lease`/`--lease-note`. The claim is POSTed after the
    wait, so a late refusal under `--timeout 0` is a night wasted — and
    `--unleased --lease hold` would additionally have stepped over the
    operator's own declaration on its way to failing.
  - **C9's `--notify` fires AFTER the `--lease` claim and reports its
    outcome** (the one decision the C9/C10 merge had to make, since both
    phases extended this command). Exactly one push either way: a refused
    claim still pages, saying so, and still fails the command. The
    question a human parked on a cell has is "is the box mine", which a
    push sent before the claim cannot answer and a push skipped on
    refusal answers with silence — the reading that sends someone to bed
    believing a batch is running. The message repeats the terminal's
    success line verbatim (`awaitSuccessLine`, the ONE renderer), because
    the surface a sleeping operator reads is the last place an evidence
    qualification may be dropped. A timeout or a fail-fast typo pushes
    nothing: `--notify` is await-UNBLOCKED.
- **Guest read-only token (fleet-control C12).** A second bearer
  (`fleet.guest_token_file`) honored on exactly `GET /api/fleet/state`
  and `GET /api/fleet/events`. Off unless configured; a fleet that never
  sets the key behaves exactly as it did before C12.
  - **`internal/vibe/fleetapi/routes.go` is the route registry AND the
    allowlist.** One table is both what `Register` mounts and what each
    route grants (`Access`: `AccessTokenOnly` | `AccessGuest` |
    `AccessPublic`). **Add a route there or it is not mounted** — no
    other non-test file in the package may call `mux.HandleFunc`, and a
    grep test enforces it. `Access`'s zero value is `AccessUndecided`
    and a test fails on it: "the next agent forgot" must not be spelled
    the same way as "the next agent decided". Deciding is one line, and
    the decision is `AccessTokenOnly` unless there is an argument.
  - **Enforcement is a positive allowlist**, in daemon/auth.go via
    `fleetapi.AccessFor(r.Method, r.URL.EscapedPath())`: exact
    (method, path), on the RAW path before the mux cleans anything,
    everything undeclared (`/mcp`, the whole Connect mount, typos,
    future routes) token-only. Never a denylist, never a prefix, never
    `path.Clean`, never `url.PathUnescape`. **C5's `/ui/fleet` exemption
    is now the table's one `AccessPublic` entry** — same GET-only exact
    match, and the same bypass family pinned in
    `daemon/fleet_registry_test.go` (the list has grown with the phases;
    do not quote a count at it).
  - **RAW means `EscapedPath()`, and the difference is not academic.**
    net/url decodes before the middleware runs
    (`url.ParseRequestURI("/ui/%66leet")` → `URL.Path == "/ui/fleet"`,
    `RawPath == "/ui/%66leet"`), so `r.URL.Path` is the DECODED path and
    matching on it granted every percent-encoded spelling of a declared
    route while this file claimed the opposite. Nothing was reachable
    that was not already reachable — Go's ServeMux routes on the decoded
    path too, so middleware and router agreed, and a positive exact
    allowlist can only re-grant a route it already granted — but a
    load-bearing security invariant stated falsely becomes a real hole
    the day anything routes on `RawPath`. Fixed 2026-08-05 by matching
    the code to the doc: an encoded spelling is a different string,
    therefore a miss, therefore token-only — the answer `/ui/fleet%2f`
    has always got. `/ui/%66leet` and `/api/fleet/%73tate` are pinned in
    `daemon/authpath_test.go`.
  - **The guest surface is state, never history.** `/api/fleet/usage`
    and `/api/fleet/savings` are refused despite being read-only GETs:
    tokens per cell per day is a record of when this house works and
    when it was away, and the savings screen adds capital and
    electricity cost. A guest sees what the fleet is doing NOW.
  - **The grant is a ROUTE grant, not a field grant**: a guest reads the
    whole state document — intent reasons, lease notes and all. No
    guest-shaped variant of the snapshot (C9's rule that every surface
    renders the same document), and the operational consequence is that
    anything on the fleet page is guest-visible.
  - **The token is a credential**: constant-time compared, never logged,
    never in an error string, never in a status document. Only
    `daemon.guest_enabled` (bool) and `daemon.guest_rejected` (count)
    surface, and guest denials are counted SEPARATELY from
    `auth_rejected` — that counter means "a client holds a stale token"
    and folding guests in would destroy the signal.
  - **Every misconfiguration fails CLOSED and non-fatally**: empty, too
    short (<16), whitespace/control chars, unwritable, or **identical to
    the control-plane token** all disable guest access with a loud
    `slog.Error` while the daemon keeps serving. The identical-token
    rung is the important one — such a token authenticates on the FIRST
    comparison and would grant every verb. A missing file is minted
    (0600) and logged CREATED-vs-loaded, C1's rule.
  - **`guest_token_file` may never BE the control-plane token file**,
    and that is checked on the PATH (`daemon.IsControlTokenFile`, before
    the file is read) rather than on the value — because the loader
    MINTS into a missing file, so the value comparison would run on a
    value it had just written over the control-plane token.
    `vibe token --guest` and `--guest --regenerate` refuse the same
    configuration: printing it hands out the control-plane token under a
    "share this" banner, and regenerating it rotates the control-plane
    token from a command whose name says guest.
  - **The page hides what a guest cannot do**, learning which credential
    it holds from the `X-Vibe-Auth: guest` response header on a request
    it already makes — no probe, no new route (C7b's rule), no
    per-viewer field in the state document (fleetapi has never known
    about tokens). Hiding is a COURTESY; the middleware is the boundary.
    If the header is stripped the page just keeps its buttons and a
    click 401s.
- **`vibe fleet doctor` (fleet-control C13).** The audit that answers
  "is the fleet still put together correctly" — the question
  `fleet_status` never asks. `fleetapi/doctor.go` (the report),
  `GET /api/fleet/doctor` (fleetd-only, `AccessTokenOnly` in C12's
  table), `fleetmcp`'s `fleet_doctor`, `cli/cmd_fleet_doctor.go`, and
  `daemon/doctor.go` (the host facts + the outbound credential probe,
  injected through `Options` exactly as `daemonInfo` is, so fleetapi
  still knows nothing about tokens or the filesystem).
  - **It is READ-ONLY and that is the feature**, because the command
    exists to be run mid-incident: no drain, warm, unload, probe, wake,
    render, queued command, intent write, lease, notification or config
    write is reachable from it. Pinned TWICE — behaviourally (every
    state file and both command queues byte-identical across a run) and
    structurally (an AST scan of `doctor.go` for mutating identifiers).
    Both are mutation-verified. The one write a doctor run can cause is
    `last-seen.json`'s sighting record, because doctor's evidence is the
    same `Snapshot` every other surface renders (C9's one-document
    rule) — that is a property of READING state, and it is documented
    rather than engineered around.
  - **Four levels, and `UNKNOWN` is not `OK`.** A check that could not
    be evaluated says so, with the reason, and carries its own exit code
    (0 clean / 1 FAIL / 2 WARN / 3 only-UNKNOWNs — "the report is
    incomplete"). This is C5's M2, C7a's `unmeasured_req`, C8's
    under-5-samples verdict and C9's `fingerprint_source` as a first-
    class output type. Never add a fifth level for "not applicable":
    where a configuration legitimately has nothing to check (an
    announce-only cell's outbound credential, a fleet with no https
    endpoint, the front's absent announcer), the verdict is OK with the
    reason — a permanent UNKNOWN on a healthy fleet teaches the operator
    to ignore the level. **This is `vibe fleet doctor`'s rule and not
    `vibe doctor`'s**; the local box audit has a genuine
    not-applicable and drops the check entirely (see *Profile authoring /
    first-run guards*). Two commands, two report shapes — do not carry a
    rule across.
  - **A check ID names what it PROVES**: `wake.configured` not
    `wake.armed` (a NIC's arming is not observable, and sending a packet
    to find out is a mutation), `tls.not_after` not `tls.valid` (the
    chain is deliberately unverified — LAN certs are self-signed, and
    the message says so). Ground rule 10 applied to check names.
  - **Missing evidence may only ever ADD concern, never subtract it.**
    `defs.parity` decides DIVERGENCE over every cell that reports a SHA
    and AGREEMENT only over the clean ones, because the first cut
    dropped a dirty checkout from the comparison entirely: the
    2026-08-05 live gate watched a correctly-reported divergence flip to
    OK the moment the diverged cell went dirty, and a fleetd dirty on a
    different commit suppressed its own WARN the same way. Dirty-and-
    diverged is strictly worse than clean-and-diverged. The two
    can't-compare shapes stay distinct UNKNOWNs — nobody reports a SHA,
    versus everybody reports one SHA and no tree is clean — and neither
    is an OK. fleetd's OWN checkout is named on every branch that names
    SHAs, not only on the agreement one: the box writing the front's
    render used to vanish from the report exactly when the cells also
    disagreed, which is the report where "so which tree is the render
    coming from" is the next question.
  - **The credential check uses the resolver the VERBS use.**
    `fleetcfg.CellCredential(cell, env, pref, localToken)` now holds
    C6's two deliberately-divergent precedences as named values
    (`PreferCellFile` for the long-lived fleetd, `PreferEnv` for a human
    typing one command); fleetmcp and the CLI both call it. A doctor
    with its own resolver would test its own code. Inbound auth needs no
    plumbing at all: the announce IS the credential test, since a cell
    with a wrong token never reaches the presence table.
  - **The one real derivation is `roaming.announcer`**: `host_probe` up
    plus no fresh announce means the announce agent is not running (or
    is being rejected — it says both, and cross-references
    `auth_rejected`). Everything else composes existing status blocks.
  - `versions.llama_swap` reports UNKNOWN naming the MISSING PRODUCER —
    the field has been a C3 reservation nothing writes. Do not "fix"
    that by guessing a llama-swap admin endpoint from a box that cannot
    verify it. `vibe fleet announce` now sends the same versions +
    capacity blocks the daemon does (`daemon.FleetVersions`,
    `daemon.FleetDiskCapacity`) — before C13 the heavy cell reported
    neither.
  - **A DECLARED suppression is the policy working, and `WARN` is
    subject to the same "permanent verdict on a healthy fleet" rule as
    `UNKNOWN`** (the review pass; three checks broke it). `warm.policy`,
    `probe.verdicts` and `usage.flow` route every skip through
    `explainedCells`, which derives the reason from the **StateSnapshot**
    — declared drain → C11 hold → class-normal absence, C11's ladder
    order — never from a loop's detail prose, and reports an explained
    skip as OK **with the reason named**. Without it one report called a
    hold both "active" and "the warm policy not doing what it was
    declared to do", and `vibe cell drain` turned two checks yellow for
    its whole duration. A probe skip is a finding only when the target
    was NEVER asked (`LastAsk == nil`): C8's guard set skips on in-flight
    work and an unreported in-flight count, so "skipped right now" is
    what a fleet in USE looks like. `intent.hygiene` gates BOTH buckets
    on `staleRequestAge` — the window between a drain request and the
    cell's echo is the normal middle of every drain — and `DRAINED?`
    (no entry) and `INCONSISTENT` (declared, not yet reconciled) are
    separate sentences; the second is not "undeclared".
  - **Every fan-out shares the report's ONE deadline**, so nothing on
    this path may be serial: the credential probes (self-review REV-1)
    and the TLS dials (review pass) both shipped serial first, and both
    produced rows describing endpoints that were never dialled. When
    `ctx.Err() != nil` a row names the BUDGET, never the host — "a host
    that is off and a broken TLS listener are indistinguishable" is a
    claim about a dial that happened.
- **`sleep_schedule` (fleet-control C14).** The declared night:
  `fleetapi/sleepsched.go` (the loop, the guard, the wake half, the
  status block), `daemon/sleep.go` (validation + the suspend RPC caller),
  `daemon/cell_suspend.go` + the `CellSuspend` RPC + `cell_cmds.suspend`,
  `fleetmcp`'s `suspend_cell`, `vibe cell suspend`, doctor's
  `sleep.schedule`, and C9's `wake_failed` alarm. It adds no HTTP route.
  - **The invariant line is the whole design: a DECLARED action deferred
    by observation is clean; observed idleness INITIATING action is
    rejected and stays rejected** (design §9's GPU-idle heuristic, still
    dead). The test for any change here: removing a guard could only ever
    make the suspend happen at a cron minute already named.
  - **Only `opportunistic` cells sleep**, refused by name for the rest:
    `always_on` absence alarms by design (§4's class table — teaching the
    alarm evaluator that some always_on absences are fine is how a
    taxonomy stops meaning anything), a `roaming` box cannot receive a
    magic packet from another city, and the front is refused
    structurally. Those three are `SuspendBlock.Structural` and `force`
    never bypasses them.
  - **The guard ladder** (`suspendGuard`, shared by the loop and the MCP
    verb): front / unknown / wrong class → refuse; declared drain (a
    drained box is one the operator TOOK — suspending it mid-game is the
    worst outcome available), C11 hold, **absence** (`Absent`: not a
    deferral — the entry stops for the night and records NO intent),
    unreported in-flight (C5's M2: unknown is not zero), in-flight > 0,
    any active lease, an outstanding `probe` command, no
    activity-observation channel, and the **quiet window** (`quiet_for`,
    default 15m) over C4's per-model activity stamps. Absence is checked
    BEFORE the in-flight rungs on purpose: an absent cell reports no
    count either, and "in-flight unknown" would turn "there is no box
    here" into a deferral that retries all night. Every skip is named in
    `fleet_status.sleep`.
  - **Suspend is an RPC with NO piggyback fallback.** The queue is
    at-least-once and retires on a HIGHER announce seq, which resets when
    a cell reboots — a redelivered `warm` costs a second, a redelivered
    `suspend` is a box that puts itself back to sleep the morning after.
    So `sleep_schedule` requires `daemon_url`, deliberately narrower than
    C2's drain.
  - **A suspend with no working wake is unwritable.** `wake:` is required
    on the entry, `cells.<name>.wake` is required in hosts.yaml, and a
    wake cron that fails to parse (or never fires) disables the **whole
    entry, suspend half included** — a broken wake must never yield a box
    that sleeps forever; it yields a box that never sleeps. Deferral is
    bounded by `max_defer` (default 2h) AND by the paired wake: the
    suspend never fires after its own wake.
  - **The wake half's order is load-bearing**: clear the sleep intent
    FIRST (a box that returns to a pending `drained` request runs its own
    `cell_cmds.drain`), then the packet through C2's `SendWake`, then
    await the return, then warm the declared models through the front.
    The clear matches on the reserved reason only — an operator's own
    `--reason gaming` drain survives a 07:15 wake untouched.
  - **No new state anywhere.** A sleeping box is axis 2's ordinary
    drained intent with `SleepIntentReason` and the wake time as the ETA,
    which renders as OFF + "asleep per sleep_schedule, eta 07:15" through
    C1 code: no display state, no intent vocabulary, no announce field,
    and an empty diff for `fleet.html`. It works only because
    `CellSuspend` **stamps the CELL's own local intent** on every
    invocation path before reporting success — without it C3's conflict
    rule hands the sleep request back on the first heartbeat after
    waking. The stamp happens AFTER the command succeeds (a failed verb
    records nothing).
  - **`wake_failed` is the one new alarm and it is default-ON.** It is
    not an observation of absence (an opportunistic cell's absence never
    alarms, forever) but a declared action of the control plane's own
    that did not complete. Read off the snapshot like every other C9
    condition; cleared when the cell announces fresh.
  - **The cron evaluator is `warmsched.go`'s, unforked** (a grep test
    fails on a second `parseCron` in the package): it carries the Vixie
    dom/dow textual-star rule, and two evaluators is how one of them
    rots.
  - **`force` is about tonight's conditions, never configuration.** It
    skips the policy rungs and the cell's `require_idle` proof (an
    operator asking is not fleetd guessing) and skips nothing structural.
  - **All THREE producers hold the structural refusals, including the
    receiving side** (the adversarial-review pass; the CLI held none of
    them, so `vibe cell suspend front` was a fleet outage and
    `vibe cell suspend <roaming>` was an unwakeable box). The schedule
    refuses at wiring, `suspend_cell` at `SuspendGuard`,
    `cli.suspendPreflight` from `hosts.yaml` — and a daemon whose
    `fleet.cell` is `front` refuses a REMOTE `CellSuspend` outright,
    because "the senders check and the receiver does not" is this repo's
    most repeated defect. Local invocation stays allowed: a human at the
    front box has declared it. Structural is also answered FIRST, ahead
    of the missing-wake-path message — telling an operator to add
    `cells.front.wake` reads as "then it would be allowed".
  - **`wake_failed` fires only for a suspend THIS schedule performed.**
    The wake half runs on its cron whether or not the schedule is why
    the box is away, so without the scope an `opportunistic` cell
    switched off pages every single morning — the one thing design §4's
    class table forbids outright, and the bug C9 already shipped once.
    The test is `st.asleep` OR the reserved sleep record being there to
    clear (that record is what carries the fact across a fleetd
    restart). A wake that finds nothing is visible, never a page.
  - **The quiet window is floored at `Server.started`** (C4's
    `swapIdleFor` rule, verbatim): with no per-model activity stamp for
    the cell, idle is measured from fleetd's own boot, because a fleetd
    that came up at 23:29 must not conclude the box has been quiet since
    23:15. The `observesActivity` rung is unreachable — rung 7 (`in-flight
    unreported`) subsumes it, since passing rung 7 requires
    `inFlightSeen[cell]` — and it stays as belt-and-braces with the
    subsumption test-pinned; do not let a test claim to exercise it.
  - The wake's declared warms skip a declared drain AND a C11 hold, and
    doctor's `sleep.suspend` is a WARN when the last suspend ERRORED (a
    deferred or abandoned night stays OK — that is the policy working).
- **The llama-swap credential (fleet-control C15).** `hosts.yaml`'s
  per-cell `swap_key_file` is the API key a cell's llama-swap demands
  (`apiKeys:`), presented as `Authorization: Bearer`.
  `fleetapi/swapauth.go` is the ONE authorizer and `AuthorizeSwap` is the
  only way a request reaches a llama-swap — an AST test in `fleetapi` and
  `fleetmcp` fails any `http.NewRequest*` in a function that does not
  call it.
  - **It is not `token_file`.** That authenticates the cell's vibe
    DAEMON (drain/resume/suspend); this authenticates llama-swap's own
    OpenAI + admin surface. The front settles it: in the reference
    deployment it runs llama-swap and no daemon at all, so it has no
    `token_file` to reuse — and it is the cell every warm goes through.
  - **`apiKeys` gates everything except `/health`** (measured on v239):
    `/v1/models`, `/running`, `/api/events`, `/api/metrics/activity`,
    `/api/models/unload/*` and `/v1/chat/completions` all 401, and a
    WRONG key gets 401 too (never 403). So a keyed fleet without this
    lost not just its warms but every probe, the whole in-flight evidence
    stream and every idle window built on it. `/health` answering is not
    evidence that anything else will.
  - **Three failure kinds, three fixes**: `unauthorized` (demanded, not
    declared), `rejected` (declared, refused), `unresolvable` (declared,
    unreadable — loud at config load, and no request is sent). They
    surface in `fleet_status.swap_auth` and doctor's `swap.credential`.
    The key VALUE appears nowhere: not a log, not an error, not a status
    document, not the page (C9's webhook-URL rule).
  - **A 401 suppresses the AUTOMATED warm producers for 5 minutes and
    re-arms**; the warm loops are tickers, so an unguarded 401 is a
    5,760-a-day retry loop. An operator's `warm_model` is never
    suppressed. A 401 is still a definitive 4xx and is never queued to
    the piggyback — routing around a broken credential hides it.
  - **The rung is drained > held > swap-credential > evidence**, and it
    is checked BOTH in `evalWarmTarget` and at fire time in `restore`
    (C11's clear-`emptySince`-on-skip makes a fire-time-only check flap
    the status row).
  - **`fleet.front_extras` is part of the credential**: the front's
    config is derived and rewritten on every membership transition, and
    the renderer emits no `apiKeys`, so without the extras merge fleetd
    deletes the key it was just given. Doctor's `front.extras` FAILs on
    exactly that combination.
  - **`fleet.front_extras` must RESOLVE, not merely be declared.**
    `router.mergeExtras` maps a missing file to "no extras, no error", so
    a typo'd path erases the front's `apiKeys` exactly as an unset
    `front_extras` would — and the front then stops demanding a
    credential at all, which is worse than the 401. `renderPass` refuses
    the write while the declared file cannot be read (input-side, and
    only when there is a non-empty front config to protect — the
    zero-defs gate's shape), and `front.extras` is a FAIL. `render_front`
    merges the LOOP's extras, not the CLI default, or its dry run reports
    the operator's `apiKeys` as a deletion.
  - **The page carries the KIND, not the sentence** (`fleet.html`'s
    status strip): the detail names `cells.<name>.swap_key_file` and the
    state document is guest-readable, so the sentence stays on
    `fleet_status` and doctor. No new route — C7b's rule.
  - **Two disclosure levels, and the split is at the source.**
    `fleetcfg.SwapKeyError.Error()` names the PATH and belongs on the
    token-only surfaces (the daemon log, the doctor, `/mcp`, an
    operator's terminal); `.Public()` names only
    `cells.<name>.swap_key_file` and the reason, and is the ONLY thing
    that may reach `/api/fleet/state` — C12 grants that document to the
    guest bearer, and the warm rows ride it too.
  - **fleetd refusing to SEND is a refusal, not a failure to deliver.**
    `definitiveWarmRefusal` covers `*swapAuthError` as well as a 4xx: an
    unresolvable key file was untyped, so `queueWarm` piggybacked the
    warm to the cell and executed it there — routing around the broken
    credential on the one tick before the sticky refusal arms.
  - **The cell-side dialers are NOT covered** (fleetannounce, modelprobe,
    the cell's own usagemeter, C18's trial prober, `ReadOwnSwapVersion`):
    different config surface, and a deliberate scope boundary rather than
    an oversight. Do not invent a second credential resolver for them.
    The hazard to know: `announceOnce` maps a `gatherModels` failure to
    an EMPTY model list, so a keyed cell without a cell-side credential
    announces "nothing resident" — which is what C4's warm policy reads
    as "restore".
- **The upgrade ritual (fleet-control C16).** The front image is
  **digest-pinned by default**, and moving the pin is
  `scripts/upgrade/ritual.sh` (preflight → record → canary → gate →
  pin), never an edit. `TestReferenceFrontStackShipsADigestPin` fails if
  the reference compose default or `.env.example` goes back to a
  floating tag.
  - **`front.image_pin` is declared, `versions.llama_swap` is observed,
    and the pair is the point.** fleetd has no docker socket and must not
    grow one, so whether the deployment is pinned can only be declared
    (`fleet.front_image`, with `unmanaged` as the closed-vocabulary
    declaration that the front runs no container). What catches a
    declaration nobody applied is the observed version matrix.
  - **`versions.llama_swap` has a producer** (C13 reported it UNKNOWN
    naming the gap): `fleetapi.readSwapVersion` is the ONE reader —
    `GET /api/version` — used by both producers, each cell for its own
    llama-swap and fleetd directly for the FRONT's (the front runs no
    announcer and is the box the incident happened to). ONE reader, TWO
    postures: the fleetd path (`frontSwapVersion`) carries the front's
    C15 credential and folds the status back through `NoteSwapStatus`,
    and `ReadOwnSwapVersion` is the cell-side entry point that presents
    none, because C15 scopes the cell-side dialers out.
    `TestOnlyTheCellSideReaderSkipsSwapAuth` is what keeps the second
    from becoming a way around C15's scan; the exemption fails if it
    outlives its caller.
  - **Absence in the matrix is NAMED, on every branch.** A front that did
    not answer and a cell whose vibe build predates the producer are both
    silent in a way no other check reports, so `versions.llama_swap`
    carries them the way `defs.parity` carries `absentNote` — and the
    front's silence goes in the Summary, because it is in no other
    absence list.
  - **A version with no recording is a WARN ahead of plain divergence.**
    `fleetapi.GatedSwapVersions()`, `internal/swaptest/fixtures/` and
    `ci.yml`'s matrix are three copies of one fact, pinned to each other
    by two tests. Adding a recording without adding it to the other two
    is red.
- **`vibe model try` (fleet-control C18).** `internal/vibe/modeltry` (the
  journal, the steps, the refusals, the derivation),
  `internal/vibe/cli/cmd_model_try.go` (the deferral and the two
  declarations), `profile.BackendDef.Trial`, and one exclusion in
  `router.Render`. It adds no HTTP route, no MCP tool, no proto change,
  no fleetd state and no dependency.
  - **A trial def never reaches the FRONT render**, and the guard is
    `case front && def.Trial` in `router.Render` — the one function every
    renderer goes through (the CLI, `RenderToFile`, fleetd's presence
    loop). On the reference fleet it is unreachable (fleetd renders from
    its OWN def checkout, and the trial def is in the cell's); it is
    exactly reachable on a single box where fleetd and the cell share
    `backends/`, which is what `scripts/fleetlab` and every adopter have.
    The exclusion is LOUD: a def that vanishes from the catalog without a
    sentence is the bug, not the fix. `trial: true` requires `cell:` and
    `backend.external: true` and refuses `cloud_peer`. **Promotion is
    deleting one line** — nothing in vibe can do it, because entering the
    fleet catalog is a change to a shared git repo with a human on it.
  - **Applying is C14's sentence again**: a declared action deferred by
    observation. The write is DISRUPTIVE (C0 measured it: `-watch-config`
    reloads, the old upstream drains under a hardcoded 30 s, then
    force-closes), so the wait is C10's `awaitCell` with `--idle` — the
    SAME idleness notion, not a second one — and C10's "missing evidence
    is never idleness" comes with it. `--now` is C14's `force`: about
    tonight's conditions, never about configuration.
  - **The derivation drops every claim on the fleet** and keeps only the
    serving flags: `router.aliases`/`alias_owner`, the
    `llama_server.alias` (reset to the trial's own name), `huggingface:`,
    `mmproj`/`draft_model`/`spec_*`/`chat_template_file`,
    `fingerprint: strict`, `lifecycle.preload`/`refresh` all go.
    Inheriting an alias is a silent re-point of every client already
    using it. **`estimated_vram_gb` is NOT inherited either**: it is a
    hand-measured claim about the incumbent's weights, and copying it
    made the resource half of the comparison equal by construction.
  - **Two declarations, and the right one for each**: a plain C2 lease on
    the cell (holder `model-try` — the trial is a CONSUMER) and a C11
    HOLD on the INCUMBENT (so the warm-target restore does not reload it
    the moment the cell looks idle and evict the candidate). The hold is
    taken only when there is not one already; the journal records what it
    TOOK and releases exactly that. **A declaration is journalled the
    moment it is taken**, not when the sequence finishes — one that lived
    only in process memory is one nothing ever releases.
  - **`--unleased` cannot skip a C11 HOLD.** It skips the waiter's own
    LEASE holder, and a hold is keyed on the reserved holder, so every
    hold looks like somebody else's. C18's deferral waits with `unleased`
    immediately before placing a hold on the incumbent, so it deadlocked
    against its own declaration on every resume and against an operator's
    hold from the start. `awaitConds.holdExempt` names ONE model; its
    zero value changes nothing, so `vibe cell await --unleased` still
    respects every hold.
  - **The journal states name what is TRUE ON DISK** — `planned` /
    `fetched` / `staged` / `applied` / `measured` — which is what makes a
    killed run resumable and reversible from a later process. `Load`
    treats a corrupt journal as an ERROR, never as "no trial". `end`
    releases the declarations FIRST and independently of the file
    rollback, then re-renders, falling back to a byte-exact banked copy
    when the render fails for an unrelated reason. Weights survive unless
    `--purge`.
  - **An apply must never REMOVE a section of the cell's config.**
    `mergeExtras` treats a missing extras file as "no extras, no error",
    so a trial rendered with the wrong `--extras` silently deleted
    `apiKeys:` (C15's credential — the cell then demands no key at all)
    and `store:` (C7a's activity log), and the rollback's re-render
    re-created the loss. Both the apply and `end` prove the render first
    and refuse a write that drops a top-level key the config has today;
    an unparseable config is a refusal, not "nothing to lose".
  - **`--dry-run` closes only a journal IT opened.** `Plan` resumes an
    open trial and `End` works from any state, so "close the journal Plan
    opened" performed a full rollback of somebody's in-flight trial under
    a flag whose whole promise is that nothing happens. The decision
    cannot live in `End`; it is `Trial.Resumed` (never serialized) plus
    `Runner.DiscardIfFresh`.
  - **The trial's prober is READ-ONLY on the cell's probe state**
    (`modelprobe.Config.ReadOnly`). That file is a whole-file rewrite
    from memory and the cell daemon's prober owns it; a second process
    holding it for the twenty minutes two cold loads take discards every
    baseline sample recorded meanwhile.
  - **Absent evidence is a refusal here too**: an unknown HuggingFace
    size is not a small file and an unreadable filesystem is not an empty
    one — both refuse the pull, `--skip-disk-check` is the only override,
    and the refusals are re-evaluated on RESUME (the commonest resume of
    all is a killed twenty-minute pull). `--idle 0` and `--min-free 0`
    are refused rather than honoured: zero is how those flags spell
    "unset", so honouring them silently skipped the guard.
  - **Both sides of the catalog observation POLL.** `-watch-config`
    notices a write on its own ~2 s cadence, so `Apply` waits for the id
    to APPEAR and `end` waits for it to LEAVE, on the same budget.
  - **The report states what it is not** (C7b's rule): one sample each
    against C8's five-sample minimum, the trial measured SECOND with the
    warmer page cache, defs-not-models, throughput-is-not-quality, and a
    comparative ratio printed ONLY when both sides reported the same
    metric. Cell-local by construction: `--cell` must be this box's own
    cell, and cross-cell `try` is a phase, not a flag.
- **Front failover (fleet-control C19).** `internal/vibe/fleetmirror`,
  `vibe fleet mirror`, `docs/runbooks/front-failover.md`, and doctor's
  `mirror.age` / `mirror.contents`.
  - **There is no automatic failover and there must not be one.** An
    automatic front promotion is the silent rerouting invariant 3
    forbids, one layer down from a router that retries a dead cell
    elsewhere. `fleetmirror` makes a MANUAL recovery fast; the only thing
    it contributes to the two-boxes-answering problem is a REFUSAL
    (`TakeoverProbe` — a TCP dial of the recorded `fleetd_url` and front
    URL; `restore` stops if anything answers, `--force` is for the one
    false positive where the operator already moved the address).
  - **The refusal fails closed on DOUBT, not only on an answer.** A dial
    that times out, a name that NXDOMAINs, and any error class nobody
    enumerated all stop the restore (`ErrProbeInconclusive`); only an
    answer (`ErrTakeover`) and a definite `ECONNREFUSED` are conclusions.
    NXDOMAIN earns its own clause because it is the one that fires on
    **every recorded name simultaneously** — resolution happens on the
    box doing the asking, and that box is by construction the one that
    was NOT the front, so a standby whose resolver lacks the internal
    zone reads as unanimous evidence that the old front is gone. It used
    to be classified as a definite negative, and the restore proceeded.
    `--force` remains the single escape, and it is a claim that a human
    checked.
  - **`vibe fleet mirror` is a HOST command on a timer, never a fleetd
    loop.** It has to keep working when fleetd is what broke, and fleetd
    cannot see the host paths its own state is bind-mounted from.
    fleetd's only involvement is READING the receipt the command leaves
    in the state dir (`paths.MirrorReceiptFile`) — no new mount, no new
    route, no network call, and C13's injection shape unchanged.
  - **The mirrored state table is bound to `paths.go`**
    (`TestMirrorCoversEveryFleetStateFile`): a new `StateHome()`-rooted
    path is either captured with a sentence saying what its loss costs,
    or excused by name in `notFleetState`. A phase that adds a state file
    and neither mirrors nor excuses it fails. The three cell-side files
    (`cell-intent.json`, `cell-usage.json`, C8's `model-probe.json`) are
    captured only as the front box's OWN copy if it happens to be a cell
    — every other cell's live on that cell and do not die with the front.
    C18's `ModelTrialFile` is excused by name for the same reason.
  - **Credential values are never carried; paths are.** `hosts.yaml`'s
    `token_file`/`swap_key_file` and `fleet.notify`'s files become
    `references` (path, exists, mode, consequence) for the runbook to
    fetch from the private repo. fleetd's OWN token and guest token ARE
    carried, because they are the identity being assumed and excluding
    them turns a recovery into a fleet-wide re-key — so the archive is
    `0600`, the manifest sets `credentials: true`, and every surface says
    so. `--no-secrets` is the opt-out, at the price of a re-key and a
    re-render.
  - **`verify` before every restore, and plan-then-write.** Modes are
    restored rather than inherited (a `0600` token landing `0644`
    publishes the fleet's root credential), archive paths are validated
    against `..` and absolute forms, `extras/` is never placed
    automatically, and an unset destination is a named skip rather than a
    guess.
  - **`fleet.mirror_max_age`**: unset is UNKNOWN, `unmanaged` is the
    closed-vocabulary opt-out, over 3× is a FAIL, and a receipt stamped
    in the FUTURE is UNKNOWN — a clock step forward would otherwise make
    a stale mirror read as fresh forever.
  - `scripts/fleetlab/gate-c19-drill.sh` is the one rig that deliberately
    kills lab processes (`./lab.sh down && up` after it), and it is the
    quarterly fire drill the runbook names. **The drill has never run on
    metal**: a second physical box taking the front's address over a real
    LAN is C19's L2 and is UNRUN.
- **The invariant harness (fleet-control C20).**
  - **`internal/vibe/observed` is where absent evidence lives.**
    `observed.Value[T]`'s zero value is UNKNOWN; the value is unexported
    so it cannot be read without the bit; `OrElse` is how a caller WRITES
    DOWN what absence means. `Server.InFlight` returns one, and so do
    `modelLastActivity` and `cellLastActivity`. Three module-wide scans
    in that package's tests keep the old shape out — a discarded known
    bit, a `(value, knownBit)` field pair, a `(numeric, bool)` return —
    each with a written-reason exemption table and an inertness floor.
    Add to the table with a reason; do not delete a scan, do not widen
    `pairSuffixes`, do not narrow `numericResult`.
  - **Carried gap, verified rather than reasoned about (U6): the
    `(bool, error)` evidence carriers are outside all three scans.** Scan
    1 matches `.Observed()` calls, scan 2 matches struct field pairs,
    scan 3 matches `(numeric, bool)` returns — and `bool` is deliberately
    not in `numericResult`. So a function like
    `modelprobe.isResident`, where **`false, err` means "no answer" and
    `false, nil` means "asked and told no"**, is seen by none of them:
    with the dropped-bit mutation applied, `./internal/vibe/observed/`
    stays green. `ok, _ := …` is one keystroke, and the answered-no here
    is not a neutral value — it is the note that says C8's cardinal rule
    fired. Where such a function exists, pin it in its OWN package
    (`TestIsResidentsErrorIsNeverDiscarded` is a package-local AST scan
    carrying both meta-guards) and register the mutation. Whether the
    module-wide scans should grow a fourth is undecided; do not assume
    they cover this shape.
  - **`internal/mutation` is the review step, encoded.** A registry of
    `{name, file, find, replace, pkg, mustFail, why}`. A guard whose
    mutation leaves every named test green is UNPROTECTED; an entry whose
    `Find` stops matching is STALE and must be re-pointed, never deleted;
    a mutation that does not compile proves nothing. The staleness audit
    runs in the blocking test job (milliseconds); the runner is its own
    CI job behind `VIBE_MUTATION_TEST=1` (~21 s warm, ~58 s cold).
    **When a review pass mutation-verifies a guard by hand, add the
    entry** — that is the whole point.
  - **`internal/astscan` is the reusable "every function that does X must
    call Y" scan.** C15's credential rule (both packages) and C4's warm
    class rule are the instances. `MinProducers` is not optional: a rule
    that matches nothing passes. An exemption is a function name mapped
    to a reason, and an unused exemption is an error. A phase that adds
    an HTTP request builder to `fleetapi`/`fleetmcp`, or a warm producer,
    must RAISE the corresponding floor — lowering it to make a deletion
    quiet is the failure this exists to prevent.
  - **`internal/shelllint` covers `scripts/`**: unguarded `cd` under no
    `set -e`, `rm -rf` on a bare `$VAR` (use `${VAR:?}` — `set -u`
    catches unset, not empty), and `pkill` patterns with no variable in
    them. Two exemptions, both in `gate-c15-warm-auth.sh`, both written
    down.
  - **`drain --wait` refuses to claim quiescence from the LOSS of the
    in-flight report, and refuses to ACT on it either.** An unreported
    count mid-wait never returns `waited`; a gap shorter than
    `inflightEvidenceGrace` (5 s, ~3 reconnect attempts) is ridden out,
    because llama-swap re-seeds a fresh `/api/events` connection with a
    current-state snapshot inside ~200 ms and giving up on the first
    missing tick turns a blip into the force-closed generation `--wait`
    exists to prevent. Only a gap that outlasts the grace — or the
    operator's own deadline expiring while the evidence is missing —
    answers `skipped_no_inflight_data`. Both renderers say "no in-flight
    evidence", never "the cell never reported": the status has two
    producers and asserting either as fact sends the operator to debug
    the wrong thing.
- **Alias scope (fleet-control C21).** The visible-repoint alias tier is
  **rejected** (design §9); `docs/design/fleet-control-plan/c21-alias-tier.md`
  holds the argument, the workaround and the revisit conditions. The rule
  the code now enforces:
  - **Alias ownership is decided over the DECLARED def set, and an
    exclusion removes an alias from the catalog rather than transferring
    it.** `router.Render` resolves over `aliasClaimants(defs)` — every
    external model def it was handed, before its own cell/trial/front
    selection — and `fleetapi.renderPass` calls the exported
    `router.ResolveAliases` on the full `LoadDefs` result (kept in
    `declared` before `applyClassPolicy` and `applyFingerprints` drop
    anything), passing the winners as `router.Options.AliasWinners`. The
    CALL sits AFTER both overlays on purpose: its error aborts the pass,
    and `applyFingerprints` is the only thing that evaluates C9's
    persistent-drift set — resolving first froze that set on its last
    value while the notify status still called the evaluator live. Both
    sites are required: the second excludes defs the first can never see.
    Resolving over the survivors is how a pruned roaming owner silently
    handed `best-coder` to another cell's model — measured end to end
    against merged `main` on real llama-swap processes, a `200 OK` from a
    model nobody asked for, with nothing in `fleet_status` and nothing in
    the log.
  - **The collision error must not heal by attrition.** Two claimants and
    no `router.alias_owner` is a render error; `renderPass` returns it
    rather than swallowing it, so it survives one claimant leaving. The
    error disappearing WAS the repoint. A fleet in that state has a
    frozen front catalog until a human sets an owner — which is the loud
    direction, and it already failed every render while both claimants
    were present.
  - **The declared alias IS the answer to the roaming-best-node
    problem**: `router.aliases` + `router.alias_owner: true`, repointed
    by moving one line in the defs repo. Do not add an automatic
    re-resolution, a `best-*` namespace, or a per-request fallback.
    `scripts/fleetlab/gate-c21-alias.sh` serves a real completion through
    a declared alias and proves the departed one 404s.
  - **An alias that leaves the catalog is named in the log**
    (`warnOrphanedAliases`). The prune's own line names a CELL, and the
    co-claimant's box is still up, so nothing connected "laptop pruned"
    to "`best-coder` stopped resolving". It fires only for defs the
    overlays dropped — the exclusions inside `Render` warn there — and
    only when the dropped def actually WON an alias.
  - **Carried gap, for whoever owns the next fleetd status surface: a
    failing `renderPass` is invisible to every surface.** No field in
    `/api/fleet/state`, nothing on the fleet page, no doctor check, no C9
    condition — only `slog.Warn("presence-derived render failed")` inside
    a container. Pre-existing, but C21 creates the first PERMANENT
    instance: an unresolvable alias collision now correctly stays an
    error instead of healing into a repoint, so the front catalog can
    stop tracking the fleet indefinitely while `front_renders` sits still
    and every display reads green.
- **Every operator-supplied shell string goes through
  `internal/vibe/shellcmd`** (U3, completed by C26a). `exec.CommandContext`
  kills the process it STARTED; with `sh -c` that is the shell, and the
  shell is not where the work is for anything containing a `;`, an `&&`,
  a subshell or a background job. The deadline then fires exactly on
  time, the error reads `signal: killed`, and the call does not return
  until the operator's `systemctl`/`ipmitool` finishes on its own — a
  bound that is armed and has no reach, which on the wire is
  indistinguishable from a bound that worked.
  - `shellcmd.New(ctx, script, killGrace)` wires **both** halves and
    neither subsumes the other: a process group of its own plus a
    negative-pid `SIGKILL` ends the WORK (reaching what the shell
    forked), and `WaitDelay` ends the WAIT for the descendant the kill
    cannot reach. A non-positive `killGrace` is refused loudly — zero is
    `WaitDelay`'s documented "block indefinitely", i.e. the defect.
  - The four call sites are `fleetapi.wakeCommand` (5 s — a wake crosses
    a network to a BMC), `daemon.runCellCmd` (2 s — drain/resume/suspend
    talk to systemd on the same box), `daemon.runHooks` (5 s — and it has
    no deadline of its own, so the missing reach was *worse* there) and
    `fleetannounce`'s desired-intent verb. `internal/mutation` holds a
    membership entry per site.
  - **A call site behind a test seam is not on the shared builder until
    something fails when the seam's DEFAULT drifts off it.** Every test
    in `fleetannounce` replaces `execCmd`, so nothing there executes the
    production value; `TestVerbSeam_ProductionDefaultIsTheBoundedRunner`
    asserts the default by pointer identity, and the bound itself is
    proved through the UNREPLACED seam. That pairing is the general
    shape: assert the default, and drive at least one behavioural test
    with nothing stubbed.
- **A recorded stop is not a request (fleet-control C24).** A cell unit's
  own hooks post `state: unit_stopped` / `unit_started` to
  `/api/fleet/intent`; fleetd stamps `fleetapi.StopIntentReason` and
  refuses such a record five things: `handleAnnounce` never returns it as
  `desired_intent` (an announcing cell answers a drained desired_intent
  by RUNNING `cell_cmds.drain` — the record would stop the stack it only
  described), it is deleted the moment the cell echoes a drain of its
  own, `resolveIntent` never marks it pending, `absentAlarm` /
  `explainedCells` never let it explain an absence, and `SetIntentAt`
  never lets it overwrite an entry that carries a why (C14's sleep record
  is written *before* the suspend takes the box down through that same
  unit stop). `unit_started` retires a stop record and **only** a stop
  record: it never clears a human's declaration and never stores a
  serving request. **The verbs are states on purpose** — an unknown state
  is a 400 on every build of this endpoint, so a drop-in installed
  against an older front degrades to doing nothing instead of to
  actuating. Adding a sixth surface that reads intent means deciding
  whether a record with no why belongs in it.
- **`deploy/cell/` is host-installed packaging, and its scripts are under
  test.** `internal/vibe/cli/c24_test.go` executes the shipped files —
  the wrapper's argv and `exec`, the hook's bound, its exit-0 contract
  and its tripwire-verified inertness — and parses the systemd drop-in.
  Editing anything in `deploy/cell/` without running
  `go test ./internal/vibe/cli/ -run TestC24` is how the example that
  gets copied stops matching the code.
  `scripts/fleetlab/gate-c24-stop-record.sh` is the live half: the
  shipped hook against a real fleetd and a real announcing cell whose
  drain verb really stops its llama-swap.
- **`vibe model try --replay` (fleet-control C25).**
  `internal/vibe/benchreplay` scores an incumbent and a candidate against
  **n of the cell's own recent captures**, replayed in place, and emits
  only scores. The bytes it reads are the most private objects in the
  fleet and this repository is public, so the constraints are mechanical
  rather than editorial: `internal/swaptest`'s recorder REFUSES the
  capture route by name (`RefuseCaptureEndpoint`, plus an astscan rule, a
  data-driven endpoint list and a walk of the embedded fixture tree);
  `Report` cannot carry a body (a reflection walk against a field
  allowlist and a closed set for every string field); the package cannot
  write a file (an AST scan over `os.*`); bodies become shapes at the
  boundary in `shape.go`, the only file that sees a `[]byte`; and nothing
  crosses a box. **Harvest precedes apply by ORDERING, not by comment** —
  C18's apply is a `-watch-config` reload, which builds a llama-swap with
  a fresh empty capture buffer, so `Harvest` refuses at journal state
  `applied`/`measured` and `Measure` takes the sample as a parameter.
  Divergence is **structural** (tool-call vs prose, tool name,
  finish_reason, JSON validity), never text similarity, and the recorded
  response is a NOISE FLOOR rather than a target, because the captured
  request carries the client's own temperature. Every rate is gated on
  ITS OWN denominator (`ToolCallRate` divides by `ToolsOffered`, not by
  `Requests`) and every floor is a refusal to answer, never a zero.
- Frontends use an explicit `frontend.kind` enum
  (`external | docker-compose | managed`) because frontends share many
  fields; the sub-block-presence trick doesn't fit.
- **`frontend.write_files: [{path, template, mcps}]`** renders multiple
  config files per profile (split-config tools like oh-my-pi); valid
  for kind=external and kind=managed. The legacy
  write_file/template/mcps trio is treated as the first entry
  (`Frontend.WriteFileSpecs`), whose resolved path backs
  `${WRITE_FILE}`.
- **Lifecycle hooks.** Top-level `hooks.pre_start` / `hooks.post_stop`
  are lists of shell commands (each run via `sh -c` with the daemon's
  environment). pre_start runs after the VRAM pre-flight and before
  backend/frontend launch — a failure aborts the start. post_stop runs
  best-effort after teardown — failures are logged and the remaining
  hooks still run.
- Backend path fields (`backend.llama_server.path`,
  `backend.llama_server.binary`, `backend.llama_server.mmproj`,
  `backend.llama_server.draft_model`,
  `backend.llama_server.chat_template_file`, `backend.comfyui.dir`,
  `backend.comfyui.python`, `backend.tabby_api.model_dir`,
  `backend.tabby_api.venv`, `backend.tabby_api.repo`,
  `backend.tabby_api.draft_model_dir`, `backend.http_server.binary`,
  and the host half of `backend.http_server.volumes` entries) are
  tilde-expanded in `Backend.normalize()`
  (`internal/vibe/profile/backend_def.go`) so inline and referenced
  backends share the expansion. Frontend path fields
  (`frontend.workdir`, `frontend.binary`, `frontend.write_file`,
  `frontend.write_files[].path`, `frontend.compose_file`) are
  tilde-expanded in `internal/vibe/profile/profile.go:Load`. Add new
  backend path fields to `Backend.normalize()` and new frontend path
  fields to `Load`.
- **Chat templates (`jinja` / `chat_template_file`).** `jinja: true`
  emits `--jinja`, which renders whatever Jinja `chat_template` the
  quantizer baked into the GGUF. That is the *only* thing it does — it
  carries no guarantee the template can handle an OpenAI `tools` array.
  Quantizers have repeatedly shipped tool-call-broken templates
  (Qwen3-Coder, gpt-oss, Gemma 4) and fixed them by re-uploading in
  place on the same HF repo, so a GGUF pulled before the fix keeps
  rendering the broken one forever. `chat_template_file` pins an
  explicit `.jinja` file via `--chat-template-file` for profiles whose
  frontend does tool calling. Two constraints, both enforced in
  `validateLlamaServer`: the file must exist (no HF pull path covers
  it), and `jinja: true` is required — llama-server validates the
  template as it parses the flag and only accepts an arbitrary file
  once `--jinja` has been seen, so `LlamaServerSpec` also emits the two
  flags in that order. To check what a running backend actually
  resolved, `GET /props` reports `chat_template` and
  `chat_template_caps`. Unrelated to vamp's per-request
  `chat_template_kwargs` (that toggles variables *inside* whichever
  template is loaded; this picks the template).
- **Vision models (mmproj).** `backend.llama_server.mmproj` is the
  path to the multimodal projector GGUF that llama-server loads via
  `--mmproj`. Required to enable image input on vision-capable
  models (Gemma 3, Qwen2.5-VL, LLaVA, etc.) — without it,
  llama-server rejects image content parts with "image input is not
  supported". When `huggingface.mmproj_file` is set, vibe pulls a
  second file from the same repo/revision into the mmproj path.
  Validation rules (in `validateLlamaServer`): mmproj path must
  exist on disk unless an HF mmproj_file is provided; setting
  mmproj_file without an mmproj target is rejected. The HF pull
  flow in `daemon.Pull` is a helper closure (`pullOne`) called
  once per file — weights, mmproj, and draft model — all
  streaming download progress over the same RPC stream.
- **`mlx_server` backend (Apple silicon).** Supervises `mlx_lm.server`
  from a venv (`<venv>/bin/mlx_lm.server`; `vibe pull` shells out to
  `<venv>/bin/hf` for the snapshot, same as tabby_api). It exists because
  MLX is measurably the faster runtime on Apple silicon — on an M3 Pro,
  Qwen3.6-35B-A3B ran ~27 tok/s under MLX 4-bit against ~15 tok/s for the
  same model as Q4_K_XL under llama-server, matched at 400 generated
  tokens. llama_server stays the right backend on the NVIDIA boxes.
  Two upstream quirks drive the schema, and both are handled for the user:
  - **No context flag.** mlx_lm.server takes the window from the model's
    `config.json`. `context:` is advertised metadata (it feeds
    `${MODEL_CONTEXT}`) and does NOT constrain the server — lowering it
    saves no memory, unlike llama_server's `context`.
  - **No alias flag.** It advertises the literal `--model` value on
    /v1/models and treats a request's `model` field as a model to *load*,
    so an unrecognised id sends it to the HuggingFace API and fails the
    request. `alias:` is still the client-facing id: the proxy rewrites
    alias→model_dir on the way in and model_dir→alias in /v1/models and in
    completion responses (including SSE chunks — otherwise an absolute
    filesystem path leaks to every client), and the router renders
    llama-swap's `useModelName` for the same reason. A path-shaped alias
    is rejected at load with that explanation.
  Unlike llama_server it does not demand a frontend (it follows
  tabby_api's shape): the same def is meant to serve a laptop frontend
  when disconnected and be spawned by hum/llama-swap when connected, and
  `router.modelCmd` builds its argv from the same `profile.MLXServerSpec`
  the daemon uses so the two can't drift. Unlike llama-server it honours
  `chat_template_args` (`{enable_thinking: false}`) — verified: without it
  Qwen3.6 spent 1200 tokens in `reasoning` and emitted no content.
- **Speculative draft models (Gemma 4 MTP).**
  `backend.llama_server.draft_model` points at a draft GGUF loaded via
  `--model-draft`; vibe also emits `--spec-type` (`spec_type`, default
  `draft-mtp`) and `--spec-draft-n-max` (`spec_draft_n_max`, default 4).
  Gemma 4's MTP head ships as a separate ~0.4B "assistant" drafter
  (unlike Qwen MTP, which is in-weights and needs no draft file).
  `huggingface.draft_file` pulls it from the same repo into the
  draft_model path (same `pullOne` flow as mmproj). `validateLlamaServer`
  mirrors the mmproj rules and additionally **warns** (stderr,
  non-fatal) on a quantized `cache_type_k/v` with `draft-mtp` —
  quantized KV needs a llama.cpp build with PR #23398 (hadamard
  rotation for quantized K); on older builds draft acceptance drops to
  ~0%. Verify acceptance after start.
- **VRAM pre-flight: warn, don't block.** `vram.Check` refuses a start
  ONLY when `estimated_vram_gb` exceeds the machine's total capacity —
  the one case no amount of freeing fixes. Merely being over *free*
  memory is a yellow warning and the start proceeds, because free memory
  is a moving target: the same profile on the same laptop reported 15.2
  GiB free (warn) and 23.5 GiB free (ok) minutes apart, purely from page
  cache. `--no-vram-check` (on `start`, `run`, and `backend start`)
  bypasses even the hard stop. `vram.DefaultProbe` is nvidia-smi where
  there's an NVIDIA GPU and vm_stat-based unified-memory accounting on
  Apple silicon; the Metal working-set ceiling is deliberately NOT
  guessed at (reading it needs Metal, and vibe is cgo-free), so a model
  between that ceiling and total RAM warns rather than failing.
- **Backends (reusable model specs).** A backend is a named model-server
  spec under `$XDG_CONFIG_HOME/vibe/backends/<name>.yaml` (`profile.BackendDef`
  = a `backend:` union + `estimated_vram_gb` + optional `mode`, no frontend).
  A profile either inlines `backend:` or references one with `backend_ref:
  <name>` (mutually exclusive; `Load` resolves the ref into `p.Backend` so
  everything downstream is identical). Lets many frontends (pi, qwen-code,
  Open WebUI profiles) share one model definition.
  - **Backend is the unit of model activation.** `StartRequest.backend`
    activates a backend with NO frontend — the daemon synthesizes a
    frontend-less profile whose `Name` IS the backend name, then runs the
    normal Start machinery. The active identity is that name, so repeated
    activations of the same backend are no-op reuse.
  - **vamp capabilities map to backend names**, not profiles:
    `capabilities.yaml` values are backend names; the executor calls
    `vibeclient.EnsureBackendActive`. Backward compat: if no backend by that
    name exists (`IsNotFound`), it falls back to `EnsureActive` (profile).
  - Adding a path field to a backend? It lives on the `Backend` union, so
    `Backend.normalize()` (in `backend_def.go`) handles tilde-expansion for
    both inline and referenced backends — add it there, not in `Load`.

## vamp stage rules

- Adding a stage type? Touch all of: `Stage` struct in
  `internal/vamp/pipeline.go`, the type switch in
  `pipeline.go:Validate`, the executor in
  `internal/vamp/<kind>_executor.go` implementing `StageExecutor`,
  `stageCacheable` in `cache_key.go` if it should be cacheable, and
  `schema.go`'s stage-properties block.
- **Cache invariants.** `stageCacheable` (in
  `internal/vamp/cache_key.go`) is the single source of truth for "can
  this stage type be cached?". Today it returns true for `text`,
  `comfyui`, `audio`, `ffmpeg`, `render`, `compact`, `pandoc`, `mix`,
  `short` and false for everything else (`youtube`, `confirm`).
  `webhook` is non-cacheable by default but opt-in cacheable via
  `cache: true` (for idempotent reads). Side-
  effect stages must not be cached — replaying a "success" would skip
  the side effect that gave the pipeline its reason for existing.
- **`.stages.X.output` semantics depend on stage type.** For text /
  render / webhook stages (including their foreach variants — the
  per-item content is `\n\n`-joined) it renders the **content**
  produced by the stage, so templates can inline it directly:
  `{{ .stages.merge_lessons.output }}` drops the merged JSON into
  the next prompt verbatim. For binary stages (comfyui / audio /
  ffmpeg / youtube) it renders the **absolute path(s)** to the
  output file, since those bytes are not text. When a downstream
  stage needs a field out of a text-stage's JSON, use `readFile`
  only if the path-shaped form is needed; otherwise pipe directly:
  `{{ .stages.X.output | parseJSON | <accessor> | toJSON }}`.
  See `examples/rag-eval-pipeline/` for the canonical chain.
- **Stage executors take injectable runners.** Every executor accepts
  a runner / httpDoer / process spawner that tests can swap. Don't
  hard-code `exec.Command` or `http.DefaultClient` at the executor
  level.
- **Webhook assertions.** `webhook` stages take an optional
  `assert:` block with `status_code` / `body_contains` /
  `body_not_contains` / `min_body_length` checks, exercised in
  `webhook_executor.go:runWebhookAsserts`. Designed for smoke-test
  pipelines that probe a stack from the outside. Setting
  `assert.status_code` overrides the executor's default "2xx
  required" so tests can verify a 401/4xx. GET/DELETE webhooks may
  omit `body:` / `body_template_file:` (POST/PUT/PATCH still
  require one to avoid silent empty notifications). The
  `examples/profiles/chat-with-search/smoke.yaml` pipeline is the
  canonical use.
- **Vision (image_dir / image_files).** Two ways to attach images to
  a `type: text` stage: `image_dir` (scan a directory, glob all
  supported files) or `image_files` (explicit templated list, one
  rendered path per entry, single-image-per-iteration fan-out).
  Mutually exclusive; same downstream encoding. SVGs get rasterised
  via `rsvg-convert` into a content-addressed PNG cache under
  `$XDG_CACHE_HOME/vamp/svg-rasterized/`. Rasterisation fits the
  output within 896×896 (`--width 896 --height 896 --keep-aspect-ratio`)
  so the result is a single Gemma 3 vision tile (~256 image tokens);
  exceeding 896 in either dimension triggers pan-and-scan and balloons
  token count. Requires `rsvg-convert` on `$PATH` when SVGs are present,
  and a vision-capable backend (Gemma 3 + mmproj) to actually consume
  the images.
- **Foreach items run independently.** A failing item no longer cancels
  sibling items via the per-stage context. Each item completes or fails
  on its own; the stage aggregates partial output from successes and
  reports joined errors. See `exec_test.go:TestExecutor_ParallelForeach_IndependentItems`.
- **Template functions.** Registered in `exec.go:templateFuncs`:
  `readFile` (tilde-expanded), `readFiles(pattern)` (glob, 200KB max
  per file, sorted, errors on no-match), `readFilesOrEmpty(pattern)`
  (same but returns "" on no-match — for foreach prompts that may
  have empty per-item globs), `readLessons(path, batch, total)`
  (paginated lesson reading), `enumerateLessons(glob)` (JSON array of
  lesson dirs, filters files >200KB), `enumerateImagePairs(root, lessonsJSON)`
  (flatten lesson list to per-image `{lesson, image, image_path}`
  entries), `enumerateUniqueImages(root, lessonsJSON)`
  (content-hash-deduped variant returning `{hash, path, ext}`),
  `imageDescriptionsForLesson(runDir, root, lesson)` (per-lesson
  reverse-lookup against `runDir/image_desc/<hash>.json` files),
  `extractSVGText(path)` (parse SVG XML, return `<text>` labels
  joined by `|` — sidecar for vision prompts so the model sees
  ground-truth strings even when small fonts in the raster blur),
  `mergeJSON(ndjson)`, `parseJSON`, `toJSON`, `urlencode`,
  `stripDataURIs`, `truncate`, `flattenItems`, `uniqueByKey`,
  `joinPath(parts...)`, `wordCount(text)` (returns int — for prompts
  that need authoritative word counts instead of model self-estimates,
  e.g., mode-switched edit passes), `mulInt(n, mult)` (int × float →
  int, for derived numeric targets in prompt prose), `addInt(a, b)`
  (int arithmetic across nested template ranges; Go templates lack
  native arithmetic), `splitSentences(text, maxChars)` (JSON array of
  greedy-packed sentence chunks under maxChars — TTS-friendly chop
  for long paragraphs that engines like Kokoro otherwise rush),
  string helpers `slugify`, `contains`, `hasPrefix`, `hasSuffix`,
  `lower`, `upper`, `trim`, `stripToHeading(text, prefix)` (drop any
  preamble before the first line starting with prefix — e.g. leaked
  reasoning before a `## ` heading), JSON-array helpers
  `filterByField(field, json)` (keep items whose field is truthy),
  `filterByValue(field, value, json)` (keep items whose field equals
  value), `joinByField(field, left, right)` (relational join of two
  `{"items":[...]}` arrays on a shared field — left items decorated
  with matched right-side fields), web-source parsers `parseSearXNG`,
  `parseWikipediaExtract`, `parseWikipediaSearch`, `parseArxiv`
  (normalize each source's response into a compact result list —
  check these before writing a new fetch-and-parse render stage),
  and prose/TTS helpers `chunkParagraphs(text, maxChars)` (greedy
  paragraph-boundary chop into `[{idx, text}]` JSON) and
  `ttsNormalize(text, rulesPath)` (apply pronunciation-normalization
  rules, defaults + optional rules file). The full registry with WHY
  docs is `exec.go:templateFuncMap`.
- **Concat WAVs.** `Stage.ConcatWavs` on an `ffmpeg` stage auto-globs
  all `*.wav` files, creates a concat list, and merges into the output
  MP3. Implemented in `ffmpeg_executor.go:executeConcatWavs`.
- **M4B audiobook mode.** `Stage.M4BFrom` / `M4BVar` / `M4BFile` /
  `M4BChapter` on an `ffmpeg` stage drive a chapterised audiobook
  build: read the upstream JSON-array stage to determine chapter
  order, ffprobe each per-chapter MP3 for duration, write an
  FFMETADATA chapter table + concat list, and run one ffmpeg
  invocation producing an Apple-Books-readable `.m4b` with embedded
  chapter navigation. `CoverImage` (also valid on `pandoc` stages)
  embeds the audiobook art / EPUB cover. Cache key folds in the
  chapter file template, chapter titles, and cover bytes so an M4B
  with empty FFmpegArgs doesn't collide with a concat_wavs entry.
- **Pandoc stage.** `type: pandoc` shells out to pandoc (docker
  `pandoc/core` image by default, override with `binary:`). Fields:
  `source_file`, `pandoc_from`, `pandoc_to`, `pandoc_metadata`
  (map of `--metadata key=value`), `pandoc_args` (raw extra args),
  `cover_image` (rendered as `--epub-cover-image`). Used today for
  markdown → EPUB study-guide generation.
- **Mix stage.** `type: mix` reads a structured-script JSON
  (`script_file`) of ordered voice-segment paths plus optional
  `intro_music` / `outro_music` / `cover_image` / chapters, and runs
  one ffmpeg invocation to concat the segments, loudnorm to -16 LUFS
  (override with `loudness_target`), and encode an audiobook/podcast
  file: `.m4b`/`.m4a` (AAC + attached-pic cover + faststart) or `.mp3`
  (libmp3lame, no cover). `metadata` keys become container tags.
- **Short stage.** `type: short` is the video analog of `mix`: a
  `script_file` JSON of shots (clip + voiceover + optional caption)
  becomes one vertical MP4 via a single ffmpeg invocation. Per shot it
  fits the clip to the voiceover duration (freeze-last-frame `tpad` by
  default, or `short_stretch_video` to time-stretch), scales/crops to
  the vertical target (`short_width`/`short_height`/`short_fps`,
  default 1080×1920@30), burns the caption via drawtext `textfile=`,
  then concats every shot, loudnorms, and optionally ducks an optional
  `background_music` bed under the voice.
- **InputSpec requires struct form.** Bare strings like
  `lesson_root: "~/path"` are rejected. Use `lesson_root: {default: "~/path"}`.
- **`chat_template_kwargs` passthrough.** Text-stage `params` accepts
  `chat_template_kwargs` (typically `{enable_thinking: false}`),
  forwarded by tabbyAPI / vLLM / SGLang to the model's chat template;
  ignored by llama-server. Use to silence Qwen3's verbose CoT preamble
  on strict-JSON stages — the model otherwise eats `max_tokens` before
  emitting a single brace. Keep CoT *on* for stages whose quality
  benefits (planning / editing). The allowlist is in
  `pipeline.go:knownTextParamKeys`.
- **`free_memory_after` on ComfyUI stages.** Set on the DSL with
  `.FreeMemoryAfter()` (Go) or `free_memory_after: true` (YAML) to
  POST /free after a successful workflow. Best-effort + non-fatal —
  weights unload + VRAM reclaim happens before the next pipeline run.
  Use on pipelines that issue a single image_gen stage per run and
  need the GPU back for a downstream LLM activation (e.g. a cover-image
  stage freeing VRAM before the next long-form text stage).
- **Auto-ensure RequireService.** `vamp run` (and any binary that
  mounts the same runCmd) probes every declared `RequireService` URL
  pre-run and auto-runs `vibe start <name>` for any unreachable
  service whose setup_hint matches that exact shape. Disable with
  `--no-ensure-services`; the legacy 3-second-retry-then-fail-with-
  hint path still works behind the flag.
- **`vamp lint` is the advisory layer.** Four checks today: webhook
  URLs on loopback hosts must have a matching `RequireService`
  declaration; text stages with `output_format: json` must include
  `"invalid_output"` in their `Retry.RetryOn` list; trivial Retry
  blocks (`MaxAttempts < 2` is a no-op); capabilities referenced but
  missing from `CapabilityModelHints`. Exit code 0
  regardless of findings — lint is editorial, not gating. New checks
  go in `internal/vamp/cli/cmd_lint.go` next to the existing four.

## Detach / job lifecycle

- `vamp run --detach` re-execs the current binary with the hidden
  `--internal-run-job` flag in a fresh session (`Setsid`), with stdin
  redirected to `/dev/null`.
- `os.Stdin` in the detached worker is `/dev/null`, which IS a
  character device. Do not use `info.Mode() & os.ModeCharDevice` to
  detect a TTY — use `isatty.IsTerminal(os.Stdin.Fd())`. (Bug fixed in
  86db9b8 because of this trap.)
- **`runs` is the single run noun.** History and live detached jobs are
  one surface: `vamp runs {ls,show,cancel,cleanup}`. `runs ls` derives a
  `STATE` column per dir from the pid file (`vamp.JobStateFor`); `runs
  show` overlays live pid/state via `FindJobByPrefix`; `runs cancel`
  reuses `runCancel`. `vamp jobs` is a hidden deprecated alias whose
  subcommands delegate to the same `runs*Cmd` constructors — don't add
  new behavior under `jobs`. The data layer (`jobs.go`: `ListJobs`,
  `FindJobByPrefix`, `JobState`) is unchanged.
- **Run-targeting commands take an `<id-or-prefix>`.** `runs show`,
  `runs cancel`, `logs`, `confirm`, `diff`, and top-level `cancel` all
  resolve via `FindRunByPrefix`/`FindJobByPrefix` (path-shaped args work
  too) and render lookup failures through the shared `renderLookupErr`;
  they tab-complete via `completeRunIDs`. Keep new run-targeting
  commands on that path rather than taking a raw run-dir.

## Profile authoring / first-run guards

- **`vibe profile new` is canonical** (name positional, `--kind` via
  flag with completion over every bundled template, `--frontend` sugar
  for llama-server). `vibe profile init` is a hidden deprecated alias.
  `--kind` values are derived from `profile_templates/*.yaml` via
  `profileKinds()` — add a template file and it shows up automatically.
- **`REPLACE-` is a hard gate.** `profile.Validate` re-marshals the
  parsed profile (dropping `# REPLACE:` comments) and rejects any
  surviving `REPLACE-` value, so an unedited starter fails to load with
  a clear message instead of a downstream file-not-found. Template
  placeholder VALUES must use the `REPLACE-...` form (with hyphen);
  explanatory comments use `# REPLACE: ...` and are exempt.
- **Read-only commands never spawn the daemon.** `vibe list` reads the
  profiles dir directly; `vibe ps` / `vibe env` ping the daemon and
  report "not running" rather than `ensureDaemon`. Only `start` / `run`
  / `pull` / `stop` / `logs` may auto-spawn.
- **`vibe doctor`: a check that cannot apply returns NOT-APPLICABLE —
  never a pass and never a fail.** Every check in `cli/cmd_doctor.go`
  answers `(checkResult, bool)` where `ok=false` means "nothing on this
  box needs this" and the runner appends nothing at all;
  `checkRSVGForVision` and `checkDockerForProfiles` are the pattern.
  `checkLlamaBinary` and `checkLlamaVersion` broke it and hard-failed a
  `cloud_peer`-only laptop — and comfyui-only and mlx-only boxes — over a
  `llama-server` nothing declared. **Applicability is computed from what
  is DECLARED ON DISK, read without validation**: a def that pins its own
  `binary:` is not a `$PATH` user, and validating the YAML first would
  make the check not-applicable on a box whose GGUF is not pulled yet,
  which is exactly the box that needs it. Validation failures are a
  different check's business; using them as a gate silently disarms this
  one. (`vibe fleet doctor` is a different command with a different
  report shape and the opposite rule — it has four levels and no
  not-applicable.)

## Router / model lifecycle (llama-swap era, 2026-07-12+)

Read `docs/design/router-lifecycle.md` before touching anything in this
area — §15/§16 record what is EXECUTED and hardware-validated vs still
planned. The short version an agent needs:

- **:9000 is llama-swap, not vibe.** A systemd user unit (`llama-swap.service`)
  serves the OpenAI+Anthropic contract there and owns LLM model lifecycle
  (JIT start on request, TTL idle-unload, swap/eviction, ComfyUI as a swap
  tenant via `/upstream/comfyui`). The vibe daemon runs with
  `disable_proxy: true` (`~/.config/vibe/config.yaml`) and keeps frontends,
  services, converge, and the control plane (:9001/unix).
- **`backend.external: true`** on a backend def means vibe launches nothing
  for it: readiness is a GET on the router's `/v1/models` matching
  alias|backend_ref|name — NEVER a completion (that JIT-loads the model and
  defeats lazy loading). Stop leaves the model to the router's TTL.
  - The catalog probed is **`client_api_url` when set**, else loopback
    (`externalCatalogURL`). This is the one readiness check that is not
    about a local process — it asserts "the model a rendered frontend is
    about to request exists where that frontend will ask", so it has to
    follow the clients. Probing loopback instead failed a fleet-only model
    (a cloud peer, another cell's weights) that would have worked, and
    passed a local cell shadowing the same id. Parse the catalog with
    `internal/vibe/modelcat`, never a local `data[]` decode: a remote
    front may answer in the Ollama shape, which decodes to an EMPTY
    catalog and reads here as "serving nothing".
  - On-disk artifacts (`path`, `mmproj`, `draft_model`,
    `chat_template_file`, `binary`) are **not** stat'd for an external
    backend: the router host owns them, and requiring them locally stopped
    a laptop from carrying a profile for a model the front serves. Same
    gate as `cell:` (they share the `offBox` predicate in
    `validateLlamaServer`). Rules that hold wherever the process runs
    (jinja-before-template-file) still apply.
- **`cloud_peer` may carry a frontend.** Pointing a harness at a cloud model
  the router already serves is the whole reason a peer exists. A
  single-entry `models:` supplies `${MODEL_ALIAS}` (a peer's model ids ARE
  the router's — no def-name indirection); several leave it unset rather
  than choosing for the user. `context:` supplies `${MODEL_CONTEXT}`,
  metadata only. This does NOT give vibe an opinion about the peer's
  residency: `cloud_peer` still implies `external`, so nothing is launched,
  supervised, or VRAM-checked. Touching this means touching all of:
  `validateFrontend`, `frontendModelVars`, the frontend-activation kind
  gate in `Start`, and `Pull`'s early returns — a peer has nothing to pull,
  and `vibe start` pulls first. `profiles/omp.example.yaml` is the worked
  example, and it is EXECUTED by a test (`example_profile_test.go`) rather
  than merely read.
- **A cloud peer's catalog ids are its `cloud_peer.models` entries, NEVER
  its def name.** Every other backend kind is served under its def name, so
  code that keys a map by `def.Name` and is then looked up by CATALOG id
  works for all of them and silently misses for peers. That is not a
  hypothetical: it made `modelprobe`'s "never probe a paid peer" guard
  inert and made `fleetannounce` report every peer model as having no
  backend def. `daemon.cloudPeerModelIDs` is the reference expansion —
  reach for it (or copy its three lines) rather than assuming def name is
  an id. A fixture that names the def after its single model hides this
  bug; name them differently in tests.
  - **Those ids are also CANONICAL** (C26a). An alias equal to one is an
    unresolvable error exactly as an alias equal to a def name is, and
    `router.alias_owner` does not arbitrate it — that key settles which of
    two *alias claimants* wins, and has never had anything to say about an
    alias colliding with a canonical id. Peer ids nevertheless do **not**
    become alias claimants: `Render` emits no aliases for a peer stanza,
    so a peer that won an alias would take it off the def that would have
    served it and advertise nothing in its place. The resolver only sees
    defs, so `checkCatalogIDsUnique` walks the RENDERED config as the
    backstop — it catches a def named after a peer's model id and two
    peers listing the same id, neither of which the resolver can see.
    §3 of C26a is the second time the first half of this rule alone was
    not enough to prevent the bug.
- **Canonical model id = backend def name** (e.g. `qwen3.6-27b`); llama-server
  aliases exist for legacy client state. Alias collisions across defs are an
  error resolved by explicit ownership, not magic. **The router's catalog is
  a namespace**: every client-facing id in it — a models key, an alias, a
  peer's model — is unique by construction, and the render refuses a config
  that says otherwise.
- **llama-swap retains recent request and response BODIES in RAM, by
  default, on every cell and the front.** `captureBuffer` defaults to
  10 MB and vibe's renderer sets it nowhere, so each llama-swap holds a
  rolling FIFO window of verbatim prompts, system prompts, tool
  definitions and completions, readable at `GET /api/captures/{id}` by
  anything holding that llama-swap's API key and discoverable via
  `has_capture` on `/api/metrics/activity`. Redaction covers five HEADER
  names and nothing in the bodies. Measured against real v239 (`dd81801`)
  and v247 (`40027d6`): the route answers 401 with no key, 401 with a
  wrong one (never 403), 404 for an evicted id and 400 for a non-integer
  id, and the object is `{id, req_path, req_headers, req_body,
  resp_headers, resp_body}` with base64 bodies. It is in-process memory,
  not the store — a restart or a `-watch-config` reload empties it, and
  C7a's `store: {path:}` does nothing for it. `captureBuffer: 0` through
  `--extras` turns it off. This is true of the fleet **today**,
  independent of C25: it is a deliberate declaration either way, and no
  vibe verb may change it (C25 §4).
- **Config flow**: `~/.config/vibe/backends/*.yaml` is the source of truth;
  the llama-swap config at `~/.config/llama-swap/config.yaml` is (post-A2)
  RENDERED — regenerate via `vibe router render`, don't hand-edit. The
  Anthropic key lives in `~/.config/llama-swap/env` (0600, systemd
  EnvironmentFile).
- **Gates**: any change to the router path re-runs
  `scripts/smoke/llama-swap/run-smoke.sh` (six-client cold-start gate;
  `DELAY_S=90` for iteration, `420` for the real thing) and
  `kill-cancel-test.sh`. Client stall/timeout behavior is version-dependent —
  re-gate after client upgrades, not just server changes.
- **Fleet-control gates**: `scripts/fleetlab/lab.sh up` stands a real
  four-cell fleet (real llama-swap v239 processes, three `hosts.yaml`
  classes, both announcer shapes, a real fleetd) on localhost with
  scratch XDG dirs, so a control-plane change can be gated without the
  physical fleet. Most gates the phase docs call "needs the real fleet"
  need a second *cell*, not a second *box* — see
  `scripts/fleetlab/README.md` for the short list that genuinely needs
  metal.
- **vamp** talks to models through :9000 (streaming warm requests tolerate
  llama-swap's `reasoning_content` loading chunks) and to ComfyUI through
  `/upstream/comfyui` — never :8188 directly, or the router can't see
  in-flight work and may TTL-reap ComfyUI mid-pipeline.
- Don't re-introduce model-serving/proxy logic into the vibe daemon; the
  design's pre-agreed fallback for router gaps is a thin front shim, decided
  deliberately — not ad-hoc daemon features.

## Fleet control (node state / intent / presence, 2026-08-02+)

`docs/design/fleet-control.md` is the design; the C0–C26a execution plan
lives in `docs/design/fleet-control-plan/` (one phase = one PR, each
phase doc ends in acceptance gates that are the definition of done),
and the ranked v2 backlog is `docs/design/fleet-control-futures.md`.
Every phase in that directory is merged. The invariants an agent must not
violate while implementing or touching adjacent code: the data plane
(client → front → cell llama-swap) gains no new hop; availability is
observed, intent is declared, model residency stays llama-swap-owned;
the `DRAINED?` display state is never acted on; mutation goes through
the daemon's bearer-authed control plane, never SSH-from-a-container;
and nothing re-points a catalog id that a human did not declare (C21),
including the front's own identity (C19 — failover is manual).

Two things about that plan's paperwork, because they are how it stays
worth reading. **A gate claim is a claim about a mechanical run**, and
the plan README's owed table names the specific physical fact each
unrun gate lacks — "needs hardware" is not an answer, and neither is
conflating "not attempted" with "not possible". And when a phase's
prose and the code disagree, **the code wins, then fix the prose**: the
SIGTERM bullet above, `noFrontRoute`'s "announce-only", and C21's alias
resolution were each documented wrong for multiple phases before
somebody measured.

**This file, the plan README and `fleet-control.md` are the plan's
conflict axis** — nearly every phase wants all three, and every merge
conflict the plan has produced landed in one of them. So a phase branch
does not edit them. It writes what it wants into a
`## For the reconciliation pass` section of its OWN phase doc, and a
later single-purpose PR applies them all at once (C22, C26b). Two things
that pass keeps finding, so write the section as a draft rather than as a
record: **a reconciliation section describes the phase as it stood when
it was written, not as it merged** (C22 corrected four phases' own gate
counts), and a rule proposed for a named command does not generalise to a
command that merely shares its name (C26a's not-applicable doctor rule
versus C13's four-level one — both are in this file, deliberately, with a
sentence apiece saying which is which).

## Things to never do

- Don't add `--no-verify`, `--no-gpg-sign`, or any hook-bypass flag to
  git commands unless the user explicitly asks.
- Don't `git add .` or `git add -A` — the repo has historically pulled
  in `.claude/worktrees/` as submodule entries when this happened.
  Stage files by name.
- Don't commit `dist/`, `*.pid`, `*.log`, `*.sock` (already in
  `.gitignore` but worth saying).
- Don't bump `go.mod`'s `go` directive without also bumping the
  `golang:X.Y.Z-alpine` line in any Dockerfile that ships from this
  repo.

## Where to look

- Architecture deep-dive: `README.md` "How it works" section.
- Open work + recent history: `TODO.md`.
- Examples (real, runnable pipelines): `examples/`.
- Wire-level smoke commands: scan recent commit messages — every
  feature merge includes the smoke that verified it.
