package fleetapi

// C13 coverage: `vibe fleet doctor`. The rules a later agent will
// otherwise get wrong, in order of how badly they fail: the doctor path
// writes nothing, UNKNOWN is never spelled the same way as OK, and a
// check's verdict follows the evidence rather than the wish.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
)

// doctorServer builds a fleetd-role Server with every store enabled, so
// a mutation anywhere in the doctor path has somewhere to show up.
func doctorServer(t *testing.T, cells ...Cell) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	if len(cells) == 0 {
		cells = []Cell{{Name: fleetcfg.FrontCell, URL: "http://127.0.0.1:1"}}
	}
	s := New(cells, filepath.Join(dir, "hist.json"), testDaemonInfo, Options{
		IntentPath:      filepath.Join(dir, "intent.json"),
		LastSeenPath:    filepath.Join(dir, "last-seen.json"),
		LeasePath:       filepath.Join(dir, "leases.json"),
		UsagePath:       filepath.Join(dir, "usage.jsonl"),
		NotifyScopePath: filepath.Join(dir, "notify-scope.json"),
	})
	s.baseBackoff = 10 * time.Millisecond
	s.maxBackoff = 50 * time.Millisecond
	t.Cleanup(s.Close)
	return s, dir
}

func checkByID(rep DoctorReport, id, cell string) (DoctorCheck, bool) {
	for _, c := range rep.Checks {
		if c.ID == id && c.Cell == cell {
			return c, true
		}
	}
	return DoctorCheck{}, false
}

func mustCheck(t *testing.T, rep DoctorReport, id, cell string) DoctorCheck {
	t.Helper()
	c, ok := checkByID(rep, id, cell)
	if !ok {
		var have []string
		for _, x := range rep.Checks {
			have = append(have, x.ID+"/"+x.Cell)
		}
		t.Fatalf("no check %s/%s in the report; have %v", id, cell, have)
	}
	return c
}

// TestDoctor_WritesNoFleetState is the phase's headline promise, asserted
// the only way that survives code nobody has written yet: hash the state
// files and snapshot the queues, run the whole report, and demand every
// one of them is untouched. A grep test (below) guards the same rule
// structurally; this one is the proof.
func TestDoctor_WritesNoFleetState(t *testing.T) {
	s, dir := doctorServer(t, Cell{Name: fleetcfg.FrontCell, URL: "http://127.0.0.1:1"},
		Cell{Name: "heavy", URL: "http://127.0.0.1:1", Class: "always_on"})
	// Populate every store so "unchanged" is a real claim rather than
	// four empty files staying empty.
	presenceOf(s, "heavy", AnnounceModel{ID: "qwen", State: "ready"})
	if _, err := s.SetIntent("heavy", "drained", "gaming", "23:00"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetHold("heavy", "qwen", "evaluating", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueCommand("heavy", AnnounceCommand{Verb: "unload", Model: "qwen"}); err != nil {
		t.Fatal(err)
	}
	s.usage.fold("heavy", &AnnounceUsage{Epoch: "e1", Models: []AnnounceUsageModel{
		{Model: "qwen", Basis: "chat", Req: 3, InFresh: 100, Out: 50},
	}}, time.Now())
	if err := s.usage.flush(); err != nil {
		t.Fatal(err)
	}

	before := snapshotDir(t, dir)
	cmdsBefore := queuedCommands(s, "heavy")
	leasesBefore := activeLeaseCount(s)

	rep := s.Doctor(context.Background())
	if len(rep.Checks) == 0 {
		t.Fatal("empty report: this test would pass vacuously")
	}

	for name, sum := range before {
		// last-seen.json is the documented exception: a state READ records
		// a sighting, and doctor reads state through the same Snapshot
		// every other surface renders (C9's one-document rule). Everything
		// that is fleet STATE must be byte-identical.
		if name == "last-seen.json" {
			continue
		}
		if now := fileSum(t, filepath.Join(dir, name)); now != sum {
			t.Errorf("%s changed across a doctor run — the doctor path wrote fleet state", name)
		}
	}
	if got := queuedCommands(s, "heavy"); got != cmdsBefore {
		t.Errorf("queued commands = %d, was %d: doctor queued or drained a piggyback verb", got, cmdsBefore)
	}
	if got := activeLeaseCount(s); got != leasesBefore {
		t.Errorf("leases = %d, was %d", got, leasesBefore)
	}
	if got := s.RenderCount(); got != 0 {
		t.Errorf("front renders = %d: doctor triggered a render", got)
	}
}

func snapshotDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		out[e.Name()] = fileSum(t, filepath.Join(dir, e.Name()))
	}
	if len(out) == 0 {
		t.Fatal("no state files to compare; the fixture did not write any")
	}
	return out
}

func fileSum(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return "MISSING"
	}
	return string(data)
}

func activeLeaseCount(s *Server) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.activeLeasesLocked())
}

func queuedCommands(s *Server, cell string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.commands[cell]) + len(s.cmdInflight[cell].cmds)
}

// TestDoctor_ReachesNoMutatingVerb is the drift guard: the behavioural
// test above proves today's code writes nothing, and this one makes the
// next agent's addition fail loudly rather than quietly. Same idiom as
// C12's mux.HandleFunc scan and C7a's Truncate scan.
func TestDoctor_ReachesNoMutatingVerb(t *testing.T) {
	banned := map[string]string{
		"SetIntent":         "writes axis 2",
		"setIntent":         "writes axis 2",
		"persistIntents":    "writes the intent file",
		"saveIntents":       "writes the intent file",
		"SetHold":           "takes a lease",
		"ReleaseHold":       "deletes a lease",
		"setLease":          "writes the lease store",
		"saveLeases":        "writes the lease store",
		"QueueCommand":      "queues a piggyback verb",
		"queueCommand":      "queues a piggyback verb",
		"queueWarm":         "queues a warm",
		"warmViaFront":      "issues a warm",
		"drainCommands":     "hands a cell its queued verbs",
		"renderPass":        "writes the front config",
		"writeAtomic":       "writes a file",
		"WriteFile":         "writes a file",
		"Create":            "creates a file",
		"CreateTemp":        "creates a file",
		"Enqueue":           "sends a notification",
		"recordAnnounce":    "mutates presence",
		"noteRenderTrigger": "triggers a render",
		// The RPC verbs. The daemon half of this path holds a vibeclient,
		// which is the one place in the whole command where reaching for an
		// actuation call is a one-line edit — and the behavioural test
		// cannot see it, because the prober is injected and the fixture
		// injects a fake.
		"CellDrain":  "drains a cell",
		"CellResume": "resumes a cell",
		"Activate":   "activates a profile",
		"Deactivate": "stops a profile",
		"Shutdown":   "stops a daemon",
	}
	// Every file on the doctor path, not just this one. The scan used to
	// cover fleetapi/doctor.go alone, which is the file LEAST able to
	// mutate anything off-box.
	for _, path := range []string{
		"doctor.go",
		// C16's two checks live beside the rest of the ritual rather than
		// in doctor.go, and one of them dials the front. A file that
		// contributes checks to the report is on the doctor path whatever
		// it is called.
		"upgrade.go",
		"../daemon/doctor.go",
		"../fleetmcp/doctor.go",
		"../cli/cmd_fleet_doctor.go",
	} {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var name string
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				name = fn.Name
			case *ast.SelectorExpr:
				name = fn.Sel.Name
			}
			if why, bad := banned[name]; bad {
				t.Errorf("%s calls %s (%s). Doctor is read-only and safe to run mid-incident; if a check "+
					"genuinely needs this, the phase doc's §1 promise has to change first.",
					fset.Position(call.Pos()), name, why)
			}
			return true
		})
	}
}

