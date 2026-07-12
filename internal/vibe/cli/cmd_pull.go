package cli

import (
	"context"
	"fmt"
	"io"
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
			if _, _, err := loadLocalProfile(args[0]); err != nil {
				return err
			}
			if err := ensureDaemon(ctx); err != nil {
				return err
			}
			return pullProfile(ctx, cmd.OutOrStdout(), args[0])
		},
	}
}

// pullProfile runs Pull and renders progress. Used by both `vibe pull` and
// `vibe start` (which calls it before Start).
func pullProfile(ctx context.Context, out io.Writer, profile string) error {
	stream, err := newClient().Pull(ctx, profile)
	if err != nil {
		return err
	}
	defer stream.Close()
	return renderPull(out, stream)
}

func renderPull(out io.Writer, stream *vibeclient.PullStream) error {
	var bar *progressBar
	for stream.Receive() {
		m := stream.Msg()
		switch m.Phase {
		case vibev1.PullProgress_PHASE_RESOLVING:
			fmt.Fprintln(out, "resolving huggingface metadata...")
		case vibev1.PullProgress_PHASE_DOWNLOADING:
			if bar == nil {
				if m.TotalBytes > 0 {
					fmt.Fprintf(out, "downloading %s\n", humanBytes(m.TotalBytes))
				} else {
					fmt.Fprintln(out, "downloading (size unknown)")
				}
				bar = newProgressBar(out, m.TotalBytes)
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
				fmt.Fprintf(out, "already cached (%s)\n", humanBytes(m.TotalBytes))
			default:
				fmt.Fprintln(out, m.Message)
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
	out   io.Writer
	total int64
	start time.Time
}

func newProgressBar(out io.Writer, total int64) *progressBar {
	return &progressBar{out: out, total: total, start: time.Now()}
}

func (b *progressBar) update(downloaded int64) {
	elapsed := time.Since(b.start).Seconds()
	if elapsed < 0.01 {
		elapsed = 0.01
	}
	rate := float64(downloaded) / elapsed / (1024 * 1024) // MB/s
	if b.total <= 0 {
		// Unknown-size mode (HEAD returned no Content-Length). Skip the
		// bar/percentage and just print bytes + rate so the user can see
		// the download is making progress.
		fmt.Fprintf(b.out, "\r  %s (%.1f MB/s)", humanBytes(downloaded), rate)
		return
	}
	pct := float64(downloaded) / float64(b.total) * 100
	const w = 30
	filled := int(pct * w / 100)
	if filled > w {
		filled = w
	}
	bar := strings.Repeat("=", filled) + strings.Repeat(" ", w-filled)
	fmt.Fprintf(b.out, "\r  [%s] %s / %s (%.1f%%, %.1f MB/s)",
		bar, humanBytes(downloaded), humanBytes(b.total), pct, rate)
}

func (b *progressBar) finish() {
	fmt.Fprintln(b.out)
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
