// Package fleetannounce is the cell-side half of the C3 inversion: the
// announce loop. Cells dial OUT to fleetd and say what they serve — no
// inbound port on the cell is required by the control plane after this.
// Two deployment forms share this one implementation (C3 §2): the
// in-daemon loop (cells already running a vibe daemon, reusing the
// fleet: config block) and `vibe fleet announce` slim mode (cells that
// run llama-swap without a full daemon).
//
// The loop's cardinal rule: an unreachable registry must never affect
// serving. Announce failures log once and then retry quietly with
// backoff — the control plane's death is not the data plane's.
package fleetannounce

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
	"github.com/gallowaysoftware/vibe/internal/vibe/router"
)

// Config is the announce loop's complete input.
type Config struct {
	// Cell is this box's name in hosts.yaml.
	Cell string
	// RegistryURL is fleetd's base URL.
	RegistryURL string
	// TokenFile is the path to fleetd's bearer token (read per call so
	// rotation needs no restart).
	TokenFile string
	// LlamaSwapURL is the local llama-swap base (models + states come
	// from its /running).
	LlamaSwapURL string
	// Defs are this cell's backend defs (flags fingerprints + modes).
	Defs []*profile.BackendDef
	// LlamaServerBinary matches the render's binary default so the
	// fingerprint matches the router's render byte-for-byte.
	LlamaServerBinary string
	// IntentPath persists the cell's local intent ({state, since}) across
	// restarts — the conflict rule needs the timestamp.
	IntentPath string
	// CellCmds run when a NEWER desired_intent arrives (drained/serving).
	// Empty verbs are skipped (a cell without verb config still announces
	// and echoes).
	CellCmds CellCmds
	// Versions supplies the versions block (vibe build, defs checkout
	// state). Nil-safe: unknown stays empty.
	Versions func() *fleetapi.AnnounceVersions
	// Capacity supplies VRAM/disk numbers. Nil-safe: block omitted.
	Capacity func() *fleetapi.AnnounceCapacity
	// Logger; nil → slog.Default.
	Logger *slog.Logger
	// Interval override for tests (0 = follow the registry's interval_s).
	Interval time.Duration
}

// CellCmds mirrors the daemon config's drain/resume verbs for the
// announce client's reconciliation (newer desired_intent executes).
type CellCmds struct {
	Drain  string
	Resume string
}

// Client is one cell's announce loop. seq is per-boot by design (it
// resets on cell reboot; fleetd's staleness derives from received_at,
// never from seq or the cell's clock).
type Client struct {
	cfg    Config
	http   *http.Client
	seq    uint64
	rng    *rand.Rand
	logger *slog.Logger
	// lastInterval is the cadence the registry last handed us (updated
	// each successful announce).
	lastInterval time.Duration

	mu          sync.Mutex
	localIntent fleetapi.AnnounceIntent
	// registryFailureLogged gates log-once-then-quiet on registry
	// outages; reset on the next successful announce. authRejected
	// marks when the failure was auth (401), not reachability — the
	// once-log says which.
	registryFailureLogged bool
	authRejected          bool
	// deflessWarned gates log-once for catalog models with no def.
	deflessWarned map[string]bool
}

// New builds the client and loads the persisted local intent (zero
// intent = serving since the epoch — any real request is newer... but
// so is any local operator action, which always stamps `now`).
func New(cfg Config) (*Client, error) {
	if cfg.Cell == "" || cfg.RegistryURL == "" {
		return nil, fmt.Errorf("cell and registry_url are required")
	}
	c := &Client{
		cfg:           cfg,
		http:          &http.Client{Timeout: 10 * time.Second},
		rng:           rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), uint64(os.Getpid()))),
		logger:        cfg.Logger,
		deflessWarned: map[string]bool{},
	}
	if c.logger == nil {
		c.logger = slog.Default()
	}
	c.localIntent = loadLocalIntent(cfg.IntentPath)
	return c, nil
}

