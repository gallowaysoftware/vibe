# Getting genuinely good long-form content out of a local LLM

This is the methodology behind the vibe content stack — the patterns that turn a
single mid-sized local model (a ~27B running on one consumer GPU) into something
that produces coherent, consistent, *good* long-form content: audiobooks from
papers, serialized fiction with years-deep canon, short-form video. The model is
the same one everybody has. The quality comes from the **pipeline around it**.

`contentkit` is the reusable distillation of these patterns; each app
(worldsmith, brainrot, textbook-to-audiobook) supplies its own prompts and state.

The throughline: **a local LLM is a strong component and a weak author.** It drafts
well, it diagnoses ruthlessly, it compares reliably — but left to "just write it
well," it produces fluent slop, drifts from its own facts, and cheats its own
rules. So you don't ask it to be the author. You build the author out of many
small, single-purpose passes, most of which are *checking* passes.

---

## The patterns

### 1. Diagnose-then-fix (don't ask for a better draft; ask what's wrong, then fix that)
A model told to "rewrite this better" won't reliably avoid the flaws it just wrote.
But a *separate, cool pass* that only **names and quotes** the problems is reliable
— diagnosis and generation are different modes, and the model is good at the
former. Then a fix pass resolves a concrete, quoted list instead of a vague
mandate. `contentkit.CritiqueRevise` wires this. It is the single highest-leverage
pattern in the stack.

### 2. The fixer's only job must be to fix
Corollary, learned the hard way: if the rewrite pass has a *competing* mandate
(e.g. "also expand this to length"), the loud mandate drowns the fix and the flaws
survive. Split it: a length/structure pass first, **then** a surgical pass whose
sole instruction is "apply these notes, change nothing else." Surgical means
surgical — keep every unflagged sentence verbatim.

### 3. Two cycles, because the rewrite introduces new flaws
The fix pass is itself a generation, so it can add a fresh problem the first
critique never saw (a butchered ending, a new off-screen reference). A second
diagnose→fix cycle on the rewrite catches those. Diminishing returns after two.

### 4. Tournaments beat single-shot for anything subjective
For comedy, hooks, prose openings — generate N independent attempts at varied
temperatures and have a judge pick the best (and graft the best beats from the
rest). The model compares humor far more reliably than it generates it.
`contentkit.Tournament`.

### 5. Multi-role rooms, and the skeptic is the valuable one
A "writers' room → producers → greenlight" of distinct personas produces better
*and more varied* concepts than one voice — but the payoff is the adversary: a
skeptic whose job is to kill the derivative, the unsustainable, and the
**unshootable** (can the local image model actually render this? can the premise
sustain a scene every episode?). Force engine/register diversity explicitly or the
personas collapse onto one joke.

### 6. Separate what the author knows from what the reader is shown (fog of war)
The most novel pattern. Keep a private knowledge layer — secrets, where threads
are going, deep interiority — distinct from the published bible and from canon
(what's been shown). Generation draws on the private layer for subtext and
foreshadowing under an explicit *reveal-control* rule: it may press on a scene from
underneath but must never state what's sealed. This is what lets a story feel
written by someone who knows the ending. (worldsmith's notebook.)

### 7. Canon = what shipped, not what was extracted
When you extract facts from a draft to feed future installments, the extraction
**drifts** — it records a death the prose didn't show, a mechanism a later edit
replaced, an invented character. Add a reconciliation pass: re-derive the canon
delta against the *finished prose* + the summary + existing canon, and let the
prose win. Without this, episode N+1 contradicts episode N and the world rots.

### 8. Count what code can count (deterministic anti-slop) and roll it into a scorecard
LLM judges miss *structural* degradation — slop-word density, the "not X but Y"
reflex, anaphora, repeated phrases — because those are counting problems, and
counting is what code is for. Compute them deterministically, surface them as a
per-installment scorecard alongside the LLM checks, and track the number across
installments. Quality you don't measure is quality you're guessing at.
`contentkit.Scorer`/`Scorecard`.

### 9. Mechanics over prompt rules
Models treat prose rules as advisory even when you write "ABSOLUTE." When you need
a *guarantee* (a length cap, a mode switch, an enumeration, a distribution), do it
at render-time or in code, not by asking nicely. A worked BAD→GOOD example in the
prompt also beats a paragraph of rules — examples land where rules don't.

### 10. Never destroy the human's work; propose and let them curate
Everything the machine adds to something a human cares about is *staged*, never
merged — reviewed accept/edit/reject, with backups on overwrite. This is what makes
it safe to let the engine run autonomously: it generates candidates, the human
keeps the good ones. `contentkit.ReviewLoop`.

---

## Local-model realities (the constraints that shape everything)

- **VRAM forces a two-phase shape.** The LLM (~26GB) and the image model (~20GB)
  can't co-reside on 32GB. So content pipelines split: phase 1 generates structure
  (LLM), unload, phase 2 renders (image/video/TTS). Orchestrated at the CLI, not in
  the DAG.
- **Geography is the model's weakness, and your leverage.** A 27B's ceiling on
  genuine comedy and on visual spectacle is real — short-form video fights the
  stack's weaknesses (slideshow visuals, viral-hit comedy). The stack's *strength*
  is long-form, consistency-heavy, audio-out content, where "is it coherent and
  engaging" is a bar these patterns can actually clear.
- **Sampler hygiene matters** (EXL3 needs explicit min_p / repetition_penalty or it
  collapses; turn off chain-of-thought on stages whose output is the product).

## The shape this points at

These patterns are domain-general. A *world*, a *paper*, a *subject* is a **canon**;
audiobook / episode / short / study-guide / wiki are just renderers over it. The
engine isn't three apps — it's one consistency-and-quality machine with many
front-ends. That's why the patterns live in one `contentkit` and the apps are thin.
