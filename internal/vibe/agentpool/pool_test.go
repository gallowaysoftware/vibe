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
	p := NewPool("agent-exec", 131072, members, Timing{TTL: time.Hour, Idle: 2 * time.Minute, Cooldown: 30 * time.Second, Patience: time.Hour})
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
	p, _ := testPool(t, MemberSpec{Model: "fast", Slots: 1}, MemberSpec{Model: "slow", Slots: 1})
	a := mustAcquire(t, p, "conv-a")
	b := mustAcquire(t, p, "conv-b")
	if a.Model() != "fast" || b.Model() != "slow" {
		t.Fatalf("got %s, %s; want fast then slow", a.Model(), b.Model())
	}
}

func TestPool_ConversationStaysOnItsMemberWhenAPreferredOneFrees(t *testing.T) {
	p, _ := testPool(t, MemberSpec{Model: "fast", Slots: 1}, MemberSpec{Model: "slow", Slots: 1})
	hold := mustAcquire(t, p, "conv-a") // fast
	b := mustAcquire(t, p, "conv-b")    // slow, the only free one
	hold.Release()
	b.Release()
	if l := mustAcquire(t, p, "conv-b"); l.Model() != "slow" {
		t.Fatalf("conv-b moved to %s; it must keep its member and prompt cache", l.Model())
	}
}

