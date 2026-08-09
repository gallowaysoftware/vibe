package vamp

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// pandocExecutor implements StageExecutor for type: pandoc stages. It
// converts a source document (markdown, HTML, etc.) into a target format
// (EPUB, PDF, etc.) by shelling out to pandoc. When the configured binary
// isn't on $PATH the executor falls back to a docker invocation against
// the `pandoc/core` image so a host without a pandoc install still works.
//
// Stage fields consumed:
//   - SourceFile (required) — template rendering the input path
//   - PandocFrom / PandocTo (required) — --from / --to format flags
//   - PandocMetadata — map of --metadata key=value
//   - PandocArgs — appended raw args (e.g. ["--toc", "--toc-depth=3"])
//   - CoverImage — when set on an EPUB output, becomes
//     --epub-cover-image
//   - Binary — overrides "pandoc"; "docker" triggers the docker fallback
//     shape explicitly
type pandocExecutor struct {
	// killer ends the container started by the docker fallback. nil means
	// dockerCLIKiller (shells out to `docker kill`). Tests substitute a
	// fake so CI never needs a live dockerd.
	killer dockerKiller
}

var _ StageExecutor = (*pandocExecutor)(nil)

// pandocDockerImage is the upstream-maintained image used when pandoc
// isn't installed locally. ~200 MB on first pull, then cached.
const pandocDockerImage = "pandoc/core:latest"

// pandocContainerPrefix is the deterministic head of every container name
// this executor hands to `docker run`. It exists to be GREPPED: after a
// SIGKILL, a panic, or a machine losing power between `docker run` and the
// Cancel hook below, nothing in this process gets to run cleanup, and the
// only remaining handle on the orphan is its name. `docker ps --filter
// name=vamp-pandoc-` lists exactly this executor's containers and nothing
// else, so a human (or a cron) can reap what the process could not.
const pandocContainerPrefix = "vamp-pandoc-"

// dockerKillTimeout bounds the reap. Cancel runs on the teardown path of a
// cancelled or timed-out stage — the caller is already trying to stop — so
// an unreachable dockerd must not convert a bounded stage into an unbounded
// one. Five seconds is generous for a local unix-socket round trip and
// still short enough to be noise next to any stage budget worth having.
//
// A var rather than a const only so the bound itself is assertable: the
// test that proves an unresponsive dockerd cannot hang teardown has to
// WAIT for the deadline, and five real seconds on every CI run to observe
// a number this file already states is a bad trade. Nothing in production
// writes it.
var dockerKillTimeout = 5 * time.Second

// dockerKiller ends a container by name.
//
// A seam rather than a direct exec because the interesting logic here —
// the name, the already-exited case, the bound — is testable and a live
// dockerd in CI is not.
type dockerKiller interface {
	// Kill ends the container called name, honouring ctx as its bound.
	Kill(ctx context.Context, name string) error
}

// dockerCLIKiller is the production dockerKiller: `docker kill <name>`.
// The combined output is folded into the returned error because that text
// is what distinguishes "already gone" from "dockerd is down", and
// containerAlreadyGone below reads it.
type dockerCLIKiller struct{}

var _ dockerKiller = dockerCLIKiller{}

