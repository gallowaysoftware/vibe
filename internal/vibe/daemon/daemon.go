// Package daemon is the long-running supervisor process. It owns the
// llama-server supervisor and the reverse proxy, and exposes a Connect/RPC
// control plane on both a unix socket (for the local CLI) and a TCP listener
// on 127.0.0.1 (for vibeclient/vamp).
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gopkg.in/yaml.v3"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetannounce"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetmcp"
	"github.com/gallowaysoftware/vibe/internal/vibe/frontend"
	"github.com/gallowaysoftware/vibe/internal/vibe/hfdownload"
	"github.com/gallowaysoftware/vibe/internal/vibe/modelcat"
	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
	"github.com/gallowaysoftware/vibe/internal/vibe/proxy"
	"github.com/gallowaysoftware/vibe/internal/vibe/supervisor"
	"github.com/gallowaysoftware/vibe/internal/vibe/vram"
	vibev1 "github.com/gallowaysoftware/vibe/proto/vibe/v1"
	"github.com/gallowaysoftware/vibe/proto/vibe/v1/vibev1connect"
)

const (
	defaultProxyPort = 9000
	defaultHTTPAddr  = "127.0.0.1:9001"
	defaultHTTPPort  = "9001"
)

type Config struct {
	ProxyPort int `yaml:"proxy_port,omitempty"`
	// DisableProxy skips binding the reverse proxy so an external router
	// (llama-swap) can own the proxy port instead — see
	// docs/design/router-lifecycle.md. Everything else keeps working: the
	// Proxy object is still constructed (just never started), so
	// SetBackend/AddRoute/RemoveRoute calls across the start/stop/respawn
	// paths degrade to harmless in-memory updates, and ${VIBE_API} /
	// Status.ProxyAddr keep pointing at ProxyPort because the external
	// router serves the same OpenAI contract there.
	DisableProxy bool   `yaml:"disable_proxy,omitempty"`
	HTTPAddr     string `yaml:"http_addr,omitempty"`    // empty → "127.0.0.1:9001"
	LlamaBinary  string `yaml:"llama_binary,omitempty"` // empty → "llama-server" from $PATH
	// BindAll, when true, switches the TCP listener from 127.0.0.1 to
	// 0.0.0.0 so a remote machine on the LAN can reach the control plane.
	// Bearer-token auth is enforced on the TCP listener regardless. If the
	// user sets HTTPAddr explicitly (e.g. "0.0.0.0:9001"), that value wins
	// and BindAll is ignored.
	BindAll bool `yaml:"bind_all,omitempty"`
	// ProxyBindAll binds the OpenAI proxy (:ProxyPort) on 0.0.0.0 instead
	// of loopback, making this box a CELL the fleet front can proxy to as
	// a peer (topology.md: the front owns no models; cells must listen on
	// their LAN address). Deliberately separate from BindAll: the proxy
	// carries no auth — the house posture is LAN/VPN-only — while the
	// control plane at least enforces a bearer token.
	ProxyBindAll bool `yaml:"proxy_bind_all,omitempty"`
	// ClientAPIURL, when set, is what rendered frontend configs get for
	// ${VIBE_API} instead of the local proxy port — e.g.
	// "http://<fleet-front-host>:9000" once a fleet front exists, so
	// coding harnesses on this box see the whole fleet catalog (cells +
	// cloud) instead of just this box's own cell. Purely a client-facing
	// override: internal bookkeeping (readiness checks, ComfyUI's
	// /upstream/ address, the fleet API's "front" cell) keeps dialing
	// 127.0.0.1:ProxyPort, since those describe THIS box's own router
	// instance, not the fleet.
	ClientAPIURL string `yaml:"client_api_url,omitempty"`
	// SearchURL is the fleet's retrieval plane (search + page fetch), served
	// by vibe-search. It backs ${VIBE_SEARCH} in frontend templates and env,
	// so one profile can point a harness at all three pieces of
	// infrastructure — models, search, fetch — instead of the operator
	// wiring search and fetch by hand on every machine.
	//
	// Purely client-facing, like ClientAPIURL: nothing in the daemon dials
	// it. Empty means no retrieval plane is deployed, and a profile that
	// references ${VIBE_SEARCH} then fails to activate with that reason
	// rather than rendering an empty URL into a harness config.
	SearchURL string `yaml:"search_url,omitempty"`
	// FleetRegistry makes this daemon fleetd (fleet-control C1): the
	// multi-cell registry from hosts.yaml, the intent store, and the /mcp
	// facade activate. Explicit role, not file-sniffing — a hosts.yaml
	// present on an ordinary box does NOT turn it into fleetd. Startup
	// fails loudly when the role is set but hosts.yaml has no cells.
	FleetRegistry bool `yaml:"fleet_registry,omitempty"`
	// CellCmds are this box's drain/resume verbs (fleet-control C2): the
	// per-box process regime (systemd unit, launchd, compose) stays, the
	// VERB unifies. Empty means the daemon has no cell verbs —
	// CellDrain/CellResume answer FailedPrecondition.
	CellCmds CellCmds `yaml:"cell_cmds,omitempty"`
	// Fleet configures this daemon as a fleet CELL (C2): its cell name and
	// the fleetd registry it posts intent to after locally-invoked
	// drains/resumes (and announces to from C3).
	Fleet FleetConfig `yaml:"fleet,omitempty"`
	// WarmTargets is the restore-after-idle policy (fleet-control C4):
	// fleetd returns a cell's default model after an operator swap goes
	// request-idle — never on a timer.
	WarmTargets []WarmTarget `yaml:"warm_targets,omitempty"`
	// WarmSchedule is cron-firing model warming (C4): evaluated in the
	// daemon's TZ (declared via the environment, not inherited).
	WarmSchedule []WarmScheduleEntry `yaml:"warm_schedule,omitempty"`
	// ProbeTargets are the declared periodic throughput probes
	// (fleet-control C8). fleetd ASKS on this schedule; the cell measures
	// and may refuse. No entries means no probing at all — this is the one
	// place the control plane deliberately spends GPU time, so it is
	// declared, never implicit.
	ProbeTargets []ProbeTarget `yaml:"probe_targets,omitempty"`
	// SleepSchedule is the declared night (fleet-control C14): a cron
	// suspend for an opportunistic cell, paired with the wake that brings
	// it back. The suspend is DEFERRED by in-flight work, leases, holds, a
	// declared drain and recent activity — never triggered by any of them.
	SleepSchedule []SleepScheduleEntry `yaml:"sleep_schedule,omitempty"`
}

// SleepScheduleEntry is one cell's declared night. Both crons are the
// five standard fields at minute granularity, evaluated in the declared
// fleet timezone by the same evaluator warm_schedule uses.
type SleepScheduleEntry struct {
	// Cell is the fleet cell name. It must be class opportunistic, carry
	// a daemon_url (the suspend is an RPC, never a piggyback command) and
	// declare a wake: block in hosts.yaml.
	Cell string `yaml:"cell"`
	// Suspend is the cron that DECLARES the suspend.
	Suspend string `yaml:"suspend"`
	// Wake is the paired wake cron. Required: a suspend with no declared
	// wake is a box that never comes back, and an unparseable wake
	// disables the suspend half too.
	Wake string `yaml:"wake"`
	// QuietFor is how long every model on the cell must have been
	// request-idle before the declared suspend fires (default 15m, floor
	// 5m). It protects the operator who is typing at 23:29.
	QuietFor string `yaml:"quiet_for,omitempty"`
	// MaxDefer bounds how long a blocked suspend keeps retrying (default
	// 2h). It is also abandoned at the paired wake, whichever is sooner.
	MaxDefer string `yaml:"max_defer,omitempty"`
	// WakeGrace is how long the cell is given to come back after the wake
	// before the schedule calls it a failed wake (default 10m).
	WakeGrace string `yaml:"wake_grace,omitempty"`
	// Warm names models to warm through the front once the cell is back.
	Warm []string `yaml:"warm,omitempty"`
}

// WarmTarget names a cell's default model and the idle window after
// which fleetd restores it once whatever the operator swapped in has
// gone request-idle.
type WarmTarget struct {
	// Cell is the fleet cell name (must exist in hosts.yaml when the
	// fleet registry is enabled).
	Cell string `yaml:"cell"`
	// Model is the model id to restore to.
	Model string `yaml:"model"`
	// RestoreAfterIdle is the request-idle window (Go duration string,
	// e.g. "30m"): the swapped-in model must serve NOTHING for this long
	// before fleetd warms the target back in. Any request to the
	// swapped-in model resets the window.
	RestoreAfterIdle string `yaml:"restore_after_idle"`
}

// WarmScheduleEntry is one cron-firing warm. The five fields are the
// standard minute/hour/dom/month/dow cron fields, evaluated at minute
// granularity in the daemon's TZ.
type WarmScheduleEntry struct {
	Cron  string `yaml:"cron"`
	Model string `yaml:"model"`
}

// ProbeTarget is one declared periodic throughput probe (C8). A probe
// only ever runs against a model the cell already holds resident, so a
// target on a model that is usually cold simply reports "not resident"
// and costs nothing.
type ProbeTarget struct {
	// Cell is the fleet cell name (must exist in hosts.yaml).
	Cell string `yaml:"cell"`
	// Model is the model id to probe, as the CELL announces it (the
	// canonical def name, never a client-side alias).
	Model string `yaml:"model"`
	// Every is the request interval (Go duration string, e.g. "6h"),
	// floored at minProbeInterval.
	Every string `yaml:"every"`
}

// CellCmds maps the unified verbs to this box's process regime. Commands
// run via sh -c with a 60s timeout; a failing drain must surface stderr
// (a stopped-but-reporting unit is the classic silent failure).
type CellCmds struct {
	// Drain reclaims the box (e.g. "systemctl --user stop llama-swap") —
	// a unit stop, never a kill, and never unload-all (an unloaded model
	// JIT-reloads on the next stray request, exactly wrong mid-game).
	// The stop does NOT let generations finish: llama-swap's SIGTERM
	// calls CloseStreams() before its graceful drain (v239, C2's live
	// gate), so in-flight streams die at the stop. `--wait` is what
	// drains them first.
	Drain string `yaml:"drain,omitempty"`
	// Resume restores JIT service (e.g. "systemctl --user start
	// llama-swap"). Models return by JIT on next request; resume does not
	// preload.
	Resume string `yaml:"resume,omitempty"`
	// Suspend puts the whole BOX to sleep (fleet-control C14) — how is
	// house-specific, which is why it is a command and not a mechanism
	// this repo picks. The reference example stops the serving stack
	// first, because CUDA contexts do not reliably survive S3:
	// "systemctl --user stop llama-swap && systemctl suspend". The
	// command must RETURN (systemctl suspend is asynchronous); one that
	// blocks until the machine freezes turns the RPC into a transport
	// error and the outcome into a guess.
	Suspend string `yaml:"suspend,omitempty"`
}

