package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/gallowaysoftware/vibe/internal/vibe/daemon"
	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
	"github.com/gallowaysoftware/vibe/internal/vibeclient"
)

// Status is a check result level. WARN is informational only; FAIL is fatal
// for the doctor's exit code.
//
// UNKNOWN is the fourth, and it is not a severity: it says the check could
// not be EVALUATED, never that it passed and never that it failed. `vibe
// fleet doctor` has had it since C13 (with exit 3 meaning "the report is
// incomplete") and this command did not, so the rows that could not find
// out an answer picked one — a probe that spent its budget was rendered as
// "in use by another process", which is a definite claim about a machine
// nobody looked at. GR10: "not attempted" and "not possible" must never
// share a heading with "attempted, and here is the answer".
//
// It is used sparingly and on purpose. A row whose heading ADVISES (WARN,
// INFO) can carry "we could not tell" in its text and stay WARN; promoting
// those would turn a laptop with a slow `docker info` into a non-zero exit
// for no new information. UNKNOWN replaces a heading that ASSERTS A FAULT
// — the three rows below where a FAIL or a definite "not running" would
// otherwise be manufactured out of an unanswered probe.
type checkStatus int

const (
	statusOK checkStatus = iota
	statusWarn
	statusFail
	statusInfo
	statusUnknown
)

func (s checkStatus) tag() string {
	switch s {
	case statusOK:
		return "[ OK ]"
	case statusWarn:
		return "[WARN]"
	case statusFail:
		return "[FAIL]"
	case statusInfo:
		return "[INFO]"
	case statusUnknown:
		return "[UNKN]"
	default:
		return "[????]"
	}
}

// checkResult is the value returned by every doctor check. Keeping them
// uniform makes the runner trivial and each check unit-testable in isolation.
type checkResult struct {
	Name    string
	Status  checkStatus
	Message string
}

// runner is a small abstraction over exec.Command so individual checks can be
// unit-tested without depending on host binaries.
type runner func(ctx context.Context, name string, args ...string) ([]byte, error)

func realRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.Bytes(), err
}

// doctorEnv bundles the externals each check needs. It exists so tests can
// swap them out (the LookPath function, the runner, the daemon client).
type doctorEnv struct {
	lookPath func(string) (string, error)
	run      runner
	// statusFn returns a daemon status if the daemon is reachable on the
	// given TCP address, otherwise an error. Tests can stub this.
	statusFn func(ctx context.Context, addr string) (string, error)
	// daemonAddr is the host:port the daemon's TCP control plane lives on.
	daemonAddr string
}

func defaultDoctorEnv() *doctorEnv {
	return &doctorEnv{
		lookPath:   exec.LookPath,
		run:        realRunner,
		statusFn:   defaultDaemonStatus,
		daemonAddr: "127.0.0.1:9001",
	}
}

// doctorPingBudget is how long the control-plane row waits for whatever
// holds :9001 to identify itself.
//
// It is ONE number. It was two: the caller opened a 1s context and
// defaultDaemonStatus set a 500ms http.Client.Timeout, so the effective
// budget was half what the code appeared to say and the smaller one was
// the invisible one. A refusal cannot quote a budget it does not know, and
// a reader tuning the visible number would have changed nothing.
//
// 1s — the number the code already claimed — rather than the CLI's 500ms
// `pingBudget` (client.go), because this probe is not the same probe: it
// crosses a TCP listener rather than the unix socket, and it runs inside a
// command that already spends five seconds on `llama-server --version`
// alone. Nor is it larger, now that a spent budget is reported as a spent
// budget: an UNKNOWN row costs a re-run, whereas a longer wait costs every
// operator running the command with a wedged port-holder on the box.
const doctorPingBudget = 1 * time.Second

// defaultDaemonStatus calls vibeclient.Status over a plain TCP connect. It
// returns the active profile name (or "" if no profile is active) on success.
//
// The CONTEXT is the budget — there is no second one on the client. A
// deadline is applied here only when the caller supplied none, so this can
// never hang the report; the one production caller always sets one.
func defaultDaemonStatus(ctx context.Context, addr string) (string, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, doctorPingBudget)
		defer cancel()
	}
	hc := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "tcp", addr)
			},
		},
	}
	// Probe runs locally against the daemon's TCP listener; resolve the
	// token from $VIBE_TOKEN or the on-disk file so a bind_all daemon
	// doesn't fail this check with 401.
	c := vibeclient.NewWithHTTPClient("http://"+addr, hc, vibeclient.ResolveToken())
	s, err := c.Status(ctx)
	if err != nil {
		return "", err
	}
	if !s.Running {
		return "", nil
	}
	return s.Profile, nil
}