// TestDoctor_EveryCheckDeclaresALevel: Level's zero value is not a
// verdict, and a check that forgot one must not read as OK.
func TestDoctor_EveryCheckDeclaresALevel(t *testing.T) {
	s, _ := doctorServer(t, Cell{Name: fleetcfg.FrontCell, URL: "http://127.0.0.1:1"},
		Cell{Name: "heavy", URL: "http://127.0.0.1:1", Class: "roaming", HostProbe: "127.0.0.1:1"})
	rep := s.Doctor(context.Background())
	seen := map[Level]bool{}
	for _, c := range rep.Checks {
		switch c.Level {
		case LevelOK, LevelWarn, LevelFail, LevelUnknown:
			seen[c.Level] = true
		default:
			t.Errorf("check %s/%s has level %q — the zero value is not a verdict", c.ID, c.Cell, c.Level)
		}
		if c.Summary == "" {
			t.Errorf("check %s/%s has no summary", c.ID, c.Cell)
		}
	}
	if rep.Summary.Checks != len(rep.Checks) {
		t.Errorf("summary counts %d checks, report has %d", rep.Summary.Checks, len(rep.Checks))
	}
	if got := rep.Summary.OK + rep.Summary.Warn + rep.Summary.Fail + rep.Summary.Unknown; got != len(rep.Checks) {
		t.Errorf("levels sum to %d, report has %d checks", got, len(rep.Checks))
	}
}

// TestDoctor_InboundAuthReadsTheAnnounceAsTheCredentialTest: the
// heartbeat IS the proof, because announce is bearer-authed — a cell
// whose token is wrong never reaches the presence table.
func TestDoctor_InboundAuthReadsTheAnnounceAsTheCredentialTest(t *testing.T) {
	s, _ := doctorServer(t, Cell{Name: fleetcfg.FrontCell, URL: "http://127.0.0.1:1"},
		Cell{Name: "fresh", URL: "http://127.0.0.1:1", Class: "always_on"},
		Cell{Name: "silent", URL: "http://127.0.0.1:1", Class: "always_on"},
		Cell{Name: "gone", URL: "http://127.0.0.1:1", Class: "always_on"})
	presenceOf(s, "fresh", AnnounceModel{ID: "qwen", State: "ready"})
	presenceOf(s, "gone")
	s.mu.Lock()
	s.presence["gone"].Stale = true
	s.mu.Unlock()

	rep := s.Doctor(context.Background())
	if got := mustCheck(t, rep, "auth.inbound", "fresh").Level; got != LevelOK {
		t.Errorf("fresh announce → %s, want ok", got)
	}
	if got := mustCheck(t, rep, "auth.inbound", "gone").Level; got != LevelWarn {
		t.Errorf("stale announcer → %s, want warn", got)
	}
	silent := mustCheck(t, rep, "auth.inbound", "silent")
	if silent.Level != LevelUnknown {
		t.Errorf("never announced → %s, want unknown: an unauthenticated probe proves nothing about a credential", silent.Level)
	}
	// The front is the one cell whose silence is expected in the
	// reference deployment.
	if got := mustCheck(t, rep, "auth.inbound", fleetcfg.FrontCell).Level; got != LevelOK {
		t.Errorf("front → %s, want ok (it does not announce and holds no inbound credential)", got)
	}
}

// TestDoctor_InboundUnknownNamesTheUnattributableRejections: fleetd
// cannot say WHICH caller 401'd, and reporting the ambiguity is the
// honest form — inventing an attribution is not.
func TestDoctor_InboundUnknownNamesTheUnattributableRejections(t *testing.T) {
	dir := t.TempDir()
	s := New([]Cell{{Name: fleetcfg.FrontCell, URL: "http://127.0.0.1:1"}, {Name: "silent", URL: "http://127.0.0.1:1"}},
		filepath.Join(dir, "h.json"),
		func() DaemonInfo { return DaemonInfo{AuthRejected: 412} },
		Options{IntentPath: filepath.Join(dir, "intent.json")})
	t.Cleanup(s.Close)
	rep := s.Doctor(context.Background())
	got := mustCheck(t, rep, "auth.inbound", "silent")
	if !strings.Contains(got.Detail, "412") {
		t.Errorf("detail = %q, want the rejection count named as a possible explanation", got.Detail)
	}
	if lvl := mustCheck(t, rep, "auth.rejections", "").Level; lvl != LevelWarn {
		t.Errorf("auth.rejections with 412 refusals → %s, want warn", lvl)
	}
}

// TestDoctor_OutboundAuthSeparatesRefusalFromAbsence: a 401 is the far
// side answering; a timeout is the absence of an answer. Collapsing them
// reports a box that is off as a wrong password.
func TestDoctor_OutboundAuthSeparatesRefusalFromAbsence(t *testing.T) {
	cells := []Cell{
		{Name: fleetcfg.FrontCell, URL: "http://127.0.0.1:1"},
		{Name: "ok-cell", URL: "http://127.0.0.1:1"},
		{Name: "denied", URL: "http://127.0.0.1:1"},
		{Name: "offline", URL: "http://127.0.0.1:1"},
		{Name: "broken-cfg", URL: "http://127.0.0.1:1"},
		{Name: "announce-only", URL: "http://127.0.0.1:1"},
		{Name: "unreachable-either-way", URL: "http://127.0.0.1:1"},
	}
	dir := t.TempDir()
	s := New(cells, filepath.Join(dir, "h.json"), testDaemonInfo, Options{
		IntentPath: filepath.Join(dir, "intent.json"),
		CellAuth: func(_ context.Context, cell string) CellAuthResult {
			switch cell {
			case "ok-cell":
				return CellAuthResult{Attempted: true, OK: true, Source: "cells.ok-cell.token_file (/x)"}
			case "denied":
				return CellAuthResult{Attempted: true, Denied: true, Source: "cells.denied.token_file (/y)", Err: "unauthenticated"}
			case "offline":
				return CellAuthResult{Attempted: true, Source: "$VIBE_TOKEN", Err: "dial tcp: connection refused"}
			case "broken-cfg":
				return CellAuthResult{Attempted: true, CredentialErr: "read cells.broken-cfg.token_file /z: no such file"}
			}
			return CellAuthResult{}
		},
	})
	t.Cleanup(s.Close)
	presenceOf(s, "announce-only")

	rep := s.Doctor(context.Background())
	for _, tc := range []struct {
		cell string
		want Level
	}{
		{"ok-cell", LevelOK},
		{"denied", LevelFail},
		{"offline", LevelUnknown},
		{"broken-cfg", LevelFail},
		// After C3 a cell with no daemon_url is correctly configured, and
		// its inbound half is proven fresh: everything configured works.
		{"announce-only", LevelOK},
		// No daemon_url AND no announce: neither direction has a proven
		// credential path, which is genuinely unknown.
		{"unreachable-either-way", LevelUnknown},
	} {
		if got := mustCheck(t, rep, "auth.outbound", tc.cell).Level; got != tc.want {
			t.Errorf("%s → %s, want %s", tc.cell, got, tc.want)
		}
	}
	if det := mustCheck(t, rep, "auth.outbound", "ok-cell").Detail; !strings.Contains(det, "token_file") {
		t.Errorf("detail = %q, want the credential SOURCE named (never its value)", det)
	}
}

