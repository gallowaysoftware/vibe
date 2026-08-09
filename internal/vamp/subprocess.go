package vamp

import (
	"context"
	"os/exec"
	"time"
)

// One shape for every external binary vamp runs.
//
// vamp shells out eleven times — ffmpeg four ways, ffprobe, piper, an
// audio-effect pass, rsvg-convert, pandoc (sometimes as `docker run`) —
// and every one of them used a bare exec.CommandContext. That gets the
// easy half right and the hard half wrong.
//
// The easy half: exec.CommandContext DOES kill the process it started
// when ctx ends. Verified, and load-bearing here: none of these eleven
// sites uses `sh -c`, so the process it started IS the work, and
// internal/vibe/shellcmd's forking-shell defect — a dash that forks the
// command and leaves the deadline killing an empty shell — does not
// apply.
//
// The hard half: Cmd.Wait does not return until the stdout/stderr PIPES
// close, and a WaitDelay of ZERO is documented to mean "wait
// indefinitely". ffmpeg's stderr is inherited by anything it spawned,
// and a killed process wedged in uninterruptible I/O never closes it. So
// a cancelled stage's Run could outlive its cancellation without bound:
// the deadline fires exactly on time and the call still does not come
// back, which on the wire is indistinguishable from the bound not
// working at all.
//
// # What this deliberately does NOT do
//
// shellcmd pairs its WaitDelay with Setpgid and a negative-pid kill, and
// copying that here would be a regression rather than defence in depth.
// Setpgid takes the child OUT of the terminal's foreground process
// group, so an operator's ctrl-C during a forty-minute ffmpeg render
// would no longer reach ffmpeg: vamp dies on the default SIGINT
// disposition without running any deferred cancel, and the render keeps
// burning a GPU with nothing left to stop it. shellcmd needs the group
// because its child is a SHELL that forks the real work somewhere the
// deadline cannot see; here the child is the work, and a group of its
// own buys nothing to pay that with.
//
// (`docker run` is the one site where the work genuinely lives
// elsewhere — in a container dockerd owns, which no process-group signal
// could reach either way. Ending that properly needs a named container
// and a `docker kill` from Cancel; it is called out in the PR rather
// than half-done here.)

// subprocessKillGrace bounds how long Wait may block on the pipes after
// the process has been killed. Two seconds, matching
// shellcmd.DefaultKillGrace: long enough to still capture a killed
// ffmpeg's last stderr line for the error a human reads, short enough to
// be noise next to any stage budget worth having.
const subprocessKillGrace = 2 * time.Second

// command builds an *exec.Cmd whose ctx cancellation actually ends the
// call.
//
// Every subprocess in this package goes through here so the bound is a
// property of the package rather than of whoever wrote the newest
// executor — the same "guard in one of N call paths" shape that left two
// of four ffmpeg output checks missing.
func command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = subprocessKillGrace
	return cmd
}
