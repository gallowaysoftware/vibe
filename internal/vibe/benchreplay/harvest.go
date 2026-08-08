package benchreplay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/observed"
	"github.com/gallowaysoftware/vibe/internal/vibe/usagemeter"
	"gopkg.in/yaml.v3"
)

// The harvest: the ONE step that reads private traffic, and it is over
// before the config is touched.
//
// Walk /api/metrics/activity newest-first for rows on the incumbent that
// advertise has_capture, GET /api/captures/{id} for each, reduce it to a
// request body plus two shapes, hold it in memory. Nothing here writes a
// file; this package cannot.

// sample is one harvested capture, held for one process lifetime.
//
// Unexported, holds []byte, has no MarshalJSON, and appears in no exported
// signature — that trio is mechanism 1 of the privacy invariant. There is
// no journal state that could hold one: a state meaning "a sample exists"
// is a standing invitation to persist it, and it would buy only the
// ability to resume a replay across a process death, which is a thing that
// SHOULD fail because the sample would be stale traffic replayed against a
// decision made later.
type sample struct {
	activityID int64
	// reqBody is the captured request, replayed verbatim except for the
	// two fields replay.go must rewrite. It is never printed, never
	// logged, never written and never returned.
	reqBody  []byte
	facts    requestFacts
	recorded shape
	// status carries the capture endpoint's HTTP code on the REFUSED path
	// and nothing else. A number, never a body.
	status int
}

// Set is a harvested sample. Every field is unexported: there is no way to
// read a body out of it, and json.Marshal of one is "{}".
type Set struct {
	opt   Options
	model string
	items []sample
	stats SampleStats
}

// Len reports how many captures were harvested. It is the only thing about
// the sample this type will tell anyone.
func (s *Set) Len() int {
	if s == nil {
		return 0
	}
	return len(s.items)
}

// activityRow is the strict subset of llama-swap's activity row this
// package reads.
//
// It is defined HERE rather than reused from usagemeter.ActivityRow on
// purpose. That type is the ledger's, it is gated by
// TestActivityRow_CannotCarryABody as "counts only, never bodies", and
// adding a field to it to serve a replay would edit the ledger's own
// privacy gate to make a capture reader more convenient. The walk is
// thirty lines; the gate is worth more.
type activityRow struct {
	ID         int64  `json:"id"`
	Model      string `json:"model"`
	ReqPath    string `json:"req_path"`
	Status     int    `json:"resp_status_code"`
	HasCapture bool   `json:"has_capture"`
}

type activityPage struct {
	Data       []activityRow `json:"data"`
	TotalPages int           `json:"total_pages"`
}

// CapturesDisabled reports whether a cell's llama-swap config declares
// `captureBuffer: 0`.
//
// The answer is an observed.Value because the three states are genuinely
// different: declared 0 (refuse by name), declared non-zero or absent
// (upstream's default is 10 MB, so captures are ON — which is true of
// every cell on the reference fleet right now, and was true for months
// before anybody wrote it down), and a config this process cannot read at
// all, which is UNKNOWN and must never read as "enabled".
func CapturesDisabled(configPath string) observed.Value[bool] {
	if configPath == "" {
		return observed.Value[bool]{}
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return observed.Value[bool]{}
	}
	var cfg struct {
		CaptureBuffer *int `yaml:"captureBuffer"`
	}
	if yaml.Unmarshal(data, &cfg) != nil {
		return observed.Value[bool]{}
	}
	if cfg.CaptureBuffer == nil {
		// Absent means upstream's default, which is 10 MB and ON.
		return observed.Known(false)
	}
	return observed.Known(*cfg.CaptureBuffer == 0)
}

