# C12 — Guest read-only token: sharing status without sharing drain

Status: PR OPEN (2026-08-05), feature + self-review + **adversarial
review** commits. Unit gates 1–14 (plus 11b) green on a full local inner
loop (`go build`, `go vet`, `go test -race -count=5`, `golangci-lint
run` 0 issues, `gofmt -l .` silent, `go mod tidy` clean); the 3 live
gates need a real fleet and a real phone and are **NOT RUN** — the
implementing environment cannot reach the fleet (SSH blocked, the LAN
does not route). The author's self-review pass found 5 items; the
separate adversarial pass (ground rule 9) found **7 more — one of them a
BLOCKER that rotated the control-plane token** — 5 fixed with
mutation-verified regression tests, 2 documented. See the two addenda at
the end.

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
| `GET /api/fleet/leases`, `POST`/`DELETE /api/fleet/lease` | *claiming and deleting* other people's declarations, C11 holds included. Note what this does **not** withhold: `cells[].leases` rides `/api/fleet/state`, so a guest already reads every holder, model and note. Denying the route denies the WRITE half and the whole-fleet listing, not the fact of a hold |
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

**And one consequence that deserves its own paragraph, because this
section's own argument points the other way** (review finding 6). The
notify block in the state document carries `scope`, `scope_since`,
`scope_until` and `scope_reason` — so `vibe fleet notify --away --until
2026-08-20 --reason "back from Spain the 20th"` is guest-readable, as an
explicit statement that the house is empty and when it stops being
empty. The case for denying `/api/fleet/usage` above is precisely that
absence is sensitive and the ledger discloses it *by inference, at day
resolution*; the away declaration discloses it outright, with an end
date, on a route this phase grants. That is not a reason to fork the
document (the two reasons against still hold) — it is a reason for the
operator to know it, and for `--reason` on an away declaration to be
written for the same audience as a shared calendar entry. It is listed
under *Known and accepted* rather than fixed.

**The CLI comes along for free, and that is intended.** `vibe cell
status` reads exactly `/api/fleet/state`, and `vibe cell await`'s wait
reads `/api/fleet/state` + `/api/fleet/events`, so both work with
`VIBE_TOKEN` set to the guest value. Everything that acts —
`drain`/`resume`/`wake`/`hold`, an intent post, a lease claim — 401s.
The guest surface is a property of the two routes, not of the browser.

### 2. Enforcement is a positive allowlist, and the route table is the registry

A denylist ("everything except these paths") silently grants every route
added after it is written, and this plan has added routes in eight of
its twelve phases. So the check is positive: **a request carrying the
guest token is served only when `(method, path)` is an exact entry in a
declared allowlist, and refused otherwise** — unknown paths, unknown
methods, and every route this repo grows next included.

Two properties, both inherited verbatim from C5's `/ui/fleet` exemption
because that exemption is the strictest thing in the file:

- **Exact match on the RAW request path, evaluated before the mux
  cleans anything.** Not a prefix, not `path.Clean`, not a regexp, and
  not decoded. So `/api/fleet/state/`, `/api/fleet/state/../usage`,
  `/api/fleet/state%2f`, `//api/fleet/state`, `/api/fleet/%73tate` and
  `/api/fleet/state/anything` are all misses, and a miss is a 401.
- **Method-exact.** `POST /api/fleet/state` is a miss. Go's server
  passes the method through verbatim, so a lowercase `get` is a miss
  too.

A request that fails the check never reaches a handler, and a request
that passes it cannot be routed anywhere except the entry it matched.

