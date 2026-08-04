package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibeclient"
)

// The await conditions (fleet-control C10). `vibe cell await X --up`
// answers "does llama-swap on X answer", which is not the question an
// overnight batch is asking: cell-up is not model-warm (6-10 min on the
// heavy cell) and cell-up is not cell-free (resume, then the operator
// sits down at the box).
//
// Every condition here is a READ of an axis something else owns —
// residency is llama-swap's, activity is observed, leases are declared —
// and they are all judged against ONE snapshot, because "ready at 03:00
// and idle at 03:05" is not "ready and idle".

// errUnknownModel is the model-shaped twin of errUnknownCell: fleetd
// answered, the cell is reachable, its catalog is non-empty, and the id
// is not in it. That can only be a typo, and `--timeout 0` is the
// documented overnight idiom — parking on it is the C6 bug. Every other
// shape of "not there yet" keeps waiting.
var errUnknownModel = errors.New("unknown model")

// maxIdleSeconds bounds the idle window read off the wire before it
// becomes a time.Duration. A century of silence is not a window anyone
// waits on, and an unbounded float64 conversion is undefined.
const maxIdleSeconds = 100 * 365 * 24 * 3600

// leaseClaim is the advisory hold await takes on success (C2's store).
type leaseClaim struct {
	holder string
	ttl    time.Duration
	note   string
}

// awaitConds is the composite the wait is parked on.
type awaitConds struct {
	wantUp   bool
	model    string
	ready    bool
	idle     time.Duration
	unleased bool
	lease    leaseClaim
}

// extras reports whether anything beyond reachability was requested.
// C3's events fast-path (return the moment a transition frame arrives)
// is only sound for a pure reachability wait: an event proves the cell
// moved, never that a model is warm or a GPU is free.
func (c awaitConds) extras() bool { return c.ready || c.idle > 0 || c.unleased }

// validateAwaitFlags rejects contradictory or under-specified
// combinations BEFORE any waiting starts. A wait that can never end is
// the failure C6 fixed for cell names, and these flags add three more
// routes to it.
func validateAwaitFlags(cell string, c awaitConds, ttlSet bool) error {
	if c.model != "" && !c.ready {
		return fmt.Errorf("--model needs a condition: use --model <id> --ready " +
			"(--model alone has no meaning today, and giving it an implicit one forecloses --gone/--degraded later)")
	}
	if c.ready && c.model == "" {
		return errors.New("--ready needs --model <id>")
	}
	if c.idle < 0 {
		return errors.New("--idle must be a positive duration")
	}
	if !c.wantUp && (c.ready || c.idle > 0 || c.unleased || c.lease.holder != "") {
		return errors.New("--down cannot be combined with --ready/--idle/--unleased/--lease: " +
			"a cell that stopped answering reports no model state, no activity and no ownership")
	}
	if c.lease.holder != "" && c.model == "" {
		return errors.New("--lease needs --model <id>: a lease is keyed (cell, model, holder)")
	}
	if c.lease.holder != "" && c.lease.ttl <= 0 {
		return errors.New("--lease-ttl must be a positive duration")
	}
	if ttlSet && c.lease.holder == "" {
		return errors.New("--lease-ttl needs --lease <holder>")
	}
	// Everything below is a refusal fleetd would issue anyway — the point
	// is WHEN. The claim is POSTed after the wait, so under the
	// documented `--timeout 0` idiom a batch parks all night, unblocks at
	// 03:00 and then exits non-zero on a 400 that was decidable before
	// the first poll. Everything else in this function exists for the
	// same reason.
	if c.lease.holder != "" {
		if c.lease.holder == fleetapi.HoldHolder {
			// C11 reserves this holder and the lease endpoint refuses it
			// for a non-hold. Worse than the late failure: --unleased
			// skips its OWN holder, so `--unleased --lease hold` would
			// step over the operator's do-not-touch declaration on the way
			// to failing.
			return fmt.Errorf("%q is the holder reserved for C11 holds — pick another --lease name (to declare a hold, use `vibe cell hold`)",
				fleetapi.HoldHolder)
		}
		if c.lease.ttl > fleetapi.MaxLeaseTTL {
			return fmt.Errorf("--lease-ttl may not exceed %s (the lease store's bound)", fleetapi.MaxLeaseTTL)
		}
	}
	for _, f := range []struct{ flag, value string }{
		{"--model", c.model}, {"--lease", c.lease.holder}, {"--lease-note", c.lease.note},
	} {
		if f.value != termSafe(f.value) {
			return fmt.Errorf("%s contains a control character, which fleetd's field hygiene refuses", f.flag)
		}
	}
	if c.ready && cell == fleetcfg.FrontCell {
		// C8's probeGuard refuses the front for the same reason: the
		// front's rendered config is peers-only, so an id there is a
		// routing entry, not a resident process, and this wait could
		// never end.
		return fmt.Errorf("%s serves no models of its own (its rendered config is peers-only), so %q there is a routing entry that never reports ready — await the cell that holds it",
			fleetcfg.FrontCell, c.model)
	}
	return nil
}