func (dockerCLIKiller) Kill(ctx context.Context, name string) error {
	out, err := command(ctx, "docker", "kill", name).CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// pandocContainerName builds the name for one `docker run`.
//
// Two requirements pull in opposite directions and both are load-bearing:
//
//   - UNIQUE per invocation. Two concurrent pandoc stages (or two foreach
//     items of one stage) sharing a name is not a cosmetic collision:
//     `docker run --name` FAILS on a name in use, so one of the two stages
//     dies on a conflict that has nothing to do with its inputs — and a
//     retry of a stage whose previous container is still shutting down
//     hits the same wall. The random tail is what removes that class.
//   - FINDABLE afterwards. Everything before the random tail is derived
//     from the run and the stage, so a name read out of a run log names
//     its own origin, and the fixed prefix makes the whole set greppable.
//
// Docker names must match [a-zA-Z0-9][a-zA-Z0-9_.-]*, so every borrowed
// segment is sanitised and length-capped rather than trusted: a stage id
// and a pipeline name are author-controlled strings.
func pandocContainerName(runDir, stageID string, itemIdx int) string {
	var tail [4]byte
	if _, err := rand.Read(tail[:]); err != nil {
		// crypto/rand failing is close to unthinkable, and a name that is
		// merely non-random is still better than no container name at all
		// (which is the orphan we are here to prevent). Fall back to the
		// clock, which is unique enough against the run+stage qualifier.
		binary.BigEndian.PutUint32(tail[:], uint32(time.Now().UnixNano()))
	}
	return pandocContainerPrefix +
		dockerNameSegment(filepath.Base(runDir), 48) + "-" +
		dockerNameSegment(stageID, 32) + "-" +
		fmt.Sprintf("%d-%s", itemIdx, hex.EncodeToString(tail[:]))
}

// dockerNameSegment reduces s to characters docker accepts inside a
// container name, capped at max characters (every character it keeps is
// one byte, so the cap is exact). Anything outside the legal set becomes
// "-"; an empty result becomes "x" so the segment never collapses to
// nothing and leaves two dashes adjacent with no information between them.
func dockerNameSegment(s string, max int) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= max {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "x"
	}
	return b.String()
}

// reapDockerContainer kills the named container, bounded, and reports
// "already gone" as success.
//
// Already-gone is the COMMON case, not an edge one: `docker kill` races
// the container's own exit, and the ordinary shape of a cancelled stage is
// that pandoc finishes or dies at almost the same moment. dockerd answers
// that race with an error, and treating it as a failure would put a
// scary line in every cancelled run's log describing nothing wrong.
//
// The bound is its own requirement (see dockerKillTimeout): this runs
// while the caller is tearing down, and it derives its context from
// Background on purpose — the stage's ctx is already cancelled by
// definition at every call site, so inheriting it would cancel the reap
// before it made its one request.
func reapDockerContainer(k dockerKiller, name string) error {
	if name == "" {
		return nil
	}
	if k == nil {
		k = dockerCLIKiller{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), dockerKillTimeout)
	defer cancel()
	err := k.Kill(ctx, name)
	if err == nil || containerAlreadyGone(err) {
		return nil
	}
	return err
}

// containerAlreadyGone reports whether err is dockerd saying the container
// this was asked to kill has already stopped or been removed. Matched on
// the message because the docker CLI's exit status is 1 for every daemon
// error, so the text is the only discriminator available without adopting
// the docker client library.
func containerAlreadyGone(err error) bool {
	if err == nil {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "is not running") ||
		strings.Contains(msg, "no such container") ||
		strings.Contains(msg, "is not up")
}

// newPandocCommand builds the *exec.Cmd for one pandoc invocation and, for
// the docker fallback ONLY, replaces the Cancel hook with one that ends the
// CONTAINER before it ends the client.
//
// This is the whole point of the named container. `docker run` is a thin
// client for a container dockerd owns: signalling the client — which is
// all exec.CommandContext's default Cancel, and all a process-group kill,
// can ever do — leaves the container running and burning the machine, and
// with --rm nothing ever cleans up after it either. The name is the only
// handle that reaches across the daemon boundary.
//
// Note what is deliberately NOT here: Setpgid. internal/vibe/shellcmd
// pairs its WaitDelay with a process group and a negative-pid kill, and
// copying that to vamp was tried and rejected as a regression — a group of
// its own takes the child out of the terminal's foreground group, so an
// operator's ctrl-C during a forty-minute render stops reaching it. See
// subprocess.go for the full argument.
func newPandocCommand(ctx context.Context, k dockerKiller, binary string, args []string, containerName string, log io.Writer) *exec.Cmd {
	cmd := command(ctx, binary, args...)
	if containerName == "" {
		return cmd
	}
	cmd.Cancel = func() error {
		if err := reapDockerContainer(k, containerName); err != nil && log != nil {
			// Log rather than return: the Cancel contract is about ending
			// the CLIENT, and failing to reap must not also prevent that.
			// The name is in the message so the manual `docker kill` is a
			// copy-paste.
			fmt.Fprintf(log, "pandoc: could not kill container %s: %v\n", containerName, err)
		}
		// Same as exec.CommandContext's default Cancel. Reached after the
		// container is down, so the client normally exits on its own and
		// this is the backstop for the case where it does not.
		return cmd.Process.Kill()
	}
	return cmd
}