*(Corrected 2026-08-05, `fix/live-gate-truth`. This section said the
middleware "decides on the same string the mux routes with
(`r.URL.Path`), and every place the two can diverge … diverges in the
strict direction", and the code passed `r.URL.Path`. But `r.URL.Path` is
the DECODED path — net/url runs before any of this, and
`url.ParseRequestURI("/ui/%66leet")` yields `URL.Path == "/ui/fleet"`
with `RawPath == "/ui/%66leet"` — so percent-encoding an ordinary
character diverged in the LOOSE direction: the middleware granted
`/ui/%66leet` and `/api/fleet/%73tate` as if they were the declared
routes. **Nothing was reachable that was not already reachable**, and it
is worth being precise about why rather than overstating it: Go's
ServeMux also routes on the decoded path, so middleware and router
agreed on the target, and the allowlist is positive and exact, so a
decoded match could only ever re-grant a route that string already
granted. The defect is that a load-bearing security invariant was stated
falsely — and it stops being harmless the moment anything routes on
`RawPath` or the mux's matching changes. The middleware now reads
`r.URL.EscapedPath()`, which is what "RAW" always claimed; the encoded
spellings are pinned in `daemon/authpath_test.go`.)*

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
| **the path IS the control-plane token file** | disabled, checked *before the file is read* |
| file empty or whitespace | disabled |
| token shorter than 16 chars | disabled |
| token contains whitespace or control characters | disabled (it cannot survive an `Authorization` header intact) |
| **token equals the control-plane token** | disabled |
| mint failed (read-only mount, unwritable dir) | disabled |

The value rung is the one that matters. An operator who copies the
control-plane token into the guest file has built a guest credential
that grants drain, and every test in this phase would still pass because
the request would authenticate on the *first* comparison. Refusing to
enable guest access is the only failure direction that cannot
over-grant.

The **path** rung above it exists because the value rung cannot be the
only one (review finding 2). `LoadOrCreateGuestToken` *mints into a
missing file* — so `guest_token_file: <the control-plane token file>`
with that file absent writes a fresh random value over the control-plane
token before any comparison happens, and the log calls it a guest token
being created. Today `Run` loads the control-plane token first, so the
file always exists by then and the value rung happens to catch it; that
is an ordering, not a guard. `daemon.IsControlTokenFile` compares by
inode (`os.SameFile`, so symlinks and two spellings of one path are the
same file) and falls back to a cleaned-string compare when the file does
not exist — which is exactly the case the inode compare cannot see.

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

  *Amended by review finding 3.* That is only true if the 401 stops at
  the button. The feature commit's `rpc()` went through the same `api()`
  wrapper that pops the token gate, so the documented degradation
  degraded to **the exact behaviour this section says must not ship** —
  a guest with a working token told it is invalid. A `/mcp` 401 is
  genuinely ambiguous (a stale control-plane token and a guest past
  their grant look identical), so it no longer gets to decide:
  `rpc()` is quiet, the button flashes `failed`, and `flash()`'s 1.5s
  `refresh()` puts the verdict on `/api/fleet/state` — the one route
  that can tell the two apart, and which still pops the gate for a token
  that really is dead.

What the guest page renders: the cell table, the models, the badges, the
warnings, the live SSE updates, and a `read-only` chip in the header.
What it hides: the per-cell action buttons, the warm row, the notify
row's buttons (the notify *status* stays — it is state), and the savings
nav link, with the savings view replaced by one line saying it needs the
control-plane token.

**The hiding is one flag and a body class, and it is a courtesy, not a
boundary.** A guest who opens devtools and POSTs `/mcp` gets a 401 from
the server, which is the only place authority is decided. The page must
never be the reason something is safe. The flag is set in BOTH
directions (review finding 2): pasting the control-plane token into a
tab that browsed as a guest gives the buttons back on the next state
fetch, with no reload.

**What a guest read costs, honestly**: `GET /api/fleet/state` is an
uncached probe round of every cell, and it refreshes fleetd's
last-seen bookkeeping. It touches no data plane and mutates no declared
intent, but it is not free, and a bored guest with a refresh key is more
load than a bored operator with one.

### 6. Rotation, revocation, and off by default