// condResult is one condition's verdict plus the phrase that explains
// it. The progress line, the success line and the timeout error are all
// built from these, so they cannot describe the wait differently.
//
// key is the STATE the detail describes, stripped of every number that
// moves on its own. The progress line is deduplicated on it: `idle 3m of
// 10m` differs from `idle 3m5s of 10m` in text and not at all in
// meaning, and an eight-hour `--timeout 0` wait deduplicated on text
// prints one line per poll.
type condResult struct {
	met    bool
	key    string
	detail string
}

// awaitEval is one round of every requested condition against ONE
// snapshot.
type awaitEval struct {
	ok     bool
	status string   // full progress line
	key    string   // change-detection identity of that line
	extra  []string // details of the non-reachability conditions
	unmet  []string // details of whatever was not met
}

func (c awaitConds) evaluate(snap *fleetapi.StateSnapshot, cell string) (awaitEval, error) {
	var cs *fleetapi.CellSnapshot
	for i := range snap.Cells {
		if snap.Cells[i].Name == cell {
			cs = &snap.Cells[i]
			break
		}
	}
	if cs == nil {
		return awaitEval{}, fmt.Errorf("%w %q (not in fleetd's registry)", errUnknownCell, cell)
	}

	results := []condResult{c.evalReachable(cs)}
	if c.ready {
		r, err := c.evalModel(cs, snap)
		if err != nil {
			return awaitEval{}, err
		}
		results = append(results, r)
	}
	if c.idle > 0 {
		results = append(results, c.evalIdle(cs))
	}
	if c.unleased {
		results = append(results, c.evalLeases(cs))
	}

	ev := awaitEval{ok: true}
	details := make([]string, 0, len(results))
	keys := make([]string, 0, len(results))
	for i, r := range results {
		details = append(details, r.detail)
		key := r.key
		if key == "" {
			key = r.detail
		}
		keys = append(keys, key)
		if i > 0 {
			ev.extra = append(ev.extra, r.detail)
		}
		if !r.met {
			ev.ok = false
			ev.unmet = append(ev.unmet, r.detail)
		}
	}
	ev.status = strings.Join(details, "; ")
	ev.key = strings.Join(keys, "\x00")
	return ev, nil
}

func (c awaitConds) evalReachable(cs *fleetapi.CellSnapshot) condResult {
	// Intent is deliberately not consulted (C1's routing-truth rule): a
	// drained cell that answers is up.
	if cs.Reachable == c.wantUp {
		return condResult{met: true, detail: upWord(c.wantUp)}
	}
	if c.wantUp {
		return condResult{detail: "not answering yet"}
	}
	return condResult{detail: "still answering"}
}

// evalModel reads llama-swap's residency as fleetd merged it (probe
// /running ⋈ /v1/models, or the cell's announce when probes can't reach
// it). Nothing is stored: the bit lives for one snapshot.
func (c awaitConds) evalModel(cs *fleetapi.CellSnapshot, snap *fleetapi.StateSnapshot) (condResult, error) {
	var row *fleetapi.ModelState
	for i := range cs.Models {
		if cs.Models[i].ID == c.model || cs.Models[i].Name == c.model {
			row = &cs.Models[i]
			break
		}
	}
	if row == nil {
		// The typo verdict is only reached on POSITIVE evidence. An
		// unreachable cell has no catalog, and a drained one announces an
		// empty model list by design (C4) — calling either "no such model"
		// would fail the wait during exactly the outage it is riding out.
		if cs.Reachable && len(cs.Models) > 0 {
			return condResult{}, fmt.Errorf("%w %q on cell %q (its catalog: %s)",
				errUnknownModel, c.model, cs.Name, termSafe(catalogSummary(cs.Models)))
		}
		return condResult{key: "model:absent", detail: fmt.Sprintf("%s not in %s's catalog yet", termSafe(c.model), termSafe(cs.Name))}, nil
	}
	if row.State == "ready" {
		return condResult{met: true, key: "model:ready", detail: termSafe(row.ID) + " ready"}, nil
	}
	state := row.State
	if state == "" {
		state = "stopped"
	}
	detail := fmt.Sprintf("%s %s", termSafe(row.ID), termSafe(state))
	if state == "starting" {
		// C1's honest-ETA source. Fleet-wide median over recorded starts,
		// labelled as such — an ETA is not a promise.
		if st, ok := snap.StartHistory[row.ID]; ok && st.P50S > 0 {
			detail += fmt.Sprintf(" (p50 %s over %d recorded starts)",
				time.Duration(st.P50S*float64(time.Second)).Round(time.Second), st.Count)
		}
	}
	return condResult{key: "model:" + state, detail: detail}, nil
}

