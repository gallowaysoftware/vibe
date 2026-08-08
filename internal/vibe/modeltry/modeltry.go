// Package modeltry is the sequencing and the safety behind
// `vibe model try` (fleet-control C18): pull a candidate model, scaffold
// a def for it from the def already serving on this box, apply it when
// the cell is quiet, warm it, and measure it against the incumbent.
//
// It is COMPOSITION. The download is hfdownload's, the def shape is
// profile's, the render is router's, the measurement is C8's modelprobe,
// the idleness verdict is fleetd's (C4/C10), the two declarations a trial
// makes are C2's lease store and C11's hold. This package owns exactly
// three things nothing else does: the ORDER, the journal that makes the
// order resumable and reversible, and the refusals.
//
// Four rules shape all of it.
//
//   - **A trial is cell-local.** Every step writes a file on the box the
//     model will run on — the weights, the def, the rendered llama-swap
//     config — and fleetd is read-and-request-only by construction. So
//     `try` runs ON the cell; a --cell naming any other cell is refused
//     structurally rather than half-implemented over SSH the design does
//     not have.
//
//   - **A trial never becomes fleet catalog.** The def carries
//     `trial: true`, which router.Render excludes from the FRONT render;
//     the front's peer map is what makes an id routable fleet-wide, so
//     the trial is reachable only by a direct request to this cell's own
//     llama-swap — which is what this package issues. Promotion is
//     deleting one line and committing the def, and nothing here can do
//     it.
//
//   - **Applying is a declared action deferred by observation** (C14's
//     sentence, verbatim). Writing the cell's llama-swap config is
//     DISRUPTIVE: `-watch-config` reloads on the write and the running
//     upstream drains under llama-swap's hardcoded 30 s (C0's measured
//     gate), so a generation in flight is truncated and every resident
//     model is evicted. The operator DECLARES the trial by running the
//     command; observation may only DELAY the apply. Nothing here infers
//     that a quiet box wants a config change.
//
//   - **Rollback is a path, not a comment.** Every step records what it
//     did before it does it, and `End` walks the record backwards. The
//     one thing it deliberately does not undo is the download: bytes on
//     disk hurt nothing and re-pulling 20 GB to re-run a trial is the
//     toil this verb exists to remove. `--purge` is the explicit form.
package modeltry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/gallowaysoftware/vibe/internal/vibe/benchreplay"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/hfdownload"
	"github.com/gallowaysoftware/vibe/internal/vibe/modelprobe"
	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
	"github.com/gallowaysoftware/vibe/internal/vibe/router"
	"gopkg.in/yaml.v3"
)

// The journal states, in order. They name what is TRUE on disk, not what
// the command was doing when it stopped: a process killed mid-`Fetch`
// leaves `planned`, and the next invocation resumes from there.
const (
	// StatePlanned: the journal exists and nothing else does.
	StatePlanned = "planned"
	// StateFetched: the weights are on disk at their final path.
	StateFetched = "fetched"
	// StateStaged: the trial def is in the backends dir and the render
	// that includes it has been proven to succeed.
	StateStaged = "staged"
	// StateApplied: the cell's llama-swap config on disk includes the
	// trial, and a byte-exact copy of the previous config is banked.
	StateApplied = "applied"
	// StateMeasured: both sides have been probed.
	StateMeasured = "measured"
)

// TrialLeaseHolder is the lease holder a trial claims on its cell. It is
// a plain C2 lease, not a C11 hold: the trial is a consumer using the
// GPU, which is exactly what the advisory store was built to declare —
// it shows in the pre-drain report, and C8's probes and the warm
// schedules skip a leased cell for its duration.
const TrialLeaseHolder = "model-try"

// DefaultQuiet is how long the cell must have been observed idle before
// an apply proceeds. Matches C14's `quiet_for` default for the same
// reason: it is long enough that a human between two prompts does not
// look idle, short enough that an evening trial is not an all-night wait.
const DefaultQuiet = 15 * time.Minute

// DefaultMinFreeBytes is the free space that must REMAIN after the pull.
// The library lives on the box with the ever-growing model library, and
// a full filesystem there breaks llama-swap, the activity store and the
// next pull at once — the failure this check exists to prevent is not
// "the download fails", it is "the download succeeds and everything
// else stops".
const DefaultMinFreeBytes int64 = 20 << 30

// DefaultCatalogWait bounds the wait for the cell's llama-swap to pick
// the new config up. `-watch-config` on v239 polls at 2 s (C0's measured
// gate); a minute is many poll intervals, and running out of it is
// EVIDENCE — it means this llama-swap is not watching — not a reason to
// keep waiting.
const DefaultCatalogWait = 60 * time.Second

// warmTimeout bounds one warm request. A cold 30B-class load runs 6-10
// minutes on the heavy cell, and the whole point of the warm is to pay
// that before the measurement.
const warmTimeout = 20 * time.Minute

// Trial is the journal. It is the only durable state this phase adds,
// and every field on it exists because End needs it to undo something or
// because a later process needs it to resume.
type Trial struct {
	V    int    `json:"v"`
	Cell string `json:"cell"`
	// Def is the trial def's name, which is also the model id the cell's
	// llama-swap serves it under.
	Def string `json:"def"`
	// Incumbent is the def the trial is measured against AND the def its
	// serving flags were copied from. One flag (--like) sets both,
	// because the honest template on this fleet is the def already
	// working on this box: it carries this GPU's layer split, this
	// build's cache types, this operator's context budget.
	Incumbent string `json:"incumbent"`

	Repo     string `json:"repo"`
	File     string `json:"file"`
	Revision string `json:"revision,omitempty"`

	WeightsPath string `json:"weights_path"`
	DefPath     string `json:"def_path"`
	ConfigPath  string `json:"config_path"`
	BackupPath  string `json:"backup_path,omitempty"`
	// ConfigCreated records that ConfigPath did not exist when the trial
	// banked it. "Restore the previous config" then means REMOVING the
	// file, not rendering a second one: a box whose llama-swap is started
	// with some other -config had no file here, and a re-render leaves it
	// one that names every model on the cell and llama-swap's default
	// upstream range. It is journalled rather than derived, because the
	// process that has to undo it is usually a later one.
	ConfigCreated bool `json:"config_created,omitempty"`
	// SizeBytes is what HuggingFace advertised for the file, or -1 when
	// it would not say. -1 is never treated as small.
	SizeBytes int64 `json:"size_bytes"`

	State     string    `json:"state"`
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Live records that the trial id was observed in the cell's own
	// /v1/models after the config was written — i.e. llama-swap actually
	// adopted it. Applied-but-not-live is a real and common state (a
	// llama-swap started without -watch-config) and must not read as
	// either failure or success.
	Live bool `json:"live"`
	// Leased / Held record declarations this trial TOOK, so End releases
	// exactly those and never an operator's own. A hold that was already
	// there when the trial started is left alone in both directions.
	Leased bool `json:"leased"`
	Held   bool `json:"held"`

	Result *Comparison `json:"result,omitempty"`
	// Log is the human-readable trail `vibe model try status` renders. It
	// is append-only and capped; a trial that has been resumed six times
	// should say so.
	Log []LogEntry `json:"log,omitempty"`

	// Resumed marks a trial Plan re-opened rather than created. It is
	// process-local (never serialized) and it exists for exactly one
	// consumer: --dry-run, which closes the journal it opened. Closing a
	// journal it did NOT open is a full rollback of somebody's in-flight
	// trial performed by a flag whose name says nothing happens.
	Resumed bool `json:"-"`
}

// LogEntry is one thing that happened, with its time.
type LogEntry struct {
	At   time.Time `json:"at"`
	Note string    `json:"note"`
}

const logCap = 200

// Measurement is one side of the comparison.
type Measurement struct {
	Model string `json:"model"`
	Kind  string `json:"kind"`
	// Metric is modelprobe's name, carried because decode_tok_s and
	// e2e_tok_s are NOT comparable — one excludes queueing, the other
	// does not — so a report that put them in one column would be
	// comparing two different quantities under one heading.
	Metric string  `json:"metric,omitempty"`
	Value  float64 `json:"value,omitempty"`
	TTFTMS int64   `json:"ttft_ms,omitempty"`
	// BaselineP50 / Samples are C8's rolling window for this model on
	// this box, when there is one. It is the honest multi-sample number
	// beside this run's single sample.
	BaselineP50 float64 `json:"baseline_p50,omitempty"`
	Samples     int     `json:"samples,omitempty"`
	// Note is why there is no number, when there is no number.
	Note string `json:"note,omitempty"`
	// VRAMEstimateGB and WeightsBytes are the resource half of the
	// comparison. A model that is 15% faster and 40% larger has not
	// obviously won, and tok/s alone cannot say so.
	VRAMEstimateGB float64 `json:"vram_estimate_gb,omitempty"`
	WeightsBytes   int64   `json:"weights_bytes,omitempty"`
}

