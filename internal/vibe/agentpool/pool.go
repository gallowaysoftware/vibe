// Package agentpool serves virtual model ids backed by a pool of member
// models, each with a fixed number of slots. It exists for coding-agent
// subagents: a harness asks for "any free executor" by one id, and the pool
// hands each conversation to whichever member has room.
//
// It is the deliberate front shim AGENTS.md reserves for router gaps, not a
// daemon feature: it runs as its own process, only requests that name a pool
// id pass through it, and everything else keeps talking to the front
// directly. See docs/design/router-lifecycle.md §18.
package agentpool

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrNoMembers means every member is marked down. It is returned at once
// rather than queued: waiting cannot make a down cell come back, and a
// harness retrying a fast error recovers sooner than one stuck in a queue.
var ErrNoMembers = errors.New("agentpool: every member is down")

type member struct {
	model     string
	slots     int
	weight    int // relative speed; only shapes placement once every slot is owned
	busy      int // requests in flight
	downUntil time.Time
}

// pin binds a conversation to a member. last is the end of its most recent
// request, or its start while one is in flight.
type pin struct {
	m        *member
	last     time.Time
	inflight int
}

// Pool schedules conversations onto members.
//
// A slot means two things. It bounds requests in flight, which is what the
// member's server can actually run at once. And it is owned by a
// conversation for as long as that conversation is active: an agent spends
// much of a task between requests, running the tool its last reply asked
// for, and a new conversation must not take the member during that gap and
// then trade turns with it while another member sits idle.
type Pool struct {
	ID      string
	Context int

	mu       sync.Mutex
	members  []*member
	pins     map[string]*pin
	wake     chan struct{}
	waiting  int
	ttl      time.Duration
	idle     time.Duration
	cooldown time.Duration
	patience time.Duration
	now      func() time.Time
}

// MemberSpec is one member of a pool.
type MemberSpec struct {
	Model string
	Slots int
	// Weight is the member's speed relative to the others; zero means 1. It
	// decides where conversations go once every slot is owned.
	Weight int
}

// Timing holds a pool's durations.
type Timing struct {
	// TTL bounds how long an idle conversation keeps its member for its own
	// next request.
	TTL time.Duration
	// Idle is how long after its last request a conversation stops owning a
	// slot that new conversations would otherwise take.
	Idle time.Duration
	// Cooldown is how long a member stays down after a failed dispatch.
	Cooldown time.Duration
	// Patience bounds how long a new conversation waits for a faster busy
	// member before taking a free slower one.
	Patience time.Duration
}

// NewPool builds a pool. Members are listed in preference order.
func NewPool(id string, context int, members []MemberSpec, t Timing) *Pool {
	p := &Pool{
		ID:       id,
		Context:  context,
		pins:     map[string]*pin{},
		wake:     make(chan struct{}),
		ttl:      t.TTL,
		idle:     t.Idle,
		cooldown: t.Cooldown,
		patience: t.Patience,
		now:      time.Now,
	}
	for _, s := range members {
		w := s.Weight
		if w < 1 {
			w = 1
		}
		p.members = append(p.members, &member{model: s.Model, slots: s.Slots, weight: w})
	}
	return p
}

// Lease is one granted request slot. Exactly one of Release or ReleaseDown
// must be called; later calls are no-ops.
type Lease struct {
	p    *Pool
	m    *member
	pin  *pin
	key  string
	once sync.Once
}

// Model is the member model id the request must be rewritten to.
func (l *Lease) Model() string { return l.m.model }

// Release returns the slot when the request has finished.
func (l *Lease) Release() {
	l.once.Do(func() {
		l.p.mu.Lock()
		l.m.busy--
		if l.pin != nil {
			l.pin.inflight--
			l.pin.last = l.p.now()
		}
		l.p.broadcastLocked()
		l.p.mu.Unlock()
	})
}

// ReleaseDown returns the slot and marks the member down for the cooldown.
// The conversation's pin is dropped so its next request is placed afresh.
func (l *Lease) ReleaseDown() {
	l.once.Do(func() {
		l.p.mu.Lock()
		l.m.busy--
		l.m.downUntil = l.p.now().Add(l.p.cooldown)
		if l.pin != nil {
			l.pin.inflight--
			if l.p.pins[l.key] == l.pin {
				delete(l.p.pins, l.key)
			}
		}
		l.p.broadcastLocked()
		l.p.mu.Unlock()
	})
}

