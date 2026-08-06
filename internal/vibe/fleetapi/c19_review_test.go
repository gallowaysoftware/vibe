package fleetapi

import (
	"strings"
	"testing"
	"time"
)

// The adversarial-review pass's half of C19's doctor checks.

// TestMirrorContents_ACaptureGapIsNotOK is REV-4.
//
// The mirror's warnings list mixed two different things: advisory notes
// that are true of a correct fleet (the front is on a literal IP), and
// CAPTURE GAPS — `--no-secrets` dropping the control-plane token, no
// config dir so hosts.yaml never entered the archive, no backend defs so
// the standby cannot render. Every one of those leaves an archive that
// cannot do the thing this phase exists for, and all of them scored
// mirror.contents OK with the reason buried in a detail line. Absent
// evidence must never render as a healthy value; six occurrences in this
// plan before this one.
func TestMirrorContents_ACaptureGapIsNotOK(t *testing.T) {
	now := time.Now().UTC()
	for name, gap := range map[string]string{
		"dropped token": "--no-secrets: state/token was not captured (the control-plane bearer)",
		"no config dir": "no config dir given: hosts.yaml, config.yaml and the backend defs are NOT in this archive",
		"no defs":       "no backend defs captured: a standby restored from this archive cannot RENDER the front's config",
	} {
		t.Run(name, func(t *testing.T) {
			host := DoctorHost{MirrorMaxAge: "36h", Mirror: &MirrorFacts{
				At: now.Add(-time.Hour), Files: 6, Bytes: 2048, Gaps: []string{gap},
			}}
			c := doctorCheck(t, mirrorReport(t, host, now), "mirror.contents")
			if c.Level == LevelOK {
				t.Fatalf("an archive that cannot restore the fleet scored OK: %s / %s", c.Summary, c.Detail)
			}
			if !strings.Contains(c.Summary+" "+c.Detail, strings.Fields(gap)[0]) {
				t.Errorf("the finding does not name the gap: %q / %q", c.Summary, c.Detail)
			}
			if c.Fix == "" {
				t.Error("a finding with no fix leaves an operator where it found them")
			}
		})
	}

	// An advisory warning is NOT a gap and must stay green: a permanent
	// WARN on a correct configuration is one an operator learns to
	// ignore (C13). This fleet's front IS on a literal IP.
	ok := DoctorHost{MirrorMaxAge: "36h", Mirror: &MirrorFacts{
		At: now.Add(-time.Hour), Files: 12, Bytes: 4096,
		Warnings: []string{"the front is addressed by literal IP (http://192.0.2.7:9000)"},
	}}
	if got := doctorCheck(t, mirrorReport(t, ok, now), "mirror.contents").Level; got != LevelOK {
		t.Errorf("an advisory warning scored %q", got)
	}
}

// TestMirrorContents_AnEmptyArchiveIsNotOK is the wrong-directory
// signature: every path the mirror looked for was somewhere else, so it
// captured nothing and reported a successful run.
func TestMirrorContents_AnEmptyArchiveIsNotOK(t *testing.T) {
	now := time.Now().UTC()
	host := DoctorHost{MirrorMaxAge: "36h", Mirror: &MirrorFacts{At: now.Add(-time.Hour), Files: 0, Bytes: 0}}
	c := doctorCheck(t, mirrorReport(t, host, now), "mirror.contents")
	if c.Level == LevelOK {
		t.Fatalf("an archive with no files scored OK: %s", c.Summary)
	}
	if c.Fix == "" {
		t.Error("no fix")
	}
}