// TestDoctor_OutboundAuthWithNoProberIsUnknownNotOK: a fleetd built
// without the prober must not report a credential it never tried.
func TestDoctor_OutboundAuthWithNoProberIsUnknownNotOK(t *testing.T) {
	s, _ := doctorServer(t, Cell{Name: fleetcfg.FrontCell, URL: "http://127.0.0.1:1"},
		Cell{Name: "heavy", URL: "http://127.0.0.1:1"})
	presenceOf(s, "heavy")
	rep := s.Doctor(context.Background())
	if got := mustCheck(t, rep, "auth.outbound", "heavy").Level; got != LevelUnknown {
		t.Errorf("no prober wired → %s, want unknown", got)
	}
}

// TestDoctor_DefsParityAgreementRestsOnCleanCheckoutsOnly: a dirty
// working tree's SHA does not describe what is running, so a verdict of
// AGREEMENT may not rest on it. Counting it as agreement because the
// string matches is the absent-evidence mistake this phase exists to
// avoid.
//
// The name and the comment used to say "treats a dirty checkout as
// UNCOMPARABLE", which the 2026-08-05 live gate showed is only half
// true and the wrong half to state as a rule: a dirty tree can vouch
// for nobody, but it can still DISAGREE, and dropping it from the
// comparison flipped a real divergence to OK. Divergence is decided
// over every cell that reports a SHA — see
// defsparity_test.go, which pins that half. Ground rule 10: a test's
// name is part of its assertion.
func TestDoctor_DefsParityAgreementRestsOnCleanCheckoutsOnly(t *testing.T) {
	t.Run("agreement", func(t *testing.T) {
		s := versionFleet(t, map[string]*AnnounceVersions{
			"a": {DefsSHA: "abc123", Vibe: "v1"},
			"b": {DefsSHA: "abc123", Vibe: "v1"},
		})
		got := mustCheck(t, s.Doctor(context.Background()), "defs.parity", "")
		if got.Level != LevelOK {
			t.Errorf("identical SHAs → %s (%s)", got.Level, got.Detail)
		}
	})
	t.Run("divergence", func(t *testing.T) {
		s := versionFleet(t, map[string]*AnnounceVersions{
			"a": {DefsSHA: "abc123"},
			"b": {DefsSHA: "def456"},
		})
		got := mustCheck(t, s.Doctor(context.Background()), "defs.parity", "")
		if got.Level != LevelWarn {
			t.Errorf("divergent SHAs → %s", got.Level)
		}
	})
	t.Run("dirty is not agreement", func(t *testing.T) {
		s := versionFleet(t, map[string]*AnnounceVersions{
			"a": {DefsSHA: "abc123", DefsDirty: true},
			"b": {DefsSHA: "abc123", DefsDirty: true},
		})
		got := mustCheck(t, s.Doctor(context.Background()), "defs.parity", "")
		if got.Level != LevelUnknown {
			t.Errorf("two dirty checkouts reporting the same SHA → %s, want unknown", got.Level)
		}
		if !strings.Contains(got.Detail, "dirty") {
			t.Errorf("detail = %q, want the reason named", got.Detail)
		}
	})
	t.Run("no versions block at all", func(t *testing.T) {
		s := versionFleet(t, map[string]*AnnounceVersions{"a": nil, "b": nil})
		rep := s.Doctor(context.Background())
		if got := mustCheck(t, rep, "defs.parity", "").Level; got != LevelUnknown {
			t.Errorf("nothing reported → %s, want unknown", got)
		}
		if got := mustCheck(t, rep, "versions.reported", "a").Level; got != LevelUnknown {
			t.Errorf("cell with no versions block → %s, want unknown", got)
		}
	})
}

// versionFleet builds a fleet whose cells announce the given versions
// blocks (nil means "announces, but carries no block").
func versionFleet(t *testing.T, versions map[string]*AnnounceVersions) *Server {
	t.Helper()
	cells := []Cell{{Name: fleetcfg.FrontCell, URL: "http://127.0.0.1:1"}}
	names := make([]string, 0, len(versions))
	for n := range versions {
		names = append(names, n)
	}
	for _, n := range names {
		cells = append(cells, Cell{Name: n, URL: "http://127.0.0.1:1", Class: "always_on"})
	}
	s, _ := doctorServer(t, cells...)
	for _, n := range names {
		s.recordAnnounce(&AnnounceRequest{V: AnnounceVersion, Cell: n, Seq: 1,
			Intent:   &AnnounceIntent{State: "serving", Since: time.Now().UTC()},
			Versions: versions[n]})
	}
	return s
}

// TestDoctor_LlamaSwapMatrixIsUnknownWhenNobodyAnswers: a silent OK would
// claim a uniform fleet nobody measured. C13 shipped this UNKNOWN naming
// the MISSING producer; C16 supplied one (each cell reads its own
// llama-swap's /api/version), so an empty matrix now means nobody
// ANSWERED — which is still not agreement, and the detail has to say so.
func TestDoctor_LlamaSwapMatrixIsUnknownWhenNobodyAnswers(t *testing.T) {
	s := versionFleet(t, map[string]*AnnounceVersions{
		"a": {DefsSHA: "abc123"},
		"b": {DefsSHA: "abc123"},
	})
	got := mustCheck(t, s.Doctor(context.Background()), "versions.llama_swap", "")
	if got.Level != LevelUnknown {
		t.Fatalf("no cell reports a version → %s, want unknown", got.Level)
	}
	if !strings.Contains(got.Detail, "not that the fleet agrees") {
		t.Errorf("detail = %q, want the absence stated so the unknown is not read as agreement", got.Detail)
	}

	// Two versions this build HAS recordings for: the mid-state of a roll,
	// which is a warn about divergence and nothing worse. (A version with
	// no recording is a different, louder branch — see c16_test.go.)
	s2 := versionFleet(t, map[string]*AnnounceVersions{
		"a": {LlamaSwap: "v239"},
		"b": {LlamaSwap: "v247"},
	})
	got2 := mustCheck(t, s2.Doctor(context.Background()), "versions.llama_swap", "")
	if got2.Level != LevelWarn {
		t.Errorf("mixed llama-swap versions → %s, want warn (the upgrade ritual's mid-state)", got2.Level)
	}
	if !strings.Contains(got2.Summary, "different llama-swap versions") {
		t.Errorf("summary = %q, want the divergence branch rather than the ungated one", got2.Summary)
	}
}

