package fleetnotify

// The attempt budget: what happens when the far side ACCEPTS the
// connection and then says nothing.
//
// Every "unreachable webhook" in this package's existing coverage is
// http://127.0.0.1:1 — an immediate ECONNREFUSED, which resolves in
// microseconds and never reaches a timer. WebhookConfig.Timeout has been
// a field since C9 and no test has ever made it ELAPSE, so nothing
// distinguished "the deadline works" from "the deadline is never
// consulted". A stalled ntfy behind a reverse proxy is the realistic
// shape, and it is the one that can hold a delivery worker open for as
// long as the far side likes.
//
// (fleetapi's TestNotifyLoop_CloseReturnsPromptlyWhileTheWebhookHangs
// uses Timeout: time.Hour deliberately, so that its assertion is about
// Close() promptness and NOT about this deadline. These tests are the
// other half.)

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// blockingEndpoint serves a handler that accepts the request and then
// stalls until the test ends or the client goes away. The release channel
// is closed BEFORE the server, because httptest.Server.Close waits on
// live connections and a handler parked forever would wedge the test
// binary rather than fail it.
func blockingEndpoint(t *testing.T, h func(w http.ResponseWriter, r *http.Request, release <-chan struct{})) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h(w, r, release)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	return srv
}

func stall(release <-chan struct{}, r *http.Request) {
	select {
	case <-release:
	case <-r.Context().Done():
	}
}

// sendWithin runs one Send and fails the test if it has not returned
// within limit. Bounded rather than blocking on purpose: a disarmed
// deadline must produce a NAMED failure here, not a package-level test
// timeout that reads as infrastructure trouble.
func sendWithin(t *testing.T, limit time.Duration, ctx context.Context, s *WebhookSink, n Notification) (error, time.Duration) {
	t.Helper()
	type result struct {
		err     error
		elapsed time.Duration
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		err := s.Send(ctx, n)
		done <- result{err: err, elapsed: time.Since(start)}
	}()
	select {
	case r := <-done:
		return r.err, r.elapsed
	case <-time.After(limit):
		t.Fatalf("Send did not return within %s against a far side that accepted the connection and said nothing — "+
			"the attempt budget is not being applied", limit)
		return nil, 0
	}
}

// TestWebhookSink_AttemptTimeoutElapsesAgainstABlockingEndpoint is the
// gap this file exists to close: a webhook that answers the TCP handshake
// and then never writes a response header.
func TestWebhookSink_AttemptTimeoutElapsesAgainstABlockingEndpoint(t *testing.T) {
	const budget = 40 * time.Millisecond
	srv := blockingEndpoint(t, func(_ http.ResponseWriter, r *http.Request, release <-chan struct{}) {
		stall(release, r)
	})

	sink, err := NewWebhookSink(WebhookConfig{URL: srv.URL + "/" + secretTopic, Token: secretToken, Timeout: budget})
	if err != nil {
		t.Fatal(err)
	}
	sendErr, elapsed := sendWithin(t, 5*time.Second, context.Background(), sink, testNotification())
	if sendErr == nil {
		t.Fatal("a webhook that never answered reported a successful delivery")
	}
	if elapsed < budget {
		t.Fatalf("Send returned after %s, which is less than the %s budget: this failure did not come from the "+
			"deadline (a refused connection also fails, in microseconds, and proves nothing about it)", elapsed, budget)
	}
	if !Retryable(sendErr) {
		t.Fatalf("a timed-out attempt must be retryable — the far side never answered, so nothing is settled: %v", sendErr)
	}
	// The credential gate, on the TIMEOUT path specifically. *url.Error
	// embeds the full URL in Error() for a deadline exactly as it does for
	// a refusal, and this is the path that had no coverage at all.
	assertNoSecret(t, "timeout error string", sendErr.Error())
	if strings.Contains(sendErr.Error(), redacted) {
		t.Fatalf("the timeout error was not unwrapped from *url.Error; only the scrub kept the topic out: %v", sendErr)
	}
	if !strings.Contains(sendErr.Error(), "deadline") && !strings.Contains(sendErr.Error(), "Timeout") {
		t.Fatalf("error does not read as a deadline: %v", sendErr)
	}
}

// TestWebhookSink_AttemptTimeoutBoundsAStalledResponseBody covers the
// half a header-only deadline would miss: the far side answers with a
// status and then stalls mid-body. http.Client.Timeout keeps running
// across the body read, so the attempt is still bounded — and the PARTIAL
// body it did send is still scrubbed, because a webhook that echoes the
// request path back is how a topic reaches last_error.
func TestWebhookSink_AttemptTimeoutBoundsAStalledResponseBody(t *testing.T) {
	const budget = 40 * time.Millisecond
	srv := blockingEndpoint(t, func(w http.ResponseWriter, r *http.Request, release <-chan struct{}) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("no such topic /" + secretTopic + " (token " + secretToken + ") "))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		stall(release, r)
	})

	sink, err := NewWebhookSink(WebhookConfig{URL: srv.URL + "/" + secretTopic, Token: secretToken, Timeout: budget})
	if err != nil {
		t.Fatal(err)
	}
	sendErr, elapsed := sendWithin(t, 5*time.Second, context.Background(), sink, testNotification())
	if sendErr == nil {
		t.Fatal("a 500 with a stalled body reported a successful delivery")
	}
	if elapsed < budget {
		t.Fatalf("Send returned after %s; the stalled body was not bounded by the %s budget", elapsed, budget)
	}
	if !Retryable(sendErr) {
		t.Fatalf("a 5xx must stay retryable even when its body stalled: %v", sendErr)
	}
	assertNoSecret(t, "stalled-body error string", sendErr.Error())
}

