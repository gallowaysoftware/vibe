package fleetapi

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The doctor's half of front failover (fleet-control C19). fleetd does
// not mirror anything and must not: the mirror has to keep running when
// fleetd is the thing that broke, so it is a command on the front HOST
// (`vibe fleet mirror`) driven by a timer. What fleetd can do is READ
// the receipt that command leaves in the state dir it already owns, and
// say whether the backup is fresh — because a stale mirror is the
// classic failure of this whole idea, and it is invisible until the
// morning somebody needs it.

// UnmanagedMirror is the declaration that this fleet's state is backed
// up by something other than `vibe fleet mirror` — a snapshotting
// filesystem, borg, the front being a VM somebody images. A closed
// vocabulary rather than a silent empty, for C16's reason: "the operator
// decided" and "nobody told fleetd" must not be spelled the same way.
const UnmanagedMirror = "unmanaged"

// mirrorStaleFactor is where a late mirror stops being a warning. One
// missed run is a timer that did not fire; three is a mechanism that
// stopped working, which is the state this check exists to catch.
const mirrorStaleFactor = 3

// MirrorFacts is the last recorded mirror run, injected by the daemon
// half exactly as the rest of DoctorHost is — fleetapi has never read a
// file and this phase does not change that.
type MirrorFacts struct {
	At          time.Time
	Archive     string
	Dest        string
	Files       int
	Bytes       int64
	Missing     []string
	Errors      []string
	Warnings    []string
	Credentials bool
	// ReadErr is set when a receipt EXISTS and could not be understood.
	// Distinct from a nil MirrorFacts (no receipt at all), because
	// "corrupt" and "never ran" are different findings with different
	// fixes.
	ReadErr string
}

const mirrorFix = "run `vibe fleet mirror --out <off-host dir>` from a timer on the front host; " +
	"the runbook is docs/runbooks/front-failover.md."

