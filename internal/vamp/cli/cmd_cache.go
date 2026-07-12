package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
		cacheInfoCmd(),
	)
	return cmd
}

func cacheInfoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "info <id-or-prefix>",
		Short: "Show per-stage cache hit/miss for a specific pipeline run.",
		Long: `info reads pipeline_timing.json from a finished run and prints,
per stage: cache hit-or-miss, duration, and stage status. Useful for
debugging "why did stage X re-run unexpectedly?" — the hit/miss field
tells you whether the cache key matched.

The run is named by id-or-prefix (the id ` + "`vamp run --detach`" + ` printed,
resolved against $XDG_STATE_HOME/vamp/runs/) or a directory path — the
same addressing as ` + "`vamp runs show`" + ` / ` + "`vamp logs`" + `.

A "miss" means the executor recomputed the stage's output AND
wrote it to the cache; the next run with the same key will hit.
A "hit" means the cache key matched a prior run and the cached
bytes were used directly.

Foreach stages report the parent's cache state; per-item caching
is not surfaced here (see vamp cache ls for raw per-key entries).
`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeRunIDs,
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := vamp.FindRunByPrefix(vamp.RunsDir(), args[0])
			if err != nil {
				return renderLookupErr(cmd, err)
			}
			return runCacheInfo(cmd, r.Path)
		},
	}
}

// runCacheInfo opens <run-dir>/pipeline_timing.json and renders a
// human-readable per-stage cache report.
func runCacheInfo(cmd *cobra.Command, runDir string) error {
	path := filepath.Join(runDir, "pipeline_timing.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	var rep struct {
		Pipeline string `json:"pipeline"`
		Stages   []struct {
			ID         string         `json:"id"`
			Type       string         `json:"type,omitempty"`
			Status     string         `json:"status"`
			DurationMS int64          `json:"duration_ms"`
			Notes      map[string]any `json:"notes,omitempty"`
		} `json:"stages"`
	}
	if err := json.Unmarshal(raw, &rep); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "pipeline:  %s\n", rep.Pipeline)
	fmt.Fprintf(out, "run dir:   %s\n", runDir)
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  %-32s  %-8s  %-9s  %-7s  status\n", "stage", "type", "cache", "duration")
	fmt.Fprintf(out, "  %s\n", strings.Repeat("-", 78))

	var hits, misses int
	for _, s := range rep.Stages {
		cacheState := "—"
		if v, ok := s.Notes["cache"]; ok {
			cacheState = fmt.Sprint(v)
			switch cacheState {
			case "hit":
				hits++
			case "miss":
				misses++
			}
		}
		dur := time.Duration(s.DurationMS) * time.Millisecond
		fmt.Fprintf(out, "  %-32s  %-8s  %-9s  %-7s  %s\n",
			truncate(s.ID, 32), s.Type, cacheState, dur.Round(time.Millisecond), s.Status)
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "summary: %d hits, %d misses, %d total stages\n",
		hits, misses, len(rep.Stages))
	return nil
}

// truncate keeps the table neat when stage ids are unusually long.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
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
			dur, err := parseDurationSpec(olderThan)
			if err != nil {
				return fmt.Errorf("--older-than %q: %w", olderThan, err)
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
	cmd.Flags().StringVar(&olderThan, "older-than", "", "Duration threshold; entries older than this are deleted (e.g. 30d, 2w, 12h, 90m).")
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