// FleetConfig is the daemon's cell identity and registry pointer. C3's
// announce loop reuses this block unchanged.
type FleetConfig struct {
	// Cell is this box's cell name in hosts.yaml.
	Cell string `yaml:"cell,omitempty"`
	// RegistryURL is fleetd's base URL (where intent posts and, from C3,
	// announces go).
	RegistryURL string `yaml:"registry_url,omitempty"`
	// TokenFile is the path to fleetd's bearer token. The path is config;
	// the token value never enters a repo. Tilde-expanded at load.
	TokenFile string `yaml:"token_file,omitempty"`
	// GuestTokenFile is the path to the guest READ-ONLY bearer
	// (fleet-control C12), honored on exactly GET /api/fleet/state and
	// GET /api/fleet/events and refused everywhere else. Same convention
	// as TokenFile: the path is config, the value never enters a repo.
	// Empty is the default and means no guest credential exists at all —
	// a fleet that never configures one behaves exactly as it did before
	// C12. A missing file is MINTED at first start; anything else wrong
	// with it (empty, too short, whitespace, or identical to the
	// control-plane token) disables guest access and says so, because a
	// misconfigured share token must fail closed rather than fail wide.
	GuestTokenFile string `yaml:"guest_token_file,omitempty"`
	// FrontConfig is the front's rendered llama-swap config path as seen
	// from THIS daemon — set on fleetd (same-host mount) so the MCP
	// render_front tool can diff a fresh render against it. Empty means
	// render-only, no diff.
	FrontConfig string `yaml:"front_config,omitempty"`
	// FrontExtras is a YAML file whose top-level sections are merged into
	// every render of the front's config (`vibe router render --extras`,
	// same merge). It exists because the front's config is a DERIVED
	// artifact — fleetd rewrites it on every membership transition — so
	// anything the operator needs there that the renderer does not emit is
	// erased on the next presence change.
	//
	// `apiKeys:` is exactly that (fleet-control C15), and it is why this
	// key landed with the credential rather than after it: a front
	// credential fleetd deletes at the next render is not a credential.
	// Same for `store:` (C7a's activity log). Empty is the reference
	// posture — a rendered front with nothing but derived content.
	FrontExtras string `yaml:"front_extras,omitempty"`
	// FrontImage is the container image reference the front is deployed
	// from (fleet-control C16), declared so `vibe fleet doctor` can report
	// an unpinned deployment. fleetd is a different container with no
	// docker socket, so this cannot be observed — and a floating tag is
	// how a routine `docker compose pull` moved the fleet onto a
	// llama-swap whose in-flight wire every busy guard misread.
	// fleetapi.UnmanagedFrontImage declares a front that runs no image.
	FrontImage string `yaml:"front_image,omitempty"`
	// MirrorMaxAge is how fresh the off-host state mirror is expected to
	// be (fleet-control C19), as a Go duration — "36h" for a nightly
	// timer with a night of slack. It configures nothing: fleetd never
	// mirrors anything and must not, because the mirror has to keep
	// running when fleetd is the thing that broke. The one consumer is
	// `vibe fleet doctor`'s mirror.age check, and the value is what turns
	// "a mirror ran at some point" into a verdict.
	//
	// fleetapi.UnmanagedMirror is the closed-vocabulary declaration that
	// this fleet's state is backed up by something else (a snapshotting
	// filesystem, borg, the whole host being a VM). Unset is UNKNOWN, not
	// OK: "the operator decided" and "nobody told fleetd" must not be
	// spelled the same way (C16's front_image rule).
	MirrorMaxAge string `yaml:"mirror_max_age,omitempty"`
	// Notify is the alarm-to-webhook bridge (fleet-control C9). Empty
	// means no notifications — the design's "alarm? yes" column then
	// terminates in an SSE stream nobody watches, which is the status
	// quo, not a regression.
	Notify NotifyConfig `yaml:"notify,omitempty"`
	// Timezone is the IANA zone (e.g. "America/Toronto") the fleet's
	// wall-clock decisions are evaluated in: the usage ledger's day
	// boundaries (C7a §6) and the warm schedule's cron fields (C4 §2).
	// Declared, never assumed — fleetd runs containerized and defaults to
	// TZ=UTC, which silently splits an evening session across two days
	// and fires an 06:30 warm at 01:30 local. Empty keeps the process
	// zone (the pre-C7a behavior).
	Timezone string `yaml:"timezone,omitempty"`
}

// NotifyConfig configures the C9 alarm notifier. The webhook URL is a
// CREDENTIAL (an ntfy topic URL is bearer-equivalent in both
// directions), so URLFile is the preferred form and neither value is
// ever logged, returned in an error, or serialized into a status
// document — see internal/vibe/fleetnotify's Redact and Scrub.
type NotifyConfig struct {
	// URL is the webhook endpoint inline. Acceptable only when
	// config.yaml itself is 0600; prefer URLFile.
	URL string `yaml:"url,omitempty"`
	// URLFile is a file containing the endpoint (first line), following
	// fleet.token_file's convention: the path is config, the value never
	// enters a repo. Tilde-expanded at load.
	URLFile string `yaml:"url_file,omitempty"`
	// TokenFile is an optional bearer token for the webhook (self-hosted
	// ntfy with access control). Tilde-expanded at load.
	TokenFile string `yaml:"token_file,omitempty"`
	// Format is "text" (default, ntfy-native headers) or "json".
	Format string `yaml:"format,omitempty"`
	// Interval is the evaluation cadence (Go duration, default 30s).
	Interval string `yaml:"interval,omitempty"`
	// Alarms overrides the enabled alarm kinds. Empty means the design
	// doc §4 class table's alarm column and nothing else.
	Alarms []string `yaml:"alarms,omitempty"`
	// Dwell overrides the per-kind fire threshold (Go durations, keyed by
	// alarm kind); ClearDwell overrides the resolve threshold.
	Dwell      map[string]string `yaml:"dwell,omitempty"`
	ClearDwell string            `yaml:"clear_dwell,omitempty"`
	// RatePerHour and Burst bound deliveries absolutely (defaults 12 and
	// 4). Anything the dwells let through is paced by this bucket.
	RatePerHour float64 `yaml:"rate_per_hour,omitempty"`
	Burst       int     `yaml:"burst,omitempty"`
	// Resolve sends a notification when a fired alarm clears (default
	// true) — the passive half of "await-unblocked".
	Resolve *bool `yaml:"resolve,omitempty"`
}

// FleetLocation resolves fleet.timezone to a Location. An unparseable
// zone warns and falls back to the process zone rather than failing
// startup: a bad TZ must not cost the fleet its registry.
func (c Config) FleetLocation() *time.Location {
	if c.Fleet.Timezone == "" {
		return time.Local
	}
	loc, err := time.LoadLocation(c.Fleet.Timezone)
	if err != nil {
		slog.Warn("fleet.timezone unparseable; falling back to the process zone",
			"timezone", c.Fleet.Timezone, "err", err)
		return time.Local
	}
	return loc
}

// LoadConfig reads the global vibe config; missing file is not an error.
func LoadConfig() (Config, error) { return LoadConfigFrom(paths.ConfigFile()) }

// LoadConfigFrom reads a config from an explicit path. It exists for
// `vibe fleet mirror` (C19), which runs on the front HOST against
// fleetd's bind-mounted config dir rather than against its own XDG
// locations — and a second parser for the same file is how two readers
// drift apart.
func LoadConfigFrom(path string) (Config, error) {
	c := Config{ProxyPort: defaultProxyPort, HTTPAddr: defaultHTTPAddr}
	data, err := os.ReadFile(path) //nolint:gosec // an explicit config path
	if errors.Is(err, os.ErrNotExist) {
		return c.resolveHTTPAddr(), nil
	}
	if err != nil {
		return c, err
	}
	// Don't default HTTPAddr before unmarshal: we need to tell "user didn't
	// set it" from "user set it to the default" so BindAll can take effect
	// only when HTTPAddr is unset.
	c.HTTPAddr = ""
	if err := yaml.Unmarshal(data, &c); err != nil {
		return c, err
	}
	if c.ProxyPort == 0 {
		c.ProxyPort = defaultProxyPort
	}
	c.Fleet.TokenFile = expandTilde(c.Fleet.TokenFile)
	c.Fleet.GuestTokenFile = expandTilde(c.Fleet.GuestTokenFile)
	c.Fleet.FrontConfig = expandTilde(c.Fleet.FrontConfig)
	c.Fleet.Notify.URLFile = expandTilde(c.Fleet.Notify.URLFile)
	c.Fleet.Notify.TokenFile = expandTilde(c.Fleet.Notify.TokenFile)
	return c.resolveHTTPAddr(), nil
}

// expandTilde expands a leading "~/" against the user's home directory
// (same anchored-prefix shape as the profile package's helper).
func expandTilde(p string) string {
	if !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return home + string(os.PathSeparator) + p[2:]
}

// resolveHTTPAddr fills in HTTPAddr from BindAll when the user didn't set
// HTTPAddr explicitly. An explicit HTTPAddr always wins.
func (c Config) resolveHTTPAddr() Config {
	if c.HTTPAddr != "" {
		return c
	}
	if c.BindAll {
		c.HTTPAddr = "0.0.0.0:" + defaultHTTPPort
	} else {
		c.HTTPAddr = defaultHTTPAddr
	}
	return c
}

// serviceInstance bundles the per-service state for one running
// service-mode profile. Services run alongside each other and
// alongside the active profile; each has its own supervisor so a
// crash + auto-respawn in one service doesn't perturb the others.
type serviceInstance struct {
	profile   *profile.Profile
	sup       *supervisor.Supervisor
	startTime time.Time
	port      int
}

type Daemon struct {
	cfg Config
	sup *supervisor.Supervisor
	prx *proxy.Proxy

	// nvidiaSMI is the VRAM probe used by Start for its pre-flight check.
	// Tests inject a stub here; production wires vram.DefaultProbe via New,
	// which is nvidia-smi where there's an NVIDIA GPU and unified-memory
	// accounting on Apple silicon.
	nvidiaSMI vram.Probe
	// vramCapacity answers "could this ever fit on this machine", the only
	// question that can refuse a start outright.
	vramCapacity vram.CapacityProbe
	// vramSlopGiB is added to free VRAM before comparing to the profile's
	// estimate, absorbing the inherent fuzziness of those numbers. Defaults
	// to vram.DefaultSlopGiB in New.
	vramSlopGiB float64

	// startMu serializes start/stop operations against each other.
	startMu sync.Mutex

	// hosts is the parsed hosts.yaml, set only under fleet_registry —
	// fleetmcp reads cell URLs and model classes from it.
	hosts *fleetcfg.File

	// authRejected counts bearer-auth 401s on the TCP listener; surfaced
	// in /api/fleet/state so a stale-token client is visible as a number,
	// not buried in logs.
	authRejected atomic.Int64
	// guestEnabled/guestRejected are the C12 guest read-only bearer's two
	// status fields: whether one is configured, and how many requests
	// presented a valid one on a route its allowlist does not name. Never
	// the token itself. Atomics because both are written during Run and
	// read from every state snapshot.
	guestEnabled  atomic.Bool
	guestRejected atomic.Int64
	// tokenMinted records that THIS start created the control-plane token
	// rather than loading one — on fleetd, the signature of a container
	// recreate over an unmounted state dir. Recorded rather than
	// re-derived: a later read of the file answers "does a token exist
	// now", which is a different question (C13's fleetd.token check).
	tokenMinted atomic.Bool

	// fleet is the fleetapi server, assigned in Run once constructed.
	// CellDrain reads the local cell's inflight count from it.
	fleet *fleetapi.Server
	// announce is the cell's C3 announce loop, started in Run when
	// fleet.cell + fleet.registry_url are set. CellDrain/CellResume stamp
	// local intent through it (the conflict rule's cell side).
	// announceCancel/announceDone let shutdown stop the loop BEFORE the
	// withdrawing goodbye; all three are written once in startAnnounce,
	// before the listeners serve.
	announce       *fleetannounce.Client
	announceCancel context.CancelFunc
	announceDone   <-chan struct{}
	// cellCmdRunner executes cell verbs; tests swap it to script
	// drain/resume outcomes without touching real units (same injection
	// pattern as nvidiaSMI). Defaults to runCellCmd in New.
	cellCmdRunner func(ctx context.Context, cmd string) (string, error)

	mu        sync.Mutex
	active    *profile.Profile
	startTime time.Time
	frontend  *frontend.Result
	// services is the map of running service-mode profiles, keyed by
	// profile name. Empty in the legacy single-active-profile case.
	// Each entry has its own supervisor so crashes don't cross-
	// contaminate. The map itself is protected by mu; entries are
	// otherwise immutable once stored (callers read pointers, never
	// mutate).
	services map[string]*serviceInstance

	shutdown     chan struct{}
	shutdownOnce sync.Once
}

// Compile-time check: Daemon implements the Connect service.
var _ vibev1connect.ControlServiceHandler = (*Daemon)(nil)

func New(cfg Config) *Daemon {
	proxyHost := "127.0.0.1"
	if cfg.ProxyBindAll {
		proxyHost = "0.0.0.0"
	}
	return &Daemon{
		cfg:           cfg,
		sup:           supervisor.New(),
		prx:           proxy.New(fmt.Sprintf("%s:%d", proxyHost, cfg.ProxyPort)),
		nvidiaSMI:     vram.DefaultProbe,
		vramCapacity:  vram.DefaultCapacityProbe,
		vramSlopGiB:   vram.DefaultSlopGiB,
		services:      map[string]*serviceInstance{},
		shutdown:      make(chan struct{}),
		cellCmdRunner: runCellCmd,
	}
}

