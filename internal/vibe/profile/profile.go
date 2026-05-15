// Package profile defines the vibe profile YAML schema and its loader.
package profile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Profile struct {
	Name            string   `yaml:"name"`
	Description     string   `yaml:"description,omitempty"`
	Model           Model    `yaml:"model"`
	Frontend        Frontend `yaml:"frontend"`
	EstimatedVRAMGB float64  `yaml:"estimated_vram_gb,omitempty"`
}

type Model struct {
	Path        string       `yaml:"path"`
	Huggingface *Huggingface `yaml:"huggingface,omitempty"`
	Alias       string       `yaml:"alias"`
	Context     int          `yaml:"context"`
	Parallel    int          `yaml:"parallel,omitempty"`
	GPULayers   int          `yaml:"gpu_layers,omitempty"`
	FlashAttn   bool         `yaml:"flash_attn,omitempty"`
	CacheTypeK  string       `yaml:"cache_type_k,omitempty"`
	CacheTypeV  string       `yaml:"cache_type_v,omitempty"`
	Jinja       bool         `yaml:"jinja,omitempty"`
	ExtraArgs   []string     `yaml:"extra_args,omitempty"`
}

// Huggingface points at a model file on huggingface.co. When set, vibe
// downloads the file to Model.Path on demand (via `vibe pull` or implicitly
// at the start of `vibe start`).
type Huggingface struct {
	Repo     string `yaml:"repo"`
	File     string `yaml:"file"`
	Revision string `yaml:"revision,omitempty"` // default "main"
}

const (
	FrontendExternal      = "external"
	FrontendDockerCompose = "docker-compose"
	FrontendManaged       = "managed"
)

type Frontend struct {
	Kind            string            `yaml:"kind"`
	App             string            `yaml:"app"`
	RestartRequired bool              `yaml:"restart_required,omitempty"`
	WriteFile       string            `yaml:"write_file,omitempty"`
	Template        map[string]any    `yaml:"template,omitempty"`
	Env             map[string]string `yaml:"env,omitempty"`
	MCPs            []string          `yaml:"mcps,omitempty"`

	// docker-compose kind fields.
	ComposeFile string       `yaml:"compose_file,omitempty"`
	ProjectName string       `yaml:"project_name,omitempty"`
	Services    []string     `yaml:"services,omitempty"`
	WaitFor     []WaitForURL `yaml:"wait_for,omitempty"`
}

// WaitForURL describes a health-check endpoint the docker-compose driver
// polls after `docker compose up -d`.
type WaitForURL struct {
	URL     string        `yaml:"url"`
	Timeout time.Duration `yaml:"-"`
}

// waitForURLYAML is the on-disk shape; Timeout arrives as a duration string
// (e.g. "60s") which we parse in UnmarshalYAML.
type waitForURLYAML struct {
	URL     string `yaml:"url"`
	Timeout string `yaml:"timeout"`
}

// UnmarshalYAML parses the wait_for entry, including its duration-shaped
// Timeout field.
func (w *WaitForURL) UnmarshalYAML(value *yaml.Node) error {
	var raw waitForURLYAML
	raw.URL = ""
	if err := value.Decode(&raw); err != nil {
		return err
	}
	w.URL = raw.URL
	if raw.Timeout == "" {
		w.Timeout = 0
		return nil
	}
	d, err := time.ParseDuration(raw.Timeout)
	if err != nil {
		return fmt.Errorf("wait_for.timeout %q: %w", raw.Timeout, err)
	}
	w.Timeout = d
	return nil
}

// Load reads, parses, and validates a profile YAML file. Unknown fields are
// rejected; ~/-prefixed paths are expanded relative to the user's home.
func Load(path string) (*Profile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open profile %s: %w", path, err)
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)

	var p Profile
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("parse profile %s: %w", path, err)
	}

	if p.Model.Parallel == 0 {
		p.Model.Parallel = 1
	}
	p.Model.Path = expandTilde(p.Model.Path)
	p.Frontend.WriteFile = expandTilde(p.Frontend.WriteFile)

	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("validate profile %s: %w", path, err)
	}
	return &p, nil
}