func TestPool_PinnedConversationWaitsForItsMemberRatherThanMoving(t *testing.T) {
	p, _ := testPool(t, MemberSpec{Model: "fast", Slots: 1}, MemberSpec{Model: "slow", Slots: 1})
	mustAcquire(t, p, "conv-a").Release() // conv-a owns fast, between requests
	mustAcquire(t, p, "conv-b").Release() // conv-b owns slow
	c := mustAcquire(t, p, "conv-c")      // every slot owned: shares fast
	if c.Model() != "fast" {
		t.Fatalf("conv-c got %s, want fast", c.Model())
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := acquireAsync(p, ctx, "conv-a")
	waitForWaiters(t, p, 1)
	if s := p.Status(); s.Members[1].Busy != 0 {
		t.Fatalf("conv-a moved to the free slow member: %+v", s.Members)
	}
	c.Release()
	select {
	case l := <-got:
		if l == nil || l.Model() != "fast" {
			t.Fatalf("conv-a got %v, want fast", l)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("conv-a never got its member back")
	}
}

func TestPool_NewConversationQueuesUntilAnySlotFrees(t *testing.T) {
	p, _ := testPool(t, MemberSpec{Model: "fast", Slots: 1}, MemberSpec{Model: "slow", Slots: 1})
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
	p, _ := testPool(t, MemberSpec{Model: "only", Slots: 1})
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
	p, _ := testPool(t, MemberSpec{Model: "fast", Slots: 1}, MemberSpec{Model: "slow", Slots: 1})
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
	p, _ := testPool(t, MemberSpec{Model: "fast", Slots: 1}, MemberSpec{Model: "slow", Slots: 1})
	mustAcquire(t, p, "a").ReleaseDown()
	mustAcquire(t, p, "b").ReleaseDown()
	if _, err := p.Acquire(context.Background(), "c"); !errors.Is(err, ErrNoMembers) {
		t.Fatalf("err = %v, want ErrNoMembers", err)
	}
}

func TestPool_DownMemberServesAgainAfterTheCooldown(t *testing.T) {
	p, clk := testPool(t, MemberSpec{Model: "fast", Slots: 1}, MemberSpec{Model: "slow", Slots: 1})
	mustAcquire(t, p, "a").ReleaseDown()
	clk.advance(31 * time.Second)
	if l := mustAcquire(t, p, "b"); l.Model() != "fast" {
		t.Fatalf("got %s after cooldown, want fast", l.Model())
	}
}

func TestPool_WaiterWakesWhenACooldownEndsWithoutAnyRelease(t *testing.T) {
	p := NewPool("agent-exec", 0, []MemberSpec{{Model: "fast", Slots: 1}, {Model: "slow", Slots: 1}}, Timing{TTL: time.Hour, Idle: time.Hour, Cooldown: 50 * time.Millisecond})
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
	p, clk := testPool(t, MemberSpec{Model: "fast", Slots: 1}, MemberSpec{Model: "slow", Slots: 1})
	mustAcquire(t, p, "conv-a").Release()
	clk.advance(2 * time.Hour)
	mustAcquire(t, p, "conv-b").Release()
	if n := p.Status().Pinned; n != 1 {
		t.Fatalf("pinned = %d; conv-a's expired pin should be gone, leaving conv-b's", n)
	}
}

func TestPool_NewConversationAvoidsAMemberWhoseConversationIsBetweenRequests(t *testing.T) {
	p, _ := testPool(t, MemberSpec{Model: "fast", Slots: 1}, MemberSpec{Model: "slow", Slots: 1})
	mustAcquire(t, p, "conv-a").Release() // conv-a is running a tool on fast
	if l := mustAcquire(t, p, "conv-b"); l.Model() != "slow" {
		t.Fatalf("conv-b took %s while conv-a owns it between requests; want slow", l.Model())
	}
}

func TestPool_ConversationIdlePastTheWindowFreesItsMember(t *testing.T) {
	p, clk := testPool(t, MemberSpec{Model: "fast", Slots: 1}, MemberSpec{Model: "slow", Slots: 1})
	mustAcquire(t, p, "conv-a").Release()
	clk.advance(3 * time.Minute)
	if l := mustAcquire(t, p, "conv-b"); l.Model() != "fast" {
		t.Fatalf("conv-b got %s; conv-a went idle, so fast is free again", l.Model())
	}
}

func TestPool_WhenEveryMemberIsOwnedNewConversationsShareTheLeastLoaded(t *testing.T) {
	p, _ := testPool(t, MemberSpec{Model: "fast", Slots: 1}, MemberSpec{Model: "slow", Slots: 1})
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
	p, _ := testPool(t, MemberSpec{Model: "fast", Slots: 2})
	mustAcquire(t, p, "a").Release()
	mustAcquire(t, p, "b")
	if s := p.Status().Members[0]; s.Owners != 2 || s.Busy != 1 {
		t.Fatalf("status %+v; want 2 active conversations, 1 busy", s)
	}
}

func TestPool_SecondRequestUnderAKeyThatIsMidRequestIsPlacedAsANewConversation(t *testing.T) {
	p, _ := testPool(t, MemberSpec{Model: "fast", Slots: 1}, MemberSpec{Model: "slow", Slots: 1})
	first := mustAcquire(t, p, "fixed-preamble") // fast, in flight
	second := mustAcquire(t, p, "fixed-preamble")
	if second.Model() != "slow" {
		t.Fatalf("second conversation sharing an opening got %s; it must not queue behind the first", second.Model())
	}
	first.Release()
	second.Release()
	if l := mustAcquire(t, p, "fixed-preamble"); l.Model() != "fast" {
		t.Fatalf("the key's own pin moved to %s", l.Model())
	}
}

func TestPool_OverflowConversationsSplitByMemberWeight(t *testing.T) {
	p, _ := testPool(t, MemberSpec{Model: "fast", Slots: 1, Weight: 3}, MemberSpec{Model: "slow", Slots: 1, Weight: 1})
	var got []string
	for _, k := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		l := mustAcquire(t, p, k)
		got = append(got, l.Model())
		l.Release()
	}
	// a and b take the unowned slots; after that each new conversation goes
	// to whichever member is furthest below its 3:1 share.
	want := []string{"fast", "slow", "fast", "fast", "fast", "slow", "fast"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("placements %v, want %v", got, want)
		}
	}
}

