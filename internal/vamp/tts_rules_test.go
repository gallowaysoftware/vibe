package vamp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
	"time"
)

// ── the shipped defaults, as a golden table ──────────────────────────

// TestTTSNormalize_DefaultRulesGolden is the instrument the review
// asked for: the default rules are DATA, so a mutation entry is the
// wrong tool and a golden table over real sentences is the right one.
//
// The rows carrying a "was ..." comment are the ones that used to come
// out WORSE than the input. `pattern:` is strings.ReplaceAll — no
// boundary of any kind — so `g/L` fired inside `µg/L` and `m/s` fired
// inside `km/s`, and the fraction rule half-converted every date it
// met while its own note claimed it skipped them.
func TestTTSNormalize_DefaultRulesGolden(t *testing.T) {
	cases := []struct{ in, want string }{
		// Units that used to be mangled mid-token.
		{"velocity 3 km/s downrange", "velocity 3 kilometres per second downrange"}, // was "kmetres per second"
		{"the cm/s reading", "the centimetres per second reading"},                  // was "cmetres per second"
		{"drift 2 mm/s left", "drift 2 millimetres per second left"},                // was "mmetres per second"
		{"dose 5 µg/L of solvent", "dose 5 micrograms per litre of solvent"},        // was "µgrams per litre"
		{"dose 5 μg/L of solvent", "dose 5 micrograms per litre of solvent"},        // U+03BC form
		{"the ng/L trace", "the nanograms per litre trace"},                         // was "ngrams per litre"
		{"meeting on 8/9/2026 at noon", "meeting on 8/9/2026 at noon"},              // was "8 over 9/2026"
		{"ratio 1/2/3 split", "ratio 1/2/3 split"},                                  // was "1 over 2/3"
		{"logged 2026/08/09 exactly", "logged 2026/08/09 exactly"},                  // ISO form too
		// Units that must still expand.
		{"dose 5 mg/L of LAA", "dose 5 milligrams per litre of LAA"},
		{"conc 4 g/L now", "conc 4 grams per litre now"},
		{"speed 9 m/s flat", "speed 9 metres per second flat"},
		{"rate 2 L/kg now", "rate 2 litres per kilogram now"},
		{"density 5 kg/m³ here", "density 5 kilograms per cubic metre here"},
		{"m/s at the start", "metres per second at the start"},
		// Fractions that must still convert, including two separated by
		// a single character (the case the left/right guards would
		// otherwise eat — hence the rule appearing twice).
		{"split the batch 1/2 and 3/4", "split the batch 1 over 2 and 3 over 4"},
		{"mix 1/2 3/4 cups", "mix 1 over 2 3 over 4 cups"},
		{"just 1/2", "just 1 over 2"},
		{"1/2 at the start", "1 over 2 at the start"},
		// Unchanged: the rules that were already right.
		{"Drive the F-150 to the lot", "Drive the F 150 to the lot"},
		{"compute the di/d0 ratio", "compute the d i over d 0 ratio"},
		{"heat to 70°C and reach 50% conversion", "heat to 70 degrees C and reach 50 percent conversion"},
		{"Lesson 10 - Cereal Wort Production", "Lesson 10 - Cereal Wort Production"},
		{"the value is 1e-4 molar", "the value is 1e-4 molar"},
		{"", ""},
	}
	for _, c := range cases {
		got, err := ttsNormalizeTemplate(c.in, "")
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("\n in:   %q\n got:  %q\n want: %q", c.in, got, c.want)
		}
	}
}

// TestDefaultTTSRules_NoLiteralPatternUnitRules is the structural form
// of the same finding, and it is the one that survives somebody adding
// a rule the golden table above does not know about. A literal
// `pattern:` for a unit is a rule with no boundary, and the next person
// to add "mL/min" will reach for the cheaper spelling unless something
// says not to.
func TestDefaultTTSRules_NoLiteralPatternUnitRules(t *testing.T) {
	defaultOnce.Do(loadDefaults)
	for i, r := range defaultRules {
		if r.Pattern != "" && strings.Contains(r.Pattern, "/") {
			t.Errorf("rule %d uses a literal pattern %q containing '/': a unit rule needs a "+
				"boundary, or it fires mid-token (this is how 'µg/L' became 'µgrams per litre')", i, r.Pattern)
		}
	}
	if len(defaultRules) == 0 {
		t.Fatal("the embedded defaults parsed to zero rules")
	}
}

// ── the per-pipeline override path (was 0.0% covered) ────────────────

