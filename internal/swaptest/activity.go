package swaptest

import (
	"net/http"
	"sort"
	"strconv"
)

// RowTokens is an activity row's token block, verbatim from the wire.
type RowTokens struct {
	CacheTokens     int64   `json:"cache_tokens"`
	DraftTokens     int64   `json:"draft_tokens"`
	DraftAccTokens  int64   `json:"draft_acc_tokens"`
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	PromptPerSecond float64 `json:"prompt_per_second"`
	TokensPerSecond float64 `json:"tokens_per_second"`
}

// Row is one /api/metrics/activity row. The field set is what v239 sends,
// has_capture and metadata included — a struct that omits them is not a
// reason to believe llama-swap does.
type Row struct {
	ID              int64     `json:"id"`
	Timestamp       string    `json:"timestamp"`
	Model           string    `json:"model"`
	ReqPath         string    `json:"req_path"`
	RespContentType string    `json:"resp_content_type"`
	RespStatusCode  int       `json:"resp_status_code"`
	Tokens          RowTokens `json:"tokens"`
	DurationMs      int64     `json:"duration_ms"`
	ErrorMsg        string    `json:"error_msg,omitempty"`
	// has_capture is on EVERY real row (checked on v239 and v247 alike), so
	// it is not omitempty here: a double whose rows are missing a key the
	// wire always carries is a double a decoder can be written against and
	// still fail in production.
	HasCapture bool              `json:"has_capture"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

// handleActivity serves the paginated activity log. Ordering is newest-id
// first, matching the real endpoint (and the sort=id&order=desc query
// usagemeter sends). `total` is the number of rows the store HOLDS, not the
// number it has ever seen: on the default in-memory ring that is capped at
// 1000 while ids climb past 90,000, and code that treats total as a
// lifetime count is wrong on every real cell.
func (c *Cell) handleActivity(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit < 1 {
		limit = 100
	}

	c.mu.Lock()
	c.pageReads++
	if c.evictAfterReq > 0 && c.pageReads > c.evictAfterReq && c.evictRows > 0 {
		// Eviction DURING a paginated walk. A ring under load does this on
		// its own; nothing but a fake can make it happen between page 1 and
		// page 2 on demand.
		n := min(c.evictRows, len(c.rows))
		c.rows = c.rows[n:]
		c.evictAfterReq = 0
	}
	rows := make([]Row, len(c.rows))
	copy(rows, c.rows)
	c.mu.Unlock()

	sort.Slice(rows, func(i, j int) bool { return rows[i].ID > rows[j].ID })
	total := len(rows)
	totalPages := (total + limit - 1) / limit
	start := (page - 1) * limit
	if start > total {
		start = total
	}
	end := min(start+limit, total)

	writeJSON(w, map[string]any{
		"data":        rows[start:end],
		"page":        page,
		"limit":       limit,
		"total":       total,
		"total_pages": totalPages,
	})
}

// EvictDuringWalk drops the oldest `rows` entries once `afterPageReads`
// activity pages have been served. It reproduces the one race a paginated
// walk over a ring cannot avoid and no test in this module has ever
// executed: the store mutating between two pages of the same walk.
func (c *Cell) EvictDuringWalk(afterPageReads, rows int) {
	c.mu.Lock()
	c.evictAfterReq = afterPageReads
	c.evictRows = rows
	c.mu.Unlock()
}

// Rows returns a copy of the activity store, newest first.
func (c *Cell) Rows() []Row {
	c.mu.Lock()
	rows := make([]Row, len(c.rows))
	copy(rows, c.rows)
	c.mu.Unlock()
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID > rows[j].ID })
	return rows
}

// SetNextID moves the activity id counter. A store swap presents as an id
// DISCONTINUITY and nothing else, which is what C7a's cursor anchor exists
// to survive: downward means a fresh store, upward is indistinguishable
// from a cell that served a lot while nobody was reading.
func (c *Cell) SetNextID(id int64) {
	c.mu.Lock()
	c.nextID = id
	c.mu.Unlock()
}

// ClearRows empties the activity store without touching the id counter —
// the shape of a llama-swap restarted onto a fresh in-memory ring.
func (c *Cell) ClearRows() {
	c.mu.Lock()
	c.rows = nil
	c.mu.Unlock()
}
