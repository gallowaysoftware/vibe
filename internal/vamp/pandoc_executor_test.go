package vamp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// writeFakeBinary writes an executable /bin/sh script at dir/name and
// returns its path.
//
// Every stage type in this package that shells out takes its binary from
// Stage.Binary, so a script standing in for pandoc or ffmpeg needs no
// production seam and no live tool: it can exit 0 having written exactly
// what the test wants the real binary to have written — including
// NOTHING, which is the case the guards under test exist for and the one
// a real pandoc will not reproduce on demand.
//
// Shared by the pandoc and mix tests. /bin/sh rather than a compiled
// helper because this package's tests already assume a POSIX host
// (t.TempDir paths, 0o755 modes, the ffmpeg/piper/rsvg call sites the
// executors are built around) and CI is ubuntu-latest.
func writeFakeBinary(t *testing.T, dir, name, script string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// The /bin/sh bodies the pandoc tests use in place
// of the real binary. pandoc's argv ends with `-o <out> <src>`, so the
// output path is found by scanning for the flag rather than by position:
// a positional read would silently follow any future argv change instead
// of failing.
const (
	// pandocExitsZeroWritingNothing is the failure an exit status cannot
	// report: the converter claims success and no artefact exists.
	pandocExitsZeroWritingNothing = `exit 0`
	// pandocWritesAnEmptyFile is its subtler twin — the file EXISTS and
	// is zero bytes, which an existence check passes and a human reading
	// a run log never notices.
	pandocWritesAnEmptyFile = `
prev=""
for a in "$@"; do
  if [ "$prev" = "-o" ]; then out="$a"; fi
  prev="$a"
done
: > "$out"
exit 0`
	// pandocWritesRealBytes is the success path.
	pandocWritesRealBytes = `
prev=""
for a in "$@"; do
  if [ "$prev" = "-o" ]; then out="$a"; fi
  prev="$a"
done
printf 'EPUB-ish bytes' > "$out"
exit 0`
)

// newPandocStage builds a minimal valid pandoc stage pointed at binary.
func newPandocStage(binary string) *Stage {
	return &Stage{
		ID:         "book",
		Type:       StageTypePandoc,
		SourceFile: "manuscript.md",
		Output:     "book.epub",
		PandocFrom: "markdown",
		PandocTo:   "epub",
		Binary:     binary,
	}
}

// writePandocSource creates the source_file the executor stats before it
// runs anything.
func writePandocSource(t *testing.T, runDir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(runDir, "manuscript.md"), []byte("# Chapter One\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestPandocExecutor_HappyPath is the control the failure cases are read
// against: the same wiring, with the fake binary writing real bytes, must
// SUCCEED and report the absolute output path. Without it, a guard that
// rejected every run would look identical to a guard that works.
func TestPandocExecutor_HappyPath(t *testing.T) {
	runDir := t.TempDir()
	writePandocSource(t, runDir)
	binDir := t.TempDir()
	fake := writeFakeBinary(t, binDir, "pandoc", pandocWritesRealBytes)

	out, err := (&pandocExecutor{}).Execute(context.Background(), StageInput{
		Stage:  newPandocStage(fake),
		RunDir: runDir,
	})
	if err != nil {
		t.Fatalf("pandoc stage failed on a binary that produced output: %v", err)
	}
	want := filepath.Join(runDir, "book.epub")
	if len(out.Files) != 1 || out.Files[0] != want {
		t.Fatalf("StageOutput.Files = %v, want [%s]", out.Files, want)
	}
}

// TestPandocExecutor_ZeroByteOutputFailsTheStage covers the failure the
// exit code does not report, at the fourth of the four output-producing
// executors.
//
// pandoc opens its output file before it knows the conversion will work,
// so a missing LaTeX engine on `--to pdf` or an undecodable
// --epub-cover-image can leave a 0-byte artefact and exit 0. The docker
// fallback adds a second route to the same place, because `docker run`
// reports the CLIENT's status. The cost is not one bad run: StageOutput.
// Files is what tryCachePut reads, so an empty EPUB becomes a
// content-addressed entry that replays until somebody clears the cache by
// hand.
//
// Both spellings are covered because they fail different checks — "no file
// at all" is caught by the stat, "an empty file" only by the size — and
// the executor previously checked only the first.
func TestPandocExecutor_ZeroByteOutputFailsTheStage(t *testing.T) {
	cases := []struct {
		name   string
		script string
	}{
		{"no file at all", pandocExitsZeroWritingNothing},
		{"an empty file", pandocWritesAnEmptyFile},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runDir := t.TempDir()
			writePandocSource(t, runDir)
			fake := writeFakeBinary(t, t.TempDir(), "pandoc", c.script)

			out, err := (&pandocExecutor{}).Execute(context.Background(), StageInput{
				Stage:  newPandocStage(fake),
				RunDir: runDir,
			})
			if err == nil {
				t.Fatalf("pandoc exited 0 having produced nothing and the stage reported success: %v", out)
			}
			if out != nil {
				t.Errorf("a failed stage must not hand a file to the cache layer: %v", out)
			}
			if !strings.Contains(err.Error(), "book") {
				t.Errorf("error should name the stage: %v", err)
			}
		})
	}
}

// TestPandocExecutor_MissingSourceFileNamesThePath pins the error text a
// human acts on. The stage fails before any subprocess runs, so the path
// it names is the only clue to what went wrong.
func TestPandocExecutor_MissingSourceFileNamesThePath(t *testing.T) {
	runDir := t.TempDir()
	_, err := (&pandocExecutor{}).Execute(context.Background(), StageInput{
		Stage:  newPandocStage("/nonexistent/pandoc"),
		RunDir: runDir,
	})
	if err == nil {
		t.Fatal("a missing source_file must fail the stage")
	}
	if !strings.Contains(err.Error(), "manuscript.md") {
		t.Errorf("error should name the missing source file: %v", err)
	}
}

// TestPandocExecutor_NonZeroExitFailsTheStage is the ordinary failure —
// the binary itself reports the problem — and exists so the 0-byte cases
// above are not the only thing standing between a broken conversion and a
// green run.
func TestPandocExecutor_NonZeroExitFailsTheStage(t *testing.T) {
	runDir := t.TempDir()
	writePandocSource(t, runDir)
	fake := writeFakeBinary(t, t.TempDir(), "pandoc", `echo "pandoc: unknown format" >&2; exit 3`)

	_, err := (&pandocExecutor{}).Execute(context.Background(), StageInput{
		Stage:  newPandocStage(fake),
		RunDir: runDir,
	})
	if err == nil {
		t.Fatal("a non-zero pandoc exit must fail the stage")
	}
	// The binary's own exit status, not just "something went wrong": a
	// converter that distinguishes "bad format" from "no such file" by
	// its status code is useless if the stage flattens both.
	if !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("error should carry pandoc's exit status: %v", err)
	}
}

