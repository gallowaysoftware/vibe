// Package agentpool serves virtual model ids backed by a pool of member
// models, each with a fixed number of slots. It exists for coding-agent
// subagents: a harness asks for "any free executor" by one id, and the pool
// hands each conversation to whichever member has a free slot.
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
	busy      int
	downUntil time.Time
}

type pin struct {
	m    *member
	last time.Time
}

// Pool schedules conversations onto members. Members are tried in
// configuration order, so the first listed is preferred when several are
// free.
type Pool struct {
	ID      string
	Context int

	mu       sync.Mutex
	members  []*member
	pins     map[string]*pin
	wake     chan struct{}
	waiting  int
	ttl      time.Duration
	cooldown time.Duration
	now      func() time.Time
}

// MemberSpec is one member of a pool.
type MemberSpec struct {
	Model string
	Slots int
}

// NewPool builds a pool. ttl bounds how long an idle conversation keeps its
// member; cooldown is how long a member stays down after a failed dispatch.
func NewPool(id string, context int, members []MemberSpec, ttl, cooldown time.Duration) *Pool {
	p := &Pool{
		ID:       id,
		Context:  context,
		pins:     map[string]*pin{},
		wake:     make(chan struct{}),
		ttl:      ttl,
		cooldown: cooldown,
		now:      time.Now,
	}
	for _, s := range members {
		p.members = append(p.members, &member{model: s.Model, slots: s.Slots})
	}
	return p
}

// Lease is one granted slot. Exactly one of Release or ReleaseDown must be
// called.
type Lease struct {
	p    *Pool
	m    *member
	key  string
	once sync.Once
}

// Model is the member model id the request must be rewritten to.
func (l *Lease) Model() string { return l.m.model }

// Release returns the slot.
func (l *Lease) Release() {
	l.once.Do(func() {
		l.p.mu.Lock()
		l.m.busy--
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
		if pn, ok := l.p.pins[l.key]; ok && pn.m == l.m {
			delete(l.p.pins, l.key)
		}
		l.p.broadcastLocked()
		l.p.mu.Unlock()
	})
}

// Acquire blocks until a slot is granted for the conversation identified by
// key, ctx ends, or every member is down. An empty key has no affinity.
//
// A pinned conversation waits for its own member rather than moving to a
// free one: switching members mid-task changes the model under the agent and
// discards the member's prompt cache, which costs more than the wait. It only
// moves when its member is down.
func (p *Pool) Acquire(ctx context.Context, key string) (*Lease, error) {
	p.mu.Lock()
	for {
		now := p.now()
		p.evictLocked(now)
		m, err := p.chooseLocked(key, now)
		if err != nil {
			p.mu.Unlock()
			return nil, err
		}
		if m != nil {
			m.busy++
			if key != "" {
				p.pins[key] = &pin{m: m, last: now}
			}
			p.mu.Unlock()
			return &Lease{p: p, m: m, key: key}, nil
		}
		wake := p.wake
		retry := p.nextUpLocked(now)
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
func (p *Pool) chooseLocked(key string, now time.Time) (*member, error) {
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
	up := 0
	for _, m := range p.members {
		if now.Before(m.downUntil) {
			continue
		}
		up++
		if m.busy < m.slots {
			return m, nil
		}
	}
	if up == 0 {
		return nil, ErrNoMembers
	}
	return nil, nil
}

// nextUpLocked is when the earliest down member comes back, or zero. A
// waiter must wake then even if no slot is released, or a pool whose only
// free capacity is behind a cooldown would sleep through it.
func (p *Pool) nextUpLocked(now time.Time) time.Time {
	var next time.Time
	for _, m := range p.members {
		if now.Before(m.downUntil) && (next.IsZero() || m.downUntil.Before(next)) {
			next = m.downUntil
		}
	}
	return next
}

func (p *Pool) evictLocked(now time.Time) {
	for k, pn := range p.pins {
		if now.Sub(pn.last) > p.ttl {
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
	Model string `json:"model"`
	Slots int    `json:"slots"`
	Busy  int    `json:"busy"`
	Down  bool   `json:"down"`
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
	s := Status{ID: p.ID, Pinned: len(p.pins), Waiting: p.waiting}
	for _, m := range p.members {
		s.Members = append(s.Members, MemberStatus{Model: m.model, Slots: m.slots, Busy: m.busy, Down: now.Before(m.downUntil)})
	}
	return s
}
