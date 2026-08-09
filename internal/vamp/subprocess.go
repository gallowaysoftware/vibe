package vamp

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// One shape for every external binary vamp runs.
//
// vamp shells out eleven times — ffmpeg four ways, ffprobe, piper, an
// audio-effect pass, rsvg-convert, pandoc (sometimes as `docker run`) —
// and every one of them used a bare exec.CommandContext. That gets the
// easy half right and the hard half wrong:
//
//   - exec.CommandContext DOES kill the process it started when ctx
//     ends. Good news, verified: none of these sites uses `sh -c`, so
//     the process it started is the work, and internal/vibe/shellcmd's
//     forking-shell defect does not apply here.
//   - Cmd.Wait, however, does not return until the STDOUT/STDERR PIPES
//     close, and a WaitDelay of zero is documented as "wait
//     indefinitely". ffmpeg's stderr is inherited by anything it
//     spawned, and a killed process wedged in uninterruptible I/O never
//     closes it. So a cancelled stage's Run can outlive the
//     cancellation by an unbounded amount — the deadline fires exactly
//     on time and the call still does not come back, which on the wire
//     is indistinguishable from the bound not working.
//
// So: same two mechanisms as shellcmd, for the same reasons, one of
// them (the process group) demoted from load-bearing to defence in
// depth because these are not shells.
//
// Not shellcmd.New itself: that builds `sh -c <script>`, which is a
// different and worse call shape for an argv this package already has
// as a slice — going through a shell here would ADD the quoting hazard
// and the forking-grandchild problem that shellcmd exists to survive.
// The shape is reused; the shell is not.

// subprocessKillGrace bounds how long Wait may block on the pipes after
// the process has been killed. Two seconds, matching
// shellcmd.DefaultKillGrace: long enough to still capture a killed
// ffmpeg's last stderr line for the error a human reads, short enough
// to be noise next to any stage budget worth having.
const subprocessKillGrace = 2 * time.Second

// command builds an *exec.Cmd whose ctx cancellation actually ends the
// call.
//
// Every subprocess in this package goes through here so the bound is a
// property of the package rather than of whoever wrote the newest
// executor — the same "guard in one of N call paths" shape that left
// two of four ffmpeg outputs unchecked.
func command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	// Setpgid + a negative-pid kill reaches anything the child spawned.
	// ffmpeg and piper do not fork today; pandoc's docker path is the
	// one that might, and a helper that only works for the binaries we
	// happen to run now is the kind that stops working silently.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				// The group is already gone: the command finished on the
				// same tick the deadline fired. Saying so lets a command
				// that beat the wire report its own result rather than
				// the context's.
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	// The bound that does not depend on the kill landing.
	cmd.WaitDelay = subprocessKillGrace
	return cmd
}