var nameRE = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func (p *Profile) Validate() error {
	if p.Name == "" {
		return errors.New("name is required")
	}
	if !nameRE.MatchString(p.Name) {
		return fmt.Errorf("name %q must match [a-zA-Z0-9_-]+", p.Name)
	}

	if p.Model.Path == "" {
		return errors.New("model.path is required")
	}
	if p.Model.Huggingface != nil {
		if p.Model.Huggingface.Repo == "" {
			return errors.New("model.huggingface.repo is required when huggingface is set")
		}
		if p.Model.Huggingface.File == "" {
			return errors.New("model.huggingface.file is required when huggingface is set")
		}
		// model.path doesn't need to exist; `vibe pull` will create it.
	} else if _, err := os.Stat(p.Model.Path); err != nil {
		return fmt.Errorf("model.path %s: %w", p.Model.Path, err)
	}
	if p.Model.Alias == "" {
		return errors.New("model.alias is required (must match /v1/models id)")
	}
	if p.Model.Context <= 0 {
		return errors.New("model.context must be > 0")
	}

	switch p.Frontend.Kind {
	case FrontendExternal:
		if p.Frontend.WriteFile == "" {
			return errors.New("frontend.write_file is required for kind=external")
		}
		if len(p.Frontend.Template) == 0 {
			return errors.New("frontend.template is required for kind=external")
		}
		if p.Frontend.ComposeFile != "" {
			return errors.New("frontend.compose_file is only valid for kind=docker-compose")
		}
		if p.Frontend.ProjectName != "" {
			return errors.New("frontend.project_name is only valid for kind=docker-compose")
		}
		if len(p.Frontend.Services) > 0 {
			return errors.New("frontend.services is only valid for kind=docker-compose")
		}
		if len(p.Frontend.WaitFor) > 0 {
			return errors.New("frontend.wait_for is only valid for kind=docker-compose")
		}
	case FrontendDockerCompose:
		if p.Frontend.ComposeFile == "" {
			return errors.New("frontend.compose_file is required for kind=docker-compose")
		}
		if p.Frontend.WriteFile != "" {
			return errors.New("frontend.write_file is only valid for kind=external")
		}
		if len(p.Frontend.Template) > 0 {
			return errors.New("frontend.template is only valid for kind=external")
		}
		if len(p.Frontend.MCPs) > 0 {
			return errors.New("frontend.mcps is only valid for kind=external")
		}
		seenSvc := make(map[string]struct{}, len(p.Frontend.Services))
		for _, s := range p.Frontend.Services {
			if s == "" {
				return errors.New("frontend.services contains empty entry")
			}
			if _, dup := seenSvc[s]; dup {
				return fmt.Errorf("frontend.services contains duplicate %q", s)
			}
			seenSvc[s] = struct{}{}
		}
		for i, w := range p.Frontend.WaitFor {
			if w.URL == "" {
				return fmt.Errorf("frontend.wait_for[%d].url is required", i)
			}
		}
	case FrontendManaged:
		return fmt.Errorf("frontend.kind %q not supported yet (Phase 1: external + docker-compose)", p.Frontend.Kind)
	case "":
		return errors.New("frontend.kind is required")
	default:
		return fmt.Errorf("frontend.kind %q is unknown (expected: external, docker-compose, managed)", p.Frontend.Kind)
	}

	if len(p.Frontend.MCPs) > 0 {
		if p.Frontend.Kind != FrontendExternal {
			return fmt.Errorf("frontend.mcps requires frontend.kind=external (got %q)", p.Frontend.Kind)
		}
		seen := make(map[string]struct{}, len(p.Frontend.MCPs))
		for _, name := range p.Frontend.MCPs {
			if _, dup := seen[name]; dup {
				return fmt.Errorf("frontend.mcps contains duplicate %q", name)
			}
			seen[name] = struct{}{}
		}
	}

	return nil
}

func expandTilde(p string) string {
	if p == "" || !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[2:])
}