func doctorCmd() *cobra.Command {
	var (
		installName   string
		installYes    bool
		installCUDA   bool
		installMethod string
		installUpdate bool
		pipelinePath  string
	)
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Verify this machine is ready to run vibe.",
		Long: "Runs a series of diagnostic checks (binaries, directories, ports, " +
			"profiles, daemon) and reports OK / WARN / FAIL / UNKN for each. " +
			"UNKN means the check could not be evaluated — never that it passed, " +
			"and never that the thing it probes is absent.\n\n" +
			"Exit codes: 0 all clear, 1 a FAIL, 3 only UNKNs (the report is " +
			"incomplete — re-run). WARN does not affect the exit status.\n\n" +
			"With --install <name>, switches from diagnostic to install mode " +
			"and runs the install procedure for that component (supported: " +
			"`comfyui`, `llama-cpp`). Each step is idempotent and skips when " +
			"already satisfied; big-disk steps prompt for confirmation unless " +
			"--yes is passed. With --install llama-cpp --cuda, prefers a " +
			"CUDA-flavoured release asset (and points at the CUDA source " +
			"build command in the [s]ource menu).\n\n" +
			"With --pipeline <binary>, switches to pipeline-preflight mode: " +
			"execs the binary's `requirements --format json` subcommand and " +
			"verifies every declared external service is reachable, printing " +
			"the pipeline-author-supplied setup command for any that fail.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if installName != "" {
				return runInstall(cmd, installName, installYes, installCUDA, installMethod, installUpdate)
			}
			if pipelinePath != "" {
				if err := runPipelineDoctor(ctx, pipelinePath, cmd.OutOrStdout()); err != nil {
					if errors.Is(err, errDoctorFailed) {
						cmd.SilenceErrors = true
					}
					return err
				}
				return nil
			}
			env := defaultDoctorEnv()
			results := runChecks(ctx, env)
			out := cmd.OutOrStdout()
			for _, r := range results {
				fmt.Fprintf(out, "%s %-28s %s\n", r.Status.tag(), r.Name, r.Message)
			}
			if err := doctorOutcome(results); err != nil {
				// Suppress cobra's "Error: ..." wrap; the per-line output is
				// already self-explanatory.
				cmd.SilenceErrors = true
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&pipelinePath, "pipeline", "",
		"preflight-check a pipeline binary's declared services + capabilities (path to the binary). Mutually exclusive with --install.")
	cmd.Flags().StringVar(&installName, "install", "",
		"install a component (supported: comfyui, llama-cpp) instead of running diagnostics")
	cmd.Flags().BoolVar(&installYes, "yes", false,
		"skip confirmation prompts when used with --install")
	cmd.Flags().BoolVar(&installCUDA, "cuda", false,
		"prefer CUDA-flavoured assets when used with --install llama-cpp")
	cmd.Flags().StringVar(&installMethod, "method", "",
		"with --install llama-cpp, skip the interactive prompt and pick "+
			"a method directly: distro | release | source. Source builds "+
			"from upstream master so you get features (e.g. MTP) before "+
			"they reach a release tag.")
	cmd.Flags().BoolVar(&installUpdate, "update", false,
		"refresh an existing install instead of short-circuiting on the "+
			"\"already present\" check. With --method release: re-fetches "+
			"the latest GitHub release tarball and re-extracts. With "+
			"--method source: git fetch + reset --hard origin/master + "+
			"incremental cmake build (already idempotent, but the flag "+
			"makes intent explicit).")
	cmd.MarkFlagsMutuallyExclusive("install", "pipeline")

	// Tab-complete the enum flags so users discover the allowed values
	// without consulting --help — same pattern as `vibe profile new`.
	_ = cmd.RegisterFlagCompletionFunc("install",
		func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			return []string{"comfyui", "llama-cpp"}, cobra.ShellCompDirectiveNoFileComp
		})
	_ = cmd.RegisterFlagCompletionFunc("method",
		func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			return []string{"distro", "release", "source"}, cobra.ShellCompDirectiveNoFileComp
		})
	return cmd
}

// runInstall dispatches to the named installer. New installers plug in here
// without touching the diagnostic path.
func runInstall(cmd *cobra.Command, name string, yes, cuda bool, method string, update bool) error {
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()
	switch name {
	case "comfyui":
		if update {
			fmt.Fprintln(errOut, "[warn] --update has no effect for comfyui; ignored")
		}
		return installComfyUI(defaultInstallerEnv(out, errOut, yes))
	case "llama-cpp":
		env := defaultLlamaInstallerEnv(out, errOut, yes, cuda, update)
		switch method {
		case "":
			// honor the interactive prompt (or default to release when --yes)
		case "distro", "release", "source":
			env.forcedMethod = llamaInstallMethod(method[:1])
		default:
			return fmt.Errorf("--method %q: unknown (allowed: distro, release, source)", method)
		}
		return installLlamaCpp(env)
	default:
		cmd.SilenceErrors = true
		return fmt.Errorf("--install %q: unknown component (supported: comfyui, llama-cpp)", name)
	}
}

var errDoctorFailed = errors.New("doctor: one or more checks failed")

// doctorOutcome turns a finished report into the process's exit status.
//
// 1 for a FAIL and 3 for "the report is incomplete" — the same two facts
// `vibe fleet doctor` has separated since C13, and the reason UNKNOWN is
// worth a level rather than just a nicer sentence: a wrapper reading the
// status can tell "this box is broken" from "I could not find out", which
// is the whole distinction this command was collapsing.
//
// A FAIL outranks an UNKNOWN: a report with both has something definite in
// it and that is what the operator should be sent to first.
//
// No run's exit status gets WORSE than it was. Every row that can now
// report UNKNOWN previously reported FAIL from the same evidence, so what
// used to exit 1 exits 3 and what exited 0 still exits 0. UNKNOWN is
// deliberately not handed to the advisory rows for exactly that reason —
// promoting a WARN would turn a green box red on no new information.
func doctorOutcome(results []checkResult) error {
	anyUnknown := false
	for _, r := range results {
		switch r.Status {
		case statusFail:
			return errDoctorFailed
		case statusUnknown:
			anyUnknown = true
		}
	}
	if anyUnknown {
		return errDoctorLevel{doctorExitUnknown}
	}
	return nil
}

// runChecks executes each check in order and returns the results.
func runChecks(ctx context.Context, env *doctorEnv) []checkResult {
	// State that flows between checks: what the :9001 probe established
	// about the daemon, and the daemon's reported active profile.
	type sharedState struct {
		daemon        daemonPresence
		daemonProfile string
	}
	st := &sharedState{}

	results := []checkResult{}

	// One declaration scan, two checks. Both are about the same binary, so
	// a box that declares nothing needing it must see neither — a FAIL on
	// the first and a "skipped" WARN on the second are the same mis-fire.
	llamaUsers := llamaServerUsers(paths.ProfilesDir(), paths.BackendsDir())
	if r, ok := checkLlamaBinary(env, llamaUsers); ok {
		results = append(results, r)
	}
	if r, ok := checkLlamaVersion(ctx, env, llamaUsers); ok {
		results = append(results, r)
	}
	results = append(results, checkHFBinary(env))
	results = append(results, checkHFAuth(ctx, env))
	results = append(results, checkXDGDirs())

	// Control-plane port 9001 first (so we know if a vibe daemon is up).
	cp := checkControlPlanePort(ctx, env, &st.daemon, &st.daemonProfile)
	results = append(results, cp)

	// Then the proxy port 9000 (also probed against the daemon).
	results = append(results, checkProxyPort(st.daemon, proxyDisabledInConfig()))

	results = append(results, checkProfiles())
	if r, ok := checkBackends(paths.ProfilesDir()); ok {
		results = append(results, r)
	}
	results = append(results, checkFrontendState())
	results = append(results, checkCommonPorts())
	results = append(results, checkMCPs())
	if r, ok := checkRSVGForVision(env, paths.ProfilesDir()); ok {
		results = append(results, r)
	}
	if r, ok := checkDockerForProfiles(ctx, env, paths.ProfilesDir()); ok {
		results = append(results, r)
	}

	results = append(results, daemonRow(st.daemon, st.daemonProfile))

	results = append(results, checkGPU(ctx, env))

	return results
}

