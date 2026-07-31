// Package store owns the git-backed memory root. Design decisions
// (docs/design/companions.md in the subkb repo, "The memory layer"):
//
//   - Markdown files in a git repo, NOT database rows: memory must be
//     human-legible, human-editable, and carry decision history. Git IS
//     the audit trail; there is no separate versioning scheme.
//   - No vector index. Memory corpora hold only the user's own writing
//     and stay small; whole-file reads and grep-style search beat an
//     embedding stack at this scale, and dropping the index removes the
//     re-ingest problem entirely (human edits need no watcher — every
//     read hits disk).
//   - One writer: all mutations pass through this store under a mutex
//     and become git commits, which is what makes concurrent clients
//     (phone, riff, Claude Code) safe on one working tree.
//   - Session context is deliberately OUTSIDE git (.session/, ignored):
//     "tonight I'm doing boarding ops" is scratch state with a TTL, not
//     memory. Session SUMMARIES graduate to memory via AppendSessionLog.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ErrNotFound distinguishes "no such expert/file" from real failures.
var ErrNotFound = errors.New("not found")

// expertName also guards against path traversal: every expert becomes a
// directory name, so the allowed alphabet is the security boundary.
var expertName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// noteSlug bounds note topic filenames the same way.
var noteSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// coreFiles are the per-expert scaffold. Conventions over configuration:
// the digest builder and the tools both key on these names.
var coreFiles = []string{"profile.md", "goals.md", "decisions.md", "log.md"}

const sessionTTL = 24 * time.Hour

// Store is the single writer over the memory root.
type Store struct {
	root string
	mu   sync.Mutex
	now  func() time.Time // injectable for tests
}

func New(root string) *Store {
	return &Store{root: root, now: time.Now}
}

// EnsureRepo makes the root a git repository with session scratch
// ignored. Idempotent; called at startup.
func (s *Store) EnsureRepo() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Join(s.root, ".session"), 0o755); err != nil {
		return fmt.Errorf("create memory root: %w", err)
	}
	if _, err := os.Stat(filepath.Join(s.root, ".git")); err == nil {
		return nil
	}
	if out, err := s.git("init", "-q"); err != nil {
		return fmt.Errorf("git init: %s: %w", out, err)
	}
	gitignore := ".session/\n"
	if err := os.WriteFile(filepath.Join(s.root, ".gitignore"), []byte(gitignore), 0o644); err != nil {
		return fmt.Errorf("write .gitignore: %w", err)
	}
	if out, err := s.commitLocked([]string{".gitignore"}, "recall: initialize memory root"); err != nil {
		return fmt.Errorf("initial commit: %s: %w", out, err)
	}
	return nil
}

