# C12 — Guest read-only token: sharing status without sharing drain

Status: IN REVIEW (2026-08-04), PR pending. Unit gates 1–12 green on a
full local inner loop (`-race -count=5 ./...`, `golangci-lint run` 0
issues, `gofmt -l .` silent, `go mod tidy` clean); the 3 live gates need
a real fleet and a real phone and are **NOT RUN** — the implementing
environment cannot reach the fleet (SSH blocked, the LAN does not
route).

Backlog item 6 in [fleet-control-futures.md](../fleet-control-futures.md)
§2, one sentence long:

> **Guest read-only token** — a second bearer honored only on
> `GET /api/fleet/state` + `/events` (interim: reverse-proxy path
> allowlist, zero code). Sharing status today means sharing drain.

## The friction

The fleet has exactly one credential. Whoever can answer *"is the GPU
box busy?"* can also drain it, wake it, unload the model somebody is
mid-batch on, declare the fleet away so alarms stop being delivered,
read the usage ledger (who ran what, when, how much) and read the
savings screen (what the hardware cost and what the household spends on
electricity).

The question that actually gets asked is the smallest one on that list,
and it is asked by people who must never hold the rest of it: the
housemate wondering whether the machine that heats the office is
working, a friend borrowing capacity for an afternoon, and — the case
that made this an item — the operator's own phone, which stores whatever
it is given in `localStorage` on a device that leaves the house and gets
handed to other people.

Today that is answered either by not sharing at all, or by sharing the
control-plane token and hoping. Those are the same answer: an
access-control question settled by trust.

## What this is not

Not a user system. No accounts, no sessions, no roles beyond the two
that exist, no per-cell scoping, no expiry, no audit trail of who read
what. One extra bearer with a fixed, tiny, enumerated grant, off unless
an operator turns it on. Anything more is a different phase and probably
a different product.

## Design

### 1. The grant is two routes, and the rest of the surface is the design

The guest token is honored on exactly:

| method | path | why |
|---|---|---|
| `GET` | `/api/fleet/state` | the derived-state table: is the cell up, what is resident, what is declared |
| `GET` | `/api/fleet/events` | the SSE stream that makes the page live instead of a 30s poll |

Everything else 401s. Enumerated, because "everything else" is the part
that has to stay true as this plan keeps adding routes:

| denied to a guest | what it would grant |
|---|---|
| `POST /mcp` | the entire MCP facade: `drain_cell`, `resume_cell`, `wake_cell`, `warm_model`, `unload_model`, `render_front`, `probe_model`, `hold_model`, `release_hold`, `fleet_notify_scope`, `fleet_notify_test`, `fleet_savings` |
| `POST /vibe.v1.ControlService/*` (the Connect mount) | every daemon RPC — `Start`, `Stop`, `Pull`, `CellDrain`, `CellResume`, `Shutdown` |
| `POST /api/fleet/intent` | writing axis 2 for any cell |
| `POST /api/fleet/wake` | a magic packet at any cell, on demand |
| `POST /api/fleet/announce` | **the cell voice.** Design §6's threat note: a forged announce can fake SERVING, prune a roaming catalog, or cancel a pending drain. A guest bearer that reached this route would be a fleet-wide write dressed as a read |
| `GET /api/fleet/leases`, `POST`/`DELETE /api/fleet/lease` | reading and *deleting* other people's declarations, C11 holds included |
| `POST /api/fleet/notify/scope`, `/notify/send` | muting the pager, or using it as an outbound message relay |
| `GET /api/fleet/usage` | see below |
| `GET /api/fleet/savings` | see below |

**Usage and savings are denied, deliberately, and this is the one
judgement call in the phase.** Both are read-only GETs, so the reflex is
that a read-only token should carry them. It should not:

- `/api/fleet/usage` is a **behavioural log of the household**. Tokens
  per cell, per model, per **day** is a record of when this house works,
  which model it works with, and — by the shape of the empty days — when
  it was away. C9 gates alarm *delivery* on an away declaration precisely
  because absence is sensitive; handing a guest the ledger hands them the
  same fact at day resolution, forever, without a declaration.
- `/api/fleet/savings` is money and hardware: what the boxes cost
  (`capital_cost`), what the electricity costs, a payback percentage.
  It is the most *quotable* screen in the fleet and the least
  status-shaped. The savings screen exposes more about the household
  than cell status does, which is the reason it exists — and the reason
  a guest does not get it.

