package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gallowaysoftware/vibe/internal/buildinfo"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetannounce"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/modelprobe"
	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
	"github.com/gallowaysoftware/vibe/internal/vibe/router"
	"github.com/gallowaysoftware/vibe/internal/vibe/usagemeter"
)

// The C3 announce wiring: a cell daemon announces to fleetd from Run
// when fleet.cell + fleet.registry_url are configured. Slim cells (no
// daemon) run the same client via `vibe fleet announce` — one code path.

// startAnnounce builds and launches the announce client. Defs are
// filtered to this cell (assigned here or unassigned — the same rule as
// a local render) so fingerprints cover exactly what this box serves.
func (d *Daemon) startAnnounce(ctx context.Context) error {
	defs, err := router.LoadDefs(paths.BackendsDir())
	// errors.Is, not os.IsNotExist: LoadDefs WRAPS the ReadDir error and
	// os.IsNotExist predates unwrapping, so a box with no backends dir
	// (defless catalog, or defs not yet converged) failed to start its
	// announce loop at all and went invisible to fleetd.
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("load backend defs: %w", err)
	}
	var cellDefs []*profile.BackendDef
	for _, def := range defs {
		if def.Cell == "" || def.Cell == d.cfg.Fleet.Cell {
			cellDefs = append(cellDefs, def)
		}
	}

	llamaSwapURL := d.localLlamaSwapURL()
	probes, runProbe := modelprobe.Hooks(llamaSwapURL, paths.CellProbeFile(), cellDefs, d.cfg.LlamaBinary)

	ann, err := fleetannounce.New(fleetannounce.Config{
		Cell:              d.cfg.Fleet.Cell,
		RegistryURL:       d.cfg.Fleet.RegistryURL,
		TokenFile:         d.cfg.Fleet.TokenFile,
		LlamaSwapURL:      llamaSwapURL,
		Defs:              cellDefs,
		LlamaServerBinary: d.cfg.LlamaBinary,
		IntentPath:        paths.CellIntentFile(),
		CellCmds: fleetannounce.CellCmds{
			Drain:  d.cfg.CellCmds.Drain,
			Resume: d.cfg.CellCmds.Resume,
		},
		Versions: d.fleetVersions,
		Capacity: d.fleetCapacity,
		Usage:    usagemeter.Snapshotter(llamaSwapURL, paths.CellUsageFile()),
		Probes:   probes,
		RunProbe: runProbe,
	})
	if err != nil {
		return err
	}
	// The loop gets its own cancel and a done channel: shutdown must be
	// able to STOP it before withdrawing. Deriving from ctx alone was not
	// enough — the shutdown-RPC path never cancels ctx, so the loop kept
	// heartbeating (and leaked) past Run's return, and a heartbeat still
	// in flight lands after the goodbye and resurrects the cell.
	annCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	d.announce = ann
	d.announceCancel = cancel
	d.announceDone = done
	go func() {
		defer close(done)
		_ = ann.Run(annCtx)
	}()
	return nil
}

// announceStopTimeout bounds the shutdown wait for the announce loop. It
// exits on ctx cancellation, so this only covers a request already in
// flight — an unreachable registry must not hold shutdown open.
const announceStopTimeout = 3 * time.Second