// TestPandocExecutor_CancelledContextEndsTheStage pins the bound. A
// pandoc conversion is minutes of work; a cancelled run whose stage
// ignores the cancellation is indistinguishable, on the wire, from a run
// that hung.
func TestPandocExecutor_CancelledContextEndsTheStage(t *testing.T) {
	runDir := t.TempDir()
	writePandocSource(t, runDir)
	fake := writeFakeBinary(t, t.TempDir(), "pandoc", `sleep 60`)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := (&pandocExecutor{}).Execute(ctx, StageInput{
		Stage:  newPandocStage(fake),
		RunDir: runDir,
	})
	if err == nil {
		t.Fatal("a cancelled pandoc stage must fail")
	}
	// The bound, not the exact duration: subprocessKillGrace is 2s and the
	// sleep is 60s, so anything under ten seconds proves the cancellation
	// reached the child rather than the call waiting the sleep out.
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("cancellation took %s; the stage outlived its context", elapsed)
	}
}

// ── the docker fallback's orphan ──────────────────────────────────────

// fakeDockerKiller records every Kill and replies with a scripted error.
// It also captures whether the context it was handed carried a deadline,
// which is the only way to assert the reap is bounded without waiting for
// one.
type fakeDockerKiller struct {
	mu           sync.Mutex
	names        []string
	hadDeadline  []bool
	err          error
	blockUntilCt bool // block until the supplied ctx is done
}

func (f *fakeDockerKiller) Kill(ctx context.Context, name string) error {
	_, ok := ctx.Deadline()
	f.mu.Lock()
	f.names = append(f.names, name)
	f.hadDeadline = append(f.hadDeadline, ok)
	blockUntil, err := f.blockUntilCt, f.err
	f.mu.Unlock()
	if blockUntil {
		<-ctx.Done()
		return ctx.Err()
	}
	return err
}