// Comparison is what the trial measured, with what it could not control.
type Comparison struct {
	At        time.Time   `json:"at"`
	Cell      string      `json:"cell"`
	Trial     Measurement `json:"trial"`
	Incumbent Measurement `json:"incumbent"`
	// Caveats are the sentences printed WITH the number, C7b's rule: a
	// screen that can only render triumph will.
	Caveats []string `json:"caveats,omitempty"`
	// Replay is C25's structural block: the same two models scored
	// against n of THIS CELL's own recent requests instead of one canned
	// prompt. Nil when --replay was not asked for, and nil when the sample
	// could not be harvested — the two are different, and ReplayNote says
	// which.
	//
	// It is journalled, and journalling it is safe because benchreplay's
	// Report cannot carry a body: no []byte, no map, no open-set string,
	// gated by a reflection walk with an explicit field allowlist. The
	// SAMPLE is a different matter and is persisted nowhere, ever.
	Replay *benchreplay.Report `json:"replay,omitempty"`
	// ReplayNote is why there is no replay block, when one was asked for.
	ReplayNote string `json:"replay_note,omitempty"`
}

// Options wires a Runner. Every path and URL is injected so the tests
// drive the real sequence against a temp dir and a swaptest cell.
type Options struct {
	// Cell is the cell this box IS (the daemon config's fleet.cell). A
	// trial is placed here and nowhere else.
	Cell string
	// Hosts is the parsed hosts.yaml, or nil on a box that has none.
	Hosts *fleetcfg.File

	BackendsDir       string
	ConfigPath        string // this cell's llama-swap config
	ExtrasPath        string
	LlamaServerBinary string
	// LlamaSwapURL is this box's OWN llama-swap. Always loopback: every
	// request this package issues measures the cell it runs on.
	LlamaSwapURL string

	StatePath      string // the trial journal
	ProbeStatePath string // C8's cell-side baselines

	HTTP *http.Client
	Now  func() time.Time
	// DiskFree reports free bytes on the filesystem holding a directory.
	// Injected because the check must be testable and because an
	// unreadable answer has to be a refusal rather than a zero.
	DiskFree func(dir string) (int64, error)
	// MinFreeBytes is the headroom that must remain after the pull.
	MinFreeBytes int64
	// CatalogWait bounds the post-apply wait for llama-swap to adopt the
	// config.
	CatalogWait time.Duration

	// HeadSize and Fetch are the HuggingFace seam. They default to
	// hfdownload, which reaches the network; injecting them is what lets
	// the sequence, the refusals and the rollback be tested without one.
	HeadSize func(ctx context.Context, spec hfdownload.Spec) (int64, error)
	Fetch    func(ctx context.Context, spec hfdownload.Spec, dest string, progress hfdownload.ProgressFunc) error
}

// Runner executes the steps.
type Runner struct {
	opt Options
}

// New validates the wiring. Everything here is a programming error if
// missing, not an operator error.
func New(opt Options) (*Runner, error) {
	switch {
	case opt.Cell == "":
		return nil, errors.New("modeltry: cell is required (set fleet.cell in the daemon config)")
	case opt.BackendsDir == "":
		return nil, errors.New("modeltry: backends dir is required")
	case opt.ConfigPath == "":
		return nil, errors.New("modeltry: llama-swap config path is required")
	case opt.LlamaSwapURL == "":
		return nil, errors.New("modeltry: llama_swap_url is required")
	case opt.StatePath == "":
		return nil, errors.New("modeltry: state path is required")
	}
	if opt.HTTP == nil {
		opt.HTTP = &http.Client{}
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.MinFreeBytes == 0 {
		opt.MinFreeBytes = DefaultMinFreeBytes
	}
	if opt.CatalogWait == 0 {
		opt.CatalogWait = DefaultCatalogWait
	}
	if opt.HeadSize == nil {
		opt.HeadSize = hfdownload.HeadSize
	}
	if opt.Fetch == nil {
		opt.Fetch = hfdownload.Download
	}
	return &Runner{opt: opt}, nil
}

// Cell reports the cell this runner acts on.
func (r *Runner) Cell() string { return r.opt.Cell }

// ── the journal ─────────────────────────────────────────────────────────

// Load reads the in-flight trial, or (nil, nil) when there is none. A
// journal that will not parse is an ERROR, never an absent trial: the
// file is the only record of what has to be rolled back, and reading it
// as "no trial" is how a half-applied config becomes permanent.
func (r *Runner) Load() (*Trial, error) {
	data, err := os.ReadFile(r.opt.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read trial journal %s: %w", r.opt.StatePath, err)
	}
	var t Trial
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("trial journal %s is corrupt (%w) — it is the record of what a rollback has to undo, so it is not treated as \"no trial\"; inspect it by hand", r.opt.StatePath, err)
	}
	return &t, nil
}

func (r *Runner) save(t *Trial) error {
	t.UpdatedAt = r.opt.Now().UTC()
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(r.opt.StatePath, data, 0o600)
}

func (t *Trial) note(now time.Time, format string, args ...any) {
	t.Log = append(t.Log, LogEntry{At: now.UTC(), Note: fmt.Sprintf(format, args...)})
	if len(t.Log) > logCap {
		t.Log = t.Log[len(t.Log)-logCap:]
	}
}

// ── plan ────────────────────────────────────────────────────────────────

// PlanRequest is what the operator asked for.
type PlanRequest struct {
	Repo     string
	File     string
	Revision string
	// Like is the incumbent def: both the template for the trial's
	// serving flags and the model the trial is measured against.
	Like string
	// As overrides the derived trial def name.
	As string
	// Dest overrides the directory the weights land in (default: the
	// directory holding the incumbent's weights).
	Dest string
	// SkipDiskCheck is the operator override for an unknowable size. It
	// exists because "HuggingFace did not send Content-Length" must not
	// silently read as "it fits".
	SkipDiskCheck bool
}

// Plan validates everything that can be decided before anything is
// written, derives the trial def, and opens the journal. It is the whole
// refusal surface: after Plan returns, every later step is mechanical.
//
// When a journal already exists it RESUMES it, provided the request
// names the same trial. Resuming is the point: the fetch is the long
// step, the apply is the disruptive one, and a command that could only
// ever start from scratch would make "re-run to continue" a promise the
// code does not keep. A request naming a DIFFERENT trial is refused —
// one trial at a time per cell — and says which one is open.
func (r *Runner) Plan(ctx context.Context, req PlanRequest) (*Trial, *profile.BackendDef, error) {
	existing, err := r.Load()
	if err != nil {
		return nil, nil, err
	}
	if err := r.refuseStructural(); err != nil {
		return nil, nil, err
	}
	if req.Repo == "" || req.File == "" {
		return nil, nil, errors.New("a HuggingFace repo and a file are required (vibe model try <owner>/<repo> --file <name>.gguf)")
	}
	if err := checkPrintable(req); err != nil {
		return nil, nil, err
	}
	if req.Like == "" {
		return nil, nil, errors.New("--like <def> is required: it names the def whose serving flags the trial copies AND the model the trial is measured against")
	}
	if existing != nil {
		return r.resume(existing, req)
	}

	defs, err := router.LoadDefs(r.opt.BackendsDir)
	if err != nil {
		return nil, nil, fmt.Errorf("load backend defs: %w", err)
	}
	inc := findDef(defs, req.Like)
	if inc == nil {
		return nil, nil, fmt.Errorf("no backend def named %q under %s", req.Like, r.opt.BackendsDir)
	}
	if err := r.checkIncumbent(inc); err != nil {
		return nil, nil, err
	}

	name := req.As
	if name == "" {
		name = derivedName(req.Repo, req.File)
	}
	if err := validDefName(name); err != nil {
		return nil, nil, err
	}
	if findDef(defs, name) != nil {
		return nil, nil, fmt.Errorf("a backend def named %q already exists — pass --as <name> for the trial (a trial must never overwrite a def that serves something)", name)
	}

	dest := req.Dest
	if dest == "" {
		dest = filepath.Dir(inc.Backend.LlamaServer.Path)
	}
	weights := filepath.Join(dest, filepath.Base(req.File))
	if owner := defServingPath(defs, weights); owner != "" {
		return nil, nil, fmt.Errorf("%s is the weights path backend def %q already serves; refusing to download over it (pass --dest <dir>)", weights, owner)
	}

	trialDef := deriveDef(inc, name, weights, r.opt.Cell)
	if err := r.checkKind(trialDef); err != nil {
		return nil, nil, err
	}

	size, sizeErr := r.opt.HeadSize(ctx, hfdownload.Spec{Repo: req.Repo, File: req.File, Revision: req.Revision})
	if sizeErr != nil {
		size = -1
	}
	if err := r.checkDisk(dest, size, sizeErr, req.SkipDiskCheck); err != nil {
		return nil, nil, err
	}

	now := r.opt.Now()
	t := &Trial{
		V: 1, Cell: r.opt.Cell, Def: name, Incumbent: inc.Name,
		Repo: req.Repo, File: req.File, Revision: req.Revision,
		WeightsPath: weights,
		DefPath:     filepath.Join(r.opt.BackendsDir, name+".yaml"),
		ConfigPath:  r.opt.ConfigPath,
		SizeBytes:   size,
		State:       StatePlanned,
		StartedAt:   now.UTC(),
	}
	t.note(now, "planned: %s/%s -> %s, def %q modelled on %q", req.Repo, req.File, weights, name, inc.Name)
	if err := r.save(t); err != nil {
		return nil, nil, err
	}
	return t, trialDef, nil
}

