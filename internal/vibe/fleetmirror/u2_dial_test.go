package fleetmirror

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// U2 — the front-failover takeover probe, and the state it could not
// previously express.
//
// RestoreOptions.Dial has been an injectable seam since C19 and no test
// ever set it, so every assertion about the probe was really an
// assertion about net.DialTimeout against a CLOSED loopback port — which
// refuses in microseconds. DialTimeout therefore never elapsed, and the
// timeout path had never once executed.
//
// What that hid: TakeoverProbe treated every dial error as `continue`,
// so a 2-second timeout and an instant RST produced the SAME empty hit
// list. A live old front behind a firewall that DROPs packets made the
// probe time out, which made Restore PROCEED — two fronts under one
// identity, which is the precise disaster the refusal exists to prevent,
// reached through the guard rather than around it.
//
// A refusal means "nothing is there, go ahead". A timeout means "I could
// not find out". These tests hold those apart, at both ends: the probe
// classifies, and Restore fails closed.
//
// The shape is the repo's oldest defect class — absent evidence read as
// a healthy value — and the template is certNotAfter in
// internal/vibe/fleetapi/doctor.go, which branches on ctx.Err() so that
// "the budget ran out" is never reported as "the host is off".

// ─── fakes ──────────────────────────────────────────────────────────────

// fakeDialer is the seam nothing used. It records what it was handed —
// the timeout above all, since the claim "DialTimeout bounds the dial"
// was never once checked — and answers however the test says.
type fakeDialer struct {
	mu     sync.Mutex
	calls  []dialCall
	answer func(addr string) (net.Conn, error)
}

type dialCall struct {
	network string
	addr    string
	timeout time.Duration
}

func (f *fakeDialer) dial(network, addr string, timeout time.Duration) (net.Conn, error) {
	f.mu.Lock()
	f.calls = append(f.calls, dialCall{network: network, addr: addr, timeout: timeout})
	f.mu.Unlock()
	return f.answer(addr)
}

func (f *fakeDialer) seen() []dialCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]dialCall(nil), f.calls...)
}

// timedOut is what a dialer that honoured its budget hands back: a
// net.Error whose Timeout() is true. os.ErrDeadlineExceeded is the OTHER
// spelling and is checked separately — net's own i/o timeout does NOT
// satisfy errors.Is(err, os.ErrDeadlineExceeded), so a classifier that
// tested only one of them would miss the real one.
type timedOut struct{}

func (timedOut) Error() string   { return "i/o timeout" }
func (timedOut) Timeout() bool   { return true }
func (timedOut) Temporary() bool { return true }

// answeringConn is an in-memory connection: a fake that "answers" has to
// hand back something Closable, and a real listener would be a second
// moving part in a test about the dialer. Built on the TEST's goroutine
// and captured by the fake, never built inside it — the fake runs on the
// probe's goroutine, and registering a Cleanup from there is a race with
// the end of the test.
func answeringConn(t *testing.T) net.Conn {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	return a
}

const (
	fleetdAddr = "fleetd.example.lan:9001" // what newFixture's hosts.yaml records
	frontAddr  = "front.example.lan:9000"
)

// ─── the seam, and the bound ────────────────────────────────────────────

// TestTakeoverProbe_TheDialSeamIsHandedTheConfiguredTimeout is the
// dormant seam, exercised. It also pins the units: a DialTimeout that
// arrived at the dialer as anything other than itself would have made
// every timeout assertion below meaningless.
func TestTakeoverProbe_TheDialSeamIsHandedTheConfiguredTimeout(t *testing.T) {
	f := &fakeDialer{answer: func(string) (net.Conn, error) { return nil, syscall.ECONNREFUSED }}
	scan := ScanTakeover(RestoreOptions{Dial: f.dial, DialTimeout: 40 * time.Millisecond},
		Identity{FleetdURL: "http://" + fleetdAddr, FrontURL: "http://" + frontAddr})

	calls := f.seen()
	require.Len(t, calls, 2, "the probe did not use the injected dialer for both recorded addresses")
	for _, c := range calls {
		require.Equal(t, "tcp", c.network)
		require.Equal(t, 40*time.Millisecond, c.timeout, "the dialer was handed a timeout that is not DialTimeout")
	}
	require.Equal(t, []string{fleetdAddr, frontAddr}, scan.Settled)
	require.Empty(t, scan.Hits)
	require.Empty(t, scan.Unknown)
}