func (p *pandocExecutor) Execute(ctx context.Context, in StageInput) (*StageOutput, error) {
	st := in.Stage
	if st == nil {
		return nil, errors.New("pandoc: missing stage")
	}
	if st.SourceFile == "" {
		return nil, fmt.Errorf("stage %s: source_file is required for type: pandoc", st.ID)
	}
	extra := map[string]any{}
	if st.Foreach != nil {
		extra[st.Foreach.Var] = in.Item
		extra["i"] = in.ItemIdx
	}
	srcRel, err := renderTemplate(st.ID+":source_file", st.SourceFile, st.Inputs, in.Inputs, in.Prior, in.RunDir, extra)
	if err != nil {
		return nil, fmt.Errorf("stage %s: render source_file: %w", st.ID, err)
	}
	srcAbs := srcRel
	if !filepath.IsAbs(srcAbs) {
		srcAbs = filepath.Join(in.RunDir, srcRel)
	}
	if _, err := os.Stat(srcAbs); err != nil {
		return nil, fmt.Errorf("stage %s: source_file %s: %w", st.ID, srcAbs, err)
	}

	outRel, err := renderTemplate(st.ID+":output", st.Output, st.Inputs, in.Inputs, in.Prior, in.RunDir, extra)
	if err != nil {
		return nil, fmt.Errorf("stage %s: render output: %w", st.ID, err)
	}
	outAbs := filepath.Join(in.RunDir, outRel)
	if err := os.MkdirAll(filepath.Dir(outAbs), 0o755); err != nil {
		return nil, fmt.Errorf("stage %s: create output dir: %w", st.ID, err)
	}

	var coverAbs string
	if st.CoverImage != "" {
		coverRel, err := renderTemplate(st.ID+":cover_image", st.CoverImage, st.Inputs, in.Inputs, in.Prior, in.RunDir, extra)
		if err != nil {
			return nil, fmt.Errorf("stage %s: render cover_image: %w", st.ID, err)
		}
		coverAbs = coverRel
		if !filepath.IsAbs(coverAbs) {
			coverAbs = filepath.Join(in.RunDir, coverRel)
		}
		if _, err := os.Stat(coverAbs); err != nil {
			return nil, fmt.Errorf("stage %s: cover_image %s: %w", st.ID, coverAbs, err)
		}
	}

	useDocker := false
	binary := st.Binary
	switch binary {
	case "":
		// Prefer a host pandoc when available so we avoid the docker
		// pull on first use; fall back to docker so the stage works on
		// hosts without pandoc installed.
		if _, err := exec.LookPath("pandoc"); err == nil {
			binary = "pandoc"
		} else {
			useDocker = true
			binary = "docker"
		}
	case "docker":
		useDocker = true
	}

	// Named ONLY on the docker path: `--name` is a docker run flag, and
	// passing it to a host pandoc would be an unrecognised option.
	containerName := ""
	if useDocker {
		containerName = pandocContainerName(in.RunDir, st.ID, in.ItemIdx)
	}
	args := buildPandocArgs(st, srcAbs, outAbs, coverAbs, useDocker, in.RunDir, containerName)
	if in.Log != nil {
		fmt.Fprintf(in.Log, "pandoc: %s -> %s (engine=%s)\n", filepath.Base(srcAbs), outRel, binary)
		if containerName != "" {
			// Recorded so the run log — a FILE under --detach — carries the
			// handle a human needs to reap an orphan this process never got
			// to clean up.
			fmt.Fprintf(in.Log, "pandoc: container %s\n", containerName)
		}
	}
	cmd := newPandocCommand(ctx, p.killer, binary, args, containerName, in.Log)
	cmd.Stdout = in.Log
	cmd.Stderr = in.Log
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("stage %s: pandoc: %w", st.ID, err)
	}
	// Existence was never the whole check. pandoc opens its output file
	// before it knows whether the conversion will work, so a filter that
	// dies (a missing LaTeX engine for --to pdf, an --epub-cover-image
	// pandoc cannot decode) can leave a 0-byte artefact behind — and the
	// docker path adds a second way to get one, since `docker run`
	// reports the CLIENT's exit status. Reuses the ffmpeg check so all
	// four output-producing executors say the same thing.
	if err := requireNonEmptyOutput(st.ID, "pandoc", outAbs); err != nil {
		return nil, err
	}
	return &StageOutput{Files: []string{outAbs}}, nil
}

