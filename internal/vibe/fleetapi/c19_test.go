package fleetapi

import (
	"strings"
	"testing"
	"time"
)

// C19: the doctor's half of front failover. The check exists because a
// mirror that stopped running looks exactly like one that is running,
// and the difference only becomes visible on the morning somebody needs
// it.

func mirrorReport(t *testing.T, host DoctorHost, now time.Time) DoctorReport {
	t.Helper()
	srv := newDoctorServer(t, host)
	var rep DoctorReport
	srv.checkMirror(&rep, host, now)
	return rep
}

func TestMirrorAge_LevelsAndReasons(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *MirrorFacts {
		return &MirrorFacts{At: now.Add(-d), Archive: "/mnt/backup/fleet-mirror-x.tar.gz", Files: 9, Bytes: 4096}
	}
	for _, tc := range []struct {
		name  string
		host  DoctorHost
		want  Level
		says  string
		nsays string
	}{
		{"undeclared", DoctorHost{}, LevelUnknown, "nothing declares", ""},
		{"declared unmanaged", DoctorHost{MirrorMaxAge: UnmanagedMirror}, LevelOK, "outside vibe", ""},
		{"not a duration", DoctorHost{MirrorMaxAge: "nightly"}, LevelFail, "not a duration", ""},
		{"zero duration", DoctorHost{MirrorMaxAge: "0s"}, LevelFail, "not positive", ""},
		{"declared, never ran", DoctorHost{MirrorMaxAge: "36h"}, LevelFail, "none has ever run", ""},
		{"fresh", DoctorHost{MirrorMaxAge: "36h", Mirror: at(4 * time.Hour)}, LevelOK, "4h0m ago", ""},
		{"one missed run", DoctorHost{MirrorMaxAge: "36h", Mirror: at(40 * time.Hour)}, LevelWarn, "past the declared 36h", ""},
		{"mechanism stopped", DoctorHost{MirrorMaxAge: "36h", Mirror: at(30 * 24 * time.Hour)}, LevelFail, "30d", ""},
		{"receipt unreadable", DoctorHost{MirrorMaxAge: "36h", Mirror: &MirrorFacts{ReadErr: "unexpected end of JSON input"}}, LevelUnknown, "could not be read", ""},
		{"receipt with no time", DoctorHost{MirrorMaxAge: "36h", Mirror: &MirrorFacts{Archive: "x"}}, LevelUnknown, "no timestamp", ""},
		{"receipt from the future", DoctorHost{MirrorMaxAge: "36h", Mirror: at(-72 * time.Hour)}, LevelUnknown, "future", ""},
		{"undeclared but a run exists", DoctorHost{Mirror: at(4 * time.Hour)}, LevelUnknown, "nothing declares how fresh", "never"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := doctorCheck(t, mirrorReport(t, tc.host, now), "mirror.age")
			if c.Level != tc.want {
				t.Errorf("level = %q, want %q (%s / %s)", c.Level, tc.want, c.Summary, c.Detail)
			}
			if !strings.Contains(c.Summary+" "+c.Detail, tc.says) {
				t.Errorf("says %q + %q, want it to mention %q", c.Summary, c.Detail, tc.says)
			}
			if tc.nsays != "" && strings.Contains(c.Summary, tc.nsays) {
				t.Errorf("summary %q must not claim %q", c.Summary, tc.nsays)
			}
			if c.Level != LevelOK && c.Fix == "" {
				t.Error("a finding with no fix leaves an operator where it found them")
			}
		})
	}
}

// TestMirrorAge_UndeclaredNeverRendersAnUnusableStampAsAnAge is the
// review pass's REV-2. The undeclared branch reports the age when it has
// one — and a zero timestamp subtracted from now reads as "20599d ago",
// a future one as "less than a minute". Both are absent evidence wearing
// a value, which the declared branch already has two rungs for.
func TestMirrorAge_UndeclaredNeverRendersAnUnusableStampAsAnAge(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	for name, f := range map[string]*MirrorFacts{
		"zero stamp":   {Archive: "/mnt/backup/x.tar.gz"},
		"future stamp": {At: now.Add(72 * time.Hour)},
	} {
		c := doctorCheck(t, mirrorReport(t, DoctorHost{Mirror: f}, now), "mirror.age")
		if strings.Contains(c.Summary, " ago") {
			t.Errorf("%s: reported an age anyway: %q", name, c.Summary)
		}
		if c.Level == LevelOK {
			t.Errorf("%s: scored OK", name)
		}
	}
	// With a usable stamp it must still report the age, or the fix above
	// would have been "print less".
	c := doctorCheck(t, mirrorReport(t, DoctorHost{Mirror: &MirrorFacts{At: now.Add(-3 * time.Hour)}}, now), "mirror.age")
	if !strings.Contains(c.Summary, "3h0m ago") {
		t.Errorf("a usable stamp stopped reporting its age: %q", c.Summary)
	}
}

// TestMirrorAge_UndeclaredIsNotOK is separate from the table above for
// C16's reason: a table can drift into asserting the opposite of the
// rule it was written for. Nothing declaring a mirror must never read as
// a fleet that has one.
func TestMirrorAge_UndeclaredIsNotOK(t *testing.T) {
	now := time.Now().UTC()
	for _, host := range []DoctorHost{
		{},
		{Mirror: &MirrorFacts{At: now.Add(-time.Hour)}},
	} {
		if got := doctorCheck(t, mirrorReport(t, host, now), "mirror.age").Level; got == LevelOK {
			t.Errorf("an undeclared mirror expectation scored %q", got)
		}
	}
}

