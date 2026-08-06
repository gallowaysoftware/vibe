// The presence-derived front render loop (fleet-control C3 §4): the
// front's peers config stops being a hand-run render and becomes a
// derived artifact of the presence table. Membership transitions
// (withdraw/stale/return) land on renderTrigger; the loop coalesces
// them into re-renders through the same router.Render code path the
// CLI uses (import, not shell-out), applying the design-doc §4 class
// policy: roaming cells prune fast and re-add slow, always_on and
// opportunistic cells hold — their ids never 404, requests get typed
// UPSTREAM_DOWN instead.

package fleetapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
	"github.com/gallowaysoftware/vibe/internal/vibe/router"
)

// Default hysteresis/rate values (fleet-control C3 §4: prune fast,
// re-add slow, cap renders at ~1/min).
const (
	defaultMinHealthyStreak  = 3
	defaultRenderMinInterval = time.Minute
	// defaultFullWaveTimeout matches the staleness bound at the default
	// cadence: by ~50s every live cell has either announced or missed
	// its window, so holding the previous config longer helps nobody.
	defaultFullWaveTimeout = 50 * time.Second
)

// RenderLoopConfig wires the presence-derived render. BackendsDir is
// the canonical def checkout; FrontConfigPath is the front llama-swap's
// -watch-config file (writes are atomic tmp+rename inside its
// directory, so the watcher never reads a half-written config).
type RenderLoopConfig struct {
	BackendsDir       string
	LlamaServerBinary string
	FrontConfigPath   string
	// FrontExtras is the operator-owned half of the front's config: a
	// YAML file whose top-level sections merge into every render (the
	// `--extras` merge). Without it the render emits only what it
	// derives, so the front's `apiKeys:` — the credential C15 exists to
	// present — is deleted on the first membership transition, and the
	// fleet's warms start 401ing hours after someone configured them.
	FrontExtras string
	Hosts       *fleetcfg.File
	// MinHealthyStreak is the re-add hysteresis: a pruned roaming cell's
	// defs return only after this many consecutive fresh announces.
	MinHealthyStreak int
	// RenderMinInterval caps render frequency; transitions inside the
	// window coalesce into one trailing render.
	RenderMinInterval time.Duration
	// FullWaveTimeout is the cold-start hold: the loop renders nothing
	// until every registered cell has announced or this elapses —
	// fleetd rebooting must never write an empty-peers config over the
	// last known-good one.
	FullWaveTimeout time.Duration

	// Test seams; nil selects the production default (router.LoadDefs,
	// router.Render, atomic tmp+rename).
	LoadDefs  func(dir string) ([]*profile.BackendDef, error)
	Render    func(defs []*profile.BackendDef, opts router.Options) (string, error)
	WriteFile func(path string, data []byte) error
}

// RenderCount returns how many front-config writes this server's
// render loop has performed (renders that produced unchanged content
// don't count — they never touched the watched file). Atomic, never
// taken under s.mu, so fleet_status gets the flap-storm signal without
// a second claim on the hub mutex.
func (s *Server) RenderCount() int {
	return int(s.renderWrites.Load())
}

// StartRenderLoop launches the presence-derived render loop. It
// returns immediately; the loop exits when the server closes.
func (s *Server) StartRenderLoop(cfg RenderLoopConfig) {
	if cfg.MinHealthyStreak <= 0 {
		cfg.MinHealthyStreak = defaultMinHealthyStreak
	}
	if cfg.RenderMinInterval <= 0 {
		cfg.RenderMinInterval = defaultRenderMinInterval
	}
	if cfg.FullWaveTimeout <= 0 {
		cfg.FullWaveTimeout = defaultFullWaveTimeout
	}
	if cfg.LoadDefs == nil {
		cfg.LoadDefs = router.LoadDefs
	}
	if cfg.Render == nil {
		cfg.Render = router.Render
	}
	if cfg.WriteFile == nil {
		cfg.WriteFile = writeAtomic
	}
	// The fingerprint alarm (C9) is only as trustworthy as the pass that
	// evaluates it, so the status names whether that pass exists at all.
	s.mu.Lock()
	s.renderLoopOn = true
	s.mu.Unlock()
	s.wg.Add(1)
	go (&renderLoop{srv: s, cfg: cfg, pruned: map[string]bool{}}).run()
}

// renderLoop carries the mutable hysteresis state: the pruned set is
// loop-owned (not derived per pass) because re-add must be slower than
// prune — a cell pruned at prune-time stays pruned until its healthy
// streak clears, even if a single fresh announce lands first.
type renderLoop struct {
	srv    *Server
	cfg    RenderLoopConfig
	pruned map[string]bool
}

