package fleetapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Wake-on-LAN (fleet-control C2): fleetd sends the magic packet from its
// LAN position so waking works from anywhere the control plane reaches —
// and when fleetd's network position can't reach L2 broadcast (macvlan
// hides broadcast domains from containers), a per-cell fallback command
// runs instead. Waking is ALWAYS explicit: never triggered by a request,
// a heuristic, or a schedule.

// WakeSpec is a cell's wake configuration, mirrored from hosts.yaml.
type WakeSpec struct {
	// MAC is the target NIC's hardware address.
	MAC string
	// Broadcast overrides the packet destination (default
	// 255.255.255.255:9) — e.g. a directed subnet broadcast.
	Broadcast string
	// Cmd runs INSTEAD of the packet when set (the per-cell fallback for
	// fleetd positions that can't reach L2). Run via sh -c.
	Cmd string
}

// defaultWakeBroadcast is the canonical magic-packet destination.
const defaultWakeBroadcast = "255.255.255.255:9"

// wakeCmdTimeout bounds the fallback command. A var, not a const, for
// the reason warmLoopConfig.tick is a field: SendWake takes no config
// struct (it is the shared delivery path for the HTTP endpoint, the MCP
// facade and the sleep schedule's wake half), so this is the only seam a
// test can dial down — and a 30-second deadline no test can shorten is a
// deadline no test ever reaches. Production never assigns it; the wake
// verbs read it and nothing else does.
var wakeCmdTimeout = 30 * time.Second

// wakeCmdKillGrace bounds the SECOND half of the fallback command's
// death, and it exists because the first half is not enough on its own.
//
// exec.CommandContext kills the process it STARTED. `sh -c` is not that
// process on the fleet's own boxes: /bin/sh is dash on Debian and
// Ubuntu, dash forks the command rather than exec'ing into it, and the
// grandchild inherits the stdout/stderr pipes CombinedOutput reads.
// Killing the shell therefore leaves the pipes open, and Wait — which
// waits for the COPY to finish, not for the process — blocks until the
// operator's ipmitool/ssh returns on its own. That is the unbounded wait
// the deadline above was written to prevent, and it survived the
// deadline: a wedged wake pinned the goroutine for as long as the far
// side stayed quiet, on CI and on every fleet box.
//
// So: kill the process GROUP (below), which reaches the descendants dash
// forked, and keep WaitDelay as the backstop for the one that got away —
// a command that setsid'd out of the group, or wedged in D-state where
// SIGKILL cannot land. WaitDelay is what closes the parent's pipe ends
// regardless, so Wait returns whatever the far side is doing.
//
// Both halves are needed and neither substitutes for the other, which is
// why each is pinned separately: the group kill by the two wake deadline
// tests (they assert the call returns BEFORE this grace, so a run that
// needed the backstop is red), WaitDelay by the wiring test that reads it
// back off the built command.
const wakeCmdKillGrace = 5 * time.Second

// wakeCommand builds the fallback command with both halves of its death
// wired: a process group of its own so the kill reaches whatever the
// shell forked, and a WaitDelay so Wait cannot outlive the kill by more
// than the grace. Split out of RunWakeCmd so a test can read the wiring
// back off the built command without starting anything.
func wakeCommand(ctx context.Context, script string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	// Setpgid makes the shell a group leader, so the negative-pid kill
	// below reaches everything it forked. A non-interactive shell runs no
	// job control, so even backgrounded work stays in this group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				// The whole group is already gone: not a failure to cancel.
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	cmd.WaitDelay = wakeCmdKillGrace
	return cmd
}

// RunWakeCmd runs a cell's wake.cmd from hosts.yaml under wakeCmdTimeout
// AND the caller's ctx, and returns its combined output.
//
// Exported for the CLI's degraded wake path (fleetd down — the operator's
// own box runs the command), the way SendMagicPacket is. That path ran
// the same operator command through a bare exec.CommandContext, so it
// carried this defect too: no deadline of its own, and a Ctrl-C that
// killed the shell and then waited on the ssh the shell had forked.
func RunWakeCmd(ctx context.Context, script string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, wakeCmdTimeout)
	defer cancel()
	return wakeCommand(ctx, script).CombinedOutput()
}