// resume re-opens an existing journal for a request that names the same
// trial, re-deriving the def from the CURRENT incumbent — so a def edit
// made between the fetch and the apply is picked up rather than
// silently serving a stale derivation.
func (r *Runner) resume(t *Trial, req PlanRequest) (*Trial, *profile.BackendDef, error) {
	name := req.As
	if name == "" {
		name = derivedName(req.Repo, req.File)
	}
	// Revision and --dest are part of the trial's IDENTITY, not decoration.
	// A resume that ignored them would silently serve the revision the
	// operator replaced and write the def for weights at the old path,
	// while echoing back the arguments they just typed.
	same := t.Def == name && t.Repo == req.Repo && t.File == req.File &&
		t.Incumbent == req.Like && t.Revision == req.Revision &&
		(req.Dest == "" || t.WeightsPath == filepath.Join(req.Dest, filepath.Base(req.File)))
	if !same {
		return nil, nil, fmt.Errorf("a trial of %s (%s/%s%s, modelled on %s, weights %s) is already in flight on %s at state %s — one trial at a time per cell. "+
			"Finish it (`vibe model try status`) or roll it back (`vibe model try end`) before starting another",
			t.Def, t.Repo, t.File, revisionSuffix(t.Revision), t.Incumbent, t.WeightsPath, t.Cell, t.State)
	}
	if err := r.agreesWithJournal(t); err != nil {
		return nil, nil, err
	}
	// §7's refusals are not a one-time toll paid at Plan. The commonest
	// resume is a killed twenty-minute pull — journal at `planned`, a
	// `.partial` on disk, hours elapsed — and the library filesystem is
	// exactly the thing that may have filled in between. The failure this
	// guards is not "the download fails", it is "the download succeeds and
	// llama-swap, the activity store and the next pull all stop", and a
	// resume that skipped the check would walk straight into it with the
	// operator believing they were re-running a checked command.
	if t.State == StatePlanned {
		if err := r.checkDisk(filepath.Dir(t.WeightsPath), t.SizeBytes, nil, req.SkipDiskCheck); err != nil {
			return nil, nil, err
		}
	}
	defs, err := router.LoadDefs(r.opt.BackendsDir)
	if err != nil {
		return nil, nil, fmt.Errorf("load backend defs: %w", err)
	}
	inc := findDef(defs, t.Incumbent)
	if inc == nil {
		return nil, nil, fmt.Errorf("the incumbent def %q has disappeared from %s since this trial started; `vibe model try end` rolls it back", t.Incumbent, r.opt.BackendsDir)
	}
	if err := r.checkIncumbent(inc); err != nil {
		return nil, nil, err
	}
	t.Resumed = true
	t.note(r.opt.Now(), "resumed at state %s", t.State)
	if err := r.save(t); err != nil {
		return nil, nil, err
	}
	return t, deriveDef(inc, t.Def, t.WeightsPath, r.opt.Cell), nil
}

// DiscardIfFresh closes a journal THIS invocation opened, and refuses to
// close one it merely re-opened. It is --dry-run's only rollback: `End`
// works from any state, so calling it on a resumed trial removes the def,
// re-renders the cell's llama-swap config and evicts every resident model
// — a full rollback performed by a flag whose entire promise is that
// nothing happens. Reports whether it closed anything so the caller can
// tell the operator which of the two it did.
func (r *Runner) DiscardIfFresh(ctx context.Context, t *Trial) (bool, error) {
	if t.Resumed {
		return false, nil
	}
	if _, err := r.End(ctx, t, false); err != nil {
		return false, err
	}
	return true, nil
}

// Save persists the journal. Exported because the two control-plane
// declarations a trial makes are taken by the CLI, and a declaration that
// is not on disk before the next step can fail is one `end` will never
// release: the lease and the hold outlive the process by four hours, and
// the next run deadlocks on its own hold under --unleased.
func (r *Runner) Save(t *Trial) error { return r.save(t) }

// agreesWithJournal refuses to act on a journal written against a
// different cell or a different llama-swap config. Both are reachable by
// editing the daemon config mid-trial, and both would make `end` restore
// a banked config over a file it never banked.
func (r *Runner) agreesWithJournal(t *Trial) error {
	if t.Cell != r.opt.Cell {
		return fmt.Errorf("the open trial was placed on cell %q and this box now declares fleet.cell %q; refusing to act on it — put fleet.cell back and roll it back there, or remove %s by hand",
			t.Cell, r.opt.Cell, r.opt.StatePath)
	}
	if t.ConfigPath != "" && t.ConfigPath != r.opt.ConfigPath {
		return fmt.Errorf("the open trial applied to %s and this box now renders to %s; refusing to act on it — the rollback would restore a config it never banked",
			t.ConfigPath, r.opt.ConfigPath)
	}
	return nil
}

// refuseStructural holds the refusals `force` never bypasses, C14's rule
// applied here: the front serves no models of its own, and a cell this
// box is not cannot be reached by writing files locally.
func (r *Runner) refuseStructural() error {
	if r.opt.Cell == fleetcfg.FrontCell {
		return fmt.Errorf("%s serves no models of its own (its rendered config is peers-only), so there is nothing on it to try a model against — run this on the cell that will hold the weights",
			fleetcfg.FrontCell)
	}
	if r.opt.Hosts.HasCells() {
		if _, ok := r.opt.Hosts.Cells[r.opt.Cell]; !ok {
			return fmt.Errorf("this box declares fleet.cell %q, which is not in hosts.yaml — fix the membership before placing a trial on it", r.opt.Cell)
		}
	}
	return nil
}

// RefuseRemote is the --cell refusal, held here rather than in the CLI so
// there is one sentence and one rule. Every step of a trial writes a file
// on the box that will serve it, and fleetd is read-and-request-only:
// there is no verb in this control plane that makes another box download
// 20 GB and rewrite its router config, and inventing one is a phase, not
// a flag.
func (r *Runner) RefuseRemote(cell string) error {
	if cell == "" || cell == r.opt.Cell {
		return nil
	}
	return fmt.Errorf("--cell %q is not this box's cell (fleet.cell %q): a trial downloads weights, writes a def and rewrites the router config, all of which happen on the cell — run it there. Cross-cell `try` is deliberately not implemented (see c18-model-try.md, \"What C18 does not do\")",
		cell, r.opt.Cell)
}

// checkIncumbent enforces what --like has to be for the derivation to
// mean anything.
func (r *Runner) checkIncumbent(def *profile.BackendDef) error {
	switch {
	case def.Trial:
		return fmt.Errorf("--like %s is itself a trial def; measure against something the fleet actually runs", def.Name)
	case !def.Backend.External:
		return fmt.Errorf("--like %s is not external (backend.external: true) — the router does not serve it, so neither the flags nor the comparison transfer", def.Name)
	case def.Backend.LlamaServer == nil:
		return fmt.Errorf("--like %s is not a llama_server def; C18 scaffolds a single-GGUF llama_server trial only (an mlx_server candidate is a snapshot directory, not a file, and needs its own pull path)", def.Name)
	case def.Cell != "" && def.Cell != r.opt.Cell:
		return fmt.Errorf("--like %s is assigned to cell %q, not this box's %q — its flags describe another box's GPU", def.Name, def.Cell, r.opt.Cell)
	}
	return nil
}