// ─── individual checks ──────────────────────────────────────────────────────

// budgets for the external commands the checks shell out to. Named
// because the rows below quote them: a row that says "did not finish
// within 5s" has to be reading the same number it waited.
const (
	llamaVersionBudget = 5 * time.Second
	hfAuthBudget       = 10 * time.Second
	dockerInfoBudget   = 3 * time.Second
	nvidiaSMIBudget    = 3 * time.Second
)

// runTimedOut describes OUR budget ending a command, and returns "" when
// the command failed on its own. It is the same discrimination
// classifyControlProbe makes, for the four rows that shell out.
//
// The context has to be asked, not the error: exec.CommandContext kills
// the child and reports `signal: killed`, and — measured on this box —
// errors.Is(err, context.DeadlineExceeded) is FALSE for it. So every row
// below rendered our own timeout as a claim about the tool. A wedged
// nvidia-smi, which is the classic symptom of a GPU in a bad state and
// hangs rather than exits, read as "nvidia-smi failed: signal: killed";
// a docker daemon merely slow to answer read as "(daemon not running?)".
//
// These stay WARN either way. A row whose heading advises can carry the
// distinction in its text; see the note on statusUnknown for why they are
// not promoted.
func runTimedOut(ctx context.Context, budget time.Duration) string {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "did not finish within " + budget.String()
	case ctx.Err() != nil:
		return "was cancelled before it finished"
	default:
		return ""
	}
}

// checkLlamaBinary fails when llama-server is missing from $PATH AND
// something on disk would spawn it from $PATH. The second half is the
// point: this check used to fail unconditionally, so a box whose only
// profiles are cloud_peer — a laptop pointed at a peer through a remote
// front, supported since PR #14 — exited non-zero over a binary it will
// never invoke. The same mis-fire hit comfyui-only and mlx-only boxes,
// which is why the fix keys on "what is declared" rather than on any one
// backend kind.
//
// Same shape as checkRSVGForVision and checkDockerForProfiles: ok=false
// means not-applicable and the runner appends nothing.
//
// The name stays "llama-server" under C13's rule (a check is named for what
// it proves) because what it proves has not changed — llama-server is on
// $PATH — only whether it runs at all. `docker` and `rsvg-convert (vision)`
// are named the same way, after the tool, with applicability carried by the
// ok=false gate rather than by the name.
func checkLlamaBinary(env *doctorEnv, users []string) (checkResult, bool) {
	const name = "llama-server"
	if len(users) == 0 {
		return checkResult{}, false
	}
	needs := "needed by " + strings.Join(users, ", ")
	path, err := env.lookPath("llama-server")
	if err != nil {
		return checkResult{
			Name:    name,
			Status:  statusFail,
			Message: "not found on $PATH (" + needs + ") — install llama.cpp (https://github.com/ggml-org/llama.cpp)",
		}, true
	}
	return checkResult{Name: name, Status: statusOK, Message: path + " (" + needs + ")"}, true
}

// llamaServerUsers names the profile and backend definitions on disk that
// would spawn `llama-server` off $PATH, sorted and deduplicated. Empty
// means nothing on this box needs the binary.
//
// A definition that pins its own `binary:` is deliberately NOT a user: it
// names an absolute path and never consults $PATH, so a fleet box running a
// custom build must not be failed over a binary nothing looks for.
//
// Declaration is read straight from the YAML rather than through
// profile.Load, which is what every other scan here does. The difference
// matters for this check alone: profile.Load VALIDATES, and a llama_server
// profile whose GGUF has not been pulled yet fails to load — which under a
// Load-based scan would silently make this check not-applicable on exactly
// the box that most needs it. What is asked here is what the file DECLARES,
// and that is answerable without the model being on disk. Unparseable files
// are skipped; the `profiles` and `backends` checks surface those.
func llamaServerUsers(profilesDir, backendsDir string) []string {
	var out []string
	scan := func(dir, prefix string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			var decl struct {
				Backend struct {
					LlamaServer *struct {
						Binary string `yaml:"binary"`
					} `yaml:"llama_server"`
				} `yaml:"backend"`
			}
			if err := yaml.Unmarshal(raw, &decl); err != nil {
				continue
			}
			if decl.Backend.LlamaServer == nil || decl.Backend.LlamaServer.Binary != "" {
				continue
			}
			out = append(out, prefix+strings.TrimSuffix(e.Name(), ".yaml"))
		}
	}
	scan(backendsDir, "backend ")
	scan(profilesDir, "profile ")
	sort.Strings(out)
	return slices.Compact(out)
}

// checkLlamaVersion is gated on the same declaration scan as
// checkLlamaBinary: a box that never spawns llama-server has no version to
// report, and warning "skipped (not on $PATH)" there is the same mis-fire
// one line down. Gating one and not the other is how a guard ends up on
// some of its call paths.
func checkLlamaVersion(ctx context.Context, env *doctorEnv, users []string) (checkResult, bool) {
	if len(users) == 0 {
		return checkResult{}, false
	}
	if _, err := env.lookPath("llama-server"); err != nil {
		return checkResult{
			Name:    "llama-server --version",
			Status:  statusWarn,
			Message: "skipped (llama-server not on $PATH)",
		}, true
	}
	cctx, cancel := context.WithTimeout(ctx, llamaVersionBudget)
	defer cancel()
	out, err := env.run(cctx, "llama-server", "--version")
	version := firstNonEmptyLine(out)
	if err != nil {
		// `llama-server --version` is known to write to stderr and may exit
		// non-zero on some builds; report whatever we got but mark WARN.
		// "exited non-zero" is a claim about the BINARY, so it is not made
		// when what happened is that we stopped waiting for it.
		why := "exited non-zero: " + err.Error()
		if to := runTimedOut(cctx, llamaVersionBudget); to != "" {
			why = to
		}
		if version == "" {
			return checkResult{
				Name:    "llama-server --version",
				Status:  statusWarn,
				Message: why,
			}, true
		}
		return checkResult{
			Name:    "llama-server --version",
			Status:  statusWarn,
			Message: version + " (" + why + ")",
		}, true
	}
	if version == "" {
		return checkResult{
			Name:    "llama-server --version",
			Status:  statusWarn,
			Message: "no output",
		}, true
	}
	return checkResult{Name: "llama-server --version", Status: statusOK, Message: version}, true
}

