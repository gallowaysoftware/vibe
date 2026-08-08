package benchreplay

import (
	"fmt"
	"io"
	"strings"

	"github.com/gallowaysoftware/vibe/internal/vibe/observed"
)

// The renderer. C7b's precedent, applied to a screen that is even easier
// to over-read than a savings figure: state what a number does not mean,
// on the same screen as the number, because a screen that can only render
// triumph will.
//
// Nothing here can print capture text. Report carries none — that is
// mechanism 1 — so this file is where the invariant is USED rather than
// where it is enforced.

// Render writes the replay block.
func (r *Report) Render(w io.Writer) {
	n := r.Sample.Replayable
	fmt.Fprintf(w, "replay: %d of this cell's own recent requests, through both models\n", n)
	// Every term of the arithmetic, including the two that mean "this
	// process could not read it" rather than "the cell did not serve it".
	// A denominator that quietly shrank is a denominator nobody can judge,
	// and `refused` in particular is a credential problem wearing a
	// workload's clothes.
	fmt.Fprintf(w, "  sampled:  %d walked, %d advertised a capture, %d evicted before the fetch, %d not replayable, %d unreadable\n",
		r.Sample.Walked, r.Sample.Candidates, r.Sample.Evicted, r.Sample.SkippedBasis, r.Sample.Malformed)
	if r.Sample.Refused > 0 {
		fmt.Fprintf(w, "  REFUSED:  %d capture fetch(es) answered HTTP %d rather than the capture. That is what this\n",
			r.Sample.Refused, r.Sample.RefusedStatus)
		fmt.Fprintln(w, "            process could read, not what this cell served — a cell that keys its own")
		fmt.Fprintln(w, "            llama-swap needs the key, and this command sends none (C15, C18 §8).")
	}

	if n < r.Sample.Floor {
		fmt.Fprintf(w, "\n  n = %d is below the rate floor of %d, so NO rate is shown. A proportion on this many\n", n, r.Sample.Floor)
		fmt.Fprintln(w, "  samples is noise wearing a percent sign. The per-request table is below.")
	}

	fmt.Fprintln(w)
	r.renderThroughput(w)
	fmt.Fprintln(w)
	r.renderStructure(w)
	fmt.Fprintln(w)
	r.renderDivergence(w)
	if len(r.Rows) > 0 {
		fmt.Fprintln(w)
		r.renderRows(w)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "what this replay is not:")
	for _, c := range r.Caveats() {
		fmt.Fprintf(w, "  - %s\n", wrap(c, 74, "    "))
	}
}

func (r *Report) renderThroughput(w io.Writer) {
	// The known bit is read, not discarded. `Why` is the sender's account
	// of what happened and the Value is the measurement itself; a renderer
	// that trusted the first and dropped the second is the "sender guards,
	// receiver doesn't" shape this repo keeps finding. Both must agree
	// before a ratio is printed.
	ratio, haveRatio := r.Paired.MedianRatio.Observed()
	switch {
	case r.Paired.Why == PairedOK && haveRatio:
		pct := (ratio - 1) * 100
		dir := "faster"
		if pct < 0 {
			dir, pct = "slower", -pct
		}
		fmt.Fprintf(w, "  throughput: %s is %.0f%% %s than %s on the MEDIAN paired request (%.2fx, n=%d, %s)\n",
			r.Candidate.Model, pct, dir, r.Incumbent.Model, ratio, r.Paired.N, r.Incumbent.Metric)
		fmt.Fprintln(w, "              median of per-request ratios, never a ratio of means: both sides saw the")
		fmt.Fprintln(w, "              identical sample, so prompt-length composition cancels exactly.")
	case r.Paired.Why == PairedMixedMetrics:
		fmt.Fprintf(w, "  throughput: NO ratio. The two sides did not report the same metric (%s vs %s).\n",
			dash(r.Incumbent.Metric), dash(r.Candidate.Metric))
		fmt.Fprintln(w, "              decode_tok_s is llama.cpp's own decode timing and excludes queueing;")
		fmt.Fprintln(w, "              e2e_tok_s is wall time for the whole request. They are not one column.")
	case r.Paired.Why == PairedUnderFloor:
		fmt.Fprintf(w, "  throughput: NO ratio. Only %d request(s) produced a rate on both sides, under C8's\n", r.Paired.N)
		fmt.Fprintf(w, "              five-sample minimum for a paired scalar. The per-request rates are below.\n")
	default:
		fmt.Fprintln(w, "  throughput: NO ratio. No request produced a rate on both sides.")
	}
}

