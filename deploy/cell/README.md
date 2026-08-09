# cell — packaging for the box a human takes back

A cell is a GPU box that serves models when nobody wants it for
anything else. This directory is what makes the *taking it back* visible
to the fleet — `docs/design/fleet-control.md` §4 axis 2, delivered as
files you install on the cell rather than as prose somebody has to
remember at 9pm on a Friday.

Two paths, and they are not equal.

| | what it is | what the fleet learns |
|---|---|---|
| `vibe-reclaim.sh` | the launcher wraps the app | **declared**: reason, ETA, deterministic resume |
| `vibe-cell-intent.sh` | the unit records itself | **recorded**: that it stopped, and when — never why |

The first is the one to install. The second exists because the first
will sometimes be bypassed, and an intent axis that goes stale on every
bypass is worse than an empty one: it is a confident wrong answer.

## 1. The declared path: `vibe-reclaim.sh`

```sh
sudo install -m 0755 vibe-reclaim.sh /usr/local/bin/vibe-reclaim.sh
```

- **Steam** ▸ right-click the game ▸ Properties ▸ Launch Options:

  ```
  /usr/local/bin/vibe-reclaim.sh %command%
  ```

  Steam substitutes `%command%` with the game's own command line, so the
  wrapper drains, launches the game, and resumes the cell when you quit
  — including when the game crashes, and including when you kill Steam's
  launch from the desktop.

- **Desktop shortcut**: copy `vibe-reclaim.desktop` to
  `~/.local/share/applications/`, edit `Exec=`.

- **A shell**, for a long local job that wants the whole GPU:

  ```sh
  vibe-reclaim.sh ./train.sh          # resumes on exit, any status
  VIBE_RECLAIM_ETA=23:00 vibe-reclaim.sh ./train.sh
  ```

`VIBE_RECLAIM_REASON` (default `gaming`) and `VIBE_RECLAIM_ETA` are what
`vibe cell status` and the fleet page show other people — "gaming, since
21:04, eta 23:00" is the sentence this whole path exists to produce.

The wrapper's exit status is the wrapped command's own, so it composes
(`vibe-reclaim.sh ./train.sh && notify-send done`).

**It passes `--yes`.** Leases are advisory by design: a wrapper that can
refuse to start the game because somebody left a lease open is a wrapper
that gets removed from the launch options after one Friday night. What
was stranded is still printed in the pre-drain report, which for Steam
lands in the launcher log.

**If the drain FAILS — or is DECLINED — the command does not run.** An
aborted drain (the confirmation refused at the pre-drain lease report) is
a not-run for the same reason a failed one is: nothing was reclaimed, so
launching would silently stop declaring. That is `vibe cell
drain --until-exit`'s own semantic and the wrapper does not soften it: a
wrapper that launched the game anyway would silently stop declaring, on
exactly the box where nobody would notice for months. The failure is
loud where a human can see it — run the same line in a terminal to read
the error. The usual causes are `cell_cmds.drain` unset in this box's
`config.yaml` (it is not a cell yet) and a drain command that fails.

## 2. The recorded path: `vibe-cell-intent.sh` + a unit drop-in

```sh
sudo install -D -m 0755 vibe-cell-intent.sh /usr/local/lib/vibe/vibe-cell-intent.sh
sudo install -D -m 0644 llama-swap.service.d/50-vibe-intent.conf \
  /etc/systemd/system/llama-swap.service.d/50-vibe-intent.conf
sudoedit /etc/systemd/system/llama-swap.service.d/50-vibe-intent.conf   # the example values
sudo systemctl daemon-reload
```

The drop-in belongs on **whatever unit `cell_cmds.drain` stops** — the
serving stack, not the vibe daemon. `ExecStopPost` records the stop,
`ExecStartPost` retires that record when the stack comes back.

What it writes, exactly, on a stop — one POST to `/api/fleet/intent`:

```json
{"cell":"gpu-cell","state":"unit_stopped"}
```

and `{"cell":"gpu-cell","state":"unit_started"}` when the stack comes
back. The unit sends **no reason and no ETA**: it knows that it stopped
and when, and a hook that could send a reason could dress a stop up as a
declaration. fleetd stamps the reserved reason `stopped out of band`
itself.