- **Rotate**: `vibe token --guest --regenerate` writes a fresh value to
  the configured path; restart the daemon. Every guest is logged out and
  must be re-given the new string. Both `--guest` forms refuse outright
  when `guest_token_file` is the control-plane token file (review
  finding 1): printing it would hand out the control-plane token under a
  banner that says *share this*, and regenerating it would rotate the
  control-plane token — every client 401s — from a command whose name
  says guest.
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
   on. `TestGuestToken_UnconfiguredIsOffAndMintsNothing` (the mint and
   the state fields) plus
   `TestGuestToken_RoutesAreProbedAgainstAnUnconfiguredDaemonToo`, which
   walks `fleetapi.Routes()` for the "every route" half — the original
   test probed one (review finding 7).
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
   `TestGuestToken_AllowlistIsExactMatchAndGETOnly`. Percent-encoded
   spellings of a declared route (`/ui/%66leet`, `/api/fleet/%73tate`)
   join the battery from 2026-08-05 —
   `TestAuth_PercentEncodedSpellingsAreNotTheDeclaredRoute`; see the
   [live-gate addendum](#live-gate-addendum-2026-08-05-raw-meant-decoded).
8. **C5's exemption is unweakened (integration).** The pre-existing
   `TestDaemon_FleetRegistry_Role` bypass battery passes untouched, and
   its list grows the two guest routes without a token (they must still
   401 — the guest grant is a *token* grant, not an anonymous one).
9. **The fail-closed ladder (unit + integration).** Each of empty file,
   short token, whitespace in the token, and **a guest token equal to
   the control-plane token** is refused by the loader:
   `TestGuestToken_MisconfigurationFailsClosed`. Against a running
   daemon, a refused file opens nothing and leaves the registry serving
   (`TestGuestToken_RefusedFileOpensNothing`), and the equal-token case
   leaves the copied credential working as the control-plane token
   everywhere (`TestGuestToken_IdenticalToControlPlaneFailsClosedOverHTTP`).
   The **path** rung — `guest_token_file` IS the control-plane token
   file — is refused before the file is read and never mints over it:
   `TestGuestToken_ControlPlaneTokenFileIsRefusedBeforeItIsRead`,
   `TestGuestToken_PointedAtTheControlPlaneFileNeverRewritesIt`, and on
   the operator's side `TestTokenGuest_RefusesTheControlPlaneTokenFile`.
   *(The three integration tests and both path tests are review
   findings 1, 2 and 5: the original gate text claimed the equal-token
   case "proves the value still works as the control-plane token" and
   the named test made no such assertion.)*
10. **The credential does not leak (unit).** The guest token appears in
    no log line (a captured `slog` buffer, across both the loaded and
    the minted paths) and in no error string the loader returns; the
    state-document half is asserted in gate 5's test, which greps the
    guest's own `/api/fleet/state` body for both tokens.
    `TestGuestToken_NeverAppearsInLogsErrorsOrState`.
11. **Counters split (integration).** A wrong token increments
    `auth_rejected` and not `guest_rejected`; a valid guest token on
    `/mcp` increments `guest_rejected` and not `auth_rejected`.
    `TestGuestToken_RejectionsCountSeparately`.
11b. **The auth-mode header rides the guest grant and nothing else
    (integration).** `X-Vibe-Auth: guest` is present on exactly the
    responses the GUEST bearer authorized: absent for the operator on
    the same two routes, absent on every 401, absent on the public page.
    `TestGuestToken_AuthModeHeaderRidesTheGuestGrantOnly` — added by
    review finding 4, which is that the header had **no** server-side
    test: deleting the `Set` left the suite green, and so did stamping
    it on the operator's responses, and those two mutations produce
    opposite user-visible bugs.

12. **The page renders read-only without a new route (unit).** A static
    assertion over the embedded page: the mode is learned from the
    response header and applied in both directions, the action buttons /
    notify controls / warm row / savings nav are conditioned on it, the
    savings fetch does not pop the token gate, the read-only chip is
    class-driven (a `hidden` attribute on a `.chip` element does
    nothing), and no `whoami`-shaped route was added.
    `TestFleetPage_GuestModeIsWiredToTheHeaderNotAProbe`, plus
    `TestFleetPage_A401FromMCPDoesNotPopTheTokenGate` (review finding 3):
    an `/mcp` 401 is quiet, the button still flashes `failed`, `flash()`
    still re-runs `refresh()`, and re-authenticating on `#savings`
    reloads the view instead of un-hiding an empty one.
13. **The CLI can read and rotate it (unit).** An unconfigured
    `--guest` names `guest_token_file` rather than a missing file;
    `vibe token --guest` prints the configured file so it can be
    shared; `--regenerate` warns that it revokes EVERY guest, leaves the
    file untouched when declined, and prints the new value plus the
    restart requirement when accepted.
    `TestTokenGuest_UnconfiguredSaysHowToConfigureIt`,
    `TestTokenGuest_PrintsTheConfiguredFile`,
    `TestTokenGuest_RegenerateRevokesEveryGuestAndSaysSo`, and
    `TestTokenGuest_RefusesTheControlPlaneTokenFile` (review finding 1).
14. **Streaming contract + full inner loop (mechanical).**
    `git diff --stat main..HEAD -- internal/vibe/proxy` is empty;
    `go build ./...`, `go vet ./...`, `gofmt -l .` silent, `go mod tidy`
    clean, `go test -race -count=5 ./...`, `golangci-lint run` — plus
    ground rule 9's adversarial self-review as its own commit.

Two supporting tests sit under gates 2 and 5 without a gate number of
their own: `TestAccessFor_IsExactMatchOnMethodAndPath` (the lookup, in
isolation) and `TestRegister_MountsExactlyTheEnabledRoutes` (the table
is what is actually served, in both role shapes — otherwise the access
declarations would describe a surface nobody mounts).

### Live gates (need a real fleet; NOT runnable from the implementing environment)

L1. **The hallway test.** Configure `guest_token_file` on fleetd,
    restart, hand the guest token to a phone browser at
    `http://<fleetd>:9001/ui/fleet`. The table renders and updates live;
    there are no action buttons, no warm row and no savings tab; the
    header says read-only. Then paste the control-plane token in the
    same browser and confirm the full page returns.
L2. **The boundary in the field.** With the guest token in `$VIBE_TOKEN`:
    `vibe cell status` **works** (it reads only `/api/fleet/state` —
    §1's intended consequence), `vibe cell drain` fails 401 and says
    something legible, `curl -H` on `/api/fleet/usage`,
    `/api/fleet/savings` and `/mcp` all 401, and `vibe cell status` with
    the OPERATOR token then shows `guest_rejected` risen by exactly the
    number of refused attempts and `auth_rejected` unmoved.
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
  and can refresh `/api/fleet/state` (a probe round of the fleet) as
  fast as any authed client. That is not free (§5), but it touches no
  data plane and no declared state, and a bound belongs on the routes
  rather than on the credential — a fleetd-wide concern, not a guest
  one.
- **Hot-reloading the token file** (§6).
- **Anonymous read.** The page is public because it contains no fleet
  data; the data always needs a bearer, guest included.
- **Guest tokens on cells.** The mechanism is per-daemon and would work
  on a cell, but the fleet's status question is answered by fleetd; a
  cell's `/api/fleet/state` is a one-cell registry and sharing it is not
  the friction this phase names.

Estimated ~250 lines + tests, on the plan's calibration (C0–C4 ran
3.6–4.5× their estimates).

## Execution (2026-08-04)

### What shipped

- **`fleetapi/routes.go`** (new) — `Access` + `AccessFor` + `Routes()` +
  the table, and `Register` as a loop over it. `Register`'s old body
  (four `if` blocks and thirteen `mux.HandleFunc` calls) is gone from
  `fleetapi.go`; `registerFleetPage` became `fleetPageHandler` so the
  page mounts through the same table with `AccessPublic` beside it.
- **`daemon/auth.go`** — `authGuard` (two tokens, two counters),
  the allowlist check through `fleetapi.AccessFor`,
  `LoadOrCreateGuestToken` / `RegenerateGuestToken` /
  `validateGuestToken` (the fail-closed ladder), `Daemon.loadGuestToken`
  (non-fatal, logs path + created-vs-loaded), and the `X-Vibe-Auth`
  header on guest-authorized responses.
- **`daemon/daemon.go`** — `fleet.guest_token_file` (tilde-expanded),
  the startup load before the listeners serve, `guestEnabled` /
  `guestRejected` atomics into `DaemonInfo`.
- **`cli/cmd_token.go`** — `--guest`, composable with `--regenerate`;
  the confirmation prompt factored so both credentials share it with
  different warnings.
- **`cli/cmd_cell.go`** — one line in `vibe cell status` when guest
  access is on, deliberately a separate sentence from the
  `auth_rejected` one.
- **`fleetapi/fleet.html`** — the read-only chip, `setAuthMode`, the
  conditioned action buttons / notify controls / warm row / savings nav,
  and the savings view's "needs the control-plane token" panel.

### Gates

Unit gates 1–14: **PASS**, run as the full inner loop — `go build ./...`,
`go vet ./...`, `gofmt -l .` (silent), `go mod tidy` (clean),
`golangci-lint run` (0 issues), `go test -race ./...` plus
`-race -count=5` over `daemon`, `fleetapi`, `cli` and `fleetmcp`. Gate
14 verified: the branch's diff against `main` touches no file under
`internal/vibe/proxy`.

Seven guards were **mutation-tested** — the production code was broken
and the named test observed to fail, then restored:

| mutation | test that failed |
|---|---|
| middleware serves any request carrying the guest token (allowlist check deleted) | `TestGuestToken_DeniedOnEveryRouteItDoesNotName`, `…ReachesNeitherMCPNorTheRPCs`, `…AllowlistIsExactMatchAndGETOnly` |
| `AccessFor` returns `AccessGuest` on a miss (deny-by-default inverted) | `TestAccessFor_IsExactMatchOnMethodAndPath`, and three daemon tests |
| `AccessFor` matches by prefix instead of exactly | `TestAccessFor_IsExactMatchOnMethodAndPath`, `TestGuestToken_AllowlistIsExactMatchAndGETOnly`, **and C5's own `TestDaemon_FleetRegistry_Role`** — the old exemption is still pinned through the new mechanism |
| identical-to-control-plane rung removed from the ladder | `TestGuestToken_MisconfigurationFailsClosed/identical_to_the_control-plane_token` |
| a route left `AccessUndecided` | `TestRoutes_EveryRouteDeclaresAnAccessLevel` |
| a route registered outside the table | `TestRoutes_NoHandlerIsRegisteredOutsideTheTable` |
| guest denials counted into `auth_rejected` | `TestGuestToken_RejectionsCountSeparately` |

Live gates L1–L3: **NOT RUN.** The implementing environment cannot reach
the fleet (SSH blocked, the LAN does not route) and has no browser on
it; a fabricated transcript is worse than an honest gap.

### Author's self-review

Five findings against the feature commit, all fixed in the first review
commit. (Ground rule 9's separate adversarial pass is the addendum at
the very end of this document, and it found seven more.)

1. **The read-only chip rendered for everyone (MAJOR, page).** The chip
   was `<span class="chip" id="guest-chip" hidden>`, and `.chip` sets
   `display: inline-block` — an author display rule beats the UA sheet's
   `[hidden] { display: none }`, so the badge showed on every load
   including the operator's. Visibility is now driven by the body class
   (`#guest-chip { display: none }` + `body.guest #guest-chip`), the
   attribute is gone, and gate 12 asserts both halves so the next
   `hidden` on a styled element is caught. The C7b savings panels are
   unaffected — none of them carries a `display` rule of its own.
2. **The guest flag was a one-way latch (MAJOR, page).** `setGuest()`
   only ever turned guest mode ON, so a browser that first used a guest
   token and then had the control-plane token pasted into the gate kept
   the read-only UI — no buttons, no savings tab — until a manual
   reload. Since the header rides only what the guest bearer authorized,
   any non-401 response answers the question in both directions:
   `setAuthMode(hasHeader)` now toggles the class and restores the
   savings body.
3. **A live gate asserted the wrong thing (doc).** L2 said `vibe cell
   status` "fails 401" under a guest token. It does not: it reads
   exactly `/api/fleet/state`, so it works — and that is a deliberate
   consequence of granting the route rather than an accident. The gate
   now asserts the working read and moves the 401 assertion to
   `vibe cell drain`, and §1 states the CLI consequence outright.
4. **"An abusive guest costs an SSE subscriber slot" was false (doc).**
   `GET /api/fleet/state` is an uncached probe round of every cell and
   refreshes last-seen bookkeeping, so a guest read is not free. The
   out-of-scope entry now says what it actually costs and why the bound
   still belongs on the routes rather than on the credential.
5. **Gates named fewer tests than the phase has (doc, ground rule 10).**
   The page test and the three CLI tests existed with no gate, and gate
   10 claimed a state-body assertion that lives in gate 5's test. Gates
   12 and 13 now name them, gate 10 says where its state half is, and
   the two supporting tests are listed rather than left implicit.

### Known and accepted (documented, not fixed)

- **A guest's `HEAD` request 401s** even though Go's `GET` pattern
  serves `HEAD` to the operator. The table declares methods exactly and
  the page never issues one; this is C5's `/ui/fleet` behaviour applied
  to two more routes, and it errs strict.
- **A guest's away-window is readable.** `notify.scope` /
  `scope_until` / `scope_reason` ride `/api/fleet/state`, so an away
  declaration tells a guest the house is empty and when it stops being
  (§1). Not fixed: forking the state document is the thing this phase
  refuses to do, and a field filter is a denylist wearing a hat. Write
  `--reason` accordingly.
- **A REFUSED guest token is invisible outside the log.**
  `guest_enabled: false` is what an operator sees whether they never
  configured guest access or mis-mounted the file, and `vibe cell
  status` prints nothing in either case. The `slog.Error` at start is
  the only signal — the same shape of gap C1 built `auth_rejected` to
  close for the control-plane token, left open here because closing it
  needs a third status field for a state nobody should be in for long.
- **No hot reload and no per-guest revocation** (§6). Rotation is
  fleet-wide and needs a daemon restart.
- **The header is advisory.** A reverse proxy that strips
  `X-Vibe-Auth` costs the page its read-only rendering, not its
  correctness; the middleware is unaffected.
- **The guest surface is only as trustworthy as the guest token.**
  Anyone holding it reads the whole state document from anywhere the
  control plane is reachable; `bind_all` on fleetd plus a shared token is
  a LAN-wide read. That is the feature, stated plainly.

## Adversarial-review addendum (2026-08-05, 7 findings, 5 fixed with
regression tests + 2 documented)