// TestWebhookSink_ContextCancellationBeatsALongAttemptBudget: the two
// bounds are independent, and shutdown must not wait for the slower one.
func TestWebhookSink_ContextCancellationBeatsALongAttemptBudget(t *testing.T) {
	srv := blockingEndpoint(t, func(_ http.ResponseWriter, r *http.Request, release <-chan struct{}) {
		stall(release, r)
	})
	sink, err := NewWebhookSink(WebhookConfig{URL: srv.URL + "/" + secretTopic, Token: secretToken, Timeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	defer cancel()

	sendErr, elapsed := sendWithin(t, 5*time.Second, ctx, sink, testNotification())
	if sendErr == nil {
		t.Fatal("a cancelled Send reported a successful delivery")
	}
	if elapsed > 30*time.Second {
		t.Fatalf("cancellation waited %s on the attempt budget", elapsed)
	}
	assertNoSecret(t, "cancelled error string", sendErr.Error())
	if strings.Contains(sendErr.Error(), redacted) {
		t.Fatalf("the cancellation error was not unwrapped from *url.Error: %v", sendErr)
	}
}

// TestWebhookSink_TheBudgetIsPerAttemptSoTheRetryLadderStillTerminates
// drives the whole worker against a silent far side. Every attempt must
// cost its own budget and the ladder must finish — a per-DELIVERY budget
// would report failure after one attempt, and no budget at all would
// never report at all.
func TestWebhookSink_TheBudgetIsPerAttemptSoTheRetryLadderStillTerminates(t *testing.T) {
	const budget = 30 * time.Millisecond
	attempts := make(chan struct{}, 16)
	srv := blockingEndpoint(t, func(_ http.ResponseWriter, r *http.Request, release <-chan struct{}) {
		select {
		case attempts <- struct{}{}:
		default:
		}
		stall(release, r)
	})

	sink, err := NewWebhookSink(WebhookConfig{URL: srv.URL + "/" + secretTopic, Token: secretToken, Timeout: budget})
	if err != nil {
		t.Fatal(err)
	}
	d := NewDeliverer(sink, DelivererConfig{Attempts: 3, Backoff: time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)
	d.Enqueue(testNotification())

	waitFor(t, func() bool { return d.Stats().Failed == 1 })
	st := d.Stats()
	if st.Retries != 2 {
		t.Fatalf("retries = %d, want 2 (a timed-out attempt is retryable, and the cap is 3)", st.Retries)
	}
	if st.Sent != 0 {
		t.Fatalf("sent = %d against a far side that never answered", st.Sent)
	}
	if got := len(attempts); got != 3 {
		t.Fatalf("the endpoint saw %d attempts, want 3: the budget must bound ONE attempt, not the delivery", got)
	}
	assertNoSecret(t, "deliverer last_error after a timeout", st.LastError)
}

// ─── the default, and an injected client ────────────────────────────────────

// TestNewWebhookSink_AttemptBudgetIsAlwaysSet pins the number that
// actually runs in production: daemon.startNotifyLoop declares no
// Timeout, so an unbounded default would mean the pager's only bound is
// fleetd's shutdown.
func TestNewWebhookSink_AttemptBudgetIsAlwaysSet(t *testing.T) {
	sink, err := NewWebhookSink(WebhookConfig{URL: "https://ntfy.example.invalid/" + secretTopic})
	if err != nil {
		t.Fatal(err)
	}
	if sink.hc.Timeout != defaultAttemptTimeout {
		t.Fatalf("default attempt budget = %s, want %s", sink.hc.Timeout, defaultAttemptTimeout)
	}
	if sink.hc.Timeout <= 0 {
		t.Fatal("the default attempt budget is unbounded: a stalled webhook would hold a delivery worker forever")
	}
}

// TestNewWebhookSink_AnInjectedClientIsStillBounded: Client is documented
// as a test seam, and a seam that quietly drops the declared deadline is
// how a bound stops existing without anybody editing it.
func TestNewWebhookSink_AnInjectedClientIsStillBounded(t *testing.T) {
	t.Run("an unbounded client gets the default", func(t *testing.T) {
		injected := &http.Client{}
		sink, err := NewWebhookSink(WebhookConfig{URL: "https://ntfy.example.invalid/hook", Client: injected})
		if err != nil {
			t.Fatal(err)
		}
		if sink.hc.Timeout != defaultAttemptTimeout {
			t.Fatalf("injected client's budget = %s, want the %s default", sink.hc.Timeout, defaultAttemptTimeout)
		}
		if injected.Timeout != 0 {
			t.Fatal("the caller's own client was mutated; it is shared and not ours to change")
		}
	})
	t.Run("an explicit Timeout wins over the client's", func(t *testing.T) {
		injected := &http.Client{Timeout: time.Hour}
		sink, err := NewWebhookSink(WebhookConfig{URL: "https://ntfy.example.invalid/hook", Client: injected, Timeout: 40 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		if sink.hc.Timeout != 40*time.Millisecond {
			t.Fatalf("declared budget = %s, want the 40ms the caller asked for", sink.hc.Timeout)
		}
		if injected.Timeout != time.Hour {
			t.Fatal("the caller's own client was mutated")
		}
	})
	t.Run("a client's own budget survives when none is declared", func(t *testing.T) {
		injected := &http.Client{Timeout: 25 * time.Millisecond}
		sink, err := NewWebhookSink(WebhookConfig{URL: "https://ntfy.example.invalid/hook", Client: injected})
		if err != nil {
			t.Fatal(err)
		}
		if sink.hc.Timeout != 25*time.Millisecond {
			t.Fatalf("budget = %s; a client that declares its own deadline and a config that declares none must be left alone", sink.hc.Timeout)
		}
	})
}
