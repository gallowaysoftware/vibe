// Package mcpserver exposes the recall store as MCP tools (Streamable
// HTTP) plus the plain-HTTP digest endpoint. Two protocols because
// memory reaches the model through two mechanisms: the digest is
// INJECTED by the harness at chat start (an Open WebUI inlet filter, a
// Claude Code hook), while the tools serve deep recall and writes
// during the conversation. Tools alone would fail — the model can't
// search for what it doesn't know it forgot.
//
// Write tools are exposed on the LAN unauthenticated, matching the
// house posture (LAN/VPN-only, no proxy host) — but unlike subkb this
// surface MUTATES state. The mitigations are structural: every write
// becomes a git commit (fully auditable, trivially revertible), paths
// are slug-validated, and the store is append-or-replace only — no
// delete tool exists.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gallowaysoftware/vibe/internal/recall/store"
)

// memoryStore is the store surface the tools need.
type memoryStore interface {
	ListExperts() ([]string, error)
	CreateExpert(expert, description string) error
	ListFiles(expert string) ([]string, error)
	ReadFile(expert, file string) (string, error)
	Search(expert, query string) ([]store.SearchHit, error)
	RecordDecision(expert, decision, why string) error
	RecordNote(expert, topic, content string) error
	ReplaceFile(expert, file, content string) error
	AppendSessionLog(expert, summary string) error
	SetSessionContext(expert, context string) error
	Digest(expert string) (string, error)
}

var _ memoryStore = (*store.Store)(nil)

// Deps wires the MCP server's dependencies.
type Deps struct {
	Store   memoryStore
	Version string
}

// Handler returns the Streamable HTTP handler to mount at /mcp.
func Handler(deps Deps) http.Handler {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "recall",
		Title:   "Recall — personal memory for domain experts",
		Version: deps.Version,
	}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name: "list_experts",
		Description: "List the experts that have memory, with their memory files. " +
			"Memory is per-expert: goals, decisions (with rationale), session logs, topic notes.",
	}, deps.listExperts)

	mcp.AddTool(server, &mcp.Tool{
		Name: "read_memory",
		Description: "Read one memory file in full. Files: profile.md (who the user is in this " +
			"domain), goals.md (current goals), decisions.md (dated decisions with rationale), " +
			"log.md (session summaries), notes/<topic>.md. Prefer this over search when you know " +
			"which file matters — they are small.",
	}, deps.readMemory)

	mcp.AddTool(server, &mcp.Tool{
		Name: "search_memory",
		Description: "Search an expert's memory files for lines matching any query term. " +
			"Use for 'did we already decide/discuss X?' before answering questions about the " +
			"user's plans or history.",
	}, deps.searchMemory)

	mcp.AddTool(server, &mcp.Tool{
		Name: "record_decision",
		Description: "Record a decision the user has made, WITH the reasoning. Call this " +
			"whenever the user states or confirms a choice (a build order, a purchase, a plan " +
			"change). The why is mandatory — it is the part future sessions need most.",
	}, deps.recordDecision)

	mcp.AddTool(server, &mcp.Tool{
		Name: "record_note",
		Description: "Append a note under a topic (lowercase-slug). For facts and context the " +
			"user shares that are neither decisions nor goals — preferences, constraints, " +
			"things learned.",
	}, deps.recordNote)

	mcp.AddTool(server, &mcp.Tool{
		Name: "update_goals",
		Description: "Replace the expert's goals.md wholesale with updated content. Use when " +
			"goals change or complete; carry forward everything still true — this is a full " +
			"replacement and the git diff is the review surface.",
	}, deps.updateGoals)

	mcp.AddTool(server, &mcp.Tool{
		Name: "update_profile",
		Description: "Replace the expert's profile.md wholesale (playstyle, preferences, " +
			"spoiler tolerance, hardware, constraints). Same full-replacement contract as " +
			"update_goals.",
	}, deps.updateProfile)

	mcp.AddTool(server, &mcp.Tool{
		Name: "append_session_log",
		Description: "Append a dated session summary: what happened, what was decided, where " +
			"things left off. Call at natural session ends ('that's it for tonight') — this is " +
			"what makes the next session continuous.",
	}, deps.appendSessionLog)

	mcp.AddTool(server, &mcp.Tool{
		Name: "set_session_context",
		Description: "Set the CURRENT session's context ('tonight: first boarding op'). " +
			"Ephemeral scratch with a 24h TTL — not memory. Use at session start or when the " +
			"user states what they're working on right now.",
	}, deps.setSessionContext)

	mcp.AddTool(server, &mcp.Tool{
		Name: "create_expert",
		Description: "Create a new expert's memory space. Only when the user explicitly asks " +
			"to start tracking a new domain.",
	}, deps.createExpert)

	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
}

// DigestHandler serves GET /digest?expert=<slug> as text/markdown — the
// injection product, fetched by harness-side hooks at chat start.
func DigestHandler(deps Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expert := r.URL.Query().Get("expert")
		if expert == "" {
			http.Error(w, "expert query parameter is required", http.StatusBadRequest)
			return
		}
		digest, err := deps.Store.Digest(expert)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "unknown expert", http.StatusNotFound)
				return
			}
			slog.Error("recall: digest failed", "expert", expert, "error", err)
			http.Error(w, "digest failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = w.Write([]byte(digest))
	})
}

type expertsOutput struct {
	Experts []expertInfo `json:"experts"`
}

type expertInfo struct {
	Expert string   `json:"expert"`
	Files  []string `json:"files"`
}

