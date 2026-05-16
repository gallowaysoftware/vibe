package vamp

import (
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Capabilities maps capability names (e.g. "reasoning") to a CapabilityBinding
// describing which vibe profile(s) can satisfy that capability. Loaded from
// $XDG_CONFIG_HOME/vamp/capabilities.yaml.
//
// Two YAML forms are accepted per binding (see CapabilityBinding):
//
//	# shorthand: single profile
//	capabilities:
//	  reasoning: code
//
//	# long form: ordered candidate list, biggest first
//	capabilities:
//	  reasoning:
//	    candidates: [code, code_small]
//
// Both forms round-trip through Profiles() as an ordered list of profile
// names; the shorthand always yields a single-element list.
type Capabilities struct {
	Mapping map[string]CapabilityBinding `yaml:"capabilities"`
}

// CapabilityBinding is one capability's profile binding. Exactly one of
// Profile and Candidates is populated after unmarshal: Profile when the YAML
// used the string shorthand, Candidates when the YAML used the long-form
// object with a `candidates:` list.
//
// Callers should prefer Profiles() on the parent Capabilities; this type's
// fields are exported only so existing code (and tests) can read them
// directly when needed.
type CapabilityBinding struct {
	// Profile is set when the YAML used the string shorthand
	// (`reasoning: code`). Empty when the long-form was used.
	Profile string
	// Candidates is set when the YAML used the long form
	// (`reasoning: { candidates: [a, b] }`). Empty when the shorthand was
	// used. Ordered biggest-first; the executor walks the list in order
	// and skips a candidate when the daemon rejects it for a VRAM reason.
	Candidates []string
}

// candidateList is the YAML schema for the long-form binding object. Kept
// unexported so the only public surface is CapabilityBinding plus its
// UnmarshalYAML.
type candidateList struct {
	Candidates []string `yaml:"candidates"`
}

// UnmarshalYAML accepts either a scalar (shorthand: `reasoning: code`) or a
// mapping with a `candidates:` list (long form). Anything else is a clear
// parse error so misshapen YAML doesn't silently degrade to an empty
// binding.
func (b *CapabilityBinding) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		var s string
		if err := value.Decode(&s); err != nil {
			return fmt.Errorf("capability binding: decode scalar: %w", err)
		}
		if s == "" {
			return errors.New("capability binding: empty profile name")
		}
		b.Profile = s
		return nil
	case yaml.MappingNode:
		var cl candidateList
		if err := value.Decode(&cl); err != nil {
			return fmt.Errorf("capability binding: decode mapping: %w", err)
		}
		if len(cl.Candidates) == 0 {
			return errors.New("capability binding: candidates list is empty")
		}
		// Defensive copy so callers can't mutate the cached slice via the
		// yaml.Node-owned backing array.
		b.Candidates = append([]string(nil), cl.Candidates...)
		return nil
	default:
		return fmt.Errorf("capability binding: expected scalar or mapping, got %v", value.Kind)
	}
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
		c.Mapping = make(map[string]CapabilityBinding)
	}
	return &c, nil
}

// Profile returns the first vibe profile bound to the given capability (so
// existing callers that don't care about candidate fallback keep working
// unchanged), or an error listing the available capabilities if there's no
// mapping. For the long-form binding this is the first entry in Candidates.
func (c *Capabilities) Profile(capability string) (string, error) {
	ps, err := c.Profiles(capability)
	if err != nil {
		return "", err
	}
	return ps[0], nil
}

// Profiles returns the ordered list of vibe profiles bound to the given
// capability. Shorthand bindings (string YAML) yield a single-element list;
// long-form bindings yield Candidates verbatim. The list is always non-empty
// on success — an empty binding is rejected at unmarshal time.
func (c *Capabilities) Profiles(capability string) ([]string, error) {
	b, ok := c.Mapping[capability]
	if !ok {
		have := make([]string, 0, len(c.Mapping))
		for k := range c.Mapping {
			have = append(have, k)
		}
		return nil, fmt.Errorf("capability %q is not mapped in %s (have: %v)", capability, CapabilitiesFile(), have)
	}
	if len(b.Candidates) > 0 {
		// Defensive copy: callers shouldn't be able to mutate the cached
		// binding by writing through the returned slice.
		out := make([]string, len(b.Candidates))
		copy(out, b.Candidates)
		return out, nil
	}
	if b.Profile != "" {
		return []string{b.Profile}, nil
	}
	// This shouldn't happen — UnmarshalYAML guarantees one of the two
	// fields is populated — but handle it explicitly so a programmatically
	// constructed empty binding produces a clear error instead of a panic.
	return nil, fmt.Errorf("capability %q has no profile or candidates", capability)
}
