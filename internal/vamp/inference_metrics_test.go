package vamp

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestFinalizeMetricsTimings(t *testing.T) {
	// llama-server exact timings take precedence over derived values.
	m := &InferenceMetrics{}
	finalizeMetrics(m,
		&usageJSON{PromptTokens: 100, CompletionTokens: 2},
		&timingsJSON{PromptN: 100, PromptMS: 50, PredictedN: 2, PredictedMS: 20},
		30, 120)

	if m.Source != "timings" {
		t.Fatalf("source = %q, want timings", m.Source)
	}
	if m.PromptTokens != 100 || m.CompletionTokens != 2 {
		t.Fatalf("tokens = %d/%d, want 100/2", m.PromptTokens, m.CompletionTokens)
	}
	// 2 tokens / 0.020s = 100 tok/s ; 100 tokens / 0.050s = 2000 tok/s
	if m.GenTPS != 100 {
		t.Fatalf("gen_tps = %v, want 100", m.GenTPS)
	}
	if m.PrefillTPS != 2000 {
		t.Fatalf("prefill_tps = %v, want 2000", m.PrefillTPS)
	}
}

func TestFinalizeMetricsUsageDerived(t *testing.T) {
	// No timings (tabbyAPI): derive from usage + measured TTFT/total.
	m := &InferenceMetrics{}
	finalizeMetrics(m, &usageJSON{PromptTokens: 10, CompletionTokens: 5}, nil, 100, 1100)

	if m.Source != "usage" {
		t.Fatalf("source = %q, want usage", m.Source)
	}
	// decode = 1100-100 = 1000ms; (5-1)/1.0s = 4 tok/s
	if m.GenTPS != 4 {
		t.Fatalf("gen_tps = %v, want 4", m.GenTPS)
	}
	// prefill = prompt_tokens / ttft = 10 / 0.1s = 100 tok/s
	if m.PrefillTPS != 100 {
		t.Fatalf("prefill_tps = %v, want 100", m.PrefillTPS)
	}
}

func TestFinalizeMetricsNilSafe(t *testing.T) {
	finalizeMetrics(nil, nil, nil, 0, 0) // must not panic
}

func TestParseSSEStreamCapturesMetrics(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Hello"}}]}`,
		``,
		`data: {"choices":[{"delta":{"content":" world"}}]}`,
		``,
		// include_usage final chunk: empty choices, carries usage + timings.
		`data: {"choices":[],"usage":{"prompt_tokens":42,"completion_tokens":2},"timings":{"prompt_n":42,"prompt_ms":21,"predicted_n":2,"predicted_ms":10}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	var got strings.Builder
	m := &InferenceMetrics{}
	// start 50ms in the past so the synthetic (instant) first delta yields a
	// non-zero, millisecond-resolution TTFT.
	out, err := parseSSEStream(context.Background(), strings.NewReader(sse),
		func(d string) { got.WriteString(d) }, time.Now().Add(-50*time.Millisecond), m)
	if err != nil {
		t.Fatalf("parseSSEStream: %v", err)
	}
	if out != "Hello world" || got.String() != "Hello world" {
		t.Fatalf("content = %q / streamed %q, want \"Hello world\"", out, got.String())
	}
	if m.Source != "timings" || m.CompletionTokens != 2 || m.PromptTokens != 42 {
		t.Fatalf("metrics = %+v, want timings/2/42", m)
	}
	if m.GenTPS != 200 { // 2 / 0.010s
		t.Fatalf("gen_tps = %v, want 200", m.GenTPS)
	}
	if m.TTFTms <= 0 {
		t.Fatalf("ttft not recorded: %d", m.TTFTms)
	}
}

func TestForeachItemMeteringAggregates(t *testing.T) {
	tr := NewTracker("bench")
	tr.StageStart("fanout", "text")
	tr.ItemStart("fanout", 0)
	tr.ItemStart("fanout", 1)
	// Two items, each 100 decode tokens. Item0: 100 tok/s -> ~1.0s decode.
	// Item1: 50 tok/s -> ~2.0s decode. Aggregate = total decode tokens /
	// total decode secs = (99+99) / (0.99+1.98) = 198/2.97 ≈ 66.7 tok/s.
	tr.ItemThroughput("fanout", 0, &InferenceMetrics{CompletionTokens: 100, GenTPS: 100, PromptTokens: 10, PrefillTPS: 100, TTFTms: 100})
	tr.ItemThroughput("fanout", 1, &InferenceMetrics{CompletionTokens: 100, GenTPS: 50, PromptTokens: 10, PrefillTPS: 100, TTFTms: 300})
	tr.ItemEnd("fanout", 0, "ok", nil)
	tr.ItemEnd("fanout", 1, "ok", nil)
	tr.StageEnd("fanout", "ok", nil)

	rep := tr.Report()
	if len(rep.Stages) != 1 {
		t.Fatalf("stages = %d, want 1", len(rep.Stages))
	}
	s := rep.Stages[0]
	if s.Throughput == nil {
		t.Fatal("foreach stage has no aggregated throughput")
	}
	if s.Throughput.CompletionTokens != 200 {
		t.Fatalf("agg tokens = %d, want 200", s.Throughput.CompletionTokens)
	}
	// 198 decode tokens / 2.97s ≈ 66.7
	if g := s.Throughput.GenTPS; g < 66 || g > 67 {
		t.Fatalf("agg gen_tps = %v, want ~66.7", g)
	}
	if s.Throughput.TTFTms != 200 { // mean of 100 and 300
		t.Fatalf("agg ttft = %d, want 200", s.Throughput.TTFTms)
	}
	// Per-item throughput must survive into the report too.
	if len(s.Items) != 2 || s.Items[0].Throughput == nil || s.Items[0].Throughput.CompletionTokens != 100 {
		t.Fatalf("per-item throughput missing: %+v", s.Items)
	}
}

func TestFormatTableThroughputColumns(t *testing.T) {
	tr := NewTracker("bench")
	// A text stage with throughput, and a non-LLM stage without.
	tr.StageStart("draft", "text")
	tr.StageThroughput("draft", &InferenceMetrics{
		CompletionTokens: 512, GenTPS: 142.4, TTFTms: 210, Source: "timings",
	})
	tr.StageEnd("draft", "ok", nil)
	tr.StageStart("encode", "ffmpeg")
	tr.StageEnd("encode", "ok", nil)

	var buf bytes.Buffer
	if err := tr.FormatTable(&buf); err != nil {
		t.Fatalf("FormatTable: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"tok/s", "tokens", "ttft", "142", "512"} {
		if !strings.Contains(out, want) {
			t.Fatalf("table missing %q:\n%s", want, out)
		}
	}
	// The ffmpeg stage must not invent a throughput number.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "encode") && strings.Contains(line, "142") {
			t.Fatalf("non-LLM stage leaked throughput: %q", line)
		}
	}
}