func checkHFBinary(env *doctorEnv) checkResult {
	path, err := env.lookPath("hf")
	if err != nil {
		return checkResult{
			Name:    "hf",
			Status:  statusWarn,
			Message: "not found — public HF repos still work via native HTTP; install `hf` for gated repos",
		}
	}
	return checkResult{Name: "hf", Status: statusOK, Message: path}
}

func checkHFAuth(ctx context.Context, env *doctorEnv) checkResult {
	if _, err := env.lookPath("hf"); err != nil {
		return checkResult{
			Name:    "hf auth",
			Status:  statusWarn,
			Message: "skipped (hf not on $PATH)",
		}
	}
	cctx, cancel := context.WithTimeout(ctx, hfAuthBudget)
	defer cancel()
	out, err := env.run(cctx, "hf", "auth", "whoami")
	text := string(out)
	// First, because this is the one failure that is EXPECTED to exit
	// non-zero: `hf auth whoami` says so and returns 1.
	if strings.Contains(text, "Not logged in") {
		return checkResult{
			Name:    "hf auth",
			Status:  statusWarn,
			Message: "not logged in — gated repos require `hf auth login`",
		}
	}
	// The error used to be discarded outright, and firstNonEmptyLine of a
	// FAILED run went straight into "logged in as …". Measured: a command
	// that prints a traceback and exits 1 rendered as
	// `[ OK ] hf auth  logged in as Traceback (most recent call last):`.
	// An OK is the strongest claim in this report and it was being made
	// out of an error message.
	if err != nil {
		why := "failed: " + err.Error()
		if to := runTimedOut(cctx, hfAuthBudget); to != "" {
			why = to
		} else if first := firstNonEmptyLine(out); first != "" {
			why += " (" + first + ")"
		}
		return checkResult{
			Name:    "hf auth",
			Status:  statusWarn,
			Message: "`hf auth whoami` " + why + " — cannot tell whether this box can reach gated repos",
		}
	}
	user := firstNonEmptyLine(out)
	if user == "" {
		return checkResult{
			Name:    "hf auth",
			Status:  statusWarn,
			Message: "no output from `hf auth whoami`",
		}
	}
	return checkResult{Name: "hf auth", Status: statusOK, Message: "logged in as " + user}
}

func checkXDGDirs() checkResult {
	if err := paths.EnsureDirs(); err != nil {
		return checkResult{
			Name:    "xdg dirs",
			Status:  statusFail,
			Message: "EnsureDirs: " + err.Error(),
		}
	}
	dirs := []struct {
		label, path string
	}{
		{"config", paths.ConfigHome()},
		{"state", paths.StateHome()},
		{"runtime", paths.RuntimeDir()},
		{"logs", paths.LogDir()},
		{"profiles", paths.ProfilesDir()},
		{"mcp", paths.MCPDir()},
	}
	var bad []string
	for _, d := range dirs {
		if err := probeDirWritable(d.path); err != nil {
			bad = append(bad, fmt.Sprintf("%s (%s): %v", d.label, d.path, err))
		}
	}
	if len(bad) > 0 {
		return checkResult{
			Name:    "xdg dirs",
			Status:  statusFail,
			Message: strings.Join(bad, "; "),
		}
	}
	return checkResult{
		Name:   "xdg dirs",
		Status: statusOK,
		Message: fmt.Sprintf("%s, %s writable",
			paths.ConfigHome(), paths.StateHome()),
	}
}

// probeDirWritable verifies path is an existing directory we can write to.
// Uses os.OpenFile for a probe write+delete (stdlib only, no unix.Access).
func probeDirWritable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("not a directory")
	}
	probe, err := os.CreateTemp(path, ".vibe-doctor-*")
	if err != nil {
		return err
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
}

// daemonPresence is what the :9001 probe ESTABLISHED about the local
// daemon, as opposed to what a bool let two later rows assume.
//
// The zero value is "not established", which is the entire reason this is
// not a bool. The bool started false, stayed false down every path that
// failed to FIND OUT — a spent budget, a holder that answered and refused
// this box's credential — and two rows downstream read that false as the
// definite fact "there is no vibe daemon". One unanswered probe therefore
// produced three claims: a FAIL saying :9001 was held by another process,
// `daemon — not running`, and a FAIL saying the healthy proxy port had
// been stolen. That is daemonAbsent's class (client.go) one transport
// over, and it lands on the one command an operator runs BECAUSE they
// already suspect something is wrong.
type daemonPresence int

const (
	// daemonPresenceUnknown is the zero value on purpose: a path that
	// learns nothing must leave nothing behind.
	daemonPresenceUnknown daemonPresence = iota
	daemonPresenceRunning
	daemonPresenceStopped
)

// controlProbe sorts a FAILED status probe into the three different things
// it can mean once we already know the port is held.
type controlProbe int

const (
	probeNoAnswer  controlProbe = iota // the budget was spent
	probeRefusedUs                     // something answered, and refused our credential
	probeNotVibe                       // something answered, and it does not speak vibe's control plane
)

// classifyControlProbe is this file's daemonAbsent — the same shape (ask
// the error what it is instead of collapsing it), a different question,
// and deliberately NOT a call to daemonAbsent itself:
//
//   - the question differs. daemonAbsent asks "is there NO daemon?", which
//     on this path is already answered: the bind failed with
//     address-in-use, so something holds :9001. What is unknown here is
//     WHO, and daemonAbsent's answer collapses "did not answer" and
//     "refused us" into one not-absent bucket.
//   - reusing it would be actively wrong. Measured: a plain HTTP 503 from
//     a non-vibe holder arrives as connect.CodeUnavailable, which is
//     exactly what daemonAbsent reads as proof of absence — on the one
//     path where absence has already been ruled out.
//
// Deadline first, and by errors.Is before the connect code, so a stubbed
// statusFn or a future non-connect transport still lands on "we do not
// know" rather than on a claim.
func classifyControlProbe(err error) controlProbe {
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return probeNoAnswer
	case connect.CodeOf(err) == connect.CodeDeadlineExceeded:
		return probeNoAnswer
	case connect.CodeOf(err) == connect.CodeUnauthenticated:
		return probeRefusedUs
	default:
		return probeNotVibe
	}
}