// TestDoctor_TLSReadsNotAfterAndSaysTheChainIsUnverified. A LAN fleet's
// certs are self-signed; failing them would paint a permanent red that
// means nothing, so the check reads an expiry and says so.
func TestDoctor_TLSReadsNotAfterAndSaysTheChainIsUnverified(t *testing.T) {
	t.Run("no https endpoints is a definitive negative", func(t *testing.T) {
		s, _ := doctorServer(t)
		s.hosts = &fleetcfg.File{Cells: map[string]fleetcfg.Cell{
			fleetcfg.FrontCell: {URL: "http://front.lan:9000"},
		}}
		got := mustCheck(t, s.Doctor(context.Background()), "tls.not_after", "")
		if got.Level != LevelOK {
			t.Errorf("no certs anywhere → %s, want ok: there is nothing that can expire", got.Level)
		}
	})
	t.Run("expiring cert", func(t *testing.T) {
		srv := tlsServerExpiring(t, 5*24*time.Hour)
		s, _ := doctorServer(t)
		s.hosts = &fleetcfg.File{Cells: map[string]fleetcfg.Cell{
			fleetcfg.FrontCell: {URL: srv.URL},
		}}
		got := mustCheck(t, s.Doctor(context.Background()), "tls.not_after", fleetcfg.FrontCell)
		if got.Level != LevelWarn {
			t.Fatalf("cert 5 days out → %s (%s / %s)", got.Level, got.Summary, got.Detail)
		}
		if !strings.Contains(got.Detail, "NOT verified") {
			t.Errorf("detail = %q, want the unverified chain stated so OK cannot read as 'trusted'", got.Detail)
		}
	})
	t.Run("expired cert", func(t *testing.T) {
		srv := tlsServerExpiring(t, -time.Hour)
		s, _ := doctorServer(t)
		s.hosts = &fleetcfg.File{Cells: map[string]fleetcfg.Cell{fleetcfg.FrontCell: {URL: srv.URL}}}
		if got := mustCheck(t, s.Doctor(context.Background()), "tls.not_after", fleetcfg.FrontCell).Level; got != LevelFail {
			t.Errorf("expired cert → %s, want fail", got)
		}
	})
	t.Run("unreachable endpoint", func(t *testing.T) {
		s, _ := doctorServer(t)
		s.hosts = &fleetcfg.File{Cells: map[string]fleetcfg.Cell{
			fleetcfg.FrontCell: {URL: "https://127.0.0.1:1"},
		}}
		if got := mustCheck(t, s.Doctor(context.Background()), "tls.not_after", fleetcfg.FrontCell).Level; got != LevelUnknown {
			t.Errorf("dial failure → %s, want unknown (off box vs broken TLS are the same observation)", got)
		}
	})
}

