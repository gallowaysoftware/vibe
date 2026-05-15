package supervisor

import (
	"reflect"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/profile"
)

func TestBuildArgs_Full(t *testing.T) {
	p := &profile.Profile{
		Model: profile.Model{
			Path:       "/m/qwen.gguf",
			Alias:      "qwen",
			Context:    8192,
			Parallel:   2,
			GPULayers:  99,
			FlashAttn:  true,
			CacheTypeK: "q8_0",
			CacheTypeV: "q8_0",
			Jinja:      true,
			ExtraArgs:  []string{"--no-mmap"},
		},
	}
	got := BuildArgs(p, 8080)
	want := []string{
		"--model", "/m/qwen.gguf",
		"--host", "127.0.0.1",
		"--port", "8080",
		"--alias", "qwen",
		"--ctx-size", "8192",
		"--parallel", "2",
		"--n-gpu-layers", "99",
		"--flash-attn",
		"--cache-type-k", "q8_0",
		"--cache-type-v", "q8_0",
		"--jinja",
		"--no-mmap",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}
}

func TestBuildArgs_OmitsZeroValueFlags(t *testing.T) {
	p := &profile.Profile{
		Model: profile.Model{
			Path:     "/m/q.gguf",
			Alias:    "q",
			Context:  4096,
			Parallel: 1,
		},
	}
	args := BuildArgs(p, 9999)
	mustNotContain := []string{"--flash-attn", "--jinja", "--cache-type-k", "--cache-type-v", "--n-gpu-layers"}
	for _, bad := range mustNotContain {
		for _, a := range args {
			if a == bad {
				t.Errorf("unexpected flag %s in %v", bad, args)
			}
		}
	}
}

func TestRingBuffer_OverflowKeepsRecent(t *testing.T) {
	rb := newRingBuffer(3)
	rb.Add("a")
	rb.Add("b")
	if got := rb.Lines(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("got %v", got)
	}
	rb.Add("c")
	rb.Add("d")
	if got := rb.Lines(); !reflect.DeepEqual(got, []string{"b", "c", "d"}) {
		t.Errorf("got %v", got)
	}
	rb.Reset()
	if got := rb.Lines(); len(got) != 0 {
		t.Errorf("after reset got %v", got)
	}
}