// catalogSummary lists a few ids so a typo is fixable from the error.
func catalogSummary(models []fleetapi.ModelState) string {
	const max = 8
	ids := make([]string, 0, max+1)
	for i, m := range models {
		if i == max {
			ids = append(ids, fmt.Sprintf("… +%d more", len(models)-max))
			break
		}
		ids = append(ids, m.ID)
	}
	return strings.Join(ids, " ")
}

// evalIdle is the rule the phase exists for: missing evidence is never
// idleness. fleetd publishes what it can vouch for (fleetapi's
// CellActivity, derived from C4's inflight fold and the watcher's own
// connection state); anything short of a computed window keeps waiting
// and says why.
func (c awaitConds) evalIdle(cs *fleetapi.CellSnapshot) condResult {
	a := cs.Activity
	if a == nil || !a.Observed || a.IdleSeconds == nil {
		// Version skew is its own answer: a pre-C10 fleetd sends no
		// activity block at all, and "no evidence for this cell" would
		// send the operator looking at the cell instead of the registry.
		reason := "this fleetd sends no activity block (pre-C10 fleetd? --idle reads /api/fleet/state's activity field)"
		if a != nil {
			reason = "fleetd reports no activity evidence for this cell"
			if a.Reason != "" {
				reason = termSafe(a.Reason)
			}
		}
		return condResult{key: "idle:unknown", detail: "idle unknown: " + reason +
			" — await will not treat missing evidence as idleness"}
	}
	// Clamped before the conversion: float→int64 out of range is
	// implementation-defined in Go, and this number arrives over a
	// network. Negative is nonsense; a century is not a window anyone
	// waits on.
	secs := *a.IdleSeconds
	if secs < 0 {
		secs = 0
	}
	if secs > maxIdleSeconds {
		secs = maxIdleSeconds
	}
	idle := time.Duration(secs * float64(time.Second)).Round(time.Second)
	if a.InFlight != nil && *a.InFlight > 0 {
		return condResult{key: "idle:busy", detail: fmt.Sprintf("%d request(s) in flight", *a.InFlight)}
	}
	// fleetd qualifies a window it can compute but not fully vouch for —
	// today, "the stream is live and no inflight frame has EVER arrived",
	// which is the one reading of `--idle` that rests on the cell's
	// silence rather than on an edge fleetd watched. §3 rule 2 promises
	// it is "surfaced in the status line either way"; dropping it on the
	// success path is how missing evidence becomes a silent green light.
	qual := ""
	if a.Reason != "" {
		qual = " — " + termSafe(a.Reason)
	}
	if idle >= c.idle {
		return condResult{met: true, key: "idle:met", detail: fmt.Sprintf("idle %s (>= %s)%s", idle, c.idle, qual)}
	}
	return condResult{key: "idle:waiting", detail: fmt.Sprintf("idle %s of %s%s", idle, c.idle, qual)}
}

// evalLeases reads C2's advisory store as a DECLARATION the waiter has
// opted to respect. Leases still block nothing; --unleased is the
// waiting process choosing to care. A lease held by this invocation's
// own holder is ignored, so a crashed run of the same job cannot
// deadlock against its own residue.
//
// The lease's MODEL is deliberately not compared: a hold on any model
// of the cell is a hold on the GPU, which is the same reason --idle is
// cell-scoped.
func (c awaitConds) evalLeases(cs *fleetapi.CellSnapshot) condResult {
	var holders []string
	for _, l := range cs.Leases {
		if c.lease.holder != "" && l.Holder == c.lease.holder {
			continue
		}
		// C11's rule for surfaces: a hold is keyed on the RESERVED
		// holder, and "leased by hold" reads as a consumer with an odd
		// name rather than the operator's do-not-touch declaration.
		if l.Holder == fleetapi.HoldHolder {
			holders = append(holders, "held: "+termSafe(l.Model))
			continue
		}
		holders = append(holders, termSafe(l.Holder))
	}
	if len(holders) == 0 {
		return condResult{met: true, detail: "no other leases"}
	}
	return condResult{detail: "leased by " + strings.Join(holders, ", ")}
}

func upWord(up bool) string {
	if up {
		return "up"
	}
	return "down"
}

// acquireLease posts the claim await was asked to take. A failure fails
// the COMMAND: the operator asked for the declaration, and a batch that
// runs undeclared is invisible to the pre-drain report that exists to
// protect it.
func acquireLease(ctx context.Context, target fleetdTarget, cell string, c awaitConds) error {
	body, err := json.Marshal(map[string]string{
		"cell":   cell,
		"model":  c.model,
		"holder": c.lease.holder,
		"note":   c.lease.note,
		"ttl":    c.lease.ttl.String(),
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.base+"/api/fleet/lease", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := vibeclient.ResolveToken(); tok != "" && !strings.HasPrefix(target.base, "http://vibe.local") {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := target.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("POST /api/fleet/lease: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return nil
}