// loadLocalIntent reads the cell's persisted intent; missing/corrupt
// means serving-since-epoch (any request outranks it, and a never-touched
// box has nothing to defend).
func loadLocalIntent(path string) fleetapi.AnnounceIntent {
	in := fleetapi.AnnounceIntent{State: "serving"}
	if path == "" {
		return in
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return in
	}
	_ = json.Unmarshal(data, &in)
	if in.State == "" {
		in.State = "serving"
	}
	return in
}

// SetLocalIntent records a local operator intent change (drain/resume
// verbs at the box) and persists it — the human-at-the-box side of the
// conflict rule. The daemon calls this from CellDrain/CellResume.
func (c *Client) SetLocalIntent(state string) error {
	c.mu.Lock()
	c.localIntent = fleetapi.AnnounceIntent{State: state, Since: time.Now().UTC()}
	in := c.localIntent
	c.mu.Unlock()
	return c.saveLocalIntent(in)
}

// LocalIntent returns the cell's current intent (echo side).
func (c *Client) LocalIntent() fleetapi.AnnounceIntent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.localIntent
}

func (c *Client) saveLocalIntent(in fleetapi.AnnounceIntent) error {
	if c.cfg.IntentPath == "" {
		return nil
	}
	data, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.cfg.IntentPath), 0o755); err != nil {
		return err
	}
	tmp := c.cfg.IntentPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.cfg.IntentPath)
}

// Run loops until ctx dies: announce, sleep the registry's interval
// (jittered), repeat. Registry failures backoff (1s → 60s cap) and log
// once per outage.
func (c *Client) Run(ctx context.Context) error {
	interval := c.cfg.Interval
	backoff := time.Second
	for {
		err := c.announceOnce(ctx)
		if err != nil {
			if !c.registryFailureLogged {
				if c.authRejected {
					c.logger.Warn("announce failed (auth rejected — check the token file); quiet retry with backoff", "err", err)
				} else {
					c.logger.Warn("announce failed (registry unreachable?); quiet retry with backoff", "err", err)
				}
				c.registryFailureLogged = true
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			if backoff < 60*time.Second {
				backoff *= 2
				if backoff > 60*time.Second {
					backoff = 60 * time.Second
				}
			}
			continue
		}
		c.authRejected = false
		if c.registryFailureLogged {
			c.logger.Info("announce succeeded; registry reachable again")
			c.registryFailureLogged = false
		}
		backoff = time.Second

		if c.cfg.Interval == 0 {
			interval = c.lastInterval
		}
		// ±20% jitter so a fleet reboot doesn't thunder-herd the registry.
		sleep := interval
		if sleep <= 0 {
			sleep = 15 * time.Second
		}
		jitter := time.Duration((c.rng.Float64()*0.4 - 0.2) * float64(sleep))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(sleep + jitter):
		}
	}
}

