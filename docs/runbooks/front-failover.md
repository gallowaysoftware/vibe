# Runbook: the front host died

The front host dying is the fleet's one total outage — every client
dials `:9000` and every cell announces to fleetd. This is the manual
path to a spare box. There is no automatic failover and there will not
be one: the control plane changes what the catalog *says*, never where a
request *goes*, and a promotion nobody asked for is exactly the silent
rerouting that rule forbids.

**Rehearsed** on the local harness, 2026-08-05: the mechanical half took
10-14 seconds (two runs; the spread is one announce interval). Budget
**10–15 minutes** for the real thing — the time goes on confirming the
old box is dead, fetching credentials, pulling the pinned image, and DNS
TTL.

## Recover

```sh
M=/mnt/backup/fleet          # the mirror destination
S=/srv/fleetd/state          # this box's fleetd state dir
C=/srv/fleetd/config         # this box's fleetd config dir
F=/srv/front-config/config.yaml
```

1. **Confirm the old front is DOWN.** Not "unreachable from here" —
   down. If it might come back, power it off. Two fronts under one
   address is worse than none: both fleetds accept announces, both fire
   warm and sleep schedules, and both fold the same cumulative totals
   into two append-only ledgers that can never afterwards be reconciled.
   Nothing detects that state, because each half looks healthy from
   where it stands.
2. **Read the mirror.** `vibe fleet mirror verify $M` — prints the
   identity to assume, the image to run, the credentials it does not
   carry, and recomputes every sha256. If it reports errors, take the
   previous archive.
3. **Restore.** `vibe fleet mirror restore $M --state-dir $S --config-dir $C --front-config $F`
   It re-runs the verification and **refuses** while anything answers on
   the fleet's own addresses (step 1, mechanically). `--force` is for the
   one honest false positive: you already moved the address to this box.
4. **Fetch the credential files** the manifest lists as references
   (`cell_token`, `swap_key`, `notify_url`) from the private fleet repo,
   to the paths it names, mode `0600`. Without them fleetd still
   observes the fleet and cannot drain, suspend or reach a keyed cell.
5. **Start the front on the PINNED image** the manifest names
   (`fleet.front_image`) — `deploy/front/docker-compose.yaml` with
   `FRONT_CONFIG_DIR=$(dirname $F)`. A standby that pulls a *different*
   llama-swap is how a recovery becomes the 2026-08-05 in-flight-wire
   incident (C16).
6. **Start fleetd** against `$S` and `$C` —
   `deploy/fleetd/docker-compose.yaml`. The control-plane token came out
   of the mirror, so every cell, client and MCP registration keeps
   working.
7. **Take the identity, last.** Repoint the DNS record (or take the IP)
   to this box. Cells re-announce within one heartbeat.
8. **Check.** `vibe fleet doctor` — `fleetd.token` must say *loaded*,
   not *minted*; every cell should be announcing within ~30 s;
   `mirror.age` will WARN until a mirror runs here.
9. **Before the old box comes back:** disable its front and fleetd units
   (`systemctl disable --now`, or `docker compose down` + remove
   `restart: unless-stopped`). It will otherwise answer on an address
   that is no longer its.

Not covered by any of this, and named in every manifest: model weights,
each cell's own state (C8 probe baselines live on the cells and survive
the front), DNS itself, systemd units, and the credential *values*.

## Set the mirror up (do this once, on the front host)

```sh
vibe fleet mirror --out /mnt/backup/fleet \
  --state-dir /srv/fleetd/state --config-dir /srv/fleetd/config \
  --front-config /srv/front-config/config.yaml \
  --include /srv/front --include /srv/fleetd/compose
```

- `--out` must not be on this host's own disk; the command refuses a
  destination inside the state or config dir, and cannot tell whether a
  path is a remote mount. **A mirror stored on the box it mirrors is not
  a mirror.**
- The archive carries the control-plane token and the guest token and is
  written `0600`: **the destination is as sensitive as the front host.**
  `--no-secrets` drops them and the rendered front config, at the price
  of a fleet-wide re-key and a re-render during the recovery.
- Run it from a `systemd` timer or cron as a user that can read fleetd's
  state dir (the container writes as root):

  ```
  [Unit]
  Description=Mirror the fleet's control-plane state off the front host
  [Service]
  Type=oneshot
  ExecStart=/usr/local/bin/vibe fleet mirror --out /mnt/backup/fleet …
  # OnCalendar=daily in the paired .timer
  ```

- Declare `fleet.mirror_max_age: 36h` in fleetd's config so
  `vibe fleet doctor` reports `mirror.age`. Undeclared is UNKNOWN, never
  OK; `unmanaged` declares that something else backs this host up.
- Prefer a **DNS name** for the front and fleetd in `hosts.yaml`. With a
  literal IP the takeover means taking the address; with a name it is one
  record. The mirror warns either way, once, where it is actionable.

## The drill (quarterly, 15 minutes)

`scripts/fleetlab/gate-c19-drill.sh` performs every step above against a
real four-cell fleet on one box: it mirrors, proves the restore refuses
while the fleet is alive, SIGKILLs fleetd and the front, shows the cells
still serving, restores onto a standby, and times the recovery. A
checklist nobody performs is worth nothing.

On real hardware the drill worth running is the same one
`deploy/fleetd/README.md` describes for `fleet doctor`, plus: restore the
newest archive onto the standby box's disks (step 3, `--dry-run`), and
confirm the credential files step 4 names are actually in the private
repo. Both are read-only against production.
