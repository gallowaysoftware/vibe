package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/buildinfo"
	"github.com/gallowaysoftware/vibe/internal/vibe/daemon"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetmirror"
	"github.com/gallowaysoftware/vibe/internal/vibe/paths"
)

// `vibe fleet mirror` (fleet-control C19): the front host's state, in
// one archive, somewhere else.
//
// It runs on the front HOST from a timer, not inside fleetd, for a
// reason worth stating once: the mirror has to keep working when fleetd
// is the thing that broke, and fleetd's own state dir is a bind mount
// whose host path fleetd cannot see. A control plane that backed itself
// up would also be a control plane that writes on a schedule nobody
// asked for.
//
// Nothing here fails over anything. `restore` places files on a standby
// and refuses to run while the fleet's own addresses still answer; who
// is the front is a decision a human makes with DNS.

func fleetMirrorCmd() *cobra.Command {
	var (
		out         string
		stateDir    string
		configDir   string
		frontConfig string
		frontExtras string
		include     []string
		keep        int
		noSecrets   bool
		asJSON      bool
	)
	cmd := &cobra.Command{
		Use:   "mirror",
		Short: "Archive the front host's fleet state somewhere it survives (C19).",
		Long: "The front host dying is the fleet's one total outage. This captures everything a " +
			"spare box needs to assume the front's identity: fleetd's state dir (intent, leases, " +
			"last-seen, start history, the append-only usage ledger, the control-plane token), the " +
			"config dir (hosts.yaml, config.yaml, the backend defs the render reads), and the front's " +
			"RENDERED llama-swap config.\n\n" +
			"What it does NOT cover is in every manifest it writes, because discovering that during a " +
			"recovery is too late: model weights, the llama-swap image (run the digest fleet.front_image " +
			"pins), each cell's own state, the credential VALUES hosts.yaml references, and DNS.\n\n" +
			"The archive carries the control-plane token unless --no-secrets, so it is as sensitive as " +
			"the front host. It is written 0600.\n\n" +
			"Runbook: docs/runbooks/front-failover.md.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if stateDir == "" {
				stateDir = paths.StateHome()
			}
			if configDir == "" {
				configDir = paths.ConfigHome()
			}
			cfg, err := daemon.LoadConfigFrom(filepath.Join(configDir, filepath.Base(paths.ConfigFile())))
			if err != nil {
				return fmt.Errorf("read %s: %w", filepath.Join(configDir, filepath.Base(paths.ConfigFile())), err)
			}
			hosts, hostsErr := fleetcfg.LoadFrom(filepath.Join(configDir, filepath.Base(paths.HostsFile())))
			if hostsErr != nil && !errors.Is(hostsErr, os.ErrNotExist) {
				return fmt.Errorf("read hosts.yaml: %w", hostsErr)
			}
			// fleet.front_config is a path inside fleetd's CONTAINER. Using
			// it as a host path is right for a bare-metal fleetd and wrong
			// for the reference deployment, so it is only a fallback and the
			// manifest says when it could not be read.
			if frontConfig == "" {
				frontConfig = cfg.Fleet.FrontConfig
			}
			if frontExtras == "" {
				frontExtras = cfg.Fleet.FrontExtras
			}
			host, _ := os.Hostname()
			m, rc, err := fleetmirror.Create(fleetmirror.Options{
				StateDir: stateDir, ConfigDir: configDir,
				FrontConfig: frontConfig, FrontExtras: frontExtras,
				Include: include, Out: out, Keep: keep, NoSecrets: noSecrets,
				Hosts:      hosts,
				FrontImage: cfg.Fleet.FrontImage, Timezone: cfg.Fleet.Timezone,
				GuestToken: cfg.Fleet.GuestTokenFile,
				NotifyURL:  cfg.Fleet.Notify.URLFile, NotifyTok: cfg.Fleet.Notify.TokenFile,
				Vibe: buildinfo.String(), Host: host,
			})
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), rc)
			}
			printMirror(cmd.OutOrStdout(), m, rc)
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "destination directory (REQUIRED; must not be on this host's own disk)")
	cmd.Flags().StringVar(&stateDir, "state-dir", "", "fleetd's state dir on THIS host (default $XDG_STATE_HOME/vibe)")
	cmd.Flags().StringVar(&configDir, "config-dir", "", "fleetd's config dir on THIS host (default $XDG_CONFIG_HOME/vibe)")
	cmd.Flags().StringVar(&frontConfig, "front-config", "", "the front's rendered llama-swap config, as a HOST path")
	cmd.Flags().StringVar(&frontExtras, "front-extras", "", "fleet.front_extras, as a HOST path")
	cmd.Flags().StringArrayVar(&include, "include", nil, "extra file or directory to archive (compose stack, .env, unit files); repeatable")
	cmd.Flags().IntVar(&keep, "keep", 7, "keep this many archives in --out (0 keeps all)")
	cmd.Flags().BoolVar(&noSecrets, "no-secrets", false, "omit the tokens and the front's rendered config; the recovery then includes a re-key and a re-render")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the receipt as JSON")
	cmd.AddCommand(fleetMirrorVerifyCmd(), fleetMirrorRestoreCmd())
	return cmd
}

func fleetMirrorVerifyCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "verify <archive|dir>",
		Short: "Read an archive back and check every claim its manifest makes.",
		Long: "Recomputes each entry's sha256 and reports anything in the manifest that is missing " +
			"from the archive or vice versa, then prints the identity a standby would assume, the " +
			"credentials it does NOT carry, and what it does not cover.\n\n" +
			"A mirror nobody has ever read back is a belief, not a backup.",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			archive, err := resolveArchive(args[0])
			if err != nil {
				return err
			}
			m, verr := fleetmirror.Verify(archive)
			if asJSON {
				if m != nil {
					if err := writeJSON(cmd.OutOrStdout(), m); err != nil {
						return err
					}
				}
				return verr
			}
			if m != nil {
				printManifest(cmd.OutOrStdout(), archive, m)
			}
			if verr != nil {
				return verr
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\nOK: %d entries, every sha256 matches.\n", len(m.Entries))
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the manifest as JSON")
	return cmd
}

func fleetMirrorRestoreCmd() *cobra.Command {
	var (
		stateDir    string
		configDir   string
		frontConfig string
		frontExtras string
		overwrite   bool
		dryRun      bool
		force       bool
		probeAddrs  []string
	)
	cmd := &cobra.Command{
		Use:   "restore <archive|dir>",
		Short: "Place a mirror's state and config on a standby box.",
		Long: "Verifies the archive, refuses if anything still answers on the fleet's own addresses, " +
			"then writes each file to the destination you name, with the mode it had.\n\n" +
			"Destinations are explicit and an unset one is a SKIP, never a guess. Files under " +
			"extras/ are never placed automatically: they came from absolute paths on the old host.\n\n" +
			"This does not fail anything over. Moving the fleet's address to this box is a DNS or " +
			"IP change you make afterwards, deliberately — see docs/runbooks/front-failover.md.",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			archive, err := resolveArchive(args[0])
			if err != nil {
				return err
			}
			rep, err := fleetmirror.Restore(fleetmirror.RestoreOptions{
				Archive: archive, StateDir: stateDir, ConfigDir: configDir,
				FrontConfig: frontConfig, FrontExtras: frontExtras,
				Overwrite: overwrite, DryRun: dryRun, Force: force,
				ProbeAddrs: probeAddrs,
			})
			if rep != nil {
				printRestore(cmd.OutOrStdout(), rep)
			}
			return err
		},
	}
	cmd.Flags().StringVar(&stateDir, "state-dir", "", "where the state slot goes (unset = skipped)")
	cmd.Flags().StringVar(&configDir, "config-dir", "", "where the config slot goes (unset = skipped)")
	cmd.Flags().StringVar(&frontConfig, "front-config", "", "where the front's rendered config goes (unset = skipped)")
	cmd.Flags().StringVar(&frontExtras, "front-extras", "", "where fleet.front_extras goes (unset = skipped)")
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "replace files that already exist on this box")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the plan and write nothing")
	cmd.Flags().BoolVar(&force, "force", false, "skip the takeover probe (only when you have confirmed the old front is down)")
	cmd.Flags().StringArrayVar(&probeAddrs, "probe-addr", nil,
		"host:port the old front answers on, when the mirror recorded no address to probe; repeatable")
	return cmd
}