// checkKind reads the probe kind off the trial's own rendered argv —
// C8's source, not the model name and not hosts.yaml's model_classes.
// The class table guards `warmViaFront`, which is a CHAT completion sent
// through the front; this package sends the request the argv implies
// directly to this cell, so the class pin is not the applicable
// declaration. What IS applicable is that some kinds have no
// single-request probe at all.
func (r *Runner) checkKind(def *profile.BackendDef) error {
	spec := modelprobe.SpecsFromDefs([]*profile.BackendDef{def}, r.opt.LlamaServerBinary)[def.Name]
	if spec.Disabled {
		return fmt.Errorf("the flags this def would serve with make it un-probeable (a reranker's request body is a query plus documents, not a batch), so there is no honest measurement to report — try it by hand")
	}
	return nil
}

// checkDisk refuses a pull that would leave the library filesystem
// under the headroom, and refuses just as hard when it cannot tell.
// Absent evidence is not a healthy value: a HEAD that returned no size
// is not a small file.
func (r *Runner) checkDisk(dest string, size int64, sizeErr error, skip bool) error {
	if skip {
		return nil
	}
	if r.opt.DiskFree == nil {
		return errors.New("no disk-free probe wired; refusing to pull a model without one (--skip-disk-check overrides)")
	}
	free, err := r.opt.DiskFree(dest)
	if err != nil {
		return fmt.Errorf("cannot read free space on %s (%w) — refusing to pull: a full model library breaks llama-swap, the activity store and the next pull at once (--skip-disk-check overrides)", dest, err)
	}
	if size <= 0 {
		why := "HuggingFace did not report a size for it"
		if sizeErr != nil {
			why = "the size request failed: " + sizeErr.Error()
		}
		return fmt.Errorf("refusing to pull %s: %s, and an unknown size is not a small one. %s free at %s; re-run with --skip-disk-check if you know it fits",
			dest, why, humanBytes(free), dest)
	}
	if free-size < r.opt.MinFreeBytes {
		left := "nothing (the file is larger than the free space)"
		if free-size >= 0 {
			left = humanBytes(free - size)
		}
		return fmt.Errorf("refusing to pull: %s free at %s, the file is %s, and that would leave %s — under the %s this box keeps for llama-swap, the activity store and the next pull (--min-free changes the floor, --skip-disk-check removes it)",
			humanBytes(free), dest, humanBytes(size), left, humanBytes(r.opt.MinFreeBytes))
	}
	return nil
}

// ── fetch ───────────────────────────────────────────────────────────────

// Fetch pulls the weights. It is the only long step and the only one
// that changes nothing about what the cell is serving — a half-finished
// download leaves a `.partial`/`.incomplete` sibling, the journal at
// `planned`, and the fleet exactly as it was.
func (r *Runner) Fetch(ctx context.Context, t *Trial, progress hfdownload.ProgressFunc) error {
	if t.State != StatePlanned {
		return nil
	}
	spec := hfdownload.Spec{Repo: t.Repo, File: t.File, Revision: t.Revision}
	if err := r.opt.Fetch(ctx, spec, t.WeightsPath, progress); err != nil {
		return fmt.Errorf("download %s/%s: %w", t.Repo, t.File, err)
	}
	info, err := os.Stat(t.WeightsPath)
	if err != nil {
		return fmt.Errorf("the download reported success but %s is not there: %w", t.WeightsPath, err)
	}
	if t.SizeBytes > 0 && info.Size() != t.SizeBytes {
		return fmt.Errorf("%s is %d bytes, HuggingFace advertised %d — refusing to serve a short file (delete it and re-run)", t.WeightsPath, info.Size(), t.SizeBytes)
	}
	t.State = StateFetched
	t.note(r.opt.Now(), "fetched %s (%s)", t.WeightsPath, humanBytes(info.Size()))
	return r.save(t)
}

// ── stage ───────────────────────────────────────────────────────────────

// Stage writes the trial def and proves the render that includes it
// succeeds. The proof runs BEFORE the def is committed to the backends
// dir, because LoadDefs treats one invalid def as a hard error for the
// whole directory: a trial def that will not parse would break every
// render and every announce on this box, including the ones that end the
// trial.
func (r *Runner) Stage(t *Trial, def *profile.BackendDef) error {
	if t.State == StateApplied || t.State == StateMeasured {
		return nil
	}
	if t.State != StateFetched && t.State != StateStaged {
		return fmt.Errorf("cannot stage a trial in state %q (the weights are not on disk yet)", t.State)
	}
	preview, err := PreviewDef(t, def)
	if err != nil {
		return fmt.Errorf("marshal trial def: %w", err)
	}
	staged := []byte(preview)
	// Resuming at `staged`: Plan re-derived the def from the CURRENT
	// incumbent, and returning early here would discard that and keep
	// serving the derivation from before the operator's edit — the exact
	// staleness the re-derivation exists to prevent. Identical bytes are
	// the common case and cost one comparison.
	if t.State == StateStaged {
		prev, rerr := os.ReadFile(t.DefPath)
		if rerr == nil && string(prev) == preview {
			return nil
		}
		if rerr == nil {
			if err := r.restageChangedDef(t, staged, prev); err != nil {
				return err
			}
			t.note(r.opt.Now(), "re-derived the trial def from the current %s", t.Incumbent)
			return r.save(t)
		}
		if !errors.Is(rerr, os.ErrNotExist) {
			return fmt.Errorf("read the staged trial def %s: %w", t.DefPath, rerr)
		}
		// The def is gone (someone deleted it under us). Fall through and
		// write it again, which is what `staged` claims is true on disk.
	}

	// Round-trip through the real loader in a scratch dir: the def that
	// reaches the backends dir is one that profile.LoadBackendFrom has
	// already accepted, with this def's cell scoping and this repo's
	// KnownFields(true).
	tmpDir, err := os.MkdirTemp("", "vibe-trial-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	if err := os.WriteFile(filepath.Join(tmpDir, t.Def+".yaml"), staged, 0o644); err != nil {
		return err
	}
	if _, err := profile.LoadBackendFrom(tmpDir, t.Def); err != nil {
		return fmt.Errorf("the scaffolded def does not load (%w) — nothing was written; this is a bug in the derivation, not in your input", err)
	}

	// Atomically: router.LoadDefs treats ONE unparseable def as a hard
	// error for the whole directory, so a torn write here breaks every
	// render and every announce on this box until someone finds the file.
	if err := writeFileAtomic(t.DefPath, staged, 0o644); err != nil {
		return fmt.Errorf("write trial def %s: %w", t.DefPath, err)
	}
	// Prove the full render with the def in place. On failure the def is
	// removed again, so a render that cannot succeed never survives the
	// step that would have made every later render fail too.
	if _, err := r.render(false); err != nil {
		_ = os.Remove(t.DefPath)
		return fmt.Errorf("rendering %s with the trial def failed (%w) — the def has been removed and nothing on this box changed", r.opt.ConfigPath, err)
	}
	t.State = StateStaged
	t.note(r.opt.Now(), "staged def %s", t.DefPath)
	return r.save(t)
}

// restageChangedDef swaps a re-derived trial def in and proves the render
// still succeeds, restoring the PREVIOUS bytes if it does not. The
// restore is the whole point: at `staged` there is already a def the box
// renders, so failing the way the first stage fails (remove it, report
// "nothing changed") would leave the trial in a state its own journal
// says is impossible.
func (r *Runner) restageChangedDef(t *Trial, staged, prev []byte) error {
	if err := writeFileAtomic(t.DefPath, staged, 0o644); err != nil {
		return fmt.Errorf("write trial def %s: %w", t.DefPath, err)
	}
	if _, err := r.render(false); err != nil {
		if rerr := writeFileAtomic(t.DefPath, prev, 0o644); rerr != nil {
			return fmt.Errorf("the re-derived def does not render (%w) AND restoring the previous one failed (%w) — %s is now unrenderable, fix or remove it by hand", err, rerr, t.DefPath)
		}
		return fmt.Errorf("the trial def re-derived from the current %s does not render (%w) — the previously staged def has been put back and nothing on this box changed", t.Incumbent, err)
	}
	return nil
}

