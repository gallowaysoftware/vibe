package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// ensureDaemon makes sure the vibe daemon is listening. If it isn't, ensureDaemon
// re-execs the current binary in "daemon" mode in a new session and waits for
// the unix socket to come up.
func ensureDaemon(ctx context.Context) error {
	if err := pingDaemon(200 * time.Millisecond); err == nil {
		return nil
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate executable: %w", err)
	}
	cmd := exec.Command(exe, "daemon")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// Don't tie the daemon to the user's terminal.
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn daemon: %w", err)
	}
	_ = cmd.Process.Release()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := pingDaemon(200 * time.Millisecond); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("vibe daemon failed to come up within 5s; try `vibe daemon` in the foreground to see why")
}
