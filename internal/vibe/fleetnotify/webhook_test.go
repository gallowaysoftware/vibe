package fleetnotify

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The secret in every test below. An ntfy topic URL is bearer-
// equivalent: the path IS the credential, which is why the assertions
// look for the topic as well as the whole URL.
const (
	secretTopic = "vibe-fleet-SECRETTOPIC"
	secretToken = "tk_SECRETTOKEN"
)

func testNotification() Notification {
	return Notification{
		Key: "cell_absent\x00hum", Kind: KindCellAbsent, Scope: "hum",
		State: StateFiring, Title: "fleet: hum", Message: "hum is OFF/AWAY",
		At: epoch, Priority: 4,
	}
}

// TestWebhookSink_TextFormatCarriesNtfyHeadersAndThePlainBody pins the
// shape that renders as a readable phone notification.
func TestWebhookSink_TextFormatCarriesNtfyHeadersAndThePlainBody(t *testing.T) {
	var gotTitle, gotPriority, gotTags, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTitle = r.Header.Get("Title")
		gotPriority = r.Header.Get("Priority")
		gotTags = r.Header.Get("Tags")
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
	}))
	defer srv.Close()

	sink, err := NewWebhookSink(WebhookConfig{URL: srv.URL + "/" + secretTopic, Token: secretToken})
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Send(context.Background(), testNotification()); err != nil {
		t.Fatalf("send: %v", err)
	}
	if gotTitle != "fleet: hum" || gotPriority != "4" || gotBody != "hum is OFF/AWAY" {
		t.Fatalf("title=%q priority=%q body=%q", gotTitle, gotPriority, gotBody)
	}
	if !strings.Contains(gotTags, "cell_absent") || !strings.Contains(gotTags, "firing") {
		t.Fatalf("tags = %q", gotTags)
	}
	if gotAuth != "Bearer "+secretToken {
		t.Fatalf("auth header = %q", gotAuth)
	}
}

// TestWebhookSink_JSONFormatPostsTheStructuredDocument covers the
// generic-consumer half.
func TestWebhookSink_JSONFormatPostsTheStructuredDocument(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
	}))
	defer srv.Close()

	sink, err := NewWebhookSink(WebhookConfig{URL: srv.URL + "/hook", Format: FormatJSON})
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Send(context.Background(), testNotification()); err != nil {
		t.Fatal(err)
	}
	var got Notification
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body is not the notification document: %q", body)
	}
	if got.Kind != KindCellAbsent || got.State != StateFiring || got.Scope != "hum" {
		t.Fatalf("decoded = %+v", got)
	}
}

// TestWebhookSink_ControlCharactersInATitleDoNotKillTheDelivery: cell
// names and model ids come off the wire, and Go's transport REJECTS a
// header value containing a newline — a hostile announce would otherwise
// mute the pager.
func TestWebhookSink_ControlCharactersInATitleDoNotKillTheDelivery(t *testing.T) {
	var gotTitle string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTitle = r.Header.Get("Title")
	}))
	defer srv.Close()

	sink, err := NewWebhookSink(WebhookConfig{URL: srv.URL + "/hook"})
	if err != nil {
		t.Fatal(err)
	}
	n := testNotification()
	n.Title = "fleet: hum\r\nX-Injected: yes\x00" + strings.Repeat("p", 400)
	if err := sink.Send(context.Background(), n); err != nil {
		t.Fatalf("a control character killed the delivery: %v", err)
	}
	if strings.ContainsAny(gotTitle, "\r\n\x00") {
		t.Fatalf("control characters reached the header: %q", gotTitle)
	}
	if len(gotTitle) > maxHeaderLen {
		t.Fatalf("header not capped: %d bytes", len(gotTitle))
	}
}

