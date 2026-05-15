// Package tui implements `vibe tui`, a Bubbletea-based real-time dashboard
// for the vibe daemon. The TUI polls the daemon for Status + recent logs and
// drives Start/Stop on the selected profile via the vibeclient SDK.
//
// The model here is split from the runner (tui.go) so that Update() can be
// exercised in pure-logic unit tests without spinning up a real terminal or a
// real daemon.
package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	vibev1 "github.com/gallowaysoftware/vibe/proto/vibe/v1"
)

// pollInterval is how often the TUI re-fetches Status + Logs from the daemon.
// 1s matches the spec; it's a balance between responsiveness and chattiness
// over SSH.
const pollInterval = time.Second

// maxLogLines caps the log pane to a fixed window so the view doesn't grow
// unbounded as the supervisor's ring buffer fills.
const maxLogLines = 20

// vibeAPI is the minimal slice of vibeclient that the model talks to. Stating
// it as an interface lets tests inject a stub instead of a live daemon.
type vibeAPI interface {
	Status(ctx context.Context) (*vibev1.Status, error)
	ListProfiles(ctx context.Context) ([]*vibev1.Profile, error)
	Logs(ctx context.Context) ([]string, error)
	Start(ctx context.Context, profile string) error
	Stop(ctx context.Context) error
}

// Model is the Bubbletea model for the dashboard. Exported so tests can
// construct one directly.
type Model struct {
	api       vibeAPI
	apiURL    string
	noColor   bool
	width     int
	height    int
	profiles  []*vibev1.Profile
	status    *vibev1.Status
	logs      []string
	cursor    int
	errMsg    string // last error from the daemon, surfaced in the footer
	connected bool   // false means daemon isn't reachable
	busy      string // non-empty while a Start/Stop is in flight
	quitting  bool
}

// NewModel constructs an initial Model. honorNoColor lets callers force the
// monochrome path even when the env var is unset (useful in tests / snapshot
// rendering).
func NewModel(api vibeAPI, apiURL string, honorNoColor bool) Model {
	return Model{
		api:     api,
		apiURL:  apiURL,
		noColor: honorNoColor || os.Getenv("NO_COLOR") != "",
	}
}

// --- messages ---

// tickMsg fires every pollInterval; we react by issuing a refresh.
type tickMsg time.Time

// refreshMsg carries the result of one poll cycle.
type refreshMsg struct {
	status   *vibev1.Status
	profiles []*vibev1.Profile
	logs     []string
	err      error
}

// actionDoneMsg is delivered when a Start/Stop completes (or errors).
type actionDoneMsg struct {
	action string
	err    error
}

func tick() tea.Cmd {
	return tea.Tick(pollInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// refreshCmd issues Status / ListProfiles / Logs concurrently — they're all
// cheap and the daemon serves them off the same in-memory state.
func (m Model) refreshCmd() tea.Cmd {
	api := m.api
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		var msg refreshMsg
		st, err := api.Status(ctx)
		if err != nil {
			msg.err = err
			return msg
		}
		msg.status = st
		profs, err := api.ListProfiles(ctx)
		if err != nil {
			msg.err = err
			return msg
		}
		msg.profiles = profs
		lines, err := api.Logs(ctx)
		if err != nil {
			msg.err = err
			return msg
		}
		msg.logs = lines
		return msg
	}
}

func (m Model) startCmd(profile string) tea.Cmd {
	api := m.api
	return func() tea.Msg {
		// Start can take many seconds (model load); use a generous timeout.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		err := api.Start(ctx, profile)
		return actionDoneMsg{action: "start", err: err}
	}
}

func (m Model) stopCmd() tea.Cmd {
	api := m.api
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err := api.Stop(ctx)
		return actionDoneMsg{action: "stop", err: err}
	}
}

// Init triggers an immediate refresh and starts the poll ticker.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.refreshCmd(), tick())
}

// Update is the heart of the TUI. It's pure: given (Model, Msg), it returns
// the next (Model, Cmd). Key bindings drive navigation and actions; tick and
// refresh messages drive periodic state updates.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tickMsg:
		// Schedule the next tick and trigger a refresh. We don't wait for the
		// refresh to finish before starting the next tick — if the daemon is
		// slow, we'll just drop the overlap (the in-flight refresh keeps
		// running) which is preferable to drifting the cadence.
		return m, tea.Batch(m.refreshCmd(), tick())

	case refreshMsg:
		if msg.err != nil {
			m.connected = false
			m.errMsg = msg.err.Error()
			return m, nil
		}
		m.connected = true
		m.errMsg = ""
		m.status = msg.status
		m.profiles = msg.profiles
		m.logs = msg.logs
		// Clamp the cursor so deletes/renames don't leave it dangling.
		if m.cursor >= len(m.profiles) {
			m.cursor = max(0, len(m.profiles)-1)
		}
		return m, nil

	case actionDoneMsg:
		m.busy = ""
		if msg.err != nil {
			m.errMsg = fmt.Sprintf("%s failed: %v", msg.action, msg.err)
		} else {
			m.errMsg = ""
		}
		// Always refresh after an action so the UI catches up to reality.
		return m, m.refreshCmd()

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// handleKey is split out so tests can call it (or Update with a KeyMsg)
// directly without a full Bubbletea harness.
func (m Model) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	// While a Start/Stop is in flight, swallow action keys but still allow
	// quit and refresh. This keeps the operator from queuing conflicting
	// commands against the daemon.
	switch k.String() {
	case "q", "ctrl+c", "esc":
		m.quitting = true
		return m, tea.Quit
	case "r":
		return m, m.refreshCmd()
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
		return m, nil
	case "down", "j":
		if m.cursor < len(m.profiles)-1 {
			m.cursor++
		}
		return m, nil
	case "s", "enter":
		if m.busy != "" || len(m.profiles) == 0 {
			return m, nil
		}
		p := m.profiles[m.cursor].Name
		m.busy = "start"
		m.errMsg = ""
		return m, m.startCmd(p)
	case "x":
		if m.busy != "" {
			return m, nil
		}
		m.busy = "stop"
		m.errMsg = ""
		return m, m.stopCmd()
	}
	return m, nil
}

