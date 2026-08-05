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
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "doctor.go", nil, 0)
	if err != nil {
		t.Fatal(err)
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

// TestDoctor_DefsParityTreatsADirtyCheckoutAsUncomparable: a dirty
// working tree's SHA does not describe what is running, so it can
// neither agree nor disagree. Counting it as agreement because the
// string matches is the absent-evidence mistake this phase exists to
// avoid.
func TestDoctor_DefsParityTreatsADirtyCheckoutAsUncomparable(t *testing.T) {
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

// TestDoctor_LlamaSwapMatrixNamesTheMissingProducer: versions.llama_swap
// has been a reserved field since C3 with nothing writing it. An UNKNOWN
// that says "no announcer fills this" is actionable; a silent OK would
// claim a uniform fleet nobody measured.
func TestDoctor_LlamaSwapMatrixNamesTheMissingProducer(t *testing.T) {
	s := versionFleet(t, map[string]*AnnounceVersions{
		"a": {DefsSHA: "abc123"},
		"b": {DefsSHA: "abc123"},
	})
	got := mustCheck(t, s.Doctor(context.Background()), "versions.llama_swap", "")
	if got.Level != LevelUnknown {
		t.Fatalf("no cell reports a version → %s, want unknown", got.Level)
	}
	if !strings.Contains(got.Detail, "populate") && !strings.Contains(got.Detail, "producer") {
		t.Errorf("detail = %q, want the missing PRODUCER named so the unknown is not read as an unreachable fleet", got.Detail)
	}

	s2 := versionFleet(t, map[string]*AnnounceVersions{
		"a": {LlamaSwap: "v239"},
		"b": {LlamaSwap: "v241"},
	})
	if got := mustCheck(t, s2.Doctor(context.Background()), "versions.llama_swap", "").Level; got != LevelWarn {
		t.Errorf("mixed llama-swap versions → %s, want warn (the upgrade ritual's mid-state)", got)
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
	lvl, _, _ := certNotAfter(ctx, "https://10.255.255.1:9443")
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