// SetVRAMProbe overrides the VRAM-free probe. Used by tests to avoid shelling
// out to nvidia-smi.
func (d *Daemon) SetVRAMProbe(p vram.Probe) {
	d.nvidiaSMI = p
}

// SetCellCmdRunner overrides the drain/resume command runner. Used by
// tests to script verb outcomes without touching real units.
func (d *Daemon) SetCellCmdRunner(f func(ctx context.Context, cmd string) (string, error)) {
	d.cellCmdRunner = f
}

// SetCapacityProbe overrides the total-capacity probe. Tests must pin this
// alongside SetVRAMProbe: capacity decides warn-vs-refuse, so leaving it
// reading the real host makes the same test pass on a big dev machine and
// fail on a small CI runner.
func (d *Daemon) SetCapacityProbe(p vram.CapacityProbe) {
	d.vramCapacity = p
}

// Run brings up the proxy and both control-plane listeners (unix + TCP),
// blocking until ctx is canceled or a Shutdown RPC fires.
func (d *Daemon) Run(ctx context.Context) error {
	if err := paths.EnsureDirs(); err != nil {
		return fmt.Errorf("ensure dirs: %w", err)
	}
	// An exclusive flock held for the daemon's lifetime is the
	// single-instance gate. The old PID-file heuristic raced: the PID file
	// is written only after the listeners bind, so two daemons spawned
	// within the CLI's ping window both passed the check — and the loser's
	// socket cleanup deleted the winner's live unix socket, wedging every
	// subsequent CLI call until manual cleanup. The flock loser exits
	// before touching the socket path, which also makes the unconditional
	// os.Remove below safe: any socket file present while we hold the lock
	// is stale by definition.
	lock, err := os.OpenFile(paths.PIDFile()+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("open daemon lock: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return errors.New("vibe daemon already running")
	}
	defer lock.Close()

	sockPath := paths.Socket()
	_ = os.Remove(sockPath)
	unixLn, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("listen unix %s: %w", sockPath, err)
	}
	defer os.Remove(sockPath)
	if err := os.Chmod(sockPath, 0o600); err != nil {
		return err
	}

	httpLn, err := net.Listen("tcp", d.cfg.HTTPAddr)
	if err != nil {
		unixLn.Close()
		return fmt.Errorf("listen tcp %s: %w", d.cfg.HTTPAddr, err)
	}

	if err := writePIDFile(); err != nil {
		return err
	}
	defer os.Remove(paths.PIDFile())

	if d.cfg.DisableProxy {
		slog.Info("reverse proxy disabled; leaving the proxy port unbound for an external router serving the same OpenAI contract",
			"proxy_addr", fmt.Sprintf("127.0.0.1:%d", d.cfg.ProxyPort))
	} else if err := d.prx.Start(); err != nil {
		return fmt.Errorf("start proxy: %w", err)
	}
	// Safe when DisableProxy skipped Start: Stop on a never-started proxy
	// is a no-op.
	defer func() { _ = d.prx.Stop(context.Background()) }()

	// Generate or load the bearer token before binding the TCP server.
	// The unix socket reuses the same Connect handler but skips token
	// validation (0600 socket perms are the auth boundary there).
	// The created/loaded distinction is logged LOUDLY: on fleetd a
	// container recreate over an unmounted state dir mints a fresh token,
	// and every client then 401s until someone notices (fleet-control C1).
	token, created, err := LoadOrCreateToken()
	if err != nil {
		unixLn.Close()
		httpLn.Close()
		return fmt.Errorf("load token: %w", err)
	}
	d.tokenMinted.Store(created)
	if created {
		slog.Warn("control-plane token CREATED (new) — no existing token file; clients must be re-provisioned with this token", "path", paths.TokenFile())
	} else {
		slog.Info("control-plane token loaded", "path", paths.TokenFile())
	}
	guestToken := d.loadGuestToken(token)

	mux := http.NewServeMux()
	mountPath, connectHandler := vibev1connect.NewControlServiceHandler(d)
	mux.Handle(mountPath, connectHandler)

	// Fleet observability surface (docs/design/router-lifecycle.md §11,
	// fleet-control C1). Shares the Connect mux, so the TCP listener's
	// bearer middleware covers it and the unix socket serves it
	// unauthenticated — same boundary as the RPCs.
	//
	// Default (no fleet_registry): the registry is the one-element front
	// cell — whatever serves the proxy port (llama-swap under
	// disable_proxy, else vibe's own proxy); an absent router degrades to
	// reachable:false + fleet.cellDown, never an error. With
	// fleet_registry the cells come from hosts.yaml (missing/invalid is a
	// startup failure: the role was explicitly requested), and the intent
	// store + /mcp facade activate.
	fleetCells := []fleetapi.Cell{{Name: "front", URL: fmt.Sprintf("http://127.0.0.1:%d", d.cfg.ProxyPort)}}
	fleetOpts := fleetapi.Options{}
	if d.cfg.FleetRegistry {
		hosts, err := fleetcfg.Load()
		if err != nil {
			unixLn.Close()
			httpLn.Close()
			return fmt.Errorf("fleet_registry: %w", err)
		}
		if !hosts.HasCells() {
			unixLn.Close()
			httpLn.Close()
			return fmt.Errorf("fleet_registry: %s has no cells: section (the role needs the registry)", paths.HostsFile())
		}
		fleetCells = fleetCells[:0]
		for _, name := range slices.Sorted(maps.Keys(hosts.Cells)) {
			c := hosts.Cells[name]
			fc := fleetapi.Cell{
				Name:      name,
				URL:       c.URL,
				Class:     string(c.Class),
				HostProbe: c.HostProbe,
			}
			if c.Wake != nil {
				fc.Wake = &fleetapi.WakeSpec{MAC: c.Wake.MAC, Broadcast: c.Wake.Broadcast, Cmd: c.Wake.Cmd}
			}
			fleetCells = append(fleetCells, fc)
		}
		fleetOpts = fleetapi.Options{
			IntentPath:   paths.IntentFile(),
			LastSeenPath: paths.LastSeenFile(),
			LeasePath:    paths.LeasesFile(),
			UsagePath:    paths.UsageLedgerFile(),
			Timezone:     d.cfg.FleetLocation(),
			// C7b: pricing, declared wattage and capital numbers live in
			// hosts.yaml beside the cells they describe. Membership still
			// comes from fleetCells above — this is the same file, not a
			// second cell list.
			Hosts:           hosts,
			NotifyScopePath: paths.NotifyScopeFile(),
			// C13: the two things the doctor cannot do from inside
			// fleetapi — read this host, and call a cell with the
			// credential the actuation verbs resolve. Both read-only.
			DoctorHost: d.doctorHost,
			CellAuth:   d.cellAuthProbe,
		}
		d.hosts = hosts
		// C15: resolve every declared llama-swap credential now, so a
		// missing key file is an ERROR line at startup rather than a warm
		// target that quietly stops firing.
		checkSwapKeys(hosts)
	}
	fleet := fleetapi.New(
		fleetCells,
		paths.StartHistoryFile(),
		d.fleetDaemonInfo,
		fleetOpts,
	)
	d.fleet = fleet
	fleet.Register(mux)
	fleet.Start()
	defer fleet.Close()
	if d.cfg.FleetRegistry {
		fleetmcp.New(fleet, d.hosts, fleetmcp.Options{
			FrontConfig: d.cfg.Fleet.FrontConfig,
			BackendsDir: paths.BackendsDir(),
			LlamaBinary: d.cfg.LlamaBinary,
			FrontExtras: d.cfg.Fleet.FrontExtras,
		}).Register(mux)
		// C3: the front config is a derived artifact once fleetd can see
		// its path (same-host mount). Without front_config the registry
		// stays observe-only (C1/C2 behavior, unchanged).
		if d.cfg.Fleet.FrontConfig != "" {
			fleet.StartRenderLoop(fleetapi.RenderLoopConfig{
				BackendsDir:       paths.BackendsDir(),
				LlamaServerBinary: d.cfg.LlamaBinary,
				FrontConfigPath:   d.cfg.Fleet.FrontConfig,
				FrontExtras:       d.cfg.Fleet.FrontExtras,
				Hosts:             d.hosts,
			})
		}
		// C4: warm policy loops ride the same presence/inflight substrate.
		d.startWarmLoops(d.cfg, d.hosts)
		// C8: the probe scheduler rides it too — same guards, and it only
		// ever ASKS (the cell measures, and may refuse).
		d.startProbeLoop(d.cfg, d.hosts)
		// C7b: actual cloud spend, tailed off the front's own activity log.
		d.startCloudSpendLoop(d.hosts)
		// C9: the class table's alarm column, delivered. Read-only over
		// the same snapshot every other surface renders.
		d.startNotifyLoop(d.cfg)
		// C14: the declared night. A cron DECLARES the suspend; in-flight
		// work, leases, holds, a declared drain and recent activity only
		// ever DEFER it.
		d.startSleepLoops(d.cfg, d.hosts)
	}

	// C3: this box announces to fleetd when it has a cell identity and a
	// registry. The loop is best-effort by construction — an unreachable
	// registry must never affect serving.
	if d.cfg.Fleet.Cell != "" && d.cfg.Fleet.RegistryURL != "" {
		if err := d.startAnnounce(ctx); err != nil {
			slog.Warn("announce loop not started (cell keeps serving; fleetd sees probes only)", "err", err)
		}
	}

	unixSrv := &http.Server{Handler: mux}
	// The TCP server gets a bearer-auth wrapper around the same mux. Two
	// separate http.Server instances (one per listener) is the cleanest
	// way to express "only TCP requires auth" without leaking the
	// distinction into per-RPC interceptors. markRemote distinguishes
	// fleetd-invoked cell verbs (fleetd writes intent) from local
	// unix-socket ones (this daemon writes it).
	httpSrv := &http.Server{Handler: bearerAuthMiddleware(authGuard{
		token:         token,
		guestToken:    guestToken,
		rejected:      &d.authRejected,
		guestRejected: &d.guestRejected,
	}, markRemote(mux))}
	errCh := make(chan error, 2)
	serve := func(srv *http.Server, ln net.Listener) {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}
	go serve(unixSrv, unixLn)
	go serve(httpSrv, httpLn)

	slog.Info("daemon ready",
		"unix_socket", sockPath,
		"http_addr", d.cfg.HTTPAddr,
		"proxy_addr", fmt.Sprintf("127.0.0.1:%d", d.cfg.ProxyPort),
		"llama_binary", d.cfg.LlamaBinary,
		"token_file", paths.TokenFile())

	var shutReason string
	select {
	case <-ctx.Done():
		shutReason = "context canceled"
	case <-d.shutdown:
		shutReason = "shutdown rpc"
	case err := <-errCh:
		slog.Error("listener failed", "err", err)
		return err
	}
	slog.Info("daemon shutting down", "reason", shutReason)
	shCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Say goodbye before anything else stops: a clean withdraw lets
	// fleetd prune this cell's catalog now instead of waiting out
	// stale_after, which is the whole reason the withdrawing state
	// exists. Best-effort — an unreachable fleetd must not delay
	// shutdown past the announce timeout.
	d.withdrawAnnounce()
	// Tear down the active frontend (if any) first; otherwise a
	// docker-compose stack outlives the daemon and keeps serving stale
	// requests at the (now-dead) proxy.
	d.mu.Lock()
	fr := d.frontend
	act := d.active
	// Clear d.active BEFORE stopping the supervisor — same rationale
	// as stopActive does for a per-profile stop. The
	// watchBackendForRespawn goroutine wakes the instant the
	// supervisor's `stopped` channel closes; if d.active still names
	// the about-to-be-killed profile, the watcher fires a respawn
	// against the shutting-down process and we spawn an orphan
	// backend during shutdown.
	d.active = nil
	d.frontend = nil
	d.mu.Unlock()
	if fr != nil {
		if err := frontend.Deactivate(shCtx, fr); err != nil {
			slog.Warn("frontend deactivate on shutdown failed", "err", err)
		}
	}
	if act != nil {
		_ = runHooks(shCtx, act.Name, "post_stop", act.Hooks.PostStop, false)
	}
	// Stop the supervisor unconditionally; if a start is mid-flight the
	// child exists and SIGINT will unblock waitReady.
	_ = d.sup.Stop(shCtx)
	d.prx.SetBackend(nil)

	// Tear down every service-mode supervisor — concurrent so a
	// hung service doesn't block the others. Each is best-effort;
	// log any failure but keep going (the daemon is exiting either
	// way). Without this, services orphan after daemon shutdown
	// and their ports stay held until the next OS-level reboot.
	d.mu.Lock()
	services := make([]*serviceInstance, 0, len(d.services))
	for n, svc := range d.services {
		services = append(services, svc)
		delete(d.services, n)
	}
	d.mu.Unlock()
	var wg sync.WaitGroup
	for _, svc := range services {
		wg.Add(1)
		go func(s *serviceInstance) {
			defer wg.Done()
			if err := s.sup.Stop(shCtx); err != nil {
				slog.Warn("service stop on shutdown failed", "profile", s.profile.Name, "err", err)
			}
		}(svc)
	}
	wg.Wait()

	// Close the fleet hub before Shutdown: open /api/fleet/events responses
	// only return once the hub's done channel closes, and Shutdown waits for
	// active requests.
	fleet.Close()
	_ = unixSrv.Shutdown(shCtx)
	_ = httpSrv.Shutdown(shCtx)
	return nil
}