// git runs a git command in the root with identity pinned per-invocation
// — no dependency on host-level git config inside the container.
func (s *Store) git(args ...string) (string, error) {
	full := append([]string{
		"-C", s.root,
		"-c", "user.name=recall",
		"-c", "user.email=recall@localhost",
	}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// commitLocked stages paths and commits. Callers hold s.mu. Before
// staging, any dirty state from direct human edits is checkpointed as
// its own commit so tool writes never absorb (and mis-attribute) manual
// changes.
func (s *Store) commitLocked(paths []string, msg string) (string, error) {
	if status, _ := s.git("status", "--porcelain"); status != "" {
		dirty := false
		for _, line := range strings.Split(status, "\n") {
			name := strings.TrimSpace(line)
			if i := strings.IndexByte(name, ' '); i >= 0 {
				name = name[i+1:]
			}
			if !slicesContains(paths, name) {
				dirty = true
				break
			}
		}
		if dirty {
			if out, err := s.git("add", "-A"); err != nil {
				return out, fmt.Errorf("checkpoint add: %w", err)
			}
			// Reset our target paths so the checkpoint holds only the
			// external edits; ignore errors for paths new in this write.
			for _, p := range paths {
				_, _ = s.git("reset", "-q", "--", p)
			}
			if status, _ := s.git("diff", "--cached", "--name-only"); status != "" {
				if out, err := s.git("commit", "-q", "-m", "recall: checkpoint external edits"); err != nil {
					return out, fmt.Errorf("checkpoint commit: %w", err)
				}
			}
		}
	}

	args := append([]string{"add", "--"}, paths...)
	if out, err := s.git(args...); err != nil {
		return out, fmt.Errorf("git add: %w", err)
	}
	if status, _ := s.git("diff", "--cached", "--name-only"); status == "" {
		return "", nil // no-op write; nothing to commit
	}
	out, err := s.git("commit", "-q", "-m", msg)
	if err != nil {
		return out, fmt.Errorf("git commit: %w", err)
	}
	return out, nil
}

func slicesContains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func (s *Store) expertDir(expert string) (string, error) {
	if !expertName.MatchString(expert) {
		return "", fmt.Errorf("invalid expert name %q (want lowercase slug)", expert)
	}
	return filepath.Join(s.root, expert), nil
}

// ListExperts returns the expert slugs (top-level directories).
func (s *Store) ListExperts() ([]string, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, fmt.Errorf("read memory root: %w", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && expertName.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// CreateExpert scaffolds a new expert's memory directory.
func (s *Store) CreateExpert(expert, description string) error {
	dir, err := s.expertDir(expert)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("expert %s already exists", expert)
	}
	if err := os.MkdirAll(filepath.Join(dir, "notes"), 0o755); err != nil {
		return fmt.Errorf("create expert dir: %w", err)
	}
	scaffold := map[string]string{
		"profile.md":   fmt.Sprintf("# %s — profile\n\n%s\n", expert, description),
		"goals.md":     "# Goals\n",
		"decisions.md": "# Decisions\n",
		"log.md":       "# Session log\n",
	}
	paths := make([]string, 0, len(scaffold))
	for name, content := range scaffold {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			return fmt.Errorf("scaffold %s: %w", name, err)
		}
		paths = append(paths, filepath.Join(expert, name))
	}
	if out, err := s.commitLocked(paths, fmt.Sprintf("recall(%s): create expert", expert)); err != nil {
		return fmt.Errorf("commit scaffold: %s: %w", out, err)
	}
	return nil
}

// resolveFile validates a caller-supplied file path within an expert:
// either a core file or notes/<slug>.md.
func resolveFile(file string) (string, error) {
	if slicesContains(coreFiles, file) {
		return file, nil
	}
	if rest, ok := strings.CutPrefix(file, "notes/"); ok {
		slug, isMD := strings.CutSuffix(rest, ".md")
		if isMD && noteSlug.MatchString(slug) {
			return file, nil
		}
	}
	return "", fmt.Errorf("invalid memory file %q (want one of %v or notes/<slug>.md)", file, coreFiles)
}

// ReadFile returns one memory file's content.
func (s *Store) ReadFile(expert, file string) (string, error) {
	dir, err := s.expertDir(expert)
	if err != nil {
		return "", err
	}
	rel, err := resolveFile(file)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%s/%s: %w", expert, file, ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("read %s/%s: %w", expert, file, err)
	}
	return string(b), nil
}

// ListFiles returns the memory files present for an expert.
func (s *Store) ListFiles(expert string) ([]string, error) {
	dir, err := s.expertDir(expert)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("expert %s: %w", expert, ErrNotFound)
	}
	var out []string
	for _, f := range coreFiles {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			out = append(out, f)
		}
	}
	notes, _ := os.ReadDir(filepath.Join(dir, "notes"))
	for _, n := range notes {
		if !n.IsDir() && strings.HasSuffix(n.Name(), ".md") {
			out = append(out, "notes/"+n.Name())
		}
	}
	return out, nil
}