func (rl *renderLoop) run() {
	defer rl.srv.wg.Done()

	// Cold start: an empty presence table is "fleetd rebooted", not
	// "the fleet withdrew" — the on-disk config stays truth until the
	// full announce wave lands or the hold times out.
	holding := !rl.fullWave()
	holdTimer := time.NewTimer(rl.cfg.FullWaveTimeout)
	defer holdTimer.Stop()

	// The cap is a trailing-edge throttle: a trigger renders
	// immediately when no render happened inside the last
	// RenderMinInterval; triggers inside the window coalesce into one
	// trailing render when it expires. A flap storm churns the catalog
	// at the cap, never faster.
	var (
		pending   bool
		cooldown  *time.Timer
		cooldownC <-chan time.Time
	)
	defer func() {
		if cooldown != nil {
			cooldown.Stop()
		}
	}()

	tryRender := func() {
		if cooldownC != nil {
			return
		}
		if err := rl.renderPass(); err != nil {
			slog.Warn("presence-derived render failed", "err", err)
		}
		pending = false
		if cooldown == nil {
			cooldown = time.NewTimer(rl.cfg.RenderMinInterval)
		} else {
			cooldown.Reset(rl.cfg.RenderMinInterval)
		}
		cooldownC = cooldown.C
	}

	for {
		select {
		case <-rl.srv.done:
			return
		case <-holdTimer.C:
			holding = false
			if pending {
				tryRender()
			}
		case <-rl.srv.renderTrigger:
			if holding && !rl.fullWave() {
				// Partial wave: remember that a render is owed; only
				// the full wave (or the timeout) ends the hold.
				pending = true
				continue
			}
			holding = false
			pending = true
			tryRender()
		case <-cooldownC:
			cooldownC = nil
			if pending {
				tryRender()
			}
		}
	}
}

// fullWave reports whether every registered cell has announced at
// least once — the signal that the presence table is whole again after
// a fleetd reboot.
func (rl *renderLoop) fullWave() bool {
	pres := rl.srv.presenceSnapshot()
	for _, c := range rl.srv.cells {
		if !pres[c.Name].Announcing {
			return false
		}
	}
	return true
}

// renderPass runs one full derive-render-write cycle. The fingerprint
// check rides every pass (not per trigger): drift is a property of the
// defs-vs-cell pair, so any render must re-verify it.
func (rl *renderLoop) renderPass() error {
	defs, err := rl.cfg.LoadDefs(rl.cfg.BackendsDir)
	if err != nil {
		return fmt.Errorf("load defs: %w", err)
	}
	// LoadDefs returns (nil, nil) for an empty dir, Render then succeeds
	// with a header-only config, and the write below replaces a good
	// front config with it. The shipped deploy makes this reachable: the
	// fleetd README tells operators to mkdir the defs mount. Gate on the
	// INPUT (zero defs loaded), never on the output — a peerless render
	// is legitimate when every def is unassigned or every roaming cell is
	// pruned, and gating there would deadlock the empty-fleet case.
	if len(defs) == 0 {
		if fi, statErr := os.Stat(rl.cfg.FrontConfigPath); statErr == nil && fi.Size() > 0 {
			return fmt.Errorf("refusing to render an empty front config: no backend defs under %s (fix the defs mount)", rl.cfg.BackendsDir)
		}
	}
	// The same input-side gate for the OTHER half of the front's config.
	// router.mergeExtras maps ErrNotExist to "no extras, no error" — a
	// sane default for `vibe router render --extras`, and a silent
	// disarming here: a declared front_extras whose path is wrong (or
	// whose mount is missing inside fleetd's container) renders a config
	// with no `apiKeys:` over one that had them. The front then stops
	// demanding a key at all, which is a worse outcome than the 401 this
	// phase exists to fix. Declared-and-unreadable is a config error, so
	// refuse and keep the last good file — but only when there IS a file
	// to protect, exactly as the zero-defs gate above does, or a
	// first-boot fleet could never render.
	if rl.cfg.FrontExtras != "" {
		if _, statErr := os.Stat(rl.cfg.FrontExtras); statErr != nil {
			if fi, cfgErr := os.Stat(rl.cfg.FrontConfigPath); cfgErr == nil && fi.Size() > 0 {
				return fmt.Errorf("refusing to render over %s: fleet.front_extras %s cannot be read (%v), so the render would drop every hand-written section (apiKeys:, store:) from the front's config",
					rl.cfg.FrontConfigPath, rl.cfg.FrontExtras, statErr)
			}
		}
	}
	pres := rl.srv.presenceSnapshot()

	defs = rl.applyClassPolicy(defs, pres)
	defs = rl.applyFingerprints(defs, pres)

	opts := router.Options{
		Cell:              fleetcfg.FrontCell,
		Hosts:             rl.cfg.Hosts,
		LlamaServerBinary: rl.cfg.LlamaServerBinary,
		ExtrasPath:        rl.cfg.FrontExtras,
		Warnf: func(format string, args ...any) {
			slog.Warn(fmt.Sprintf("front render: "+format, args...))
		},
	}
	content, err := rl.cfg.Render(defs, opts)
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}

	current, err := os.ReadFile(rl.cfg.FrontConfigPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read current config: %w", err)
	}
	// Unchanged content means no write: llama-swap's -watch-config
	// reloads on every touch, and a churned catalog invalidates
	// consumers' cached model lists for nothing.
	if string(current) == content {
		return nil
	}
	if err := rl.cfg.WriteFile(rl.cfg.FrontConfigPath, []byte(content)); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	slog.Info("front config re-rendered from presence", "path", rl.cfg.FrontConfigPath, "renders", rl.srv.renderWrites.Add(1))
	return nil
}