func checkControlPlanePort(ctx context.Context, env *doctorEnv, presence *daemonPresence, daemonProfile *string) checkResult {
	return checkControlPlanePortAt(ctx, env, "127.0.0.1:9001", presence, daemonProfile)
}

// checkControlPlanePortAt is the testable core, split out for the same
// reason checkProxyPortAt was: the address was hardcoded, so the row that
// makes the most consequential claim in the report was the one row no test
// could drive end to end. The old test said so in a comment and asserted
// the "composable inputs" instead.
func checkControlPlanePortAt(ctx context.Context, env *doctorEnv, addr string, presence *daemonPresence, daemonProfile *string) checkResult {
	name := "control-plane port " + strings.TrimPrefix(addr, "127.0.0.1")
	free, err := tryBind(addr)
	if free {
		// The one branch that PROVES absence: we just held the port
		// ourselves, so nothing was listening on it.
		*presence = daemonPresenceStopped
		return checkResult{Name: name, Status: statusOK, Message: "free"}
	}
	if !isAddrInUse(err) {
		// Some other listen failure (permissions, bad iface). Report it —
		// and leave presence unknown: a bind we were not allowed to attempt
		// is not evidence about who holds the port.
		return checkResult{Name: name, Status: statusFail, Message: err.Error()}
	}
	// Port is bound; figure out whether the holder is a vibe daemon.
	cctx, cancel := context.WithTimeout(ctx, doctorPingBudget)
	defer cancel()
	profileName, sErr := env.statusFn(cctx, env.daemonAddr)
	if sErr == nil {
		*presence = daemonPresenceRunning
		*daemonProfile = profileName
		return checkResult{
			Name:    name,
			Status:  statusOK,
			Message: "vibe daemon already running",
		}
	}
	switch classifyControlProbe(sErr) {
	case probeNoAnswer:
		// A daemon holding a model and serving the proxy can miss a
		// one-second window with nothing whatever wrong. Saying "another
		// process" here sends the operator hunting a port conflict that
		// does not exist, on the command they opened because they already
		// believed something was broken.
		return checkResult{
			Name:   name,
			Status: statusUnknown,
			Message: fmt.Sprintf("in use, holder unidentified: no answer within %s (%v). "+
				"NOT evidence of a port conflict — a busy vibe daemon looks like this; re-run to retry", doctorPingBudget, sErr),
		}
	case probeRefusedUs:
		// It answered. It is a control plane, and it refused US. The fix
		// is a credential, not a port — the same distinction `vibe fleet
		// doctor` draws for a cell that answers and rejects the key (C15),
		// where reporting "no answer" sends the operator to the wrong box.
		return checkResult{
			Name:   name,
			Status: statusUnknown,
			Message: "in use, and the holder refused this box's credential — a vibe daemon started with a token " +
				"this shell cannot resolve looks exactly like this. Check $VIBE_TOKEN or " + paths.TokenFile(),
		}
	default:
		// Something is listening and it does not speak vibe's control
		// plane. This is the definite claim and it keeps its FAIL — and,
		// with it, the definite presence: the daemon binds :9001 at
		// startup and cannot start when the port is taken, so a holder
		// that is not a vibe control plane means no vibe daemon is
		// serving on this box. Leaving presence unknown here would buy
		// the fix's honesty by going vague about a real double conflict,
		// which is the row's whole reason to exist.
		*presence = daemonPresenceStopped
		return checkResult{
			Name:    name,
			Status:  statusFail,
			Message: "in use by another process (no vibe daemon answered: " + sErr.Error() + ")",
		}
	}
}

// daemonRow renders the daemon's own line from what the control-plane
// probe established. Split out from runChecks so the three-way outcome is
// testable without a live :9001.
func daemonRow(presence daemonPresence, profile string) checkResult {
	const name = "daemon"
	switch presence {
	case daemonPresenceRunning:
		msg := "running, no active profile"
		if profile != "" {
			msg = "running, active profile: " + profile
		}
		return checkResult{Name: name, Status: statusOK, Message: msg}
	case daemonPresenceStopped:
		return checkResult{Name: name, Status: statusInfo, Message: "not running"}
	default:
		return checkResult{
			Name:   name,
			Status: statusUnknown,
			Message: "could not tell — the control-plane row above says why. " +
				"`not running` is a claim, and nothing here supports it",
		}
	}
}

// proxyDisabledInConfig reports whether the daemon config opts out of the
// reverse proxy (disable_proxy: an external router owns :9000). A config
// read error degrades to false so the enabled-path messaging — the safe
// default — applies.
func proxyDisabledInConfig() bool {
	cfg, err := daemon.LoadConfig()
	return err == nil && cfg.DisableProxy
}

func checkProxyPort(presence daemonPresence, proxyDisabled bool) checkResult {
	return checkProxyPortAt("127.0.0.1:9000", presence, proxyDisabled)
}

// checkProxyPortAt takes a daemonPresence rather than a bool so that a
// caller cannot pass "we did not find out" as "there is no daemon" — which
// is exactly what it received before, and why a healthy :9000 was reported
// stolen whenever :9001 was merely slow.
func checkProxyPortAt(addr string, presence daemonPresence, proxyDisabled bool) checkResult {
	name := "proxy port " + strings.TrimPrefix(addr, "127.0.0.1")
	free, err := tryBind(addr)
	if !free && !isAddrInUse(err) {
		// Some other listen failure (permissions, bad iface). Report it.
		return checkResult{Name: name, Status: statusFail, Message: err.Error()}
	}
	if proxyDisabled {
		// disable_proxy hands the proxy port to an external router
		// (llama-swap), so the port being held is the healthy state here,
		// not a conflict.
		if free {
			return checkResult{
				Name:    name,
				Status:  statusWarn,
				Message: "free, but disable_proxy is set — external router not listening yet",
			}
		}
		return checkResult{
			Name:    name,
			Status:  statusOK,
			Message: "in use (disable_proxy set; external router expected here)",
		}
	}
	if free {
		return checkResult{Name: name, Status: statusOK, Message: "free"}
	}
	switch presence {
	case daemonPresenceRunning:
		return checkResult{
			Name:    name,
			Status:  statusOK,
			Message: "already bound by vibe daemon",
		}
	case daemonPresenceStopped:
		return checkResult{
			Name:    name,
			Status:  statusFail,
			Message: "in use by another process (no vibe daemon detected on :9001)",
		}
	default:
		// The port is held and we could not attribute it, because the
		// thing that would have attributed it — :9001 — did not answer.
		// The likeliest holder is the vibe daemon's own proxy, which is
		// what makes the FAIL this used to print so expensive: it names a
		// conflict, on the healthy port, in the report an operator opened
		// to find one.
		return checkResult{
			Name:   name,
			Status: statusUnknown,
			Message: "in use, holder unattributed: :9001 did not identify itself (see the control-plane row), " +
				"so this may well be vibe's own proxy",
		}
	}
}