// render runs the cell's own render. write=false is the proof pass.
func (r *Runner) render(write bool) (router.Outcome, error) {
	return router.RenderToFile(r.opt.BackendsDir, r.opt.ConfigPath, router.Options{
		LlamaServerBinary: r.opt.LlamaServerBinary,
		ExtrasPath:        r.opt.ExtrasPath,
		Cell:              r.opt.Cell,
		Hosts:             r.opt.Hosts,
	}, write)
}

// ── apply ───────────────────────────────────────────────────────────────

// ApplyResult reports what the write did and whether llama-swap took it.
type ApplyResult struct {
	Changed bool
	Diff    string
	Live    bool
	// Note names the evidence when Live is false. A llama-swap started
	// without -watch-config is the common cause and the message says so;
	// it is not a failure of the trial, it is a fact about the cell that
	// the operator has to act on.
	Note string
}

// Apply writes the cell's llama-swap config with the trial in it, having
// first banked a byte-exact copy of what was there. The caller is
// responsible for the deferral: the idleness verdict belongs to fleetd
// (C4's activity fold, C10's --idle rule) and is applied before this is
// called. Nothing in this function infers anything about the cell's
// state — it is the declared action, and it happens when it is called.
func (r *Runner) Apply(ctx context.Context, t *Trial) (ApplyResult, error) {
	if t.State == StateMeasured {
		return ApplyResult{Live: t.Live}, nil
	}
	if t.State != StateStaged && t.State != StateApplied {
		return ApplyResult{}, fmt.Errorf("cannot apply a trial in state %q", t.State)
	}
	if t.State == StateStaged {
		if err := r.bankConfig(t); err != nil {
			return ApplyResult{}, err
		}
		// Prove the write BEFORE it happens, against the bytes llama-swap
		// is serving now. A trial adds one models: entry and must change
		// nothing else; a render that would also DELETE a hand-written
		// section is not an apply, it is a config edit nobody asked for.
		proof, err := r.render(false)
		if err != nil {
			return ApplyResult{}, fmt.Errorf("render %s: %w", r.opt.ConfigPath, err)
		}
		if err := r.refuseDroppedSections(proof.Rendered); err != nil {
			return ApplyResult{}, err
		}
		out, err := r.render(true)
		if err != nil {
			return ApplyResult{}, fmt.Errorf("render %s: %w", r.opt.ConfigPath, err)
		}
		t.State = StateApplied
		t.note(r.opt.Now(), "applied: wrote %s (changed=%v), previous config banked at %s", r.opt.ConfigPath, out.Changed, t.BackupPath)
		if err := r.save(t); err != nil {
			return ApplyResult{}, err
		}
		res := ApplyResult{Changed: out.Changed, Diff: out.Diff}
		res.Live, res.Note = r.awaitCatalog(ctx, t.Def)
		t.Live = res.Live
		t.note(r.opt.Now(), "catalog: live=%v %s", res.Live, res.Note)
		return res, r.save(t)
	}
	res := ApplyResult{}
	res.Live, res.Note = r.awaitCatalog(ctx, t.Def)
	t.Live = res.Live
	return res, r.save(t)
}

// bankConfig copies the current llama-swap config verbatim beside the
// journal. Rollback prefers a fresh render (defs may legitimately have
// changed while the trial ran), but a render can fail for reasons that
// have nothing to do with the trial, and then the operator needs the
// exact bytes back.
func (r *Runner) bankConfig(t *Trial) error {
	backup := r.opt.StatePath + ".config.bak"
	data, err := os.ReadFile(r.opt.ConfigPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// No config yet is a real state (a fresh cell, or — far more
		// commonly — a llama-swap started with a different -config).
		// Record the absence explicitly so End removes the file it
		// created rather than restoring bytes that never existed.
		t.BackupPath = ""
		t.ConfigCreated = true
		return nil
	case err != nil:
		return fmt.Errorf("read %s before applying: %w", r.opt.ConfigPath, err)
	}
	if err := writeFileAtomic(backup, data, 0o644); err != nil {
		return fmt.Errorf("bank the current config: %w", err)
	}
	t.BackupPath = backup
	return nil
}

// awaitCatalog polls this cell's own llama-swap until the trial id shows
// up in its catalog. Running out of the budget is EVIDENCE, not a
// timeout to retry: llama-swap either watches its config or it does not.
func (r *Runner) awaitCatalog(ctx context.Context, id string) (bool, string) {
	// time.Now, not r.opt.Now: the loop sleeps on the wall clock, and a
	// deadline from an injectable clock that does not advance would spin
	// here forever. The two must be the same clock.
	deadline := time.Now().Add(r.opt.CatalogWait)
	var last string
	for {
		ids, err := r.catalog(ctx)
		if err != nil {
			last = err.Error()
		} else {
			last = ""
			for _, got := range ids {
				if got == id {
					return true, "the cell's llama-swap adopted the config"
				}
			}
		}
		if !time.Now().Before(deadline) {
			why := fmt.Sprintf("%s is not in this cell's /v1/models after %s", id, r.opt.CatalogWait)
			if last != "" {
				why += " (last read: " + printableSnippet(last) + ")"
			}
			// TWO causes, named, because prescribing only the first one
			// sends an operator to restart a llama-swap that will come back
			// reading the same file it was already reading. The restart
			// fixes a cell started without -watch-config; nothing fixes a
			// cell started with a DIFFERENT -config except writing the file
			// it actually reads.
			return false, why + " — " + r.opt.ConfigPath + " is written, so either this llama-swap was started without -watch-config " +
				"(`systemctl --user restart llama-swap`, which evicts whatever is resident), or it was started with a different `-config` " +
				"and that file is the one to write (`vibe model try --config <path>`; check the unit's ExecStart)"
		}
		select {
		case <-ctx.Done():
			return false, "cancelled while waiting for the cell's llama-swap to adopt the config"
		case <-time.After(2 * time.Second):
		}
	}
}

func (r *Runner) catalog(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(r.opt.LlamaSwapURL, "/")+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	// No Authorization header, by the same rule as the cell-side probe
	// and the cell-side version read (C15 §8): a box reading its OWN
	// 127.0.0.1 llama-swap resolves no credential, because the credential
	// is declared per CELL in fleetd's hosts.yaml and a cell need not
	// hold that file at all. A cell that keys its own llama-swap gets a
	// 401 here, reported as itself — C15's recorded futures item, not a
	// credential invented in this package.
	resp, err := r.opt.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /v1/models: HTTP %d", resp.StatusCode)
	}
	var wrap struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&wrap); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(wrap.Data))
	for _, d := range wrap.Data {
		out = append(out, d.ID)
	}
	return out, nil
}

// ── measure ─────────────────────────────────────────────────────────────

// Harvest takes the C25 replay sample off this cell — BEFORE the apply,
// which is the only time it exists.
//
// C18's apply writes the cell's llama-swap config, and a `-watch-config`
// reload does not mutate a running server: it builds a new one, and the
// new one allocates a fresh, empty capture buffer. The activity STORE is
// carried across; the capture buffer is not. So at the moment the
// candidate first exists, the sample is gone.
//
// The ordering is therefore enforced rather than documented, in two
// places that cannot be satisfied out of order:
//
//   - this method refuses once the journal says `applied` or `measured`,
//     and the journal is what a resumed process reads;
//   - Measure takes the sample as a PARAMETER, and benchreplay.Harvest is
//     the only producer of one, so there is no path from inside the
//     measurement to a sample of a buffer the apply has already emptied.
//
// A trial resumed at `applied` — the not-live case, or a process death —
// therefore measures without a replay and says so. That is the honest
// outcome: the sample it would have used no longer exists.
func (r *Runner) Harvest(ctx context.Context, t *Trial, opt benchreplay.Options) (*benchreplay.Set, error) {
	if opt.LlamaSwapURL == "" {
		opt.LlamaSwapURL = r.opt.LlamaSwapURL
	}
	if opt.ConfigPath == "" {
		opt.ConfigPath = r.opt.ConfigPath
	}
	if opt.HTTP == nil {
		opt.HTTP = r.opt.HTTP
	}
	if opt.Now == nil {
		opt.Now = r.opt.Now
	}
	return benchreplay.Harvest(ctx, opt, t.Incumbent, t.State == StateApplied || t.State == StateMeasured)
}

