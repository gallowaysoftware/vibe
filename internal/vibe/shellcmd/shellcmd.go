// Package shellcmd builds the one shape this repo runs an operator's
// shell string in.
//
// It exists because that shape is subtle, security-adjacent, and was
// derived independently twice in the same week — once for a cell's wake
// command (U5) and once for a cell's drain verb (U3) — each time from a
// CI failure that read as a broken test rather than a broken bound. A
// third call site deriving it a third time is the "guard in one of N call
// paths" class this plan keeps meeting, so the derivation lives here and
// the call sites name it.
//
// # The defect
//
// exec.CommandContext kills the process it STARTED. With `sh -c` that is
// the shell, and the shell is frequently not where the work is: dash —
// /bin/sh on Debian, Ubuntu and the CI runner — FORKS the command rather
// than exec'ing into it, and so does every shell for a script containing
// a `;`, a `&&`, a subshell or a background job. The forked grandchild
// inherits the stdout/stderr pipes, and Cmd.Wait waits for the pipe COPY
// to finish, not for the process it killed. So the deadline fires exactly
// on time, the shell dies, the error says `signal: killed` — and the call
// does not return until the operator's ssh/ipmitool/systemctl finishes on
// its own.
//
// It is invisible on a workstation whose /bin/sh is bash, because bash
// execs a lone simple command and the one process the deadline kills
// really is the whole thing. Both units above shipped green locally and
// red in CI at exactly the command's own runtime.
//
// # The shape
//
// Two mechanisms, neither of which subsumes the other:
//
//   - A process group of its own plus a negative-pid SIGKILL ends the
//     WORK, reaching whatever the shell forked. Half-finished work that
//     outlives the verb that launched it is what an operator pays for
//     later, and on the wire it is indistinguishable from the bound
//     having worked.
//   - WaitDelay ends the WAIT, unconditionally, for the descendant the
//     kill cannot reach: one that setsid'd out of the group, or one
//     wedged in uninterruptible I/O. A ZERO WaitDelay is documented to
//     mean Wait blocks indefinitely on the pipes, which is the defect
//     itself — see New for why a caller cannot reinstate it by accident.
package shellcmd

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// DefaultKillGrace is what New falls back to when a caller asks for a
// non-positive one.
//
// Two seconds: long enough to still capture a killed command's last
// stderr line for the error a human reads, short enough to be noise next
// to any budget worth having.
const DefaultKillGrace = 2 * time.Second

// New returns an `sh -c script` command whose ctx cancellation actually
// ends the call: the kill reaches the process group the shell forked
// into, and Wait cannot outlive that kill by more than killGrace.
//
// It builds the command without starting it, so a test can read the
// wiring back off the result — which is the only assertion available on a
// platform where a process cannot be driven out of its own group.
//
// A non-positive killGrace is DefaultKillGrace, loudly. Zero is not a
// tighter bound, it is the absence of one (Cmd.WaitDelay documents zero
// as "wait indefinitely"), and a caller that reached it through an unset
// field would silently reinstate the exact defect this package exists to
// close. Refusing it here rather than at each call site is the point of
// having one builder.
func New(ctx context.Context, script string, killGrace time.Duration) *exec.Cmd {
	if killGrace <= 0 {
		slog.Warn("shellcmd: a non-positive kill grace is not a bound; using the default",
			"requested", killGrace, "default", DefaultKillGrace)
		killGrace = DefaultKillGrace
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	// Setpgid makes the shell a group leader, so the negative-pid kill
	// below reaches everything it forked. A non-interactive shell runs no
	// job control, so even backgrounded work stays in this group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		// SIGKILL rather than a graceful SIGTERM: the graceful window is
		// the budget the caller has just spent, and a command that
		// ignored it is not the kind that will honour a term. It is also
		// what exec.CommandContext does by default, so the failure mode
		// stays the one every reader expects — with the single difference
		// that it now lands on the group.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				// The whole group is already gone: the command finished
				// on the same tick the deadline fired. Not a failure to
				// cancel, and saying so lets a command that beat the wire
				// report its own result instead of the context's.
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	cmd.WaitDelay = killGrace
	return cmd
}