// TestWebhookSink_TransportFailureLeaksNeitherTheURLNorTheTopic is the
// secret gate's core: *url.Error's Error() embeds the full URL, so an
// unwrapped failure is how a bearer-equivalent topic reaches a log file.
func TestWebhookSink_TransportFailureLeaksNeitherTheURLNorTheTopic(t *testing.T) {
	// A port nothing listens on: a real transport error, not a stub.
	full := "http://127.0.0.1:1/" + secretTopic
	sink, err := NewWebhookSink(WebhookConfig{URL: full, Token: secretToken, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	err = sink.Send(context.Background(), testNotification())
	if err == nil {
		t.Fatal("want a transport error")
	}
	assertNoSecret(t, "error string", err.Error())
	assertNoSecret(t, "endpoint", sink.Endpoint())
	// Two independent guards protect this path — the *url.Error unwrap
	// and the scrub — and either alone would hide the other's removal.
	// A surviving "<redacted>" means the wrapper reached the message and
	// only the scrub saved it, so assert the unwrap ran too.
	if strings.Contains(err.Error(), redacted) {
		t.Fatalf("the transport error was not unwrapped; only the scrub kept the URL out: %v", err)
	}
	if !Retryable(err) {
		t.Fatalf("a transport failure must be retryable: %v", err)
	}
}

// TestWebhookSink_ErrorBodyIsScrubbedOfTheTopic: a far side that echoes
// the request URL back in its error body must not turn that body into a
// leak when it is stored as last_error.
func TestWebhookSink_ErrorBodyIsScrubbedOfTheTopic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such topic /"+secretTopic+" (token "+secretToken+")", http.StatusNotFound)
	}))
	defer srv.Close()

	sink, err := NewWebhookSink(WebhookConfig{URL: srv.URL + "/" + secretTopic, Token: secretToken})
	if err != nil {
		t.Fatal(err)
	}
	err = sink.Send(context.Background(), testNotification())
	if err == nil {
		t.Fatal("want a 404 error")
	}
	assertNoSecret(t, "404 error string", err.Error())
}

// TestScrubURL_RemovesACredentialThatIsNotInThePath: the path is not the
// only place a webhook keeps its bearer, and it is the only place ScrubURL
// used to look past the whole-URL match.
//
// The two shapes below are real deployments, not constructions: a `?auth=`
// query is how ntfy's own access-token-in-a-URL works and how several
// self-hosted receivers authenticate, and userinfo is what a `https://
// user:pass@host/hook` line in hosts.yaml produces. Both defeat the
// whole-URL replacement the moment the far side quotes a FRAGMENT of the
// request rather than the string vibe sent — `r.RequestURI` is path+query
// with no scheme or host, which is what an echoing 404 body contains.
//
// This is not a hypothetical surface. (*WebhookSink).Scrub feeds
// deliver.go's stats.LastError, which fleetapi publishes on
// GET /api/fleet/state — an AccessGuest route — and in the fleet_status
// MCP tool. A credential that survives this function is a credential on
// the guest surface.
func TestScrubURL_RemovesACredentialThatIsNotInThePath(t *testing.T) {
	const querySecret = "QUERYBEARER-SECRET"
	const userinfoSecret = "USERINFOPASS-SECRET"
	for _, tc := range []struct {
		name   string
		raw    string
		msg    string
		secret string
	}{
		{
			name:   "credential in the query, quoted as RequestURI",
			raw:    "https://hook.example/notify?auth=" + querySecret,
			msg:    "404 Not Found: no such webhook /notify?auth=" + querySecret,
			secret: querySecret,
		},
		{
			// Path is "/" so the len(u.Path) > 1 fallback does not fire at
			// all: the whole-URL match is the ONLY thing standing here, and
			// a quoted fragment walks straight past it.
			name:   "credential in the query with a bare path",
			raw:    "https://hook.example/?auth=" + querySecret,
			msg:    "rejected request-uri /?auth=" + querySecret,
			secret: querySecret,
		},
		{
			name:   "credential in userinfo",
			raw:    "https://bot:" + userinfoSecret + "@hook.example/notify",
			msg:    `Post "https://bot:` + userinfoSecret + `@hook.example/notify": EOF`,
			secret: userinfoSecret,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ScrubURL(tc.raw, tc.msg)
			if strings.Contains(got, tc.secret) {
				t.Errorf("ScrubURL left the credential in place: %q", got)
			}
			// And the sink's own wrapper, which is what deliver.go calls.
			sink, err := NewWebhookSink(WebhookConfig{URL: tc.raw})
			if err != nil {
				t.Fatalf("NewWebhookSink: %v", err)
			}
			if s := sink.Scrub(tc.msg); strings.Contains(s, tc.secret) {
				t.Errorf("(*WebhookSink).Scrub left the credential in place: %q", s)
			}
			// Redact is the other half of the same promise and must not
			// have regressed while this one was being fixed.
			if e := sink.Endpoint(); strings.Contains(e, tc.secret) {
				t.Errorf("Endpoint() carries the credential: %q", e)
			}
		})
	}
}