// checkMirror emits mirror.age (is the backup fresh) and, whenever a run
// has ever been recorded, mirror.contents (did that run capture what it
// set out to).
func (s *Server) checkMirror(rep *DoctorReport, host DoctorHost, now time.Time) {
	decl := strings.TrimSpace(host.MirrorMaxAge)
	f := host.Mirror

	switch decl {
	case UnmanagedMirror:
		rep.Add(DoctorCheck{ID: "mirror.age", Level: LevelOK,
			Summary: "the front's state mirror is declared to be handled outside vibe",
			Detail: "whatever does it owns the freshness question. It still has to cover the state dir " +
				"(intent, leases, the append-only usage ledger) AND the front's rendered config, and it still has to " +
				"land somewhere that survives this host."})
	case "":
		lvl, sum := LevelUnknown, "nothing declares how fresh the front's state mirror should be"
		det := "the front host dying is the fleet's one total outage, and the state that dies with it " +
			"(intent.json, leases.json, the append-only usage ledger, the rendered front config) is not " +
			"reconstructible from the cells."
		if f != nil && f.ReadErr == "" {
			sum = "a mirror ran " + humanAge(now.Sub(f.At)) + " ago; nothing declares how fresh it must be"
			det = "no threshold, so no verdict: this check can report the age and cannot say whether it is late."
		}
		rep.Add(DoctorCheck{ID: "mirror.age", Level: lvl, Summary: sum, Detail: det,
			Fix: "set fleet.mirror_max_age (e.g. 36h for a nightly timer), or the literal " +
				UnmanagedMirror + " if something else backs this host up. " + mirrorFix})
	default:
		max, err := time.ParseDuration(decl)
		switch {
		case err != nil:
			rep.Add(DoctorCheck{ID: "mirror.age", Level: LevelFail,
				Summary: "fleet.mirror_max_age is not a duration: " + decl,
				Detail:  err.Error(),
				Fix:     "use a Go duration (36h, 24h) or the literal " + UnmanagedMirror + "."})
		case max <= 0:
			rep.Add(DoctorCheck{ID: "mirror.age", Level: LevelFail,
				Summary: "fleet.mirror_max_age is not positive: " + decl,
				Fix:     "use a Go duration (36h, 24h) or the literal " + UnmanagedMirror + "."})
		case f == nil:
			rep.Add(DoctorCheck{ID: "mirror.age", Level: LevelFail,
				Summary: "a state mirror is declared (" + decl + ") and none has ever run here",
				Detail: "no receipt in the state dir. Either the timer was never installed, or it writes its " +
					"receipt somewhere fleetd cannot see — the mirror must run against THIS state dir.",
				Fix: mirrorFix})
		case f.ReadErr != "":
			rep.Add(DoctorCheck{ID: "mirror.age", Level: LevelUnknown,
				Summary: "the mirror receipt could not be read",
				Detail:  f.ReadErr,
				Fix:     "re-run the mirror; the receipt is rewritten every run. " + mirrorFix})
		case f.At.IsZero():
			rep.Add(DoctorCheck{ID: "mirror.age", Level: LevelUnknown,
				Summary: "the mirror receipt carries no timestamp",
				Detail:  "a receipt with no time cannot answer the only question this check asks.",
				Fix:     mirrorFix})
		case f.At.After(now.Add(time.Minute)):
			// A future stamp would read as freshly-mirrored forever, which
			// is this repo's oldest mistake: absent evidence rendering as a
			// healthy value.
			rep.Add(DoctorCheck{ID: "mirror.age", Level: LevelUnknown,
				Summary: "the mirror receipt is stamped in the future (" + f.At.UTC().Format(time.RFC3339) + ")",
				Detail: "the age cannot be computed. A clock stepped on this host or on whatever wrote the " +
					"receipt; until it is fixed a stale mirror would read as a fresh one.",
				Fix: "fix the clock, then re-run the mirror."})
		default:
			age := now.Sub(f.At)
			where := f.Archive
			if where == "" {
				where = f.Dest
			}
			det := fmt.Sprintf("%d files, %s, last archive %s", f.Files, humanBytes(f.Bytes), where)
			if f.Credentials {
				det += ". It carries the control-plane token: the destination is as sensitive as this host."
			}
			switch {
			case age <= max:
				rep.Add(DoctorCheck{ID: "mirror.age", Level: LevelOK,
					Summary: "state mirrored " + humanAge(age) + " ago (declared limit " + decl + ")",
					Detail:  det})
			case age <= time.Duration(mirrorStaleFactor)*max:
				rep.Add(DoctorCheck{ID: "mirror.age", Level: LevelWarn,
					Summary: "state mirror is " + humanAge(age) + " old, past the declared " + decl,
					Detail:  det + ". One missed run is a timer that did not fire.",
					Fix:     "check the timer on the front host: `systemctl --user list-timers` (or cron). " + mirrorFix})
			default:
				rep.Add(DoctorCheck{ID: "mirror.age", Level: LevelFail,
					Summary: "state mirror is " + humanAge(age) + " old, more than " + strconv.Itoa(mirrorStaleFactor) + "x the declared " + decl,
					Detail: det + ". A backup this far behind is a mechanism that stopped, not a run that slipped: " +
						"everything since is unrecoverable if this host dies.",
					Fix: mirrorFix})
			}
		}
	}

	if f == nil || f.ReadErr != "" {
		return
	}
	// A run that could not read a file produced a partial archive, and
	// the file it could not read is exactly the one the recovery will
	// want. Missing files are NOT a finding — a fleet with no leases has
	// no leases.json — but they are named, so "absent" and "not looked
	// for" stay different answers.
	det := ""
	if len(f.Missing) > 0 {
		det = "not present on this host (normal for a fleet that has never used them): " + strings.Join(f.Missing, "; ")
	}
	if len(f.Errors) > 0 {
		rep.Add(DoctorCheck{ID: "mirror.contents", Level: LevelWarn,
			Summary: fmt.Sprintf("the last mirror could not read %d file(s)", len(f.Errors)),
			Detail:  strings.Join(f.Errors, "; ") + ifNotEmpty(". ", det),
			Fix: "the archive is partial: whatever it could not read is not backed up. Usually a permission " +
				"(fleetd's container writes its state as root; the mirror must run as a user that can read it)."})
		return
	}
	sum := fmt.Sprintf("last mirror captured %d files (%s)", f.Files, humanBytes(f.Bytes))
	if len(f.Warnings) > 0 {
		det = ifNotEmpty("", det) + ifNotEmpty(". ", strings.Join(f.Warnings, "; "))
	}
	rep.Add(DoctorCheck{ID: "mirror.contents", Level: LevelOK, Summary: sum, Detail: strings.TrimPrefix(det, ". ")})
}

func ifNotEmpty(sep, s string) string {
	if s == "" {
		return ""
	}
	return sep + s
}

// humanAge renders a duration the way an operator reads one: hours and
// minutes up to a day, then days.
func humanAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "less than a minute"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 48*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		return strconv.Itoa(h) + "h" + strconv.Itoa(m) + "m"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d"
	}
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(1<<10))
	default:
		return strconv.FormatInt(n, 10) + " B"
	}
}