// checkCommonPorts probes the loopback ports the profiles and backends on
// this machine actually declare and reports any that are already bound.
// Bound != bad — the bind could be one of vibe's own frontends still
// running from a previous session, or some unrelated service — so the
// result stays INFO, never FAIL. Goal is just to make conflicts visible
// BEFORE a `vibe start` runs into them. The 9000/9001 checks above cover
// the vibe-owned ports specifically; this check covers the next tier.
//
// The probe list is derived from disk (rather than hardcoded) so it tracks
// the real inventory as profiles are renamed or archived. A small fixed
// tail covers compose-managed ports that never appear in profile YAML
// (Open WebUI's in-container default 8080).
func checkCommonPorts() checkResult {
	ports := declaredPorts()
	if _, ok := ports[8080]; !ok {
		ports[8080] = []string{"Open WebUI (compose default)"}
	}
	return checkCommonPortsWith(ports, tryBind)
}

// checkCommonPortsWith is the testable core, taking the inventory and the
// probe rather than reading the one from disk and the other from the host.
//
// The third bucket is the fix. `isAddrInUse` has three call sites and this
// was the one that never asked it: `if ok, _ := tryBind(...); ok` discards
// the error, so EVERY listen failure became "in use". Measured: a declared
// loopback port under 1024 fails with `bind: permission denied` as an
// ordinary user, and this row reported it as a port conflict — on the
// check whose entire job is telling an operator which ports are taken.
// Both siblings above already separate in-use from could-not-attempt.
func checkCommonPortsWith(ports map[int][]string, probe func(string) (bool, error)) checkResult {
	const name = "common ports"
	nums := make([]int, 0, len(ports))
	for port := range ports {
		nums = append(nums, port)
	}
	sort.Ints(nums)
	var bound, free, unprobed []string
	for _, port := range nums {
		label := fmt.Sprintf(":%d (%s)", port, strings.Join(ports[port], ", "))
		switch ok, err := probe(fmt.Sprintf("127.0.0.1:%d", port)); {
		case ok:
			free = append(free, strconv.Itoa(port))
		case isAddrInUse(err):
			bound = append(bound, label)
		default:
			unprobed = append(unprobed, fmt.Sprintf("%s: %v", label, err))
		}
	}
	if len(bound) == 0 && len(unprobed) == 0 {
		return checkResult{Name: name, Status: statusOK, Message: strings.Join(free, "/") + " free"}
	}
	var parts []string
	if len(bound) > 0 {
		parts = append(parts, "in use: "+strings.Join(bound, ", "))
	}
	if len(unprobed) > 0 {
		// Not folded into "in use": whether anything holds these is
		// exactly what was not established. An OK would be as wrong.
		parts = append(parts, "could not probe (bind refused, so nothing is known about the holder): "+
			strings.Join(unprobed, ", "))
	}
	return checkResult{
		Name:    name,
		Status:  statusInfo,
		Message: strings.Join(parts, "; "),
	}
}

// declaredPorts collects every loopback port the on-disk backends and
// profiles declare, keyed to the declaring file(s). Since the backend_ref
// split, backend port fields mostly live in backends/*.yaml; profiles
// contribute inline backend ports plus the loopback wait_for / url entries
// on compose frontends (the only place a compose-published port is written
// down). Files that fail to parse are skipped — the profiles/backends
// checks surface those; doctor must not hard-fail on one bad file.
func declaredPorts() map[int][]string {
	ports := map[int][]string{}
	add := func(port int, label string) {
		if port <= 0 || slices.Contains(ports[port], label) {
			return
		}
		ports[port] = append(ports[port], label)
	}
	collect := func(label string, b profile.Backend) {
		switch {
		case b.LlamaServer != nil:
			add(b.LlamaServer.Port, label)
		case b.ComfyUI != nil:
			add(b.ComfyUI.Port, label)
		case b.HTTPServer != nil:
			add(b.HTTPServer.Port, label)
		case b.TabbyAPI != nil:
			add(b.TabbyAPI.Port, label)
		case b.MLXServer != nil:
			add(b.MLXServer.Port, label)
		}
	}
	if names, err := profile.ListBackends(); err == nil {
		for _, n := range names {
			def, err := profile.LoadBackend(n)
			if err != nil {
				continue
			}
			collect("backend "+n, def.Backend)
		}
	}
	dir := paths.ProfilesDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ports
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		p, err := profile.Load(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		label := "profile " + strings.TrimSuffix(e.Name(), ".yaml")
		// A referenced backend's port is already labeled under the backend's
		// own name; only inline backends contribute under the profile label.
		if p.BackendRef == "" {
			collect(label, p.Backend)
		}
		for _, w := range p.Frontend.WaitFor {
			add(loopbackPort(w.URL), label)
		}
		add(loopbackPort(p.Frontend.URL), label)
	}
	return ports
}

// loopbackPort extracts the port from a loopback URL. Non-loopback hosts
// return 0 — probing a bind on a port some remote URL happens to use would
// report phantom conflicts.
func loopbackPort(raw string) int {
	u, err := url.Parse(raw)
	if err != nil {
		return 0
	}
	switch u.Hostname() {
	case "127.0.0.1", "localhost", "::1", "0.0.0.0":
	default:
		return 0
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		return 0
	}
	return port
}

// tryBind attempts a one-shot TCP listen on addr and closes the listener
// immediately. Returns (true, nil) on success; (false, err) on any failure.
func tryBind(addr string) (bool, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return false, err
	}
	_ = ln.Close()
	return true, nil
}

