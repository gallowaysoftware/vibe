package modelprobe

import (
	"testing"

	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
)

func llamaDef(name string, args ...string) *profile.BackendDef {
	return &profile.BackendDef{
		Name: name,
		Backend: profile.Backend{
			External: true,
			LlamaServer: &profile.LlamaServerBackend{
				Path: "/models/" + name + ".gguf", Alias: name, Context: 4096, ExtraArgs: args,
			},
		},
	}
}

// TestSpecsFromDefs_KindComesFromTheServingFlagsNotTheName: an
// embedding model probed with a chat completion measures a 400, and a
// name-based guess ("everything with 'embed' in it") is the kind of
// heuristic this repo keeps rejecting.
func TestSpecsFromDefs_KindComesFromTheServingFlagsNotTheName(t *testing.T) {
	specs := SpecsFromDefs([]*profile.BackendDef{
		llamaDef("qwen3.6-27b"),
		llamaDef("embed-large", "--embedding", "--pooling", "mean"),
		// A chat model whose NAME suggests embeddings.
		llamaDef("embeddings-explainer"),
	}, "/usr/bin/llama-server")

	if got := specs["qwen3.6-27b"].Kind; got != KindChat {
		t.Fatalf("chat def probes as %q", got)
	}
	if got := specs["embed-large"].Kind; got != KindEmbed {
		t.Fatalf("embedding def probes as %q", got)
	}
	if got := specs["embeddings-explainer"].Kind; got != KindChat {
		t.Fatalf("kind was guessed from the model NAME: %q", got)
	}
}

// TestSpecsFromDefs_RerankIsDisabledRatherThanMisProbed: its request
// body is a query plus documents, so the embed batch would measure a
// 400 and call it a throughput number.
func TestSpecsFromDefs_RerankIsDisabledRatherThanMisProbed(t *testing.T) {
	specs := SpecsFromDefs([]*profile.BackendDef{llamaDef("reranker", "--reranking")}, "/usr/bin/llama-server")
	if !specs["reranker"].Disabled {
		t.Fatal("a rerank def was left probeable with a request shape it does not speak")
	}
}

// TestSpecsFromDefs_CloudPeersAreNeverProbed: every probe of a cloud
// peer costs real money and measures someone else's operations.
func TestSpecsFromDefs_CloudPeersAreNeverProbed(t *testing.T) {
	def := &profile.BackendDef{
		Name:    "sonnet",
		Backend: profile.Backend{External: true, CloudPeer: &profile.CloudPeerBackend{BaseURL: "https://api.example.test", Models: []string{"sonnet"}}},
	}
	specs := SpecsFromDefs([]*profile.BackendDef{def}, "/usr/bin/llama-server")
	if !specs["sonnet"].Disabled {
		t.Fatal("a cloud peer was left probeable — every probe of it is a paid request")
	}
}

// TestSpecsFromDefs_BindsTheBaselineToTheRenderedFlags: the fingerprint
// is what makes "the def changed" distinguishable from "the model got
// slower".
func TestSpecsFromDefs_BindsTheBaselineToTheRenderedFlags(t *testing.T) {
	before := SpecsFromDefs([]*profile.BackendDef{llamaDef("qwen", "--ctx-size", "8192")}, "/usr/bin/llama-server")
	after := SpecsFromDefs([]*profile.BackendDef{llamaDef("qwen", "--ctx-size", "16384")}, "/usr/bin/llama-server")
	if before["qwen"].FlagsSHA256 == "" {
		t.Fatal("no fingerprint bound to the spec")
	}
	if before["qwen"].FlagsSHA256 == after["qwen"].FlagsSHA256 {
		t.Fatal("a serving-flag change left the baseline key unchanged")
	}
}

// ─── adversarial-review regressions (ground rule 9, second pass) ──────

// TestSpecsFromDefs_RerankIsDisabledWhateverTheFlagOrder: a reranker IS
// an embedding server plus --reranking (llama.cpp's rerank mode is
// pooling-type rank), so the disabling flag routinely arrives AFTER
// --embedding or --pooling. Deciding on the first flag that matched made
// the same def probeable or disabled depending on which flag the
// operator typed first — and probeable means a 64-input embed batch
// against a rank-pooled server, i.e. a 400 recorded as a failed probe
// every five minutes.
func TestSpecsFromDefs_RerankIsDisabledWhateverTheFlagOrder(t *testing.T) {
	for _, args := range [][]string{
		{"--reranking"},
		{"--embedding", "--reranking"},
		{"--pooling", "rank", "--reranking"},
		{"--embedding", "--pooling", "rank", "--rerank"},
	} {
		specs := SpecsFromDefs([]*profile.BackendDef{llamaDef("reranker", args...)}, "/usr/bin/llama-server")
		if !specs["reranker"].Disabled {
			t.Errorf("args %v left a rerank def probeable with a request shape it does not speak", args)
		}
	}
}