// tlsServerExpiring serves TLS with a self-signed leaf whose NotAfter is
// `in` from now (negative = already expired).
func tlsServerExpiring(t *testing.T, in time.Duration) *httptest.Server {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "fleet-doctor-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(in),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// TestDoctor_AnnouncerAgentIsDerivedFromHostUpPlusSilence is the one
// genuine diagnosis in the report: fleetd just reached the box at L4 and
// the box is not heartbeating, so the ANNOUNCER is the broken part —
// which otherwise hides behind OFF/AWAY? for a week.
func TestDoctor_AnnouncerAgentIsDerivedFromHostUpPlusSilence(t *testing.T) {
	up := probeableHost(t)
	t.Run("host up, no announce", func(t *testing.T) {
		s, _ := doctorServer(t, Cell{Name: fleetcfg.FrontCell, URL: "http://127.0.0.1:1"},
			Cell{Name: "mac", URL: "http://127.0.0.1:1", Class: "roaming", HostProbe: up})
		got := mustCheck(t, s.Doctor(context.Background()), "roaming.announcer", "mac")
		if got.Level != LevelWarn {
			t.Fatalf("host up + silence → %s, want warn", got.Level)
		}
		if !strings.Contains(got.Detail, "not loaded") && !strings.Contains(got.Detail, "not running") {
			t.Errorf("detail = %q, want the announcer named as the broken part", got.Detail)
		}
	})
	t.Run("host up, announcing", func(t *testing.T) {
		s, _ := doctorServer(t, Cell{Name: fleetcfg.FrontCell, URL: "http://127.0.0.1:1"},
			Cell{Name: "mac", URL: "http://127.0.0.1:1", Class: "roaming", HostProbe: up})
		presenceOf(s, "mac")
		if got := mustCheck(t, s.Doctor(context.Background()), "roaming.announcer", "mac").Level; got != LevelOK {
			t.Errorf("host up + fresh announce → %s, want ok (the agent is loaded, proven)", got)
		}
	})
	t.Run("host down", func(t *testing.T) {
		s, _ := doctorServer(t, Cell{Name: fleetcfg.FrontCell, URL: "http://127.0.0.1:1"},
			Cell{Name: "mac", URL: "http://127.0.0.1:1", Class: "roaming", HostProbe: "127.0.0.1:1"})
		if got := mustCheck(t, s.Doctor(context.Background()), "roaming.announcer", "mac").Level; got != LevelUnknown {
			t.Errorf("box away → %s, want unknown", got)
		}
	})
	t.Run("no host_probe on a roaming cell", func(t *testing.T) {
		s, _ := doctorServer(t, Cell{Name: fleetcfg.FrontCell, URL: "http://127.0.0.1:1"},
			Cell{Name: "mac", URL: "http://127.0.0.1:1", Class: "roaming"})
		got := mustCheck(t, s.Doctor(context.Background()), "roaming.announcer", "mac")
		if got.Level != LevelUnknown {
			t.Errorf("no host_probe → %s, want unknown", got)
		}
		if got.Fix == "" {
			t.Error("an unknown a config change can answer must say so")
		}
	})
}

// probeableHost returns a host:port that accepts TCP for the test's
// lifetime — the "host is up" half of the derivation.
func probeableHost(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	return ln.Addr().String()
}

// TestDoctor_WakeChecksTheCONFIGAndSaysSo: wake.configured, not
// wake.armed. Sending a packet to find out is a mutation, and the NIC's
// state is not observable from the control plane — the fire drill is
// that test.
func TestDoctor_WakeChecksTheCONFIGAndSaysSo(t *testing.T) {
	s, _ := doctorServer(t, Cell{Name: fleetcfg.FrontCell, URL: "http://127.0.0.1:1"},
		Cell{Name: "armed", URL: "http://127.0.0.1:1", Class: "opportunistic",
			Wake: &WakeSpec{MAC: "aa:bb:cc:dd:ee:ff"}},
		Cell{Name: "stranded", URL: "http://127.0.0.1:1", Class: "opportunistic"})
	rep := s.Doctor(context.Background())
	armed := mustCheck(t, rep, "wake.configured", "armed")
	if armed.Level != LevelOK {
		t.Errorf("wake configured → %s", armed.Level)
	}
	if !strings.Contains(armed.Detail, "not observable") {
		t.Errorf("detail = %q, want the untestable half stated", armed.Detail)
	}
	if got := mustCheck(t, rep, "wake.configured", "stranded").Level; got != LevelWarn {
		t.Errorf("absent cell with no wake path → %s, want warn", got)
	}
	for _, c := range rep.Checks {
		if c.ID == "wake.armed" {
			t.Error("a check named wake.armed would claim something the control plane cannot see")
		}
	}
}

// TestDoctor_ProbeVerdictsWithNoMeasurementAreUnknown: friction pain 2 is
// that llama-server rots 10-100x while /health stays green. "No degraded
// model" from a fleet that measures nothing is not evidence of health.
func TestDoctor_ProbeVerdictsWithNoMeasurementAreUnknown(t *testing.T) {
	s, _ := doctorServer(t, Cell{Name: fleetcfg.FrontCell, URL: "http://127.0.0.1:1"},
		Cell{Name: "heavy", URL: "http://127.0.0.1:1"})
	presenceOf(s, "heavy", AnnounceModel{ID: "qwen", State: "ready"})
	if got := mustCheck(t, s.Doctor(context.Background()), "probe.verdicts", "").Level; got != LevelUnknown {
		t.Errorf("nothing measured → %s, want unknown", got)
	}

	s2, _ := doctorServer(t, Cell{Name: fleetcfg.FrontCell, URL: "http://127.0.0.1:1"},
		Cell{Name: "heavy", URL: "http://127.0.0.1:1"})
	presenceOf(s2, "heavy", AnnounceModel{ID: "qwen", State: "ready",
		Probe: &AnnounceProbe{Kind: "chat", Metric: "decode_tok_s", Value: 40, Verdict: VerdictOK, At: time.Now()}})
	if got := mustCheck(t, s2.Doctor(context.Background()), "probe.verdicts", "").Level; got != LevelOK {
		t.Errorf("a measured ok verdict → %s, want ok", got)
	}
}

// TestDoctor_FingerprintDriftIsUnknownWithoutARenderLoop mirrors C9's
// fingerprint_source rule: with no render pass the mismatch set is
// permanently empty, and a silent zero would read as "no drift".
func TestDoctor_FingerprintDriftIsUnknownWithoutARenderLoop(t *testing.T) {
	s, _ := doctorServer(t)
	rep := s.Doctor(context.Background())
	if got := mustCheck(t, rep, "fingerprint.drift", "").Level; got != LevelUnknown {
		t.Errorf("no render loop → %s, want unknown", got)
	}
	if got := mustCheck(t, rep, "front.render_mount", "").Level; got != LevelWarn {
		t.Errorf("no front_config → %s, want warn (no presence-derived catalog, no enforcement, no alarm evaluator)", got)
	}
}

// TestDoctor_LeaseOutlivingADayIsFlagged: a lease suppresses that cell's
// scheduled warms and its probes for as long as it lives, and the
// eleven-day-old batch marker is the archetypal two-weeks-later finding.
func TestDoctor_LeaseOutlivingADayIsFlagged(t *testing.T) {
	s, _ := doctorServer(t, Cell{Name: fleetcfg.FrontCell, URL: "http://127.0.0.1:1"},
		Cell{Name: "heavy", URL: "http://127.0.0.1:1"})
	if err := s.putLease(Lease{Cell: "heavy", Model: "qwen", Holder: "overnight-batch",
		Note: "sweep", ExpiresAt: time.Now().Add(100 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if got := mustCheck(t, s.Doctor(context.Background()), "leases.age", "").Level; got != LevelWarn {
		t.Errorf("100h lease → %s, want warn", got)
	}
}

// TestDoctor_ReportRoundTripsAsJSON: the CLI and the MCP tool both carry
// this document over the wire, so every field an operator reads must
// survive marshalling.
func TestDoctor_ReportRoundTripsAsJSON(t *testing.T) {
	s, _ := doctorServer(t, Cell{Name: fleetcfg.FrontCell, URL: "http://127.0.0.1:1"},
		Cell{Name: "heavy", URL: "http://127.0.0.1:1"})
	rep := s.Doctor(context.Background())
	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var back DoctorReport
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Checks) != len(rep.Checks) || back.Summary != rep.Summary {
		t.Fatalf("round trip lost data: %d/%d checks, summary %+v vs %+v",
			len(back.Checks), len(rep.Checks), back.Summary, rep.Summary)
	}
	for i := range rep.Checks {
		if back.Checks[i] != rep.Checks[i] {
			t.Errorf("check %d differs: %+v vs %+v", i, back.Checks[i], rep.Checks[i])
		}
	}
}

// TestDoctor_SortBySeverityPutsTheFindingFirst: a tired human reads
// top-down and stops when they find the thing.
func TestDoctor_SortBySeverityPutsTheFindingFirst(t *testing.T) {
	rep := DoctorReport{}
	rep.Add(DoctorCheck{ID: "a", Level: LevelOK})
	rep.Add(DoctorCheck{ID: "b", Level: LevelUnknown})
	rep.Add(DoctorCheck{ID: "c", Level: LevelWarn})
	rep.Add(DoctorCheck{ID: "d", Level: LevelFail})
	rep.Add(DoctorCheck{ID: "e", Level: LevelOK})
	rep.SortBySeverity()
	var order []string
	for _, c := range rep.Checks {
		order = append(order, c.ID)
	}
	want := []string{"d", "c", "b", "a", "e"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v (fail, warn, unknown, then ok — stable within a level)", order, want)
		}
	}
	if rep.Summary.Fail != 1 || rep.Summary.Warn != 1 || rep.Summary.Unknown != 1 || rep.Summary.OK != 2 {
		t.Errorf("summary = %+v", rep.Summary)
	}
}

// TestDoctor_DiskReportsEachFilesystemSeparately: fleetd is its own
// container and may not share a host with the front, so its disk is not
// the front's. The front row exists only when fleet.front_config
// declares the shared mount.
func TestDoctor_DiskReportsEachFilesystemSeparately(t *testing.T) {
	dir := t.TempDir()
	s := New([]Cell{{Name: fleetcfg.FrontCell, URL: "http://127.0.0.1:1"}, {Name: "heavy", URL: "http://127.0.0.1:1"}},
		filepath.Join(dir, "h.json"), testDaemonInfo, Options{
			IntentPath: filepath.Join(dir, "intent.json"),
			DoctorHost: func() DoctorHost {
				return DoctorHost{StateDir: dir, DiskFreeBytes: func(string) (uint64, error) { return 500 << 30, nil }}
			},
		})
	t.Cleanup(s.Close)
	presenceOf(s, "heavy")
	rep := s.Doctor(context.Background())
	if _, ok := checkByID(rep, "disk.free", fleetcfg.FrontCell); ok {
		t.Error("reported the front's disk with no fleet.front_config: fleetd's filesystem is not the front's")
	}
	if got := mustCheck(t, rep, "fleetd.state_dir", "").Level; got != LevelOK {
		t.Errorf("500 GiB free → %s", got)
	}
	// The cell announced no capacity block: unknown, never a healthy zero.
	if got := mustCheck(t, rep, "disk.free", "heavy").Level; got != LevelUnknown {
		t.Errorf("cell with no announced capacity → %s, want unknown", got)
	}

	full := New([]Cell{{Name: fleetcfg.FrontCell, URL: "http://127.0.0.1:1"}},
		filepath.Join(dir, "h2.json"), testDaemonInfo, Options{
			IntentPath: filepath.Join(dir, "intent2.json"),
			DoctorHost: func() DoctorHost {
				return DoctorHost{
					StateDir:      dir,
					FrontConfig:   filepath.Join(dir, "front", "config.yaml"),
					DiskFreeBytes: func(string) (uint64, error) { return 1 << 20, nil },
				}
			},
		})
	t.Cleanup(full.Close)
	rep2 := full.Doctor(context.Background())
	if got := mustCheck(t, rep2, "disk.free", fleetcfg.FrontCell).Level; got != LevelFail {
		t.Errorf("1 MiB free on the render mount → %s, want fail", got)
	}
}

// TestDoctor_MintedTokenPlusRejectionsIsTheUnmountedVolumeSignature.
func TestDoctor_MintedTokenPlusRejectionsIsTheUnmountedVolumeSignature(t *testing.T) {
	dir := t.TempDir()
	s := New([]Cell{{Name: fleetcfg.FrontCell, URL: "http://127.0.0.1:1"}},
		filepath.Join(dir, "h.json"),
		func() DaemonInfo { return DaemonInfo{AuthRejected: 57} },
		Options{
			IntentPath: filepath.Join(dir, "intent.json"),
			DoctorHost: func() DoctorHost { return DoctorHost{StateDir: dir, TokenMinted: true} },
		})
	t.Cleanup(s.Close)
	got := mustCheck(t, s.Doctor(context.Background()), "fleetd.token", "")
	if got.Level != LevelWarn {
		t.Fatalf("minted token + 57 rejections → %s, want warn", got.Level)
	}
	if !strings.Contains(got.Detail, "state dir") {
		t.Errorf("detail = %q, want the unmounted state dir named — that is the fix", got.Detail)
	}
}

// ─── review-pass regressions ────────────────────────────────────────────────

// TestDoctor_OutboundProbesRunInParallel pins REV-1. Each probe is
// bounded at 5s, so a fleet with three boxes off would spend the whole
// report deadline dialling them serially — and the cells behind them
// would report "context deadline exceeded" for a call that was never
// made. A misdiagnosis is worse than a slow report.
func TestDoctor_OutboundProbesRunInParallel(t *testing.T) {
	const cells, delay = 6, 150 * time.Millisecond
	reg := []Cell{{Name: fleetcfg.FrontCell, URL: "http://127.0.0.1:1"}}
	for i := range cells {
		reg = append(reg, Cell{Name: fmt.Sprintf("cell-%d", i), URL: "http://127.0.0.1:1"})
	}
	dir := t.TempDir()
	s := New(reg, filepath.Join(dir, "h.json"), testDaemonInfo, Options{
		IntentPath: filepath.Join(dir, "intent.json"),
		CellAuth: func(ctx context.Context, cell string) CellAuthResult {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
			}
			return CellAuthResult{Attempted: true, OK: true, Source: "test"}
		},
	})
	t.Cleanup(s.Close)

	start := time.Now()
	rep := s.Doctor(context.Background())
	elapsed := time.Since(start)
	// Serial would be (cells+1)*delay ≈ 1.05s; parallel is one delay plus
	// the snapshot round. The bound is deliberately loose — this asserts
	// the SHAPE, not a benchmark.
	if elapsed > time.Duration(cells)*delay {
		t.Errorf("Doctor took %v for %d cells at %v each: the outbound probes are serial", elapsed, cells+1, delay)
	}
	for i := range cells {
		if got := mustCheck(t, rep, "auth.outbound", fmt.Sprintf("cell-%d", i)).Level; got != LevelOK {
			t.Errorf("cell-%d → %s", i, got)
		}
	}
}

// TestDoctor_TLSDialHonoursTheReportContext pins REV-2: the first cut
// used tls.DialWithDialer, which ignores context entirely, so a
// cancelled request left one 3s dial per https endpoint running.
func TestDoctor_TLSDialHonoursTheReportContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	// 10.255.255.1 is RFC1918 space that does not answer; without context
	// support this blocks for the full tlsDialTimeout.
	lvl, _, _ := certNotAfter(ctx, "https://10.255.255.1:9443", tlsDialTimeout)
	if elapsed := time.Since(start); elapsed > tlsDialTimeout/2 {
		t.Errorf("certNotAfter took %v against a cancelled context; the dial ignores it", elapsed)
	}
	if lvl != LevelUnknown {
		t.Errorf("level = %s, want unknown", lvl)
	}
}

