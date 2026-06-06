// Package contentkit holds the reusable content-generation pipeline patterns
// shared across the vibe content apps (e.g. a serialized-fiction pipeline, a
// short-video pipeline, a textbook-to-audiobook pipeline).
//
// Each app independently reinvented the same quality-and-consistency machinery:
// diagnose-then-fix critique cycles, generate-N-pick-best tournaments, the
// shot-enumeration render that feeds a Foreach, and the staged accept/discard
// review loop. contentkit owns only the vamp *wiring* of those patterns. What
// stays app-specific, deliberately:
//
//   - PROMPTS — each app's critique/judge prompts audit different axes (a
//     serialized-fiction pipeline hunts prose slop + canon; a short-video
//     pipeline runs a laugh audit). Builders take a PromptFS so each app passes
//     its own embedded prompts.
//   - STATE MODELS — a fiction pipeline's notebook/canon, a video pipeline's
//     series, a textbook's curriculum. contentkit never models domain state.
//   - QUALITY METRICS — the Scorer interface is shared; the implementations
//     (slop terms, laugh ratings) live in each app.
//
// The builders are thin: they construct vamp stages directly and return them, so
// callers keep full access to the underlying DSL.
package contentkit