// buildPandocArgs assembles the argv for the pandoc invocation. The host
// vs docker shapes differ only at the head: docker prepends the
// `run --rm --name NAME -v RUNDIR:/data -w /data` flags and rewrites paths
// to be relative to /data. The remaining pandoc flags are identical.
//
// containerName is ignored unless useDocker: it is a docker flag, and the
// pandoc binary would reject it.
func buildPandocArgs(st *Stage, srcAbs, outAbs, coverAbs string, useDocker bool, runDir, containerName string) []string {
	src, out, cover := srcAbs, outAbs, coverAbs
	pre := []string{}
	if useDocker {
		// Bind the run dir at /data so every path in the rest of the
		// argv resolves inside the container without smuggling
		// host-specific absolute paths in.
		pre = append(pre, "run", "--rm")
		if containerName != "" {
			// BEFORE the image name: everything after it is the
			// container's own argv, so a --name there would be handed to
			// pandoc instead of to docker.
			pre = append(pre, "--name", containerName)
		}
		pre = append(pre,
			"-v", runDir+":/data",
			"-w", "/data",
			pandocDockerImage,
		)
		src = rebasePath(src, runDir, "/data")
		out = rebasePath(out, runDir, "/data")
		if cover != "" {
			cover = rebasePath(cover, runDir, "/data")
		}
	}
	args := append([]string(nil), pre...)
	if st.PandocFrom != "" {
		args = append(args, "--from", st.PandocFrom)
	}
	if st.PandocTo != "" {
		args = append(args, "--to", st.PandocTo)
	}
	// Deterministic metadata-key order so the cache key (rendered argv)
	// doesn't oscillate run-to-run on Go map iteration.
	if len(st.PandocMetadata) > 0 {
		keys := make([]string, 0, len(st.PandocMetadata))
		for k := range st.PandocMetadata {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, "--metadata", k+"="+st.PandocMetadata[k])
		}
	}
	if cover != "" {
		args = append(args, "--epub-cover-image", cover)
	}
	args = append(args, st.PandocArgs...)
	args = append(args, "-o", out, src)
	return args
}

// rebasePath rewrites abs from a host prefix to a container prefix,
// preserving the trailing relative segment. Used only inside the docker
// fallback to convert host paths into container-internal /data paths.
// Inputs already live under runDir by construction (executor ensures
// it), so a path that doesn't match is a programmer error and we panic
// at runtime via an obvious empty-string result — caller validates the
// stat above.
func rebasePath(abs, hostPrefix, containerPrefix string) string {
	rel, err := filepath.Rel(hostPrefix, abs)
	if err != nil {
		return abs
	}
	return filepath.Join(containerPrefix, rel)
}

// pandocOutputExtension reports the file extension the pandoc stage
// produces — derived from PandocTo for the validator. Empty when no
// reliable mapping exists.
func pandocOutputExtension(to string) string {
	switch strings.ToLower(to) {
	case "epub", "epub3":
		return ".epub"
	case "pdf":
		return ".pdf"
	case "html", "html5":
		return ".html"
	case "docx":
		return ".docx"
	}
	return ""
}
