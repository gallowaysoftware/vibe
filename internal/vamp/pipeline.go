package vamp

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Pipeline is the parsed YAML description of a vamp pipeline.
type Pipeline struct {
	Name        string               `yaml:"name"`
	Description string               `yaml:"description,omitempty"`
	Inputs      map[string]InputSpec `yaml:"inputs,omitempty"`
	Stages      []Stage              `yaml:"stages"`
}

// InputSpec declares a pipeline-level input passed on the CLI as --input.
type InputSpec struct {
	Type     string `yaml:"type,omitempty"` // informational; only "string" supported in Phase 1
	Required bool   `yaml:"required,omitempty"`
	Default  string `yaml:"default,omitempty"`
}

// Stage is a single step in the pipeline.
type Stage struct {
	ID           string         `yaml:"id"`
	Type         StageType      `yaml:"type,omitempty"` // "" | "text" | "comfyui"; empty defaults to "text"
	Capability   string         `yaml:"capability"`
	Prompt       string         `yaml:"prompt,omitempty"`
	PromptFile   string         `yaml:"prompt_file,omitempty"`
	Inputs       []string       `yaml:"inputs,omitempty"` // ids of prior stages to depend on
	Output       string         `yaml:"output"`
	OutputFormat string         `yaml:"output_format,omitempty"` // "" | "json"
	Params       map[string]any `yaml:"params,omitempty"`        // merged into the chat-completion body (text stages only)
	// Foreach, when non-nil, makes this a fan-out stage. The upstream stage
	// referenced by From must produce output_format: json and its output must
	// parse as a JSON array (or {"items":[...]} convenience wrap). The stage
	// then runs once per array element, in parallel, sharing the same profile
	// activation. The per-item value is bound under Foreach.Var in the
	// template namespace (defaults to "item").
	Foreach *ForeachSpec `yaml:"foreach,omitempty"`

	// ComfyUI-stage fields. Workflow is a path to a ComfyUI workflow JSON file
	// (relative to the pipeline YAML's directory). Parameters maps
	// "<node_id>.<input_name>" -> template string; each rendered value is
	// type-coerced (int/float/bool/string) and substituted into the workflow's
	// node inputs prior to submission. Capability is still required and maps
	// to the vibe profile that supervises the ComfyUI backend.
	Workflow   string            `yaml:"workflow,omitempty"`
	Parameters map[string]string `yaml:"parameters,omitempty"`
}

// ForeachSpec is the structured fan-out descriptor for a stage. The previous
// Phase 1 form was a free-form template string plus a separate foreach_as
// field; this form ties the iteration directly to a declared upstream stage,
// which keeps Phase 2 (non-LLM stages) honest about its dependencies.
type ForeachSpec struct {
	// From is the id of the upstream stage whose JSON-array output drives the
	// fan-out. Must appear in the consuming stage's Inputs list.
	From string `yaml:"from"`
	// Var is the template variable name bound to each item while rendering
	// Prompt/Output. Defaults to "item" when empty.
	Var string `yaml:"var,omitempty"`
}

var (
	pipelineNameRE = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	stageIDRE      = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	// comfyParamKeyRE enforces the "<node_id>.<input_name>" shape expected for
	// every entry in a comfyui stage's parameters map. ComfyUI workflow nodes
	// are keyed by all-numeric string ids; input names are Go-identifier-like.
	comfyParamKeyRE = regexp.MustCompile(`^[0-9]+\.[A-Za-z_][A-Za-z0-9_]*$`)
)

// LoadPipeline reads, parses, and validates a pipeline YAML file. Unknown
// fields are rejected so typos surface early.
func LoadPipeline(path string) (*Pipeline, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	var p Pipeline
	if err := dec.Decode(&p); err != nil {
		return nil, migrateForeachError(fmt.Errorf("parse %s: %w", path, err))
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("validate %s: %w", path, err)
	}
	return &p, nil
}

// DefaultForeachVar is the template variable name bound to each item of a
// foreach stage's array when ForeachSpec.Var is unset.
const DefaultForeachVar = "item"