Ground rule 9's separate pass over the feature + self-review commits.
Findings 1–5 are fixed in the `review:` commit; each production change
was reverted and the named test watched to FAIL, then restored. Findings
6–7 are consequences the phase argued past rather than defects, and are
now written down where an operator will meet them.

1. **`vibe token --guest` disclosed and ROTATED the control-plane token
   (BLOCKER, CLI).** `guest_token_file` pointing at the control-plane
   token file is a configuration this doc already listed as reachable —
   the daemon refuses it and logs why, and the operator's next move is
   this command. It obeyed the path: `vibe token --guest` printed the
   **control-plane token** under a banner that says share it, and
   `vibe token --guest --regenerate --yes` **overwrote** it with a fresh
   random value, locking out every control-plane client, from a command
   whose name says guest and whose output says "restart the daemon for
   the new guest token to take effect". Both forms now refuse, naming
   both paths. Mutation: removing the guard makes the pre-fix behaviour
   reproduce verbatim — the test prints the disclosed token and the
   rotated value. `TestTokenGuest_RefusesTheControlPlaneTokenFile`.
2. **The fail-closed ladder could mint over the control-plane token
   (MAJOR, daemon).** The identical-token rung compares *values*, and it
   runs **after** the branch that mints into a missing file — so
   `guest_token_file: <the control-plane token file>` with that file
   absent writes a random value over it and enables guest access on the
   result. `Run`'s ordering hides this today (the control-plane token is
   loaded first, so the file exists), which makes it an ordering rather
   than a guard, and the log said "identical to the control-plane token"
   rather than naming the actual mistake. `loadGuestToken` now refuses on
   the PATH before reading, via `daemon.IsControlTokenFile`
   (`os.SameFile`, falling back to a cleaned compare precisely when the
   file does not exist).
   `TestGuestToken_ControlPlaneTokenFileIsRefusedBeforeItIsRead`
   (mutation: the guest loader is caught minting a 43-char token into
   `$XDG_STATE_HOME/vibe/token`), plus the end-to-end
   `TestGuestToken_PointedAtTheControlPlaneFileNeverRewritesIt`.