// TestMirrorAge_AFutureReceiptIsNotFresh is the repo's oldest defect
// class in this phase's shape: a clock step forward would make a stale
// mirror read as a fresh one, forever, silently.
func TestMirrorAge_AFutureReceiptIsNotFresh(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	host := DoctorHost{MirrorMaxAge: "36h", Mirror: &MirrorFacts{At: now.Add(48 * time.Hour)}}
	c := doctorCheck(t, mirrorReport(t, host, now), "mirror.age")
	if c.Level == LevelOK {
		t.Fatalf("a receipt stamped in the future scored OK: %s", c.Summary)
	}
	// A minute of skew is not a clock step; the check must not go UNKNOWN
	// over ordinary NTP jitter.
	ok := DoctorHost{MirrorMaxAge: "36h", Mirror: &MirrorFacts{At: now.Add(30 * time.Second)}}
	if got := doctorCheck(t, mirrorReport(t, ok, now), "mirror.age").Level; got != LevelOK {
		t.Errorf("30s of skew scored %q, want ok", got)
	}
}

func TestMirrorContents_ReadFailuresWarnAndAbsencesDoNot(t *testing.T) {
	now := time.Now().UTC()
	warn := DoctorHost{MirrorMaxAge: "36h", Mirror: &MirrorFacts{
		At: now.Add(-time.Hour), Files: 8, Bytes: 2048,
		Errors:  []string{"/state/vibe/fleet/usage.jsonl: permission denied"},
		Missing: []string{"fleet/leases.json (never created)"},
	}}
	c := doctorCheck(t, mirrorReport(t, warn, now), "mirror.contents")
	if c.Level != LevelWarn {
		t.Fatalf("a mirror that could not read a state file scored %q", c.Level)
	}
	if !strings.Contains(c.Detail, "usage.jsonl") {
		t.Errorf("the WARN does not name the file that is not backed up: %q", c.Detail)
	}

	clean := DoctorHost{MirrorMaxAge: "36h", Mirror: &MirrorFacts{
		At: now.Add(-time.Hour), Files: 9, Bytes: 4096,
		Missing: []string{"fleet/leases.json (never created)", "fleet/notify-scope.json (never created)"},
	}}
	c2 := doctorCheck(t, mirrorReport(t, clean, now), "mirror.contents")
	if c2.Level != LevelOK {
		t.Fatalf("a fleet with no leases has no leases.json; that scored %q", c2.Level)
	}
	if !strings.Contains(c2.Detail, "leases.json") {
		t.Errorf("absences must still be NAMED so 'absent' and 'not looked for' stay different answers: %q", c2.Detail)
	}
}

// TestMirrorContents_OnlyWhenARunExists keeps the report from carrying
// two UNKNOWNs about the same missing thing — C13's rule that a
// permanent verdict on a healthy fleet teaches the operator to ignore
// the level.
func TestMirrorContents_OnlyWhenARunExists(t *testing.T) {
	now := time.Now().UTC()
	for _, host := range []DoctorHost{
		{},
		{MirrorMaxAge: "36h"},
		{MirrorMaxAge: "36h", Mirror: &MirrorFacts{ReadErr: "boom"}},
	} {
		rep := mirrorReport(t, host, now)
		for _, c := range rep.Checks {
			if c.ID == "mirror.contents" {
				t.Errorf("mirror.contents was emitted with no readable receipt: %+v", c)
			}
		}
	}
}

// TestMirrorAge_NamesTheArchiveAndItsSensitivity: the operator reading
// this mid-incident needs to know WHERE the archive is, and that it
// carries the control-plane token.
func TestMirrorAge_NamesTheArchiveAndItsSensitivity(t *testing.T) {
	now := time.Now().UTC()
	host := DoctorHost{MirrorMaxAge: "36h", Mirror: &MirrorFacts{
		At: now.Add(-2 * time.Hour), Archive: "/mnt/nas/fleet/fleet-mirror-20260805T030000Z.tar.gz",
		Files: 12, Bytes: 3 << 20, Credentials: true,
	}}
	c := doctorCheck(t, mirrorReport(t, host, now), "mirror.age")
	if !strings.Contains(c.Detail, "/mnt/nas/fleet/") {
		t.Errorf("detail does not say where the archive is: %q", c.Detail)
	}
	if !strings.Contains(c.Detail, "control-plane token") {
		t.Errorf("an archive carrying the fleet's root credential must say so: %q", c.Detail)
	}
	if !strings.Contains(c.Detail, "3.0 MiB") {
		t.Errorf("size not rendered: %q", c.Detail)
	}
}

func TestHumanAge(t *testing.T) {
	for in, want := range map[time.Duration]string{
		30 * time.Second:                "less than a minute",
		45 * time.Minute:                "45m",
		4*time.Hour + 12*60*time.Second: "4h12m",
		30 * 24 * time.Hour:             "30d",
		-5 * time.Minute:                "less than a minute",
	} {
		if got := humanAge(in); got != want {
			t.Errorf("humanAge(%v) = %q, want %q", in, got, want)
		}
	}
}

// TestDoctorEmitsTheMirrorChecks proves the checks are wired into the
// real report rather than only reachable from this test file.
func TestDoctorEmitsTheMirrorChecks(t *testing.T) {
	host := DoctorHost{MirrorMaxAge: "36h", Mirror: &MirrorFacts{At: time.Now().UTC().Add(-time.Hour), Files: 3, Bytes: 99}}
	srv := newDoctorServer(t, host)
	rep := srv.Doctor(t.Context())
	if got := doctorCheck(t, rep, "mirror.age").Level; got != LevelOK {
		t.Errorf("mirror.age through Doctor = %q", got)
	}
	doctorCheck(t, rep, "mirror.contents")
}