// Harvest walks the cell's activity log for the model's own recent
// requests and fetches their captures.
//
// applied says the trial has already written the cell's config. It is a
// parameter rather than something this package infers because the caller
// owns the journal — and it is a REFUSAL rather than a warning, because
// the apply is a -watch-config reload and the reload allocates a fresh,
// empty capture buffer. "Apply the trial, then replay the cell's captures"
// is not a thing that can happen.
func Harvest(ctx context.Context, opt Options, model string, applied bool) (*Set, error) {
	if applied {
		return nil, ErrAlreadyApplied
	}
	opt = opt.withDefaults()
	if disabled, known := CapturesDisabled(opt.ConfigPath).Observed(); known && disabled {
		return nil, ErrCapturesDisabled
	}

	set := &Set{opt: opt, model: model}
	// The harvest's own wall bound. MaxPages x up to 999 rows, each
	// candidate row costing a fetch with its own captureFetchTimeout, and a
	// row that 404s or 401s never counts toward MaxSample — so without this
	// the fetch count is uncapped. It runs in the same command as the
	// replay, under the same four-hour lease and C11 hold, and immediately
	// before the disruptive apply, so it refuses rather than holding the
	// cell open.
	deadline := opt.Now().Add(opt.HarvestBudget)
	for page := 1; page <= opt.MaxPages && len(set.items) < opt.MaxSample; page++ {
		p, err := fetchActivityPage(ctx, opt, page)
		if err != nil {
			return nil, err
		}
		if len(p.Data) == 0 {
			break
		}
		for _, row := range p.Data {
			if opt.Now().After(deadline) {
				return nil, fmt.Errorf("the harvest ran past its %s budget after %d row(s), with %d capture(s) in hand",
					opt.HarvestBudget, set.stats.Walked, len(set.items))
			}
			set.stats.Walked++
			if row.Model != model || !row.HasCapture {
				continue
			}
			// Two filters, and they are not the same filter.
			//
			// The basis branch is usagemeter's, on req_path and never on
			// backend kind: only the chat family has the tool-call and
			// finish-reason structure this package scores.
			//
			// The second is narrower and it is the one a review had to
			// find: usagemeter's chat basis also covers /v1/completions,
			// /infill, /completion, /v1/responses and /v1/messages —
			// real endpoints a llama.cpp cell serves, with request shapes
			// this package cannot rebuild. Admitting them meant the
			// rewrite failed, `one` returned an unknown shape, and the
			// row was counted as a request the MODEL failed, on both
			// sides. A request the harness could not construct is not a
			// model's failure, and /v1/messages is worse than the rest:
			// it parses, gets POSTed to the wrong API, and its
			// Anthropic-shaped `tools` array reads as "no tools
			// declared", so it silently leaves the tool-call denominator.
			if usagemeter.BasisFor(row.ReqPath) != usagemeter.BasisChat || !replayable(row.ReqPath) {
				set.stats.SkippedBasis++
				continue
			}
			set.stats.Candidates++
			if len(set.items) >= opt.MaxSample {
				continue
			}
			s, why := fetchSample(ctx, opt, row)
			switch why {
			case fetchOK:
				set.items = append(set.items, s)
			case fetchEvicted:
				// The buffer dropped it between the activity read and the
				// fetch. FIFO eviction proceeds during a harvest, so a long
				// harvest races its own buffer. Counted, never retried:
				// retrying asks for a row that is by definition older than
				// the one that displaced it.
				set.stats.Evicted++
			case fetchRefused:
				set.stats.Refused++
				if set.stats.RefusedStatus == 0 {
					set.stats.RefusedStatus = s.status
				}
			case fetchMalformed:
				set.stats.Malformed++
			}
		}
		if page >= p.TotalPages {
			break
		}
	}
	set.stats.Replayable = len(set.items)
	set.stats.Floor = opt.RateFloor
	if len(set.items) == 0 {
		// Every arithmetic term is named, including the two a review found
		// missing. Without `refused` in this sentence, a cell that keys its
		// own llama-swap — the case C15 exists for, and the one
		// swapauth.go's own comment says matters — answers 401 to every
		// capture fetch and the operator is told the box has served nothing
		// recently. That is a fact about the WORKLOAD invented out of a
		// credential problem: the most-repeated defect class in this repo,
		// in the sentence an operator acts on.
		if set.stats.Refused > 0 {
			return nil, fmt.Errorf("%w — but note that %d of %d capture fetch(es) were REFUSED with HTTP %d rather than answered, so this is a statement about what this process could read and not about what the cell has served. A cell that keys its own llama-swap needs the key; this command sends none (C15, C18 §8)",
				ErrNoCaptures, set.stats.Refused, set.stats.Candidates, set.stats.RefusedStatus)
		}
		return nil, fmt.Errorf("%w (walked %d row(s) on %s: %d advertised a capture, %d had been evicted, %d could not be read, %d were not replayable chat requests)",
			ErrNoCaptures, set.stats.Walked, model, set.stats.Candidates,
			set.stats.Evicted, set.stats.Malformed, set.stats.SkippedBasis)
	}
	return set, nil
}