3. **The documented degradation degraded to the banned behaviour
   (MAJOR, page).** §5 accepts "a proxy strips the header, the buttons
   render, a click 401s" as the un-polished fallback — but `rpc()` went
   through the same `api()` wrapper that pops the token gate, so that
   click produced *exactly* the outcome §5 says must not ship: a guest
   with a working token told it is invalid, re-enter it. A `/mcp` 401
   cannot distinguish a stale control-plane token from a guest past
   their grant, so it no longer decides: `rpc()` is quiet, the button
   still flashes `failed`, and `flash()`'s existing 1.5s `refresh()`
   puts the verdict on `/api/fleet/state`, which pops the gate for a
   token that really is dead. Also fixed alongside: re-authenticating
   while on `#savings` un-hid a body that had never been rendered, so an
   operator who pasted the control-plane token into a guest tab got a
   blank savings screen for up to 60 s until the timer fired.
   `TestFleetPage_A401FromMCPDoesNotPopTheTokenGate`.
4. **`X-Vibe-Auth` had NO server-side test (MAJOR, test truth).** The
   header is the entire mechanism behind §5, and the whole suite stayed
   green under **both** mutations: deleting the `Set` (every guest gets
   the operator page — buttons that 401 and a token gate that lies) and
   stamping it on the operator's responses too (every operator page
   renders read-only). Only the embedded page's *string* was asserted,
   never the daemon's behaviour. Now pinned on both routes, in both
   credential directions, plus absent on 401s and on the public page.
   `TestGuestToken_AuthModeHeaderRidesTheGuestGrantOnly` (gate 11b).