func (f *fakeDockerKiller) killed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.names...)
}

// TestPandocContainerName_IsUniquePerInvocationAndGreppable pins both
// halves of the naming rule, which pull in opposite directions.
//
// Unique, because `docker run --name` FAILS on a name already in use: two
// concurrent pandoc stages — or two foreach items of one stage, or a
// retry whose predecessor is still shutting down — sharing a name is a
// stage death with no relation to its inputs.
//
// Greppable, because the process does not always get to run its own
// cleanup. After a SIGKILL there is no Cancel hook, no defer and no
// process; the container's NAME is the entire remaining handle, and a
// fixed prefix is what makes `docker ps --filter name=vamp-pandoc-` list
// this executor's orphans and nothing else.
func TestPandocContainerName_IsUniquePerInvocationAndGreppable(t *testing.T) {
	runDir := "/state/vamp/runs/2026-08-09T11-02-03_book"
	a := pandocContainerName(runDir, "build_epub", 0)
	b := pandocContainerName(runDir, "build_epub", 0)
	if a == b {
		t.Fatalf("two invocations of the same stage got the same container name %q; `docker run --name` fails on a name in use", a)
	}
	for _, name := range []string{a, b} {
		if !strings.HasPrefix(name, pandocContainerPrefix) {
			t.Errorf("container name %q lost the greppable prefix %q", name, pandocContainerPrefix)
		}
		if !strings.Contains(name, "build_epub") {
			t.Errorf("container name %q does not name the stage it belongs to", name)
		}
		if !strings.Contains(name, "2026-08-09T11-02-03_book") {
			t.Errorf("container name %q does not name the run it belongs to", name)
		}
	}
	// Two foreach items of ONE stage must differ even before the random
	// tail is considered, so a name read out of a log identifies the item.
	i0 := pandocContainerName(runDir, "build_epub", 0)
	i1 := pandocContainerName(runDir, "build_epub", 1)
	if strings.TrimSuffix(i0, i0[strings.LastIndex(i0, "-"):]) == strings.TrimSuffix(i1, i1[strings.LastIndex(i1, "-"):]) {
		t.Errorf("foreach items 0 and 1 share everything but the random tail: %q vs %q", i0, i1)
	}
}

// TestPandocContainerName_IsLegalDockerIdentifier keeps author-controlled
// strings out of the argv unfiltered. A stage id and a pipeline name come
// from YAML; docker names must match [a-zA-Z0-9][a-zA-Z0-9_.-]*, so a
// stage called "build epub / v2" would otherwise produce a name docker
// rejects — turning a cosmetic YAML choice into a stage that cannot run.
func TestPandocContainerName_IsLegalDockerIdentifier(t *testing.T) {
	name := pandocContainerName("/runs/2026-08-09T11-02-03_a book (draft)", "build epub / v2", 0)
	if name == "" {
		t.Fatal("empty container name")
	}
	first := rune(name[0])
	if (first < 'a' || first > 'z') && (first < 'A' || first > 'Z') && (first < '0' || first > '9') {
		t.Errorf("container name %q must start alphanumeric", name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
		default:
			t.Errorf("container name %q contains %q, which docker rejects", name, r)
		}
	}
}

// TestBuildPandocArgs_NamesTheContainerBeforeTheImage pins the position,
// not just the presence. Everything after the image name on a `docker
// run` argv is the CONTAINER's argv: a --name that drifted past
// pandoc/core:latest would be handed to pandoc, which fails with an
// unrelated "unrecognised option" and leaves the container unnamed —
// exactly the orphan this whole mechanism exists to prevent, with a
// misleading error on top.
func TestBuildPandocArgs_NamesTheContainerBeforeTheImage(t *testing.T) {
	st := newPandocStage("docker")
	args := buildPandocArgs(st, "/run/manuscript.md", "/run/book.epub", "", true, "/run", "vamp-pandoc-x-y-0-deadbeef")

	nameIdx, imageIdx := -1, -1
	for i, a := range args {
		switch a {
		case "--name":
			nameIdx = i
		case pandocDockerImage:
			imageIdx = i
		}
	}
	if nameIdx < 0 {
		t.Fatalf("docker argv carries no --name: %v", args)
	}
	if imageIdx < 0 {
		t.Fatalf("docker argv carries no image: %v", args)
	}
	if nameIdx > imageIdx {
		t.Errorf("--name at %d is after the image at %d, so docker never sees it: %v", nameIdx, imageIdx, args)
	}
	if args[nameIdx+1] != "vamp-pandoc-x-y-0-deadbeef" {
		t.Errorf("--name value = %q", args[nameIdx+1])
	}
}