The line this draws is: a guest may see **what the fleet is doing right
now**, not **what the household has done over time or spent doing it**.
State and events are instantaneous; usage and savings are history. If a
later phase wants a shareable savings figure, the honest form is a
deliberately published artifact (a rendered number), not a guest bearer
on the endpoint that computes it.

**What a guest DOES see is the whole state document, not a filtered
one.** The grant is a route grant, never a field grant. `/api/fleet/state`
carries cell names and URLs, model ids and probe verdicts, display
states, **intent reasons and ETAs**, **lease holders and notes**,
`daemon.active_profile`, and the warm/probe/notify status blocks — a
guest reads all of it. Two reasons not to fork the document:

1. C9's rule is that the pager and the page render the same snapshot, so
   they cannot disagree. A guest-shaped variant is a second rendering of
   fleet state with its own drift, and every future field would have to
   be classified twice.
2. A field filter is a denylist wearing a hat. Routes get an allowlist
   here because a forgotten route is a granted capability; a forgotten
   *field* on a document already granted is not a new capability, and
   maintaining a per-field allowlist across five nested structs is
   exactly the hand-maintained list this phase's tests exist to abolish.

The operational consequence is one sentence and it belongs in the
operator's head before they issue a token: **anything visible on the
fleet page is visible to a guest**, including the free text an operator
types into `--reason` and `--note`. Write those like a shared calendar
entry.

### 2. Enforcement is a positive allowlist, and the route table is the registry

A denylist ("everything except these paths") silently grants every route
added after it is written, and this plan has added routes in eight of
its twelve phases. So the check is positive: **a request carrying the
guest token is served only when `(method, path)` is an exact entry in a
declared allowlist, and refused otherwise** — unknown paths, unknown
methods, and every route this repo grows next included.

Two properties, both inherited verbatim from C5's `/ui/fleet` exemption
because that exemption is the strictest thing in the file:

- **Exact match on `r.URL.Path`, evaluated before the mux cleans
  anything.** Not a prefix, not `path.Clean`, not a regexp. So
  `/api/fleet/state/`, `/api/fleet/state/../usage`, `/api/fleet/state%2f`,
  `//api/fleet/state` and `/api/fleet/state/anything` are all misses,
  and a miss is a 401.
- **Method-exact.** `POST /api/fleet/state` is a miss. Go's server
  passes the method through verbatim, so a lowercase `get` is a miss
  too.

The list itself is not hand-maintained beside the routes; it is
**derived from them**. `internal/vibe/fleetapi/routes.go` holds one
table that is simultaneously (a) what `Register` mounts and (b) what
each route grants:

```go
{http.MethodGet, "/api/fleet/state", AccessGuest, ...},
{http.MethodPost, "/api/fleet/intent", AccessTokenOnly, ...},
{http.MethodGet, "/ui/fleet", AccessPublic, ...},
```

`Access` has **no safe zero value**: `AccessUndecided` is what a new
entry gets by forgetting, and a test fails on it (gate 2). The
middleware asks `fleetapi.AccessFor(method, path)` and treats
everything it does not recognise — every non-fleetapi route, `/mcp` and
the whole Connect mount included — as `AccessTokenOnly`. Deny by
default at the lookup, decided explicitly at the table.

Folding C5's exemption into the same table is the other half of the
mechanism, and it does not widen it: `AccessPublic` is still one entry,
still `GET`, still exact-match, still evaluated first, and the six
bypass attempts C5 pinned still 401. What changes is that the page's
exemption and the guest's allowlist can no longer drift apart, and a
future route cannot be made public or guest-visible without editing the
one line that also mounts it.

The rejected alternative here is the backlog entry's own interim: a
**reverse-proxy path allowlist**. It is genuinely zero code and it stays
a valid deployment (an operator who fronts fleetd with Caddy can do
this today). It is not what ships, because the allowlist would live in a
file nobody edits when a route is added, in a repo this one cannot test,
and the whole difficulty of this phase is keeping the allowlist true
across future phases. A second listener bound to a different port was
rejected for the same reason plus a worse one — it needs a second mux,
and then routes get registered twice.

### 3. The token: where it lives, how it is compared, where it must never appear

`fleet.guest_token_file: <path>` in `config.yaml`. **The path is
config; the value never enters a repo** — the same shape as
`fleet.token_file` and C9's `notify.url_file`, and the boundary rule
(ground rule 3) in the one place it most obviously applies. There is no
inline `guest_token:` form: one way to hold a credential is one place to
leak it.

- **Unset is off.** No file is read, no token exists, the middleware is
  byte-for-byte the pre-C12 one, and a fleet that never configures a
  guest behaves exactly as today. This is the default and it is tested
  as a default (gate 1).
