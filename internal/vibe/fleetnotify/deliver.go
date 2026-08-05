package fleetnotify

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// The delivery worker. Two properties are load-bearing and both exist
// because fleetd must survive its notifier:
//
//   - Enqueue never blocks. A wedged webhook must not stall the
//     evaluation loop, and a full queue is a counted drop, not a pause.
//   - Every wait is bound to the caller's context. Run is started on the
//     fleet server's WaitGroup, so an unlinked backoff sleep or an
//     unlinked HTTP timeout would hold Close() open for as long as the
//     far side wants — the exact failure C4's warmCtx exists to prevent.
const (
	defaultQueueSize   = 64
	defaultAttempts    = 4
	defaultBaseBackoff = time.Second
)

// DeliveryStats is the delivery half of the fleet_status notify block.
type DeliveryStats struct {
	Sent    int64 `json:"sent,omitempty"`
	Failed  int64 `json:"failed,omitempty"`
	Retries int64 `json:"retries,omitempty"`
	// Dropped counts notifications the queue could not accept.
	Dropped   int64      `json:"dropped,omitempty"`
	Queued    int        `json:"queued,omitempty"`
	LastSend  *time.Time `json:"last_send,omitempty"`
	LastError string     `json:"last_error,omitempty"`
}

// Deliverer owns the outbound queue and the retry policy.
type Deliverer struct {
	sink     Sink
	q        chan Notification
	attempts int
	backoff  time.Duration

	mu    sync.Mutex
	stats DeliveryStats
}

// DelivererConfig tunes the worker; zero values take the defaults.
type DelivererConfig struct {
	QueueSize int
	Attempts  int
	// Backoff is the first retry delay; each subsequent attempt doubles.
	Backoff time.Duration
}

// NewDeliverer builds the worker over a sink.
func NewDeliverer(sink Sink, cfg DelivererConfig) *Deliverer {
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueueSize
	}
	if cfg.Attempts <= 0 {
		cfg.Attempts = defaultAttempts
	}
	if cfg.Backoff <= 0 {
		cfg.Backoff = defaultBaseBackoff
	}
	return &Deliverer{
		sink:     sink,
		q:        make(chan Notification, cfg.QueueSize),
		attempts: cfg.Attempts,
		backoff:  cfg.Backoff,
	}
}

// Endpoint is the sink's redacted description.
func (d *Deliverer) Endpoint() string { return d.sink.Endpoint() }

// Enqueue hands a notification to the worker. It never blocks; a full
// queue counts a drop and says so, because a notifier that applies
// back-pressure to the thing it is watching is a notifier that takes the
// fleet down with it.
func (d *Deliverer) Enqueue(n Notification) bool {
	select {
	case d.q <- n:
		return true
	default:
		d.mu.Lock()
		d.stats.Dropped++
		d.mu.Unlock()
		slog.Warn("fleet notify queue full; notification dropped", "state", n.State, "key", DisplayKey(n.Key))
		return false
	}
}

// Run delivers until ctx is cancelled. Undelivered queue entries are
// abandoned at cancellation by design: the alternative is a shutdown
// that waits on a far side which may never answer.
func (d *Deliverer) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case n := <-d.q:
			d.deliver(ctx, n)
		}
	}
}

func (d *Deliverer) deliver(ctx context.Context, n Notification) {
	backoff := d.backoff
	for attempt := 1; ; attempt++ {
		err := d.sink.Send(ctx, n)
		if err == nil {
			now := time.Now().UTC()
			d.mu.Lock()
			d.stats.Sent++
			d.stats.LastSend = &now
			d.stats.LastError = ""
			d.mu.Unlock()
			return
		}
		if attempt >= d.attempts || !Retryable(err) || ctx.Err() != nil {
			d.mu.Lock()
			d.stats.Failed++
			d.stats.LastError = d.scrub(err.Error())
			d.mu.Unlock()
			slog.Warn("fleet notify delivery failed",
				"endpoint", d.sink.Endpoint(), "attempts", attempt, "err", d.scrub(err.Error()))
			return
		}
		d.mu.Lock()
		d.stats.Retries++
		d.mu.Unlock()
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			d.mu.Lock()
			d.stats.Failed++
			d.stats.LastError = "abandoned at shutdown"
			d.mu.Unlock()
			return
		case <-timer.C:
		}
		backoff *= 2
	}
}

// scrub applies the sink's own redaction to a string the Deliverer is
// about to store or log. The sink already scrubs its errors; this is the
// second lock on the same door, because the cost of one leaked topic is
// a rotated credential and the cost of this call is nothing.
func (d *Deliverer) scrub(msg string) string {
	if sc, ok := d.sink.(Scrubber); ok {
		return sc.Scrub(msg)
	}
	return msg
}

// Stats returns a snapshot of the delivery counters.
func (d *Deliverer) Stats() DeliveryStats {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.stats
	st.Queued = len(d.q)
	if d.stats.LastSend != nil {
		t := *d.stats.LastSend
		st.LastSend = &t
	}
	return st
}
