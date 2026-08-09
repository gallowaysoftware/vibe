package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	vibev1 "github.com/gallowaysoftware/vibe/proto/vibe/v1"
)

// psReport is `vibe ps --json`'s document: the same facts the table
// prints, in the one shape a script can read.
//
// Deliberately not the wire type. The daemon answers in protobuf, whose
// JSON rendering spells a timestamp {"seconds":…,"nanos":…} and drops
// every zero-valued field, so printing it would publish the RPC's
// encoding as this command's contract.
//
// daemon_running is a field rather than an absent document because
// "nothing is running" and "nobody was asked" are different facts — the
// table says so in its one line, and a consumer seeing only `active:
// null` would read a stopped daemon as an idle box.
type psReport struct {
	DaemonRunning bool      `json:"daemon_running"`
	Active        *psEntry  `json:"active"`
	Services      []psEntry `json:"services"`
}

// psEntry is one running thing: the active profile, or a service.
type psEntry struct {
	Profile     string     `json:"profile"`
	Running     bool       `json:"running"`
	Ready       bool       `json:"ready"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	BackendAddr string     `json:"backend_addr,omitempty"`
	ProxyAddr   string     `json:"proxy_addr,omitempty"`
	Pid         int32      `json:"pid,omitempty"`
}

func newPSEntry(s *vibev1.Status) psEntry {
	e := psEntry{
		Profile:     s.Profile,
		Running:     s.Running,
		Ready:       s.Ready,
		BackendAddr: s.BackendAddr,
		ProxyAddr:   s.ProxyAddr,
		Pid:         s.Pid,
	}
	if s.StartedAt != nil {
		t := s.StartedAt.AsTime()
		e.StartedAt = &t
	}
	return e
}

func psCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "ps",
		Short: "Show the active profile and every running service.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			// `ps` is a read-only status query, so it must not spawn a
			// daemon as a side effect — that would make asking "is anything
			// running?" itself start a background process. If the daemon
			// isn't up, nothing is running, full stop.
			if err := pingDaemon(500 * time.Millisecond); err != nil {
				if asJSON {
					return writeJSON(cmd.OutOrStdout(), psReport{Services: []psEntry{}})
				}
				fmt.Fprintln(cmd.OutOrStdout(), "no active profile (daemon not running)")
				return nil
			}
			active, services, err := newClient().StatusFull(ctx)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()

			if asJSON {
				rep := psReport{DaemonRunning: true, Services: []psEntry{}}
				if active != nil && active.Running {
					e := newPSEntry(active)
					rep.Active = &e
				}
				for _, s := range services {
					if s == nil {
						continue
					}
					rep.Services = append(rep.Services, newPSEntry(s))
				}
				return writeJSON(out, rep)
			}

			// Active profile section.
			if active == nil || !active.Running {
				fmt.Fprintln(out, "no active profile")
			} else {
				ready := "starting"
				if active.Ready {
					ready = "ready"
				}
				fmt.Fprintf(out, "active:   %s (%s)\n", active.Profile, ready)
				if active.StartedAt != nil {
					fmt.Fprintf(out, "  uptime: %s\n", time.Since(active.StartedAt.AsTime()).Round(time.Second))
				}
				if active.BackendAddr != "" {
					fmt.Fprintf(out, "  backend: %s\n", active.BackendAddr)
				}
				if active.ProxyAddr != "" {
					fmt.Fprintf(out, "  proxy:   %s\n", active.ProxyAddr)
				}
				if active.Pid != 0 {
					fmt.Fprintf(out, "  pid:     %d\n", active.Pid)
				}
			}

			// Services section. Empty in the legacy single-profile
			// case; one line per running sidecar otherwise. Backend
			// addr is the published port the caller actually hits
			// (services bypass the proxy).
			if len(services) > 0 {
				fmt.Fprintln(out)
				fmt.Fprintf(out, "services: %d running\n", len(services))
				for _, s := range services {
					ready := "starting"
					if s.Ready {
						ready = "ready"
					}
					uptime := ""
					if s.StartedAt != nil {
						uptime = " " + time.Since(s.StartedAt.AsTime()).Round(time.Second).String()
					}
					fmt.Fprintf(out, "  - %-24s %s%s  %s (pid %d)\n",
						s.Profile, ready, uptime, s.BackendAddr, s.Pid)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the status as JSON")
	return cmd
}