// migrateForeachError annotates the YAML decode error with a migration hint
// when the failure is most likely caused by a pipeline still using the old
// Phase 1 foreach syntax (string template + separate foreach_as). YAML's
// strict-field mode rejects the legacy syntax with a generic "cannot unmarshal
// !!str into vamp.ForeachSpec" / "field foreach_as not found" message;
// rewriting that to point at the new form saves users the dig.
func migrateForeachError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(msg, "foreach_as") ||
		strings.Contains(msg, "into vamp.ForeachSpec") ||
		strings.Contains(msg, "cannot unmarshal !!str into") {
		return fmt.Errorf("%w\n\nhint: the foreach syntax changed. Replace:\n  foreach: \"{{.stages.X.output}}\"\n  foreach_as: var\nwith:\n  foreach:\n    from: X\n    var: var", err)
	}
	return err
}

func (p *Pipeline) Validate() error {
	if p.Name == "" {
		return errors.New("name is required")
	}
	if !pipelineNameRE.MatchString(p.Name) {
		return fmt.Errorf("name %q must match [a-zA-Z0-9_-]+", p.Name)
	}
	if len(p.Stages) == 0 {
		return errors.New("pipeline must have at least one stage")
	}
	for name, spec := range p.Inputs {
		if !pipelineNameRE.MatchString(name) {
			return fmt.Errorf("inputs[%s]: name must match [a-zA-Z0-9_-]+", name)
		}
		if spec.Type != "" && spec.Type != "string" {
			return fmt.Errorf("inputs[%s]: type %q is not supported (only \"string\" in Phase 1)", name, spec.Type)
		}
	}
	// First pass: per-stage shape validation + duplicate-id detection. We
	// intentionally do NOT enforce the "input must reference an earlier
	// stage" rule here anymore: the DAG executor only requires the
	// dependency graph to be acyclic, so forward references are fine. A
	// dedicated cycle check below produces a clearer error than the old
	// rule, and lets pipelines declare stages in any order.
	seenStages := make(map[string]bool)
	for i, s := range p.Stages {
		loc := fmt.Sprintf("stages[%d]", i)
		if s.ID == "" {
			return fmt.Errorf("%s: id is required", loc)
		}
		if !stageIDRE.MatchString(s.ID) {
			return fmt.Errorf("%s: id %q must match [a-zA-Z_][a-zA-Z0-9_]* (template-syntax-safe)", loc, s.ID)
		}
		if seenStages[s.ID] {
			return fmt.Errorf("%s: duplicate stage id %q", loc, s.ID)
		}
		seenStages[s.ID] = true
		ctx := fmt.Sprintf("stage %s", s.ID)
		if s.Capability == "" {
			return fmt.Errorf("%s: capability is required", ctx)
		}
		if s.Output == "" {
			return fmt.Errorf("%s: output is required", ctx)
		}
		// Type discrimination: empty defaults to text. Per-type shape rules
		// below reject fields that belong to the other type.
		stageType := s.Type
		if stageType == "" {
			stageType = StageTypeText
		}
		switch stageType {
		case StageTypeText:
			if (s.Prompt == "") == (s.PromptFile == "") {
				return fmt.Errorf("%s: exactly one of prompt or prompt_file is required", ctx)
			}
			if s.Workflow != "" {
				return fmt.Errorf("%s: workflow is only valid on type: comfyui stages", ctx)
			}
			if len(s.Parameters) > 0 {
				return fmt.Errorf("%s: parameters is only valid on type: comfyui stages (text stages use params)", ctx)
			}
		case StageTypeComfyUI:
			if s.Workflow == "" {
				return fmt.Errorf("%s: workflow is required for type: comfyui stages", ctx)
			}
			if len(s.Parameters) == 0 {
				return fmt.Errorf("%s: parameters must have at least one entry for type: comfyui stages", ctx)
			}
			for key := range s.Parameters {
				if !comfyParamKeyRE.MatchString(key) {
					return fmt.Errorf("%s: parameters key %q must match \"<node_id>.<input_name>\" (e.g. \"6.text\")", ctx, key)
				}
			}
			if s.Prompt != "" {
				return fmt.Errorf("%s: prompt is only valid on type: text stages", ctx)
			}
			if s.PromptFile != "" {
				return fmt.Errorf("%s: prompt_file is only valid on type: text stages", ctx)
			}
			if len(s.Params) > 0 {
				return fmt.Errorf("%s: params is only valid on type: text stages (comfyui uses parameters)", ctx)
			}
			if s.OutputFormat != "" {
				return fmt.Errorf("%s: output_format is only valid on type: text stages", ctx)
			}
		default:
			return fmt.Errorf("%s: type %q is not supported (allowed: \"\", text, comfyui)", ctx, s.Type)
		}
		if s.OutputFormat != "" && s.OutputFormat != "json" {
			return fmt.Errorf("%s: output_format %q is not supported (allowed: \"\", json)", ctx, s.OutputFormat)
		}
		if s.Foreach != nil {
			if s.Foreach.From == "" {
				return fmt.Errorf("%s: foreach.from is required", ctx)
			}
			// foreach stages must use a templated output path so per-item runs
			// don't collide on the same file. We treat the presence of "{{" as
			// the templated marker; that's the same syntax users see in YAML.
			if !strings.Contains(s.Output, "{{") {
				return fmt.Errorf("%s: foreach stages require a templated output path (contains {{...}}) so per-item writes don't collide", ctx)
			}
			// Default Var to "item" so the executor can rely on the field
			// being set whenever Foreach is. We mutate the stage in place to
			// keep downstream code simple.
			if s.Foreach.Var == "" {
				p.Stages[i].Foreach.Var = DefaultForeachVar
			}
		}
	}
	// Second pass: every input must reference a declared stage, and no stage
	// may depend on itself. For foreach stages we additionally require the
	// upstream named in foreach.from to be declared as an input and to emit
	// output_format: json.
	for _, s := range p.Stages {
		ctx := fmt.Sprintf("stage %s", s.ID)
		for _, dep := range s.Inputs {
			if dep == s.ID {
				return fmt.Errorf("%s: input %q depends on itself", ctx, dep)
			}
			if !seenStages[dep] {
				return fmt.Errorf("%s: input %q does not reference any declared stage", ctx, dep)
			}
		}
		if s.Foreach != nil {
			from := s.Foreach.From
			if !seenStages[from] {
				return fmt.Errorf("%s: foreach.from %q does not reference any declared stage", ctx, from)
			}
			// Require an explicit declaration in Inputs. Auto-adding would
			// hide a real misconfiguration (e.g. typos in the inputs list,
			// or a user forgetting that the upstream needs to complete before
			// this stage runs); an explicit list keeps the DAG honest and the
			// error easier to diagnose.
			declared := false
			for _, dep := range s.Inputs {
				if dep == from {
					declared = true
					break
				}
			}
			if !declared {
				return fmt.Errorf("%s: foreach.from %q must also appear in inputs", ctx, from)
			}
			// The named upstream must produce JSON; otherwise the rendered
			// foreach source can't be parsed as an array.
			hasJSONSource := false
			for _, other := range p.Stages {
				if other.ID == from && other.OutputFormat == "json" {
					hasJSONSource = true
					break
				}
			}
			if !hasJSONSource {
				return fmt.Errorf("%s: foreach.from %q must reference a stage with output_format: json", ctx, from)
			}
		}
	}
	// Third pass: reject dependency cycles. A cycle is detected if a
	// topological sort cannot consume every stage.
	if cycle := findCycle(p.Stages); cycle != nil {
		return fmt.Errorf("dependency cycle detected: %s", strings.Join(cycle, " -> "))
	}
	return nil
}

