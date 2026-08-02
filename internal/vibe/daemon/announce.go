package daemon

import (
	"context"
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
)

// The C3 announce wiring: a cell daemon announces to fleetd from Run
// when fleet.cell + fleet.registry_url are configured. Slim cells (no
// daemon) run the same client via `vibe fleet announce` — one code path.

// startAnnounce builds and launches the announce client. Defs are
// filtered to this cell (assigned here or unassigned — the same rule as
// a local render) so fingerprints cover exactly what this box serves.
func (d *Daemon) startAnnounce(ctx context.Context) error {
	defs, err := router.LoadDefs(paths.BackendsDir())
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("load backend defs: %w", err)
	}
	var cellDefs []*profile.BackendDef
	for _, def := range defs {
		if def.Cell == "" || def.Cell == d.cfg.Fleet.Cell {
			cellDefs = append(cellDefs, def)
		}
	}

	ann, err := fleetannounce.New(fleetannounce.Config{
		Cell:              d.cfg.Fleet.Cell,
		RegistryURL:       d.cfg.Fleet.RegistryURL,
		TokenFile:         d.cfg.Fleet.TokenFile,
		LlamaSwapURL:      "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(d.cfg.ProxyPort)),
		Defs:              cellDefs,
		LlamaServerBinary: d.cfg.LlamaBinary,
		IntentPath:        paths.CellIntentFile(),
		CellCmds: fleetannounce.CellCmds{
			Drain:  d.cfg.CellCmds.Drain,
			Resume: d.cfg.CellCmds.Resume,
		},
		Versions: d.fleetVersions,
		Capacity: d.fleetCapacity,
	})
	if err != nil {
		return err
	}
	d.announce = ann
	go func() { _ = ann.Run(ctx) }()
	return nil
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