- **Set and the file is missing: the daemon mints one** (32 random
  bytes, base64url, 0600, atomic tmp+rename — `writeTokenFile`, the same
  writer the control-plane token uses) and logs `guest read-only token
  CREATED (new)` with the path. Naming the path *is* the opt-in; making
  the operator also invent a random string invites `guest123`.
- **Compared with `crypto/subtle.ConstantTimeCompare`**, like the
  control-plane token, and only after the control-plane comparison
  fails. Both comparisons are constant-time; the early return on a
  control-plane match leaks only which of two secrets was presented,
  which the response body already tells the holder.
- **Never logged, never in an error string, never in a status
  document, never on the page.** The daemon logs the *path* and the
  created/loaded distinction (C1's rule — a token file that silently
  re-minted is the bug that costs an afternoon). `fleet_status` and
  `/api/fleet/state` gain `daemon.guest_enabled` (a boolean) and
  `daemon.guest_rejected` (a count) and nothing else.

**Every misconfiguration fails CLOSED — the daemon starts, guest access
does not.** The ladder, each rung a `slog.Error` naming the path and the
reason:

| condition | result |
|---|---|
| file empty or whitespace | disabled |
| token shorter than 16 chars | disabled |
| token contains whitespace or control characters | disabled (it cannot survive an `Authorization` header intact) |
| **token equals the control-plane token** | disabled |
| mint failed (read-only mount, unwritable dir) | disabled |

The fourth rung is the one that matters. An operator who copies the
control-plane token into the guest file has built a guest credential
that grants drain, and every test in this phase would still pass because
the request would authenticate on the *first* comparison. Refusing to
enable guest access is the only failure direction that cannot
over-grant.

Refusing to *start* was considered and rejected: fleetd is
read-and-request-only (invariant 4), and killing the fleet's
observability because a shared read token was mis-mounted trades a real
outage for a feature nobody has yet used.

### 4. A denial is a 401, and it is counted separately

A valid guest token on a denied route gets **401**, not 403, with the
same `WWW-Authenticate` challenge every other rejection carries. One
status code for "this credential does not open this door" means a
guest token cannot be used to map which routes exist versus which are
merely forbidden. The *body* differs (`read-only token: not permitted on
this path`) because the alternative is telling an operator their working
token is invalid, and the holder learning their own token is a guest
token learns nothing they did not have.

The count goes to a **new** counter, `daemon.guest_rejected`, and not to
`daemon.auth_rejected`. C1 built `auth_rejected` to answer exactly one
question — *"is a client somewhere holding a stale token?"* — and
`vibe cell status` prints it as that sentence. A guest tapping a button
that is not theirs is not a stale token, and folding those in would
degrade a working signal into noise on the exact day someone shares a
guest link. Wrong and missing tokens keep counting into `auth_rejected`
unchanged.

### 5. The page under a guest token

The page is bearer-exempt (C5), so it loads for anyone; its two data
calls are the two guest routes, so it renders. Its action buttons POST
`/mcp` and its savings view GETs `/api/fleet/savings` — both 401 for a
guest, and the page's existing 401 handler pops the **token gate**,
which tells a guest with a perfectly good token that their token is
wrong. That is the status quo this phase must not ship.

Two candidate answers: leave the buttons and let them fail loudly, or
hide actions when the token is a guest. **The page hides them**, and the
argument is that hiding is the *more* honest of the two here — a button
that exists but cannot work is a claim about the reader's authority that
the server has already contradicted. Failing loudly is only more honest
when the failure teaches something true, and "your token is invalid,
re-enter it" is false.

Hiding needs the page to know, and the constraint is that **no new route
may be added** (§ the C7b rule: a second path forces C5's exemption to
widen). The page learns from a **response header on a request it already
makes**: the middleware sets `X-Vibe-Auth: guest` on responses it
authorized with the guest token, and the page's one `api()` wrapper
reads it. Three properties made this the chosen mechanism over adding a
`viewer` field to `/api/fleet/state`:

- **`fleetapi` stays auth-unaware.** `Register`'s comment has said
  "auth is the mux wrapper's business" since C1; a per-requester field
  on the state document would move knowledge of the auth boundary into
  the package that renders fleet state, and would make the one document
  every surface shares depend on who asked for it.
- **It is not a route**, and it costs no extra request.
- **It degrades to the other answer.** If a reverse proxy strips the
  header, the page shows the buttons and a click 401s — exactly option
  one. The failure mode of the polish is the un-polished behaviour, not
  a broken page.