// TestTakeoverProbe_ADialerThatIgnoresItsBudgetIsStillBounded is the
// other half of "DialTimeout bounds the dial": the bound belongs to the
// probe, not to the goodwill of whatever dialer it was handed. A probe
// runs at the worst moment in the fleet's life — the front is down and
// an operator is standing over it — and one that hangs forever is a
// recovery that never starts.
func TestTakeoverProbe_ADialerThatIgnoresItsBudgetIsStillBounded(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	f := &fakeDialer{answer: func(string) (net.Conn, error) {
		<-release // a dialer that will not come back until the test ends
		return nil, errors.New("too late")
	}}

	type result struct {
		scan    TakeoverScan
		elapsed time.Duration
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		s := ScanTakeover(RestoreOptions{Dial: f.dial, DialTimeout: 40 * time.Millisecond},
			Identity{FleetdURL: "http://" + fleetdAddr})
		done <- result{scan: s, elapsed: time.Since(start)}
	}()

	select {
	case got := <-done:
		require.Empty(t, got.scan.Settled, "an address that never answered was reported as settled")
		require.Empty(t, got.scan.Hits)
		require.Len(t, got.scan.Unknown, 1)
		require.Equal(t, fleetdAddr, got.scan.Unknown[0].Addr)
		require.Contains(t, got.scan.Unknown[0].Why, "twice")
		require.Less(t, got.elapsed, 2*time.Second, "the probe returned, but nowhere near its budget")
	case <-time.After(5 * time.Second):
		t.Fatal("the probe hung on a dialer that ignored its timeout: DialTimeout bounds nothing")
	}
}

// TestTakeoverProbe_ADialerReturningBothIsAHit is the asymmetric twin of
// the (nil, nil) case, and it went the dangerous way: the error was
// classified and the CONNECTION — a session established with something,
// the strongest evidence the old front is up — was dropped on the floor
// unread and unclosed. A refusing error beside a live conn scored as
// "nothing is there", which is the green light.
func TestTakeoverProbe_ADialerReturningBothIsAHit(t *testing.T) {
	live := answeringConn(t)
	f := &fakeDialer{answer: func(string) (net.Conn, error) {
		return live, &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
	}}
	scan := ScanTakeover(RestoreOptions{Dial: f.dial, DialTimeout: 40 * time.Millisecond},
		Identity{FleetdURL: "http://" + fleetdAddr})
	require.Len(t, scan.Hits, 1, "a dialer that handed back a live connection was scored as nothing being there")
	require.Equal(t, fleetdAddr, scan.Hits[0].Addr)
	require.Empty(t, scan.Unknown)
}

// TestTakeoverProbe_ADialerReturningNeitherIsUnsettled: nothing in the
// stdlib returns (nil, nil), a wrapper can, and the old code called
// Close on it — a nil dereference in the middle of a recovery, with a
// live front possibly still up. Reading it as "nothing answered" would
// be a guess made by the code whose whole job is not to guess.
func TestTakeoverProbe_ADialerReturningNeitherIsUnsettled(t *testing.T) {
	f := &fakeDialer{answer: func(string) (net.Conn, error) { return nil, nil }}
	scan := ScanTakeover(RestoreOptions{Dial: f.dial, DialTimeout: 40 * time.Millisecond},
		Identity{FleetdURL: "http://" + fleetdAddr})
	require.Len(t, scan.Unknown, 1)
	require.Empty(t, scan.Settled)
	require.Empty(t, scan.Hits)
}

// TestTakeoverProbe_TheDefaultDialerStillSettlesARealRefusal keeps the
// seam honest about the thing it stands in for: with Dial unset the
// probe must still dial for real, and a closed loopback port must still
// come back as a definite negative — the one green light a restore has.
func TestTakeoverProbe_TheDefaultDialerStillSettlesARealRefusal(t *testing.T) {
	// Port 1 is privileged: nothing in this binary can bind it, and a
	// loopback connect gets an immediate ECONNREFUSED. (Same idiom as
	// c19_review_test.go's deadAddr, for the same reason: a recycled
	// ephemeral port can be handed to an httptest server microseconds
	// later and then the "dead" address answers.)
	scan := ScanTakeover(RestoreOptions{DialTimeout: 2 * time.Second},
		Identity{FleetdURL: "http://127.0.0.1:1"})
	require.Equal(t, []string{"127.0.0.1:1"}, scan.Settled)
	require.Empty(t, scan.Unknown, "a real ECONNREFUSED came back as something other than a definite negative")
	require.Empty(t, scan.Hits)
}

// ─── the classifier: which errors are allowed to mean "go ahead" ────────