func isAddrInUse(err error) bool {
	if err == nil {
		return false
	}
	// We don't depend on syscall.EADDRINUSE so we stay portable; the
	// canonical error text from net.OpError contains "address already in
	// use" on linux/darwin/bsd.
	return strings.Contains(err.Error(), "address already in use") ||
		strings.Contains(err.Error(), "Only one usage of each socket address") // Windows
}

func checkProfiles() checkResult {
	dir := paths.ProfilesDir()
	return checkProfilesAt(dir)
}

// checkProfilesAt is the testable core of checkProfiles. Reads *.yaml files
// from `dir`, validates each with profile.Load, and reports the count.
func checkProfilesAt(dir string) checkResult {
	const name = "profiles"
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return checkResult{Name: name, Status: statusWarn, Message: "no profiles dir at " + dir}
		}
		return checkResult{Name: name, Status: statusFail, Message: err.Error()}
	}
	var (
		valid    []string
		invalids []string
	)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		p, err := profile.Load(path)
		if err != nil {
			invalids = append(invalids, fmt.Sprintf("%s (%v)", e.Name(), err))
			continue
		}
		valid = append(valid, p.Name)
	}
	sort.Strings(valid)
	if len(valid) == 0 && len(invalids) == 0 {
		return checkResult{Name: name, Status: statusWarn, Message: "no profiles in " + dir}
	}
	if len(valid) == 0 {
		return checkResult{
			Name:    name,
			Status:  statusFail,
			Message: fmt.Sprintf("0 valid; %d invalid: %s", len(invalids), strings.Join(invalids, "; ")),
		}
	}
	msg := fmt.Sprintf("%d valid (%s)", len(valid), strings.Join(valid, ", "))
	status := statusOK
	if len(invalids) > 0 {
		msg += fmt.Sprintf("; %d invalid: %s", len(invalids), strings.Join(invalids, "; "))
		status = statusWarn
	}
	return checkResult{Name: name, Status: status, Message: msg}
}

// checkBackends mirrors checkProfilesAt for backends/<name>.yaml. A broken
// backend that a profile references already fails through that profile's
// row in checkProfilesAt (backend_ref resolution is eager in profile.Load),
// so this check's distinct value is backends nothing references —
// capability-only targets that would otherwise surface only mid-pipeline
// as a vamp stage failure. Backends are optional, so the check is skipped
// (ok=false) when none are defined.
func checkBackends(profilesDir string) (checkResult, bool) {
	const name = "backends"
	names, err := profile.ListBackends()
	if err != nil {
		return checkResult{Name: name, Status: statusWarn, Message: err.Error()}, true
	}
	if len(names) == 0 {
		return checkResult{}, false
	}
	referenced := backendRefsIn(profilesDir)
	var valid, invalids, capOnly []string
	for _, n := range names {
		if _, err := profile.LoadBackend(n); err != nil {
			invalids = append(invalids, fmt.Sprintf("%s (%v)", n, err))
			continue
		}
		valid = append(valid, n)
		if len(referenced[n]) == 0 {
			capOnly = append(capOnly, n)
		}
	}
	if len(valid) == 0 {
		return checkResult{
			Name:    name,
			Status:  statusFail,
			Message: fmt.Sprintf("0 valid; %d invalid: %s", len(invalids), strings.Join(invalids, "; ")),
		}, true
	}
	msg := fmt.Sprintf("%d valid (%s)", len(valid), strings.Join(valid, ", "))
	status := statusOK
	if len(invalids) > 0 {
		msg += fmt.Sprintf("; %d invalid: %s", len(invalids), strings.Join(invalids, "; "))
		status = statusWarn
	}
	if len(capOnly) > 0 {
		msg += "; no backend_ref points here (capability-only or stale): " + strings.Join(capOnly, ", ")
	}
	return checkResult{Name: name, Status: status, Message: msg}, true
}

// checkFrontendState surfaces where per-profile frontend state lives so
// users can find their Open WebUI db, opencode config, etc. without
// spelunking through container volumes or grepping CLAUDE.md. The dir is
// created lazily on profile activation; missing-here is fine.
func checkFrontendState() checkResult {
	const name = "frontend state"
	root := paths.FrontendStateRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return checkResult{Name: name, Status: statusInfo, Message: root + " (not yet created — populated on first profile start)"}
		}
		return checkResult{Name: name, Status: statusWarn, Message: err.Error()}
	}
	var profs []string
	for _, e := range entries {
		if e.IsDir() {
			profs = append(profs, e.Name())
		}
	}
	sort.Strings(profs)
	if len(profs) == 0 {
		return checkResult{Name: name, Status: statusInfo, Message: root + " (empty)"}
	}
	return checkResult{
		Name:    name,
		Status:  statusInfo,
		Message: fmt.Sprintf("%s (profiles: %s)", root, strings.Join(profs, ", ")),
	}
}

func checkMCPs() checkResult {
	dir := paths.MCPDir()
	return checkMCPsAt(dir)
}

func checkMCPsAt(dir string) checkResult {
	const name = "mcp"
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return checkResult{Name: name, Status: statusInfo, Message: "0 definitions"}
		}
		return checkResult{Name: name, Status: statusWarn, Message: err.Error()}
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".yaml") {
			n++
		}
	}
	return checkResult{Name: name, Status: statusInfo, Message: fmt.Sprintf("%d definitions", n)}
}

// checkRSVGForVision warns when a vision-capable (mmproj-bearing) profile is
// installed but `rsvg-convert` is missing from $PATH. The vamp side
// rasterizes SVGs found under a pipeline's `image_dir` to PNG via this
// binary before sending them to llama-server, so an mmproj profile + an
// image_dir pipeline + no rsvg-convert is a runtime-only failure that we
// can catch proactively here.
//
// The second return value is false when there's nothing to report at all —
// the runner skips appending the result in that case so users without a
// vision profile don't see noise. Returning (_, false) covers "no profiles
// dir" / "no mmproj profile" / "we couldn't read the dir at all" because
// the check is informational and shouldn't FAIL the overall doctor run.
func checkRSVGForVision(env *doctorEnv, profilesDir string) (checkResult, bool) {
	const name = "rsvg-convert (vision)"
	visionProfiles := scanMMProjProfiles(profilesDir)
	if len(visionProfiles) == 0 {
		return checkResult{}, false
	}
	if _, err := env.lookPath("rsvg-convert"); err == nil {
		return checkResult{
			Name:    name,
			Status:  statusOK,
			Message: "found (needed for SVG inputs to mmproj profiles: " + strings.Join(visionProfiles, ", ") + ")",
		}, true
	}
	return checkResult{
		Name:   name,
		Status: statusWarn,
		Message: "not on $PATH — vamp pipelines with image_dir feeding " +
			"mmproj profiles (" + strings.Join(visionProfiles, ", ") +
			") rasterize SVGs via rsvg-convert. Install `librsvg2-bin` " +
			"(Debian/Ubuntu) / `librsvg` (Arch, macOS Homebrew).",
	}, true
}

