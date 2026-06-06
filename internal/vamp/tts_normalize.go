package vamp

import (
	_ "embed"
	"fmt"
	"os"
	"regexp"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// TTS engines (Kokoro, Chatterbox) read text as-typed: they treat
// "-" as "minus", spell out "di/d0" letter-by-letter, and mangle
// any token that isn't a normal English word. The cure is to
// preprocess the script before audio submission, rewriting
// problematic patterns into their spoken-English form.
//
// The ttsNormalize template function applies a YAML-driven rules
// table (defaults + optional per-pipeline overrides) to a string
// and returns the rewritten version. Pipelines call it from their
// Audio stage's TextTemplate so the rewrite happens once, per
// segment, immediately before the TTS request.

// ttsRule is one entry in the rules YAML.
type ttsRule struct {
	// Pattern is a literal string match (case-sensitive). When set,
	// Regex MUST be empty. Cheaper than the regex path for the
	// common case ("LAA" → "L-A-A").
	Pattern string `yaml:"pattern,omitempty"`
	// Regex is a Go-flavor regex. When set, Pattern MUST be empty.
	// Capture group references in Replacement use $1, $2, ... per
	// regexp.Regexp.ReplaceAllString semantics.
	Regex string `yaml:"regex,omitempty"`
	// Replacement is the spoken-form text. Single space-separated
	// words generally do best — Kokoro's prosody is happiest with
	// natural English cadence.
	Replacement string `yaml:"replacement"`
	// Note is a free-text comment explaining why this rule exists
	// (heard mispronunciation, exam jargon, etc.). Carried through
	// YAML round-trip; unused at runtime.
	Note string `yaml:"note,omitempty"`

	// compiled is the regexp instance for Regex rules; populated by
	// compileRules before the rule set is published so applyTTSRules
	// (which runs concurrently across foreach goroutines) only reads it.
	compiled *regexp.Regexp
}

// compileRules compiles every Regex rule in place. A rule with a bad regex
// gets a never-matching pattern so one malformed entry can't kill the audio
// stage. Called once per rule set before it's shared with concurrent readers.
func compileRules(rules []ttsRule) {
	for i := range rules {
		r := &rules[i]
		if r.Regex == "" {
			continue
		}
		re, err := regexp.Compile(r.Regex)
		if err != nil {
			re = regexp.MustCompile(`$.^`) // never matches
		}
		r.compiled = re
	}
}

// ttsRules wraps a slice of rules with the YAML wrapper struct so
// the file shape matches the conventional "rules: [...]" key.
type ttsRulesFile struct {
	Rules []ttsRule `yaml:"rules"`
}

//go:embed default_tts_rules.yaml
var defaultTTSRulesYAML []byte

// rulesCache memoises parsed rule files by their absolute path +
// modtime. ttsNormalize is template-hot — once per audio segment —
// so re-parsing the YAML each invocation would dominate any TTS
// latency on long modules.
type rulesCacheEntry struct {
	mtime time.Time
	rules []ttsRule
}

var (
	rulesCacheMu sync.Mutex
	rulesCache   = map[string]rulesCacheEntry{}
	defaultRules []ttsRule
	defaultOnce  sync.Once
)

// loadDefaults parses the embedded default rules YAML. Called once
// per process via defaultOnce. A parse error is fatal — the embedded
// asset is shipped with the binary, so a parse failure means the
// binary itself is malformed.
func loadDefaults() {
	var f ttsRulesFile
	if err := yaml.Unmarshal(defaultTTSRulesYAML, &f); err != nil {
		panic(fmt.Sprintf("default_tts_rules.yaml is malformed: %v", err))
	}
	compileRules(f.Rules)
	defaultRules = f.Rules
}

// loadRulesFile reads the rules YAML at path, caching the result
// keyed on (path, mtime). Returns empty (no error) when path is "".
func loadRulesFile(path string) ([]ttsRule, error) {
	if path == "" {
		return nil, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	rulesCacheMu.Lock()
	if entry, ok := rulesCache[path]; ok && entry.mtime.Equal(info.ModTime()) {
		rulesCacheMu.Unlock()
		return entry.rules, nil
	}
	rulesCacheMu.Unlock()

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var f ttsRulesFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	compileRules(f.Rules)
	rulesCacheMu.Lock()
	rulesCache[path] = rulesCacheEntry{mtime: info.ModTime(), rules: f.Rules}
	rulesCacheMu.Unlock()
	return f.Rules, nil
}

// applyTTSRules applies the supplied rules in order. Read-only: regex rules
// are pre-compiled by compileRules before the set is published, so this is
// safe to call from the parallel foreach goroutines that share a rule set.
func applyTTSRules(text string, rules []ttsRule) string {
	for i := range rules {
		r := &rules[i]
		switch {
		case r.Pattern != "":
			// Literal replace — Go's strings.ReplaceAll is the
			// natural fit but cheaper to inline via Replacer for
			// the multi-rule case. Single-rule case here.
			text = literalReplaceAll(text, r.Pattern, r.Replacement)
		case r.compiled != nil:
			text = r.compiled.ReplaceAllString(text, r.Replacement)
		}
	}
	return text
}

// literalReplaceAll is strings.ReplaceAll but written here so the
// import surface stays tight (only stdlib + yaml).
func literalReplaceAll(s, from, to string) string {
	if from == "" {
		return s
	}
	var b []byte
	start := 0
	for {
		i := indexAt(s, from, start)
		if i < 0 {
			break
		}
		b = append(b, s[start:i]...)
		b = append(b, to...)
		start = i + len(from)
	}
	if start == 0 {
		return s
	}
	b = append(b, s[start:]...)
	return string(b)
}

// indexAt returns the index of sub in s starting at from, or -1.
func indexAt(s, sub string, from int) int {
	if from >= len(s) {
		return -1
	}
	rest := s[from:]
	for i := 0; i+len(sub) <= len(rest); i++ {
		if rest[i:i+len(sub)] == sub {
			return from + i
		}
	}
	return -1
}

// ttsNormalizeTemplate is the template-function entry point. Wires
// the default rules + (optionally) a rules file path into the
// engine.
//
// Template signature:
//
//	{{ ttsNormalize .segment.text "" }}            # defaults only
//	{{ ttsNormalize .segment.text "tts_rules.yaml" }}  # defaults + overrides
//
// Errors at the function level produce an empty string + log a
// warning — the audio stage continues with the original text. This
// keeps a bad rules-file from killing a run mid-flight.
func ttsNormalizeTemplate(text, rulesPath string) (string, error) {
	defaultOnce.Do(loadDefaults)
	out := applyTTSRules(text, defaultRules)
	if rulesPath != "" {
		extra, err := loadRulesFile(rulesPath)
		if err != nil {
			return "", fmt.Errorf("ttsNormalize: %w", err)
		}
		out = applyTTSRules(out, extra)
	}
	return out, nil
}