// TestBuildPandocArgs_HostInvocationCarriesNoDockerFlags is the other
// side of the same guard: --name is a docker flag, and a host pandoc
// would reject it. A single argv builder serving both shapes is exactly
// where that leaks.
func TestBuildPandocArgs_HostInvocationCarriesNoDockerFlags(t *testing.T) {
	st := newPandocStage("pandoc")
	args := buildPandocArgs(st, "/run/manuscript.md", "/run/book.epub", "", false, "/run", "vamp-pandoc-x-y-0-deadbeef")
	for _, a := range args {
		if a == "--name" || a == "--rm" || a == pandocDockerImage {
			t.Errorf("host pandoc argv carries the docker flag %q: %v", a, args)
		}
	}
}

// TestReapDockerContainer_AlreadyExitedIsNotAFailure covers the COMMON
// case rather than an edge one. `docker kill` races the container's own
// exit, and the ordinary shape of a cancelled stage is that pandoc
// finishes or dies at almost the same instant the reap fires. dockerd
// answers that race with an error; treating it as a failure would put an
// alarming line in the log of every correctly-cancelled run.
//
// Both daemon spellings are covered because they come from different
// races: the container stopped (is not running) versus --rm already
// removed it (No such container).
func TestReapDockerContainer_AlreadyExitedIsNotAFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"stopped", errors.New("exit status 1: Error response from daemon: Cannot kill container: vamp-pandoc-x: Container 9f3c is not running")},
		{"already removed", errors.New("exit status 1: Error response from daemon: No such container: vamp-pandoc-x")},
		{"clean kill", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := &fakeDockerKiller{err: c.err}
			if err := reapDockerContainer(k, "vamp-pandoc-x"); err != nil {
				t.Fatalf("a container that is already gone is a successful reap, got: %v", err)
			}
			if got := k.killed(); len(got) != 1 || got[0] != "vamp-pandoc-x" {
				t.Errorf("Kill calls = %v, want exactly [vamp-pandoc-x]", got)
			}
		})
	}
}

// TestReapDockerContainer_SurfacesARealFailure is the control: a dockerd
// that is DOWN is not the same as a container that is gone, and a reap
// that swallowed both would report success while a container kept
// burning the machine.
func TestReapDockerContainer_SurfacesARealFailure(t *testing.T) {
	k := &fakeDockerKiller{err: errors.New("Cannot connect to the Docker daemon at unix:///var/run/docker.sock")}
	err := reapDockerContainer(k, "vamp-pandoc-x")
	if err == nil {
		t.Fatal("an unreachable dockerd must be reported, not treated as a successful reap")
	}
	if !strings.Contains(err.Error(), "docker.sock") {
		t.Errorf("error should carry the daemon's own message: %v", err)
	}
}

// TestReapDockerContainer_IsBounded pins the bound. The reap runs on the
// teardown path of a stage that is ALREADY being cancelled — a timeout
// firing, an operator's ctrl-C — so an unresponsive dockerd must not
// convert a bounded stage into an unbounded one.
//
// Two assertions, because either alone is satisfiable by a broken
// implementation: that the killer is handed a context carrying a
// deadline, and that the call returns without one being reached by the
// test's own patience.
func TestReapDockerContainer_IsBounded(t *testing.T) {
	k := &fakeDockerKiller{}
	if err := reapDockerContainer(k, "vamp-pandoc-x"); err != nil {
		t.Fatal(err)
	}
	k.mu.Lock()
	deadlines := append([]bool(nil), k.hadDeadline...)
	k.mu.Unlock()
	if len(deadlines) != 1 || !deadlines[0] {
		t.Fatalf("the reap handed the killer an unbounded context (deadlines=%v); an unreachable dockerd would hang teardown", deadlines)
	}

	// And the context is one the reap OWNS, derived from Background: every
	// call site is on a path where the stage's own ctx is already
	// cancelled, so inheriting it would cancel the reap before it made its
	// one request.
	//
	// The bound is shortened for the duration: the assertion is that the
	// deadline FIRES and is reported, not what the production number is
	// (that is asserted above, and stated in the constant's own doc).
	restore := dockerKillTimeout
	dockerKillTimeout = 50 * time.Millisecond
	defer func() { dockerKillTimeout = restore }()

	blocking := &fakeDockerKiller{blockUntilCt: true}
	done := make(chan error, 1)
	go func() { done <- reapDockerContainer(blocking, "vamp-pandoc-x") }()
	select {
	case err := <-done:
		// Reaching the deadline is a real failure to report, not silence.
		if err == nil {
			t.Error("a reap that timed out must be reported")
		}
	case <-time.After(dockerKillTimeout + 5*time.Second):
		t.Fatal("the reap never returned; teardown is unbounded")
	}
}