// scanMMProjProfiles returns the names of profiles under dir that declare an
// mmproj path (vision-capable llama-server profiles). YAML parse failures
// are tolerated silently — the broader `profiles` check already surfaces
// those — so a single malformed file doesn't blank out the rsvg warning.
func scanMMProjProfiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		p, err := profile.Load(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if p.Backend.LlamaServer != nil && p.Backend.LlamaServer.MMProj != "" {
			out = append(out, p.Name)
		}
	}
	sort.Strings(out)
	return out
}

// checkDockerForProfiles warns when something on disk needs docker (a
// docker-compose frontend or an http_server image backend) but the docker
// CLI is missing or its daemon is unreachable. Without this, a stopped
// docker daemon surfaces as a mid-start supervisor failure ("start
// backend: ...") instead of a pre-flight hint. WARN, never FAIL — the
// non-docker profiles still work. Skipped (ok=false) when nothing on disk
// needs docker. vamp's pandoc stage also defaults to the pandoc/core
// docker image, but that isn't detectable from vibe profiles, so the
// message mentions docker generally rather than claiming full coverage.
func checkDockerForProfiles(ctx context.Context, env *doctorEnv, profilesDir string) (checkResult, bool) {
	const name = "docker"
	users := scanDockerProfiles(profilesDir)
	if len(users) == 0 {
		return checkResult{}, false
	}
	needs := "needed by " + strings.Join(users, ", ") +
		" (vamp pandoc stages also default to a docker image)"
	if _, err := env.lookPath("docker"); err != nil {
		return checkResult{
			Name:    name,
			Status:  statusWarn,
			Message: "not on $PATH — " + needs,
		}, true
	}
	cctx, cancel := context.WithTimeout(ctx, dockerInfoBudget)
	defer cancel()
	if _, err := env.run(cctx, "docker", "info"); err != nil {
		// "(daemon not running?)" is a guess at a cause, and a fair one
		// when docker itself answered. It is not fair when we stopped
		// waiting: `docker info` on a busy host routinely takes longer
		// than this budget with the daemon perfectly healthy.
		if to := runTimedOut(cctx, dockerInfoBudget); to != "" {
			return checkResult{
				Name:    name,
				Status:  statusWarn,
				Message: "`docker info` " + to + " — cannot tell whether the daemon is up; " + needs,
			}, true
		}
		return checkResult{
			Name:    name,
			Status:  statusWarn,
			Message: "`docker info` failed (daemon not running?): " + err.Error() + " — " + needs,
		}, true
	}
	return checkResult{Name: name, Status: statusOK, Message: "daemon reachable; " + needs}, true
}

// scanDockerProfiles returns the profiles and backend definitions whose
// start path shells out to docker. YAML parse failures are tolerated
// silently, like scanMMProjProfiles — the profiles/backends checks
// already surface those.
func scanDockerProfiles(profilesDir string) []string {
	var out []string
	entries, _ := os.ReadDir(profilesDir)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		p, err := profile.Load(filepath.Join(profilesDir, e.Name()))
		if err != nil {
			continue
		}
		if p.Frontend.Kind == profile.FrontendDockerCompose ||
			(p.Backend.HTTPServer != nil && p.Backend.HTTPServer.Image != "") {
			out = append(out, p.Name)
		}
	}
	if names, err := profile.ListBackends(); err == nil {
		for _, n := range names {
			def, err := profile.LoadBackend(n)
			if err != nil {
				continue
			}
			if def.Backend.HTTPServer != nil && def.Backend.HTTPServer.Image != "" {
				out = append(out, "backend "+n)
			}
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

func checkGPU(ctx context.Context, env *doctorEnv) checkResult {
	const name = "gpu"
	if _, err := env.lookPath("nvidia-smi"); err != nil {
		if runtime.GOOS == "darwin" {
			return checkResult{Name: name, Status: statusInfo, Message: "macOS: using Metal (unified memory; VRAM pre-flight skipped)"}
		}
		return checkResult{Name: name, Status: statusInfo, Message: "nvidia-smi not found (CPU-only or non-Nvidia GPU; VRAM pre-flight skipped)"}
	}
	cctx, cancel := context.WithTimeout(ctx, nvidiaSMIBudget)
	defer cancel()
	out, err := env.run(cctx, "nvidia-smi", "--query-gpu=name,memory.free", "--format=csv,noheader")
	if err != nil {
		// A driver in a bad state makes nvidia-smi HANG rather than exit,
		// which is the single most recognisable GPU symptom there is —
		// and it arrived here as "nvidia-smi failed: signal: killed",
		// naming our own kill as the tool's failure and hiding the one
		// detail that identifies the fault.
		if to := runTimedOut(cctx, nvidiaSMIBudget); to != "" {
			return checkResult{
				Name:   name,
				Status: statusWarn,
				Message: "nvidia-smi " + to + " — that is what a wedged driver looks like; " +
					"VRAM pre-flight is unavailable (not clear)",
			}
		}
		return checkResult{Name: name, Status: statusWarn, Message: "nvidia-smi failed: " + err.Error()}
	}
	lines := nonEmptyLines(out)
	if len(lines) == 0 {
		return checkResult{Name: name, Status: statusInfo, Message: "no GPUs reported"}
	}
	return checkResult{Name: name, Status: statusInfo, Message: strings.Join(lines, "; ")}
}

// ─── helpers ────────────────────────────────────────────────────────────────

func firstNonEmptyLine(b []byte) string {
	r := bufio.NewReader(bytes.NewReader(b))
	for {
		line, err := r.ReadString('\n')
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
		if err == io.EOF {
			return ""
		}
		if err != nil {
			return ""
		}
	}
}

func nonEmptyLines(b []byte) []string {
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}