// TestClassifyDialErr_OnlyADefiniteNegativeMeansNothingIsThere is the
// membership test for the one list that must stay short. The DIRECTION
// is what matters: an error class nobody anticipated has to land in
// doubt, because doubt costs an operator one --force and the other
// default costs two fleetds folding the same cumulative totals into two
// ledgers that can never afterwards be reconciled.
func TestClassifyDialErr_OnlyADefiniteNegativeMeansNothingIsThere(t *testing.T) {
	definite := map[string]error{
		"connection refused (RST)": &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED},
	}
	for name, err := range definite {
		t.Run("definite/"+name, func(t *testing.T) {
			v, _ := classifyDialErr(err)
			require.Equal(t, verdictNothingThere, v, "%s must be readable as 'nothing is there'", name)
		})
	}

	doubt := map[string]error{
		// The headline: a firewall that DROPs rather than REJECTs, which
		// is how an old front that is very much alive presents to a
		// standby on another network.
		"net timeout": &net.OpError{Op: "dial", Err: timedOut{}},
		// ECONNRESET is not absence. Something accepted and then tore the
		// connection down — a proxy shedding load, a middlebox — which is
		// evidence of a LIVE peer, and reading it as "nothing is there"
		// would be fail-open inside the list that must be fail-closed.
		"connection reset by a live peer": &net.OpError{Op: "dial", Err: syscall.ECONNRESET},
		"os deadline exceeded":            os.ErrDeadlineExceeded,
		"context deadline":                context.DeadlineExceeded,
		// NXDOMAIN was in the DEFINITE map above, under protest and with a
		// written reason: closing it would have failed roughly a dozen
		// restore fixtures that reached their green light through it, so U2
		// named it and left it. This line moving down is that fail-open
		// closed. Resolution happens on the box doing the asking, and the
		// box doing the asking is the standby — a resolver with no view of
		// the internal zone NXDOMAINs the name of a front that is alive.
		"the name does not resolve from here": &net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", IsNotFound: true}},
		"host unreachable":                    &net.OpError{Op: "dial", Err: syscall.EHOSTUNREACH},
		"network unreachable":                 &net.OpError{Op: "dial", Err: syscall.ENETUNREACH},
		"blocked by local policy":             &net.OpError{Op: "dial", Err: syscall.EACCES},
		"resolver is unwell":                  &net.OpError{Op: "dial", Err: &net.DNSError{Err: "server misbehaving", IsTemporary: true}},
		"something nobody planned":            errors.New("a brand new failure mode"),
	}
	for name, err := range doubt {
		t.Run("doubt/"+name, func(t *testing.T) {
			v, why := classifyDialErr(err)
			require.Equal(t, verdictUnsettled, v, "%s was read as evidence the old front is dead", name)
			require.NotEmpty(t, why, "an unsettled dial must say what it could not establish")
		})
	}
}

// ─── Restore: the refusal ───────────────────────────────────────────────

// dialing builds a standby's RestoreOptions with the seam wired to a
// per-address answer table. Anything not in the table refuses.
func dialing(o RestoreOptions, answers map[string]func() (net.Conn, error)) RestoreOptions {
	o.DialTimeout = 40 * time.Millisecond
	o.Dial = func(_, addr string, _ time.Duration) (net.Conn, error) {
		if a, ok := answers[addr]; ok {
			return a()
		}
		return nil, &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
	}
	return o
}

func mirroredArchive(t *testing.T) string {
	t.Helper()
	f := newFixture(t)
	_, rc, err := Create(f.opts())
	require.NoError(t, err)
	return rc.Archive
}

// TestRestore_RefusesWhenTheProbeCouldNotFindOut is the hazard itself.
//
// Both recorded addresses time out. Before U2 that was an empty hit
// list, an empty Takeover slice and a restore that WROTE — the standby
// stood up beside an old front that was never dead, only firewalled.
func TestRestore_RefusesWhenTheProbeCouldNotFindOut(t *testing.T) {
	archive := mirroredArchive(t)
	sb := newStandby(t)
	timeout := func() (net.Conn, error) { return nil, &net.OpError{Op: "dial", Err: timedOut{}} }

	rep, err := Restore(dialing(sb.opts(archive), map[string]func() (net.Conn, error){
		fleetdAddr: timeout,
		frontAddr:  timeout,
	}))
	require.ErrorIs(t, err, ErrProbeInconclusive, "a restore proceeded on a probe that established nothing")
	require.Equal(t, 0, rep.Wrote)
	require.Len(t, rep.Unresolved, 2, "the refusal does not name what it could not settle")
	require.Empty(t, rep.Probed, "an address the probe could not settle was reported as evidence")
	_, statErr := os.Stat(filepath.Join(sb.state, "token"))
	require.ErrorIs(t, statErr, fs.ErrNotExist, "the refusal still placed the fleet's token")
}

