package fleetmirror

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The adversarial-review pass (ground rule 9, second reviewer). Each
// test here failed against the pre-fix code; the addendum in
// docs/design/fleet-control-plan/c19-front-failover.md records which
// production line each one is mutation-bound to.

// TestRestore_RefusesAManifestRelThatEscapesItsSlot is REV-1.
//
// `safeName` guarded Entry.Archive. The field a restore JOINS onto
// --state-dir is Entry.Rel, and nothing looked at it — so an archive
// with a harmless `archive` and a `rel` of "../../.." wrote anywhere the
// restoring user could write, with the mode the manifest asked for. An
// archive is untrusted input: it comes off a backup target, the one
// machine in this design that is neither the front nor a cell, and the
// runbook has an operator restore whichever archive `verify` liked.
func TestRestore_RefusesAManifestRelThatEscapesItsSlot(t *testing.T) {
	f := newFixture(t)
	_, rc, err := Create(f.opts())
	if err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "outside", "owned")
	rewriteArchive(t, rc.Archive, func(name string, data []byte) []byte {
		if name != ManifestName {
			return data
		}
		var m Manifest
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		for i := range m.Entries {
			if m.Entries[i].Archive == "state/token" {
				m.Entries[i].Rel = strings.Repeat("../", 12) + strings.TrimPrefix(victim, "/")
			}
		}
		return mustJSON(t, &m)
	})

	// `verify` alone must catch it: the runbook has a human run that
	// first, on the box they are about to restore onto.
	if _, err := Verify(rc.Archive); err == nil || !strings.Contains(err.Error(), "rel") {
		t.Fatalf("verify accepted a traversing rel: %v", err)
	}

	sb := newStandby(t)
	if _, err := Restore(sb.opts(rc.Archive)); err == nil {
		t.Fatal("restore accepted a traversing rel")
	}
	if _, err := os.Stat(victim); err == nil {
		t.Fatalf("the restore wrote outside the state dir at %s", victim)
	}
}

// TestRestore_PreservesTheAppendOnlyLedgerItWouldReplace is REV-2, and
// the one finding in this phase whose damage cannot be undone.
//
// C7a's ledger is append-only and irreplaceable — cells announce
// CUMULATIVE totals, so a row that goes does not come back and C7b's
// payback bars are computed from it. `--overwrite` replaced it with the
// archive's copy like any other file.
func TestRestore_PreservesTheAppendOnlyLedgerItWouldReplace(t *testing.T) {
	f := newFixture(t)
	_, rc, err := Create(f.opts())
	if err != nil {
		t.Fatal(err)
	}
	sb := newStandby(t)
	live := filepath.Join(sb.state, "fleet", "usage.jsonl")
	const rows = "{\"day\":\"2026-08-01\"}\n{\"day\":\"2026-08-02\"}\n{\"day\":\"2026-08-03\"}\n"
	write(t, live, rows, 0o644)
	// Everything else present too, so the partial-state rule (REV-5) is
	// not what this test is measuring.
	for _, rel := range []string{"token", "fleet/intent.json"} {
		write(t, filepath.Join(sb.state, filepath.FromSlash(rel)), "x", 0o600)
	}

	o := RestoreOptions{Archive: rc.Archive, StateDir: sb.state, Overwrite: true, Dial: refusingDial}
	rep, err := Restore(o)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if len(rep.Preserved) != 1 {
		t.Fatalf("the append-only ledger was replaced with an older copy and nothing was kept: preserved %v", rep.Preserved)
	}
	kept := string(read(t, rep.Preserved[0]))
	if kept != rows {
		t.Errorf("the preserved copy is not the file that was here: %q", kept)
	}
	if !strings.Contains(rep.Preserved[0], "usage.jsonl") {
		t.Errorf("preserved the wrong file: %s", rep.Preserved[0])
	}
	// ...and the archive's copy still landed, so the restore is coherent.
	if got := string(read(t, live)); !strings.Contains(got, "2026-08-01") {
		t.Errorf("the archive's ledger was not placed: %q", got)
	}
}

// TestRestore_ExtendingTheLedgerNeedsNoSidecar keeps the fix from
// becoming a file-per-restore nuisance: when what is on disk is a PREFIX
// of the archive's copy, the archive is a strict superset and nothing is
// at risk. A permanent sidecar on a correct restore is noise an operator
// learns to ignore.
func TestRestore_ExtendingTheLedgerNeedsNoSidecar(t *testing.T) {
	f := newFixture(t)
	// The fixture's ledger is one row; put a strict prefix of it on the
	// standby.
	_, rc, err := Create(f.opts())
	if err != nil {
		t.Fatal(err)
	}
	sb := newStandby(t)
	for _, rel := range []string{"token", "fleet/intent.json"} {
		write(t, filepath.Join(sb.state, filepath.FromSlash(rel)), "x", 0o600)
	}
	write(t, filepath.Join(sb.state, "fleet", "usage.jsonl"), "{\"day\":", 0o644)

	rep, err := Restore(RestoreOptions{Archive: rc.Archive, StateDir: sb.state, Overwrite: true, Dial: refusingDial})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Preserved) != 0 {
		t.Errorf("a pure extension made a sidecar anyway: %v", rep.Preserved)
	}
}

