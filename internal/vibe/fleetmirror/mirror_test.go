package fleetmirror

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
)

// ─── the enumeration ────────────────────────────────────────────────────

// notFleetState is every StateHome-rooted path in paths.go that a mirror
// of the front host deliberately does not carry, with the reason. It is
// the exclusion half of TestMirrorCoversEveryFleetStateFile: a future
// phase that adds a state file either mirrors it or writes down here why
// not, and cannot do neither.
var notFleetState = map[string]string{
	"StateHome":         "the root itself, not a file",
	"RuntimeDir":        "runtime, not state",
	"Socket":            "a unix socket: recreated at every start",
	"PIDFile":           "this box's process id; meaningless on the standby",
	"LogDir":            "logs are diagnosis, not state; they do not restore a fleet",
	"FrontendStateRoot": "per-profile frontend state (Open WebUI data, harness configs): not fleet state, and a fleetd deployment has no profiles",
	"FrontendStateDir":  "same, per profile",
	"TokenFile":         "carried, but under its own secret rules — see knownState",
}

// TestMirrorCoversEveryFleetStateFile is the "find every producer" rule
// applied to the thing this phase exists for. The mirror's value is
// entirely in its completeness, and the way it silently stops being
// complete is a later phase adding a state file and nobody noticing —
// which is exactly how C7a's ledger, C9's notify scope and C11's holds
// each arrived.
func TestMirrorCoversEveryFleetStateFile(t *testing.T) {
	covered := map[string]bool{}
	for _, sf := range knownState() {
		covered[sf.producer] = true
	}
	// TokenFile is in the table AND in the exclusion map's prose; the
	// table wins, and this asserts the two agree rather than letting the
	// prose quietly exempt something real.
	if !covered["TokenFile"] {
		t.Fatal("the control-plane token is not in knownState: a standby that mints a fresh one 401s the whole fleet")
	}

	src := filepath.Join("..", "paths", "paths.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, src, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}
	seen := 0
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Body == nil {
			continue
		}
		if fn.Type.Results == nil || len(fn.Type.Results.List) != 1 {
			continue
		}
		if id, ok := fn.Type.Results.List[0].Type.(*ast.Ident); !ok || id.Name != "string" {
			continue
		}
		var body bytes.Buffer
		if err := printer.Fprint(&body, fset, fn.Body); err != nil {
			t.Fatalf("print %s: %v", fn.Name.Name, err)
		}
		if !strings.Contains(body.String(), "StateHome()") {
			continue
		}
		seen++
		name := fn.Name.Name
		if covered[name] {
			continue
		}
		if _, excused := notFleetState[name]; excused {
			continue
		}
		t.Errorf("paths.%s() is state on the front host and the mirror neither captures it nor excuses it. "+
			"Add it to knownState() with what the fleet loses without it, or to notFleetState with why not.", name)
	}
	if seen < len(covered) {
		t.Fatalf("the scan found %d StateHome paths but the table names %d producers — the scan stopped working", seen, len(covered))
	}
}

// ─── create ─────────────────────────────────────────────────────────────

type fixture struct {
	root, state, config, front, extras, out string
	hosts                                   *fleetcfg.File
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	root := t.TempDir()
	f := fixture{
		root:   root,
		state:  filepath.Join(root, "state"),
		config: filepath.Join(root, "config"),
		front:  filepath.Join(root, "front"),
		extras: filepath.Join(root, "extras"),
		out:    filepath.Join(root, "out"),
	}
	mkdirs(t, filepath.Join(f.state, "fleet"), filepath.Join(f.config, "backends"), f.front, f.extras)
	write(t, filepath.Join(f.state, "token"), "TOKEN-VALUE-fleet-root", 0o600)
	write(t, filepath.Join(f.state, "fleet", "intent.json"), `{"gpu":{"state":"drained"}}`, 0o644)
	write(t, filepath.Join(f.state, "fleet", "usage.jsonl"), "{\"day\":\"2026-08-01\"}\n", 0o644)
	write(t, filepath.Join(f.config, "hosts.yaml"), hostsYAML("http://front.example.lan:9000", "http://fleetd.example.lan:9001", filepath.Join(root, "cell-token")), 0o644)
	write(t, filepath.Join(f.config, "config.yaml"), "fleet_registry: true\n", 0o644)
	write(t, filepath.Join(f.config, "backends", "chat.yaml"), "backend: {}\n", 0o644)
	write(t, filepath.Join(f.front, "config.yaml"), "models:\n  peer: {}\n", 0o644)
	write(t, filepath.Join(f.extras, ".env"), "FRONT_IPV4=203.0.113.4\n", 0o600)
	write(t, filepath.Join(root, "cell-token"), "CELL-TOKEN-DO-NOT-CARRY", 0o600)

	hosts, err := fleetcfg.LoadFrom(filepath.Join(f.config, "hosts.yaml"))
	if err != nil {
		t.Fatalf("load hosts: %v", err)
	}
	f.hosts = hosts
	return f
}

