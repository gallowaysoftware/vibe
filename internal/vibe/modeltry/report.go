package modeltry

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// The report. C7b's precedent applies verbatim: a number that cannot say
// what it is not is a number that will be believed too hard. Everything
// in here that looks like editorial hedging is load-bearing — the whole
// point of the verb is to compress a week's toil into one command, and
// the thing that makes that safe is that the answer arrives with its
// error bars in the same paragraph.

// caveats lists what this comparison did NOT control for. It is computed
// from the measurements rather than hardcoded so a caveat that stopped
// applying stops printing — a list that is the same every time is a list
// nobody reads.
func caveats(c *Comparison) []string {
	out := []string{
		"one sample each, not a benchmark. C8's verdicts need five samples before they mean anything; these are two.",
		"the trial was measured SECOND. On a single-GPU cell loading one model evicts the other, so the two cannot be measured side by side, and whichever goes second has the warmer page cache — its weights were written to this disk minutes ago.",
		"this compares DEFS, not models. The trial inherited the incumbent's flags (context, layer split, cache types) and its own quantization; a candidate that wants different flags will measure differently under them.",
	}
	if c.Trial.Metric != "" && c.Incumbent.Metric != "" && c.Trial.Metric != c.Incumbent.Metric {
		out = append(out, fmt.Sprintf("the two sides reported DIFFERENT metrics (%s vs %s) and are not comparable: %s is llama.cpp's own decode timing and excludes queueing, %s is wall time for the whole request. No ratio is shown.",
			c.Trial.Metric, c.Incumbent.Metric, metricGloss(c.Trial.Metric), metricGloss(c.Incumbent.Metric)))
	}
	if c.Trial.Note != "" || c.Incumbent.Note != "" {
		out = append(out, "at least one side did not produce a rate; see its note. An absent number is not a slow one.")
	}
	// The quality caveat is C18's own, and C25 is the phase that fills it.
	// It stops printing when a replay actually measured the thing, because
	// a caveat that is the same every time is a caveat nobody reads.
	if c.Replay == nil {
		out = append(out,
			"nothing here says the answer is right. Throughput is not quality: a faster model that tool-calls worse is a worse model, and no probe in this repo measures that. `--replay` scores both models against this cell's own recent requests instead (C25).",
		)
	} else {
		out = append(out,
			"the throughput rows above are ONE sample each. The replay block below is the multi-request one, and it is the half that says anything about tool-calling.",
		)
	}
	if c.ReplayNote != "" {
		out = append(out, "a replay was asked for and did not produce a score; its reason is printed above. An absent replay is not a passing one.")
	}
	return out
}

func metricGloss(m string) string {
	switch m {
	case "decode_tok_s":
		return "decode_tok_s"
	case "e2e_tok_s":
		return "e2e_tok_s"
	case "embed_inputs_s":
		return "embed_inputs_s"
	default:
		return m
	}
}