// TestLoadRulesFile_OverridesLayerOnTopOfDefaults exercises the whole
// override feature end to end. `ttsNormalize rulesPath!="" branch
// removed` survived the suite: the entire feature could be deleted
// with nothing noticing.
func TestLoadRulesFile_OverridesLayerOnTopOfDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tts_rules.yaml")
	write(t, path, "rules:\n"+
		"  - pattern: 'LAA'\n"+
		"    replacement: 'L-A-A'\n"+
		"  - regex: '\\bABV\\b'\n"+
		"    replacement: 'A-B-V'\n")

	got, err := ttsNormalizeTemplate("dose 5 mg/L of LAA at 40 ABV", path)
	if err != nil {
		t.Fatal(err)
	}
	// Defaults applied first, then the overrides.
	if want := "dose 5 milligrams per litre of L-A-A at 40 A-B-V"; got != want {
		t.Errorf("\n got:  %q\n want: %q", got, want)
	}
	// The defaults are unaffected for a caller that names no file.
	plain, err := ttsNormalizeTemplate("of LAA", "")
	if err != nil {
		t.Fatal(err)
	}
	if plain != "of LAA" {
		t.Errorf("overrides leaked into the defaults: %q", plain)
	}
}

// TestLoadRulesFile_MissingOrBrokenIsAnError pins the behaviour the
// doc comment used to deny. ttsNormalize's comment promised
// warn-and-continue; there is no logger in the file and never was, so
// a typo'd rules path aborts the audio stage's template render. The
// code was kept and the comment fixed — a pipeline that names a rules
// file has declared it load-bearing, and silently shipping a whole
// module of mispronounced audio is the worse failure.
func TestLoadRulesFile_MissingOrBrokenIsAnError(t *testing.T) {
	dir := t.TempDir()

	missing := filepath.Join(dir, "nope.yaml")
	if _, err := loadRulesFile(missing); err == nil {
		t.Error("a missing rules file must be an error")
	}
	out, err := ttsNormalizeTemplate("hello", missing)
	if err == nil {
		t.Error("ttsNormalize must surface the missing rules file")
	}
	if out != "" {
		t.Errorf("error path must not return partial output, got %q", out)
	}

	bad := filepath.Join(dir, "bad.yaml")
	write(t, bad, "rules: [ this is not: valid: yaml\n")
	if _, err := loadRulesFile(bad); err == nil {
		t.Error("an unparseable rules file must be an error")
	}

	// And it reaches the caller as a stage-render failure rather than
	// silently mispronounced audio — the whole point of the choice.
	tmpl := template.Must(template.New("audio").Funcs(templateFuncs()).
		Parse(`{{ ttsNormalize "hello" "` + missing + `" }}`))
	var sb strings.Builder
	if err := tmpl.Execute(&sb, nil); err == nil {
		t.Error("the template render must fail, not emit an empty string")
	}

	// Empty path is not an error: opting out is spelled by naming no
	// file, not by naming a broken one.
	rules, err := loadRulesFile("")
	if err != nil || rules != nil {
		t.Errorf(`loadRulesFile("") = %v, %v; want nil, nil`, rules, err)
	}
}

// TestLoadRulesFile_CacheServesAndInvalidates covers the mtime-keyed
// cache, which was entirely unexecuted. The size half of the key is
// the fix: an editor that rewrites within the same filesystem mtime
// tick used to leave the cached rules in place and the operator's edit
// silently did not apply.
func TestLoadRulesFile_CacheServesAndInvalidates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cached.yaml")
	write(t, path, "rules:\n  - pattern: 'AAA'\n    replacement: 'first'\n")

	first, err := loadRulesFile(path)
	if err != nil || len(first) != 1 || first[0].Replacement != "first" {
		t.Fatalf("first load: %+v %v", first, err)
	}
	// Unchanged file: served from cache, same answer.
	again, err := loadRulesFile(path)
	if err != nil || len(again) != 1 || again[0].Replacement != "first" {
		t.Fatalf("cached load: %+v %v", again, err)
	}

	// Rewrite with the mtime forced back to the original value. Only
	// the size differs, which is exactly the case mtime alone missed.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, "rules:\n  - pattern: 'AAA'\n    replacement: 'second-and-longer'\n")
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := loadRulesFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if after[0].Replacement != "second-and-longer" {
		t.Errorf("stale cache entry served after a same-mtime rewrite: %q", after[0].Replacement)
	}

	// A normal edit (new mtime) invalidates too.
	write(t, path, "rules:\n  - pattern: 'AAA'\n    replacement: 'third'\n")
	if err := os.Chtimes(path, time.Now().Add(time.Second), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	third, err := loadRulesFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if third[0].Replacement != "third" {
		t.Errorf("cache did not invalidate on a new mtime: %q", third[0].Replacement)
	}
}

// TestCompileRules_MalformedRegexNeverMatches states the documented
// fallback rather than leaving it to a comment. The review correctly
// disqualified the corresponding mutant as EQUIVALENT — applyTTSRules
// skips nil-compiled rules, so swapping the never-matching pattern for
// nil changes nothing observable — which is precisely why the
// behaviour needs a test instead of a registry entry.
func TestCompileRules_MalformedRegexNeverMatches(t *testing.T) {
	rules := []ttsRule{
		{Regex: `([unclosed`, Replacement: "BOOM"},
		{Pattern: "cat", Replacement: "dog"},
	}
	compileRules(rules)
	if got := applyTTSRules("fine ([unclosed cat", rules); got != "fine ([unclosed dog" {
		t.Errorf("a malformed rule must be inert, not fatal and not matching: %q", got)
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