// ─── Connect handler methods ────────────────────────────────────────────────

func (d *Daemon) Status(_ context.Context, _ *connect.Request[vibev1.StatusRequest]) (*connect.Response[vibev1.StatusResponse], error) {
	return connect.NewResponse(&vibev1.StatusResponse{
		Status:   d.protoStatus(),
		Services: d.servicesStatus(),
	}), nil
}

func (d *Daemon) ListProfiles(_ context.Context, _ *connect.Request[vibev1.ListProfilesRequest]) (*connect.Response[vibev1.ListProfilesResponse], error) {
	dir := paths.ProfilesDir()
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	var out []*vibev1.Profile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		p, err := profile.Load(path)
		if err != nil {
			continue
		}
		out = append(out, &vibev1.Profile{Name: p.Name, Description: p.Description, Path: path})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return connect.NewResponse(&vibev1.ListProfilesResponse{Profiles: out}), nil
}

func (d *Daemon) Start(_ context.Context, req *connect.Request[vibev1.StartRequest]) (*connect.Response[vibev1.StartResponse], error) {
	if !d.startMu.TryLock() {
		return nil, connect.NewError(connect.CodeAborted, errors.New("another start/stop is in progress"))
	}
	defer d.startMu.Unlock()

	profileName := strings.TrimSpace(req.Msg.Profile)
	backendName := strings.TrimSpace(req.Msg.Backend)
	if (profileName == "") == (backendName == "") {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("exactly one of profile or backend must be set"))
	}

	var p *profile.Profile
	if backendName != "" {
		if !validBackendName.MatchString(backendName) {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("backend name %q is invalid (allowed: [a-zA-Z0-9._-]+)", backendName))
		}
		def, err := profile.LoadBackend(backendName)
		if err != nil {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		// Activate a backend with NO frontend: the model server is the
		// deliverable (reached via the proxy). The synthetic profile's Name is
		// the backend name, so the active-identity check below treats repeated
		// activations of the same backend as no-op reuse — which is how vamp
		// capability resolution keys on a backend rather than a profile.
		p = &profile.Profile{
			Name:            backendName,
			Backend:         def.Backend,
			EstimatedVRAMGB: def.EstimatedVRAMGB,
			Mode:            def.Mode,
		}
	} else {
		var err error
		p, err = loadProfileByName(profileName)
		if err != nil {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
	}

	startCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Service-mode profiles run as background sidecars: their own
	// supervisor instance, no proxy / frontend wiring, no contention
	// for the daemon's single "active" slot. Branch off the active
	// path here so the existing code below stays untouched.
	if p.ResolvedMode() == profile.ModeService {
		return d.startService(startCtx, p)
	}

	d.mu.Lock()
	if d.active != nil {
		name := d.active.Name
		d.mu.Unlock()
		return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("profile %q is already running; stop first", name))
	}
	d.mu.Unlock()

	// Pre-flight VRAM check. We only run this when the profile declares an
	// estimate (otherwise the older zero-VRAM profiles would all skip
	// silently). A missing nvidia-smi degrades to a warning so users on
	// CPU/AMD/Apple-Silicon hosts aren't blocked. External backends skip it
	// outright: the router owns placement, and the model may not even load
	// until its first request (JIT), so "free VRAM now" proves nothing.
	if !req.Msg.NoVramCheck && p.EstimatedVRAMGB > 0 && !p.Backend.External {
		res := vram.CheckWith(startCtx, d.nvidiaSMI, d.vramCapacity, p.EstimatedVRAMGB, d.vramSlopGiB)
		switch {
		case res.Skipped:
			slog.Warn("vram pre-flight skipped",
				"profile", p.Name,
				"estimated_gib", p.EstimatedVRAMGB,
				"reason", res.Message)
		case !res.OK:
			// Only reached when the estimate exceeds the machine's total
			// capacity — no amount of freeing memory makes this one work,
			// so the override is offered but not encouraged.
			msg := fmt.Sprintf(
				"profile %q needs ~%.1f GiB but this machine has only %.1f GiB of usable memory in total.\nUse a smaller quantisation, or re-run with --no-vram-check to try anyway.",
				p.Name, p.EstimatedVRAMGB, res.TotalGiB)
			slog.Warn("vram pre-flight failed",
				"profile", p.Name,
				"estimated_gib", p.EstimatedVRAMGB,
				"total_gib", res.TotalGiB,
				"free_gib", res.FreeGiB)
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New(msg))
		case res.Warn:
			// Tight, not impossible: free memory moves (page cache, another
			// tenant exiting), so this proceeds and says so rather than
			// refusing a start that usually works. The CLI's progress tail
			// renders this msg in yellow.
			slog.Warn("vram pre-flight tight",
				"profile", p.Name,
				"estimated_gib", p.EstimatedVRAMGB,
				"free_gib", res.FreeGiB)
		default:
			slog.Info("vram pre-flight ok",
				"profile", p.Name,
				"estimated_gib", p.EstimatedVRAMGB,
				"free_gib", res.FreeGiB)
		}
	}

	// Lifecycle: pre_start hooks run after the VRAM pre-flight and before the
	// backend/frontend come up. A failed hook aborts the start so we never
	// half-launch against a missing dependency.
	if err := runHooks(startCtx, p.Name, "pre_start", p.Hooks.PreStart, true); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// Once pre_start side effects exist (e.g. `docker compose up` of a
	// sidecar), a failure anywhere below must run post_stop or the sidecar
	// leaks and conflicts with the next activation. Defer ordering runs this
	// after the in-branch sup.Stop calls, matching stopActive's
	// backend-down-then-post_stop order. Disarmed on success.
	startOK := false
	defer func() {
		if startOK {
			return
		}
		// Fresh context: startCtx may be the very thing that failed
		// (cancelled/expired), and CommandContext would kill the hooks
		// instantly — same reason the error paths use context.Background()
		// for d.sup.Stop.
		hookCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = runHooks(hookCtx, p.Name, "post_stop", p.Hooks.PostStop, false)
	}()

	// External backends have no process to launch: the router (llama-swap)
	// on the proxy port owns lifecycle + placement and JIT-loads the model
	// on first request. vibe's contribution shrinks to a cheap catalog
	// check — is the router up, does it know this model — plus the frontend
	// wiring below (which reads alias/context from the same backend def as
	// always, so client-facing config is unchanged).
	external := p.Backend.External
	var (
		spec        supervisor.LaunchSpec
		port        int
		backendAddr string
	)
	if external {
		if err := d.checkExternalBackendReady(startCtx, p); err != nil {
			slog.Warn("external backend readiness check failed", "profile", p.Name, "err", err)
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
		backendAddr = externalBackendAddr
		if p.Backend.ComfyUI != nil {
			// External ComfyUI is a llama-swap swap tenant reached through the
			// router's raw passthrough. Unlike LLM backends (whose clients use
			// the OpenAI surface at ${VIBE_API}), vamp's comfyui executor dials
			// BackendAddr directly — so it must carry the real upstream URL,
			// not the "external (router)" marker.
			backendAddr = fmt.Sprintf("http://127.0.0.1:%d/upstream/%s", d.cfg.ProxyPort, canonicalRouterModelID(p))
		}
		slog.Info("starting profile (external backend)",
			"profile", p.Name, "mode", p.ResolvedMode(),
			"alias", externalBackendAlias(p), "router_port", d.cfg.ProxyPort)
	} else {
		// Dispatch by backend kind to build the launch spec + pick a port.
		var err error
		spec, port, err = d.buildLaunchSpec(p)
		if err != nil {
			return nil, err
		}

		if err := d.sup.Start(startCtx, spec, port); err != nil {
			slog.Error("supervisor start failed", "profile", p.Name, "err", err)
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("start backend: %w", err))
		}
		backendAddr = d.sup.Status().Addr

		// Wire the proxy for llama_server and http_server backends — both
		// front a generic HTTP API that vamp / external tools reach through
		// the proxy. ComfyUI is reached directly via Status.BackendAddr
		// (workflow API is one big POST, no streaming, no profile-managed
		// state worth proxying) and ships its own UI so there's no frontend.
		if p.Backend.LlamaServer != nil || p.Backend.HTTPServer != nil || p.Backend.TabbyAPI != nil || p.Backend.MLXServer != nil {
			backendURL, err := url.Parse(backendAddr)
			if err != nil {
				_ = d.sup.Stop(context.Background())
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("parse backend url: %w", err))
			}
			d.prx.SetBackend(backendURL)
			// mlx_lm.server can only answer to the path it was launched
			// with, so the proxy carries the alias <-> path translation for
			// it. Cleared for every other backend kind, which advertise
			// their own alias.
			if m := p.Backend.MLXServer; m != nil {
				d.prx.SetModelRewrite(m.Alias, m.ModelDir)
			} else {
				d.prx.SetModelRewrite("", "")
			}
		}
	}
	var fr *frontend.Result
	// Frontend activation for every backend kind that serves an OpenAI
	// surface a client can be pointed at — each may also have a separate
	// UI/client process to launch (e.g. Open WebUI via docker-compose).
	// cloud_peer included: the peer has no local process, but the rendered
	// config is the entire deliverable there, and omitting it left a start
	// reporting "ready" with nothing written.
	if p.Backend.LlamaServer != nil || p.Backend.TabbyAPI != nil || p.Backend.MLXServer != nil || p.Backend.CloudPeer != nil {
		if p.Frontend.Kind != "" {
			// Pre-create the per-profile frontend state dir so docker-compose
			// bind mounts (and any other path the profile points inside it)
			// don't fail with a "no such directory" race the first time a
			// profile activates. Cheap idempotent mkdir.
			stateDir := paths.FrontendStateDir(p.Name)
			if err := os.MkdirAll(stateDir, 0o755); err != nil {
				d.rollbackBackendStart(external)
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create frontend state dir %s: %w", stateDir, err))
			}

			// Still ProxyPort when DisableProxy is set: the external
			// router owning that port serves the same OpenAI contract,
			// so rendered frontend configs keep pointing at it — unless
			// ClientAPIURL redirects clients to a fleet front instead.
			vibeAPI := fmt.Sprintf("http://127.0.0.1:%d/v1", d.cfg.ProxyPort)
			if d.cfg.ClientAPIURL != "" {
				vibeAPI = strings.TrimRight(d.cfg.ClientAPIURL, "/") + "/v1"
			}
			alias, ctxLen := frontendModelVars(p, external)
			ectx := profile.ExpandContext{
				VibeAPI:      vibeAPI,
				ModelAlias:   alias,
				ModelContext: ctxLen,
				VibeStateDir: paths.StateHome(),
				VibeSearch:   strings.TrimRight(d.cfg.SearchURL, "/"),
			}
			var err error
			if req.Msg.Foreground {
				fr, err = frontend.ActivateForeground(startCtx, p, ectx)
			} else {
				fr, err = frontend.ActivateWithContext(startCtx, p, ectx)
			}
			if err != nil {
				d.rollbackBackendStart(external)
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("activate frontend: %w", err))
			}
		}
	}

	d.mu.Lock()
	d.active = p
	d.startTime = time.Now()
	d.frontend = fr
	d.mu.Unlock()

	// Co-start declared sidecars. Best-effort: runs after the active
	// profile is up so a sidecar failure can't abort it, and a slow
	// sidecar doesn't delay dropping the user into the frontend.
	d.startCompanions(startCtx, p)

	// Auto-respawn watcher: when the supervised process exits mid-life
	// (after reaching Ready) without an operator-initiated Stop, the
	// backend probably crashed (e.g. llama-server SIGABRT from a flaky
	// CUDA kernel mid-foreach). Without this, the daemon stays in
	// "Running but !Ready" forever and every vamp retry returns 502.
	// We try up to maxBackendRespawns within respawnWindow; on the last
	// failed restart we clear d.active so the next vamp EnsureActive
	// goes through a clean start path. External backends have nothing to
	// watch — the router restarts (or JIT-reloads) its own children.
	if !external {
		go d.watchBackendForRespawn(p.Name, spec, port)
	}

	resp := &vibev1.StartResponse{Status: d.protoStatus()}
	if fr != nil {
		slog.Info("profile started", "profile", p.Name, "backend", backendAddr, "wrote", fr.WroteFile)
		resp.Frontend = &vibev1.FrontendInfo{
			WroteFile:       fr.WroteFile,
			RestartRequired: fr.RestartRequired,
			EnvVars:         fr.Env,
			Kind:            p.Frontend.Kind,
			Url:             p.Frontend.BrowserURL(),
			Args:            fr.Args,
		}
	} else {
		slog.Info("profile started", "profile", p.Name, "backend", backendAddr)
	}
	startOK = true
	return connect.NewResponse(resp), nil
}