// wakeRequest is the POST /api/fleet/wake body.
type wakeRequest struct {
	Cell string `json:"cell"`
}

// wakeResponse reports how the wake was delivered (packet vs command —
// the distinction matters when debugging a cell that didn't come up).
type wakeResponse struct {
	Cell   string `json:"cell"`
	Sent   string `json:"sent"` // "packet" | "cmd"
	Target string `json:"target"`
}

// handleWake serves POST /api/fleet/wake. Unknown cells are 400; a cell
// with no wake config is 404 (the config gap is the answer, not an
// error to retry).
func (s *Server) handleWake(w http.ResponseWriter, r *http.Request) {
	var req wakeRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	resp, err := s.SendWake(r.Context(), req.Cell)
	if err != nil {
		var nc *noWakeConfigError
		var uk *unknownCellError
		switch {
		case errors.As(err, &uk):
			http.Error(w, err.Error(), http.StatusBadRequest)
		case errors.As(err, &nc):
			http.Error(w, err.Error(), http.StatusNotFound)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

type unknownCellError struct{ cell string }

func (e *unknownCellError) Error() string {
	return fmt.Sprintf("unknown cell %q (not in the registry)", e.cell)
}

type noWakeConfigError struct{ cell string }

func (e *noWakeConfigError) Error() string {
	return fmt.Sprintf("cell %q has no wake: config in hosts.yaml", e.cell)
}

// SendWake delivers a wake to the named cell: the fallback command when
// configured, else a magic packet. Exported so the MCP facade shares the
// exact delivery path with the HTTP endpoint.
func (s *Server) SendWake(ctx context.Context, cellName string) (*wakeResponse, error) {
	var wake *WakeSpec
	found := false
	for _, c := range s.cells {
		if c.Name == cellName {
			wake, found = c.Wake, true
			break
		}
	}
	if !found {
		return nil, &unknownCellError{cell: cellName}
	}
	if wake == nil {
		return nil, &noWakeConfigError{cell: cellName}
	}

	if wake.Cmd != "" {
		out, err := RunWakeCmd(ctx, wake.Cmd)
		if err != nil {
			return nil, fmt.Errorf("wake command failed: %v: %s", err, string(out))
		}
		return &wakeResponse{Cell: cellName, Sent: "cmd", Target: wake.Cmd}, nil
	}

	mac, err := net.ParseMAC(wake.MAC)
	if err != nil {
		return nil, fmt.Errorf("cell %q wake.mac %q is invalid: %v", cellName, wake.MAC, err)
	}
	if len(mac) != 6 {
		return nil, fmt.Errorf("cell %q wake.mac %q must be a 48-bit MAC (got %d bytes)", cellName, wake.MAC, len(mac))
	}
	target := wake.Broadcast
	if target == "" {
		target = defaultWakeBroadcast
	}
	if err := s.sendMagicPacket(target, mac); err != nil {
		return nil, fmt.Errorf("send magic packet to %s: %v", target, err)
	}
	return &wakeResponse{Cell: cellName, Sent: "packet", Target: target}, nil
}

// sendMagicPacket broadcasts the WoL frame. The sender is a field so
// tests can capture the packet without touching real broadcast.
func (s *Server) sendMagicPacket(target string, mac net.HardwareAddr) error {
	return SendMagicPacket(target, mac)
}

// SendMagicPacket broadcasts a WoL frame for mac to target (host:port).
// Exported for the CLI's degraded wake path (fleetd down — any LAN box
// can send the packet).
func SendMagicPacket(target string, mac net.HardwareAddr) error {
	packet := magicPacket(mac)
	conn, err := net.Dial("udp", target)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write(packet)
	return err
}

// magicPacket builds the 102-byte WoL frame: 6 sync bytes then the MAC
// 16 times.
func magicPacket(mac net.HardwareAddr) []byte {
	packet := make([]byte, 6+16*len(mac))
	for i := range 6 {
		packet[i] = 0xFF
	}
	for i := range 16 {
		copy(packet[6+i*len(mac):], mac)
	}
	return packet
}