func hostsYAML(frontURL, fleetdURL, tokenFile string) string {
	return "fleetd_url: \"" + fleetdURL + "\"\ncells:\n" +
		"  front: { url: \"" + frontURL + "\", class: always_on }\n" +
		"  gpu: { url: \"http://gpu.example.lan:9000\", class: opportunistic, token_file: \"" + tokenFile + "\" }\n"
}

func (f fixture) opts() Options {
	return Options{
		StateDir: f.state, ConfigDir: f.config,
		FrontConfig: filepath.Join(f.front, "config.yaml"),
		Out:         f.out, Hosts: f.hosts,
		FrontImage: "ghcr.io/example/llama-swap@sha256:" + strings.Repeat("a", 64),
		Vibe:       "test", Host: "front-host",
	}
}

func TestCreate_CapturesStateConfigAndTheRenderedFront(t *testing.T) {
	f := newFixture(t)
	m, rc, err := Create(f.opts())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	want := []string{
		"state/token",
		"state/fleet/intent.json",
		"state/fleet/usage.jsonl",
		"config/hosts.yaml",
		"config/config.yaml",
		"config/backends/chat.yaml",
		"front/config.yaml",
	}
	for _, w := range want {
		if !hasArchive(m, w) {
			t.Errorf("archive is missing %s", w)
		}
	}
	if len(m.Errors) != 0 {
		t.Errorf("unexpected errors: %v", m.Errors)
	}
	if !m.Credentials {
		t.Error("the archive carries the control-plane token and does not say so")
	}
	if rc.Files != len(m.Entries) || rc.Bytes <= 0 {
		t.Errorf("receipt disagrees with the manifest: %+v", rc)
	}
	if _, err := Verify(rc.Archive); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// TestCreate_RecordsCredentialPathsAndCarriesNoCredentialBytes is the
// boundary rule, mechanically. The cell's token file is REFERENCED so a
// recovery knows what to fetch from the private repo, and its bytes must
// not be anywhere in the archive.
func TestCreate_RecordsCredentialPathsAndCarriesNoCredentialBytes(t *testing.T) {
	f := newFixture(t)
	m, rc, err := Create(f.opts())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	found := false
	for _, r := range m.References {
		if r.Kind == "cell_token" && r.Cell == "gpu" {
			found = true
			if !r.Exists {
				t.Error("the cell token file exists and the reference says it does not")
			}
			if r.Why == "" {
				t.Error("a reference with no reason tells a recovery nothing")
			}
		}
	}
	if !found {
		t.Fatal("hosts.yaml declares a cell token_file and the manifest does not reference it")
	}
	raw, err := os.ReadFile(rc.Archive)
	if err != nil {
		t.Fatal(err)
	}
	// gzip, so a substring search on the compressed bytes proves little;
	// decompress and search the payload.
	if bytes.Contains(decompress(t, raw), []byte("CELL-TOKEN-DO-NOT-CARRY")) {
		t.Error("another box's credential VALUE is inside the archive (ground rule 3: paths, not values)")
	}
	if !bytes.Contains(decompress(t, raw), []byte("TOKEN-VALUE-fleet-root")) {
		t.Error("the control-plane token is NOT in the archive: a standby would mint a fresh one and 401 the fleet")
	}
}

func TestCreate_NoSecretsDropsTheTokensAndTheRenderedFront(t *testing.T) {
	f := newFixture(t)
	o := f.opts()
	o.NoSecrets = true
	m, rc, err := Create(o)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, bad := range []string{"state/token", "front/config.yaml"} {
		if hasArchive(m, bad) {
			t.Errorf("--no-secrets still captured %s", bad)
		}
	}
	if m.Credentials {
		t.Error("--no-secrets archive still reports carrying credentials")
	}
	if bytes.Contains(decompress(t, read(t, rc.Archive)), []byte("TOKEN-VALUE-fleet-root")) {
		t.Error("--no-secrets archive contains the token anyway")
	}
	if len(m.Warnings) == 0 {
		t.Error("dropping the token silently is worse than not dropping it: no warning was recorded")
	}
}

func TestCreate_RefusesADestinationOnTheBoxItMirrors(t *testing.T) {
	f := newFixture(t)
	for _, dest := range []string{
		filepath.Join(f.state, "backup"),
		f.state,
		filepath.Join(f.config, "backup"),
	} {
		o := f.opts()
		o.Out = dest
		if _, _, err := Create(o); err == nil {
			t.Errorf("--out %s was accepted: a mirror stored on the box it mirrors dies with it", dest)
		}
	}
}

func TestCreate_AbsentIsMissingAndUnreadableIsAnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	f := newFixture(t)
	bad := filepath.Join(f.state, "fleet", "leases.json")
	write(t, bad, "{}", 0o644)
	if err := os.Chmod(bad, 0o000); err != nil {
		t.Fatal(err)
	}
	m, _, err := Create(f.opts())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(m.Errors) != 1 || !strings.Contains(m.Errors[0], "leases.json") {
		t.Fatalf("an unreadable state file must be an ERROR (it is not backed up), got %v", m.Errors)
	}
	// last-seen.json was never created: that is Missing, not an error.
	if !containsSubstr(m.Missing, "last-seen.json") {
		t.Errorf("an absent file must be named as missing, got %v", m.Missing)
	}
	if containsSubstr(m.Errors, "last-seen.json") {
		t.Error("an absent file must not read as a read failure")
	}
}