**These two verbs need fleetd ≥ C24, and that is deliberate.** An older
front answers an unknown state with `HTTP 400`, which the hook treats
like every other failure: record nothing, say why, exit 0. Spelled as a
*reason* on `drained` instead, an old front would have recorded an
ordinary drain REQUEST — and handed it back to this cell on its next
heartbeat as an instruction to drain itself. Version skew must degrade
to doing nothing, never to actuating.

The reason fleetd stamps is **reserved**, and it keys four behaviours,
each of which is the difference between a record and a trigger:

1. it is **never handed back to the cell** as `desired_intent`. Without
   this, the next heartbeat after the box comes back carries the record
   to the announcer's `reconcile`, which runs `cell_cmds.drain` — the
   hook would have stopped the serving stack it only meant to describe.
2. it **loses to the cell's own drained echo**: a declared drain at the
   box outranks a record written by the stop, so wrapping Steam does not
   get stamped "stopped out of band".
3. it is **never a pending request** — nothing was asked of the cell, so
   there is no ack to wait for and `vibe fleet doctor` reports no residue
   for a box that is simply off.
4. it **does not explain the absence**. A crash fires `ExecStopPost`
   exactly as `systemctl stop` does, so an `always_on` cell still alarms
   and `vibe fleet doctor` still names the stop as undeclared. The record
   adds the *when*; the *why* is still missing, and every surface that
   cares about the why behaves as it did before.

And one rule on the way in: **a stop record never overwrites an existing
declaration.** If the cell is already drained for a reason — a human's
`--reason gaming`, a `sleep_schedule` suspend that is about to take the
box down through this very unit — that entry stays, because it knows the
why and the unit does not. The hook's journal line is hedged for exactly
this reason. A stop record may replace a stop record: that is the unit
updating the one entry it owns.

### When it cannot reach fleetd

It gives up, says so in the journal, and **exits 0**. Nothing is
written: the cell has no intent entry, the fleet renders `DRAINED?`, and
that question mark is the truth. The one thing this hook must never do
is leave a confident value behind — not "serving", not a stale
"drained".

It is bounded hard, because it runs inside the unit's stop and a hook
that hangs is a shutdown that hangs: 1s to connect, `VIBE_INTENT_TIMEOUT`
(default 3, ceiling 30) in total, one request, no retries. Set the unit's
`TimeoutStopSec` above llama-swap's own 30s in-flight stream grace plus
that bound — the drop-in ships 45s.

### Requirements

- `curl`, and a readable copy of fleetd's control-plane bearer token
  (`POST /api/fleet/intent` is token-only, and correctly so).
- The cell's name in `hosts.yaml`. An unknown name gets a 400 from
  fleetd, which the hook reports and treats as "not recorded" — a typo
  cannot invent a cell.

### What it deliberately does not do

No `systemctl`, no `vibe cell` verb, no signal, no retry, no state file.
Its entire side effect is one POST. `TestC24HookActuatesNothing` runs it
with a PATH full of tripwires and fails if it invokes any of them.

## Known limitation: an announcing cell whose daemon outlives the stop

On a cell that announces (C3), the vibe daemon is its own unit and keeps
heartbeating `serving` after the serving stack stops — the echo is
state-only, so it cannot say "the stack under me is down". fleetd trusts
a fresh announce over a failed probe, so the pair renders
**INCONSISTENT** rather than STOPPED until the announcer goes stale. The
record is still there and still says when; the display is the part that
is wrong, and fixing it needs a reason (or a serving-stack liveness bit)
on the announce echo, which is a wire change. Written up as the C24
follow-up in `docs/design/fleet-control-futures.md`.

Cells that fleetd reaches by probe — no announcer — render **STOPPED**
with the record, which is the intended shape. *Amended C27:* that state
used to be DRAINED, which conflated "somebody chose this and said why"
with "the unit recorded that it stopped, and nothing knows why".
`STOPPED` is the second of those; the host being unreachable as well
still reads OFF, because the box being gone dominates.