// withdrawAnnounce stops the announce loop and sends the goodbye
// heartbeat, in that order: a heartbeat racing the withdraw would land
// after it and re-announce the cell as serving. Best-effort throughout —
// fleetd's staleness is the fallback for every failure here.
func (d *Daemon) withdrawAnnounce() {
	if d.announce == nil {
		return
	}
	if d.announceCancel != nil {
		d.announceCancel()
	}
	if d.announceDone != nil {
		select {
		case <-d.announceDone:
		case <-time.After(announceStopTimeout):
			slog.Info("announce loop still running at shutdown; withdrawing anyway")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.announce.Withdraw(ctx); err != nil {
		slog.Info("withdraw announce failed; fleetd will prune on staleness", "err", err)
	}
}

// fleetVersions fills the announce versions block: the vibe build, the
// def checkout's git state — the "cell is N commits behind" context a
// fingerprint mismatch report needs — and the cell's own llama-swap
// version.
func (d *Daemon) fleetVersions() *fleetapi.AnnounceVersions {
	return fleetVersionsAt(paths.BackendsDir(), d.localLlamaSwapURL())
}

// FleetVersions is the same block for announcers that are not a daemon
// (`vibe fleet announce`, the slim cell form). Exported because C13's
// def-SHA parity check is worthless when half the fleet reports no
// versions at all, and before it the slim announcer passed no provider —
// the heavy cell, whose def checkout is the one most likely to drift,
// was the one cell that never said which checkout it had.
//
// llamaSwapURL may be empty; the version is then simply not reported,
// which doctor renders as UNKNOWN rather than as agreement.
func FleetVersions(backendsDir, llamaSwapURL string) *fleetapi.AnnounceVersions {
	return fleetVersionsAt(backendsDir, llamaSwapURL)
}

func fleetVersionsAt(backendsDir, llamaSwapURL string) *fleetapi.AnnounceVersions {
	v := &fleetapi.AnnounceVersions{Vibe: buildinfo.String()}
	if sha, err := gitOut(backendsDir, "rev-parse", "--short", "HEAD"); err == nil {
		v.DefsSHA = sha
		if dirty, err := gitOut(backendsDir, "status", "--porcelain"); err == nil && dirty != "" {
			v.DefsDirty = true
		}
	}
	v.LlamaSwap = llamaSwapVersion(llamaSwapURL)
	return v
}

// llamaSwapVersionTimeout bounds the local read. The heartbeat is the
// cell's only evidence of life (C8's rule about the prober), so a
// llama-swap that has wedged must cost the announce milliseconds and a
// blank field, never the announce itself.
const llamaSwapVersionTimeout = 2 * time.Second

// swapVersionClient is its own client rather than http.DefaultClient: the
// timeout belongs to this read, and a pooled connection to a llama-swap
// that has wedged has no business being shared with the rest of the
// daemon.
var swapVersionClient = &http.Client{Timeout: llamaSwapVersionTimeout}

// llamaSwapVersion reads the local llama-swap's own version for the
// announce block.
//
// GET /api/version answers {"version":"v239","commit":…,"build_date":…} and
// was verified against real v239 and v247 binaries before this producer
// was written — C13 reported the field UNKNOWN naming the missing
// producer precisely because guessing an endpoint from a box that cannot
// check it is worse than an honest gap.
//
// The read itself is fleetapi's, shared with the front-side reader, so the
// two cannot drift on what they accept. Every failure returns "", which
// doctor renders as "no cell reports a version", never as agreement.
//
// The UNAUTHENTICATED half of that shared reader, deliberately: this is a
// cell reading its own 127.0.0.1 llama-swap, and C15 §8 scopes the
// cell-side dialers out of the credential (different config surface — a
// slim announcer's box may hold no hosts.yaml). If this cell keys its own
// llama-swap, `/api/version` 401s and the announce carries no version;
// the fix is C15's recorded futures item, not a credential invented here.
// fleetapi.ReadOwnSwapVersion's doc comment is the long form.
func llamaSwapVersion(baseURL string) string {
	if baseURL == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), llamaSwapVersionTimeout)
	defer cancel()
	return fleetapi.ReadOwnSwapVersion(ctx, swapVersionClient, baseURL)
}

// localLlamaSwapURL is the daemon's own router base. Same construction as
// startAnnounce's, which is where the announce loop's copy comes from.
func (d *Daemon) localLlamaSwapURL() string {
	return "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(d.cfg.ProxyPort))
}

// fleetCapacity fills the announce capacity block: VRAM total/free from
// the daemon's own probes, disk free for the state dir.
func (d *Daemon) fleetCapacity() *fleetapi.AnnounceCapacity {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cap := FleetDiskCapacity(filepath.Dir(paths.StartHistoryFile()))
	if free, err := d.nvidiaSMI(ctx); err == nil {
		cap.VRAMFreeGB = free
	}
	if total, err := d.vramCapacity(ctx); err == nil {
		cap.VRAMTotalGB = total
	}
	return cap
}

// FleetDiskCapacity is the capacity block a slim announcer can fill: the
// disk half only, because a cell without a daemon has no VRAM probe
// wired. An unreadable filesystem leaves the field at zero, which C13's
// disk check reads as "not reported" rather than as "full".
func FleetDiskCapacity(dir string) *fleetapi.AnnounceCapacity {
	cap := &fleetapi.AnnounceCapacity{}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err == nil {
		cap.DiskFreeGB = float64(st.Bavail) * float64(st.Bsize) / (1 << 30)
	}
	return cap
}

func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// noteLocalDrain stamps the cell's local intent after a successful
// locally-invoked verb — the human-at-the-box side of the C3 conflict
// rule. Best-effort: the announce loop simply isn't running on some
// deployments (no registry configured).
func (d *Daemon) noteLocalIntent(state string) {
	if d.announce != nil {
		if err := d.announce.SetLocalIntent(state); err != nil {
			slog.Warn("local intent not persisted (echo loses the next conflict comparison)", "err", err)
		}
	}
}