5. **Two gates claimed assertions their tests do not make (ground rule
   10).** Gate 9 said the equal-token case "additionally proves the
   value still works as the control-plane token" — its named test only
   calls the loader and never starts a daemon; and "leaves guest access
   disabled with the daemon still serving" was likewise unasserted. Gate
   1 said "401 to every route" and probed one. Three integration tests
   now make those claims true and the gate text names them:
   `TestGuestToken_IdenticalToControlPlaneFailsClosedOverHTTP`,
   `TestGuestToken_RefusedFileOpensNothing`,
   `TestGuestToken_RoutesAreProbedAgainstAnUnconfiguredDaemonToo` (the
   last derived from `fleetapi.Routes()`, so it covers future routes).
6. **The away declaration is guest-readable, and §1 argues the opposite
   (doc).** The judgement-call section spends twenty lines denying
   `/api/fleet/usage` because absence is sensitive and the ledger leaks
   it *by inference*; `notify.scope` / `scope_since` / `scope_until` /
   `scope_reason` ride `/api/fleet/state` and leak it *outright, with an
   end date*, on a route this phase grants. Not fixed — the two
   arguments against forking the state document still hold — but §1 now
   says so in its own paragraph and it is listed under Known and
   accepted, because an operator cannot infer it from "anything on the
   page is guest-visible".
