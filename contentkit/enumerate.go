package contentkit

import (
	"fmt"
	"strings"

	"github.com/gallowaysoftware/vibe/vamp"
)

// EnumerateConfig parameterises EnumerateItems.
type EnumerateConfig struct {
	// From is the upstream stage whose JSON output holds the array to enumerate.
	From vamp.Ref
	// StageName is the render stage's id (e.g. "enumerate_shots").
	StageName string
	// Output is the run-dir filename to write (e.g. "shots.json").
	Output string
	// ArrayKey is the key into From's JSON object holding the array. Default "items".
	ArrayKey string
	// IndexKey is the per-item index field to emit. Default "idx".
	IndexKey string
	// Fields are copied verbatim from each source item into each emitted item
	// (via toJSON), in order. e.g. ["image_prompt","motion","narration","speaker","voice_id"].
	Fields []string
}

// EnumerateItems builds the deterministic render stage that reads From's JSON
// output, walks its ArrayKey array, stamps each element with a stable IndexKey,
// and re-emits {"items":[{<IndexKey>:i, <field>:<value>, ...}]} — the shape a
// downstream vamp Foreach consumes.
//
// This is the exact template worldsmith (scene shots, narration segments) and
// brainrot (episode shots) each hand-rolled verbatim; this is now the one source.
// It changes no behaviour — the emitted JSON matches the inline templates.
func EnumerateItems(p *vamp.Pipeline, cfg EnumerateConfig) *vamp.RenderStage {
	if cfg.IndexKey == "" {
		cfg.IndexKey = "idx"
	}
	if cfg.ArrayKey == "" {
		cfg.ArrayKey = "items"
	}

	return p.Render(cfg.StageName).
		After(cfg.From).
		Prompt(enumerateTemplate(cfg.From.ID(), cfg.ArrayKey, cfg.IndexKey, cfg.Fields)).
		Output(cfg.Output).
		OutputFormatJSON()
}

// enumerateTemplate builds the vamp render template (pure; unit-tested). It
// emits {"items":[{<indexKey>:i, <field>:<toJSON value>, ...}]} over the array
// at arrayKey in stage fromID's JSON output — the shape a Foreach consumes.
func enumerateTemplate(fromID, arrayKey, indexKey string, fields []string) string {
	var b strings.Builder
	b.WriteString("{\"items\": [\n")
	fmt.Fprintf(&b, "{{- $items := index (parseJSON .stages.%s.output) %q -}}\n", fromID, arrayKey)
	b.WriteString("{{- range $i, $it := $items -}}\n{{- if $i }},{{ end }}\n")
	fmt.Fprintf(&b, "{%q: {{ $i }}", indexKey)
	for _, f := range fields {
		fmt.Fprintf(&b, ", %q: {{ toJSON (index $it %q) }}", f, f)
	}
	b.WriteString("}\n{{- end }}\n] }")
	return b.String()
}
