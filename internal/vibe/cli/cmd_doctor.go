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
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
	"github.com/gallowaysoftware/vibe/internal/vibeclient"
)

// Status is a check result level. WARN is informational only; FAIL is fatal
// for the doctor's exit code.
type checkStatus int

const (
	statusOK checkStatus = iota
	statusWarn
	statusFail
	statusInfo
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

// defaultDaemonStatus calls vibeclient.Status over a plain TCP connect. It
// returns the active profile name (or "" if no profile is active) on success.
func defaultDaemonStatus(ctx context.Context, addr string) (string, error) {
	hc := &http.Client{
		Timeout: 500 * time.Millisecond,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "tcp", addr)
			},
		},
	}
	c := vibeclient.NewWithHTTPClient("http://"+addr, hc)
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
	return &cobra.Command{
		Use:   "doctor",
		Short: "Verify this machine is ready to run vibe.",
		Long: "Runs a series of diagnostic checks (binaries, directories, ports, " +
			"profiles, daemon) and reports OK / WARN / FAIL for each. " +
			"Exits non-zero if any check fails.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			env := defaultDoctorEnv()
			results := runChecks(ctx, env)
			anyFail := false
			out := cmd.OutOrStdout()
			for _, r := range results {
				fmt.Fprintf(out, "%s %-28s %s\n", r.Status.tag(), r.Name, r.Message)
				if r.Status == statusFail {
					anyFail = true
				}
			}
			if anyFail {
				// Suppress cobra's "Error: ..." wrap; the per-line output is
				// already self-explanatory. Exit non-zero via SilenceErrors
				// + a sentinel that produces no extra output.
				cmd.SilenceErrors = true
				return errDoctorFailed
			}
			return nil
		},
	}
}

var errDoctorFailed = errors.New("doctor: one or more checks failed")

// runChecks executes each check in order and returns the results.
func runChecks(ctx context.Context, env *doctorEnv) []checkResult {
	// State that flows between checks: daemon PID when port 9001 is held by
	// us, and the daemon's reported active profile.
	type sharedState struct {
		daemonOnControl bool
		daemonProfile   string
	}
	st := &sharedState{}

	results := []checkResult{}

	results = append(results, checkLlamaBinary(env))
	results = append(results, checkLlamaVersion(ctx, env))
	results = append(results, checkHFBinary(env))
	results = append(results, checkHFAuth(ctx, env))
	results = append(results, checkXDGDirs())

	// Control-plane port 9001 first (so we know if a vibe daemon is up).
	cp := checkControlPlanePort(ctx, env, &st.daemonOnControl, &st.daemonProfile)
	results = append(results, cp)

	// Then the proxy port 9000 (also probed against the daemon).
	results = append(results, checkProxyPort(ctx, env, st.daemonOnControl))

	results = append(results, checkProfiles())
	results = append(results, checkMCPs())

	if st.daemonOnControl {
		msg := "running, no active profile"
		if st.daemonProfile != "" {
			msg = "running, active profile: " + st.daemonProfile
		}
		results = append(results, checkResult{Name: "daemon", Status: statusOK, Message: msg})
	} else {
		results = append(results, checkResult{Name: "daemon", Status: statusInfo, Message: "not running"})
	}

	results = append(results, checkGPU(ctx, env))

	return results
}

// ─── individual checks ──────────────────────────────────────────────────────

func checkLlamaBinary(env *doctorEnv) checkResult {
	path, err := env.lookPath("llama-server")
	if err != nil {
		return checkResult{
			Name:    "llama-server",
			Status:  statusFail,
			Message: "not found on $PATH — install llama.cpp (https://github.com/ggml-org/llama.cpp)",
		}
	}
	return checkResult{Name: "llama-server", Status: statusOK, Message: path}
}

func checkLlamaVersion(ctx context.Context, env *doctorEnv) checkResult {
	if _, err := env.lookPath("llama-server"); err != nil {
		return checkResult{
			Name:    "llama-server --version",
			Status:  statusWarn,
			Message: "skipped (llama-server not on $PATH)",
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := env.run(cctx, "llama-server", "--version")
	version := firstNonEmptyLine(out)
	if err != nil {
		// `llama-server --version` is known to write to stderr and may exit
		// non-zero on some builds; report whatever we got but mark WARN.
		if version == "" {
			return checkResult{
				Name:    "llama-server --version",
				Status:  statusWarn,
				Message: "exited non-zero: " + err.Error(),
			}
		}
		return checkResult{
			Name:    "llama-server --version",
			Status:  statusWarn,
			Message: version + " (non-zero exit)",
		}
	}
	if version == "" {
		return checkResult{
			Name:    "llama-server --version",
			Status:  statusWarn,
			Message: "no output",
		}
	}
	return checkResult{Name: "llama-server --version", Status: statusOK, Message: version}
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
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, _ := env.run(cctx, "hf", "auth", "whoami")
	text := string(out)
	if strings.Contains(text, "Not logged in") {
		return checkResult{
			Name:    "hf auth",
			Status:  statusWarn,
			Message: "not logged in — gated repos require `hf auth login`",
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

func checkControlPlanePort(ctx context.Context, env *doctorEnv, daemonOnControl *bool, daemonProfile *string) checkResult {
	const name = "control-plane port :9001"
	free, err := tryBind("127.0.0.1:9001")
	if free {
		return checkResult{Name: name, Status: statusOK, Message: "free"}
	}
	if !isAddrInUse(err) {
		// Some other listen failure (permissions, bad iface). Report it.
		return checkResult{Name: name, Status: statusFail, Message: err.Error()}
	}
	// Port is bound; figure out whether the holder is a vibe daemon.
	cctx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()
	profileName, sErr := env.statusFn(cctx, env.daemonAddr)
	if sErr != nil {
		return checkResult{
			Name:    name,
			Status:  statusFail,
			Message: "in use by another process (no vibe daemon answered: " + sErr.Error() + ")",
		}
	}
	*daemonOnControl = true
	*daemonProfile = profileName
	return checkResult{
		Name:    name,
		Status:  statusOK,
		Message: "vibe daemon already running",
	}
}

func checkProxyPort(ctx context.Context, env *doctorEnv, daemonOnControl bool) checkResult {
	const name = "proxy port :9000"
	free, err := tryBind("127.0.0.1:9000")
	if free {
		return checkResult{Name: name, Status: statusOK, Message: "free"}
	}
	if !isAddrInUse(err) {
		return checkResult{Name: name, Status: statusFail, Message: err.Error()}
	}
	if daemonOnControl {
		return checkResult{
			Name:    name,
			Status:  statusOK,
			Message: "already bound by vibe daemon",
		}
	}
	return checkResult{
		Name:    name,
		Status:  statusFail,
		Message: "in use by another process (no vibe daemon detected on :9001)",
	}
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

func checkGPU(ctx context.Context, env *doctorEnv) checkResult {
	const name = "gpu"
	if _, err := env.lookPath("nvidia-smi"); err != nil {
		return checkResult{Name: name, Status: statusInfo, Message: "nvidia-smi not found (CPU-only fine)"}
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := env.run(cctx, "nvidia-smi", "--query-gpu=name,memory.free", "--format=csv,noheader")
	if err != nil {
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