What the guest page renders: the cell table, the models, the badges, the
warnings, the live SSE updates, and a `read-only` chip in the header.
What it hides: the per-cell action buttons, the warm row, the notify
row's buttons (the notify *status* stays — it is state), and the savings
nav link, with the savings view replaced by one line saying it needs the
control-plane token.

**The hiding is CSS and a class, and it is a courtesy, not a boundary.**
A guest who opens devtools and POSTs `/mcp` gets a 401 from the server,
which is the only place authority is decided. The page must never be
the reason something is safe.

### 6. Rotation, revocation, and off by default

- **Rotate**: `vibe token --guest --regenerate` writes a fresh value to
  the configured path; restart the daemon. Every guest is logged out and
  must be re-given the new string.
- **Revoke everyone**: same command, or empty the file, or remove
  `guest_token_file`; restart. There is no per-guest revocation because
  there are no per-guest tokens — that is the accepted cost of a
  two-credential design, and it is stated here so nobody discovers it
  during an incident.
- **Read it to share it**: `vibe token --guest`.
- **No hot reload**, deliberately: the control-plane token is read once
  at startup and cached, and a guest token that reloaded on its own
  schedule would give the two credentials different revocation stories.
  One restart, both.

The daemon reads the file once, at start, before the listeners serve.

## Files

- `internal/vibe/fleetapi/routes.go` — **new.** The `Access` vocabulary
  (`AccessUndecided` / `AccessTokenOnly` / `AccessGuest` /
  `AccessPublic`), the route table that `Register` now iterates, the
  exact-match `AccessFor(method, path)` lookup, and `Routes()` for the
  tests that walk the registry.
- `internal/vibe/fleetapi/fleetapi.go` — `Register` becomes a loop over
  the table; `DaemonInfo` gains `guest_enabled` / `guest_rejected`.
- `internal/vibe/fleetapi/fleetpage.go` — the page handler stays,
  registration moves into the table (its `AccessPublic` entry is C5's
  exemption, now stated where the route is).
- `internal/vibe/daemon/auth.go` — `authGuard` (the two tokens and the
  two counters), the allowlist check, `LoadOrCreateGuestToken` and the
  fail-closed validation ladder.
- `internal/vibe/daemon/daemon.go` — `fleet.guest_token_file`, the
  startup load, the counters on `DaemonInfo`.
- `internal/vibe/cli/cmd_token.go` — `--guest`, composable with
  `--regenerate`.
- `internal/vibe/cli/cmd_cell.go` — one line in `vibe cell status` when
  guest access is on.
- `internal/vibe/fleetapi/fleet.html` — guest mode: the header chip, the
  hidden actions, the savings note.

## Acceptance gates

1. **Off by default (unit).** A daemon with no `guest_token_file` mints
   no file, reports `guest_enabled` false, and answers 401 to every
   route for every token that is not the control-plane one — including a
   token that would have been a valid guest token had the feature been
   on. `TestGuestToken_UnconfiguredIsOffAndMintsNothing`.
2. **Every route declares its access (unit, registry-completeness).**
   Walking `fleetapi.Routes()`: no entry may be `AccessUndecided`, no
   `(method, path)` may repeat, and `AccessFor` must agree with the
   table for every entry. A route added without deciding fails this.
   `TestRoutes_EveryRouteDeclaresAnAccessLevel`.
3. **The table is the only registrar (unit, source grep).** No file
   under `internal/vibe/fleetapi` may call `mux.HandleFunc` /
   `mux.Handle` outside `routes.go`, so a route cannot be mounted while
   bypassing the access decision. Same shape as C7a's
   `Truncate(24*time.Hour)` grep.
   `TestRoutes_NoHandlerIsRegisteredOutsideTheTable`.
4. **The guest and public surfaces are pinned (unit, tripwire).** The
   guest surface is exactly `{GET /api/fleet/state, GET
   /api/fleet/events}` and the public surface is exactly
   `{GET /ui/fleet}`. Widening either is then a deliberate edit to a
   test that names this doc.
   `TestRoutes_GuestSurfaceIsExactlyStateAndEvents`,
   `TestRoutes_PublicSurfaceIsExactlyTheFleetPage`.
5. **The whole registry is probed with a guest token (integration).**
   Against a real daemon on a real TCP listener, for **every** entry in
   `fleetapi.Routes()`: `AccessGuest` entries answer non-401,
   `AccessPublic` answers 200, everything else answers **401**. Derived
   from the registry, so a route added by a future phase is probed
   without anyone remembering to add it here.
   `TestGuestToken_DeniedOnEveryRouteItDoesNotName`.