func (r *Report) renderStructure(w io.Writer) {
	rows := [][3]string{
		{"", r.Incumbent.Model, r.Candidate.Model},
		{"answered", count(r.Incumbent.Answered), count(r.Candidate.Answered)},
		{"no answer", count(r.Incumbent.Failed), count(r.Candidate.Failed)},
		{"tools offered", count(r.Incumbent.ToolsOffered), count(r.Candidate.ToolsOffered)},
		{"tool called", count(r.Incumbent.ToolCalled), count(r.Candidate.ToolCalled)},
		{"tool-call rate", rate(r.Incumbent.ToolCallRate), rate(r.Candidate.ToolCallRate)},
		{"name declared", count(r.Incumbent.ToolDeclared), count(r.Candidate.ToolDeclared)},
		{ToolUndeclared, count(r.Incumbent.ToolUndeclared), count(r.Candidate.ToolUndeclared)},
		{"args parse", count(r.Incumbent.ArgsValidJSON), count(r.Candidate.ArgsValidJSON)},
		{"args complete", count(r.Incumbent.ArgsHaveRequired), count(r.Candidate.ArgsHaveRequired)},
		{"schema asked", count(r.Incumbent.SchemaAsked), count(r.Candidate.SchemaAsked)},
		{"schema conformed", count(r.Incumbent.SchemaConformed), count(r.Candidate.SchemaConformed)},
		{"finish stop", count(r.Incumbent.FinishStop), count(r.Candidate.FinishStop)},
		{"finish length", count(r.Incumbent.FinishLength), count(r.Candidate.FinishLength)},
		{"finish tool_calls", count(r.Incumbent.FinishToolCalls), count(r.Candidate.FinishToolCalls)},
		{"truncated", count(r.Incumbent.Truncated), count(r.Candidate.Truncated)},
		{"empty", count(r.Incumbent.Empty), count(r.Candidate.Empty)},
		{"output tokens", count(r.Incumbent.OutTokens), count(r.Candidate.OutTokens)},
	}
	table(w, rows)
}

func (r *Report) renderDivergence(w io.Writer) {
	d := r.Divergence
	if d.N == 0 {
		fmt.Fprintln(w, "  divergence: NOT MEASURED. No sample had a recorded response that could be")
		fmt.Fprintln(w, "              reduced to a structure — llama-swap stores no response body for a")
		fmt.Fprintln(w, "              non-200, and those are the most interesting requests in a sample.")
		return
	}
	fmt.Fprintf(w, "  divergence: the NOISE FLOOR is %d of %d — that is %s disagreeing with its OWN\n",
		d.IncumbentDisagreed, d.N, r.Incumbent.Model)
	fmt.Fprintln(w, "              recorded output, because the captured requests carry the client's own")
	fmt.Fprintln(w, "              temperature and a model does not reproduce itself.")
	// Same rule as renderThroughput: the flag and the number must agree.
	// An excess nobody computed is not a small one.
	excess, haveExcess := d.Excess.Observed()
	if !d.Claimed || !haveExcess {
		fmt.Fprintf(w, "              %s disagreed %d of %d — at or below the floor, so there is NO\n",
			r.Candidate.Model, d.CandidateDisagreed, d.N)
		fmt.Fprintln(w, "              divergence claim to make here.")
		return
	}
	fmt.Fprintf(w, "              %s disagreed %d of %d, i.e. %.0f points ABOVE the floor.\n",
		r.Candidate.Model, d.CandidateDisagreed, d.N, excess*100)
	fmt.Fprintln(w, "              Structural only: tool-call-vs-prose, which tool, finish reason, whether")
	fmt.Fprintln(w, "              the arguments parse. Different wording is not a signal and is not counted.")
}

func (r *Report) renderRows(w io.Writer) {
	fmt.Fprintln(w, "  per request (activity id, incumbent | candidate):")
	rows := [][3]string{{"id", r.Incumbent.Model, r.Candidate.Model}}
	for _, row := range r.Rows {
		rows = append(rows, [3]string{
			fmt.Sprintf("%d", row.ActivityID),
			cell(row.IncumbentStatus, row.IncumbentTokS, row.IncumbentTool, row.IncumbentFinish),
			cell(row.CandidateStatus, row.CandidateTokS, row.CandidateTool, row.CandidateFinish),
		})
	}
	table(w, rows)
}

