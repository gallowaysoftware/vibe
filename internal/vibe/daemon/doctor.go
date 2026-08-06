package daemon

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"

	"github.com/gallowaysoftware/vibe/internal/buildinfo"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetmirror"
	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
	"github.com/gallowaysoftware/vibe/internal/vibeclient"
)

// The daemon half of `vibe fleet doctor` (fleet-control C13): the two
// things fleetapi cannot do for itself. Everything here is READ-ONLY —
// a stat, a statfs, an environment lookup, and one Status RPC, which is
// the lightest authenticated read the control plane has.

// doctorHost reports the fleetd-host facts the report needs. The token
// mint flag comes from the value recorded at startup, not from a re-read:
// re-reading answers "does a token file exist now", which is not the
// question (C1's created-vs-loaded distinction is about THIS start).
func (d *Daemon) doctorHost() fleetapi.DoctorHost {
	h := fleetapi.DoctorHost{
		StateDir:      filepath.Dir(paths.IntentFile()),
		FrontConfig:   d.cfg.Fleet.FrontConfig,
		FrontExtras:   d.cfg.Fleet.FrontExtras,
		FrontImage:    d.cfg.Fleet.FrontImage,
		MirrorMaxAge:  d.cfg.Fleet.MirrorMaxAge,
		Mirror:        mirrorFacts(),
		TokenMinted:   d.tokenMinted.Load(),
		EnvTokenSet:   strings.TrimSpace(os.Getenv("VIBE_TOKEN")) != "",
		Version:       buildinfo.String(),
		DiskFreeBytes: diskFreeBytes,
	}
	// The same git read the announce versions block uses, so fleetd's own
	// parity reference and a cell's are the same measurement.
	if v := fleetVersionsAt(paths.BackendsDir(), ""); v != nil {
		h.DefsSHA, h.DefsDirty = v.DefsSHA, v.DefsDirty
	}
	return h
}

// mirrorFacts reads the receipt `vibe fleet mirror` leaves in this
// daemon's own state dir (fleet-control C19). A READ, on the doctor
// path: nothing here writes, and a missing receipt is nil rather than a
// zero-valued run — beside a declared fleet.mirror_max_age the
// difference is a FAIL versus an OK.
func mirrorFacts() *fleetapi.MirrorFacts {
	rc, err := fleetmirror.ReadReceipt(paths.StateHome())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return &fleetapi.MirrorFacts{ReadErr: err.Error()}
	}
	return &fleetapi.MirrorFacts{
		At: rc.At, Archive: rc.Archive, Dest: rc.Dest, Files: rc.Files, Bytes: rc.Bytes,
		Missing: rc.Missing, Errors: rc.Errors, Warnings: rc.Warnings, Credentials: rc.Credentials,
	}
}

// diskFreeBytes reports free bytes on the filesystem holding path.
// Statfs's Bavail is the unprivileged-writer figure, which is the one
// that matters to a daemon that does not run as root everywhere.
func diskFreeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	// Bsize is int64 on linux and int32 on darwin; the conversion is
	// explicit rather than assumed.
	return uint64(st.Bavail) * uint64(st.Bsize), nil //nolint:gosec,unconvert // both fields are non-negative sizes
}

// doctorAuthTimeout bounds one outbound credential probe. Short on
// purpose: doctor asks every cell, and a box that is off must cost the
// report seconds, not minutes.
const doctorAuthTimeout = 5 * time.Second

// cellAuthProbe answers "would fleetd's own verbs authenticate to this
// cell right now" with the ONLY honest method: trying, using the
// credential the actuation path resolves (fleetcfg.CellCredential with
// the fleetd preference — a doctor that resolved credentials its own way
// would be testing its own code).
//
// Status is the call because it is read-only and cheap: it reports the
// active profile and changes nothing. There is no verb on this path and
// there must never be one.
func (d *Daemon) cellAuthProbe(ctx context.Context, cell string) fleetapi.CellAuthResult {
	if d.hosts == nil {
		return fleetapi.CellAuthResult{}
	}
	c, ok := d.hosts.Cells[cell]
	if !ok || c.DaemonURL == "" {
		// After C3 a cell without daemon_url is correctly configured: the
		// announce piggyback queue is its actuation channel.
		return fleetapi.CellAuthResult{}
	}
	cred, err := d.hosts.CellCredential(cell, os.Getenv("VIBE_TOKEN"), fleetcfg.PreferCellFile, vibeclient.ResolveToken)
	if err != nil {
		return fleetapi.CellAuthResult{Attempted: true, CredentialErr: err.Error()}
	}
	res := fleetapi.CellAuthResult{Attempted: true, Source: cred.Source}
	cctx, cancel := context.WithTimeout(ctx, doctorAuthTimeout)
	defer cancel()
	client := vibeclient.NewWithHTTPClient(c.DaemonURL, &http.Client{Timeout: doctorAuthTimeout}, cred.Token)
	if _, err := client.Status(cctx); err != nil {
		res.Err = err.Error()
		// A 401 is the far side ANSWERING; a timeout is the absence of an
		// answer. Collapsing the two would report a box that is off as a
		// wrong password.
		if connect.CodeOf(err) == connect.CodeUnauthenticated || connect.CodeOf(err) == connect.CodePermissionDenied {
			res.Denied = true
		}
		return res
	}
	res.OK = true
	return res
}