6. **The non-fleetapi surfaces are probed too (integration).**
   `POST /mcp` (initialize *and* a `tools/call`), the Connect RPC mount,
   and two paths that do not exist, all 401 with the guest token.
   `TestGuestToken_ReachesNeitherMCPNorTheRPCs`.
7. **The boundary is exact-match and method-exact (integration).** With
   the guest token: `POST /api/fleet/state`, `GET /api/fleet/state/`,
   `GET /api/fleet/state/../usage`, `GET /api/fleet/state%2f`,
   `GET //api/fleet/state`, `GET /api/fleet/state/sub` and
   `GET /api/fleet/statex` all 401 — the same six shapes C5 pinned for
   `/ui/fleet`, applied to the new hole.
   `TestGuestToken_AllowlistIsExactMatchAndGETOnly`.
8. **C5's exemption is unweakened (integration).** The pre-existing
   `TestDaemon_FleetRegistry_Role` bypass battery passes untouched, and
   its list grows the two guest routes without a token (they must still
   401 — the guest grant is a *token* grant, not an anonymous one).
9. **The fail-closed ladder (unit).** Each of empty file, short token,
   whitespace in the token, and **a guest token equal to the
   control-plane token** leaves guest access disabled with the daemon
   still serving; the equal-token case additionally proves the value
   still works as the control-plane token.
   `TestGuestToken_MisconfigurationFailsClosed`.
10. **The credential does not leak (unit).** The guest token appears in
    no log line, no error string, no `/api/fleet/state` body, and not in
    the page. Asserted by scanning a captured `slog` buffer, the
    rendered state JSON and every error returned by the loader for the
    token value.
    `TestGuestToken_NeverAppearsInLogsErrorsOrState`.
11. **Counters split (integration).** A wrong token increments
    `auth_rejected` and not `guest_rejected`; a valid guest token on
    `/mcp` increments `guest_rejected` and not `auth_rejected`.
    `TestGuestToken_RejectionsCountSeparately`.
12. **Streaming contract + full inner loop (mechanical).**
    `git diff --stat main..HEAD -- internal/vibe/proxy` is empty;
    `go build ./...`, `go vet ./...`, `gofmt -l .` silent, `go mod tidy`
    clean, `go test -race -count=5 ./...`, `golangci-lint run` — plus
    ground rule 9's adversarial self-review as its own commit.

### Live gates (need a real fleet; NOT runnable from the implementing environment)

L1. **The hallway test.** Configure `guest_token_file` on fleetd,
    restart, hand the guest token to a phone browser at
    `http://<fleetd>:9001/ui/fleet`. The table renders and updates live;
    there are no action buttons, no warm row and no savings tab; the
    header says read-only. Then paste the control-plane token in the
    same browser and confirm the full page returns.
L2. **The boundary in the field.** With the guest token in `$VIBE_TOKEN`,
    `vibe cell status` fails 401 (it reads `/api/fleet/state` — confirm
    which half fails and that the failure is legible), `curl -H` on
    `/api/fleet/usage`, `/api/fleet/savings` and `/mcp` all 401, and
    `fleet_status` shows `guest_rejected` rising by exactly the number of
    attempts.
L3. **Rotation.** `vibe token --guest --regenerate`, restart fleetd, and
    confirm the phone that had the old token gets a 401 and the token
    gate, and works again with the new value.

## Out of scope (deliberately)

- **Per-guest tokens, expiry, or revocation of one guest.** Two
  credentials, one grant. A third would need a store, and a store needs
  a lifecycle.
- **A guest-shaped state document** (§1). The grant is a route grant.
- **Guest access to usage or savings** (§1), and no "aggregate-only"
  variant of either — a rounded number is still the shape of the
  household's week.
- **Rate limiting the guest.** A guest can hold `/api/fleet/events` open
  like any authed client can; fleetd is read-and-request-only and an
  abusive guest costs an SSE subscriber slot, not a request path. If
  that ever matters it is a fleetd-wide concern, not a guest one.
- **Hot-reloading the token file** (§6).
- **Anonymous read.** The page is public because it contains no fleet
  data; the data always needs a bearer, guest included.
- **Guest tokens on cells.** The mechanism is per-daemon and would work
  on a cell, but the fleet's status question is answered by fleetd; a
  cell's `/api/fleet/state` is a one-cell registry and sharing it is not
  the friction this phase names.

Estimated ~250 lines + tests, on the plan's calibration (C0–C4 ran
3.6–4.5× their estimates).