const (
	// maxBackendRespawns is the cap on automatic restarts of a crashed
	// backend within respawnWindow. We size it for a worst-case 4-5 h
	// pipeline (a long-form-distilling vision phase) where the
	// llama.cpp flash-attn pool-VMM kernel SIGABRTs every ~3-4 min on
	// the current 595.71.05 / kernel 7.0.9 combo. 60 respawns / 30 min
	// = roughly one respawn every 30 s before the budget trips, which
	// still catches a deterministic-on-every-restart crash (would burn
	// the budget in seconds) while surviving the flaky CUDA hiccup
	// case. Lower this once the upstream FA kernel is fixed.
	maxBackendRespawns = 60
	respawnWindow      = 30 * time.Minute
)

// watchBackendForRespawn watches the active supervisor for unexpected
// exits and re-launches the same spec on the same port if the operator
// hasn't manually stopped the profile. Exits on:
//   - clean Stop (d.active cleared)
//   - profile change (d.active.Name != name)
//   - respawn budget exhausted
func (d *Daemon) watchBackendForRespawn(name string, spec supervisor.LaunchSpec, port int) {
	respawns := 0
	windowStart := time.Now()
	for {
		stopped := d.sup.Stopped()
		if stopped == nil {
			return
		}
		<-stopped

		// Serialize the respawn decision+launch against operator Start/Stop:
		// a Stop that lands between the stillActive check and Start would
		// otherwise resurrect a backend the operator stopped. Lock (not
		// TryLock) so an in-flight Stop completes first; we then observe the
		// cleared d.active and return.
		d.startMu.Lock()
		d.mu.Lock()
		stillActive := d.active != nil && d.active.Name == name
		d.mu.Unlock()
		if !stillActive {
			// Operator-initiated Stop or profile switch: nothing to do.
			d.startMu.Unlock()
			return
		}
		if time.Since(windowStart) > respawnWindow {
			respawns = 0
			windowStart = time.Now()
		}
		if respawns >= maxBackendRespawns {
			slog.Error("backend respawn budget exhausted; giving up",
				"profile", name, "respawns", respawns, "window", respawnWindow)
			// Full teardown, not just clearing the active slot: leaving
			// d.frontend set would orphan the compose stack / managed binary
			// forever (a later Start overwrites the only reference), and a
			// managed frontend keeps holding its port so the next
			// activation's wait_for can pass against the stale instance.
			// Bounded context — nobody waits on this goroutine. Holding
			// d.startMu through the teardown is safe: Start/Stop TryLock and
			// fail fast.
			d.mu.Lock()
			p := d.active
			fr := d.frontend
			d.active = nil
			d.frontend = nil
			d.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			if fr != nil {
				if err := frontend.Deactivate(ctx, fr); err != nil {
					slog.Warn("frontend deactivate after respawn budget exhaustion failed",
						"profile", name, "err", err)
				}
			}
			d.prx.SetBackend(nil)
			if p != nil {
				d.stopCompanions(ctx, p)
				_ = runHooks(ctx, p.Name, "post_stop", p.Hooks.PostStop, false)
			}
			cancel()
			d.startMu.Unlock()
			return
		}
		respawns++
		slog.Warn("backend exited unexpectedly; auto-respawning",
			"profile", name, "attempt", respawns, "of", maxBackendRespawns)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		err := d.sup.Start(ctx, spec, port)
		cancel()
		if err != nil {
			d.startMu.Unlock()
			slog.Error("backend respawn failed", "profile", name, "attempt", respawns, "err", err)
			// A failed Start leaves supervisor in StateExited with its
			// `stopped` channel already closed; the next loop iteration
			// would unblock instantly and we'd burn the entire respawn
			// budget in <1 s on a deterministic config error
			// (typo in model path, missing binary, etc.). Sleep a beat
			// so the operator has time to spot the recurring failure
			// in the log before the budget trips.
			time.Sleep(2 * time.Second)
			continue
		}
		// Re-wire the proxy at the same backend URL — Supervisor.Start
		// reuses the same port (we pass it in), so the URL is identical
		// to the original, but SetBackend is cheap and avoids any
		// staleness if the proxy was cleared during a transient.
		backendURL, perr := url.Parse(d.sup.Status().Addr)
		if perr == nil && backendURL != nil {
			d.prx.SetBackend(backendURL)
		}
		d.startMu.Unlock()
		slog.Info("backend respawned", "profile", name, "addr", d.sup.Status().Addr)
	}
}

