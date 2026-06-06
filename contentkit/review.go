package contentkit

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// ReviewItem is one staged, machine-generated candidate awaiting a human verdict.
type ReviewItem struct {
	ID       string // short identifier (slug)
	Title    string // human title
	Body     string // the full content to show
	Stamp    string // when it was staged
	IsUpdate bool   // true if accepting overwrites existing content (a backup is implied)
}

// ReviewActions are the app-specific side-effects of a verdict. Any may be nil
// (a nil Edit disables the [e] option).
type ReviewActions struct {
	Accept  func(ReviewItem) error // merge into the live store (with backup if IsUpdate)
	Discard func(ReviewItem) error // drop the staged candidate
	Edit    func(ReviewItem) error // open in $EDITOR (or similar) before accepting
}

// ReviewResult tallies a review session.
type ReviewResult struct{ Accepted, Discarded, Skipped int }

// ReviewLoop runs the non-destructive a/e/r/s/q review over staged items: show
// each, take a verdict, apply the matching action. This is worldsmith's
// `expand review` loop, generalised so any "stage → human accept/discard"
// workflow (notebook dossiers, timeline events, idea candidates) reuses it.
// Non-interactive batch curation (accept-all etc.) is the caller's concern —
// it just doesn't call this.
func ReviewLoop(in io.Reader, out io.Writer, items []ReviewItem, act ReviewActions) (ReviewResult, error) {
	var res ReviewResult
	r := bufio.NewReader(in)
	bar := strings.Repeat("─", 72)
	opts := "[a]ccept  [r]eject  [s]kip  [q]uit"
	if act.Edit != nil {
		opts = "[a]ccept  [e]dit then accept  [r]eject  [s]kip  [q]uit"
	}

	for _, it := range items {
		fmt.Fprintf(out, "\n%s\n%s   [%s]   (staged %s)\n", bar, it.Title, it.ID, it.Stamp)
		if it.IsUpdate {
			fmt.Fprintf(out, "  ⚠ updates existing content — the current version is backed up on accept\n")
		}
		fmt.Fprintf(out, "%s\n\n%s\n", bar, strings.TrimSpace(it.Body))
	prompt:
		fmt.Fprintf(out, "\n%s > ", opts)
		line, err := r.ReadString('\n')
		if err != nil && line == "" {
			fmt.Fprintln(out, "\n(end of input) — remaining items left staged")
			break
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "a", "accept":
			if act.Accept != nil {
				if err := act.Accept(it); err != nil {
					return res, fmt.Errorf("accept %s: %w", it.ID, err)
				}
			}
			res.Accepted++
			fmt.Fprintf(out, "✓ accepted %s\n", it.ID)
		case "e", "edit":
			if act.Edit == nil {
				goto prompt
			}
			if err := act.Edit(it); err != nil {
				fmt.Fprintf(out, "  edit failed (%v); not accepted\n", err)
				goto prompt
			}
			if act.Accept != nil {
				if err := act.Accept(it); err != nil {
					return res, fmt.Errorf("accept %s: %w", it.ID, err)
				}
			}
			res.Accepted++
			fmt.Fprintf(out, "✓ edited + accepted %s\n", it.ID)
		case "r", "reject":
			if act.Discard != nil {
				if err := act.Discard(it); err != nil {
					return res, err
				}
			}
			res.Discarded++
			fmt.Fprintf(out, "✗ discarded %s\n", it.ID)
		case "s", "skip":
			res.Skipped++
			fmt.Fprintf(out, "↷ skipped (left staged)\n")
		case "q", "quit":
			fmt.Fprintf(out, "quit — remaining left staged\n")
			return res, nil
		default:
			goto prompt
		}
	}
	return res, nil
}