// findCycle returns the participating stage ids if the dependency graph
// contains a cycle, in the order they appear in the cycle, with the first id
// repeated at the end (e.g. ["a","b","a"]). Returns nil when the graph is
// acyclic.
func findCycle(stages []Stage) []string {
	const (
		white = 0 // unvisited
		gray  = 1 // on the current DFS stack
		black = 2 // fully explored
	)
	deps := make(map[string][]string, len(stages))
	for _, s := range stages {
		deps[s.ID] = s.Inputs
	}
	color := make(map[string]int, len(stages))
	var stack []string
	var dfs func(id string) []string
	dfs = func(id string) []string {
		color[id] = gray
		stack = append(stack, id)
		for _, dep := range deps[id] {
			switch color[dep] {
			case gray:
				// Found a back-edge to `dep`; build cycle slice from the
				// first occurrence on the stack.
				for i, n := range stack {
					if n == dep {
						out := append([]string{}, stack[i:]...)
						return append(out, dep)
					}
				}
				return []string{dep, dep}
			case white:
				if c := dfs(dep); c != nil {
					return c
				}
			}
		}
		color[id] = black
		stack = stack[:len(stack)-1]
		return nil
	}
	for _, s := range stages {
		if color[s.ID] == white {
			if c := dfs(s.ID); c != nil {
				return c
			}
		}
	}
	return nil
}