func (d *Daemon) Stop(ctx context.Context, req *connect.Request[vibev1.StopRequest]) (*connect.Response[vibev1.StopResponse], error) {
	if !d.startMu.TryLock() {
		return nil, connect.NewError(connect.CodeAborted, errors.New("another start/stop is in progress"))
	}
	defer d.startMu.Unlock()

	name := strings.TrimSpace(req.Msg.GetProfile())
	// Snapshot the active profile name under the lock: the respawn watcher
	// writes d.active (and nils it when the budget trips) under d.mu, so a
	// bare read in the switch races it (CI runs -race).
	d.mu.Lock()
	activeName := ""
	if d.active != nil {
		activeName = d.active.Name
	}
	d.mu.Unlock()

	// Empty or active-name targets the active profile (legacy behavior).
	// The active-name path lets callers be explicit without having to know
	// which mode the profile is in. Handled ahead of the name switch so the
	// switch stays a simple tagged dispatch.
	if name == "" || name == activeName {
		if err := d.stopActive(ctx); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		return connect.NewResponse(&vibev1.StopResponse{Status: d.protoStatus()}), nil
	}

	switch name {
	case "all":
		// Best-effort stop everything. The first error gets surfaced
		// but every entry is attempted so a partial failure doesn't
		// leave half the stack up. Active goes last so a service stop
		// failure doesn't dangle the frontend.
		var firstErr error
		d.mu.Lock()
		names := make([]string, 0, len(d.services))
		for n := range d.services {
			names = append(names, n)
		}
		d.mu.Unlock()
		for _, n := range names {
			if err := d.stopService(ctx, n); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if err := d.stopActive(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
		if firstErr != nil {
			return nil, connect.NewError(connect.CodeInternal, firstErr)
		}
	default:
		// Anything else is a service name.
		if err := d.stopService(ctx, name); err != nil {
			if errors.Is(err, errServiceNotFound) {
				return nil, connect.NewError(connect.CodeNotFound, err)
			}
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	}
	return connect.NewResponse(&vibev1.StopResponse{Status: d.protoStatus()}), nil
}

// errServiceNotFound is returned by stopService when the named
// service isn't registered. Stop converts this to CodeNotFound so
// CLI callers can distinguish "wrong name" from "stop genuinely
// failed".
var errServiceNotFound = errors.New("service not found")

// stopService tears down a single service-mode profile. The entry
// is removed from d.services BEFORE stopping the supervisor so the
// per-service respawn watcher sees no registration and bails out
// cleanly (same pattern stopActive uses for d.active).
func (d *Daemon) stopService(ctx context.Context, name string) error {
	d.mu.Lock()
	svc, ok := d.services[name]
	if !ok {
		d.mu.Unlock()
		return fmt.Errorf("%w: %q", errServiceNotFound, name)
	}
	delete(d.services, name)
	d.mu.Unlock()

	if alias := serviceRouteAlias(svc.profile); alias != "" {
		d.prx.RemoveRoute(alias)
	}

	slog.Info("stopping service", "profile", name)
	if err := svc.sup.Stop(ctx); err != nil {
		return fmt.Errorf("stop service %q: %w", name, err)
	}
	return nil
}

func (d *Daemon) Shutdown(_ context.Context, _ *connect.Request[vibev1.ShutdownRequest]) (*connect.Response[vibev1.ShutdownResponse], error) {
	go func() {
		// Trigger after the response flushes.
		time.Sleep(50 * time.Millisecond)
		d.shutdownOnce.Do(func() { close(d.shutdown) })
	}()
	return connect.NewResponse(&vibev1.ShutdownResponse{}), nil
}

func (d *Daemon) Logs(_ context.Context, req *connect.Request[vibev1.LogsRequest]) (*connect.Response[vibev1.LogsResponse], error) {
	name := strings.TrimSpace(req.Msg.GetProfile())
	// Empty / "active" → the active profile's supervisor (legacy
	// behavior). Anything else → look up the named service.
	if name == "" || name == "active" {
		return connect.NewResponse(&vibev1.LogsResponse{Lines: d.sup.Logs()}), nil
	}
	d.mu.Lock()
	svc, ok := d.services[name]
	d.mu.Unlock()
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound,
			fmt.Errorf("no running service named %q (use `vibe ps` to list)", name))
	}
	return connect.NewResponse(&vibev1.LogsResponse{Lines: svc.sup.Logs()}), nil
}

func (d *Daemon) Pull(ctx context.Context, req *connect.Request[vibev1.PullRequest], stream *connect.ServerStream[vibev1.PullProgress]) error {
	if !d.startMu.TryLock() {
		return connect.NewError(connect.CodeAborted, errors.New("another start/stop/pull is in progress"))
	}
	defer d.startMu.Unlock()

	name := strings.TrimSpace(req.Msg.Profile)
	if name == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("profile name required"))
	}
	p, err := loadProfileByName(name)
	if err != nil {
		return connect.NewError(connect.CodeNotFound, err)
	}
	// ComfyUI manages its own model assets (ComfyUI-Manager + manual placement
	// under models/); vibe has nothing to pull. Surface a clear DONE so the
	// CLI doesn't spin forever waiting for download phase messages.
	if p.Backend.ComfyUI != nil {
		return stream.Send(&vibev1.PullProgress{
			Phase:   vibev1.PullProgress_PHASE_DONE,
			Message: "no model file to pull (ComfyUI manages its own model assets)",
		})
	}
	// A cloud peer's weights are the provider's. `vibe start` pulls before it
	// starts, so without this a peer profile fails there rather than at any
	// point that has to do with what the user asked for.
	if p.Backend.CloudPeer != nil {
		return stream.Send(&vibev1.PullProgress{
			Phase:   vibev1.PullProgress_PHASE_DONE,
			Message: "no model file to pull (cloud_peer models live at the provider)",
		})
	}
	// http_server backends bring their own model artefacts (baked into the
	// image or mounted via volumes — kokoro-fastapi pulls weights at first
	// run into the kokoro-models volume). Nothing for vibe to pull.
	if p.Backend.HTTPServer != nil {
		return stream.Send(&vibev1.PullProgress{
			Phase:   vibev1.PullProgress_PHASE_DONE,
			Message: "no model file to pull (http_server backends manage their own artefacts)",
		})
	}
	// tabby_api models are HF snapshots (multi-shard directories), not
	// single files. We shell out to the venv's `huggingface-cli` since
	// it ships with exllamav3 anyway, dodging the need for a Go-side
	// snapshot downloader. Skip when no huggingface block.
	if p.Backend.TabbyAPI != nil {
		t := p.Backend.TabbyAPI
		if t.Huggingface == nil {
			return stream.Send(&vibev1.PullProgress{
				Phase:   vibev1.PullProgress_PHASE_DONE,
				Message: "no huggingface block; nothing to pull (tabby_api: pre-place the EXL3 snapshot at model_dir)",
			})
		}
		if err := stream.Send(&vibev1.PullProgress{Phase: vibev1.PullProgress_PHASE_RESOLVING}); err != nil {
			return err
		}
		// huggingface_hub renamed the CLI from `huggingface-cli` to `hf`
		// — the venv ships the new name. Use it directly; the old name
		// just prints a deprecation banner and exits non-zero.
		hfCli := filepath.Join(t.Venv, "bin", "hf")
		args := []string{"download", t.Huggingface.Repo, "--local-dir", t.ModelDir}
		if t.Huggingface.Revision != "" {
			args = append(args, "--revision", t.Huggingface.Revision)
		}
		slog.Info("pulling tabby_api snapshot", "profile", p.Name,
			"repo", t.Huggingface.Repo, "revision", t.Huggingface.Revision,
			"dest", t.ModelDir)
		cmd := exec.CommandContext(ctx, hfCli, args...)
		out, runErr := cmd.CombinedOutput()
		if runErr != nil {
			return connect.NewError(connect.CodeInternal,
				fmt.Errorf("huggingface-cli download: %w (output: %s)", runErr, strings.TrimSpace(string(out))))
		}
		return stream.Send(&vibev1.PullProgress{
			Phase:   vibev1.PullProgress_PHASE_DONE,
			Message: fmt.Sprintf("downloaded snapshot to %s", t.ModelDir),
		})
	}
	// MLX models are HF snapshots like tabby_api's, so the same shell-out to
	// the venv's `hf` applies — mlx-lm depends on huggingface_hub, so the
	// CLI is always present in a venv that can run the backend at all.
	if p.Backend.MLXServer != nil {
		m := p.Backend.MLXServer
		if m.Huggingface == nil {
			return stream.Send(&vibev1.PullProgress{
				Phase:   vibev1.PullProgress_PHASE_DONE,
				Message: "no huggingface block; nothing to pull (mlx_server: pre-place the MLX snapshot at model_dir)",
			})
		}
		if err := stream.Send(&vibev1.PullProgress{Phase: vibev1.PullProgress_PHASE_RESOLVING}); err != nil {
			return err
		}
		hfCli := filepath.Join(m.Venv, "bin", "hf")
		args := []string{"download", m.Huggingface.Repo, "--local-dir", m.ModelDir}
		if m.Huggingface.Revision != "" {
			args = append(args, "--revision", m.Huggingface.Revision)
		}
		slog.Info("pulling mlx_server snapshot", "profile", p.Name,
			"repo", m.Huggingface.Repo, "revision", m.Huggingface.Revision,
			"dest", m.ModelDir)
		cmd := exec.CommandContext(ctx, hfCli, args...)
		out, runErr := cmd.CombinedOutput()
		if runErr != nil {
			return connect.NewError(connect.CodeInternal,
				fmt.Errorf("hf download: %w (output: %s)", runErr, strings.TrimSpace(string(out))))
		}
		return stream.Send(&vibev1.PullProgress{
			Phase:   vibev1.PullProgress_PHASE_DONE,
			Message: fmt.Sprintf("downloaded snapshot to %s", m.ModelDir),
		})
	}
	if p.Backend.LlamaServer == nil {
		return connect.NewError(connect.CodeFailedPrecondition,
			errors.New("profile has no backend"))
	}
	m := p.Backend.LlamaServer
	if m.Huggingface == nil {
		return stream.Send(&vibev1.PullProgress{
			Phase:   vibev1.PullProgress_PHASE_DONE,
			Message: "no huggingface block; nothing to pull",
		})
	}

	if err := stream.Send(&vibev1.PullProgress{Phase: vibev1.PullProgress_PHASE_RESOLVING}); err != nil {
		return err
	}

	// pullOne fetches a single repo file into dest, streaming download
	// progress over the same RPC stream. Returns the file's final size
	// on disk and whether the download actually transferred bytes (vs.
	// short-circuiting on a cached copy).
	pullOne := func(file, dest string) (int64, bool, error) {
		spec := hfdownload.Spec{
			Repo:     m.Huggingface.Repo,
			File:     file,
			Revision: m.Huggingface.Revision,
		}
		slog.Info("pulling model file", "profile", p.Name, "repo", spec.Repo, "file", spec.File, "dest", dest)
		var bytesFlowed bool
		progress := func(downloaded, total int64) {
			bytesFlowed = true
			_ = stream.Send(&vibev1.PullProgress{
				Phase:           vibev1.PullProgress_PHASE_DOWNLOADING,
				DownloadedBytes: downloaded,
				TotalBytes:      total,
			})
		}
		if err := hfdownload.Download(ctx, spec, dest, progress); err != nil {
			return 0, false, err
		}
		var size int64
		if info, statErr := os.Stat(dest); statErr == nil {
			size = info.Size()
		}
		return size, bytesFlowed, nil
	}

	modelSize, modelFlowed, err := pullOne(m.Huggingface.File, m.Path)
	if err != nil {
		slog.Error("download failed", "profile", p.Name, "err", err)
		return connect.NewError(connect.CodeInternal, fmt.Errorf("download model: %w", err))
	}

	var mmprojSize int64
	mmprojFlowed := false
	if m.Huggingface.MMProjFile != "" {
		size, flowed, err := pullOne(m.Huggingface.MMProjFile, m.MMProj)
		if err != nil {
			slog.Error("mmproj download failed", "profile", p.Name, "err", err)
			return connect.NewError(connect.CodeInternal, fmt.Errorf("download mmproj: %w", err))
		}
		mmprojSize = size
		mmprojFlowed = flowed
	}

	var draftSize int64
	draftFlowed := false
	if m.Huggingface.DraftFile != "" {
		size, flowed, err := pullOne(m.Huggingface.DraftFile, m.DraftModel)
		if err != nil {
			slog.Error("draft model download failed", "profile", p.Name, "err", err)
			return connect.NewError(connect.CodeInternal, fmt.Errorf("download draft model: %w", err))
		}
		draftSize = size
		draftFlowed = flowed
	}

	total := modelSize + mmprojSize + draftSize
	msg := "complete"
	if !modelFlowed && !mmprojFlowed && !draftFlowed {
		msg = "already cached"
	}
	slog.Info("pull done", "profile", p.Name, "model", m.Path, "model_size", modelSize, "mmproj", m.MMProj, "mmproj_size", mmprojSize, "draft", m.DraftModel, "draft_size", draftSize, "flowed", modelFlowed || mmprojFlowed || draftFlowed)
	return stream.Send(&vibev1.PullProgress{
		Phase:           vibev1.PullProgress_PHASE_DONE,
		DownloadedBytes: total,
		TotalBytes:      total,
		Message:         msg,
	})
}

// ─── Internals ──────────────────────────────────────────────────────────────

func (d *Daemon) stopActive(ctx context.Context) error {
	d.mu.Lock()
	active := d.active
	fr := d.frontend
	// Clear d.active up-front so the watchBackendForRespawn goroutine
	// — which fires the moment the supervisor exits — sees no active
	// profile and bails out instead of respawning the backend right
	// out from under our Stop call. Without this, an EnsureActive
	// switch (Stop A, then Start B) races: the watcher restarts A on
	// the same port before Start B can grab the supervisor, and the
	// switch fails with "supervisor: already running".
	d.active = nil
	d.frontend = nil
	d.mu.Unlock()
	if active != nil {
		slog.Info("stopping profile", "profile", active.Name)
	}
	// Tear down the frontend first. For docker-compose this issues
	// `docker compose down`, which may make requests against the proxy on
	// its way out — better to fail those requests cleanly than to surface
	// 502s from a half-stopped proxy/supervisor. Runs even when active is
	// nil: dropping the snapshot here would discard the only reference to
	// the frontend's teardown, orphaning it forever.
	if fr != nil {
		if err := frontend.Deactivate(ctx, fr); err != nil {
			// Log and continue: leaving a stack up is bad, but failing to
			// stop the supervisor is worse. The user can still `docker
			// compose down` by hand if needed.
			slog.Warn("frontend deactivate failed", "err", err)
		}
	}
	if active == nil {
		return nil
	}
	if active.Backend.External {
		// No backend process to stop: the router's TTL owns model unload,
		// so a vibe stop only takes down the frontend (already done above)
		// and the profile's sidecars/hooks.
		slog.Info("external backend left to the router", "profile", active.Name)
	} else {
		if err := d.sup.Stop(ctx); err != nil {
			return err
		}
		d.prx.SetBackend(nil)
	}
	// Tear down the sidecars this profile co-started (best-effort).
	d.stopCompanions(ctx, active)
	// post_stop hooks run after the frontend and backend are down (best-effort).
	_ = runHooks(ctx, active.Name, "post_stop", active.Hooks.PostStop, false)
	return nil
}

// runHooks executes a profile's lifecycle hook commands sequentially, each via
// `sh -c` with the daemon's environment. When abortOnErr is true (pre_start),
// the first failing hook returns an error so the caller aborts the start;
// otherwise (post_stop) failures are logged and the remaining hooks still run.
func runHooks(ctx context.Context, profileName, phase string, cmds []string, abortOnErr bool) error {
	for i, cmd := range cmds {
		c := exec.CommandContext(ctx, "sh", "-c", cmd)
		c.Env = os.Environ()
		out, err := c.CombinedOutput()
		if err != nil {
			slog.Warn("lifecycle hook failed",
				"profile", profileName, "phase", phase, "index", i,
				"cmd", cmd, "err", err, "output", strings.TrimSpace(string(out)))
			if abortOnErr {
				return fmt.Errorf("%s hook %d (%q) failed: %w", phase, i, cmd, err)
			}
			continue
		}
		slog.Info("lifecycle hook ran", "profile", profileName, "phase", phase, "cmd", cmd)
	}
	return nil
}

func (d *Daemon) protoStatus() *vibev1.Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.sup.Status()
	s := &vibev1.Status{
		Running:     d.active != nil,
		Ready:       st.State == supervisor.StateReady,
		BackendAddr: st.Addr,
		ProxyAddr:   fmt.Sprintf("http://127.0.0.1:%d", d.cfg.ProxyPort),
		Pid:         int32(st.PID),
	}
	if d.active != nil {
		s.Profile = d.active.Name
		s.StartedAt = timestamppb.New(d.startTime)
		if d.active.Backend.External {
			// The supervisor never ran for this profile, so its state is
			// stale (whatever the previous profile left behind) — mask it.
			// Readiness was verified against the router catalog at start;
			// after that, JIT load and TTL unload are the router's business,
			// so "running" IS "ready". Parallel stays 0 too: the router (not
			// this def) owns concurrency, and a leaked default of 1 would
			// cap vamp's foreach fan-out at a single request.
			s.Ready = true
			s.BackendAddr = externalBackendAddr
			if d.active.Backend.ComfyUI != nil {
				// vamp's comfyui executor dials BackendAddr directly, so the
				// external mask must carry the router's raw passthrough URL
				// (mirrors the Start-path computation) rather than the marker.
				s.BackendAddr = fmt.Sprintf("http://127.0.0.1:%d/upstream/%s", d.cfg.ProxyPort, canonicalRouterModelID(d.active))
			}
			s.Pid = 0
		} else if ls := d.active.Backend.LlamaServer; ls != nil {
			// Surface the llama_server `parallel` value so clients (notably
			// vamp) can cap their own foreach concurrency at the actual
			// back-pressure point. ComfyUI-backed profiles leave Parallel at
			// its zero value: the parallel concept is llama-server-specific.
			s.Parallel = int32(ls.Parallel)
		}
	}
	if d.frontend != nil {
		s.FrontendEnv = d.frontend.Env
	}
	return s
}