func (d Deps) listExperts(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, expertsOutput, error) {
	experts, err := d.Store.ListExperts()
	if err != nil {
		slog.Error("recall: list experts failed", "error", err)
		return nil, expertsOutput{}, fmt.Errorf("list experts failed")
	}
	out := expertsOutput{Experts: make([]expertInfo, 0, len(experts))}
	for _, e := range experts {
		files, err := d.Store.ListFiles(e)
		if err != nil {
			continue
		}
		out.Experts = append(out.Experts, expertInfo{Expert: e, Files: files})
	}
	return nil, out, nil
}

type readInput struct {
	Expert string `json:"expert" jsonschema:"expert slug from list_experts"`
	File   string `json:"file" jsonschema:"memory file: profile.md, goals.md, decisions.md, log.md, or notes/<topic>.md"`
}

type readOutput struct {
	Content string `json:"content"`
}

func (d Deps) readMemory(ctx context.Context, _ *mcp.CallToolRequest, in readInput) (*mcp.CallToolResult, readOutput, error) {
	content, err := d.Store.ReadFile(in.Expert, in.File)
	if err != nil {
		return nil, readOutput{}, err
	}
	return nil, readOutput{Content: content}, nil
}

type searchInput struct {
	Expert string `json:"expert" jsonschema:"expert slug"`
	Query  string `json:"query" jsonschema:"search terms; lines matching any term are returned"`
}

type searchOutput struct {
	Hits []store.SearchHit `json:"hits"`
}

func (d Deps) searchMemory(ctx context.Context, _ *mcp.CallToolRequest, in searchInput) (*mcp.CallToolResult, searchOutput, error) {
	hits, err := d.Store.Search(in.Expert, in.Query)
	if err != nil {
		return nil, searchOutput{}, err
	}
	return nil, searchOutput{Hits: hits}, nil
}

type decisionInput struct {
	Expert   string `json:"expert" jsonschema:"expert slug"`
	Decision string `json:"decision" jsonschema:"the decision, one clear sentence first"`
	Why      string `json:"why" jsonschema:"the reasoning behind it — mandatory"`
}

type ackOutput struct {
	OK bool `json:"ok"`
}

func (d Deps) recordDecision(ctx context.Context, _ *mcp.CallToolRequest, in decisionInput) (*mcp.CallToolResult, ackOutput, error) {
	if err := d.Store.RecordDecision(in.Expert, in.Decision, in.Why); err != nil {
		return nil, ackOutput{}, err
	}
	return nil, ackOutput{OK: true}, nil
}

type noteInput struct {
	Expert  string `json:"expert" jsonschema:"expert slug"`
	Topic   string `json:"topic" jsonschema:"lowercase-slug topic, e.g. boarding or excise-rules"`
	Content string `json:"content" jsonschema:"the note content (markdown)"`
}

func (d Deps) recordNote(ctx context.Context, _ *mcp.CallToolRequest, in noteInput) (*mcp.CallToolResult, ackOutput, error) {
	if err := d.Store.RecordNote(in.Expert, in.Topic, in.Content); err != nil {
		return nil, ackOutput{}, err
	}
	return nil, ackOutput{OK: true}, nil
}

type replaceInput struct {
	Expert  string `json:"expert" jsonschema:"expert slug"`
	Content string `json:"content" jsonschema:"the complete new file content (markdown)"`
}

func (d Deps) updateGoals(ctx context.Context, _ *mcp.CallToolRequest, in replaceInput) (*mcp.CallToolResult, ackOutput, error) {
	if err := d.Store.ReplaceFile(in.Expert, "goals.md", in.Content); err != nil {
		return nil, ackOutput{}, err
	}
	return nil, ackOutput{OK: true}, nil
}

func (d Deps) updateProfile(ctx context.Context, _ *mcp.CallToolRequest, in replaceInput) (*mcp.CallToolResult, ackOutput, error) {
	if err := d.Store.ReplaceFile(in.Expert, "profile.md", in.Content); err != nil {
		return nil, ackOutput{}, err
	}
	return nil, ackOutput{OK: true}, nil
}

type sessionLogInput struct {
	Expert  string `json:"expert" jsonschema:"expert slug"`
	Summary string `json:"summary" jsonschema:"what happened, what was decided, where things left off"`
}

func (d Deps) appendSessionLog(ctx context.Context, _ *mcp.CallToolRequest, in sessionLogInput) (*mcp.CallToolResult, ackOutput, error) {
	if err := d.Store.AppendSessionLog(in.Expert, in.Summary); err != nil {
		return nil, ackOutput{}, err
	}
	return nil, ackOutput{OK: true}, nil
}

type sessionContextInput struct {
	Expert  string `json:"expert" jsonschema:"expert slug"`
	Context string `json:"context" jsonschema:"what the user is working on right now"`
}

func (d Deps) setSessionContext(ctx context.Context, _ *mcp.CallToolRequest, in sessionContextInput) (*mcp.CallToolResult, ackOutput, error) {
	if err := d.Store.SetSessionContext(in.Expert, in.Context); err != nil {
		return nil, ackOutput{}, err
	}
	return nil, ackOutput{OK: true}, nil
}

type createExpertInput struct {
	Expert      string `json:"expert" jsonschema:"lowercase slug, e.g. x4 or distillery"`
	Description string `json:"description" jsonschema:"one line on what this expert covers"`
}

func (d Deps) createExpert(ctx context.Context, _ *mcp.CallToolRequest, in createExpertInput) (*mcp.CallToolResult, ackOutput, error) {
	if err := d.Store.CreateExpert(in.Expert, in.Description); err != nil {
		return nil, ackOutput{}, err
	}
	return nil, ackOutput{OK: true}, nil
}