// applyClassPolicy prunes roaming cells' defs per presence (design doc
// §4). Hold is the default: always_on/opportunistic defs stay rendered
// no matter what presence says, and a roaming cell that has NEVER
// announced keeps its defs too — presence-derived rendering only
// applies once presence exists (probe-era behavior otherwise).
func (rl *renderLoop) applyClassPolicy(defs []*profile.BackendDef, pres map[string]Presence) []*profile.BackendDef {
	for name, p := range pres {
		if rl.classOf(name) != fleetcfg.ClassRoaming || !p.Announcing {
			continue
		}
		switch {
		case p.Stale || p.Withdrawn:
			if !rl.pruned[name] {
				slog.Info("pruning roaming cell from front render", "cell", name, "stale", p.Stale, "withdrawn", p.Withdrawn)
			}
			rl.pruned[name] = true
		case rl.pruned[name] && p.HealthyStreak >= rl.cfg.MinHealthyStreak:
			slog.Info("roaming cell re-added to front render", "cell", name, "healthy_streak", p.HealthyStreak)
			delete(rl.pruned, name)
		}
	}
	if len(rl.pruned) == 0 {
		return defs
	}
	kept := make([]*profile.BackendDef, 0, len(defs))
	for _, d := range defs {
		if d.Cell != "" && rl.pruned[d.Cell] {
			continue
		}
		kept = append(kept, d)
	}
	return kept
}

