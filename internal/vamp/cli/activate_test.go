package cli

import "testing"

func TestParseVibeStartHint(t *testing.T) {
	for _, tc := range []struct {
		hint string
		want string
	}{
		{"vibe start searxng", "searxng"},
		{"  vibe start  tts_kokoro  ", "tts_kokoro"},
		{"vibe start embed_bge_large --no-vram-check", "embed_bge_large"},
		// Real shape from earlier setup_hint strings — multi-line.
		{"# fire up the search sidecar\nvibe start searxng", "searxng"},
		// Hyphenated profile name (allowed).
		{"vibe start chat-with-search", "chat-with-search"},
		// Non-matching hints return "".
		{"docker compose up -d", ""},
		{"vibe profile activate searxng", ""}, // dead pre-`vibe start` form
		{"", ""},
		{"vibe stop searxng", ""},
	} {
		got := parseVibeStartHint(tc.hint)
		if got != tc.want {
			t.Errorf("parseVibeStartHint(%q) = %q, want %q", tc.hint, got, tc.want)
		}
	}
}
