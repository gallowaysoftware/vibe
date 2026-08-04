package modelprobe

import (
	"log/slog"
	"strings"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
	"github.com/gallowaysoftware/vibe/internal/vibe/router"
)

// Hooks is the announce loop's one-liner wiring, shared by the daemon's
// loop and `vibe fleet announce` so the two can't drift (usagemeter's
// Snapshotter sets the precedent). A prober that cannot even be
// constructed degrades to "this cell announces no probe blocks" — the
// truth — instead of costing the cell its heartbeat.
func Hooks(llamaSwapURL, statePath string, defs []*profile.BackendDef, llamaBinary string) (func() map[string]*fleetapi.AnnounceProbe, func(string, bool)) {
	specs := SpecsFromDefs(defs, llamaBinary)
	p, err := New(Config{
		LlamaSwapURL: llamaSwapURL,
		StatePath:    statePath,
		Specs: func(model string) Spec {
			if s, ok := specs[model]; ok {
				return s
			}
			// A catalog id with no def announces hashless (C3's rule) and
			// probes with no baseline binding: still measurable, just not
			// pinned to a rendered argv.
			return Spec{Model: model, Kind: KindChat}
		},
	})
	if err != nil {
		slog.Warn("probe support not started; this cell announces no probe results", "err", err)
		return nil, nil
	}
	return p.Results, p.Start
}

// SpecsFromDefs derives each def's probe spec from the SAME rendered
// argv the C3 fingerprint is computed over — so the kind is read off
// what the server is actually told to do, not guessed from a model name,
// and the baseline binds to the exact flags it was measured against.
func SpecsFromDefs(defs []*profile.BackendDef, llamaBinary string) map[string]Spec {
	out := make(map[string]Spec, len(defs))
	for _, def := range defs {
		if def == nil {
			continue
		}
		spec := Spec{Model: def.Name, Kind: KindChat}
		// Only the spec-rendered kinds have an argv to read (the same
		// split gatherModels applies to fingerprints).
		if def.Backend.LlamaServer != nil || def.Backend.MLXServer != nil {
			if cmd, err := router.ModelCmd(def, router.Options{LlamaServerBinary: llamaBinary}); err == nil {
				if hash, err := router.FlagsSHA256(cmd); err == nil {
					spec.FlagsSHA256 = hash
				}
				spec.Kind, spec.Disabled = kindFromArgv(cmd)
			}
		}
		if def.Backend.CloudPeer != nil {
			// A cloud peer's throughput is someone else's operational
			// problem and every probe of it costs real money.
			spec.Disabled = true
		}
		out[def.Name] = spec
	}
	return out
}

// kindFromArgv reads the probe kind off the serving flags. Rerank is
// DISABLED rather than mis-probed: its request body is a query plus
// documents, so sending it the embed batch would measure a 400.
func kindFromArgv(cmd string) (kind string, disabled bool) {
	fields := strings.Fields(cmd)
	for _, f := range fields {
		switch {
		case f == "--reranking" || f == "--rerank":
			return KindEmbed, true
		case f == "--embedding" || f == "--embeddings":
			return KindEmbed, false
		case strings.HasPrefix(f, "--pooling"):
			// --pooling only makes sense on an embedding server.
			return KindEmbed, false
		}
	}
	return KindChat, false
}
