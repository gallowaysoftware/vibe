package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gallowaysoftware/vibe/internal/vibeclient"
	vibev1 "github.com/gallowaysoftware/vibe/proto/vibe/v1"
)

func pullCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "pull <profile>",
		Short:             "Download the profile's model from HuggingFace if not already cached.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeProfileNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if err := ensureDaemon(ctx); err != nil {
				return err
			}
			return pullProfile(ctx, args[0])
		},
	}
}

// pullProfile runs Pull and renders progress. Used by both `vibe pull` and
// `vibe start` (which calls it before Start).
func pullProfile(ctx context.Context, profile string) error {
	stream, err := newClient().Pull(ctx, profile)
	if err != nil {
		return err
	}
	defer stream.Close()
	return renderPull(stream)
}

func renderPull(stream *vibeclient.PullStream) error {
	var bar *progressBar
	for stream.Receive() {
		m := stream.Msg()
		switch m.Phase {
		case vibev1.PullProgress_PHASE_RESOLVING:
			fmt.Println("resolving huggingface metadata...")
		case vibev1.PullProgress_PHASE_DOWNLOADING:
			if bar == nil {
				fmt.Printf("downloading %s\n", humanBytes(m.TotalBytes))
				bar = newProgressBar(m.TotalBytes)
			}
			bar.update(m.DownloadedBytes)
		case vibev1.PullProgress_PHASE_DONE:
			if bar != nil {
				bar.update(m.TotalBytes)
				bar.finish()
			}
			switch m.Message {
			case "", "no huggingface block; nothing to pull":
				// silent
			case "already cached":
				fmt.Printf("already cached (%s)\n", humanBytes(m.TotalBytes))
			default:
				fmt.Println(m.Message)
			}
		}
	}
	if err := stream.Err(); err != nil {
		if bar != nil {
			bar.finish()
		}
		return err
	}
	return nil
}

type progressBar struct {
	total int64
	start time.Time
}

func newProgressBar(total int64) *progressBar {
	return &progressBar{total: total, start: time.Now()}
}

func (b *progressBar) update(downloaded int64) {
	var pct float64
	if b.total > 0 {
		pct = float64(downloaded) / float64(b.total) * 100
	}
	elapsed := time.Since(b.start).Seconds()
	if elapsed < 0.01 {
		elapsed = 0.01
	}
	rate := float64(downloaded) / elapsed / (1024 * 1024) // MB/s
	const w = 30
	filled := int(pct * w / 100)
	if filled > w {
		filled = w
	}
	bar := strings.Repeat("=", filled) + strings.Repeat(" ", w-filled)
	fmt.Printf("\r  [%s] %s / %s (%.1f%%, %.1f MB/s)",
		bar, humanBytes(downloaded), humanBytes(b.total), pct, rate)
}

func (b *progressBar) finish() {
	fmt.Println()
}

func humanBytes(n int64) string {
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
	)
	switch {
	case n >= gb:
		return fmt.Sprintf("%.1f GB", float64(n)/gb)
	case n >= mb:
		return fmt.Sprintf("%.1f MB", float64(n)/mb)
	case n >= kb:
		return fmt.Sprintf("%.1f KB", float64(n)/kb)
	default:
		return fmt.Sprintf("%d B", n)
	}
}