// TestScrubURL_DoesNotRedactTheWholeMessage is the over-correction guard.
// A scrubber that replaced too little is the bug above; one that replaces
// a one-character path or an empty query eats the operator's diagnosis and
// is how a scrub gets deleted rather than fixed.
func TestScrubURL_DoesNotRedactTheWholeMessage(t *testing.T) {
	got := ScrubURL("https://hook.example/", "connect: connection refused after 3 attempts")
	if got != "connect: connection refused after 3 attempts" {
		t.Errorf("ScrubURL rewrote a message containing none of its URL: %q", got)
	}
	if got := ScrubURL("", "anything at all"); got != "anything at all" {
		t.Errorf("an empty URL must be a no-op, got %q", got)
	}
}

// TestWebhookSink_FourXXIsNotRetriedAndFiveXXIs: a 4xx is the far side
// ANSWERING (C6's piggyback rule) — a bad topic is a permanent failure,
// and reporting it four attempts late helps nobody.
func TestWebhookSink_FourXXIsNotRetriedAndFiveXXIs(t *testing.T) {
	for _, tc := range []struct {
		status    int
		retryable bool
	}{
		{http.StatusNotFound, false},
		{http.StatusForbidden, false},
		{http.StatusBadRequest, false},
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusBadGateway, true},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
		}))
		sink, err := NewWebhookSink(WebhookConfig{URL: srv.URL + "/hook"})
		if err != nil {
			t.Fatal(err)
		}
		err = sink.Send(context.Background(), testNotification())
		if err == nil {
			t.Fatalf("status %d: want an error", tc.status)
		}
		if Retryable(err) != tc.retryable {
			t.Errorf("status %d: retryable = %v, want %v", tc.status, Retryable(err), tc.retryable)
		}
		srv.Close()
	}
}

// TestRedact_KeepsSchemeAndHostAndDropsTheTopicQueryAndUserinfo.
func TestRedact_KeepsSchemeAndHostAndDropsTheTopicQueryAndUserinfo(t *testing.T) {
	got := Redact("https://user:pw@ntfy.example.invalid:8443/" + secretTopic + "?auth=" + secretToken)
	assertNoSecret(t, "redacted", got)
	if !strings.Contains(got, "ntfy.example.invalid:8443") || !strings.HasPrefix(got, "https://") {
		t.Fatalf("redacted form lost its useful half: %q", got)
	}
	if strings.Contains(got, "user") || strings.Contains(got, "pw") {
		t.Fatalf("userinfo survived redaction: %q", got)
	}
	// Two different endpoints must be distinguishable without either
	// being reconstructable.
	if Redact("https://ntfy.example.invalid/a") == Redact("https://ntfy.example.invalid/b") {
		t.Fatal("redaction collapses distinct endpoints to one string")
	}
}

func TestNewWebhookSink_RejectsANonHTTPURLWithoutEchoingIt(t *testing.T) {
	_, err := NewWebhookSink(WebhookConfig{URL: "file:///etc/" + secretTopic})
	if err == nil {
		t.Fatal("want an error for a file:// endpoint")
	}
	assertNoSecret(t, "constructor error", err.Error())
}

// ─── the delivery worker ────────────────────────────────────────────────────

type stubSink struct {
	mu    sync.Mutex
	calls int
	errs  []error
	sent  []Notification
}

func (s *stubSink) Send(_ context.Context, n Notification) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.sent = append(s.sent, n)
	if len(s.errs) > 0 {
		err := s.errs[0]
		s.errs = s.errs[1:]
		return err
	}
	return nil
}

func (s *stubSink) Endpoint() string { return "stub://sink" }