// servicesStatus snapshots every running service into a list of
// vibev1.Status entries — one per service. Sorted by name so CLI
// output is stable across calls. Returned with the daemon lock NOT
// held; caller must not race against starts/stops. Status RPC is
// the only caller today.
func (d *Daemon) servicesStatus() []*vibev1.Status {
	d.mu.Lock()
	names := make([]string, 0, len(d.services))
	for n := range d.services {
		names = append(names, n)
	}
	d.mu.Unlock()
	sort.Strings(names)

	out := make([]*vibev1.Status, 0, len(names))
	for _, n := range names {
		d.mu.Lock()
		svc, ok := d.services[n]
		d.mu.Unlock()
		if !ok {
			continue // raced with a Stop; skip
		}
		st := svc.sup.Status()
		s := &vibev1.Status{
			Running:     true,
			Ready:       st.State == supervisor.StateReady,
			Profile:     svc.profile.Name,
			StartedAt:   timestamppb.New(svc.startTime),
			BackendAddr: st.Addr,
			// ProxyAddr intentionally empty: services don't go through
			// the daemon's reverse proxy. Callers reach them by their
			// published port (BackendAddr).
			Pid: int32(st.PID),
		}
		out = append(out, s)
	}
	return out
}

// fleetDaemonInfo snapshots the daemon half of /api/fleet/state: the active
// profile plus every running service. Same data protoStatus/servicesStatus
// expose over Connect, reshaped for the JSON surface.
func (d *Daemon) fleetDaemonInfo() fleetapi.DaemonInfo {
	info := fleetapi.DaemonInfo{
		Services:      []fleetapi.ServiceInfo{},
		AuthRejected:  d.authRejected.Load(),
		GuestEnabled:  d.guestEnabled.Load(),
		GuestRejected: d.guestRejected.Load(),
	}
	d.mu.Lock()
	if d.active != nil {
		info.ActiveProfile = d.active.Name
	}
	svcs := make([]*serviceInstance, 0, len(d.services))
	for _, svc := range d.services {
		svcs = append(svcs, svc)
	}
	d.mu.Unlock()
	for _, svc := range svcs {
		st := svc.sup.Status()
		info.Services = append(info.Services, fleetapi.ServiceInfo{
			Name:  svc.profile.Name,
			Ready: st.State == supervisor.StateReady,
			Addr:  st.Addr,
			Pid:   st.PID,
		})
	}
	sort.Slice(info.Services, func(i, j int) bool { return info.Services[i].Name < info.Services[j].Name })
	return info
}

// firstNonEmpty returns the first non-"" string from its arguments.
// Used by the http_server start logging path so the same slog field can
// surface either the docker image or the bare-binary path without two
// separate log lines per mode.
func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// validProfileName matches the same shape `vibe profile init` enforces:
// alphanumerics, underscore, and hyphen only. Rejecting "/" / "." /
// non-printable characters here means the daemon's control plane can't
// be tricked into reading arbitrary YAML files via path traversal in
// a Start/Pull/Status RPC's profile-name field.
var validProfileName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// validBackendName guards StartRequest.backend the same way — the control
// plane must not be able to load YAML from outside backends/ — but admits
// '.', which real backend names use (qwen3.6-27b). Dots without a path
// separator cannot escape the backends dir.
var validBackendName = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

func loadProfileByName(name string) (*profile.Profile, error) {
	if !validProfileName.MatchString(name) {
		return nil, fmt.Errorf("profile name %q is invalid (allowed: [a-zA-Z0-9_-]+)", name)
	}
	path := filepath.Join(paths.ProfilesDir(), name+".yaml")
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("profile %q not found at %s", name, path)
	}
	return profile.Load(path)
}

func writePIDFile() error {
	return os.WriteFile(paths.PIDFile(), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644)
}

// externalBackendAddr is surfaced as Status.BackendAddr for external
// backends: there is no vibe-supervised process (and no pid), so `vibe ps`
// and `vibe start` show where the model actually lives instead of a stale
// supervisor address.
const externalBackendAddr = "external (router)"

// externalBackendAlias returns the OpenAI model id an external backend is
// expected to advertise. Only the LLM-serving kinds can be external
// (enforced by profile validation); cloud_peer has no single alias — its
// model list is checked wholesale by checkExternalBackendReady.
func externalBackendAlias(p *profile.Profile) string {
	switch {
	case p.Backend.LlamaServer != nil:
		return p.Backend.LlamaServer.Alias
	case p.Backend.TabbyAPI != nil:
		return p.Backend.TabbyAPI.Alias
	case p.Backend.MLXServer != nil:
		return p.Backend.MLXServer.Alias
	}
	return ""
}

// canonicalRouterModelID is the model id the rendered llama-swap config
// serves an external backend under: the backend def's name. Falls back to
// the profile name for inline external backends and backend-synthesized
// profiles, whose Name IS the def name.
func canonicalRouterModelID(p *profile.Profile) string {
	if p.BackendRef != "" {
		return p.BackendRef
	}
	return p.Name
}

// frontendModelVars resolves ${MODEL_ALIAS} and ${MODEL_CONTEXT} for a
// profile's rendered frontend config. Extracted from Start and given a name
// because it is the exact shape this repo keeps getting wrong — a dispatch
// over backend kinds where forgetting one kind is silent. Its test walks all
// six, so a seventh kind fails a test instead of rendering an empty model id
// into somebody's harness config.
//
// Either result may legitimately be zero. That is not a fallback: an unset
// var drops out of the expansion map (profile.optionalVars) and a template
// referencing it fails naming the field that fixes it, which is the whole
// reason "" and 0 must not be rendered.
func frontendModelVars(p *profile.Profile, external bool) (alias string, ctxLen int) {
	switch {
	case p.Backend.LlamaServer != nil:
		alias, ctxLen = p.Backend.LlamaServer.Alias, p.Backend.LlamaServer.Context
	case p.Backend.TabbyAPI != nil:
		alias, ctxLen = p.Backend.TabbyAPI.Alias, p.Backend.TabbyAPI.Context
	case p.Backend.MLXServer != nil:
		// The friendly alias, not ModelDir: clients reach this backend
		// through the proxy, which rewrites the alias to the path the server
		// demands.
		alias, ctxLen = p.Backend.MLXServer.Alias, p.Backend.MLXServer.Context
	case p.Backend.CloudPeer != nil:
		// A peer's model ids ARE the router's ids — there is no separate
		// def-name indirection to resolve, so this returns before the
		// canonicalRouterModelID branch below rather than after it. Only a
		// single-model peer has an unambiguous answer; with several,
		// ${MODEL_ALIAS} stays unset and a template that wants one names the
		// model literally.
		if c := p.Backend.CloudPeer; len(c.Models) == 1 {
			alias = c.Models[0]
		}
		return alias, p.Backend.CloudPeer.Context
	}
	// comfyui and http_server fall through with both unset: neither renders a
	// frontend config (validateFrontend rejects a frontend block on both), so
	// there is no template here to expand a model id into.
	if external {
		// The rendered router config keys models by BACKEND DEF NAME — the
		// canonical model id (router-lifecycle.md §4) — because a
		// llama_server alias can be shared across def variants (base +
		// -tools) and the router can attach it to only one of them.
		// Expanding ${MODEL_ALIAS} to the def name guarantees a
		// freshly-rendered frontend config routes to exactly this
		// definition; the old aliases stay in the router config purely for
		// stale client state.
		alias = canonicalRouterModelID(p)
	}
	return alias, ctxLen
}

// rollbackBackendStart undoes the backend half of a Start whose frontend
// half failed. For a vibe-supervised backend that means stopping the child
// and unwiring the proxy; for an external backend there is nothing to undo
// — the router was never touched (the readiness check is read-only).
func (d *Daemon) rollbackBackendStart(external bool) {
	if external {
		return
	}
	_ = d.sup.Stop(context.Background())
	d.prx.SetBackend(nil)
}

// externalCatalogURL is the router whose /v1/models decides whether an
// external backend is ready: the address CLIENTS will use.
//
// This is the one readiness check that is not about a process this box
// launched — an external backend has none. What it asserts is "the model a
// rendered frontend is about to request exists where that frontend will ask
// for it", so it has to follow client_api_url when set. Probing loopback
// instead makes a fleet model that only the front serves (a cloud peer, or
// another cell's weights) fail a check it would have passed, while a local
// cell that shadows the same id passes one it should not.
func (d *Daemon) externalCatalogURL() string {
	if d.cfg.ClientAPIURL != "" {
		return strings.TrimRight(d.cfg.ClientAPIURL, "/")
	}
	return fmt.Sprintf("http://127.0.0.1:%d", d.cfg.ProxyPort)
}

// checkExternalBackendReady is the external-backend replacement for the
// supervisor's health wait: a GET on the router's /v1/models catalog,
// verifying the backend's model id (alias, or the backend/profile name) is
// served there. Deliberately NOT a completion request — the router (llama-swap)
// JIT-loads a model on its first completion, which can take minutes and
// would defeat lazy loading; the catalog read is cheap and load-free.
func (d *Daemon) checkExternalBackendReady(ctx context.Context, p *profile.Profile) error {
	alias := externalBackendAlias(p)
	base := d.externalCatalogURL()
	// Short budget independent of the caller's 5-minute start window: a
	// catalog read answers immediately when the router is up, so anything
	// slower is "router not there" and should fail fast.
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		return fmt.Errorf("external backend %q: build catalog request: %w", p.Name, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("external backend %q: router not reachable at %s (llama-swap not listening there?): %w", p.Name, base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("external backend %q: GET %s/v1/models returned %s (is a llama-swap router serving that address?)",
			p.Name, base, resp.Status)
	}
	// modelcat, not a local data[] decode: once the probe follows
	// client_api_url it can land on a front that is not this box's
	// llama-swap, and an Ollama-shaped body read as data[] yields an EMPTY
	// catalog — which here reads as "the router serves nothing" and fails a
	// start that should have passed. Parse also errors on an unreadable body
	// rather than returning an empty catalog, so "could not read" and "not
	// serving it" stay different answers.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("external backend %q: read %s/v1/models: %w", p.Name, base, err)
	}
	catalog, err := modelcat.Parse(body)
	if err != nil {
		return fmt.Errorf("external backend %q: parse %s/v1/models: %w", p.Name, base, err)
	}
	ids := catalog.IDs()
	have := make(map[string]bool, len(ids))
	for _, id := range ids {
		have[id] = true
	}
	serving := strings.Join(ids, ", ")
	if serving == "" {
		serving = "none"
	}
	// cloud_peer readiness is the whole model list: the router advertises a
	// peer's models in the merged catalog, so any missing id means the peer
	// stanza is absent or stale.
	if cp := p.Backend.CloudPeer; cp != nil {
		var missing []string
		for _, m := range cp.Models {
			if !have[m] {
				missing = append(missing, m)
			}
		}
		if len(missing) == 0 {
			return nil
		}
		return fmt.Errorf("external backend %q: peer model(s) %s missing from the router catalog at %s/v1/models (serving: %s) — re-render the llama-swap config (vibe router render)",
			p.Name, strings.Join(missing, ", "), base, serving)
	}
	// The router's catalog lists model IDS; aliases appear only with
	// llama-swap's includeAliasesInList. A profile reaches its backend via
	// backend_ref, and the A1 convention names the llama-swap model after
	// the backend def — so the ref name is the id most likely to match.
	// Try all three: alias, backend_ref, profile name (inline backends).
	wanted := []string{alias, p.BackendRef, p.Name}
	for _, w := range wanted {
		if w != "" && have[w] {
			return nil
		}
	}
	return fmt.Errorf("external backend %q: none of %q are in the router catalog at %s/v1/models (serving: %s) — add the model to the llama-swap config",
		p.Name, wanted, base, serving)
}