// SearchHit is one matching line.
type SearchHit struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// Search scans an expert's memory files for lines containing ANY of the
// query's terms (case-insensitive). Deliberately grep, not vectors —
// see the package comment.
func (s *Store) Search(expert, query string) ([]SearchHit, error) {
	files, err := s.ListFiles(expert)
	if err != nil {
		return nil, err
	}
	terms := strings.Fields(strings.ToLower(query))
	if len(terms) == 0 {
		return nil, fmt.Errorf("empty query")
	}
	const maxHits = 50
	var hits []SearchHit
	for _, f := range files {
		content, err := s.ReadFile(expert, f)
		if err != nil {
			continue
		}
		for i, line := range strings.Split(content, "\n") {
			lower := strings.ToLower(line)
			for _, t := range terms {
				if strings.Contains(lower, t) {
					hits = append(hits, SearchHit{File: f, Line: i + 1, Text: strings.TrimSpace(line)})
					break
				}
			}
			if len(hits) >= maxHits {
				return hits, nil
			}
		}
	}
	return hits, nil
}

// RecordDecision appends a dated decision-with-why to decisions.md.
// The "why" is mandatory by signature: a decision without rationale is
// the thing this system exists to prevent.
func (s *Store) RecordDecision(expert, decision, why string) error {
	if strings.TrimSpace(decision) == "" || strings.TrimSpace(why) == "" {
		return fmt.Errorf("decision and why are both required")
	}
	entry := fmt.Sprintf("\n## %s — %s\n\n**Why:** %s\n",
		s.now().UTC().Format("2006-01-02"), strings.TrimSpace(decision), strings.TrimSpace(why))
	return s.appendFile(expert, "decisions.md", entry,
		fmt.Sprintf("recall(%s): decision — %s", expert, firstLine(decision, 60)))
}

// AppendSessionLog appends a dated session summary to log.md.
func (s *Store) AppendSessionLog(expert, summary string) error {
	if strings.TrimSpace(summary) == "" {
		return fmt.Errorf("summary is required")
	}
	entry := fmt.Sprintf("\n## %s\n\n%s\n", s.now().UTC().Format("2006-01-02"), strings.TrimSpace(summary))
	return s.appendFile(expert, "log.md", entry,
		fmt.Sprintf("recall(%s): session log", expert))
}

// RecordNote appends to (or creates) a topic note.
func (s *Store) RecordNote(expert, topic, content string) error {
	if !noteSlug.MatchString(topic) {
		return fmt.Errorf("invalid topic %q (want lowercase slug)", topic)
	}
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("content is required")
	}
	dir, err := s.expertDir(expert)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(dir, "notes", topic+".md")
	var body []byte
	if existing, err := os.ReadFile(path); err == nil {
		body = append(existing, []byte(fmt.Sprintf("\n%s\n", strings.TrimSpace(content)))...)
	} else {
		body = []byte(fmt.Sprintf("# %s\n\n%s\n", topic, strings.TrimSpace(content)))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create notes dir: %w", err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return fmt.Errorf("write note: %w", err)
	}
	if out, err := s.commitLocked([]string{filepath.Join(expert, "notes", topic+".md")},
		fmt.Sprintf("recall(%s): note %s", expert, topic)); err != nil {
		return fmt.Errorf("commit note: %s: %w", out, err)
	}
	return nil
}

// ReplaceFile overwrites profile.md or goals.md wholesale. Full
// replacement (not patch) keeps the tool contract simple and makes the
// git diff the review surface.
func (s *Store) ReplaceFile(expert, file, content string) error {
	if file != "profile.md" && file != "goals.md" {
		return fmt.Errorf("only profile.md and goals.md are replaceable (got %q)", file)
	}
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("refusing to replace %s with empty content", file)
	}
	dir, err := s.expertDir(expert)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("expert %s: %w", expert, ErrNotFound)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(strings.TrimSpace(content)+"\n"), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", file, err)
	}
	if out, err := s.commitLocked([]string{filepath.Join(expert, file)},
		fmt.Sprintf("recall(%s): update %s", expert, file)); err != nil {
		return fmt.Errorf("commit %s: %s: %w", file, out, err)
	}
	return nil
}

