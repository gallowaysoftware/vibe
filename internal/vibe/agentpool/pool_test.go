package agentpool

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func testPool(t *testing.T, members ...MemberSpec) (*Pool, *fakeClock) {
	t.Helper()
	clk := &fakeClock{t: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	p := NewPool("agent-exec", 131072, members, Timing{TTL: time.Hour, Idle: 2 * time.Minute, Cooldown: 30 * time.Second})
	p.now = clk.now
	return p, clk
}

func mustAcquire(t *testing.T, p *Pool, key string) *Lease {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	l, err := p.Acquire(ctx, key)
	if err != nil {
		t.Fatalf("Acquire(%q): %v", key, err)
	}
	return l
}

// acquireAsync starts an Acquire and returns a channel that yields its lease.
func acquireAsync(p *Pool, ctx context.Context, key string) <-chan *Lease {
	ch := make(chan *Lease, 1)
	go func() {
		l, err := p.Acquire(ctx, key)
		if err != nil {
			close(ch)
			return
		}
		ch <- l
	}()
	return ch
}

func waitForWaiters(t *testing.T, p *Pool, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for p.Status().Waiting != n {
		if time.Now().After(deadline) {
			t.Fatalf("waiting = %d, want %d", p.Status().Waiting, n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPool_NewConversationsFillMembersInConfiguredOrder(t *testing.T) {
	p, _ := testPool(t, MemberSpec{"fast", 1}, MemberSpec{"slow", 1})
	a := mustAcquire(t, p, "conv-a")
	b := mustAcquire(t, p, "conv-b")
	if a.Model() != "fast" || b.Model() != "slow" {
		t.Fatalf("got %s, %s; want fast then slow", a.Model(), b.Model())
	}
}

func TestPool_ConversationStaysOnItsMemberWhenAPreferredOneFrees(t *testing.T) {
	p, _ := testPool(t, MemberSpec{"fast", 1}, MemberSpec{"slow", 1})
	hold := mustAcquire(t, p, "conv-a") // fast
	b := mustAcquire(t, p, "conv-b")    // slow, the only free one
	hold.Release()
	b.Release()
	if l := mustAcquire(t, p, "conv-b"); l.Model() != "slow" {
		t.Fatalf("conv-b moved to %s; it must keep its member and prompt cache", l.Model())
	}
}

func TestPool_PinnedConversationWaitsForItsMemberRatherThanMoving(t *testing.T) {
	p, _ := testPool(t, MemberSpec{"fast", 1}, MemberSpec{"slow", 1})
	first := mustAcquire(t, p, "conv-a") // pins conv-a to fast
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := acquireAsync(p, ctx, "conv-a")
	waitForWaiters(t, p, 1)
	if s := p.Status(); s.Members[1].Busy != 0 {
		t.Fatalf("pinned request took the free slow member: %+v", s.Members)
	}
	first.Release()
	select {
	case l := <-got:
		if l == nil || l.Model() != "fast" {
			t.Fatalf("pinned conversation got %v, want fast", l)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pinned conversation never got its member back")
	}
}

func TestPool_NewConversationQueuesUntilAnySlotFrees(t *testing.T) {
	p, _ := testPool(t, MemberSpec{"fast", 1}, MemberSpec{"slow", 1})
	mustAcquire(t, p, "conv-a")
	b := mustAcquire(t, p, "conv-b")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := acquireAsync(p, ctx, "conv-c")
	waitForWaiters(t, p, 1)
	b.Release()
	select {
	case l := <-got:
		if l == nil || l.Model() != "slow" {
			t.Fatalf("queued conversation got %v, want slow", l)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued conversation was never granted the freed slot")
	}
}

func TestPool_QueuedRequestReturnsWhenItsContextEnds(t *testing.T) {
	p, _ := testPool(t, MemberSpec{"only", 1})
	mustAcquire(t, p, "conv-a")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := p.Acquire(ctx, "conv-b"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if w := p.Status().Waiting; w != 0 {
		t.Fatalf("waiting = %d after the waiter left", w)
	}
}

func TestPool_DownMemberIsSkippedAndItsConversationsMove(t *testing.T) {
	p, _ := testPool(t, MemberSpec{"fast", 1}, MemberSpec{"slow", 1})
	mustAcquire(t, p, "conv-a").ReleaseDown() // fast failed for conv-a
	if s := p.Status(); !s.Members[0].Down {
		t.Fatalf("fast not marked down: %+v", s.Members)
	}
	a := mustAcquire(t, p, "conv-a")
	if a.Model() != "slow" {
		t.Fatalf("conv-a got %s after fast went down, want slow", a.Model())
	}
}

func TestPool_EveryMemberDownFailsFastInsteadOfQueueing(t *testing.T) {
	p, _ := testPool(t, MemberSpec{"fast", 1}, MemberSpec{"slow", 1})
	mustAcquire(t, p, "a").ReleaseDown()
	mustAcquire(t, p, "b").ReleaseDown()
	if _, err := p.Acquire(context.Background(), "c"); !errors.Is(err, ErrNoMembers) {
		t.Fatalf("err = %v, want ErrNoMembers", err)
	}
}

func TestPool_DownMemberServesAgainAfterTheCooldown(t *testing.T) {
	p, clk := testPool(t, MemberSpec{"fast", 1}, MemberSpec{"slow", 1})
	mustAcquire(t, p, "a").ReleaseDown()
	clk.advance(31 * time.Second)
	if l := mustAcquire(t, p, "b"); l.Model() != "fast" {
		t.Fatalf("got %s after cooldown, want fast", l.Model())
	}
}

func TestPool_WaiterWakesWhenACooldownEndsWithoutAnyRelease(t *testing.T) {
	p := NewPool("agent-exec", 0, []MemberSpec{{"fast", 1}, {"slow", 1}}, Timing{TTL: time.Hour, Idle: time.Hour, Cooldown: 50 * time.Millisecond})
	mustAcquire(t, p, "a").ReleaseDown() // fast down for 50ms
	mustAcquire(t, p, "b")               // slow busy, never released
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	l, err := p.Acquire(ctx, "c")
	if err != nil || l.Model() != "fast" {
		t.Fatalf("got %v, %v; want fast once its cooldown ends", l, err)
	}
}

func TestPool_IdleConversationLosesItsPinAfterTheTTL(t *testing.T) {
	p, clk := testPool(t, MemberSpec{"fast", 1}, MemberSpec{"slow", 1})
	mustAcquire(t, p, "conv-a").Release()
	clk.advance(2 * time.Hour)
	mustAcquire(t, p, "conv-b").Release()
	if n := p.Status().Pinned; n != 1 {
		t.Fatalf("pinned = %d; conv-a's expired pin should be gone, leaving conv-b's", n)
	}
}

func TestPool_NewConversationAvoidsAMemberWhoseConversationIsBetweenRequests(t *testing.T) {
	p, _ := testPool(t, MemberSpec{"fast", 1}, MemberSpec{"slow", 1})
	mustAcquire(t, p, "conv-a").Release() // conv-a is running a tool on fast
	if l := mustAcquire(t, p, "conv-b"); l.Model() != "slow" {
		t.Fatalf("conv-b took %s while conv-a owns it between requests; want slow", l.Model())
	}
}

func TestPool_ConversationIdlePastTheWindowFreesItsMember(t *testing.T) {
	p, clk := testPool(t, MemberSpec{"fast", 1}, MemberSpec{"slow", 1})
	mustAcquire(t, p, "conv-a").Release()
	clk.advance(3 * time.Minute)
	if l := mustAcquire(t, p, "conv-b"); l.Model() != "fast" {
		t.Fatalf("conv-b got %s; conv-a went idle, so fast is free again", l.Model())
	}
}

func TestPool_WhenEveryMemberIsOwnedNewConversationsShareTheLeastLoaded(t *testing.T) {
	p, _ := testPool(t, MemberSpec{"fast", 1}, MemberSpec{"slow", 1})
	mustAcquire(t, p, "a").Release() // fast
	mustAcquire(t, p, "b").Release() // slow
	c := mustAcquire(t, p, "c")      // tie: preference order, fast
	c.Release()
	d := mustAcquire(t, p, "d") // fast now has two, slow one
	if c.Model() != "fast" || d.Model() != "slow" {
		t.Fatalf("c=%s d=%s; want fast then slow", c.Model(), d.Model())
	}
}

func TestPool_StatusCountsConversationsOwningEachMember(t *testing.T) {
	p, _ := testPool(t, MemberSpec{"fast", 2})
	mustAcquire(t, p, "a").Release()
	mustAcquire(t, p, "b")
	if s := p.Status().Members[0]; s.Owners != 2 || s.Busy != 1 {
		t.Fatalf("status %+v; want 2 active conversations, 1 busy", s)
	}
}

func TestPool_EmptyKeyHasNoAffinity(t *testing.T) {
	p, _ := testPool(t, MemberSpec{"fast", 1}, MemberSpec{"slow", 1})
	mustAcquire(t, p, "").Release()
	if n := p.Status().Pinned; n != 0 {
		t.Fatalf("pinned = %d for an empty key", n)
	}
}

func TestLease_ReleaseIsIdempotent(t *testing.T) {
	p, _ := testPool(t, MemberSpec{"only", 2})
	l := mustAcquire(t, p, "a")
	l.Release()
	l.Release()
	l.ReleaseDown()
	if s := p.Status(); s.Members[0].Busy != 0 || s.Members[0].Down {
		t.Fatalf("double release corrupted state: %+v", s.Members)
	}
}
