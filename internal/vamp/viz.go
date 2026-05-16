package vamp

import (
	"fmt"
	"sort"
	"strings"
)

// VizOptions controls Mermaid rendering of a pipeline DAG.
//
// ShowInputs, when true, prepends a `subgraph inputs` block listing each
// declared pipeline input so the rendered diagram is self-contained
// documentation rather than just the stage graph.
type VizOptions struct {
	ShowInputs bool
}

// stageTypeStyle is the visual classification applied to a stage node in
// the rendered Mermaid output. The mapping intentionally mirrors the
// allowed StageType values plus the "text" default; the colour palette
// is hand-picked for legibility on a white markdown background and for
// distinctness on a Mermaid live-editor dark background.
type stageTypeStyle struct {
	class string // mermaid classDef name (also the type)
	fill  string // hex fill colour
}

var stageStyleByType = map[StageType]stageTypeStyle{
	StageTypeText:    {class: "text", fill: "#cfe2ff"},
	StageTypeComfyUI: {class: "comfyui", fill: "#e0cffc"},
	StageTypeAudio:   {class: "audio", fill: "#ffe5b4"},
	StageTypeFFmpeg:  {class: "ffmpeg", fill: "#cfe9cf"},
	StageTypeYouTube: {class: "youtube", fill: "#f5c2c7"},
	StageTypeWebhook: {class: "webhook", fill: "#dcdcdc"},
	StageTypeConfirm: {class: "confirm", fill: "#fff3cd"},
}

// stageStyleOrder is the canonical class-definition emission order so the
// rendered Mermaid is deterministic across runs. We emit class definitions
// only for the types actually present in the pipeline (keeps output tight),
// but the order in which we emit them never depends on Go map iteration.
var stageStyleOrder = []StageType{
	StageTypeText,
	StageTypeComfyUI,
	StageTypeAudio,
	StageTypeFFmpeg,
	StageTypeYouTube,
	StageTypeWebhook,
	StageTypeConfirm,
}

// RenderMermaid emits a `flowchart TD` description of the pipeline's stage
// DAG. The renderer is pure (no I/O) so it's trivial to golden-test against
// the shipped example pipelines, and stable in output ordering: nodes are
// emitted in pipeline declaration order, edges follow each stage's declared
// inputs slice, and class definitions are emitted only for the types
// actually present in the pipeline in stageStyleOrder.
//
// Foreach stages get a dotted "foreach" edge from their `foreach.from`
// upstream in addition to the regular solid input edges. Rendering both
// keeps the dependency DAG honest (the executor still treats it as a
// normal dependency) while making the fan-out visually distinct.
func RenderMermaid(p *Pipeline, opts VizOptions) string {
	var b strings.Builder
	b.WriteString("flowchart TD\n")

	if opts.ShowInputs && len(p.Inputs) > 0 {
		// Sort input names for deterministic output: Go map iteration order
		// is randomised, but rendered Mermaid that flips line order on each
		// invocation makes diffs miserable.
		names := make([]string, 0, len(p.Inputs))
		for name := range p.Inputs {
			names = append(names, name)
		}
		sort.Strings(names)
		b.WriteString("  subgraph inputs\n")
		for _, name := range names {
			spec := p.Inputs[name]
			label := name
			if spec.Required {
				label += " (required)"
			}
			if spec.Default != "" {
				label += fmt.Sprintf(" [default: %s]", spec.Default)
			}
			fmt.Fprintf(&b, "    input_%s[%s]\n", name, escapeLabel(label))
		}
		b.WriteString("  end\n")
	}

	// Per-type bucket of stage ids actually present in this pipeline; used
	// at the end of the rendered output to build `class id1,id2 typeName`
	// directives. We collect this as we walk stages so we only iterate the
	// pipeline once.
	classMembers := make(map[StageType][]string, len(stageStyleByType))

	for _, s := range p.Stages {
		st := stageTypeOrDefault(&s)
		classMembers[st] = append(classMembers[st], s.ID)
		fmt.Fprintf(&b, "  %s[%s]\n", s.ID, escapeLabel(stageLabel(s)))
	}

	// Edges. We emit foreach edges first (so they sort before the regular
	// solid edges that originate from the same upstream), then ordinary
	// input edges. Foreach edges are dotted to distinguish them visually;
	// the executor still treats the foreach.from as a regular dependency
	// (it's required to also be listed in Inputs), so the solid edge will
	// appear in addition to the dotted one — that's intentional.
	for _, s := range p.Stages {
		if s.Foreach != nil && s.Foreach.From != "" {
			fmt.Fprintf(&b, "  %s -.foreach.-> %s\n", s.Foreach.From, s.ID)
		}
	}
	for _, s := range p.Stages {
		for _, dep := range s.Inputs {
			fmt.Fprintf(&b, "  %s --> %s\n", dep, s.ID)
		}
	}

	// Class definitions for only the types actually present, in canonical
	// order. We also emit a `class <ids...> <type>` line per type so users
	// can recolour by editing the classDef block in their copy of the
	// diagram without touching the per-node lines.
	for _, st := range stageStyleOrder {
		members, ok := classMembers[st]
		if !ok || len(members) == 0 {
			continue
		}
		style := stageStyleByType[st]
		fmt.Fprintf(&b, "  classDef %s fill:%s\n", style.class, style.fill)
	}
	for _, st := range stageStyleOrder {
		members, ok := classMembers[st]
		if !ok || len(members) == 0 {
			continue
		}
		style := stageStyleByType[st]
		fmt.Fprintf(&b, "  class %s %s\n", strings.Join(members, ","), style.class)
	}

	return b.String()
}

// stageLabel builds the human-readable label inside a stage node. The
// minimum content is the stage id; we additionally surface capability,
// output_format, and foreach metadata so the diagram is useful without
// having to flip back to the YAML. Type is omitted from the label when
// it's the implicit default (text) — the class-based colouring already
// telegraphs the type, and the rendered label stays terse.
func stageLabel(s Stage) string {
	parts := []string{s.ID}
	st := stageTypeOrDefault(&s)
	if s.Capability != "" {
		parts = append(parts, s.Capability)
	} else {
		// For stage types that don't take a capability (ffmpeg, youtube,
		// webhook, audio) we still surface the type itself so the label
		// isn't reduced to just the id.
		parts = append(parts, string(st))
	}
	if s.OutputFormat != "" {
		parts = append(parts, "output_format="+s.OutputFormat)
	}
	if s.Foreach != nil && s.Foreach.From != "" {
		parts = append(parts, "foreach: "+s.Foreach.From)
	}
	return strings.Join(parts, " · ")
}

// escapeLabel quotes a Mermaid node label when it contains characters
// that would otherwise terminate the `[...]` shape early or produce a
// parser error. Mermaid's rule of thumb: wrap in double quotes when the
// label contains any of `[]"()` or whitespace-sensitive characters that
// the renderer might otherwise treat as syntax. We over-quote a little
// here (any label with a colon, pipe, or quote) because the cost is one
// extra pair of quotes and the benefit is never having to track down a
// "parse error" rendered by Mermaid live editor against output we
// produced.
func escapeLabel(s string) string {
	if !strings.ContainsAny(s, ":|\"()[]") {
		return s
	}
	// Mermaid uses backslash to escape double quotes inside a quoted label.
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}