// announceOnce gathers and posts one heartbeat, then reconciles the
// response (desired intent + commands).
func (c *Client) announceOnce(ctx context.Context) error {
	req := fleetapi.AnnounceRequest{
		V:      fleetapi.AnnounceVersion,
		Cell:   c.cfg.Cell,
		Seq:    c.seq,
		Intent: c.intentEcho(),
	}
	c.seq++
	models, err := c.gatherModels(ctx)
	if err != nil {
		// A llama-swap that's down answers nothing: the cell still
		// announces (that's how "the unit is stopped" reads as an empty
		// model list instead of an absent cell).
		models = nil
	}
	req.Models = models
	if c.cfg.Capacity != nil {
		req.Capacity = c.cfg.Capacity()
	}
	if c.cfg.Versions != nil {
		req.Versions = c.cfg.Versions()
	}

	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	url := strings.TrimRight(c.cfg.RegistryURL, "/") + "/api/fleet/announce"
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	hreq.Header.Set("Content-Type", "application/json")
	if tok := c.token(); tok != "" {
		hreq.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.http.Do(hreq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		c.authRejected = true
		return fmt.Errorf("announce: HTTP 401 (auth rejected — check the token file: %s)", c.tokenProblem())
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("announce: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out fleetapi.AnnounceResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return fmt.Errorf("decode announce response: %w", err)
	}
	if out.IntervalS > 0 {
		c.lastInterval = time.Duration(out.IntervalS) * time.Second
	}
	c.reconcile(ctx, &out)
	return nil
}

// intentEcho builds the intent block for this heartbeat.
func (c *Client) intentEcho() *fleetapi.AnnounceIntent {
	c.mu.Lock()
	defer c.mu.Unlock()
	in := c.localIntent
	return &in
}

// reconcile applies the conflict rule to the registry's desired intent
// (newer wins, ties to the cell) and executes queued commands. Intent
// only changes on a SUCCESSFUL verb — a failed or unconfigured verb
// leaves the echo alone so the request stays pending and the mismatch
// keeps showing (the C2 one-writer rule: a failed drain never records).
func (c *Client) reconcile(ctx context.Context, resp *fleetapi.AnnounceResponse) {
	if d := resp.DesiredIntent; d != nil {
		c.mu.Lock()
		local := c.localIntent
		c.mu.Unlock()
		if d.Since.After(local.Since) {
			switch d.State {
			case local.State:
				// Already in the requested state: ack by advancing the
				// echo, or the request is handed back on every heartbeat
				// forever (the ghost-request livelock).
				if err := c.SetLocalIntent(d.State); err != nil {
					c.logger.Warn("intent ack not persisted; the request stays pending", "err", err)
				}
			case "drained":
				if c.runVerb(ctx, "drain", c.cfg.CellCmds.Drain) {
					if err := c.SetLocalIntent("drained"); err != nil {
						c.logger.Warn("drained intent not persisted", "err", err)
					}
				}
			case "serving":
				if c.runVerb(ctx, "resume", c.cfg.CellCmds.Resume) {
					if err := c.SetLocalIntent("serving"); err != nil {
						c.logger.Warn("serving intent not persisted", "err", err)
					}
				}
			}
		}
		// Older or same: the cell wins (we echo ours; fleetd drops its
		// stale request when it sees the newer echo).
	}
	for _, cmd := range resp.Commands {
		c.executeCommand(ctx, cmd)
	}
}

// runVerb executes a drain/resume verb via sh -c (same semantics as the
// daemon's cell_cmds: bounded, stderr reported), returning whether it
// RAN successfully — intent is stamped only then, so a failed or
// unconfigured verb keeps the request pending and the mismatch visible.
func (c *Client) runVerb(ctx context.Context, name, cmd string) bool {
	if cmd == "" {
		c.logger.Info("desired intent arrived but no cell verb is configured; request stays pending", "verb", name)
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := execSh(ctx, cmd)
	if err != nil {
		c.logger.Warn("desired-intent verb failed", "verb", name, "err", err, "output", out)
		return false
	}
	c.logger.Info("executed desired-intent verb", "verb", name)
	return true
}

// executeCommand runs one piggybacked verb (unload/warm) against the
// local llama-swap and logs the result; the NEXT announce reflects it.
func (c *Client) executeCommand(ctx context.Context, cmd fleetapi.AnnounceCommand) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var err error
	switch cmd.Verb {
	case "unload":
		err = c.postNoContent(ctx, c.cfg.LlamaSwapURL+"/api/models/unload/"+url.PathEscape(cmd.Model))
	case "warm":
		body := `{"model":` + strconv.Quote(cmd.Model) + `,"max_tokens":1,"messages":[{"role":"user","content":"warm"}]}`
		err = c.postNoContent(ctx, c.cfg.LlamaSwapURL+"/v1/chat/completions", body)
	default:
		c.logger.Warn("unknown piggyback verb", "verb", cmd.Verb)
		return
	}
	if err != nil {
		c.logger.Warn("piggyback command failed", "verb", cmd.Verb, "model", cmd.Model, "err", err)
		return
	}
	c.logger.Info("piggyback command done", "verb", cmd.Verb, "model", cmd.Model)
}

func (c *Client) postNoContent(ctx context.Context, url string, bodies ...string) error {
	var rdr io.Reader
	ct := ""
	if len(bodies) > 0 && bodies[0] != "" {
		rdr = strings.NewReader(bodies[0])
		ct = "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, rdr)
	if err != nil {
		return err
	}
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("POST %s: HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// gatherModels builds the models block: every def this cell actually
// serves — defs intersected with the local llama-swap catalog (a box
// hosting more than one cell must not let one cell inherit the others'
// unassigned defs) — with llama-swap states and flags fingerprints.
// Catalog ids with no def are drift the fingerprint can't verify: they
// announce hashless (visible, unverifiable) and log once per run.
func (c *Client) gatherModels(ctx context.Context) ([]fleetapi.AnnounceModel, error) {
	states, err := c.runningStates(ctx)
	if err != nil {
		return nil, err
	}
	catalog, err := c.catalogIDs(ctx)
	if err != nil {
		return nil, err
	}
	var out []fleetapi.AnnounceModel
	covered := map[string]bool{}
	for _, def := range c.cfg.Defs {
		if !def.Backend.External || def.Backend.CloudPeer != nil {
			continue
		}
		if !catalog[def.Name] {
			continue
		}
		covered[def.Name] = true
		state := states[def.Name]
		if state == "" {
			state = "stopped"
		}
		m := fleetapi.AnnounceModel{ID: def.Name, State: state}
		// Fingerprints only cover spec-rendered kinds (llama_server,
		// mlx_server): ModelCmd's llama path has no comfyui/http_server
		// branch — the router's own render has the same split.
		if def.Backend.LlamaServer != nil || def.Backend.MLXServer != nil {
			cmd, err := router.ModelCmd(def, router.Options{LlamaServerBinary: c.cfg.LlamaServerBinary})
			if err == nil {
				if hash, err := router.FlagsSHA256(cmd); err == nil {
					m.FlagsSHA256 = hash
				}
			}
		}
		if def.Fingerprint != "" {
			m.Fingerprint = def.Fingerprint
		}
		out = append(out, m)
	}
	for id := range catalog {
		if covered[id] {
			continue
		}
		if !c.deflessWarned[id] {
			c.logger.Warn("catalog model has no backend def — announcing unverifiable (no flags fingerprint)", "model", id)
			c.deflessWarned[id] = true
		}
		state := states[id]
		if state == "" {
			state = "stopped"
		}
		out = append(out, fleetapi.AnnounceModel{ID: id, State: state})
	}
	return out, nil
}

// catalogIDs fetches the cell's /v1/models as a set.
func (c *Client) catalogIDs(ctx context.Context) (map[string]bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.LlamaSwapURL+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /v1/models: HTTP %d", resp.StatusCode)
	}
	var wrap struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wrap); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, m := range wrap.Data {
		out[m.ID] = true
	}
	return out, nil
}

// runningStates maps model id → llama-swap state from /running.
func (c *Client) runningStates(ctx context.Context) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.LlamaSwapURL+"/running", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /running: HTTP %d", resp.StatusCode)
	}
	var wrap struct {
		Running []struct {
			Model string `json:"model"`
			State string `json:"state"`
		} `json:"running"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wrap); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, m := range wrap.Running {
		out[m.Model] = m.State
	}
	return out, nil
}

// tokenProblem describes the token situation for the 401 log line:
// missing file vs unreadable vs empty vs present-but-rejected.
func (c *Client) tokenProblem() string {
	if c.cfg.TokenFile == "" {
		return "no token_file configured"
	}
	data, err := os.ReadFile(c.cfg.TokenFile)
	if err != nil {
		return fmt.Sprintf("%s: %v", c.cfg.TokenFile, err)
	}
	if strings.TrimSpace(string(data)) == "" {
		return c.cfg.TokenFile + " is empty"
	}
	return "token present but rejected (wrong token? rotate or re-provision)"
}

func (c *Client) token() string {
	if c.cfg.TokenFile == "" {
		return ""
	}
	data, err := os.ReadFile(c.cfg.TokenFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// execSh runs a command via sh -c, returning combined output.
func execSh(ctx context.Context, cmd string) (string, error) {
	out, err := execCmd(ctx, "sh", "-c", cmd)
	return strings.TrimSpace(string(out)), err
}

// execCmd is the sh seam (tests record instead of executing).
var execCmd = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}