// Measure warms and probes both sides and records the comparison.
//
// The order is incumbent first, then trial, and it is not arbitrary: on
// a single-GPU cell loading one evicts the other, so the two CANNOT be
// measured concurrently or in an interleaved way, and whichever goes
// second was written to disk more recently. That is a caveat, not a
// control, and it is printed with the number.
func (r *Runner) Measure(ctx context.Context, t *Trial, replay *benchreplay.Set) (*Comparison, error) {
	if t.State != StateApplied && t.State != StateMeasured {
		return nil, fmt.Errorf("cannot measure a trial in state %q (apply it first)", t.State)
	}
	if !t.Live {
		return nil, fmt.Errorf("%s is not in this cell's llama-swap catalog, so a measurement would be measuring nothing — apply the config first (`vibe model try status` says how)", t.Def)
	}
	defs, err := router.LoadDefs(r.opt.BackendsDir)
	if err != nil {
		return nil, fmt.Errorf("load backend defs: %w", err)
	}
	specs := modelprobe.SpecsFromDefs(defs, r.opt.LlamaServerBinary)
	prober, err := modelprobe.New(modelprobe.Config{
		LlamaSwapURL: r.opt.LlamaSwapURL,
		StatePath:    r.opt.ProbeStatePath,
		// READ-ONLY. The cell daemon's own prober owns this file and
		// rewrites it whole from memory; a trial that wrote it back would
		// delete every baseline sample and every budget entry the daemon
		// recorded during the twenty minutes a trial takes. The incumbent's
		// rolling window is the only multi-sample number on the report, so
		// it is read — and left exactly as it was found.
		ReadOnly: true,
		Specs: func(model string) modelprobe.Spec {
			if s, ok := specs[model]; ok {
				return s
			}
			return modelprobe.Spec{Model: model, Kind: modelprobe.KindChat}
		},
		HTTPClient: r.opt.HTTP,
		Now:        r.opt.Now,
	})
	if err != nil {
		return nil, err
	}

	cmp := &Comparison{At: r.opt.Now().UTC(), Cell: r.opt.Cell}
	// The two sides, in the one order a single-GPU cell allows, with each
	// side's C8 probe AND its C25 replay taken while that side is the
	// resident one. Interleaving them this way is what keeps the sequence
	// at two cold loads rather than four: warm+probe+replay the incumbent,
	// then warm+probe+replay the candidate, which evicts it.
	//
	// benchreplay never loads a model — C8's cardinal rule holds on its
	// side of the seam too — so it re-checks /running itself and refuses.
	if replay.Len() > 0 {
		rep, rerr := replay.Run(ctx, r.opt.Cell,
			benchreplay.Side{Model: t.Incumbent, Warm: func(ctx context.Context) error {
				cmp.Incumbent = r.measureOne(ctx, prober, specs, defs, t.Incumbent)
				return nil
			}},
			benchreplay.Side{Model: t.Def, Warm: func(ctx context.Context) error {
				cmp.Trial = r.measureOne(ctx, prober, specs, defs, t.Def)
				return nil
			}},
		)
		if rerr != nil {
			// A failed replay must not cost the throughput comparison, and
			// it must not be silent either. printableSnippet for the same
			// reason every other stored string here gets it: this note is
			// journalled and re-printed by every later `status`.
			cmp.ReplayNote = printableSnippet("the replay produced no score: " + rerr.Error())
		} else {
			cmp.Replay = rep
		}
	}
	// Whichever side the replay did not prepare is measured here. Both,
	// when there is no replay; neither, when there is one and it ran.
	if cmp.Incumbent.Model == "" {
		cmp.Incumbent = r.measureOne(ctx, prober, specs, defs, t.Incumbent)
	}
	if cmp.Trial.Model == "" {
		cmp.Trial = r.measureOne(ctx, prober, specs, defs, t.Def)
	}
	cmp.Trial.WeightsBytes = fileSize(t.WeightsPath)
	if inc := findDef(defs, t.Incumbent); inc != nil && inc.Backend.LlamaServer != nil {
		cmp.Incumbent.WeightsBytes = fileSize(inc.Backend.LlamaServer.Path)
	}
	cmp.Caveats = caveats(cmp)

	t.Result = cmp
	t.State = StateMeasured
	t.note(r.opt.Now(), "measured %s vs %s", t.Def, t.Incumbent)
	return cmp, r.save(t)
}

// measureOne warms a model and then probes it. The warm is deliberate
// and declared — it is what makes the probe legal at all, since C8's
// cardinal rule is that a probe never loads a model — and it is the
// operator's request, not fleetd's policy.
func (r *Runner) measureOne(ctx context.Context, prober *modelprobe.Prober, specs map[string]modelprobe.Spec, defs []*profile.BackendDef, model string) Measurement {
	m := Measurement{Model: model, Kind: modelprobe.KindChat}
	if s, ok := specs[model]; ok && s.Kind != "" {
		m.Kind = s.Kind
	}
	if def := findDef(defs, model); def != nil {
		m.VRAMEstimateGB = def.EstimatedVRAMGB
	}
	if err := r.warm(ctx, model, m.Kind); err != nil {
		m.Note = printableSnippet("warm failed: " + err.Error())
		return m
	}
	// rebaseline is FALSE, deliberately. Passing true would wipe the
	// incumbent's C8 rolling window on this box — the only multi-sample
	// number the report prints, and the one thing that makes a single
	// sample interpretable. The trial's own key is new, so it starts
	// empty either way and reports `unknown` under C8's five-sample
	// minimum, which is the honest verdict for one measurement.
	res := prober.Run(ctx, model, false)
	if res == nil {
		m.Note = "the prober returned nothing"
		return m
	}
	m.Metric, m.Value, m.TTFTMS = res.Metric, res.Value, res.TTFTMS
	m.BaselineP50, m.Samples = res.BaselineP50, res.Samples
	if res.Note != "" {
		// Scrubbed for the same reason `warm` scrubs, and it is the
		// LARGER exposure of the two: modelprobe's postJSON embeds up to
		// 4096 raw bytes of llama-swap's body in the error it returns,
		// and that error becomes this Note — which is SAVED to the journal
		// and re-printed by `Comparison.Render` and by every later
		// `vibe model try status`. C8 renders its notes through fleetd's
		// own hygiene; C18 stores them, so the rule has to be held here,
		// at the point of capture.
		m.Note = printableSnippet(res.Note)
	}
	if m.Value <= 0 && m.Note == "" {
		m.Note = "the probe produced no rate"
	}
	return m
}