func TestCreate_LiteralIPIdentityIsWarnedAtMirrorTime(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.config, "hosts.yaml"),
		hostsYAML("http://192.0.2.10:9000", "http://192.0.2.11:9001", filepath.Join(f.root, "cell-token")), 0o644)
	hosts, err := fleetcfg.LoadFrom(filepath.Join(f.config, "hosts.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	o := f.opts()
	o.Hosts = hosts
	m, _, err := Create(o)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !containsSubstr(m.Warnings, "192.0.2.10") || !containsSubstr(m.Warnings, "192.0.2.11") {
		t.Fatalf("a literal-IP identity decides whether the takeover is one DNS record or an edit on every box; warnings were %v", m.Warnings)
	}
	// And a name must not warn: a permanent warning on a correct
	// configuration is one an operator learns to ignore.
	m2, _, err := Create(f.opts())
	if err != nil {
		t.Fatal(err)
	}
	if containsSubstr(m2.Warnings, "literal IP") {
		t.Errorf("a DNS-named front warned anyway: %v", m2.Warnings)
	}
}

func TestNotCoveredNamesThePinnedImage(t *testing.T) {
	got := strings.Join(NotCovered("repo@sha256:beef"), " | ")
	if !strings.Contains(got, "repo@sha256:beef") {
		t.Error("the standby must run the pinned image; the manifest does not say which")
	}
	if !strings.Contains(NotCovered("")[1], "undeclared") {
		t.Error("an undeclared image must read as undeclared, not as an empty instruction")
	}
	for _, want := range []string{"probe baselines", "DNS", "credential VALUES"} {
		if !strings.Contains(got, want) {
			t.Errorf("NotCovered omits %q — discovering it mid-recovery is too late", want)
		}
	}
}

func TestPrune_OnlyTouchesItsOwnArchives(t *testing.T) {
	f := newFixture(t)
	o := f.opts()
	o.Keep = 2
	var last string
	for i := range 4 {
		o.Now = func() time.Time { return time.Date(2026, 8, 1, 0, 0, i, 0, time.UTC) }
		_, rc, err := Create(o)
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		last = rc.Archive
	}
	write(t, filepath.Join(f.out, "someone-elses-backup.tar.gz"), "x", 0o600)
	entries, err := os.ReadDir(f.out)
	if err != nil {
		t.Fatal(err)
	}
	archives := 0
	foreign := false
	for _, e := range entries {
		switch {
		case strings.HasPrefix(e.Name(), ArchivePrefix) && strings.HasSuffix(e.Name(), ArchiveSuffix):
			archives++
		case e.Name() == "someone-elses-backup.tar.gz":
			foreign = true
		}
	}
	if archives != 2 {
		t.Errorf("keep=2 left %d archives", archives)
	}
	if !foreign {
		t.Error("prune deleted a file this package did not write")
	}
	if newest, err := Newest(f.out); err != nil || newest != last {
		t.Errorf("Newest = %q (%v), want %q", newest, err, last)
	}
}

func TestReceipt_RoundTripsThroughTheStateDir(t *testing.T) {
	f := newFixture(t)
	if _, err := ReadReceipt(f.state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a state dir with no receipt must report not-exist, got %v", err)
	}
	_, rc, err := Create(f.opts())
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadReceipt(f.state)
	if err != nil {
		t.Fatalf("read receipt: %v", err)
	}
	if got.Archive != rc.Archive || got.Files != rc.Files || !got.Credentials {
		t.Errorf("receipt round trip: %+v", got)
	}
	rel, err := filepath.Rel(paths.StateHome(), paths.MirrorReceiptFile())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.state, rel)); err != nil {
		t.Errorf("the receipt is not where fleetd reads it: %v", err)
	}
}

