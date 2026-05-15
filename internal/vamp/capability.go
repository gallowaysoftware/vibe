package vamp

import (
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Capabilities maps capability names (e.g. "reasoning") to vibe profile names
// (e.g. "code"). Loaded from $XDG_CONFIG_HOME/vamp/capabilities.yaml.
type Capabilities struct {
	Mapping map[string]string `yaml:"capabilities"`
}

// LoadCapabilities reads the capabilities file. Missing file is a clear,
// actionable error pointing at the path to create.
func LoadCapabilities() (*Capabilities, error) {
	path := CapabilitiesFile()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("capabilities file not found at %s; create one with `capabilities: { reasoning: <profile>, ... }`", path)
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var c Capabilities
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.Mapping == nil {
		c.Mapping = make(map[string]string)
	}
	return &c, nil
}

// Profile returns the vibe profile bound to the given capability, or an error
// listing the available capabilities if there's no mapping.
func (c *Capabilities) Profile(capability string) (string, error) {
	if p, ok := c.Mapping[capability]; ok {
		return p, nil
	}
	have := make([]string, 0, len(c.Mapping))
	for k := range c.Mapping {
		have = append(have, k)
	}
	return "", fmt.Errorf("capability %q is not mapped in %s (have: %v)", capability, CapabilitiesFile(), have)
}