// replayablePaths are the endpoints whose captured body this package can
// reconstruct into a request: the OpenAI chat-completions shape, in
// llama-swap's two spellings of it (the /v/ form is its versionless route,
// stripped before forwarding upstream).
//
// Nothing else. The replay POSTs to /v1/chat/completions and rewrites two
// keys of an object that must carry `messages`; a text-completion body or
// an Anthropic-shaped one is not that object, and pretending otherwise
// scores the harness's own inability as the model's.
var replayablePaths = map[string]bool{
	"/v1/chat/completions": true,
	"/v/chat/completions":  true,
}

func replayable(reqPath string) bool {
	p := reqPath
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	if rest, ok := strings.CutPrefix(p, "/upstream/"); ok {
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			p = rest[i:]
		}
	}
	return replayablePaths[p]
}

type fetchOutcome int

const (
	fetchOK fetchOutcome = iota
	fetchEvicted
	// fetchRefused: the capture endpoint answered a non-200 that is not a
	// 404 — a 401 on a cell that keys its own llama-swap, a 500, anything
	// else. It is kept apart from fetchMalformed because the two need
	// different sentences: one says "this box will not let me read them",
	// the other says "I read it and could not use it", and folding both
	// into "there is nothing to replay" tells the operator a falsehood
	// about their own workload.
	fetchRefused
	fetchMalformed
)

// fetchSample performs the one GET that reads private traffic and reduces
// it immediately. The capture object does not outlive this function; only
// the request body (which has to be replayed) and two shapes do.
func fetchSample(ctx context.Context, opt Options, row activityRow) (sample, fetchOutcome) {
	ctx, cancel := context.WithTimeout(ctx, captureFetchTimeout)
	defer cancel()
	u := fmt.Sprintf("%s/api/captures/%d", strings.TrimRight(opt.LlamaSwapURL, "/"), row.ID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return sample{}, fetchMalformed
	}
	resp, err := opt.HTTP.Do(req)
	if err != nil {
		return sample{}, fetchMalformed
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return sample{}, fetchEvicted
	}
	if resp.StatusCode != http.StatusOK {
		// Note what is NOT here: the body. A non-200 from the capture
		// endpoint is reported by activity id and status and nothing else,
		// because everything that endpoint says is about a capture.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return sample{status: resp.StatusCode}, fetchRefused
	}
	var payload capturePayload
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxCaptureBytes)).Decode(&payload); err != nil {
		return sample{}, fetchMalformed
	}
	facts, ok := extractRequest(payload.ReqBody)
	if !ok {
		return sample{}, fetchMalformed
	}
	return sample{
		activityID: row.ID,
		reqBody:    payload.ReqBody,
		facts:      facts,
		recorded:   shapeOfRecorded(row.Status, payload.RespHeaders["content-type"], payload.RespBody, facts),
	}, fetchOK
}

func fetchActivityPage(ctx context.Context, opt Options, page int) (activityPage, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	u := fmt.Sprintf("%s/api/metrics/activity?sort=id&order=desc&limit=%d&page=%d",
		strings.TrimRight(opt.LlamaSwapURL, "/"), activityLimit, page)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return activityPage{}, err
	}
	resp, err := opt.HTTP.Do(req)
	if err != nil {
		return activityPage{}, fmt.Errorf("GET the cell's activity log: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Same unauthenticated-loopback posture C8's cell-side prober and
		// C18 hold: the llama-swap credential is declared per CELL in
		// fleetd's hosts.yaml and a cell need not hold that file. A 401
		// here is reported as itself rather than papered over.
		return activityPage{}, fmt.Errorf("GET the cell's activity log: HTTP %d (a cell that keys its own llama-swap needs the key; see C15)", resp.StatusCode)
	}
	var p activityPage
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&p); err != nil {
		return activityPage{}, fmt.Errorf("decode the cell's activity page: %w", err)
	}
	return p, nil
}