7. **The denial table overstated what `GET /api/fleet/leases` withholds
   (doc).** Its row read "reading and *deleting* other people's
   declarations, C11 holds included" — but `cells[].leases` rides
   `/api/fleet/state`, so a guest already reads every holder, model and
   note, C11 holds included. What the denial actually withholds is the
   write half and the fleet-wide listing. Row corrected.

Also checked and found correct, so the next reader does not re-derive
it: the `deploy/fleetd/README.md` guest-token example was
`/state/guest-token`, one level above the compose's only rw state mount
(`/state/vibe` = `$XDG_STATE_HOME/vibe`) — a MINTED file there is
container-local, so every recreate silently revokes every guest, which
is the exact failure the same commit's state-contract table says the
mount prevents. Corrected to `/state/vibe/guest-token` with the reason
written next to it. And `notify.endpoint` / `delivery.last_error`, the
two C9 fields a guest now reads, are redacted and scrubbed at the
source (`WebhookSink.redacted`, `Deliverer.scrub`) — the guest grant
does not leak the webhook credential.

## Live-gate addendum (2026-08-05): "RAW" meant decoded

Found while running the C12/C13 live gates against a real 4-cell fleet.
Fixed on `fix/live-gate-truth`. This one is a **false invariant**, not
an exploit, and the difference is the point.

