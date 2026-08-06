package cli

// fleet-control C18 — the control-plane half of `vibe model try`: the two
// declarations a trial makes, and the one it must NOT make.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/hfdownload"
	"github.com/gallowaysoftware/vibe/internal/vibe/modeltry"
)

// trialFleetd records every lease mutation and serves a state document
// with whatever leases the test declares.
type trialFleetd struct {
	posts   []map[string]any
	deletes []map[string]any
	leases  []fleetapi.Lease
}

func (f *trialFleetd) start(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	record := func(dst *[]map[string]any, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		m := map[string]any{}
		_ = json.Unmarshal(raw, &m)
		*dst = append(*dst, m)
	}
	mux.HandleFunc("POST /api/fleet/lease", func(w http.ResponseWriter, r *http.Request) {
		record(&f.posts, r)
		_ = json.NewEncoder(w).Encode(fleetapi.Lease{Cell: "gpu-cell", ExpiresAt: time.Now().Add(time.Hour)})
	})
	mux.HandleFunc("DELETE /api/fleet/lease", func(w http.ResponseWriter, r *http.Request) {
		record(&f.deletes, r)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "deleted", "existed": true})
	})
	mux.HandleFunc("GET /api/fleet/state", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(fleetapi.StateSnapshot{
			Cells: []fleetapi.CellSnapshot{{Name: "gpu-cell", Reachable: true, Leases: f.leases}},
		})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func trialFor(state string) *modeltry.Trial {
	return &modeltry.Trial{
		Cell: "gpu-cell", Def: "trial-glm5", Incumbent: "qwen3.6-27b", State: state,
	}
}

// trialRunner is a Runner wired to a scratch journal. The declaration
// path writes to it, so the tests get the real persistence and can read
// back what a later process would.
func trialRunner(t *testing.T) (*modeltry.Runner, string) {
	t.Helper()
	dir := t.TempDir()
	statePath := filepath.Join(dir, "trial.json")
	run, err := modeltry.New(modeltry.Options{
		Cell: "gpu-cell", BackendsDir: filepath.Join(dir, "backends"),
		ConfigPath:   filepath.Join(dir, "config.yaml"),
		LlamaSwapURL: "http://127.0.0.1:1", StatePath: statePath,
	})
	if err != nil {
		t.Fatal(err)
	}
	return run, statePath
}

// TestTrialNowDeclaresTheLeaseAndTheHold pins the composition: a trial
// takes a plain C2 lease on the cell (so the pre-drain report, the warm
// schedules and C8's probes all see a consumer) and a C11 hold on the
// INCUMBENT (so fleetd's warm-target restore does not reload it the
// moment the cell looks idle and evict the candidate mid-comparison).
func TestTrialNowDeclaresTheLeaseAndTheHold(t *testing.T) {
	fd := &trialFleetd{}
	ts := fd.start(t)
	target, err := resolveFleetd(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	tr := trialFor(modeltry.StateStaged)
	run, _ := trialRunner(t)
	var out bytes.Buffer
	if err := trialDeclareAndWait(context.Background(), &out, run, target, tr, trialGateOpts{now: true, quiet: time.Minute}); err != nil {
		t.Fatalf("declare: %v", err)
	}
	if len(fd.posts) != 2 {
		t.Fatalf("want a lease and a hold, got %d posts: %v", len(fd.posts), fd.posts)
	}
	lease, hold := fd.posts[0], fd.posts[1]
	if lease["holder"] != modeltry.TrialLeaseHolder || lease["model"] != tr.Def {
		t.Fatalf("the lease is wrong: %v", lease)
	}
	if lease["hold"] == true {
		t.Fatalf("the trial's own lease must not be a C11 hold — a hold suspends fleetd's policy and this is a consumer declaring itself: %v", lease)
	}
	if hold["holder"] != fleetapi.HoldHolder || hold["model"] != tr.Incumbent || hold["hold"] != true {
		t.Fatalf("the hold must be a C11 hold on the INCUMBENT: %v", hold)
	}
	if !tr.Leased || !tr.Held {
		t.Fatalf("the journal did not record what was taken: leased=%v held=%v", tr.Leased, tr.Held)
	}
	if !strings.Contains(out.String(), "truncated") {
		t.Fatalf("--now must say what it costs:\n%s", out.String())
	}
}

// TestTrialLeavesAnExistingHoldAloneInBothDirections. C11 says a
// re-issue REFRESHES the same key, so posting over an operator's hold
// would overwrite their note and their expiry — and this trial's `end`
// would then delete a declaration somebody else still means.
func TestTrialLeavesAnExistingHoldAloneInBothDirections(t *testing.T) {
	fd := &trialFleetd{leases: []fleetapi.Lease{{
		Cell: "gpu-cell", Model: "qwen3.6-27b", Holder: fleetapi.HoldHolder, Hold: true,
		Note: "the operator's own afternoon", ExpiresAt: time.Now().Add(6 * time.Hour),
	}}}
	ts := fd.start(t)
	target, err := resolveFleetd(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	tr := trialFor(modeltry.StateStaged)
	run, _ := trialRunner(t)
	var out bytes.Buffer
	if err := trialDeclareAndWait(context.Background(), &out, run, target, tr, trialGateOpts{now: true}); err != nil {
		t.Fatal(err)
	}
	if len(fd.posts) != 1 || fd.posts[0]["holder"] != modeltry.TrialLeaseHolder {
		t.Fatalf("an existing hold must not be re-issued over: %v", fd.posts)
	}
	if tr.Held {
		t.Fatal("the journal claims a hold this trial did not take; `end` would delete the operator's")
	}
	if !strings.Contains(out.String(), "already exists") {
		t.Fatalf("the operator must be told their hold is being honoured:\n%s", out.String())
	}
}

// TestEndReleasesOnlyWhatTheTrialTook is the other half of the same rule,
// on the way out, and it drives the real `end` path rather than the two
// DELETEs it happens to issue.
func TestEndReleasesOnlyWhatTheTrialTook(t *testing.T) {
	for _, tc := range []struct {
		name        string
		leased      bool
		held        bool
		wantDeletes []string
	}{
		{"both", true, true, []string{modeltry.TrialLeaseHolder, fleetapi.HoldHolder}},
		{"lease only", true, false, []string{modeltry.TrialLeaseHolder}},
		{"neither", false, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fd := &trialFleetd{}
			ts := fd.start(t)
			dir := t.TempDir()
			runner, err := modeltry.New(modeltry.Options{
				Cell: "gpu-cell", BackendsDir: filepath.Join(dir, "backends"),
				ConfigPath:   filepath.Join(dir, "config.yaml"),
				LlamaSwapURL: "http://127.0.0.1:1", StatePath: filepath.Join(dir, "trial.json"),
			})
			if err != nil {
				t.Fatal(err)
			}
			tr := trialFor(modeltry.StatePlanned)
			tr.Leased, tr.Held = tc.leased, tc.held
			var out bytes.Buffer
			if err := runModelTryEnd(context.Background(), &out, runner, tr, ts.URL, false); err != nil {
				t.Fatalf("end: %v\n%s", err, out.String())
			}
			if len(fd.deletes) != len(tc.wantDeletes) {
				t.Fatalf("want %d deletes, got %v\n%s", len(tc.wantDeletes), fd.deletes, out.String())
			}
			for i, holder := range tc.wantDeletes {
				if fd.deletes[i]["holder"] != holder {
					t.Fatalf("delete %d: want holder %q, got %v", i, holder, fd.deletes[i])
				}
			}
		})
	}
}

// TestEndStillRollsBackWhenFleetdIsGone. The declarations expire on their
// own; a stranded llama-swap config does not.
func TestEndStillRollsBackWhenFleetdIsGone(t *testing.T) {
	dir := t.TempDir()
	runner, err := modeltry.New(modeltry.Options{
		Cell: "gpu-cell", BackendsDir: filepath.Join(dir, "backends"),
		ConfigPath:   filepath.Join(dir, "config.yaml"),
		LlamaSwapURL: "http://127.0.0.1:1", StatePath: filepath.Join(dir, "trial.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	tr := trialFor(modeltry.StatePlanned)
	tr.Leased, tr.Held = true, true
	var out bytes.Buffer
	// An unroutable fleetd: the release fails, the rollback must not.
	if err := runModelTryEnd(context.Background(), &out, runner, tr, "http://127.0.0.1:1", false); err != nil {
		t.Fatalf("end must complete without fleetd: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "expire") {
		t.Fatalf("the operator must be told the declarations are still standing:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "closed the trial journal") {
		t.Fatalf("the journal must still close:\n%s", out.String())
	}
}

// TestTrialRefusesToApplyUndeclared. A config rewrite nothing announced
// is invisible to the pre-drain report that exists to protect running
// work, so a lease fleetd will not accept fails the command rather than
// proceeding quietly.
func TestTrialRefusesToApplyUndeclared(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/fleet/lease", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "lease store disabled", http.StatusNotFound)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	target, err := resolveFleetd(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	run, _ := trialRunner(t)
	var out bytes.Buffer
	err = trialDeclareAndWait(context.Background(), &out, run, target, trialFor(modeltry.StateStaged), trialGateOpts{now: true})
	if err == nil || !strings.Contains(err.Error(), "refusing to apply undeclared") {
		t.Fatalf("want an undeclared refusal, got %v", err)
	}
}

// TestTrialDeclarationsAreSkippedOnResume: an already-applied trial made
// its declarations when it applied, and re-taking them on every resume
// would refresh a lease the operator may have deliberately released.
func TestTrialDeclarationsAreSkippedOnResume(t *testing.T) {
	fd := &trialFleetd{}
	ts := fd.start(t)
	target, err := resolveFleetd(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	run, _ := trialRunner(t)
	var out bytes.Buffer
	if err := trialDeclareAndWait(context.Background(), &out, run, target, trialFor(modeltry.StateApplied), trialGateOpts{now: true}); err != nil {
		t.Fatal(err)
	}
	if len(fd.posts) != 0 {
		t.Fatalf("a resumed trial re-declared: %v", fd.posts)
	}
}

// TestProgressPrinterDoesNotFloodTheLog. hfdownload calls back ~4/s for
// hours on a 20 GB pull; an agent transcript is a consumer of this
// output.
func TestProgressPrinterDoesNotFloodTheLog(t *testing.T) {
	var out bytes.Buffer
	p := progressPrinter(&out)
	const total = 1 << 30
	for i := 0; i <= 10000; i++ {
		p(int64(i)*total/10000, total)
	}
	lines := strings.Count(strings.TrimSpace(out.String()), "\n") + 1
	if lines > 25 {
		t.Fatalf("%d progress lines for one download", lines)
	}
	if !strings.Contains(out.String(), "100%") {
		t.Fatalf("the final line is missing:\n%s", out.String())
	}
}

// ── the adversarial-review pass's findings ──────────────────────────────

// TestTrialDeclarationsAreOnDiskBeforeTheApply is REV-A2. Everything after
// the declarations can fail — banking the config, the render, the write —
// and the journal is not saved again until Apply succeeds. A lease and a
// hold that live only in this process's memory are declarations
// `vibe model try end` will never release: they suppress the fleet's warm
// policy for four hours, and the next run parks forever on --unleased
// against the trial's own hold.
func TestTrialDeclarationsAreOnDiskBeforeTheApply(t *testing.T) {
	fd := &trialFleetd{}
	ts := fd.start(t)
	target, err := resolveFleetd(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	run, statePath := trialRunner(t)
	tr := trialFor(modeltry.StateStaged)
	var out bytes.Buffer
	if err := trialDeclareAndWait(context.Background(), &out, run, target, tr, trialGateOpts{now: true}); err != nil {
		t.Fatal(err)
	}
	// The process dies here. A later one reads the journal, and nothing
	// else.
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("the declarations were never written to the journal: %v", err)
	}
	var got modeltry.Trial
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Leased {
		t.Fatalf("the lease is not in the journal, so `end` releases nothing:\n%s", raw)
	}
	if !got.Held {
		t.Fatalf("the hold is not in the journal, so `end` leaves the warm policy suspended:\n%s", raw)
	}
}

// TestTrialGateRefusesAZeroIdleWindow. The deferral IS this phase: a
// config write truncates streams past 30s and evicts every resident
// model. `awaitConds.evalIdle` only runs on a POSITIVE window, so
// `--idle 0` applies the moment the cell answers — which is what --now
// does, minus the sentence saying what it costs.
func TestTrialGateRefusesAZeroIdleWindow(t *testing.T) {
	if err := validateTrialGate(0, 20, false); err == nil || !strings.Contains(err.Error(), "--now") {
		t.Fatalf("--idle 0 must be refused and must name --now, got %v", err)
	}
	if err := validateTrialGate(-time.Minute, 20, false); err == nil {
		t.Fatal("a negative idle window must be refused")
	}
	if err := validateTrialGate(0, 20, true); err != nil {
		t.Fatalf("--now is the documented way to skip the wait: %v", err)
	}
	if err := validateTrialGate(15*time.Minute, 20, false); err != nil {
		t.Fatalf("the default must pass: %v", err)
	}
}

// TestTrialGateRefusesAZeroMinFree: zero is how a float flag spells
// "unset", so `--min-free 0` silently restored the 20 GiB default instead
// of removing the floor the operator was trying to remove.
func TestTrialGateRefusesAZeroMinFree(t *testing.T) {
	err := validateTrialGate(time.Minute, 0, false)
	if err == nil || !strings.Contains(err.Error(), "--skip-disk-check") {
		t.Fatalf("--min-free 0 must be refused and must name the flag that DOES remove the floor, got %v", err)
	}
	if err := validateTrialGate(time.Minute, -1, false); err == nil {
		t.Fatal("a negative --min-free must be refused")
	}
}

// TestDryRunNeverRollsBackAnInFlightTrial is the review pass's blocker.
// `Plan` RESUMES an open journal (REV-1), and --dry-run closed "the
// journal Plan opened" through the real `End`. `End` works from any
// state: on a trial already `applied` that is the entire rollback — the
// def deleted, the cell's llama-swap config re-rendered, every resident
// model evicted by the reload — performed by a flag whose promise is that
// nothing happens, and reported as "nothing on this box changed".
func TestDryRunNeverRollsBackAnInFlightTrial(t *testing.T) {
	dir := t.TempDir()
	backends := filepath.Join(dir, "backends")
	weightsDir := filepath.Join(dir, "models")
	configPath := filepath.Join(dir, "llama-swap", "config.yaml")
	statePath := filepath.Join(dir, "state", "model-trial.json")
	for _, d := range []string{backends, weightsDir, filepath.Dir(configPath), filepath.Dir(statePath)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	hostsPath := filepath.Join(dir, "hosts.yaml")
	if err := os.WriteFile(hostsPath, []byte("cells:\n  front:\n    url: http://front.example.lan:9000\n    class: always_on\n  gpu-cell:\n    url: http://gpu.example.lan:9000\n    class: opportunistic\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hosts, err := fleetcfg.LoadFrom(hostsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backends, "qwen3.6-27b.yaml"), []byte(
		"name: qwen3.6-27b\ncell: gpu-cell\nbackend:\n  external: true\n  llama_server:\n    path: "+
			filepath.Join(weightsDir, "qwen3.6-27b-Q5.gguf")+"\n    alias: qwen3.6-27b\n    context: 4096\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	newRunner := func() *modeltry.Runner {
		run, err := modeltry.New(modeltry.Options{
			Cell: "gpu-cell", Hosts: hosts, BackendsDir: backends,
			ConfigPath: configPath, LlamaServerBinary: "/usr/bin/llama-server",
			LlamaSwapURL: "http://127.0.0.1:1", StatePath: statePath,
			CatalogWait: time.Millisecond,
			DiskFree:    func(string) (int64, error) { return 500 << 30, nil },
			HeadSize:    func(context.Context, hfdownload.Spec) (int64, error) { return 4096, nil },
			Fetch: func(_ context.Context, _ hfdownload.Spec, dest string, _ hfdownload.ProgressFunc) error {
				return os.WriteFile(dest, make([]byte, 4096), 0o644)
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	req := modeltry.PlanRequest{Repo: "unsloth/Qwen3.7-32B-GGUF", File: "Qwen3.7-32B-Q4_K_XL.gguf", Like: "qwen3.6-27b"}
	ctx := context.Background()

	// Drive the real sequence to `applied`, exactly as a first run leaves it.
	run := newRunner()
	tr, def, err := run.Plan(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Fetch(ctx, tr, nil); err != nil {
		t.Fatal(err)
	}
	if err := run.Stage(tr, def); err != nil {
		t.Fatal(err)
	}
	if _, err := run.Apply(ctx, tr); err != nil {
		t.Fatal(err)
	}
	appliedConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(appliedConfig), tr.Def) {
		t.Fatalf("the applied config does not carry the trial:\n%s", appliedConfig)
	}

	// The operator re-runs with --dry-run to re-read the def.
	var out bytes.Buffer
	if err := runModelTry(ctx, &out, newRunner(), "", req, trialGateOpts{dryRun: true, quiet: time.Minute}); err != nil {
		t.Fatalf("dry-run: %v\n%s", err, out.String())
	}
	got, err := os.ReadFile(configPath)
	if err != nil || string(got) != string(appliedConfig) {
		t.Fatalf("--dry-run re-rendered the cell's llama-swap config, evicting every resident model.\nwant:\n%s\ngot:\n%s (err %v)", appliedConfig, got, err)
	}
	if _, err := os.Stat(tr.DefPath); err != nil {
		t.Fatalf("--dry-run deleted the in-flight trial's def: %v", err)
	}
	again, err := newRunner().Load()
	if err != nil || again == nil {
		t.Fatalf("--dry-run closed the in-flight trial's journal; `status` now reports no trial on a box still serving it (%v, %v)", again, err)
	}
	if again.State != modeltry.StateApplied {
		t.Fatalf("the journal moved: %q", again.State)
	}
	if strings.Contains(out.String(), "nothing on this box changed, and no trial is open") {
		t.Fatalf("--dry-run claimed no trial is open while one is:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "already in flight") {
		t.Fatalf("--dry-run must say the open trial was left alone:\n%s", out.String())
	}

	// And the fresh case still closes its own journal, or the next real
	// invocation resumes a trial the operator never started.
	if _, err := newRunner().End(ctx, again, false); err != nil {
		t.Fatal(err)
	}
	var out2 bytes.Buffer
	if err := runModelTry(ctx, &out2, newRunner(), "", req, trialGateOpts{dryRun: true, quiet: time.Minute}); err != nil {
		t.Fatalf("dry-run on a clean box: %v\n%s", err, out2.String())
	}
	if left, err := newRunner().Load(); err != nil || left != nil {
		t.Fatalf("a --dry-run that opened the journal must close it: %v %v", left, err)
	}
}
