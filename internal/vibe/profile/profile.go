// Package profile defines the vibe profile YAML schema and its loader.
package profile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

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
	Path       string   `yaml:"path"`
	Alias      string   `yaml:"alias"`
	Context    int      `yaml:"context"`
	Parallel   int      `yaml:"parallel,omitempty"`
	GPULayers  int      `yaml:"gpu_layers,omitempty"`
	FlashAttn  bool     `yaml:"flash_attn,omitempty"`
	CacheTypeK string   `yaml:"cache_type_k,omitempty"`
	CacheTypeV string   `yaml:"cache_type_v,omitempty"`
	Jinja      bool     `yaml:"jinja,omitempty"`
	ExtraArgs  []string `yaml:"extra_args,omitempty"`
}

const (
	FrontendExternal      = "external"
	FrontendDockerCompose = "docker-compose"
	FrontendManaged       = "managed"
)

type Frontend struct {
	Kind            string         `yaml:"kind"`
	App             string         `yaml:"app"`
	RestartRequired bool           `yaml:"restart_required,omitempty"`
	WriteFile       string         `yaml:"write_file,omitempty"`
	Template        map[string]any `yaml:"template,omitempty"`
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
	if _, err := os.Stat(p.Model.Path); err != nil {
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
	case FrontendDockerCompose, FrontendManaged:
		return fmt.Errorf("frontend.kind %q not supported yet (Phase 1: external only)", p.Frontend.Kind)
	case "":
		return errors.New("frontend.kind is required")
	default:
		return fmt.Errorf("frontend.kind %q is unknown (expected: external, docker-compose, managed)", p.Frontend.Kind)
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