// Render writes the comparison. The layout puts the trial FIRST because
// the operator asked about the trial, and the incumbent's rolling
// baseline on its own row because it is the only multi-sample number on
// the page.
func (c *Comparison) Render(w io.Writer) {
	fmt.Fprintf(w, "trial %s vs incumbent %s on %s (%s)\n\n",
		c.Trial.Model, c.Incumbent.Model, c.Cell, c.At.Local().Format(time.RFC1123))

	rows := [][3]string{
		{"", c.Trial.Model, c.Incumbent.Model},
		{"throughput", rate(c.Trial), rate(c.Incumbent)},
		{"metric", dashIfEmpty(c.Trial.Metric), dashIfEmpty(c.Incumbent.Metric)},
		{"time to first token", ttft(c.Trial), ttft(c.Incumbent)},
		{"this box's baseline", baseline(c.Trial), baseline(c.Incumbent)},
		{"weights on disk", sizeCell(c.Trial.WeightsBytes), sizeCell(c.Incumbent.WeightsBytes)},
		{"estimated VRAM", vram(c.Trial), vram(c.Incumbent)},
	}
	widths := [3]int{0, 0, 0}
	for _, r := range rows {
		for i, cell := range r {
			if len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}
	for _, r := range rows {
		fmt.Fprintf(w, "  %-*s  %*s  %*s\n", widths[0], r[0], widths[1], r[1], widths[2], r[2])
	}

	fmt.Fprintln(w)
	if d := c.Delta(); d != "" {
		fmt.Fprintf(w, "  %s\n\n", d)
	}
	if c.Trial.Note != "" {
		fmt.Fprintf(w, "  %s: %s\n", c.Trial.Model, c.Trial.Note)
	}
	if c.Incumbent.Note != "" {
		fmt.Fprintf(w, "  %s: %s\n", c.Incumbent.Model, c.Incumbent.Note)
	}
	// The C25 block, when there is one. It goes ABOVE the caveats because
	// its own caveats are printed with it, and below the throughput table
	// because the operator asked about speed first.
	if c.ReplayNote != "" {
		fmt.Fprintf(w, "\n  replay: %s\n", c.ReplayNote)
	}
	if c.Replay != nil {
		fmt.Fprintln(w)
		c.Replay.Render(w)
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, "what this number is not:")
	for _, s := range c.Caveats {
		fmt.Fprintf(w, "  - %s\n", wrapIndent(s, 74, "    "))
	}
}

// Delta is the one comparative sentence, and it is only produced when
// both sides measured THE SAME quantity. Comparing a decode rate against
// a wall-clock rate would put two different measurements under one
// percentage — the failure C8 avoids by keeping metric in the baseline
// key, applied to the surface a human reads.
func (c *Comparison) Delta() string {
	t, i := c.Trial, c.Incumbent
	if t.Value <= 0 || i.Value <= 0 || t.Metric == "" || t.Metric != i.Metric {
		return ""
	}
	ratio := t.Value / i.Value
	pct := (ratio - 1) * 100
	dir := "faster"
	if pct < 0 {
		dir, pct = "slower", -pct
	}
	return fmt.Sprintf("%s measured %.0f%% %s than %s on %s (%.2fx) — one sample each, on this box, right now.",
		t.Model, pct, dir, i.Model, t.Metric, ratio)
}

func rate(m Measurement) string {
	if m.Value <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f/s", m.Value)
}

func ttft(m Measurement) string {
	if m.TTFTMS <= 0 {
		return "—"
	}
	return fmt.Sprintf("%d ms", m.TTFTMS)
}

// baseline renders C8's rolling window, which is the honest multi-sample
// figure. Under C8's minimum it says so rather than printing a median
// nobody should act on.
func baseline(m Measurement) string {
	if m.BaselineP50 <= 0 || m.Samples <= 0 {
		return "none yet"
	}
	if m.Samples < 5 {
		return fmt.Sprintf("%.1f/s (n=%d, under C8's 5)", m.BaselineP50, m.Samples)
	}
	return fmt.Sprintf("%.1f/s (n=%d)", m.BaselineP50, m.Samples)
}

func vram(m Measurement) string {
	if m.VRAMEstimateGB <= 0 {
		return "not declared"
	}
	return fmt.Sprintf("%.1f GB", m.VRAMEstimateGB)
}

func sizeCell(n int64) string {
	if n <= 0 {
		return "—"
	}
	return humanBytes(n)
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// wrapIndent hard-wraps a caveat so a 74-column terminal renders it as a
// paragraph rather than one line that scrolls off.
func wrapIndent(s string, width int, indent string) string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return s
	}
	var b strings.Builder
	line := words[0]
	for _, w := range words[1:] {
		if len(line)+1+len(w) > width {
			b.WriteString(line)
			b.WriteString("\n")
			b.WriteString(indent)
			line = w
			continue
		}
		line += " " + w
	}
	b.WriteString(line)
	return b.String()
}

// RenderStatus is `vibe model try status`: where the trial is, what it
// has done to this box, and the exact next command. A status that does
// not name the rollback is a status that leaves a stranded config.
func (t *Trial) RenderStatus(w io.Writer) {
	fmt.Fprintf(w, "trial %s on %s: %s (started %s)\n", t.Def, t.Cell, t.State, t.StartedAt.Local().Format(time.RFC1123))
	fmt.Fprintf(w, "  candidate:  %s / %s\n", t.Repo, t.File)
	fmt.Fprintf(w, "  modelled on: %s\n", t.Incumbent)
	fmt.Fprintf(w, "  weights:    %s\n", t.WeightsPath)
	if t.State != StatePlanned {
		fmt.Fprintf(w, "  def:        %s\n", t.DefPath)
	}
	switch t.State {
	case StateApplied, StateMeasured:
		fmt.Fprintf(w, "  config:     %s (previous banked at %s)\n", t.ConfigPath, dashIfEmpty(t.BackupPath))
		if !t.Live {
			fmt.Fprintln(w, "  NOT LIVE:   the config declares the trial but this cell's llama-swap has not adopted it")
		}
	}
	if t.Leased || t.Held {
		fmt.Fprintf(w, "  declared:   lease=%v hold-on-incumbent=%v (released by `vibe model try end`)\n", t.Leased, t.Held)
	}
	if len(t.Log) > 0 {
		fmt.Fprintln(w, "  log:")
		for _, e := range t.Log {
			fmt.Fprintf(w, "    %s  %s\n", e.At.Local().Format("15:04:05"), e.Note)
		}
	}
	if t.Result != nil {
		fmt.Fprintln(w)
		t.Result.Render(w)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "roll it back: vibe model try end            (keeps the weights)")
	fmt.Fprintln(w, "              vibe model try end --purge    (deletes them too)")
	fmt.Fprintf(w, "keep it:      edit %s, delete the `trial: true` line, review every flag, commit it, then `vibe router render`.\n", t.DefPath)
	fmt.Fprintln(w, "              until that def is in the fleet's def checkout the id is reachable on this cell only — it is not in the front's catalog.")
}