// TestRestore_RefusesWhenThereWasNothingToProbe is REV-3, and it is this
// repo's oldest defect class in the place it costs the most.
//
// The takeover refusal is C19's entire contribution to the
// two-boxes-answering problem. `fleetd_url` is `omitempty` and a fleet
// need not declare a `front` cell, so a manifest can record no dialable
// address at all — and the probe then returns the same empty hit list it
// returns when the old front is genuinely dead. Absent evidence must
// never read as a healthy value.
func TestRestore_RefusesWhenThereWasNothingToProbe(t *testing.T) {
	f := newFixture(t)
	o := f.opts()
	o.Hosts = nil // no hosts.yaml at mirror time: the identity records nothing
	_, rc, err := Create(o)
	if err != nil {
		t.Fatal(err)
	}
	sb := newStandby(t)
	rep, err := Restore(sb.opts(rc.Archive))
	if !errors.Is(err, ErrNoProbeTargets) {
		t.Fatalf("restore proceeded having probed nothing: %v", err)
	}
	if rep.Wrote != 0 {
		t.Errorf("the refusal wrote %d files", rep.Wrote)
	}
	if _, err := os.Stat(filepath.Join(sb.state, "token")); err == nil {
		t.Error("the refusal still placed the token")
	}

	// --probe-addr is the escape that is still a probe: name the old
	// front and the refusal becomes a real answer.
	withAddr := sb.opts(rc.Archive)
	// The real dialer, deliberately: deadAddr is a loopback literal, so no
	// resolver is involved, and this keeps one path in the review suite
	// where a definite negative comes off an actual socket rather than out
	// of a fake.
	withAddr.Dial = nil
	withAddr.DialTimeout = 2 * time.Second // a real dial gets a real budget
	withAddr.ProbeAddrs = []string{deadAddr(t)}
	got, err := Restore(withAddr)
	if err != nil {
		t.Fatalf("--probe-addr against a dead address: %v", err)
	}
	if len(got.Probed) != 1 {
		t.Errorf("probed = %v, want the address that was named", got.Probed)
	}

	// --force is the other escape, and it stays available.
	forced := sb.opts(rc.Archive)
	forced.Force = true
	if _, err := Restore(forced); err != nil {
		t.Fatalf("--force: %v", err)
	}
}

// TestRestore_ARecordedAddressIsStillProbed is the other half of REV-3:
// the fix must not have turned the probe off for the fleets that DO
// record an address.
func TestRestore_ARecordedAddressIsStillProbed(t *testing.T) {
	f := newFixture(t)
	_, rc, err := Create(f.opts())
	if err != nil {
		t.Fatal(err)
	}
	sb := newStandby(t)
	rep, err := Restore(sb.opts(rc.Archive))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if len(rep.Probed) != 2 {
		t.Errorf("probed = %v, want fleetd and the front", rep.Probed)
	}
}

// TestRestore_RefusesToMixTwoFleetsInOneStateDir is REV-5.
//
// Restore's own comment claimed it "never stops halfway with a state dir
// holding one fleet's token and another fleet's intent" — and the
// exists-skip produced exactly that: this box's token kept, the
// archive's intent.json written beside it. Every file parses, so nothing
// downstream ever notices; the fleetd authenticates as one fleet and
// acts on another's declarations.
func TestRestore_RefusesToMixTwoFleetsInOneStateDir(t *testing.T) {
	f := newFixture(t)
	_, rc, err := Create(f.opts())
	if err != nil {
		t.Fatal(err)
	}
	sb := newStandby(t)
	write(t, filepath.Join(sb.state, "token"), "THIS-BOXES-OWN-TOKEN", 0o600)

	rep, err := Restore(sb.opts(rc.Archive))
	if !errors.Is(err, ErrPartialState) {
		t.Fatalf("a mixed state dir was allowed: %v", err)
	}
	if rep.Wrote != 0 {
		t.Errorf("the refusal wrote %d files", rep.Wrote)
	}
	if _, err := os.Stat(filepath.Join(sb.state, "fleet", "intent.json")); err == nil {
		t.Error("this box's token now sits beside another fleet's intent")
	}
	if string(read(t, filepath.Join(sb.state, "token"))) != "THIS-BOXES-OWN-TOKEN" {
		t.Error("the refusal replaced the token it was refusing to mix with")
	}

	// --overwrite is the clean answer and must still work.
	over := sb.opts(rc.Archive)
	over.Overwrite = true
	if _, err := Restore(over); err != nil {
		t.Fatalf("--overwrite: %v", err)
	}
	if string(read(t, filepath.Join(sb.state, "token"))) != "TOKEN-VALUE-fleet-root" {
		t.Error("--overwrite did not replace the token")
	}
}