func (s *stubSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// TestDeliverer_RetriesARetryableFailureUpToTheCapThenReportsIt.
func TestDeliverer_RetriesARetryableFailureUpToTheCapThenReportsIt(t *testing.T) {
	sink := &stubSink{errs: []error{
		&SendError{Status: 500, Msg: "boom", Retryable: true},
		&SendError{Status: 500, Msg: "boom", Retryable: true},
		&SendError{Status: 500, Msg: "boom", Retryable: true},
		&SendError{Status: 500, Msg: "boom", Retryable: true},
	}}
	d := NewDeliverer(sink, DelivererConfig{Attempts: 4, Backoff: time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)
	d.Enqueue(testNotification())

	waitFor(t, func() bool { return d.Stats().Failed == 1 })
	if got := sink.count(); got != 4 {
		t.Fatalf("attempts = %d, want the 4-attempt cap", got)
	}
	st := d.Stats()
	if st.Retries != 3 || st.Sent != 0 || !strings.Contains(st.LastError, "500") {
		t.Fatalf("stats = %+v", st)
	}
}

// TestDeliverer_APermanentFailureIsNotRetried is the same rule from the
// worker's side: four attempts against a 404 is three pointless
// requests and a late report.
func TestDeliverer_APermanentFailureIsNotRetried(t *testing.T) {
	sink := &stubSink{errs: []error{&SendError{Status: 404, Msg: "no such topic", Retryable: false}}}
	d := NewDeliverer(sink, DelivererConfig{Attempts: 4, Backoff: time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)
	d.Enqueue(testNotification())

	waitFor(t, func() bool { return d.Stats().Failed == 1 })
	if got := sink.count(); got != 1 {
		t.Fatalf("a 4xx was attempted %d times; the far side already answered", got)
	}
}

// TestDeliverer_EnqueueNeverBlocksAndCountsTheDrop: a notifier that
// applies back-pressure to the thing it is watching takes the fleet down
// with it.
func TestDeliverer_EnqueueNeverBlocksAndCountsTheDrop(t *testing.T) {
	d := NewDeliverer(&stubSink{}, DelivererConfig{QueueSize: 2})
	// Run is deliberately NOT started: the queue fills and Enqueue must
	// still return immediately.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 10 {
			d.Enqueue(testNotification())
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Enqueue blocked on a full queue")
	}
	if st := d.Stats(); st.Dropped != 8 {
		t.Fatalf("dropped = %d, want 8 (10 enqueued into a 2-deep queue)", st.Dropped)
	}
}

// TestDeliverer_CancellationAbandonsABackoffImmediately: the retry sleep
// is bound to the caller's context, so a wedged webhook cannot hold
// fleetd's shutdown open for its full backoff schedule.
func TestDeliverer_CancellationAbandonsABackoffImmediately(t *testing.T) {
	sink := &stubSink{errs: []error{&SendError{Status: 500, Msg: "boom", Retryable: true}}}
	d := NewDeliverer(sink, DelivererConfig{Attempts: 5, Backoff: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { d.Run(ctx); close(stopped) }()
	d.Enqueue(testNotification())
	waitFor(t, func() bool { return sink.count() == 1 })

	start := time.Now()
	cancel()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancellation; a one-hour backoff held it")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("cancellation took %s", elapsed)
	}
}

// TestDeliverer_LastErrorIsScrubbedThroughTheSink is the second lock on
// the same door: a sink error the Deliverer stores must not carry a
// credential even if the sink forgot to scrub it.
func TestDeliverer_LastErrorIsScrubbedThroughTheSink(t *testing.T) {
	sink, err := NewWebhookSink(WebhookConfig{URL: "http://127.0.0.1:1/" + secretTopic, Token: secretToken, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	leaky := &leakySink{WebhookSink: sink}
	d := NewDeliverer(leaky, DelivererConfig{Attempts: 1, Backoff: time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)
	d.Enqueue(testNotification())
	waitFor(t, func() bool { return d.Stats().Failed == 1 })
	assertNoSecret(t, "deliverer last_error", d.Stats().LastError)
}

// leakySink is a sink that returns the raw URL in its error — the
// mistake the Deliverer's own scrub exists to survive.
type leakySink struct{ *WebhookSink }

func (l *leakySink) Send(context.Context, Notification) error {
	return &SendError{Msg: "post http://127.0.0.1:1/" + secretTopic + " failed", Retryable: false}
}

// TestDeliveryLogsNeverCarryTheSecret drives a failing delivery with the
// package's slog output captured, which is where a leaked topic would
// actually land in production.
func TestDeliveryLogsNeverCarryTheSecret(t *testing.T) {
	var buf syncBuffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	sink, err := NewWebhookSink(WebhookConfig{URL: "http://127.0.0.1:1/" + secretTopic, Token: secretToken, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	d := NewDeliverer(sink, DelivererConfig{QueueSize: 1, Attempts: 1, Backoff: time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)
	d.Enqueue(testNotification())
	waitFor(t, func() bool { return d.Stats().Failed == 1 })
	// Also force the queue-full log line.
	for range 5 {
		d.Enqueue(testNotification())
	}
	assertNoSecret(t, "log output", buf.String())
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func assertNoSecret(t *testing.T, where, s string) {
	t.Helper()
	for _, secret := range []string{secretTopic, secretToken} {
		if strings.Contains(s, secret) {
			t.Fatalf("%s leaked a credential (%s): %q", where, secret, s)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within 3s")
}