// TestDoctor_ReportsFleetdsOwnBuild pins REV-3: DoctorHost.Version was
// collected and never rendered, which is precisely the asymmetry
// defs.parity exists to catch — the box writing the front's render is
// part of the fleet's version story.
func TestDoctor_ReportsFleetdsOwnBuild(t *testing.T) {
	dir := t.TempDir()
	s := New([]Cell{{Name: fleetcfg.FrontCell, URL: "http://127.0.0.1:1"}},
		filepath.Join(dir, "h.json"), testDaemonInfo, Options{
			IntentPath: filepath.Join(dir, "intent.json"),
			DoctorHost: func() DoctorHost {
				return DoctorHost{Version: "v9.9.9", DefsSHA: "cafe123", DefsDirty: true}
			},
		})
	t.Cleanup(s.Close)
	got := mustCheck(t, s.Doctor(context.Background()), "versions.reported", "")
	for _, want := range []string{"v9.9.9", "cafe123", "DIRTY"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail = %q, want it to carry %q", got.Detail, want)
		}
	}
}

// ─── adversarial-review-pass regressions (ground rule 9) ────────────────────

// tlsBlackhole accepts TCP and never speaks TLS, so every dial burns the
// whole dial timeout — what a powered-down box behind a DROP rule looks
// like, and the only shape that exposes a serial fan-out.
func tlsBlackhole(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		var held []net.Conn
		defer func() {
			for _, c := range held {
				c.Close()
			}
		}()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			held = append(held, c)
		}
	}()
	return ln.Addr().String()
}