// applyFingerprints verifies every announced flags_sha256 against
// fleetd's own render of the same def. A mismatch ALWAYS raises the
// loud event (with the cell's defs provenance, so "N commits behind"
// replaces guesswork); strict-mode defs additionally leave the render
// — embed-class drift is silent retrieval damage, while advisory
// (default) stays fail-open so a hash-normalization bug can never yank
// a working chat model.
func (rl *renderLoop) applyFingerprints(defs []*profile.BackendDef, pres map[string]Presence) []*profile.BackendDef {
	byName := make(map[string]*profile.BackendDef, len(defs))
	for _, d := range defs {
		byName[d.Name] = d
	}
	excluded := map[string]bool{}
	// mismatched accumulates what THIS pass found. The set is rebuilt from
	// it (C9): the mismatch event fires once and then goes silent, so
	// "persistent" can only be measured against state, and this pass is
	// the only thing that evaluates it.
	var mismatched []FingerprintMismatch
	for _, p := range pres {
		if !p.Announcing {
			continue
		}
		for _, m := range p.Models {
			if m.FlagsSHA256 == "" {
				continue
			}
			def := byName[m.ID]
			if def == nil {
				continue
			}
			// Mirror the announcer's own guard (fleetannounce: fingerprints
			// cover spec-rendered kinds only). The SENDING side has it; the
			// RECEIVING side, which by construction must not trust the
			// sender, needs it more — a comfyui/http_server/tabby_api/
			// cloud_peer def named by an announce carrying a hash would
			// otherwise reach ModelCmd's llama_server deref.
			if def.Backend.LlamaServer == nil && def.Backend.MLXServer == nil {
				continue
			}
			// Enforcement is bound to the def's home cell: a mismatch is
			// only meaningful from the cell that owns the def (its flags
			// are what's actually serving). Any other cell's announce
			// carrying this id is ignored — an announcing cell can never
			// yank another cell's def from the render. Unassigned defs
			// skip enforcement: every cell announces them, and the
			// front's own checkout is their render truth (defs_sha in
			// the announce reports checkout drift instead).
			if def.Cell == "" || def.Cell != p.Cell {
				continue
			}
			// A verification error skips this model, never the pass: an
			// aborted pass leaves p.Announcing uncleared, so one bad def
			// would freeze prune, re-add and enforcement fleet-wide forever.
			cmd, err := router.ModelCmd(def, router.Options{Hosts: rl.cfg.Hosts, LlamaServerBinary: rl.cfg.LlamaServerBinary})
			if err != nil {
				slog.Warn("fingerprint check skipped: cannot render def", "model", m.ID, "cell", p.Cell, "err", err)
				continue
			}
			expected, err := router.FlagsSHA256(cmd)
			if err != nil {
				slog.Warn("fingerprint check skipped: cannot hash flags", "model", m.ID, "cell", p.Cell, "err", err)
				continue
			}
			if strings.EqualFold(expected, m.FlagsSHA256) {
				continue
			}
			mode := def.Fingerprint
			if mode == "" {
				mode = "advisory"
			}
			var defsSHA string
			var defsDirty bool
			if p.Versions != nil {
				defsSHA = p.Versions.DefsSHA
				defsDirty = p.Versions.DefsDirty
			}
			data, err := json.Marshal(map[string]any{
				"model":      m.ID,
				"cell":       p.Cell,
				"expected":   expected,
				"got":        m.FlagsSHA256,
				"mode":       mode,
				"defs_sha":   defsSHA,
				"defs_dirty": defsDirty,
			})
			if err != nil {
				slog.Warn("fingerprint event not published", "model", m.ID, "cell", p.Cell, "err", err)
				data = nil
			}
			slog.Warn("flags fingerprint mismatch", "model", m.ID, "cell", p.Cell, "mode", mode, "defs_sha", defsSHA, "defs_dirty", defsDirty)
			rl.srv.publish(Event{Cell: p.Cell, Type: EventFingerprint, Data: data})
			// The C9 alarm SET carries FRESH announces only. Announcing
			// stays true through staleness and a clean withdraw, so a cell
			// that has been powered off keeps its last-announced model list
			// — and would keep a mismatch in the set forever, paging about
			// drift on a box that is serving nothing (and, on a roaming
			// laptop, every night). C8's probe.degraded roll-up already
			// draws this line: a stale announce is history, not evidence of
			// what the cell is serving right now. The EVENT above and the
			// strict exclusion below are C3's and keep their own semantics.
			if !p.Stale && !p.Withdrawn {
				mismatched = append(mismatched, FingerprintMismatch{
					Cell: p.Cell, Model: m.ID, Expected: expected, Got: m.FlagsSHA256, Mode: mode,
				})
			}
			if mode == "strict" {
				excluded[def.Name] = true
			}
		}
	}
	rl.srv.setFingerprintMismatches(mismatched, time.Now().UTC())
	if len(excluded) == 0 {
		return defs
	}
	kept := make([]*profile.BackendDef, 0, len(defs))
	for _, d := range defs {
		if excluded[d.Name] {
			slog.Warn("excluding strict-fingerprint def from front render", "model", d.Name, "cell", d.Cell)
			continue
		}
		kept = append(kept, d)
	}
	return kept
}

// classOf resolves a cell's absence-semantics class from hosts.yaml,
// falling back to the registry slice (the two are built from the same
// file, but the config value wins: it is the fleet's membership truth).
func (rl *renderLoop) classOf(cell string) fleetcfg.Class {
	if rl.cfg.Hosts != nil {
		if c, ok := rl.cfg.Hosts.Cells[cell]; ok {
			return c.Class
		}
	}
	for _, c := range rl.srv.cells {
		if c.Name == cell {
			return fleetcfg.Class(c.Class)
		}
	}
	return ""
}

// writeAtomic replaces path via a tmp file in the SAME directory
// (rename across filesystems fails, and llama-swap's watcher must only
// ever see complete configs). The mode is set BEFORE the rename: a
// chmod afterwards leaves a window where the watcher reads a file it
// cannot open, which is the failure the atomic swap exists to prevent.
func writeAtomic(path string, data []byte) error {
	// The front config exists to be READ by another process (llama-swap,
	// often another user), so CreateTemp's 0600 is wrong here. An
	// existing file's mode wins — an operator who widened it meant it —
	// but read bits are forced back on: every fleetd deployed before this
	// fix left a 0600 file behind, and inheriting THAT mode would keep
	// the bug alive forever on exactly the boxes that have it.
	mode := os.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm() | 0o044
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".render-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
