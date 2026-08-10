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
	// The POSITIVE test — `err == nil` rather than `!daemonAbsent(err)` —
	// is deliberate here, and it is the one place in the CLI where it is
	// safe. This is NOT `ps`, `env` or `shutdown`: nothing is claimed from
	// this ping. A timeout falls through to spawning, and a second daemon
	// is harmless because daemon.Run takes an exclusive flock on
	// $PIDFILE.lock BEFORE it touches the socket path (daemon.go, "An
	// exclusive flock held for the daemon's lifetime is the single-instance
	// gate"). The loser returns "vibe daemon already running" and exits
	// without reaching the unconditional os.Remove(sockPath) below it, so
	// it cannot unlink the winner's live socket — the exact race the flock
	// replaced a PID file to close. The retry loop then succeeds against
	// whichever daemon holds the lock.
	//
	// So the short 200ms budget is a cost of one wasted fork, not a wrong
	// answer, which is why it stays short and does not use pingBudget. Do
	// not "fix" this to match the three commands above: the guard there
	// exists because they PRINT a claim, and this prints nothing.
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