// warm issues one real request so llama-swap JIT-loads the model. The
// BODY is derived from the def's own rendered argv (C8's kind), never
// from the model's name: sending a chat completion to an embedding
// server measures a 400.
func (r *Runner) warm(ctx context.Context, model, kind string) error {
	ctx, cancel := context.WithTimeout(ctx, warmTimeout)
	defer cancel()
	path, body := "/v1/chat/completions", map[string]any{
		"model": model, "max_tokens": 1, "temperature": 0, "stream": false,
		"messages": []map[string]string{{"role": "user", "content": "ok"}},
	}
	if kind == modelprobe.KindEmbed {
		path, body = "/v1/embeddings", map[string]any{"model": model, "input": []string{"ok"}}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(r.opt.LlamaSwapURL, "/")+path, strings.NewReader(string(raw)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// Unauthenticated for the same reason r.catalog is; see there.
	resp, err := r.opt.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		// Scrubbed: this body reaches the operator's terminal AND the
		// journal `status` re-renders, and it is written by a process on
		// the other end of a socket. Same rule as cli.termSafe, held here
		// because the string is stored before any renderer sees it.
		return fmt.Errorf("POST %s: HTTP %d: %s", path, resp.StatusCode, printableSnippet(string(snippet)))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return nil
}

// ── end ─────────────────────────────────────────────────────────────────

// EndReport is what the rollback actually did, step by step. It is
// returned rather than logged because "is the fleet in its prior state"
// is the question the operator is asking, and an answer they cannot see
// is not one.
type EndReport struct {
	Steps      []string
	ConfigPath string
	// RestoredFromBackup is true when the fresh render failed and the
	// banked bytes were put back instead.
	RestoredFromBackup bool
	// Live reports whether the trial id is GONE from the cell's catalog
	// after the rollback. False with a note is the -watch-config case
	// again, and it is the difference between "rolled back" and "rolled
	// back on disk".
	StillInCatalog bool
	Note           string
}

// End undoes the trial in reverse order. It is idempotent and it works
// from any state, including a journal written by a process that died —
// which is the case it exists for.
func (r *Runner) End(ctx context.Context, t *Trial, purge bool) (EndReport, error) {
	rep := EndReport{ConfigPath: t.ConfigPath}
	// A journal written against another cell or another config path
	// cannot be rolled back here: the re-render would target the wrong
	// cell and the banked bytes would land on a file they never came
	// from. Refusing names both values and how to recover.
	if err := r.agreesWithJournal(t); err != nil {
		return rep, err
	}
	var problems []error

	if t.State == StateApplied || t.State == StateMeasured || t.State == StateStaged {
		if err := os.Remove(t.DefPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			problems = append(problems, fmt.Errorf("remove trial def %s: %w", t.DefPath, err))
		} else {
			rep.Steps = append(rep.Steps, "removed the trial def "+t.DefPath)
		}
	}
	// The re-render runs from `staged` too, and only when the config on
	// disk still names the trial. `staged` promises the config was never
	// written by the TRIAL — it does not promise nobody else ran
	// `vibe router render` in between, and after that the config serves a
	// def this function just deleted. Gating on the name is what keeps
	// this from creating or rewriting a config on the ordinary path, where
	// the trial genuinely never touched it.
	if t.State == StateApplied || t.State == StateMeasured ||
		(t.State == StateStaged && r.configNamesTrial(t)) {
		// The trial CREATED this file: the box had none before, so
		// "restore the previous config" means REMOVING it. Re-rendering
		// instead leaves behind a config the operator never had, naming
		// every model on the cell and llama-swap's default upstream range
		// — and on the shape this is reachable on (a llama-swap started
		// with a different -config, which is every deploy that does not
		// use the XDG default) it is a file nothing reads and nobody
		// knows to delete. `bankConfig` recorded the absence for exactly
		// this. Never on the `staged` branch: there the file is somebody
		// ELSE's render, and removing it would take the cell's real
		// config with it.
		if t.ConfigCreated && t.State != StateStaged {
			if err := os.Remove(t.ConfigPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				problems = append(problems, fmt.Errorf("remove the config this trial created (%s): %w", t.ConfigPath, err))
			} else {
				rep.Steps = append(rep.Steps, "removed "+t.ConfigPath+" — this trial created it; the box had no llama-swap config there before")
			}
		} else {
			// PROVE, then write. The re-render loses the same hand-written
			// sections the apply refuses to lose, and here it is worse:
			// `end` is the surface that tells the operator the box is back
			// where it started. When the render would drop one, the
			// byte-exact bank IS the prior state — so fall through to it,
			// which the report already names.
			out, err := r.render(false)
			if err == nil {
				if derr := r.refuseDroppedSections(out.Rendered); derr != nil {
					err = derr
				} else if _, werr := r.render(true); werr != nil {
					err = werr
				}
			}
			switch {
			case err == nil:
				rep.Steps = append(rep.Steps, fmt.Sprintf("re-rendered %s without the trial (changed=%v)", t.ConfigPath, out.Changed))
			case t.BackupPath != "":
				// A render can fail for reasons unrelated to the trial (a def
				// edited by someone else, a broken extras file). The operator
				// still gets their router back.
				data, rerr := os.ReadFile(t.BackupPath)
				if rerr == nil {
					rerr = writeFileAtomic(t.ConfigPath, data, 0o644)
				}
				if rerr != nil {
					problems = append(problems, fmt.Errorf("re-render failed (%w) AND restoring the banked config failed (%w) — %s still contains the trial", err, rerr, t.ConfigPath))
				} else {
					rep.RestoredFromBackup = true
					rep.Steps = append(rep.Steps, fmt.Sprintf("re-render failed (%v); restored the banked config byte-for-byte from %s", err, t.BackupPath))
				}
			default:
				problems = append(problems, fmt.Errorf("re-render failed (%w) and there was no banked config to restore — %s may still contain the trial", err, t.ConfigPath))
			}
		}
		rep.StillInCatalog, rep.Note = r.stillListed(ctx, t.Def)
	}
	if purge {
		for _, p := range []string{t.WeightsPath, t.WeightsPath + ".partial"} {
			if err := os.Remove(p); err == nil {
				rep.Steps = append(rep.Steps, "deleted "+p)
			} else if !errors.Is(err, os.ErrNotExist) {
				problems = append(problems, fmt.Errorf("remove %s: %w", p, err))
			}
		}
	} else if t.State != StatePlanned {
		rep.Steps = append(rep.Steps, "kept the weights at "+t.WeightsPath+" (--purge deletes them)")
	}
	// A FAILED rollback keeps both the banked config and the journal.
	// They are the only two things a retry has: deleting them because
	// the function is returning would turn a recoverable half-rollback
	// into a permanent one, and `status` would then report no trial on a
	// box still serving it.
	if len(problems) > 0 {
		rep.Steps = append(rep.Steps, "KEPT the journal and the banked config — the rollback did not complete, and they are what a retry needs")
		return rep, errors.Join(problems...)
	}
	if t.BackupPath != "" {
		_ = os.Remove(t.BackupPath)
	}
	if err := os.Remove(r.opt.StatePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return rep, fmt.Errorf("remove trial journal: %w", err)
	}
	rep.Steps = append(rep.Steps, "closed the trial journal")
	return rep, nil
}

// refuseDroppedSections refuses a write that would DELETE a top-level
// section the cell's llama-swap config has today.
//
// `router.Render` merges hand-written sections from an extras file, and
// `mergeExtras` maps a missing file to "no extras, no error" — a sane
// default for `vibe router render --extras`, and a silent disarming here.
// A trial rendered with the WRONG extras path (the XDG default, on a cell
// whose render is driven by `--extras <file>`) still succeeds, and quietly
// strips whatever that file contributes:
//
//   - `apiKeys:` — C15's llama-swap credential. The cell stops demanding a
//     key at all for the trial's duration, which is strictly worse than
//     the 401 C15 exists to prevent, and is the exact reasoning C19's
//     renderPass already refuses on for the front.
//   - `store: {path: …}` — the SQLite activity log C7a's usage ledger and
//     C7b's savings screen are computed from. Without it llama-swap keeps
//     a 1000-row in-memory ring and the meter reads ~nothing.
//
// Neither loss is visible in the trial's own output, and `end`'s
// re-render reproduces it, so the rollback does not put them back either.
// A trial adds one `models:` entry; anything it would REMOVE is a config
// edit nobody asked for, and it is refused before the write rather than
// diagnosed afterwards.
func (r *Runner) refuseDroppedSections(rendered string) error {
	current, err := os.ReadFile(r.opt.ConfigPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil // nothing to lose
	}
	if err != nil {
		return fmt.Errorf("read %s before applying: %w", r.opt.ConfigPath, err)
	}
	dropped, err := droppedSections(current, []byte(rendered))
	if err != nil {
		// Unparseable is not "nothing to lose". It is the one state where
		// this check cannot answer, and absent evidence is not a healthy
		// value: refuse and say which file to look at.
		return fmt.Errorf("cannot tell whether applying would drop a hand-written section of %s (%w) — refusing rather than guessing; fix the file, or roll the trial back with `vibe model try end`", r.opt.ConfigPath, err)
	}
	if len(dropped) == 0 {
		return nil
	}
	return fmt.Errorf("applying would DELETE %s from %s — a trial adds one model and must remove nothing. "+
		"Those sections come from a router extras file (`vibe router render --extras <file>`), and this trial rendered with %q. "+
		"Re-run with `--extras <the file your renders use>`; nothing has been written and the trial is still staged",
		strings.Join(dropped, ", "), r.opt.ConfigPath, r.opt.ExtrasPath)
}

// droppedSections lists the top-level YAML keys `current` has and
// `rendered` does not.
func droppedSections(current, rendered []byte) ([]string, error) {
	var cur, next map[string]any
	if err := yaml.Unmarshal(current, &cur); err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(rendered, &next); err != nil {
		return nil, err
	}
	var out []string
	for k := range cur {
		if _, ok := next[k]; !ok {
			out = append(out, k+":")
		}
	}
	sort.Strings(out)
	return out, nil
}

// configNamesTrial reports whether the llama-swap config on disk still
// declares the trial id as a models: key. Read from the file rather than
// re-derived, because the question is about the bytes llama-swap is
// serving, not about what a render would produce now.
func (r *Runner) configNamesTrial(t *Trial) bool {
	data, err := os.ReadFile(r.opt.ConfigPath)
	if err != nil {
		return false
	}
	// A models: key is its own line, at whatever indent yaml.v3 chose.
	// Matching the whole trimmed line rather than a substring keeps the
	// id's other appearances — it is also the --alias inside the rendered
	// cmd block — from answering a question about the catalog.
	key := t.Def + ":"
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == key {
			return true
		}
	}
	return false
}

// stillListed answers whether the cell's llama-swap has really let go of
// the trial id. It POLLS, on the same budget Apply gives adoption, for
// the same reason: `-watch-config` notices a write on its own 2 s cadence
// (C0's measured gate), so a single read taken microseconds after the
// re-render finds the trial still listed on EVERY healthy rollback and
// tells the operator to restart llama-swap. A warning that fires on the
// success path is a warning nobody reads on the failure path.
//
// An unreadable catalog is reported as unknown, never as gone.
func (r *Runner) stillListed(ctx context.Context, id string) (bool, string) {
	// time.Now, not r.opt.Now: this loop sleeps on the wall clock, and a
	// deadline from a frozen injectable clock never expires (awaitCatalog's
	// REV-7 rule, held here too).
	deadline := time.Now().Add(r.opt.CatalogWait)
	for {
		ids, err := r.catalog(ctx)
		listed, known := false, err == nil
		for _, got := range ids {
			if got == id {
				listed = true
				break
			}
		}
		if known && !listed {
			return false, ""
		}
		if !time.Now().Before(deadline) {
			if !known {
				return false, "could not read this cell's /v1/models (" + err.Error() + "), so whether llama-swap has dropped " + id + " is unverified"
			}
			return true, id + " is still in this cell's catalog " + r.opt.CatalogWait.String() +
				" after the re-render: llama-swap has not reloaded — restart it to finish the rollback"
		}
		select {
		case <-ctx.Done():
			return false, "cancelled while waiting for the cell's llama-swap to drop " + id
		case <-time.After(2 * time.Second):
		}
	}
}

// ── derivation ──────────────────────────────────────────────────────────

// deriveDef builds the trial def from the incumbent. The rule is
// deliberately narrow: copy the serving flags, replace the weights, drop
// every field that is a CLAIM ON THE FLEET rather than a description of
// how to serve the model.
//
// The dropped fields are the whole safety of the derivation. Aliases
// would collide with the incumbent's (a render error at best, a silent
// re-point at worst); preload would load the candidate at every
// llama-swap start; the huggingface block would make a later `vibe pull`
// re-fetch under the trial's name; fingerprint: strict would let a
// candidate's hash drift yank models out of the front render.
func deriveDef(inc *profile.BackendDef, name, weights, cell string) *profile.BackendDef {
	ls := *inc.Backend.LlamaServer
	ls.Path = weights
	ls.Huggingface = nil
	// A trial's alias is its own name. Inheriting the incumbent's would
	// make `qwen3.6-27b` resolve to the candidate on this cell — a silent
	// re-point of everything already pointing at the incumbent, which is
	// the exact failure "no silent rerouting" names.
	ls.Alias = name
	// mmproj / draft_model / chat_template_file are paths to files that
	// belong to the incumbent's weights. A candidate is a different model
	// and they do not transfer; carrying them would load another model's
	// projector.
	ls.MMProj, ls.DraftModel, ls.SpecType, ls.SpecDraftNMax = "", "", "", 0
	ls.ChatTemplateFile = ""

	def := &profile.BackendDef{
		Name:    name,
		Backend: profile.Backend{External: true, LlamaServer: &ls},
		// estimated_vram_gb is deliberately NOT inherited. It is a
		// hand-measured claim about the INCUMBENT's weights, and the
		// report puts it in a column headed by the candidate's name —
		// "a model that is 15% faster and 40% larger has not obviously
		// won" is the sentence that column exists for, and copying the
		// number makes the two sides equal by construction, every time.
		// An external def is never VRAM-preflighted (vibe launches
		// nothing for it), so the field costs nothing but the lie.
		Cell:  cell,
		Trial: true,
	}
	if lc := inc.Lifecycle; lc != nil {
		// TTL and start_timeout describe how this BOX behaves; preload
		// and refresh are declarations about the fleet's steady state and
		// a candidate has no business making them.
		def.Lifecycle = &profile.Lifecycle{TTL: lc.TTL, StartTimeout: lc.StartTimeout}
	}
	return def
}

// PreviewDef renders exactly the bytes Stage would write. --dry-run
// exists to be READ, and a preview that reconstructed the file its own
// way would be a second renderer of the one artifact whose contents this
// phase asks an operator to review before promoting.
func PreviewDef(t *Trial, def *profile.BackendDef) (string, error) {
	body, err := yaml.Marshal(def)
	if err != nil {
		return "", err
	}
	return trialDefHeader(t) + string(body), nil
}

func trialDefHeader(t *Trial) string {
	return fmt.Sprintf(`# scaffolded by vibe model try (fleet-control C18) — a CANDIDATE, not fleet config.
#
# trial: true keeps this def out of the front render, so the id below is
# reachable only on this cell. To promote it: delete the trial: line,
# review every flag (they were copied from %s and describe THAT model),
# then commit the def and re-render.
#
# To roll it back: vibe model try end
# source: https://huggingface.co/%s (%s)
`, t.Incumbent, t.Repo, t.File)
}

// derivedName turns an HF coordinate into a def name. The `trial-`
// prefix is not decoration: it is what an operator scanning a backends
// dir at 2 a.m. reads before they read anything else.
func derivedName(repo, file string) string {
	base := repo
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		base = repo[i+1:]
	}
	base = strings.TrimSuffix(base, "-GGUF")
	base = strings.TrimSuffix(base, "-gguf")
	if base == "" {
		base = strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
	}
	return "trial-" + sanitize(base)
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-':
			b.WriteRune(r)
		case r == '_' || r == ' ':
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-.")
}

