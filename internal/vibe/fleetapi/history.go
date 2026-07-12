package fleetapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// historyPerModelCap bounds the persisted start-duration samples per model.
// 50 is enough for a stable p50 while keeping the file small and the JSON
// rewrite-on-record cheap.
const historyPerModelCap = 50

type startEntry struct {
	At      time.Time `json:"at"`
	Seconds float64   `json:"seconds"`
}

// StartStats is the per-model aggregate exposed in /api/fleet/state. The web
// UI uses p50 as the honest ETA for a cold start in progress.
type StartStats struct {
	Count int     `json:"count"`
	LastS float64 `json:"last_s"`
	P50S  float64 `json:"p50_s"`
	MaxS  float64 `json:"max_s"`
}

// history is the rolling starting→ready duration store, persisted as JSON so
// ETAs survive daemon restarts. Every Record rewrites the file — starts are
// rare (seconds-to-minutes apart at best), so simplicity beats batching.
type history struct {
	mu      sync.Mutex
	path    string
	entries map[string][]startEntry
}

func loadHistory(path string) *history {
	h := &history{path: path, entries: map[string][]startEntry{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return h
	}
	if err != nil {
		slog.Warn("fleetapi: read start history; starting empty", "path", path, "err", err)
		return h
	}
	var onDisk struct {
		Models map[string][]startEntry `json:"models"`
	}
	if err := json.Unmarshal(data, &onDisk); err != nil {
		slog.Warn("fleetapi: parse start history; starting empty", "path", path, "err", err)
		return h
	}
	if onDisk.Models != nil {
		h.entries = onDisk.Models
	}
	return h
}

func (h *history) Record(model string, at time.Time, seconds float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	list := append(h.entries[model], startEntry{At: at.UTC(), Seconds: seconds})
	if len(list) > historyPerModelCap {
		list = list[len(list)-historyPerModelCap:]
	}
	h.entries[model] = list
	h.saveLocked()
}

// saveLocked persists via temp-file + rename so a crash mid-write can't
// truncate the history. Failures are logged, not fatal: the in-memory copy
// stays authoritative for this daemon's lifetime.
func (h *history) saveLocked() {
	payload := struct {
		Models map[string][]startEntry `json:"models"`
	}{Models: h.entries}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		slog.Warn("fleetapi: marshal start history", "err", err)
		return
	}
	dir := filepath.Dir(h.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("fleetapi: mkdir start history dir", "dir", dir, "err", err)
		return
	}
	tmp, err := os.CreateTemp(dir, ".start-history-*")
	if err != nil {
		slog.Warn("fleetapi: create start history tmp", "err", err)
		return
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		slog.Warn("fleetapi: write start history tmp", "err", err)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		slog.Warn("fleetapi: close start history tmp", "err", err)
		return
	}
	if err := os.Rename(tmpPath, h.path); err != nil {
		_ = os.Remove(tmpPath)
		slog.Warn("fleetapi: rename start history", "err", err)
	}
}

func (h *history) Stats() map[string]StartStats {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]StartStats, len(h.entries))
	for model, list := range h.entries {
		if len(list) == 0 {
			continue
		}
		secs := make([]float64, len(list))
		for i, e := range list {
			secs[i] = e.Seconds
		}
		sort.Float64s(secs)
		var p50 float64
		if n := len(secs); n%2 == 1 {
			p50 = secs[n/2]
		} else {
			p50 = (secs[n/2-1] + secs[n/2]) / 2
		}
		out[model] = StartStats{
			Count: len(list),
			LastS: list[len(list)-1].Seconds,
			P50S:  p50,
			MaxS:  secs[len(secs)-1],
		}
	}
	return out
}