// TestDoctor_TLSDialsRunInParallel is REV-1 one subsystem over: the
// credential probes were fanned out and the TLS dials were left serial,
// sharing the report's single deadline. Ten unreachable https endpoints
// consumed the whole 20s budget in a queue, and the endpoints behind the
// queue reported "TLS dial failed — a host that is off" for a dial that
// was never attempted. A diagnostic that invents an observation is worse
// than a slow one.
func TestDoctor_TLSDialsRunInParallel(t *testing.T) {
	addr := tlsBlackhole(t)
	const n = 6
	cells := []Cell{{Name: fleetcfg.FrontCell, URL: "http://127.0.0.1:1"}}
	hostCells := map[string]fleetcfg.Cell{fleetcfg.FrontCell: {URL: "http://127.0.0.1:1"}}
	for i := range n {
		name := fmt.Sprintf("cell-%d", i)
		cells = append(cells, Cell{Name: name, URL: "http://127.0.0.1:1"})
		// Distinct hosts (same listener) so the dedupe does not hide the
		// serial cost this test is about.
		hostCells[name] = fleetcfg.Cell{URL: "https://" + strings.Replace(addr, "127.0.0.1", "localhost", 1) + "/" + name}
	}
	s, _ := doctorServer(t, cells...)
	s.tlsDial = 300 * time.Millisecond
	s.hosts = &fleetcfg.File{Cells: hostCells}

	// A budget of three dials for six endpoints: parallel fits, serial
	// cannot, and the endpoints serial never reached must say so rather
	// than describe a host.
	ctx, cancel := context.WithTimeout(context.Background(), 3*s.tlsDial)
	defer cancel()
	start := time.Now()
	rep := s.Doctor(ctx)
	elapsed := time.Since(start)
	if elapsed > time.Duration(n-1)*s.tlsDial {
		t.Errorf("Doctor took %v for %d blackholed TLS endpoints at %v each: the dials are serial", elapsed, n, s.tlsDial)
	}
	rows := 0
	for _, c := range rep.Checks {
		if c.ID != "tls.not_after" {
			continue
		}
		rows++
		if c.Level != LevelUnknown {
			t.Errorf("%s → %s, want unknown", c.Cell, c.Level)
		}
		if strings.Contains(c.Summary, "not reached inside the report") {
			t.Errorf("%s never got a dial: %q. Serially, the endpoints behind the queue are reported without "+
				"having been observed at all", c.Cell, c.Summary)
		}
	}
	if rows != n {
		t.Errorf("%d tls rows, want %d", rows, n)
	}
}

// TestDoctor_TLSRowNamesTheBudgetWhenTheReportRanOut: "a host that is
// off and a broken TLS listener are indistinguishable" is a claim about
// an endpoint doctor actually dialled. When the report's own deadline is
// what expired, the row must say so — otherwise a slow report reads as
// a fleet of dead boxes.
func TestDoctor_TLSRowNamesTheBudgetWhenTheReportRanOut(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lvl, sum, det := certNotAfter(ctx, "https://10.255.255.1:9443", tlsDialTimeout)
	if lvl != LevelUnknown {
		t.Fatalf("level = %s, want unknown", lvl)
	}
	if !strings.Contains(sum, "not reached inside the report") || !strings.Contains(det, "BUDGET") {
		t.Errorf("summary/detail = %q / %q, want the report's budget named rather than the host", sum, det)
	}
}

// TestDoctor_TLSDialsEachEndpointOnce: a cell whose url and daemon_url
// are the same host produced two identical rows under one (id, cell)
// key, and paid for two handshakes to learn the same fact.
func TestDoctor_TLSDialsEachEndpointOnce(t *testing.T) {
	srv := tlsServerExpiring(t, 90*24*time.Hour)
	s, _ := doctorServer(t)
	s.hosts = &fleetcfg.File{
		FleetdURL: srv.URL,
		Cells:     map[string]fleetcfg.Cell{fleetcfg.FrontCell: {URL: srv.URL, DaemonURL: srv.URL}},
	}
	rows := 0
	for _, c := range s.Doctor(context.Background()).Checks {
		if c.ID == "tls.not_after" {
			rows++
		}
	}
	if rows != 1 {
		t.Errorf("%d tls.not_after rows for one endpoint named three times, want 1", rows)
	}
}

// TestDoctor_DeclaredSuppressionIsNotAPolicyFailure. A C11 hold and a
// drain are the operator's own declarations, and the class table says an
// absent opportunistic or roaming box is not news. Before this, one
// report said "1 active lease, none outliving a day — active holds: …"
// two rows above "the warm policy is not doing what it was declared to
// do", about the same hold.
func TestDoctor_DeclaredSuppressionIsNotAPolicyFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		class   string
		arrange func(t *testing.T, s *Server)
		wantIn  string
	}{
		{"a C11 hold", "always_on", func(t *testing.T, s *Server) {
			if _, err := s.SetHold("heavy", "qwen", "evaluating", 4*time.Hour); err != nil {
				t.Fatal(err)
			}
		}, "hold in force"},
		{"a declared drain", "always_on", func(t *testing.T, s *Server) {
			if _, err := s.SetIntent("heavy", "drained", "gaming", ""); err != nil {
				t.Fatal(err)
			}
			s.mu.Lock()
			since := s.intents["heavy"].Since
			s.mu.Unlock()
			s.recordAnnounce(&AnnounceRequest{V: AnnounceVersion, Cell: "heavy", Seq: 9,
				Intent: &AnnounceIntent{State: "drained", Since: since}})
		}, "cell drained, declared"},
		{"an absent opportunistic box", "opportunistic", func(*testing.T, *Server) {}, "class expects"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := doctorServer(t, Cell{Name: fleetcfg.FrontCell, URL: "http://127.0.0.1:1"},
				Cell{Name: "heavy", URL: "http://127.0.0.1:1", Class: tc.class})
			if tc.class == "always_on" {
				presenceOf(s, "heavy", AnnounceModel{ID: "qwen", State: "ready"})
			}
			s.startWarmLoopWithConfig(warmLoopConfig{
				targets: []WarmTarget{{Cell: "heavy", Model: "qwen", RestoreAfterIdle: time.Hour}},
				tick:    5 * time.Millisecond,
				warmFn:  func(context.Context, string, string) error { return nil },
			})
			s.startProbeLoopWithTick([]ProbeTarget{{Cell: "heavy", Model: "qwen", Every: time.Hour}}, 5*time.Millisecond)
			tc.arrange(t, s)
			waitForCond(t, func() bool {
				for _, st := range s.warmReport().Targets {
					if st.State == "skipped" {
						return true
					}
				}
				return false
			})

			rep := s.Doctor(context.Background())
			warm := mustCheck(t, rep, "warm.policy", "")
			if warm.Level != LevelOK {
				t.Errorf("warm.policy → %s (%s): a skip the fleet itself declared is the policy working", warm.Level, warm.Detail)
			}
			if !strings.Contains(warm.Detail, tc.wantIn) {
				t.Errorf("warm.policy detail = %q, want the declared reason named (%q) rather than swallowed", warm.Detail, tc.wantIn)
			}
			if probe := mustCheck(t, rep, "probe.verdicts", ""); probe.Level == LevelWarn {
				t.Errorf("probe.verdicts → warn (%s): a guard skip the fleet declared is not a finding", probe.Detail)
			}
		})
	}
}

func waitForCond(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition never held")
}