§2 said the allowlist matched "on the RAW path before the mux cleans
anything", and `AGENTS.md` said the same. The code read `r.URL.Path`,
which net/url has already percent-DECODED:
`url.ParseRequestURI("/ui/%66leet")` yields `URL.Path == "/ui/fleet"`
with `RawPath == "/ui/%66leet"`. So `GET /ui/%66leet` was served
anonymously and `GET /api/fleet/%73tate` was served to a guest bearer,
as if each were the declared route.

**Nothing was reachable that was not already reachable**, and
overstating that would be its own kind of dishonesty. Go's `ServeMux`
routes on the decoded path too, so the middleware and the router agreed
on which handler the request meant; and the allowlist is positive and
exact, so a decoded match could only ever re-grant the route that
string already granted. There is no bypass here to report.

The defect is that a load-bearing security invariant was written down
falsely — in the design doc, in `AGENTS.md`, and in the middleware's own
comment — and the property those three documents claim is the one the
next agent will rely on. It stops being harmless the first time
anything in this daemon routes on `RawPath`, or Go's matching changes,
or someone adds a route whose encoded and decoded spellings mean
different things.

Fixed by making the code match the doc rather than the reverse:
`fleetapi.AccessFor(r.Method, r.URL.EscapedPath())`. `EscapedPath()`
returns `RawPath` when it is a valid encoding of `Path` and re-encodes
`Path` otherwise, so the plain spellings are unaffected and an encoded
one is simply a different string — a miss, therefore token-only, which
is the answer `/ui/fleet%2f` has always got. C5's pinned bypass
attempts pass unchanged (the encoded spelling joins them, so the list is
no longer six — it has grown in three phases and the count is not worth
restating anywhere), and the encoded family is pinned in
`daemon/authpath_test.go`, which also asserts the net/url premise so a
future stdlib change cannot quietly invalidate the test's reasoning.
