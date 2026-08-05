package swaptest

import (
	"embed"
	"encoding/json"
	"fmt"
	"path"
	"strings"
)

// Fixtures are embedded rather than read from testdata/ because consumers
// live in other packages, where the working directory is theirs and not
// this one. `fixtures` (not `testdata`) so the go tool treats them as
// ordinary package data.
//
//go:embed fixtures
var fixtures embed.FS

// Recording is the provenance stamp beside each fixture set. Whoever bumps
// the pinned llama-swap version re-runs TestRecord and commits the new
// directory BESIDE the old one — a heterogeneous fleet is the normal state,
// so "both wires must pass" is the requirement, not "newest wins".
type Recording struct {
	Version      string `json:"version"`
	Commit       string `json:"commit"`
	At           string `json:"at"`
	Source       string `json:"source"`
	CapturedFrom string `json:"captured_from"`
	Caveat       string `json:"caveat,omitempty"`
	LogDataBytes []int  `json:"logdata_bytes,omitempty"`
}

// Synthesized reports whether this recording came from upstream source
// rather than from a running binary. A synthesized fixture is evidence
// about what upstream INTENDS, not about what it does.
func (r Recording) Synthesized() bool {
	return strings.Contains(strings.ToLower(r.CapturedFrom), "not captured")
}

// FixtureDir maps a Wire to its recorded directory name.
func (w Wire) FixtureDir() string { return w.String() }

// LoadRecording reads a wire's provenance stamp.
func LoadRecording(w Wire) (Recording, error) {
	b, err := fixtures.ReadFile(path.Join("fixtures", w.FixtureDir(), "RECORDED"))
	if err != nil {
		return Recording{}, err
	}
	var r Recording
	if err := json.Unmarshal(b, &r); err != nil {
		return Recording{}, fmt.Errorf("decode RECORDED for %s: %w", w, err)
	}
	return r, nil
}

// Fixture reads one recorded file verbatim.
func Fixture(w Wire, name string) ([]byte, error) {
	return fixtures.ReadFile(path.Join("fixtures", w.FixtureDir(), name))
}

// RecordedFrame is one parsed SSE frame from a recording: the envelope type
// plus the inner JSON text, which is a STRING in the envelope and JSON
// again inside it.
type RecordedFrame struct {
	Type  string
	Inner string
	Raw   string
}

// RecordedFrames parses a recorded .sse capture into frames.
func RecordedFrames(w Wire, name string) ([]RecordedFrame, error) {
	b, err := Fixture(w, name)
	if err != nil {
		return nil, err
	}
	var out []RecordedFrame
	for _, blk := range strings.Split(string(b), "\n\n") {
		if strings.TrimSpace(blk) == "" {
			continue
		}
		var data []string
		for _, line := range strings.Split(blk, "\n") {
			if rest, ok := strings.CutPrefix(line, "data:"); ok {
				data = append(data, strings.TrimPrefix(rest, " "))
			}
		}
		if len(data) == 0 {
			continue
		}
		var env envelope
		if err := json.Unmarshal([]byte(strings.Join(data, "\n")), &env); err != nil {
			return nil, fmt.Errorf("%s/%s: decode envelope: %w", w, name, err)
		}
		out = append(out, RecordedFrame{Type: env.Type, Inner: env.Data, Raw: blk + "\n\n"})
	}
	return out, nil
}

// Wires is every wire version this package can speak.
func Wires() []Wire { return []Wire{WireV239, WireV247} }

// FixtureDirs lists every recorded directory on disk, whether or not a Wire
// claims it. Wires() is hand-written; this reads the tree, and the gap
// between the two is what TestEveryFixtureDirIsReachable exists to close.
func FixtureDirs() ([]string, error) {
	ents, err := fixtures.ReadDir("fixtures")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}