// validDefName keeps the name usable as a bare file stem and as a model
// id on the wire. LoadBackendFrom enforces the first; the second is why
// control characters and spaces are refused here rather than discovered
// as a mangled llama-swap key.
func validDefName(name string) error {
	if name == "" {
		return errors.New("the trial def name is empty (pass --as <name>)")
	}
	if name != sanitize(name) {
		return fmt.Errorf("trial def name %q must be lowercase letters, digits, '.' and '-' (it is a filename and a model id); pass --as", name)
	}
	if name != filepath.Base(name) || name == "." || name == ".." {
		return fmt.Errorf("trial def name %q must be a bare name", name)
	}
	return nil
}

func revisionSuffix(rev string) string {
	if rev == "" {
		return ""
	}
	return " @ " + rev
}

func findDef(defs []*profile.BackendDef, name string) *profile.BackendDef {
	for _, d := range defs {
		if d != nil && d.Name == name {
			return d
		}
	}
	return nil
}

// defServingPath reports the def, if any, whose weights are the given
// path. Downloading over a served model's file is the one download
// failure that takes a working model with it.
func defServingPath(defs []*profile.BackendDef, path string) string {
	for _, d := range defs {
		if d == nil || d.Backend.LlamaServer == nil {
			continue
		}
		if d.Backend.LlamaServer.Path == path {
			return d.Name
		}
	}
	return ""
}

// checkPrintable refuses control characters in the strings that leave
// this process as something other than an argv word. The coordinate is
// interpolated verbatim into the scaffolded def's HEADER — a comment
// block, where a newline ends the comment and the rest becomes YAML — and
// into the journal, which `vibe model try status` re-prints to a terminal
// on every later invocation. `validateAwaitFlags` already holds this rule
// for the fields it forwards to fleetd; C18 held it only for the def NAME,
// which is the one string it derives rather than carries.
func checkPrintable(req PlanRequest) error {
	for _, f := range []struct{ flag, value string }{
		{"<hf-repo>", req.Repo}, {"--file", req.File}, {"--revision", req.Revision},
		{"--as", req.As}, {"--dest", req.Dest},
	} {
		if f.value != printableSnippet(f.value) {
			return fmt.Errorf("%s contains a control character or leading/trailing space: it is written into the trial def's header, saved to the journal and re-printed by every later `vibe model try status`", f.flag)
		}
	}
	return nil
}

// printableSnippet strips control characters from text this package did
// not write. Everything llama-swap says ends up in a `Note` that is saved
// to the journal and printed to a tty; an escape sequence there is an
// injection into the operator's terminal, replayed by every later
// `vibe model try status`.
func printableSnippet(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return ' '
		case !unicode.IsPrint(r):
			return -1
		}
		return r
	}, s))
}

func fileSize(path string) int64 {
	if info, err := os.Stat(path); err == nil {
		return info.Size()
	}
	return 0
}

func humanBytes(n int64) string {
	switch {
	case n < 0:
		return "unknown"
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".trial-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