// TestNewPandocCommand_CancelKillsTheContainerNotJustTheClient is the
// whole point of the named container, and it is the one assertion a
// process-level test cannot make by watching the child alone.
//
// `docker run` is a thin client for a container DOCKERD owns. Killing the
// client — which is all exec.CommandContext's default Cancel can do, and
// all a process-group kill could do either — leaves the container
// running, and with --rm nothing ever cleans up after it. So the
// assertion is not "the child died" (it does either way) but "the reap
// ran, with the right name, before the client was killed".
func TestNewPandocCommand_CancelKillsTheContainerNotJustTheClient(t *testing.T) {
	sleeper := writeFakeBinary(t, t.TempDir(), "docker", `sleep 60`)
	k := &fakeDockerKiller{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var log bytes.Buffer
	cmd := newPandocCommand(ctx, k, sleeper, []string{"run", "--rm", "--name", "vamp-pandoc-run-stage-0-deadbeef"}, "vamp-pandoc-run-stage-0-deadbeef", &log)
	cmd.Stdout = &log
	cmd.Stderr = &log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	cancel()
	start := time.Now()
	err := cmd.Wait()
	if err == nil {
		t.Fatal("a cancelled docker run must not report success")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("Wait took %s after cancellation; WaitDelay is not bounding the pipe wait", elapsed)
	}
	if got := k.killed(); len(got) != 1 || got[0] != "vamp-pandoc-run-stage-0-deadbeef" {
		t.Fatalf("docker kill calls = %v; the container dockerd owns was left running", got)
	}
}

// TestNewPandocCommand_HostInvocationNeverRunsDockerKill is the negative
// half, named for exactly what it asserts. A host pandoc has no
// container: reaping one that never existed would spawn `docker kill` on
// every cancelled host conversion, on a machine that may not have docker
// installed at all, and print an error about it.
func TestNewPandocCommand_HostInvocationNeverRunsDockerKill(t *testing.T) {
	sleeper := writeFakeBinary(t, t.TempDir(), "pandoc", `sleep 60`)
	k := &fakeDockerKiller{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := newPandocCommand(ctx, k, sleeper, []string{"--version"}, "", nil)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = cmd.Wait()
	if got := k.killed(); len(got) != 0 {
		t.Errorf("a host pandoc invocation ran `docker kill` on %v", got)
	}
}

// TestPandocCommand_DoesNotPutTheChildInItsOwnProcessGroup pins a
// DELIBERATE omission, which is the kind that silently comes back.
//
// internal/vibe/shellcmd pairs its WaitDelay with Setpgid and a
// negative-pid kill, and copying that here was tried and rejected as a
// regression: Setpgid takes the child out of the terminal's foreground
// process group, so an operator's ctrl-C during a forty-minute render
// stops reaching it — vamp dies on the default SIGINT disposition
// without running any deferred cancel, and the render keeps burning a
// GPU with nothing left to stop it. shellcmd needs the group because its
// child is a SHELL that forks the real work; here the child IS the work.
//
// Both shapes are asserted because the docker path builds its Cmd
// through a second constructor, which is where a well-meaning "defence in
// depth" edit would land.
func TestPandocCommand_DoesNotPutTheChildInItsOwnProcessGroup(t *testing.T) {
	plain := command(context.Background(), "true")
	if plain.SysProcAttr != nil {
		t.Errorf("command() set SysProcAttr = %+v; ctrl-C must keep reaching a long render", plain.SysProcAttr)
	}
	docked := newPandocCommand(context.Background(), &fakeDockerKiller{}, "docker", []string{"run"}, "vamp-pandoc-x", nil)
	if docked.SysProcAttr != nil {
		t.Errorf("newPandocCommand set SysProcAttr = %+v; ctrl-C must keep reaching a long render", docked.SysProcAttr)
	}
	if docked.WaitDelay <= 0 {
		t.Error("WaitDelay is what bounds Wait on the pipes after a kill; zero means wait forever")
	}
}

// TestDockerCLIKiller_FoldsTheDaemonMessageIntoTheError pins the contract
// containerAlreadyGone depends on. The docker CLI exits 1 for EVERY
// daemon error, so the exit status cannot distinguish "already stopped"
// from "dockerd is down": the message text is the only discriminator, and
// a killer that dropped it would make every reap look like a hard failure.
func TestDockerCLIKiller_FoldsTheDaemonMessageIntoTheError(t *testing.T) {
	// A stand-in that fails the way the CLI does: a message on stderr and
	// a non-zero exit. Run through the real dockerCLIKiller shape by
	// pointing it at a fake `docker` on PATH.
	binDir := t.TempDir()
	writeFakeBinary(t, binDir, "docker", `echo "Error response from daemon: No such container: vamp-pandoc-x" >&2; exit 1`)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	err := dockerCLIKiller{}.Kill(context.Background(), "vamp-pandoc-x")
	if err == nil {
		t.Fatal("a failing `docker kill` must return an error")
	}
	if !strings.Contains(err.Error(), "No such container") {
		t.Fatalf("the daemon's message was dropped, so containerAlreadyGone cannot classify it: %v", err)
	}
	if !containerAlreadyGone(err) {
		t.Errorf("this is the already-gone case and it was not recognised: %v", err)
	}
	// And the round trip through the real killer is a successful reap.
	if reapErr := reapDockerContainer(dockerCLIKiller{}, "vamp-pandoc-x"); reapErr != nil {
		t.Errorf("reap of an already-gone container reported failure: %v", reapErr)
	}
}

// TestPandocExecutor_DockerFallbackIsNamed ties the naming to the
// EXECUTOR rather than to buildPandocArgs alone: a stage that reached
// docker with a correct argv builder it never called is the same orphan.
//
// Driven through Stage.Binary "docker" — the documented explicit trigger
// for the fallback shape — with a fake `docker` on PATH, so nothing here
// needs a dockerd.
func TestPandocExecutor_DockerFallbackIsNamed(t *testing.T) {
	runDir := t.TempDir()
	writePandocSource(t, runDir)
	binDir := t.TempDir()
	// Record the argv, then write the output the real conversion would
	// have produced (docker's argv ends `-o <out> <src>`, and the bind
	// mount means the container path is under /data — the fake writes to
	// the HOST path the executor stats, which is what a real bind-mounted
	// run also ends up doing).
	argvLog := filepath.Join(binDir, "argv.txt")
	writeFakeBinary(t, binDir, "docker", fmt.Sprintf(`
for a in "$@"; do echo "$a" >> %q; done
printf 'EPUB-ish bytes' > %q
exit 0`, argvLog, partialOutputPath(filepath.Join(runDir, "book.epub"))))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var log bytes.Buffer
	st := newPandocStage("docker")
	if _, err := (&pandocExecutor{}).Execute(context.Background(), StageInput{
		Stage:  st,
		RunDir: runDir,
		Log:    &log,
	}); err != nil {
		t.Fatalf("docker fallback failed: %v", err)
	}

	raw, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Split(strings.TrimSpace(string(raw)), "\n")
	nameIdx := -1
	for i, a := range argv {
		if a == "--name" {
			nameIdx = i
		}
	}
	if nameIdx < 0 || nameIdx+1 >= len(argv) {
		t.Fatalf("the executor ran `docker run` without naming the container: %v", argv)
	}
	name := argv[nameIdx+1]
	if !strings.HasPrefix(name, pandocContainerPrefix) {
		t.Errorf("container name %q is not greppable by the documented prefix", name)
	}
	// The run log is where a human looks for the handle after a crash the
	// Cancel hook never got to run for.
	if !strings.Contains(log.String(), name) {
		t.Errorf("the run log does not record the container name; an orphan left by a SIGKILL has no handle:\n%s", log.String())
	}
}
