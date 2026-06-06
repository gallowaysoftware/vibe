// Command bench-formats-go benchmarks two inference backends head-to-head
// across several context depths, letting vamp's per-stage throughput capture do
// the measuring. Each stage runs the same long code-gen task; the only things
// that vary are the backend (GGUF+MTP vs EXL3) and the amount of filler context
// prepended to the prompt.
//
// Stages map to capabilities `bench_gguf` / `bench_exl3` in
// ~/.config/vamp/capabilities.yaml (→ the `code` GGUF+MTP profile and the
// `code-exl3` EXL3 profile). All GGUF stages run first (one model load), then
// all EXL3 stages (one swap); a linear After() chain keeps it strictly
// sequential on the single GPU.
//
// After a run, read pipeline_timing.json from the run dir for the full
// per-stage throughput block (gen_tps, prefill_tps, ttft_ms, prompt_tokens).
//
// Usage:  go run . run   |   go run . validate
package main

import (
	"strings"

	"github.com/gallowaysoftware/vibe/vamp"
)

func main() { vamp.Main(pipeline) }

// task is a long, realistic code-generation request so each stage emits a
// large, stable decode sample (amortizing prefill/warmup) with real output.
const task = "Implement a generic, thread-safe LRU cache in Go. Provide the type with a " +
	"constructor New[K comparable, V any](capacity int) and methods Get, Put, Len, Peek, " +
	"Remove, and Purge, achieving O(1) Get/Put with a doubly linked list plus a map and a " +
	"sync.Mutex. Add thorough doc comments on every exported symbol, then a complete " +
	"table-driven test suite covering eviction order, capacity edge cases, " +
	"overwrite-updates-recency, Peek-does-not-update-recency, and concurrent access. " +
	"Finally write a short benchmark. Output Go code with explanations."

// depths are the approximate prompt-token sizes to test prefill/decode at.
// Capped at 96k so it fits both profiles (GGUF ctx 131072, EXL3 163840).
var depths = []struct {
	name string
	tok  int
}{
	{"1k", 1000},
	{"32k", 32000},
	{"96k", 96000},
}

// fillerUnit is code-like padding with no Go-template syntax ("{{") so it
// passes through prompt rendering untouched. ~58 chars/line.
const fillerUnit = "// ref: x := compute(a, b) + offset // benchmark context padding line\n"

func depthPrompt(approxTokens int) string {
	if approxTokens <= 0 {
		return task
	}
	reps := approxTokens * 4 / len(fillerUnit) // ~4 chars/token
	if reps < 1 {
		reps = 1
	}
	var b strings.Builder
	b.Grow(reps*len(fillerUnit) + len(task) + 64)
	b.WriteString("Reference code (context only — ignore it):\n\n")
	for i := 0; i < reps; i++ {
		b.WriteString(fillerUnit)
	}
	b.WriteString("\n\nNow do this:\n")
	b.WriteString(task)
	return b.String()
}

func pipeline() (*vamp.Pipeline, error) {
	p := vamp.New("bench-formats").
		Describe("GGUF+MTP (code) vs EXL3 (code-exl3) across context depths. Identical task; depth = prepended filler. Read gen/prefill tok/s + ttft per stage from pipeline_timing.json.")

	// enable_thinking=false so the budget goes to code, not a <think> trace.
	noThink := map[string]any{"enable_thinking": false}

	var prev *vamp.TextStage
	mk := func(id, capability string, tok int) {
		s := p.Text(id).
			Capability(capability).
			Prompt(depthPrompt(tok)).
			Output(id+".md").
			Param("max_tokens", 1024).
			Param("temperature", 0.2).
			Param("chat_template_kwargs", noThink)
		if prev != nil {
			s.After(prev)
		}
		prev = s
	}

	// All GGUF depths first (single load), then all EXL3 depths (one swap).
	for _, d := range depths {
		mk("gguf_"+d.name, "bench_gguf", d.tok)
	}
	for _, d := range depths {
		mk("exl3_"+d.name, "bench_exl3", d.tok)
	}

	return p.Build()
}
