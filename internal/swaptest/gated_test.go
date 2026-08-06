package swaptest_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/swaptest"
	"github.com/gallowaysoftware/vibe/internal/vibe/fleetapi"
)

// TestGatedSwapVersionsMatchesRecordings keeps doctor's claim about which
// llama-swap versions this build is gated on tied to the recordings that
// make the claim true (fleet-control C16).
//
// fleetapi.GatedSwapVersions is a hand-written list because production has
// no business importing a test double. This is the test package that
// imports both, so the drift is caught here rather than by an operator
// reading a check that names a version nothing replays.
func TestGatedSwapVersionsMatchesRecordings(t *testing.T) {
	dirs, err := swaptest.FixtureDirs()
	if err != nil {
		t.Fatalf("FixtureDirs: %v", err)
	}
	slices.Sort(dirs)
	gated := fleetapi.GatedSwapVersions()
	slices.Sort(gated)
	if !slices.Equal(dirs, gated) {
		t.Fatalf("fleetapi.GatedSwapVersions() = %v but internal/swaptest/fixtures holds %v.\n"+
			"doctor's versions.llama_swap check tells an operator which versions this build has replayed; "+
			"a list that names a version with no recording (or omits one that has) makes that sentence false.", gated, dirs)
	}
}

// TestConformanceMatrixCoversEveryRecording ties the third copy of the
// same fact — ci.yml's matrix — to the other two. A recording that no CI
// job ever replays is a fixture, not a gate.
func TestConformanceMatrixCoversEveryRecording(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	m := regexp.MustCompile(`(?m)^\s*llama-swap: \[(.*)\]\s*$`).FindSubmatch(b)
	if m == nil {
		t.Fatal("ci.yml has no `llama-swap: [...]` conformance matrix; the recordings are replayed by nothing")
	}
	var matrix []string
	for _, v := range strings.Split(string(m[1]), ",") {
		matrix = append(matrix, strings.TrimSpace(v))
	}
	slices.Sort(matrix)
	dirs, err := swaptest.FixtureDirs()
	if err != nil {
		t.Fatalf("FixtureDirs: %v", err)
	}
	slices.Sort(dirs)
	if !slices.Equal(matrix, dirs) {
		t.Fatalf("ci.yml's conformance matrix is %v but the recordings are %v.\n"+
			"Bumping a pin is the documented trigger to re-record (see TestRecord); the two must be added together.", matrix, dirs)
	}
}
