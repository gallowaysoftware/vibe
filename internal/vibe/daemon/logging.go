package daemon

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
)

// SetupLogging configures slog.Default for the daemon. Logs are always
// appended to $XDG_STATE_HOME/vibe/daemon.log and tee'd to stderr when the
// daemon runs attached to a terminal (foreground `vibe daemon`).
//
// The returned closer should be Close'd at daemon exit.
func SetupLogging() (io.Closer, error) {
	if err := os.MkdirAll(paths.StateHome(), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(paths.StateHome(), "daemon.log"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	var w io.Writer = f
	if fi, err := os.Stderr.Stat(); err == nil && (fi.Mode()&os.ModeCharDevice) != 0 {
		w = io.MultiWriter(f, os.Stderr)
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo})))
	return f, nil
}