// resolveArchive accepts an archive or the directory holding them, so
// the recovery command is the same whether or not the operator
// remembers the timestamp.
func resolveArchive(arg string) (string, error) {
	fi, err := os.Stat(arg)
	if err != nil {
		return "", err
	}
	if fi.IsDir() {
		return fleetmirror.Newest(arg)
	}
	return arg, nil
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func printMirror(w io.Writer, m *fleetmirror.Manifest, rc *fleetmirror.Receipt) {
	fmt.Fprintf(w, "wrote %s (%d files, %d bytes)\n", rc.Archive, rc.Files, rc.Bytes)
	if m.Credentials {
		fmt.Fprintln(w, "  contains the control-plane token: this archive is as sensitive as the front host.")
	}
	printList(w, "missing (not present on this host)", m.Missing)
	printList(w, "ERRORS (these files are NOT backed up)", m.Errors)
	printList(w, "GAPS (this archive cannot restore these)", m.Gaps)
	printList(w, "warnings", m.Warnings)
	if len(m.References) > 0 {
		fmt.Fprintln(w, "  credentials referenced and deliberately NOT carried:")
		for _, r := range m.References {
			state := "missing"
			if r.Exists {
				state = fmt.Sprintf("%o", r.Mode.Perm())
			}
			fmt.Fprintf(w, "    %-12s %-10s %s (%s)\n", r.Kind, r.Cell, r.Path, state)
		}
	}
}

func printManifest(w io.Writer, archive string, m *fleetmirror.Manifest) {
	fmt.Fprintf(w, "%s\n  created %s on %s by vibe %s\n", archive,
		m.CreatedAt.Format("2006-01-02 15:04:05Z"), m.Host, m.Vibe)
	fmt.Fprintln(w, "\nidentity a standby would assume:")
	fmt.Fprintf(w, "  fleetd     %s\n", or(m.Identity.FleetdURL, "(not recorded)"))
	fmt.Fprintf(w, "  front      %s\n", or(m.Identity.FrontURL, "(not recorded)"))
	fmt.Fprintf(w, "  image      %s\n", or(m.Identity.FrontImage, "(undeclared: fleet.front_image)"))
	fmt.Fprintf(w, "  timezone   %s\n", or(m.Identity.Timezone, "(unset)"))
	for _, c := range m.Identity.Cells {
		fmt.Fprintf(w, "  cell %-10s %-28s %s\n", c.Name, c.URL, c.Class)
	}
	fmt.Fprintf(w, "\ncontents (%d):\n", len(m.Entries))
	for _, e := range m.Entries {
		secret := ""
		if e.Secret {
			secret = "  [secret]"
		}
		fmt.Fprintf(w, "  %-44s %6d B  %04o%s\n", e.Archive, e.Size, e.Mode.Perm(), secret)
	}
	printList(w, "missing", m.Missing)
	printList(w, "ERRORS", m.Errors)
	printList(w, "GAPS (this archive cannot restore these)", m.Gaps)
	printList(w, "warnings", m.Warnings)
	if len(m.References) > 0 {
		fmt.Fprintln(w, "\nfetch these from the private fleet repo (paths recorded, values never carried):")
		for _, r := range m.References {
			fmt.Fprintf(w, "  %-12s %-10s %s\n      %s\n", r.Kind, r.Cell, r.Path, r.Why)
		}
	}
	fmt.Fprintln(w, "\nNOT covered by this archive:")
	for _, n := range m.NotCovered {
		fmt.Fprintf(w, "  - %s\n", n)
	}
}

func printRestore(w io.Writer, rep *fleetmirror.RestoreReport) {
	for _, h := range rep.Takeover {
		fmt.Fprintf(w, "STILL ANSWERING: %s at %s\n", h.What, h.Addr)
	}
	if len(rep.Takeover) == 0 && len(rep.Probed) > 0 {
		fmt.Fprintf(w, "takeover probe: %s answered nothing\n", strings.Join(rep.Probed, ", "))
	}
	for _, a := range rep.Actions {
		dest := a.Dest
		if dest == "" {
			dest = "-"
		}
		note := ""
		if a.Note != "" {
			note = "  (" + a.Note + ")"
		}
		fmt.Fprintf(w, "  %-14s %-40s -> %s%s\n", a.Status, a.Archive, dest, note)
	}
	if rep.DryRun {
		fmt.Fprintln(w, "dry run: nothing written.")
		return
	}
	if rep.Wrote == 0 {
		return
	}
	for _, p := range rep.Preserved {
		fmt.Fprintf(w, "PRESERVED: the append-only file that was here is at %s (it was not a prefix of the archive's copy)\n", p)
	}
	fmt.Fprintf(w, "\nrestored %d files. Still yours to do:\n", rep.Wrote)
	fmt.Fprintln(w, "  1. fetch the credential files the manifest lists as references (private fleet repo)")
	if img := rep.Manifest.Identity.FrontImage; img != "" {
		fmt.Fprintf(w, "  2. run the front on the PINNED image: %s\n", img)
	} else {
		fmt.Fprintln(w, "  2. run the front on the image fleet.front_image pins (undeclared in this mirror)")
	}
	fmt.Fprintln(w, "  3. start fleetd against the restored state dir, then confirm cells announce")
	fmt.Fprintln(w, "  4. LAST: move the fleet's address to this box (DNS record, or take the IP)")
	fmt.Fprintln(w, "  runbook: docs/runbooks/front-failover.md")
}

func printList(w io.Writer, label string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(w, "  %s:\n", label)
	for _, s := range items {
		fmt.Fprintf(w, "    - %s\n", strings.TrimSpace(s))
	}
}

func or(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
