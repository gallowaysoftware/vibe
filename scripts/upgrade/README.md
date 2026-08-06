# The upgrade ritual

`ritual.sh` is the only sanctioned way to move the fleet's llama-swap.
Its job is to make the sequence **canary → gate → fleet** cheaper to
perform than to skip, because the alternative already happened.

## The incident this exists for

On **2026-08-05** `ghcr.io/mostlygeek/llama-swap:cpu` was found serving
**v247** while the fleet was gated on **v239**. v240+ replaced the
`/api/events` in-flight wire — a `requests` array became
`{"operation":"upsert","request":…}` / `{"operation":"remove","id":…}`
with `requests` omitempty and absent — so vibe counted the length of an
array that was no longer there, got 0, and **reported that zero as
known**. Eight busy guards disarm on a reported zero: `drain --wait`'s
quiescence, C14's suspend, C8's probe guard, both warm loops, the
pre-drain report.

Nothing failed. Nothing paged. `deploy/front/docker-compose.yaml` floated
the tag, so a routine `docker compose pull` on the front was the entire
trigger, and the fleet would have kept truncating generations at drain
time until somebody noticed by hand.

PR #37 fixed the parser and gated both wires. This directory is the
discipline that stops the next one reaching the fleet.

## The steps

```
./ritual.sh preflight <version>   # what is this build, and is it gated?
./ritual.sh record    <version>   # capture fixtures for a version we have none for
./ritual.sh canary    <version>   # the wire, the behaviours, a real four-cell fleet
./ritual.sh gate      <version>   # the six-client SSE cold-start gate
./ritual.sh pin       <version>   # print the FRONT_IMAGE line to paste
```

Nothing here touches production: scratch dirs under `$UPGRADE_DIR`, ports
9810-9819, upstreams 6100+ — clear of the router on `:9000` and the daemon
on `:9001`.

One thing to know before running `canary`: its second step hands off to
`scripts/fleetlab`, which binds *its* fixed 9600-9799 range and whose
`down` sweep is anchored on the shared upstream ports. A second lab on the
same box will be killed by it — [futures item
15](../../docs/design/fleet-control-futures.md) is the port-offset knob
that fixes it, and it is why C16's own L4 gate went unrun.

## What each step catches

| step | catches | how |
|---|---|---|
| `preflight` | the version is not one this build has ever replayed | `internal/swaptest/fixtures/<v>` + `ci.yml`'s matrix |
| `canary` (1/2) | **a wire change in `/api/events`** | `TestSwapContract` I1/I5 — folded through the REAL `fleetapi` parser, so a shape it does not recognise has to reach *unknown*, not a valid-looking zero |
| | **a shape change in `/api/metrics/activity`** | `TestSwapContract` I2/I3/I4 — classified through the REAL `usagemeter`, over rows the candidate produced |
| | **the SSE keepalive the data plane leans on** | `TestSwapBehaviour` B1 — a streaming completion at a model that stays unready for 6s must be receiving parseable delta frames within 2s, and the same stream must go on to carry the model's own tokens |
| | **a behavioural change in SIGTERM stream handling** | `TestSwapBehaviour` B2 — `/api/events` closes at once, in-flight *inference* streams are NOT cancelled, and whatever is still running at the 30s grace is force-closed |
| `canary` (2/2) | what any of that does to a *fleet* | `scripts/fleetlab` on the candidate binary: four real llama-swap processes, three cell classes, a real fleetd, both announcer shapes, then `lab.sh prove` |
| `gate` | real clients not tolerating the keepalive | `scripts/smoke/llama-swap/run-smoke.sh` — curl, the OpenAI and Anthropic SDKs, `claude -p`, `qwen`, plus the kill-cancel test |
| `pin` | the pin never being applied | prints the resolved `repo:tag@sha256:…` and the four things a human still does |

## Automatable, and honestly not

**In CI, on every PR** (`ci.yml`'s `conformance` job, both pinned
versions in parallel): the wire, the activity shape, the keepalive and the
SIGTERM behaviour. Roughly 40s on top of a job that already downloads a
real llama-swap and a real llama.cpp. The blocking `test` job stays ~15s.

**In CI, weekly** (`drift.yml`): the same list against llama-swap
*latest*, unpinned. It is meant to go red — red means the contract moved,
and it is the only automatic signal that arrives *before* a
`docker compose pull` arms the change.

**Locally, scripted** (`canary` step 2): the fleetlab pass. CI has no
GPU, no models and no second cell; this is where the candidate meets
drain, suspend, warm and probe.

**Locally, supervised** (`gate`): the six-client rig. It is scripted, but
it takes ~45 minutes at `DELAY_S=420` and two of the seven clients (Open
WebUI, pi) are manual browser/TUI work — `scripts/smoke/llama-swap/README.md`
sections 5 and 6. Run the automated five at `DELAY_S=90` for iteration and
the full thing before the pin moves.

**A human, and no pretending otherwise**: applying the pin, recreating the
front container, rolling the cells one at a time, and reading
`vibe fleet doctor`. That is four commands with a real fleet behind them.

### What deliberately is NOT a checklist step

Nobody performs a checklist at 2am, so the things that must hold on a bad
night are code, not instructions:

- **"Check whether the front image is pinned"** is `front.image_pin` in
  `vibe fleet doctor`, and the reference stack ships a digest so the
  default is already right.
- **"Check every cell is on the version you gated"** is
  `versions.llama_swap` — each cell announces its own llama-swap's
  `/api/version`, and fleetd reads the front's directly because the front
  runs no announcer. One reader (`fleetapi.ReadSwapVersion`) for both, and
  the endpoint is itself gated by `TestSwapContract` I6 against a real
  binary, because a producer nothing replays goes silent on exactly the
  upgrade that moves it. A version with no recording in this build is a
  WARN naming itself, and a box that did NOT answer is named too — the
  check may never render agreement over a silence.
- **"Remember that both wires must keep passing"** is
  `TestGatedSwapVersionsMatchesRecordings` and
  `TestConformanceMatrixCoversEveryRecording`: the recordings, the CI
  matrix and doctor's claim are three copies of one fact, and two tests
  fail when they disagree.
- **"Re-record after a bump"** is `TestRecordingIsCurrent`, which fires on
  the box where the upgrade happened, in front of the person who can act.

## A heterogeneous fleet is the normal state

The front is a pure proxy; the cells own the models. So the front moves
first and alone, and cells roll one at a time behind it. Old recordings
are kept rather than replaced, `ci.yml` replays every one of them, and
doctor's version matrix reports a split as a mid-upgrade rather than as a
fault. "Newest wins" would make the mid-state — which is most of the
elapsed time of any upgrade — the untested one.