// TestRestore_AResolverThatNXDOMAINsEverythingIsNotADeadFront is the
// fail-open U2 named in its addendum and could not close, because the
// fixtures that would have failed were in a file it did not own.
//
// The scenario is one broken RESOLVER, not one retired name. A standby
// whose /etc/resolv.conf points at the front host that just died, or
// which sits where the internal zone is not served, NXDOMAINs every
// recorded name at once — including the name of a front that is alive
// and serving every client that already holds its address. Under the old
// classification each of those was a definite negative, so the whole
// scan settled clean, `Hits` was empty and the restore PROCEEDED: two
// fronts under one identity, reached through the guard rather than
// around it, and reached by the single most ordinary way a spare box
// differs from the one it is replacing.
//
// The all-NXDOMAIN scan is the shape that matters, because a resolver
// that cannot answer cannot answer for anything: the failure arrives on
// every address simultaneously, which is precisely what made it look
// like unanimous evidence of absence.
func TestRestore_AResolverThatNXDOMAINsEverythingIsNotADeadFront(t *testing.T) {
	archive := mirroredArchive(t)
	sb := newStandby(t)
	nxdomain := func(name string) func() (net.Conn, error) {
		return func() (net.Conn, error) {
			return nil, &net.OpError{Op: "dial", Err: &net.DNSError{
				Err: "no such host", Name: name, IsNotFound: true}}
		}
	}
	answers := map[string]func() (net.Conn, error){
		fleetdAddr: nxdomain("fleetd.example.lan"),
		frontAddr:  nxdomain("front.example.lan"),
	}

	rep, err := Restore(dialing(sb.opts(archive), answers))
	require.ErrorIs(t, err, ErrProbeInconclusive,
		"a resolver that answered nothing was read as a front that is gone")
	require.Equal(t, 0, rep.Wrote)
	require.Empty(t, rep.Probed,
		"an NXDOMAIN was reported as evidence about the old front; it is evidence about this box's resolver")
	require.Len(t, rep.Unresolved, 2, "the refusal does not name both names it could not resolve")
	for _, u := range rep.Unresolved {
		require.Contains(t, u.Why, "resolver",
			"an unsettled DNS dial must say the doubt is about resolution, or the operator debugs the wrong host")
	}
	require.NoFileExists(t, filepath.Join(sb.state, "token"),
		"the refusal still placed the fleet's token")

	// --force remains the escape, and it is the right one here: a human
	// standing in front of the old box can establish what this box's
	// resolver cannot.
	forced := dialing(sb.opts(archive), answers)
	forced.Force = true
	after, err := Restore(forced)
	require.NoError(t, err)
	require.Greater(t, after.Wrote, 0)
}

// TestRestore_ATimedOutProbeIsNotReportedAsNothingToDial pins the ORDER
// of the two refusals. Both stop the restore, so safety does not turn on
// it — but they send an operator to different places: ErrNoProbeTargets
// says "your mirror recorded no address, name one", and that is a lie
// when an address was recorded, dialed, and simply never answered.
func TestRestore_ATimedOutProbeIsNotReportedAsNothingToDial(t *testing.T) {
	archive := mirroredArchive(t)
	sb := newStandby(t)
	timeout := func() (net.Conn, error) { return nil, &net.OpError{Op: "dial", Err: timedOut{}} }

	_, err := Restore(dialing(sb.opts(archive), map[string]func() (net.Conn, error){
		fleetdAddr: timeout,
		frontAddr:  timeout,
	}))
	require.ErrorIs(t, err, ErrProbeInconclusive)
	require.NotErrorIs(t, err, ErrNoProbeTargets, "a probe that dialed and learned nothing was reported as having nothing to dial")
}