// buildLaunchSpec dispatches a Profile to its backend-specific spec
// builder. Returns the supervisor.LaunchSpec ready to hand to
// Supervisor.Start, the host port the proxy / external callers should
// target, and a Connect-coded error on failure. Shared between
// active-mode (the main Start path) and service-mode (startService)
// so both paths construct identical specs from the same profile.
func (d *Daemon) buildLaunchSpec(p *profile.Profile) (supervisor.LaunchSpec, int, error) {
	var (
		spec supervisor.LaunchSpec
		port int
		err  error
	)
	switch {
	case p.Backend.LlamaServer != nil:
		// Honor the profile's pinned port if set — service-mode
		// profiles that pipelines reach by well-known address need
		// the port to survive daemon restarts. Otherwise fall back
		// to PickFreePort (the legacy behavior for active profiles).
		if p.Backend.LlamaServer.Port > 0 {
			port = p.Backend.LlamaServer.Port
		} else {
			port, err = supervisor.PickFreePort()
			if err != nil {
				return spec, 0, connect.NewError(connect.CodeInternal, fmt.Errorf("pick port: %w", err))
			}
		}
		llamaBin := d.cfg.LlamaBinary
		if p.Backend.LlamaServer.Binary != "" {
			llamaBin = p.Backend.LlamaServer.Binary
		}
		spec, err = profile.LlamaServerSpec(p, llamaBin, port)
		if err != nil {
			return spec, 0, connect.NewError(connect.CodeInternal, err)
		}
		slog.Info("starting profile (llama_server)",
			"profile", p.Name, "mode", p.ResolvedMode(),
			"alias", p.Backend.LlamaServer.Alias,
			"model_file", filepath.Base(p.Backend.LlamaServer.Path),
			"context", p.Backend.LlamaServer.Context, "port", port,
			"binary", llamaBin)
	case p.Backend.ComfyUI != nil:
		port = p.Backend.ComfyUI.Port
		if port == 0 {
			port, err = supervisor.PickFreePort()
			if err != nil {
				return spec, 0, connect.NewError(connect.CodeInternal, fmt.Errorf("pick port: %w", err))
			}
		}
		spec, err = profile.ComfyUISpec(p, port)
		if err != nil {
			return spec, 0, connect.NewError(connect.CodeInternal, err)
		}
		slog.Info("starting profile (comfyui)",
			"profile", p.Name, "mode", p.ResolvedMode(),
			"dir", p.Backend.ComfyUI.Dir, "port", port)
	case p.Backend.HTTPServer != nil:
		port = p.Backend.HTTPServer.Port
		spec, err = profile.HTTPServerSpec(p, p.Name)
		if err != nil {
			return spec, 0, connect.NewError(connect.CodeInternal, err)
		}
		mode := "docker"
		if p.Backend.HTTPServer.Image == "" {
			mode = "binary"
		}
		// Same stale-container cleanup the original inline switch did:
		// if a previous run left a `vibe-<name>` container behind, a
		// fresh `docker run --name` collision aborts the start.
		if mode == "docker" {
			rm := exec.Command("docker", "rm", "-f", "vibe-"+p.Name)
			rm.Stdout = nil
			rm.Stderr = nil
			_ = rm.Run()
		}
		slog.Info("starting profile (http_server)",
			"profile", p.Name, "mode", p.ResolvedMode(),
			"docker_or_binary", mode,
			"image_or_binary", firstNonEmpty(p.Backend.HTTPServer.Image, p.Backend.HTTPServer.Binary),
			"port", port)
	case p.Backend.TabbyAPI != nil:
		port = p.Backend.TabbyAPI.Port
		spec, err = profile.TabbyAPISpec(p)
		if err != nil {
			return spec, 0, connect.NewError(connect.CodeInternal, err)
		}
		slog.Info("starting profile (tabby_api)",
			"profile", p.Name, "mode", p.ResolvedMode(),
			"alias", p.Backend.TabbyAPI.Alias,
			"model_dir", filepath.Base(p.Backend.TabbyAPI.ModelDir),
			"context", p.Backend.TabbyAPI.Context, "port", port,
			"cache_mode", firstNonEmpty(p.Backend.TabbyAPI.CacheMode, "FP16"))
	case p.Backend.MLXServer != nil:
		if p.Backend.MLXServer.Port > 0 {
			port = p.Backend.MLXServer.Port
		} else {
			port, err = supervisor.PickFreePort()
			if err != nil {
				return spec, 0, connect.NewError(connect.CodeInternal, fmt.Errorf("pick port: %w", err))
			}
		}
		spec, err = profile.MLXServerSpec(p, port)
		if err != nil {
			return spec, 0, connect.NewError(connect.CodeInternal, err)
		}
		slog.Info("starting profile (mlx_server)",
			"profile", p.Name, "mode", p.ResolvedMode(),
			"alias", p.Backend.MLXServer.Alias,
			"model_dir", filepath.Base(p.Backend.MLXServer.ModelDir),
			"context", p.Backend.MLXServer.Context, "port", port,
			"host", p.Backend.MLXServer.Host)
	default:
		return spec, 0, connect.NewError(connect.CodeFailedPrecondition,
			errors.New("profile has no backend (set backend.llama_server, backend.comfyui, backend.http_server, backend.tabby_api, or backend.mlx_server)"))
	}
	return spec, port, nil
}

// startCompanions brings up the service-mode profiles named in p.Services
// alongside the just-started active profile. Best-effort by design: a
// missing, mis-moded, or failing sidecar logs a warning and is skipped so
// the active profile (the thing the user asked for) still comes up.
// Already-running services are left untouched (idempotent), so switching
// between two active profiles that share a sidecar doesn't restart it.
//
// Safe to call while holding d.startMu (Start does): startService
// synchronises on d.mu only, not d.startMu.
func (d *Daemon) startCompanions(ctx context.Context, p *profile.Profile) {
	for _, name := range p.Services {
		d.mu.Lock()
		_, running := d.services[name]
		d.mu.Unlock()
		if running {
			continue
		}
		sp, err := loadProfileByName(name)
		if err != nil {
			slog.Warn("companion service not found; continuing without it",
				"active", p.Name, "service", name, "err", err)
			continue
		}
		if sp.ResolvedMode() != profile.ModeService {
			slog.Warn("companion is not a service-mode profile; skipping",
				"active", p.Name, "service", name, "mode", sp.ResolvedMode())
			continue
		}
		if _, err := d.startService(ctx, sp); err != nil {
			slog.Warn("companion service failed to start; continuing without it",
				"active", p.Name, "service", name, "err", err)
			continue
		}
		slog.Info("companion service started", "active", p.Name, "service", name)
	}
}

// stopCompanions tears down the sidecars a stopping active profile
// declared. Best-effort and idempotent: a sidecar already gone (or never
// started) is skipped. A service not declared here is left alone, so
// manually-started services and other profiles' sidecars are untouched.
func (d *Daemon) stopCompanions(ctx context.Context, p *profile.Profile) {
	for _, name := range p.Services {
		d.mu.Lock()
		_, running := d.services[name]
		d.mu.Unlock()
		if !running {
			continue
		}
		if err := d.stopService(ctx, name); err != nil {
			slog.Warn("companion service stop failed",
				"active", p.Name, "service", name, "err", err)
		}
	}
}

// serviceRouteAlias returns the model alias a service-mode profile
// should be routed by on the proxy, or "" if it exposes no OpenAI model
// id (only llama_server backends advertise one today).
func serviceRouteAlias(p *profile.Profile) string {
	if p == nil || p.Backend.LlamaServer == nil {
		return ""
	}
	return p.Backend.LlamaServer.Alias
}

// serviceRouteURL is the loopback upstream for a service listening on
// port, in the form the proxy's reverse-proxy expects.
func serviceRouteURL(port int) *url.URL {
	return &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", port)}
}

// startService runs a service-mode profile alongside the daemon's
// other services and (optionally) an active profile. Each service
// gets its own supervisor instance so a crash + auto-respawn in one
// service doesn't disturb the others.
//
// Service-mode profiles bypass:
//   - the d.active "already running" check (services don't compete for the active slot),
//   - the VRAM pre-flight (services are typically CPU-bound; if a service genuinely needs GPU, the caller bears responsibility for sizing),
//   - the default proxy backend wiring (the active profile owns the default upstream),
//   - frontend activation (a sidecar has no UI to launch).
//
// A service IS reachable on the proxy when it exposes a model alias
// (llama_server): startService adds a per-model route so the shared
// proxy port forwards requests carrying that "model" to this service,
// while everything else still flows to the active profile.
//
// Returns a StartResponse populated from the per-service supervisor
// status; the StatusResponse's `services` list will pick up the new
// entry on subsequent Status calls.
func (d *Daemon) startService(ctx context.Context, p *profile.Profile) (*connect.Response[vibev1.StartResponse], error) {
	d.mu.Lock()
	if _, exists := d.services[p.Name]; exists {
		d.mu.Unlock()
		return nil, connect.NewError(connect.CodeAlreadyExists,
			fmt.Errorf("service %q is already running; stop it first with `vibe stop %s`", p.Name, p.Name))
	}
	d.mu.Unlock()

	spec, port, err := d.buildLaunchSpec(p)
	if err != nil {
		return nil, err
	}

	sup := supervisor.New()
	if err := sup.Start(ctx, spec, port); err != nil {
		slog.Error("service supervisor start failed", "profile", p.Name, "err", err)
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("start service: %w", err))
	}

	svc := &serviceInstance{
		profile:   p,
		sup:       sup,
		startTime: time.Now(),
		port:      port,
	}
	d.mu.Lock()
	d.services[p.Name] = svc
	d.mu.Unlock()

	// Expose this service on the proxy by its model alias so callers can
	// reach it on the shared proxy port (selected by the request's
	// "model"), not just the service's published port directly. Only
	// llama_server services advertise an OpenAI model id worth routing.
	if alias := serviceRouteAlias(p); alias != "" {
		d.prx.AddRoute(alias, serviceRouteURL(port))
	}

	// Service-specific auto-respawn watcher: same shape as the active
	// watcher, but parameterised on the service's own supervisor +
	// its slot in d.services. Crash in one service doesn't tear down
	// the others (each has its own supervisor; the active is on yet
	// a third).
	go d.watchServiceForRespawn(p.Name, sup, spec, port)

	// Return the SERVICE's status, not the active-profile status —
	// d.protoStatus() would surface the active slot (empty in this
	// case) and confuse the CLI. The Profile / BackendAddr fields
	// in StartResponse are how `vibe start` decides what to print.
	st := sup.Status()
	resp := &vibev1.StartResponse{Status: &vibev1.Status{
		Running:     true,
		Ready:       st.State == supervisor.StateReady,
		Profile:     p.Name,
		StartedAt:   timestamppb.New(svc.startTime),
		BackendAddr: st.Addr,
		// ProxyAddr left empty: services don't go through the proxy.
		Pid: int32(st.PID),
	}}
	slog.Info("service started", "profile", p.Name, "backend", st.Addr)
	return connect.NewResponse(resp), nil
}

// watchServiceForRespawn parallels watchBackendForRespawn but is
// scoped to a single service's supervisor and slot in d.services.
// Exits when the service is stopped by name (entry removed) or when
// the respawn budget is exhausted (entry cleared).
func (d *Daemon) watchServiceForRespawn(name string, sup *supervisor.Supervisor, spec supervisor.LaunchSpec, port int) {
	respawns := 0
	windowStart := time.Now()
	for {
		stopped := sup.Stopped()
		if stopped == nil {
			return
		}
		<-stopped

		// Serialize against operator Start/Stop the same way the backend
		// watcher does: a stopService that lands after the registration
		// check but before Start would otherwise resurrect a deregistered
		// service. Lock (not TryLock) so an in-flight stop completes first.
		d.startMu.Lock()
		d.mu.Lock()
		_, stillRegistered := d.services[name]
		d.mu.Unlock()
		if !stillRegistered {
			d.startMu.Unlock()
			return // operator-initiated stop
		}
		if time.Since(windowStart) > respawnWindow {
			respawns = 0
			windowStart = time.Now()
		}
		if respawns >= maxBackendRespawns {
			slog.Error("service respawn budget exhausted; deregistering",
				"profile", name, "respawns", respawns, "window", respawnWindow)
			d.mu.Lock()
			svc := d.services[name]
			delete(d.services, name)
			d.mu.Unlock()
			if svc != nil {
				if alias := serviceRouteAlias(svc.profile); alias != "" {
					d.prx.RemoveRoute(alias)
				}
			}
			d.startMu.Unlock()
			return
		}
		respawns++
		slog.Warn("service exited unexpectedly; auto-respawning",
			"profile", name, "attempt", respawns)
		startCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		err := sup.Start(startCtx, spec, port)
		cancel()
		d.startMu.Unlock()
		if err != nil {
			slog.Error("service respawn failed", "profile", name, "attempt", respawns, "err", err)
			// Mirror the backend watcher: a transient relaunch failure
			// (port in TIME_WAIT, GPU momentarily busy) shouldn't
			// permanently deregister the service. Sleep a beat and retry
			// within the respawn budget; only the budget exhaustion path
			// above deregisters.
			time.Sleep(2 * time.Second)
			continue
		}
	}
}
