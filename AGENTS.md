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
- **`search_url` (`~/.config/vibe/config.yaml`)** backs `${VIBE_SEARCH}`
  in frontend templates and env, so one profile points a harness at
  models, search, and fetch together. Client-facing only — nothing in
  the daemon dials it. Unset is not the same as empty: `${VIBE_SEARCH}`
  is left out of the expansion map entirely so a profile referencing it
  fails to activate with a message naming `search_url`, instead of
  rendering an empty URL into a harness config.
- **Fleet control plane (fleet-control C1+).** `docs/design/fleet-control.md`
  is the design; the phase plan (C0–C4) is
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
  - **llama-swap's SIGTERM path cancels in-flight streams
    immediately** (v239 verified: `CloseStreams()` precedes the
    graceful drain). `vibe cell drain --wait <dur>` /
    `CellDrainRequest.wait_seconds` is the quiescence wait that lets
    generations finish BEFORE the unit stops — driven by fleetapi's
    inflight SSE tracking. Unit `TimeoutStopSec` must exceed
    llama-swap's 30s internal cap.
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
    asset ONLY (the ONE middleware exemption — exact-match, GET-only,
    evaluated before mux path-cleaning, boundary test-pinned in
    daemon/fleet_registry_test.go; do NOT widen it to a prefix match or
    `path.Clean`). SSE (`/api/fleet/events`) drives debounced state
    refreshes; action buttons POST `/mcp` tools/call — never add
    mutation routes for it; if a button needs something new, the MCP
    facade is what's incomplete. `esc()` is the TEXT escaper and
    `attr()` the attribute one — they stay separate because esc()'s
    output also feeds `textContent`.
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
    route at all** — the front's peers are rendered from `hosts.yaml`,
    so a cell known only through its announces is the warm-loop analog
    of "no daemon_url" and is queued without a pointless round trip. In
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
    `warm_model` skips `chat`-class entries and still refuses the rest.
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
  - **Ladder: drained > held > stale > unreachable > policy.** Every
    rung skips, so the order decides only the reason an operator reads —
    which is why it is written down here and in the phase doc.
  - **Bounded and self-expiring**: default 4h, max 24h (tighter than the
    lease store's 168h, because a hold disables a configured policy). A
    re-issue refreshes the same key. There is no unbounded hold.
  - **A SKIP is not an observation of emptiness** (`setWarmState` clears
    `emptySince` on `skipped`): every skip returns before residency is
    read, so banked emptiness let the first tick after a hold or a drain
    ended fire the empty-restore against a mid-cold-start model — the
    live race C4's grace window exists to prevent.
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
- **Canonical model id = backend def name** (e.g. `qwen3.6-27b`); llama-server
  aliases exist for legacy client state. Alias collisions across defs are an
  error resolved by explicit ownership, not magic.
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
- **vamp** talks to models through :9000 (streaming warm requests tolerate
  llama-swap's `reasoning_content` loading chunks) and to ComfyUI through
  `/upstream/comfyui` — never :8188 directly, or the router can't see
  in-flight work and may TTL-reap ComfyUI mid-pipeline.
- Don't re-introduce model-serving/proxy logic into the vibe daemon; the
  design's pre-agreed fallback for router gaps is a thin front shim, decided
  deliberately — not ad-hoc daemon features.

## Fleet control (node state / intent / presence, 2026-08-02+)

`docs/design/fleet-control.md` is the design; the C0–C4 execution plan
lives in `docs/design/fleet-control-plan/` (one phase = one PR, each
phase doc ends in acceptance gates that are the definition of done).
The invariants an agent must not violate while implementing or
touching adjacent code: the data plane (client → front → cell
llama-swap) gains no new hop; availability is observed, intent is
declared, model residency stays llama-swap-owned; the `DRAINED?`
display state is never acted on; mutation goes through the daemon's
bearer-authed control plane, never SSH-from-a-container.

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