// TestDoctor_ProbeSkipIsAFindingOnlyWhenTheTargetWasNeverAsked. C8's
// guard set skips on in-flight work, an UNREPORTED in-flight count, a
// model that is not resident and an active lease — so on a fleet in use
// a declared target is skipped most passes, and "skipped right now" made
// probe.verdicts a permanent WARN. §11 asks for the target skipped for
// its whole LIFE.
func TestDoctor_ProbeSkipIsAFindingOnlyWhenTheTargetWasNeverAsked(t *testing.T) {
	s, _ := doctorServer(t, Cell{Name: fleetcfg.FrontCell, URL: "http://127.0.0.1:1"},
		Cell{Name: "heavy", URL: "http://127.0.0.1:1", Class: "always_on"})
	presenceOf(s, "heavy", AnnounceModel{ID: "qwen", State: "ready"})

	asked := time.Now().Add(-time.Minute)
	s.mu.Lock()
	s.probeStates = append(s.probeStates,
		&probeTargetState{Cell: "heavy", Model: "qwen", State: "skipped",
			Detail: "cell heavy has 2 in-flight", LastSkip: "cell heavy has 2 in-flight", LastAsk: &asked},
	)
	s.mu.Unlock()
	if got := mustCheck(t, s.Doctor(context.Background()), "probe.verdicts", "").Level; got == LevelWarn {
		t.Error("a target that has measured before and is skipped because the cell is BUSY is the fleet working")
	}

	s2, _ := doctorServer(t, Cell{Name: fleetcfg.FrontCell, URL: "http://127.0.0.1:1"},
		Cell{Name: "heavy", URL: "http://127.0.0.1:1", Class: "always_on"})
	presenceOf(s2, "heavy", AnnounceModel{ID: "qwen", State: "ready"})
	s2.mu.Lock()
	s2.probeStates = append(s2.probeStates,
		&probeTargetState{Cell: "heavy", Model: "qwen", State: "skipped",
			Detail: "cell heavy in-flight unknown", LastSkip: "cell heavy in-flight unknown"},
	)
	s2.mu.Unlock()
	got := mustCheck(t, s2.Doctor(context.Background()), "probe.verdicts", "")
	if got.Level != LevelWarn {
		t.Fatalf("a target never once asked → %s, want warn: it measures nothing where it was declared to watch", got.Level)
	}
	if !strings.Contains(got.Detail, "in-flight unknown") {
		t.Errorf("detail = %q, want the skip reason carried", got.Detail)
	}
}

// TestDoctor_IntentHygieneDoesNotAccuseAFreshDrainRequest: a request the
// cell has not answered yet is the normal middle of every drain — the
// echo rides the next heartbeat, and the cell answering while it winds
// down renders INCONSISTENT by design. Flagging that one second after
// `vibe cell drain` accused the operator of their own verb; the
// staleRequestAge gate existed and was applied to the other bucket only.
func TestDoctor_IntentHygieneDoesNotAccuseAFreshDrainRequest(t *testing.T) {
	s, _ := doctorServer(t, Cell{Name: fleetcfg.FrontCell, URL: "http://127.0.0.1:1"},
		Cell{Name: "heavy", URL: "http://127.0.0.1:1", Class: "always_on"})
	presenceOf(s, "heavy", AnnounceModel{ID: "qwen", State: "ready"})
	if _, err := s.SetIntent("heavy", "drained", "gaming", ""); err != nil {
		t.Fatal(err)
	}
	if got := mustCheck(t, s.Doctor(context.Background()), "intent.hygiene", ""); got.Level != LevelOK {
		t.Errorf("a drain requested one second ago → %s (%s), want ok: the echo rides the next heartbeat",
			got.Level, got.Detail)
	}

	// Past the ack window the same state IS residue — and it is not
	// "undeclared": the intent is declared and the cell is ignoring it.
	s.mu.Lock()
	in := s.intents["heavy"]
	in.Since = time.Now().Add(-2 * staleRequestAge)
	s.intents["heavy"] = in
	s.mu.Unlock()
	got := mustCheck(t, s.Doctor(context.Background()), "intent.hygiene", "")
	if got.Level != LevelWarn {
		t.Fatalf("a request unacked for %v → %s, want warn", 2*staleRequestAge, got.Level)
	}
	if !strings.Contains(got.Detail, "still answering") {
		t.Errorf("detail = %q, want INCONSISTENT described as declared-but-unreconciled, not as an undeclared stop", got.Detail)
	}
}

// TestDoctor_VersionsWithoutADefsSHACannotJoinParity. §5's rule is
// assert the evidence EXISTS before asserting parity over it: a block
// carrying a vibe build and no defs_sha scored OK while defs.parity
// silently dropped that cell from a divergence report.
func TestDoctor_VersionsWithoutADefsSHACannotJoinParity(t *testing.T) {
	s := versionFleet(t, map[string]*AnnounceVersions{
		"a":       {DefsSHA: "abc123", Vibe: "v1"},
		"b":       {DefsSHA: "def456", Vibe: "v1"},
		"no-defs": {Vibe: "v1"},
	})
	rep := s.Doctor(context.Background())
	if got := mustCheck(t, rep, "versions.reported", "no-defs"); got.Level != LevelUnknown {
		t.Errorf("a versions block with no defs_sha → %s, want unknown: it can neither agree nor disagree", got.Level)
	}
	parity := mustCheck(t, rep, "defs.parity", "")
	if parity.Level != LevelWarn {
		t.Fatalf("divergent SHAs → %s", parity.Level)
	}
	if !strings.Contains(parity.Detail, "no-defs") {
		t.Errorf("detail = %q, want the cell the verdict does NOT cover named — on a divergence report it is the "+
			"most likely culprit", parity.Detail)
	}
}

// TestDoctor_ZeroDiskFigureNamesBothReadings: the producer leaves
// disk_free_gb at zero when statfs fails, and a genuinely full
// filesystem announces the same zero. UNKNOWN is right; "the capacity
// block is absent or reports nothing" was a claim doctor cannot make,
// and it hid the one failure the row exists to catch.
func TestDoctor_ZeroDiskFigureNamesBothReadings(t *testing.T) {
	s, _ := doctorServer(t, Cell{Name: fleetcfg.FrontCell, URL: "http://127.0.0.1:1"},
		Cell{Name: "heavy", URL: "http://127.0.0.1:1", Class: "always_on"})
	s.recordAnnounce(&AnnounceRequest{V: AnnounceVersion, Cell: "heavy", Seq: 1,
		Intent:   &AnnounceIntent{State: "serving", Since: time.Now().UTC()},
		Capacity: &AnnounceCapacity{VRAMTotalGB: 24}})
	got := mustCheck(t, s.Doctor(context.Background()), "disk.free", "heavy")
	if got.Level != LevelUnknown {
		t.Fatalf("zero disk figure → %s, want unknown", got.Level)
	}
	if !strings.Contains(got.Detail, "nothing left") {
		t.Errorf("detail = %q, want a full filesystem named as the other reading of the same zero", got.Detail)
	}
}

// TestDoctor_StateDirUnreadableIsNotReportedAsMissing: "missing" sends
// an operator to remount a volume that is already mounted.
func TestDoctor_StateDirUnreadableIsNotReportedAsMissing(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	notADir := filepath.Join(file, "state") // ENOTDIR, not ENOENT
	s := New([]Cell{{Name: fleetcfg.FrontCell, URL: "http://127.0.0.1:1"}},
		filepath.Join(dir, "h.json"), testDaemonInfo, Options{
			IntentPath: filepath.Join(dir, "intent.json"),
			DoctorHost: func() DoctorHost { return DoctorHost{StateDir: notADir} },
		})
	t.Cleanup(s.Close)
	got := mustCheck(t, s.Doctor(context.Background()), "fleetd.state_dir", "")
	if got.Level != LevelFail {
		t.Fatalf("unstattable state dir → %s, want fail", got.Level)
	}
	if !strings.Contains(got.Summary, "unreadable") {
		t.Errorf("summary = %q, want the stat error reported as unreadable rather than as missing", got.Summary)
	}
}
