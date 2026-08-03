package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
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

	llamaSwapURL := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(d.cfg.ProxyPort))

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

// fleetVersions fills the announce versions block: the vibe build and
// the def checkout's git state — the "cell is N commits behind" context
// a fingerprint mismatch report needs.
func (d *Daemon) fleetVersions() *fleetapi.AnnounceVersions {
	v := &fleetapi.AnnounceVersions{Vibe: buildinfo.String()}
	dir := paths.BackendsDir()
	if sha, err := gitOut(dir, "rev-parse", "--short", "HEAD"); err == nil {
		v.DefsSHA = sha
		if dirty, err := gitOut(dir, "status", "--porcelain"); err == nil && dirty != "" {
			v.DefsDirty = true
		}
	}
	return v
}

// fleetCapacity fills the announce capacity block: VRAM total/free from
// the daemon's own probes, disk free for the state dir.
func (d *Daemon) fleetCapacity() *fleetapi.AnnounceCapacity {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cap := &fleetapi.AnnounceCapacity{}
	if free, err := d.nvidiaSMI(ctx); err == nil {
		cap.VRAMFreeGB = free
	}
	if total, err := d.vramCapacity(ctx); err == nil {
		cap.VRAMTotalGB = total
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(filepath.Dir(paths.StartHistoryFile()), &st); err == nil {
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