// TestRestore_AnAnsweredAddressOutranksAnUnsettledOne: one address
// answers, the other times out. Both refuse, and the operator must be
// told the specific one — "the fleet's address still answers" is a fact,
// "I could not find out" is the absence of one.
func TestRestore_AnAnsweredAddressOutranksAnUnsettledOne(t *testing.T) {
	archive := mirroredArchive(t)
	sb := newStandby(t)
	live := answeringConn(t)

	rep, err := Restore(dialing(sb.opts(archive), map[string]func() (net.Conn, error){
		fleetdAddr: func() (net.Conn, error) { return live, nil },
		frontAddr:  func() (net.Conn, error) { return nil, &net.OpError{Op: "dial", Err: timedOut{}} },
	}))
	require.ErrorIs(t, err, ErrTakeover)
	require.Len(t, rep.Takeover, 1)
	require.Equal(t, fleetdAddr, rep.Takeover[0].Addr)
	require.Equal(t, 0, rep.Wrote)
	// ...and the address that was never settled is still reported, because
	// the operator about to pass --force needs to know that half of this
	// probe found nothing out at all.
	require.Len(t, rep.Unresolved, 1)
	require.Equal(t, frontAddr, rep.Unresolved[0].Addr)
}

// TestRestore_ASettledProbeStillProceeds is the guard's other side: the
// refusal must not have swallowed the ordinary recovery, which is the
// whole reason C19 exists. Every address gives a definite negative — the
// old front really is gone — and the standby comes up.
func TestRestore_ASettledProbeStillProceeds(t *testing.T) {
	archive := mirroredArchive(t)
	sb := newStandby(t)

	rep, err := Restore(dialing(sb.opts(archive), nil)) // nothing in the table: everything refuses
	require.NoError(t, err, "a probe that settled every address still refused")
	require.Greater(t, rep.Wrote, 0)
	require.Equal(t, []string{fleetdAddr, frontAddr}, rep.Probed)
	require.Empty(t, rep.Unresolved)
	require.FileExists(t, filepath.Join(sb.state, "token"))
}

// TestRestore_ForceRemainsTheOnlyEscapeFromAnUnsettledProbe.
//
// --force stays the escape hatch, and deliberately: C19's design rests
// on a HUMAN confirming the old front is down, and --force is the only
// thing in the system that says a human did. A second, gentler flag for
// "the probe timed out, proceed anyway" would be an easier lever
// reaching the same irreversible place, so there isn't one.
func TestRestore_ForceRemainsTheOnlyEscapeFromAnUnsettledProbe(t *testing.T) {
	archive := mirroredArchive(t)
	sb := newStandby(t)
	answers := map[string]func() (net.Conn, error){
		fleetdAddr: func() (net.Conn, error) { return nil, &net.OpError{Op: "dial", Err: timedOut{}} },
		frontAddr:  func() (net.Conn, error) { return nil, &net.OpError{Op: "dial", Err: timedOut{}} },
	}

	o := dialing(sb.opts(archive), answers)
	_, err := Restore(o)
	require.ErrorIs(t, err, ErrProbeInconclusive)

	o.Force = true
	rep, err := Restore(o)
	require.NoError(t, err, "--force no longer skips the probe")
	require.Greater(t, rep.Wrote, 0)
	require.Empty(t, rep.Unresolved, "--force skipped the probe but reported its findings anyway")
}

// ─── the deprecated two-return shape ────────────────────────────────────

// TestTakeoverProbe_TheTwoReturnShapeDegradesToARefusal.
//
// TakeoverProbe cannot express the third state, and a wrapper that
// dropped it would BE this repo's recurring defect — the pair whose bit
// a later reader discards. It doesn't: an address it could not settle
// appears in neither return, so the caller's own `len(probed) == 0`
// guard fires and refuses. The degradation goes the safe way.
func TestTakeoverProbe_TheTwoReturnShapeDegradesToARefusal(t *testing.T) {
	f := &fakeDialer{answer: func(string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Err: timedOut{}}
	}}
	hits, probed := TakeoverProbe(RestoreOptions{Dial: f.dial, DialTimeout: 40 * time.Millisecond},
		Identity{FleetdURL: "http://" + fleetdAddr, FrontURL: "http://" + frontAddr})
	require.Empty(t, hits)
	require.Empty(t, probed, "the two-return shape reported an unsettled address as probed, which reads as 'nothing answered'")
}