// cell renders one side of one request. The tool column is the CLOSED SET
// — none / declared / <undeclared> — and never a name: a name the model
// invented is model output, which is to say private traffic, and a name
// the request declared is capture text too.
func cell(status int, tokS float64, tool, finish string) string {
	rate := "—"
	if tokS > 0 {
		rate = fmt.Sprintf("%.1f/s", tokS)
	}
	if status != 200 && status != 0 {
		return fmt.Sprintf("HTTP %d", status)
	}
	if status == 0 {
		return "no answer"
	}
	return fmt.Sprintf("%s %s %s", rate, tool, finish)
}

// Caveats is computed from the numbers rather than hardcoded, so a caveat
// that stopped applying stops printing — a list that is the same every
// time is a list nobody reads.
func (r *Report) Caveats() []string {
	out := []string{
		"this is YOUR traffic, which means it is not a fixed benchmark. Re-running tomorrow " +
			"samples different requests, so these numbers compare two models against ONE sample in ONE " +
			"run and nothing here is stored or trended. A score kept across runs would compare two " +
			"workloads and call the difference a model change.",
		"the sample is whatever survived llama-swap's ~10 MB in-RAM FIFO capture buffer: the most " +
			"recent requests, biased toward long agentic prompts, and emptied by every restart and " +
			"every config reload.",
		"the replay forces stream=false on both sides. That is the only edit to the client's own " +
			"request besides the model id — temperature, top_p, max_tokens and the tools are as " +
			"recorded — and it is what makes llama.cpp's timings block arrive as one object rather " +
			"than as frames whose boundaries are not a stable contract.",
		"a captured agentic request carries its whole prior conversation in its own body, so every " +
			"replay is single-shot. Nothing here measures how a model behaves over a session.",
	}
	if r.Sample.Replayable < r.Sample.Floor {
		out = append(out, fmt.Sprintf(
			"n = %d is under the rate floor of %d, so every rate above reads `unknown` rather than a "+
				"number. That is not a zero; it is the absence of enough evidence to state a proportion.",
			r.Sample.Replayable, r.Sample.Floor))
	}
	if r.Sample.Evicted > 0 {
		out = append(out, fmt.Sprintf(
			"%d capture(s) were evicted between the activity read and the fetch, because FIFO eviction "+
				"proceeds while a harvest is running. They are counted, never retried, and never guessed at.",
			r.Sample.Evicted))
	}
	if r.Divergence.N < r.Sample.Replayable {
		out = append(out, fmt.Sprintf(
			"divergence covers %d of %d samples. The rest had no recorded response that could be "+
				"reduced — llama-swap stores no response body for a non-200 — and a request that made "+
				"the model choke is the most interesting one to replay and the one with nothing to "+
				"diverge from.", r.Divergence.N, r.Sample.Replayable))
	}
	out = append(out,
		fmt.Sprintf("this run added %d real requests and %d output tokens to this cell-day, under "+
			"`%s` and `%s` in C7a's ledger. Nothing tags replay traffic; the cost is disclosed instead. "+
			"It also displaced older captures from the buffer, since the replays are themselves captured.",
			r.Incumbent.Requests+r.Candidate.Requests,
			r.Incumbent.OutTokens+r.Candidate.OutTokens,
			r.Incumbent.Model, r.Candidate.Model),
		"a bad score here changes NOTHING. No model is marked degraded, nothing is unloaded, no alias "+
			"moves, no model leaves the render and fleetd is told none of it. The measurement is not an "+
			"actuator (C8's cardinal rule).",
	)
	return out
}

// ── formatting ──────────────────────────────────────────────────────────

func table(w io.Writer, rows [][3]string) {
	var widths [3]int
	for _, r := range rows {
		for i, c := range r {
			if len(c) > widths[i] {
				widths[i] = len(c)
			}
		}
	}
	for _, r := range rows {
		fmt.Fprintf(w, "  %-*s  %*s  %*s\n", widths[0], r[0], widths[1], r[1], widths[2], r[2])
	}
}

func count(n int) string { return fmt.Sprintf("%d", n) }

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// rate renders an observed rate. An unknown one prints "unknown", never
// "0%": the whole reason it is an observed.Value is that a rate nobody
// could compute must not read as a measured floor.
func rate(v observed.Value[float64]) string {
	f, ok := v.Observed()
	if !ok {
		return "unknown"
	}
	return fmt.Sprintf("%.0f%%", f*100)
}

func wrap(s string, width int, indent string) string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return s
	}
	var b strings.Builder
	line := words[0]
	for _, w := range words[1:] {
		if len(line)+1+len(w) > width {
			b.WriteString(line + "\n" + indent)
			line = w
			continue
		}
		line += " " + w
	}
	b.WriteString(line)
	return b.String()
}
