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
	for page := 1; page <= opt.MaxPages && len(set.items) < opt.MaxSample; page++ {
		p, err := fetchActivityPage(ctx, opt, page)
		if err != nil {
			return nil, err
		}
		if len(p.Data) == 0 {
			break
		}
		for _, row := range p.Data {
			set.stats.Walked++
			if row.Model != model || !row.HasCapture {
				continue
			}
			// The basis branch is usagemeter's, on req_path and never on
			// backend kind: only the chat family has the tool-call and
			// finish-reason structure this package scores.
			if usagemeter.BasisFor(row.ReqPath) != usagemeter.BasisChat {
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
		return nil, fmt.Errorf("%w (walked %d row(s) on %s: %d advertised a capture, %d had been evicted, %d were not chat requests)",
			ErrNoCaptures, set.stats.Walked, model, set.stats.Candidates, set.stats.Evicted, set.stats.SkippedBasis)
	}
	return set, nil
}

type fetchOutcome int

const (
	fetchOK fetchOutcome = iota
	fetchEvicted
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
		return sample{}, fetchMalformed
	}
	var cap capturePayload
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxCaptureBytes)).Decode(&cap); err != nil {
		return sample{}, fetchMalformed
	}
	facts, ok := extractRequest(cap.ReqBody)
	if !ok {
		return sample{}, fetchMalformed
	}
	return sample{
		activityID: row.ID,
		reqBody:    cap.ReqBody,
		facts:      facts,
		recorded:   shapeOfRecorded(row.Status, cap.RespBody, facts),
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
