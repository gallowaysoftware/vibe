package router

import (
	"strings"
	"testing"
)

func TestFlagsSHA256_HomeNormalized(t *testing.T) {
	// The same `~/models/...` def renders as /root/models on one box and
	// /home/alice/models on another — the fingerprint must agree (the C3
	// gate found exactly this: fleetd runs root, cells run users).
	cmd := func(home string) string {
		return home + "/bin/llama-server --model " + home + "/models/q.gguf --ctx-size 4096 --model-draft " + home + "/models/d.gguf"
	}
	t.Setenv("HOME", "/root")
	ref, err := FlagsSHA256(cmd("/root"))
	if err != nil {
		t.Fatal(err)
	}
	for _, home := range []string{"/home/pequalsnp", "/home/alice"} {
		t.Setenv("HOME", home)
		h, err := FlagsSHA256(cmd(home))
		if err != nil {
			t.Fatal(err)
		}
		if h != ref {
			t.Errorf("home %s changed the hash: %s vs %s", home, h, ref)
		}
	}
	// But a genuinely different weights file still mismatches (the
	// normalization must not blind the fingerprint to weights swaps).
	t.Setenv("HOME", "/home/pequalsnp")
	other, _ := FlagsSHA256(`/home/pequalsnp/bin/llama-server --model /home/pequalsnp/models/OTHER.gguf --ctx-size 4096 --model-draft /home/pequalsnp/models/d.gguf`)
	if other == ref {
		t.Error("weights swap produced the same hash — normalization went too far")
	}
}

func TestFlagsSHA256_Canonicalization(t *testing.T) {
	// Flag ORDER must not matter: same flags, different order, same hash.
	a := `/usr/bin/llama-server --model /models/q.gguf --ctx-size 65536 --flash-attn --jinja`
	b := `./llama-server --jinja --ctx-size 65536 --model /models/q.gguf --flash-attn`
	ha, err := FlagsSHA256(a)
	if err != nil {
		t.Fatal(err)
	}
	hb, err := FlagsSHA256(b)
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb {
		t.Errorf("order/binary changed the hash: %s vs %s", ha, hb)
	}

	// The port argument is dropped: with and without --port, same hash.
	c := `/usr/bin/llama-server --model /models/q.gguf --ctx-size 65536 --flash-attn --jinja --port ${PORT}`
	hc, err := FlagsSHA256(c)
	if err != nil {
		t.Fatal(err)
	}
	if hc != ha {
		t.Errorf("port changed the hash: %s vs %s", hc, ha)
	}

	// A real flag difference MUST change it (that is the fingerprint's job).
	d := `/usr/bin/llama-server --model /models/q.gguf --ctx-size 32768 --flash-attn --jinja`
	hd, err := FlagsSHA256(d)
	if err != nil {
		t.Fatal(err)
	}
	if hd == ha {
		t.Error("ctx-size drift produced the same hash — the fingerprint is blind")
	}
}

func TestFlagsSHA256_Quoting(t *testing.T) {
	// The renderer double-quotes values with spaces; the tokenizer must
	// reassemble them, so quoting style never changes the hash.
	quoted := `/usr/bin/llama-server --model "/models/some dir/q.gguf" --ctx-size 4096`
	unquoted := `/usr/bin/llama-server --model /models/some\ dir/q.gguf --ctx-size 4096`
	_ = unquoted // the unquoted form is a different argv; not the comparison point
	ha, err := FlagsSHA256(quoted)
	if err != nil {
		t.Fatalf("quoted cmd: %v", err)
	}
	same := `/usr/bin/llama-server --model "/models/some dir/q.gguf" --ctx-size 4096 --port 5800`
	hb, err := FlagsSHA256(same)
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb {
		t.Errorf("identical flags with a port diverged: %s vs %s", ha, hb)
	}
	// And a quoted value must hash differently from a different value.
	other := `/usr/bin/llama-server --model "/models/other/q.gguf" --ctx-size 4096`
	hc, err := FlagsSHA256(other)
	if err != nil {
		t.Fatal(err)
	}
	if hc == ha {
		t.Error("different model path produced the same hash")
	}
}

func TestTokenizeCmd_Escapes(t *testing.T) {
	argv, err := tokenizeCmd(`bin --flag "a \"quoted\" path" --other x`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"bin", "--flag", `a "quoted" path`, "--other", "x"}
	if strings.Join(argv, "|") != strings.Join(want, "|") {
		t.Errorf("argv = %v, want %v", argv, want)
	}
	if _, err := tokenizeCmd(`bin --flag "unterminated`); err == nil {
		t.Error("unterminated quote must error")
	}
}

// TestFlagsSHA256_ForeignHomeNormalized pins MIN-B: the def carries a
// LITERAL absolute path (no tilde in the yaml), so the two sides never
// share a $HOME to anchor on. Anchoring only on the local home made such
// a def hash differently on cell and fleetd forever — fail-closed on a
// strict def, which yanks a working model.
func TestFlagsSHA256_ForeignHomeNormalized(t *testing.T) {
	const literal = `/usr/bin/llama-server --model /home/pequalsnp/models/q.gguf --ctx-size 4096`

	t.Setenv("HOME", "/root") // fleetd, containerized
	asRoot, err := FlagsSHA256(literal)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", "/home/pequalsnp") // the cell
	asUser, err := FlagsSHA256(literal)
	if err != nil {
		t.Fatal(err)
	}
	if asRoot != asUser {
		t.Errorf("a literal /home path hashed differently by box: %s vs %s", asRoot, asUser)
	}

	// A root-anchored literal folds the same way, and macOS cells too.
	t.Setenv("HOME", "/home/pequalsnp")
	rootPath, _ := FlagsSHA256(`/usr/bin/llama-server --model /root/models/q.gguf --ctx-size 4096`)
	macPath, _ := FlagsSHA256(`/usr/bin/llama-server --model /Users/kyle/models/q.gguf --ctx-size 4096`)
	if rootPath != asUser || macPath != asUser {
		t.Errorf("home folding is not box-independent: root=%s mac=%s want=%s", rootPath, macPath, asUser)
	}

	// Still blind to nothing that matters: a path OUTSIDE any home, and a
	// weights swap inside one, both mismatch.
	outside, _ := FlagsSHA256(`/usr/bin/llama-server --model /srv/models/q.gguf --ctx-size 4096`)
	if outside == asUser {
		t.Error("a non-home path folded into the home form")
	}
	swapped, _ := FlagsSHA256(`/usr/bin/llama-server --model /home/pequalsnp/models/OTHER.gguf --ctx-size 4096`)
	if swapped == asUser {
		t.Error("weights swap produced the same hash")
	}
}
