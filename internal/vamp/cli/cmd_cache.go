package cli

import (
	"bufio"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vamp"
	"github.com/gallowaysoftware/vibe/internal/vamp/cache"
)

// cacheCmd is the parent of `vamp cache {ls,size,prune,clean}`. It mirrors
// `vamp runs` (the existing inspection-and-management surface) in shape so
// users hit muscle memory for both. Calling it bare prints help; we only act
// on explicit sub-verbs to avoid accidental deletions.
func cacheCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "Inspect and manage the content-addressed cache (under $XDG_CACHE_HOME/vamp/).",
	}
	cmd.AddCommand(
		cacheLsCmd(),
		cacheSizeCmd(),
		cachePruneCmd(),
		cacheCleanCmd(),
	)
	return cmd
}

func cacheLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List cache entries, newest first.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := cache.New(cache.DefaultRoot())
			if err != nil {
				return err
			}
			stats, err := store.Stat()
			if err != nil {
				return err
			}
			if len(stats) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "no cache entries under %s\n", cache.DefaultRoot())
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "HASH\tSTAGE\tSIZE\tAGE\tCREATED")
			now := time.Now()
			for _, st := range stats {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
					shortHash(st.Hash),
					st.Meta.StageType,
					vamp.FormatSize(st.Size),
					vamp.FormatDuration(now.Sub(st.Modified)),
					st.Meta.CreatedAt.Local().Format("2006-01-02 15:04:05"),
				)
			}
			return tw.Flush()
		},
	}
}

func cacheSizeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "size",
		Short: "Print total cache size and entry count.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := cache.New(cache.DefaultRoot())
			if err != nil {
				return err
			}
			total, count, err := store.Size()
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%d entries, %s (%s)\n", count, vamp.FormatSize(total), cache.DefaultRoot())
			return nil
		},
	}
}

func cachePruneCmd() *cobra.Command {
	var olderThan string
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Delete cache entries older than the supplied --older-than duration.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if olderThan == "" {
				return fmt.Errorf("--older-than is required (e.g. --older-than 30d)")
			}
			dur, err := parseAge(olderThan)
			if err != nil {
				return err
			}
			store, err := cache.New(cache.DefaultRoot())
			if err != nil {
				return err
			}
			removed, bytesGone, err := store.Prune(dur)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "pruned %d entries (%s reclaimed)\n", removed, vamp.FormatSize(bytesGone))
			return nil
		},
	}
	cmd.Flags().StringVar(&olderThan, "older-than", "", "Duration threshold; entries older than this are deleted (e.g. 30d, 12h, 90m).")
	return cmd
}

func cacheCleanCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "clean",
		Short: "Delete every cache entry (asks for confirmation unless --yes is set).",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := cache.New(cache.DefaultRoot())
			if err != nil {
				return err
			}
			if !yes {
				_, count, err := store.Size()
				if err != nil {
					return err
				}
				if count == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "cache is already empty")
					return nil
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Delete all %d cache entries under %s? [y/N] ", count, cache.DefaultRoot())
				reader := bufio.NewReader(cmd.InOrStdin())
				resp, _ := reader.ReadString('\n')
				resp = strings.TrimSpace(strings.ToLower(resp))
				if resp != "y" && resp != "yes" {
					fmt.Fprintln(cmd.OutOrStdout(), "aborted")
					return nil
				}
			}
			count, bytesGone, err := store.Clean()
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed %d entries (%s reclaimed)\n", count, vamp.FormatSize(bytesGone))
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip the interactive confirmation prompt.")
	return cmd
}

// shortHash truncates a full 64-char sha256 hex digest to its first 12
// characters for the `ls` table. 12 chars is the same length git uses for
// abbreviated commit hashes; collisions are astronomically unlikely at this
// scale.
func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// ageRE matches durations like "30d", "12h", "90m", "45s". Days are not
// handled by time.ParseDuration so we parse them by hand; hours/minutes/
// seconds delegate to the stdlib parser.
var ageRE = regexp.MustCompile(`^(\d+)([dhms])$`)

// parseAge parses age strings users would naturally type. We accept the
// stdlib forms (e.g. "30m", "12h") AND a "d" suffix (days) that
// time.ParseDuration rejects. Numbers are unsigned because the only sensible
// interpretation is "delete entries older than N units".
func parseAge(s string) (time.Duration, error) {
	if m := ageRE.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		switch m[2] {
		case "d":
			return time.Duration(n) * 24 * time.Hour, nil
		case "h":
			return time.Duration(n) * time.Hour, nil
		case "m":
			return time.Duration(n) * time.Minute, nil
		case "s":
			return time.Duration(n) * time.Second, nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("--older-than %q: must be a duration like 30d, 12h, 90m, 45s", s)
	}
	return d, nil
}