func (s *Store) appendFile(expert, file, entry, msg string) error {
	dir, err := s.expertDir(expert)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("expert %s: %w", expert, ErrNotFound)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(dir, file)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", file, err)
	}
	if _, err := f.WriteString(entry); err != nil {
		f.Close()
		return fmt.Errorf("append %s: %w", file, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", file, err)
	}
	if out, err := s.commitLocked([]string{filepath.Join(expert, file)}, msg); err != nil {
		return fmt.Errorf("commit %s: %s: %w", file, out, err)
	}
	return nil
}

// session is the ephemeral scratch state, persisted outside git.
type session struct {
	Context   string    `json:"context"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SetSessionContext stores the current session's scratch context.
func (s *Store) SetSessionContext(expert, context string) error {
	if _, err := s.expertDir(expert); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(session{Context: strings.TrimSpace(context), UpdatedAt: s.now().UTC()})
	if err != nil {
		return fmt.Errorf("marshal session: %w", err)
	}
	path := filepath.Join(s.root, ".session", expert+".json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("write session: %w", err)
	}
	return nil
}

// SessionContext returns the current context, or "" once the TTL lapses
// — yesterday's "tonight I'm boarding" must not leak into today.
func (s *Store) SessionContext(expert string) (string, error) {
	if _, err := s.expertDir(expert); err != nil {
		return "", err
	}
	b, err := os.ReadFile(filepath.Join(s.root, ".session", expert+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read session: %w", err)
	}
	var sess session
	if err := json.Unmarshal(b, &sess); err != nil {
		return "", fmt.Errorf("decode session: %w", err)
	}
	if s.now().Sub(sess.UpdatedAt) > sessionTTL {
		return "", nil
	}
	return sess.Context, nil
}

// Digest composes the injection-designed markdown block: session
// context, profile, goals, recent decisions, last session log entry.
// This is the always-in-context core (Letta-style); the harness injects
// it at chat start, so it must stay compact — sections are individually
// capped and the caller-facing contract is "a few hundred tokens".
func (s *Store) Digest(expert string) (string, error) {
	if _, err := s.ListFiles(expert); err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Memory digest: %s\n", expert)

	if ctx, err := s.SessionContext(expert); err == nil && ctx != "" {
		fmt.Fprintf(&b, "\n## Current session\n%s\n", ctx)
	}
	if profile, err := s.ReadFile(expert, "profile.md"); err == nil {
		fmt.Fprintf(&b, "\n%s\n", capRunes(strings.TrimSpace(profile), 2000))
	}
	if goals, err := s.ReadFile(expert, "goals.md"); err == nil {
		fmt.Fprintf(&b, "\n%s\n", capRunes(strings.TrimSpace(goals), 2000))
	}
	if decisions, err := s.ReadFile(expert, "decisions.md"); err == nil {
		if recent := lastEntries(decisions, 5); recent != "" {
			fmt.Fprintf(&b, "\n## Recent decisions\n%s\n", capRunes(recent, 2500))
		}
	}
	if log, err := s.ReadFile(expert, "log.md"); err == nil {
		if last := lastEntries(log, 1); last != "" {
			fmt.Fprintf(&b, "\n## Last session\n%s\n", capRunes(last, 1500))
		}
	}
	return b.String(), nil
}

// lastEntries returns the last n "## "-delimited entries of a file.
func lastEntries(content string, n int) string {
	parts := strings.Split(content, "\n## ")
	if len(parts) <= 1 {
		return ""
	}
	entries := parts[1:]
	if len(entries) > n {
		entries = entries[len(entries)-n:]
	}
	return "## " + strings.Join(entries, "\n## ")
}

func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

func firstLine(s string, maxRunes int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return capRunes(s, maxRunes)
}