// ─── verify ─────────────────────────────────────────────────────────────

func TestVerify_CatchesACorruptedEntry(t *testing.T) {
	f := newFixture(t)
	_, rc, err := Create(f.opts())
	if err != nil {
		t.Fatal(err)
	}
	rewriteArchive(t, rc.Archive, func(name string, data []byte) []byte {
		if name == "state/fleet/intent.json" {
			return []byte(`{"gpu":{"state":"serving"}}`)
		}
		return data
	})
	if _, err := Verify(rc.Archive); err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("a rewritten entry must fail verification, got %v", err)
	}
}

func TestVerify_CatchesADroppedEntry(t *testing.T) {
	f := newFixture(t)
	_, rc, err := Create(f.opts())
	if err != nil {
		t.Fatal(err)
	}
	rewriteArchive(t, rc.Archive, func(name string, data []byte) []byte {
		if name == "state/token" {
			return nil // dropped
		}
		return data
	})
	if _, err := Verify(rc.Archive); err == nil || !strings.Contains(err.Error(), "missing from the archive") {
		t.Fatalf("a dropped entry must fail verification, got %v", err)
	}
}

func TestVerify_RefusesPathsThatEscapeTheArchive(t *testing.T) {
	for _, name := range []string{"../evil", "/etc/passwd", "state/../../evil", "./state/x"} {
		if err := safeName(name); err == nil {
			t.Errorf("safeName(%q) accepted an escape", name)
		}
	}
	for _, name := range []string{"state/token", "config/backends/a.yaml", ManifestName} {
		if err := safeName(name); err != nil {
			t.Errorf("safeName(%q) rejected a legitimate entry: %v", name, err)
		}
	}
	dir := t.TempDir()
	archive := filepath.Join(dir, "evil.tar.gz")
	handRolled(t, archive, map[string][]byte{
		ManifestName: mustJSON(t, Manifest{Version: ManifestVersion}),
		"../evil":    []byte("pwned"),
	})
	if _, err := Verify(archive); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("an archive with ../ must be refused, got %v", err)
	}
}

func TestVerify_RefusesAnUnknownFormatVersion(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "future.tar.gz")
	handRolled(t, archive, map[string][]byte{
		ManifestName: mustJSON(t, Manifest{Version: ManifestVersion + 1}),
	})
	if _, err := Verify(archive); err == nil || !strings.Contains(err.Error(), "manifest version") {
		t.Fatalf("a newer archive must be refused rather than half-understood, got %v", err)
	}
}

// ─── restore ────────────────────────────────────────────────────────────

type standby struct{ state, config, front string }

func newStandby(t *testing.T) standby {
	t.Helper()
	root := t.TempDir()
	s := standby{
		state:  filepath.Join(root, "state"),
		config: filepath.Join(root, "config"),
		front:  filepath.Join(root, "front", "config.yaml"),
	}
	return s
}

func (s standby) opts(archive string) RestoreOptions {
	return RestoreOptions{Archive: archive, StateDir: s.state, ConfigDir: s.config, FrontConfig: s.front}
}