// TestTakeoverProbe_TheTwoReturnShapeRefusesOnAMIXEDScan is the case the
// test above cannot reach, and it is the one that matters.
//
// Dropping the unsettled addresses and returning the settled ones is not
// enough: fleetd refused (the host rebooted) and the front timed out
// (alive, behind a DROP rule) left ONE address in `probed` and no hits —
// which is the all-clear, handed to a caller that cannot see the third
// state, for the exact scan that most needs a refusal. Returning a
// SUBSET of the evidence is the dropped bit wearing a different coat.
func TestTakeoverProbe_TheTwoReturnShapeRefusesOnAMIXEDScan(t *testing.T) {
	f := &fakeDialer{answer: func(addr string) (net.Conn, error) {
		if addr == fleetdAddr {
			return nil, &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
		}
		return nil, &net.OpError{Op: "dial", Err: timedOut{}}
	}}
	opts := RestoreOptions{Dial: f.dial, DialTimeout: 40 * time.Millisecond}
	id := Identity{FleetdURL: "http://" + fleetdAddr, FrontURL: "http://" + frontAddr}

	// The full scan sees all three states.
	scan := ScanTakeover(opts, id)
	require.Equal(t, []string{fleetdAddr}, scan.Settled)
	require.Len(t, scan.Unknown, 1)
	require.Equal(t, frontAddr, scan.Unknown[0].Addr)

	// The two-return shape must hand back nothing rather than the half it
	// can express.
	hits, probed := TakeoverProbe(opts, id)
	require.Empty(t, hits)
	require.Empty(t, probed,
		"a mixed scan handed the legacy caller a settled address and no hits — the all-clear, with a live front unaccounted for")
}

// TestTakeoverProbe_HasNoProductionCallers keeps the paragraph above
// true. Restore is the only thing that decides whether a standby comes
// up, and it must decide on the full scan; a future caller reaching for
// the shorter signature is exactly how the dropped bit gets back in.
func TestTakeoverProbe_HasNoProductionCallers(t *testing.T) {
	callers, err := nonTestCallersOf("../../..", "TakeoverProbe", probeDefinedIn)
	require.NoError(t, err)
	require.Empty(t, callers, "use ScanTakeover: the two-return probe cannot say 'I could not find out'")
}

// probeDefinedIn is a PATH, not a basename. Excluding by basename made
// every restore.go in the repo invisible to the scan — including a
// future internal/vibe/fleetapi/restore.go calling straight into the
// deprecated probe, which is the one regression this guard exists to
// catch.
var probeDefinedIn = filepath.Join("internal", "vibe", "fleetmirror", "restore.go")

// TestNonTestCallersOf_FindsAPlantedCaller is the scan's own proof. A
// source guard that can only pass is not a guard.
func TestNonTestCallersOf_FindsAPlantedCaller(t *testing.T) {
	root := t.TempDir()
	plant := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
	}
	plant("pkg/caller.go", "package pkg\n\nfunc x() { TakeoverProbe(nil, nil) }\n")
	plant("pkg/caller_test.go", "package pkg\n\nfunc y() { TakeoverProbe(nil, nil) }\n")
	plant("pkg/restore.go", "package pkg\n\nfunc z() { TakeoverProbe(nil, nil) }\n")
	plant("pkg/.git/hook.go", "package pkg\n\nfunc w() { TakeoverProbe(nil, nil) }\n")
	// A caller taking the function VALUE rather than calling it — the
	// reason the scan matches the bare identifier and not "TakeoverProbe(".
	plant("pkg/indirect.go", "package pkg\n\nvar f = TakeoverProbe\n")

	callers, err := nonTestCallersOf(root, "TakeoverProbe", filepath.Join("pkg", "restore.go"))
	require.NoError(t, err)
	require.Equal(t, []string{filepath.Join("pkg", "caller.go"), filepath.Join("pkg", "indirect.go")}, callers,
		"the scan must see production callers and only production callers")

	// ...and the exclusion is the DEFINING PATH, not any file that happens
	// to share its name.
	elsewhere, err := nonTestCallersOf(root, "TakeoverProbe", filepath.Join("other", "restore.go"))
	require.NoError(t, err)
	require.Contains(t, elsewhere, filepath.Join("pkg", "restore.go"),
		"a same-named file in another package was excluded by its basename")
}

// nonTestCallersOf lists the non-test .go files under root that mention
// symbol, excluding the file at definedIn (a root-relative PATH).
func nonTestCallersOf(root, symbol, definedIn string) ([]string, error) {
	var found []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// .claude holds this batch's sibling worktrees — whole copies of
			// the repo, whose contents are not this tree's business.
			switch d.Name() {
			case ".git", ".claude", "vendor", "node_modules":
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == definedIn {
			return nil
		}
		b, err := os.ReadFile(p) //nolint:gosec // a repo-relative source file this test just walked to
		if err != nil {
			return err
		}
		if strings.Contains(string(b), symbol) {
			found = append(found, rel)
		}
		return nil
	})
	return found, err
}
