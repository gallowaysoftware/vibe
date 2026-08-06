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
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
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
	var out bytes.Buffer
	if err := trialDeclareAndWait(context.Background(), &out, target, tr, trialGateOpts{now: true, quiet: time.Minute}); err != nil {
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
	var out bytes.Buffer
	if err := trialDeclareAndWait(context.Background(), &out, target, tr, trialGateOpts{now: true}); err != nil {
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
	var out bytes.Buffer
	err = trialDeclareAndWait(context.Background(), &out, target, trialFor(modeltry.StateStaged), trialGateOpts{now: true})
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
	var out bytes.Buffer
	if err := trialDeclareAndWait(context.Background(), &out, target, trialFor(modeltry.StateApplied), trialGateOpts{now: true}); err != nil {
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