func TestRestore_PlacesFilesAndKeepsTheTokenMode(t *testing.T) {
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
	if rep.Wrote == 0 {
		t.Fatal("restored nothing")
	}
	tok := filepath.Join(sb.state, "token")
	fi, err := os.Stat(tok)
	if err != nil {
		t.Fatalf("token not restored: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("the control-plane token restored as %04o: a standby that widens it has published the fleet's root credential", fi.Mode().Perm())
	}
	if got := read(t, tok); string(got) != "TOKEN-VALUE-fleet-root" {
		t.Errorf("token content = %q", got)
	}
	for _, p := range []string{
		filepath.Join(sb.state, "fleet", "intent.json"),
		filepath.Join(sb.config, "hosts.yaml"),
		filepath.Join(sb.config, "backends", "chat.yaml"),
		sb.front,
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("not restored: %s (%v)", p, err)
		}
	}
}

// TestRestore_RefusesWhileTheFleetsOwnAddressAnswers is C19's answer to
// "what if two boxes answer at once". Two fleetds both accept announces
// and both fold the same cumulative totals into two append-only ledgers
// that can never be reconciled; nothing detects it, because each half
// looks healthy from where it stands.
func TestRestore_RefusesWhileTheFleetsOwnAddressAnswers(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	f := newFixture(t)
	write(t, filepath.Join(f.config, "hosts.yaml"),
		hostsYAML("http://"+ln.Addr().String(), "http://"+ln.Addr().String(), filepath.Join(f.root, "cell-token")), 0o644)
	hosts, err := fleetcfg.LoadFrom(filepath.Join(f.config, "hosts.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	o := f.opts()
	o.Hosts = hosts
	_, rc, err := Create(o)
	if err != nil {
		t.Fatal(err)
	}

	sb := newStandby(t)
	rep, err := Restore(sb.opts(rc.Archive))
	if !errors.Is(err, ErrTakeover) {
		t.Fatalf("restore ran while the fleet's address still answered: %v", err)
	}
	if len(rep.Takeover) == 0 {
		t.Error("the refusal names no address")
	}
	if _, err := os.Stat(filepath.Join(sb.state, "token")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the refusal still wrote files")
	}

	// --force is the documented escape for the probe's one honest false
	// positive: the operator already moved the address to this box.
	forced := sb.opts(rc.Archive)
	forced.Force = true
	if _, err := Restore(forced); err != nil {
		t.Fatalf("--force restore: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sb.state, "token")); err != nil {
		t.Errorf("--force wrote nothing: %v", err)
	}
}

func TestRestore_WritesNothingWhenTheArchiveIsCorrupt(t *testing.T) {
	f := newFixture(t)
	_, rc, err := Create(f.opts())
	if err != nil {
		t.Fatal(err)
	}
	rewriteArchive(t, rc.Archive, func(name string, data []byte) []byte {
		if name == "config/hosts.yaml" {
			return []byte("cells: {}\n")
		}
		return data
	})
	sb := newStandby(t)
	if _, err := Restore(sb.opts(rc.Archive)); err == nil {
		t.Fatal("a corrupt archive was restored")
	}
	if _, err := os.Stat(sb.state); !errors.Is(err, os.ErrNotExist) {
		t.Error("a failed verification still touched the standby's state dir")
	}
}

func TestRestore_DoesNotOverwriteWithoutBeingAsked(t *testing.T) {
	f := newFixture(t)
	_, rc, err := Create(f.opts())
	if err != nil {
		t.Fatal(err)
	}
	sb := newStandby(t)
	mkdirs(t, sb.state)
	write(t, filepath.Join(sb.state, "token"), "A-LIVE-FLEETS-TOKEN", 0o600)

	rep, err := Restore(sb.opts(rc.Archive))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := string(read(t, filepath.Join(sb.state, "token"))); got != "A-LIVE-FLEETS-TOKEN" {
		t.Fatalf("an existing file was replaced without --overwrite: %q", got)
	}
	if !hasAction(rep, "state/token", "exists") {
		t.Error("the skip was not reported")
	}

	over := sb.opts(rc.Archive)
	over.Overwrite = true
	if _, err := Restore(over); err != nil {
		t.Fatal(err)
	}
	if got := string(read(t, filepath.Join(sb.state, "token"))); got != "TOKEN-VALUE-fleet-root" {
		t.Errorf("--overwrite did not replace: %q", got)
	}
}

func TestRestore_SkipsSlotsWithNoDestinationAndNeverPlacesExtras(t *testing.T) {
	f := newFixture(t)
	o := f.opts()
	o.Include = []string{filepath.Join(f.extras, ".env")}
	_, rc, err := Create(o)
	if err != nil {
		t.Fatal(err)
	}
	sb := newStandby(t)
	only := RestoreOptions{Archive: rc.Archive, StateDir: sb.state}
	rep, err := Restore(only)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !hasAction(rep, "config/hosts.yaml", "no-destination") {
		t.Error("an unset destination must be a named skip, not a guess")
	}
	var extra string
	for _, a := range rep.Actions {
		if strings.HasPrefix(a.Archive, "extras/") {
			extra = a.Status
		}
	}
	if extra != "manual" {
		t.Errorf("an --include'd file was not marked manual (got %q): extras came from absolute paths on the old host", extra)
	}
	if _, err := os.Stat(filepath.Join(sb.state, "token")); err != nil {
		t.Errorf("the state slot was not restored: %v", err)
	}
}

func TestRestore_DryRunWritesNothing(t *testing.T) {
	f := newFixture(t)
	_, rc, err := Create(f.opts())
	if err != nil {
		t.Fatal(err)
	}
	sb := newStandby(t)
	o := sb.opts(rc.Archive)
	o.DryRun = true
	rep, err := Restore(o)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Wrote != 0 {
		t.Errorf("dry run wrote %d files", rep.Wrote)
	}
	if _, err := os.Stat(sb.state); !errors.Is(err, os.ErrNotExist) {
		t.Error("dry run created the state dir")
	}
	if !hasAction(rep, "state/token", "would-write") {
		t.Error("dry run reported no plan")
	}
}

func TestTakeoverProbe_QuietWhenNothingAnswers(t *testing.T) {
	// A closed port on loopback: the box is gone, which is the state a
	// recovery actually runs in.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	hits := TakeoverProbe(RestoreOptions{DialTimeout: 500 * time.Millisecond},
		Identity{FleetdURL: "http://" + addr, FrontURL: "http://" + addr})
	if len(hits) != 0 {
		t.Errorf("probe reported %v against a dead address", hits)
	}
}

func TestDialAddr(t *testing.T) {
	cases := map[string]string{
		"http://front.lan:9000":  "front.lan:9000",
		"https://front.lan":      "front.lan:443",
		"http://front.lan":       "front.lan:80",
		"front.lan:9001":         "front.lan:9001",
		"http://192.0.2.7:9001":  "192.0.2.7:9001",
		"":                       "",
		"not a url":              "",
		"http://[::1]:9000/path": "[::1]:9000",
	}
	for in, want := range cases {
		if got := dialAddr(in); got != want {
			t.Errorf("dialAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

// ─── helpers ────────────────────────────────────────────────────────────

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func hasArchive(m *Manifest, name string) bool {
	for _, e := range m.Entries {
		if e.Archive == name {
			return true
		}
	}
	return false
}

func hasAction(rep *RestoreReport, archive, status string) bool {
	for _, a := range rep.Actions {
		if a.Archive == archive && a.Status == status {
			return true
		}
	}
	return false
}

func containsSubstr(list []string, want string) bool {
	for _, s := range list {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

func decompress(t *testing.T, raw []byte) []byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	out := &bytes.Buffer{}
	if _, err := out.ReadFrom(gz); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// rewriteArchive rebuilds an archive with each entry's payload passed
// through fn; returning nil drops the entry. It is how a corrupted or
// truncated backup is simulated without hand-writing a tar.
func rewriteArchive(t *testing.T, archive string, fn func(name string, data []byte) []byte) {
	t.Helper()
	files := map[string][]byte{}
	order := []string{}
	src, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(src)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		buf := &bytes.Buffer{}
		if _, err := buf.ReadFrom(tr); err != nil {
			t.Fatal(err)
		}
		out := fn(h.Name, buf.Bytes())
		if out == nil {
			continue
		}
		files[h.Name] = out
		order = append(order, h.Name)
	}
	gz.Close()
	src.Close()
	handRolledOrdered(t, archive, files, order)
}

func handRolled(t *testing.T, archive string, files map[string][]byte) {
	t.Helper()
	order := make([]string, 0, len(files))
	if _, ok := files[ManifestName]; ok {
		order = append(order, ManifestName)
	}
	for name := range files {
		if name != ManifestName {
			order = append(order, name)
		}
	}
	handRolledOrdered(t, archive, files, order)
}

func handRolledOrdered(t *testing.T, archive string, files map[string][]byte, order []string) {
	t.Helper()
	buf := &bytes.Buffer{}
	gz := gzip.NewWriter(buf)
	tw := tar.NewWriter(gz)
	for _, name := range order {
		data := files[name]
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o600, Size: int64(len(data)), Format: tar.FormatPAX}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archive, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// ─── review pass ────────────────────────────────────────────────────────

// TestRestore_RefusesContentThatChangedAfterVerification closes the gap
// between the two reads. Verify checks the archive; the write reads it
// again, and between the two the file can be replaced — by a mirror run
// finishing onto the same name, by a half-copied file on a network
// mount. Without this, "verified" is a claim about bytes that are not
// the bytes that land on the standby.
func TestRestore_RefusesContentThatChangedAfterVerification(t *testing.T) {
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
		files["state/token"] = []byte("A-TOKEN-NOBODY-VERIFIED")
		return files, nil
	}
	t.Cleanup(func() { readArchiveFn = orig })

	sb := newStandby(t)
	if _, err := Restore(sb.opts(rc.Archive)); err == nil || !strings.Contains(err.Error(), "changed after verification") {
		t.Fatalf("tampered payload was accepted: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(sb.state, "token")); err == nil && string(b) == "A-TOKEN-NOBODY-VERIFIED" {
		t.Fatal("the unverified bytes were written to the standby")
	}
}

func TestCreate_AnEmptyDefsDirIsAWarning(t *testing.T) {
	f := newFixture(t)
	if err := os.Remove(filepath.Join(f.config, "backends", "chat.yaml")); err != nil {
		t.Fatal(err)
	}
	m, _, err := Create(f.opts())
	if err != nil {
		t.Fatal(err)
	}
	if !containsSubstr(m.Warnings, "cannot RENDER") {
		t.Fatalf("an empty defs dir is the one shape that looks like a good capture and is not; warnings were %v", m.Warnings)
	}
	// And a populated one must not warn: a permanent warning on a correct
	// configuration is one an operator learns to ignore.
	m2, _, err := Create(f.opts())
	_ = m2
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(f.config, "backends", "chat.yaml"), "backend: {}\n", 0o644)
	m3, _, err := Create(f.opts())
	if err != nil {
		t.Fatal(err)
	}
	if containsSubstr(m3.Warnings, "cannot RENDER") {
		t.Errorf("a defs dir with defs warned anyway: %v", m3.Warnings)
	}
}

func TestCreate_NoConfigDirSaysWhatIsNotInTheArchive(t *testing.T) {
	f := newFixture(t)
	o := f.opts()
	o.ConfigDir = ""
	m, _, err := Create(o)
	if err != nil {
		t.Fatal(err)
	}
	if !containsSubstr(m.Warnings, "NOT in this archive") {
		t.Fatalf("a mirror with no config dir captured nothing and said nothing: %v", m.Warnings)
	}
}

func TestCreate_TwoRunsInOneSecondDoNotCollide(t *testing.T) {
	f := newFixture(t)
	o := f.opts()
	fixed := time.Date(2026, 8, 5, 3, 0, 0, 0, time.UTC)
	o.Now = func() time.Time { return fixed }
	_, first, err := Create(o)
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := Create(o)
	if err != nil {
		t.Fatal(err)
	}
	if first.Archive == second.Archive {
		t.Fatal("the second run overwrote the first: one archive, two receipts, the older describing a file that is gone")
	}
	for _, a := range []string{first.Archive, second.Archive} {
		if _, err := Verify(a); err != nil {
			t.Errorf("verify %s: %v", a, err)
		}
	}
}

// TestArchive_ManifestIsTheFirstEntry pins what the writer does (so
// `tar tzf` shows a human what they are holding first). Verify itself
// deliberately tolerates any order.
func TestArchive_ManifestIsTheFirstEntry(t *testing.T) {
	f := newFixture(t)
	_, rc, err := Create(f.opts())
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.Open(rc.Archive)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	gz, err := gzip.NewReader(src)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	h, err := tar.NewReader(gz).Next()
	if err != nil {
		t.Fatal(err)
	}
	if h.Name != ManifestName {
		t.Errorf("first archive entry is %q, want %q", h.Name, ManifestName)
	}
}