// --- view ---

// styles bundles the lipgloss styles. When NO_COLOR is set we hand back
// no-op styles so the output is plain ASCII.
type styles struct {
	title    lipgloss.Style
	header   lipgloss.Style
	label    lipgloss.Style
	selected lipgloss.Style
	dim      lipgloss.Style
	err      lipgloss.Style
	ready    lipgloss.Style
	starting lipgloss.Style
}

func newStyles(noColor bool) styles {
	if noColor {
		// Identity styles — no ANSI escapes get emitted.
		id := lipgloss.NewStyle()
		return styles{title: id, header: id, label: id, selected: id, dim: id, err: id, ready: id, starting: id}
	}
	return styles{
		title:    lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12")),
		header:   lipgloss.NewStyle().Bold(true),
		label:    lipgloss.NewStyle().Foreground(lipgloss.Color("8")),
		selected: lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Bold(true),
		dim:      lipgloss.NewStyle().Foreground(lipgloss.Color("8")),
		err:      lipgloss.NewStyle().Foreground(lipgloss.Color("9")),
		ready:    lipgloss.NewStyle().Foreground(lipgloss.Color("10")),
		starting: lipgloss.NewStyle().Foreground(lipgloss.Color("11")),
	}
}

// View renders the model as a string. Pure — depends only on m.
func (m Model) View() string {
	if m.quitting {
		return ""
	}
	s := newStyles(m.noColor)
	var b strings.Builder

	// Title bar.
	fmt.Fprintf(&b, "%s — connected to %s\n\n", s.title.Render("vibe"), m.apiURL)

	if !m.connected && m.status == nil {
		fmt.Fprintf(&b, "  %s\n", s.err.Render("vibe daemon not running."))
		fmt.Fprintf(&b, "  %s\n", "Run `vibe daemon &` or `vibe start <profile>`.")
		if m.errMsg != "" {
			fmt.Fprintf(&b, "  %s %s\n", s.label.Render("(error:"), s.dim.Render(m.errMsg+")"))
		}
		fmt.Fprintf(&b, "\n  %s\n", s.dim.Render("[r] refresh   [q] quit"))
		return b.String()
	}

	// Status block.
	b.WriteString(renderStatus(&s, m.status))
	b.WriteString("\n")

	// Profiles block.
	fmt.Fprintf(&b, "  %s\n", s.header.Render("Profiles"))
	if len(m.profiles) == 0 {
		fmt.Fprintf(&b, "    %s\n", s.dim.Render("(no profiles — drop YAMLs in ~/.config/vibe/profiles/)"))
	}
	for i, p := range m.profiles {
		marker := "  "
		nameStyle := s.label
		if i == m.cursor {
			marker = s.selected.Render("▸ ")
			nameStyle = s.selected
		}
		desc := p.Description
		if desc == "" {
			desc = s.dim.Render("(no description)")
		}
		fmt.Fprintf(&b, "    %s%s  %s\n", marker, nameStyle.Render(padRight(p.Name, 10)), desc)
	}
	b.WriteString("\n")

	// Logs block.
	fmt.Fprintf(&b, "  %s\n", s.header.Render(fmt.Sprintf("Logs (last %d)", maxLogLines)))
	logs := m.logs
	if len(logs) > maxLogLines {
		logs = logs[len(logs)-maxLogLines:]
	}
	if len(logs) == 0 {
		fmt.Fprintf(&b, "    %s\n", s.dim.Render("(no logs yet)"))
	}
	for _, ln := range logs {
		fmt.Fprintf(&b, "    %s\n", ln)
	}
	b.WriteString("\n")

	// Footer.
	if m.busy != "" {
		fmt.Fprintf(&b, "  %s %s...\n", s.starting.Render("working:"), m.busy)
	}
	if m.errMsg != "" {
		fmt.Fprintf(&b, "  %s\n", s.err.Render(m.errMsg))
	}
	fmt.Fprintf(&b, "  %s\n", s.dim.Render("[s/enter] start selected   [x] stop   [r] refresh   [q] quit"))

	return b.String()
}

func renderStatus(s *styles, st *vibev1.Status) string {
	var b strings.Builder
	if st == nil || !st.Running {
		fmt.Fprintf(&b, "  %s %s\n", s.label.Render("Profile:"), s.dim.Render("(none active)"))
		return b.String()
	}
	stateRendered := s.starting.Render("starting")
	if st.Ready {
		stateRendered = s.ready.Render("ready")
	}
	parts := []string{fmt.Sprintf("%s (%s)", st.Profile, stateRendered)}
	if st.StartedAt != nil {
		parts = append(parts, fmt.Sprintf("uptime %s", time.Since(st.StartedAt.AsTime()).Round(time.Second)))
	}
	if st.Pid != 0 {
		parts = append(parts, fmt.Sprintf("pid %d", st.Pid))
	}
	fmt.Fprintf(&b, "  %s %s\n", s.label.Render("Profile:"), strings.Join(parts, " · "))
	if st.BackendAddr != "" {
		fmt.Fprintf(&b, "  %s %s\n", s.label.Render("Backend:"), st.BackendAddr)
	}
	if st.ProxyAddr != "" {
		fmt.Fprintf(&b, "  %s   %s\n", s.label.Render("Proxy:"), st.ProxyAddr)
	}
	return b.String()
}

func padRight(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}
