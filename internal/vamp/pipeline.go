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
	Capability   string         `yaml:"capability"`
	Prompt       string         `yaml:"prompt,omitempty"`
	PromptFile   string         `yaml:"prompt_file,omitempty"`
	Inputs       []string       `yaml:"inputs,omitempty"` // ids of prior stages to depend on
	Output       string         `yaml:"output"`
	OutputFormat string         `yaml:"output_format,omitempty"` // "" | "json"
	Params       map[string]any `yaml:"params,omitempty"`        // merged into the chat-completion body
}

var (
	pipelineNameRE = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	stageIDRE      = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
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
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("validate %s: %w", path, err)
	}
	return &p, nil
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
		if (s.Prompt == "") == (s.PromptFile == "") {
			return fmt.Errorf("%s: exactly one of prompt or prompt_file is required", ctx)
		}
		if s.OutputFormat != "" && s.OutputFormat != "json" {
			return fmt.Errorf("%s: output_format %q is not supported (allowed: \"\", json)", ctx, s.OutputFormat)
		}
	}
	// Second pass: every input must reference a declared stage, and no stage
	// may depend on itself.
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