func TestPool_OverflowWaitsForAFasterMemberRatherThanTakingAFreeSlowOne(t *testing.T) {
	p, _ := testPool(t, MemberSpec{Model: "fast", Slots: 1, Weight: 3}, MemberSpec{Model: "slow", Slots: 1, Weight: 1})
	mustAcquire(t, p, "a").Release() // fast
	mustAcquire(t, p, "b").Release() // slow
	c := mustAcquire(t, p, "c")      // fast, in flight
	if c.Model() != "fast" {
		t.Fatalf("c got %s, want fast", c.Model())
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := acquireAsync(p, ctx, "d")
	waitForWaiters(t, p, 1)
	if s := p.Status(); s.Members[1].Busy != 0 {
		t.Fatalf("d took the free slow member while fast is far below its share: %+v", s.Members)
	}
	c.Release()
	select {
	case l := <-got:
		if l == nil || l.Model() != "fast" {
			t.Fatalf("d got %v, want fast", l)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("d never got the fast member")
	}
}

func TestPool_UnpinnedRequestNeverWaitsForAFasterMember(t *testing.T) {
	p, _ := testPool(t, MemberSpec{Model: "fast", Slots: 1, Weight: 3}, MemberSpec{Model: "slow", Slots: 1, Weight: 1})
	mustAcquire(t, p, "a").Release()
	mustAcquire(t, p, "b").Release()
	mustAcquire(t, p, "c") // fast, in flight
	if l := mustAcquire(t, p, ""); l.Model() != "slow" {
		t.Fatalf("unpinned request got %s; it adds no owner, so waiting would queue every one on fast", l.Model())
	}
}

func TestPool_EqualWeightsTakeAFreeMemberInsteadOfWaiting(t *testing.T) {
	p, _ := testPool(t, MemberSpec{Model: "a", Slots: 1}, MemberSpec{Model: "b", Slots: 1})
	mustAcquire(t, p, "x").Release() // a
	held := mustAcquire(t, p, "y")   // b, in flight
	if held.Model() != "b" {
		t.Fatalf("y got %s, want b", held.Model())
	}
	mustAcquire(t, p, "z").Release() // a: tie on owners, b busy
	// Now b is busy with one owner and a is free with two. b has the lower
	// share, but neither member is faster, so there is nothing to wait for.
	if l := mustAcquire(t, p, "v"); l.Model() != "a" {
		t.Fatalf("v got %s; with equal weights it should take free a", l.Model())
	}
}

func TestPool_NewConversationStopsWaitingForTheFasterMemberAfterPatience(t *testing.T) {
	p := NewPool("agent-exec", 0, []MemberSpec{{Model: "fast", Slots: 1, Weight: 3}, {Model: "slow", Slots: 1, Weight: 1}},
		Timing{TTL: time.Hour, Idle: time.Hour, Cooldown: time.Minute, Patience: 50 * time.Millisecond})
	mustAcquire(t, p, "a").Release()
	mustAcquire(t, p, "b").Release()
	mustAcquire(t, p, "c") // fast, held for the whole test
	start := time.Now()
	l := mustAcquire(t, p, "d")
	if l.Model() != "slow" {
		t.Fatalf("d got %s, want slow once patience ran out", l.Model())
	}
	if waited := time.Since(start); waited < 40*time.Millisecond {
		t.Fatalf("d took slow after %s; it should first wait for fast", waited)
	}
}

func TestPool_EmptyKeyHasNoAffinity(t *testing.T) {
	p, _ := testPool(t, MemberSpec{Model: "fast", Slots: 1}, MemberSpec{Model: "slow", Slots: 1})
	mustAcquire(t, p, "").Release()
	if n := p.Status().Pinned; n != 0 {
		t.Fatalf("pinned = %d for an empty key", n)
	}
}

func TestLease_ReleaseIsIdempotent(t *testing.T) {
	p, _ := testPool(t, MemberSpec{Model: "only", Slots: 2})
	l := mustAcquire(t, p, "a")
	l.Release()
	l.Release()
	l.ReleaseDown()
	if s := p.Status(); s.Members[0].Busy != 0 || s.Members[0].Down {
		t.Fatalf("double release corrupted state: %+v", s.Members)
	}
}