// Acquire blocks until a request slot is granted for the conversation
// identified by key, ctx ends, or every member is down. An empty key has no
// affinity and owns nothing.
//
// A pinned conversation waits for its own member rather than moving to a
// free one: switching members mid-task changes the model under the agent and
// discards the member's prompt cache, which costs more than the wait. It only
// moves when its member is down.
func (p *Pool) Acquire(ctx context.Context, key string) (*Lease, error) {
	p.mu.Lock()
	// One agent conversation never has two requests in flight: it waits for
	// each reply before sending the next. A second one under a key that is
	// mid-request is a different conversation whose opening happens to match
	// (a harness that sends a fixed preamble as the first user message), and
	// pinning it would serialise exactly the fan-out the pool exists to
	// spread. It is placed with no affinity instead.
	if pn, ok := p.pins[key]; ok && pn.inflight > 0 {
		key = ""
	}
	patientUntil := p.now().Add(p.patience)
	for {
		now := p.now()
		p.evictLocked(now)
		m, err := p.chooseLocked(key, now, now.Before(patientUntil))
		if err != nil {
			p.mu.Unlock()
			return nil, err
		}
		if m != nil {
			m.busy++
			l := &Lease{p: p, m: m, key: key}
			if key != "" {
				pn := p.pins[key]
				if pn == nil || pn.m != m {
					pn = &pin{m: m}
					p.pins[key] = pn
				}
				pn.inflight++
				pn.last = now
				l.pin = pn
			}
			p.mu.Unlock()
			return l, nil
		}
		wake := p.wake
		retry := p.nextChangeLocked(now)
		if patientUntil.After(now) && (retry.IsZero() || patientUntil.Before(retry)) {
			retry = patientUntil
		}
		p.waiting++
		p.mu.Unlock()

		var t *time.Timer
		var timer <-chan time.Time
		if !retry.IsZero() {
			t = time.NewTimer(retry.Sub(now))
			timer = t.C
		}
		select {
		case <-ctx.Done():
		case <-wake:
		case <-timer:
		}
		if t != nil {
			t.Stop()
		}
		p.mu.Lock()
		p.waiting--
		if err := ctx.Err(); err != nil {
			p.mu.Unlock()
			return nil, err
		}
	}
}

// chooseLocked returns the member to grant, nil to wait, or ErrNoMembers.
// patient allows a new conversation to wait for a faster busy member.
func (p *Pool) chooseLocked(key string, now time.Time, patient bool) (*member, error) {
	if pn, ok := p.pins[key]; ok && key != "" {
		if now.Before(pn.m.downUntil) {
			delete(p.pins, key)
		} else {
			if pn.m.busy < pn.m.slots {
				return pn.m, nil
			}
			return nil, nil
		}
	}
	owners := p.ownersLocked(now, key)
	var up []*member
	for _, m := range p.members {
		if !now.Before(m.downUntil) {
			up = append(up, m)
		}
	}
	if len(up) == 0 {
		return nil, ErrNoMembers
	}
	// First choice: a member with unowned capacity, in preference order.
	for _, m := range up {
		if owners[m] < m.slots && m.busy < m.slots {
			return m, nil
		}
	}
	// Every slot is owned, and a conversation stays where it lands, so place
	// it on the member furthest below its share: conversations per slot per
	// unit of weight. A conversation that will be pinned waits, for up to
	// the patience bound, when that member is mid-request and strictly faster
	// than the best free one. Unpinned requests never wait: they add no
	// owner, so every one of them would choose, and queue on, the same member.
	better := func(a, b *member) bool {
		return owners[a]*b.slots*b.weight < owners[b]*a.slots*a.weight
	}
	var best, bestFree *member
	for _, m := range up {
		if best == nil || better(m, best) {
			best = m
		}
		if m.busy < m.slots && (bestFree == nil || better(m, bestFree)) {
			bestFree = m
		}
	}
	if best.busy < best.slots {
		return best, nil
	}
	faster := bestFree == nil || best.weight*bestFree.slots > bestFree.weight*best.slots
	if key != "" && patient && faster && (bestFree == nil || better(best, bestFree)) {
		return nil, nil
	}
	return bestFree, nil
}

// ownersLocked counts the active conversations on each member, other than
// the one asking.
func (p *Pool) ownersLocked(now time.Time, except string) map[*member]int {
	owners := map[*member]int{}
	for k, pn := range p.pins {
		if k == except {
			continue
		}
		if pn.inflight > 0 || now.Sub(pn.last) < p.idle {
			owners[pn.m]++
		}
	}
	return owners
}

// nextChangeLocked is the earliest moment a waiter's answer can change
// without any slot being released: a down member coming back, or an owned
// slot going idle. A waiter must wake then, or a pool whose only capacity is
// behind a cooldown or an idle conversation would sleep through it.
func (p *Pool) nextChangeLocked(now time.Time) time.Time {
	var next time.Time
	consider := func(t time.Time) {
		if t.After(now) && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}
	for _, m := range p.members {
		consider(m.downUntil)
	}
	for _, pn := range p.pins {
		if pn.inflight == 0 {
			consider(pn.last.Add(p.idle))
		}
	}
	return next
}

func (p *Pool) evictLocked(now time.Time) {
	for k, pn := range p.pins {
		if pn.inflight == 0 && now.Sub(pn.last) > p.ttl {
			delete(p.pins, k)
		}
	}
}

func (p *Pool) broadcastLocked() {
	close(p.wake)
	p.wake = make(chan struct{})
}

// MemberStatus is one member's state for /status.
type MemberStatus struct {
	Model  string `json:"model"`
	Slots  int    `json:"slots"`
	Weight int    `json:"weight"`
	Busy   int    `json:"busy"`
	Owners int    `json:"active_conversations"`
	Down   bool   `json:"down"`
}

// Status is a pool's state for /status.
type Status struct {
	ID      string         `json:"id"`
	Members []MemberStatus `json:"members"`
	Pinned  int            `json:"pinned_conversations"`
	Waiting int            `json:"waiting"`
}

// Status reports the pool's current state.
func (p *Pool) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	owners := p.ownersLocked(now, "")
	s := Status{ID: p.ID, Pinned: len(p.pins), Waiting: p.waiting}
	for _, m := range p.members {
		s.Members = append(s.Members, MemberStatus{
			Model: m.model, Slots: m.slots, Weight: m.weight, Busy: m.busy, Owners: owners[m], Down: now.Before(m.downUntil),
		})
	}
	return s
}