// TestRestore_WritesNothingWhenAPayloadChangedAfterVerification is
// REV-6. The re-hash lived INSIDE the write loop, so a tampered entry
// that sorted late aborted a restore which had already placed six files
// — the half-restored state dir the plan step exists to prevent, and the
// exact shape the function's comment promises cannot happen.
func TestRestore_WritesNothingWhenAPayloadChangedAfterVerification(t *testing.T) {
	f := newFixture(t)
	_, rc, err := Create(f.opts())
	if err != nil {
		t.Fatal(err)
	}
	orig := readArchiveFn
	readArchiveFn = func(archive string) (map[string][]byte, error) {
		files, err := orig(archive)
		if err != nil {
			return nil, err
		}
		// state/token sorts LAST: everything else would already be on disk.
		files["state/token"] = []byte("A-TOKEN-NOBODY-VERIFIED")
		return files, nil
	}
	t.Cleanup(func() { readArchiveFn = orig })

	sb := newStandby(t)
	rep, err := Restore(sb.opts(rc.Archive))
	if err == nil || !strings.Contains(err.Error(), "changed after verification") {
		t.Fatalf("tampered payload accepted: %v", err)
	}
	if rep.Wrote != 0 {
		t.Errorf("wrote %d files before noticing", rep.Wrote)
	}
	for _, p := range []string{
		filepath.Join(sb.state, "token"),
		filepath.Join(sb.state, "fleet", "intent.json"),
		filepath.Join(sb.state, "fleet", "usage.jsonl"),
		sb.config,
	} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("the aborted restore left %s on the standby", p)
		}
	}
}

// TestArchiveOrder_TheSecondRunOfASecondIsTheNewer is REV-7.
//
// `…Z-1.tar.gz` compares LOWER than `…Z.tar.gz` ('-' < '.'), so the
// plain lexical sort called the second run of a second the OLDEST
// archive in the directory. `--keep 1` then deleted the archive whose
// path had just been written into the receipt, and `Newest` handed a
// recovery the older of the two.
func TestArchiveOrder_TheSecondRunOfASecondIsTheNewer(t *testing.T) {
	f := newFixture(t)
	o := f.opts()
	o.Now = func() time.Time { return time.Date(2026, 8, 5, 3, 0, 0, 0, time.UTC) }
	o.Keep = 1
	if _, _, err := Create(o); err != nil {
		t.Fatal(err)
	}
	_, second, err := Create(o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(second.Archive); err != nil {
		t.Fatalf("prune deleted the archive the receipt names (%s): %v", second.Archive, err)
	}
	newest, err := Newest(f.out)
	if err != nil {
		t.Fatal(err)
	}
	if newest != second.Archive {
		t.Fatalf("Newest = %s, want the second run %s", newest, second.Archive)
	}
	// Ordinary stamps must still order by stamp.
	o2 := f.opts()
	o2.Now = func() time.Time { return time.Date(2026, 8, 6, 3, 0, 0, 0, time.UTC) }
	_, third, err := Create(o2)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := Newest(f.out); err != nil || n != third.Archive {
		t.Fatalf("Newest = %s (%v), want %s", n, err, third.Archive)
	}
}

// deadAddr returns a loopback address nothing listens on.
func deadAddr(t *testing.T) string {
	t.Helper()
	// A RESERVED address, not a recycled ephemeral one. The old shape
	// (listen on :0, read the port, close) hands the port straight back
	// to the kernel, and a test binary that stands up dozens of
	// httptest servers can be given that exact port a few microseconds
	// later — at which point the "dead" cell answers 404 and the test
	// fails for a reason that has nothing to do with what it asserts.
	// Seen once in ~25 package runs of `go test -race ./...`, which is
	// precisely the failure rate that makes a green CI meaningless.
	// Port 1 is privileged: nothing in this binary can bind it, and a
	// loopback connect gets an immediate ECONNREFUSED. It is already the
	// idiom these tests use for an unreachable front.
	_ = t
	return "127.0.0.1:1"
}
