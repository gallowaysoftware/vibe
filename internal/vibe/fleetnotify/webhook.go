package fleetnotify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Delivery, stdlib only. The sink is an HTTP POST because that is what
// ntfy, Gotify, Home Assistant and every generic webhook consumer speak.
//
// The webhook URL is a CREDENTIAL: an ntfy topic URL is bearer-
// equivalent in both directions (publish and subscribe). Everything in
// this file is arranged so it cannot be printed — see Redact and
// WebhookSink.Scrub, and note that *url.Error carries the full URL in
// its Error() string, which is the trap this package exists to close.

// Body formats.
const (
	// FormatText posts the message as the body with ntfy's native
	// Title/Priority/Tags headers — the shape that renders as a readable
	// phone notification.
	FormatText = "text"
	// FormatJSON posts a structured document, for consumers that parse.
	FormatJSON = "json"
)

// Sink delivers one notification. Implementations MUST return errors
// that are safe to log: a sink holding a credential is the only party
// that can scrub it out of its own failures.
type Sink interface {
	Send(ctx context.Context, n Notification) error
	// Endpoint is a redacted, loggable description of where this sink
	// sends. It is what the status surface shows.
	Endpoint() string
}

// Scrubber lets a Deliverer strip a sink's secrets from strings it did
// not produce (defence in depth: the sink already scrubs its own).
type Scrubber interface {
	Scrub(string) string
}

// SendError classifies a delivery failure. Status is 0 for a transport
// failure.
type SendError struct {
	Status    int
	Msg       string
	Retryable bool
}

func (e *SendError) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("webhook responded %d: %s", e.Status, e.Msg)
	}
	return "webhook delivery failed: " + e.Msg
}

// Retryable reports whether a failed send is worth another attempt. A
// 4xx is the far side ANSWERING — a bad topic or a rotated token is a
// permanent failure, and retrying it four times just delays the report.
// (C6's piggyback-fallback rule, same reasoning, different transport.)
func Retryable(err error) bool {
	var se *SendError
	if errors.As(err, &se) {
		return se.Retryable
	}
	return false
}

// WebhookConfig configures a WebhookSink.
type WebhookConfig struct {
	// URL is the full webhook endpoint (for ntfy, the topic URL). It is a
	// secret; it is never logged, never returned in an error, and never
	// serialized into a status document.
	URL string
	// Token, when set, rides an Authorization: Bearer header (self-hosted
	// ntfy with access control). Also a secret.
	Token string
	// Format is FormatText (default) or FormatJSON.
	Format string
	// Timeout bounds one attempt (default defaultAttemptTimeout). It is
	// the ONLY bound on a far side that accepts the connection and then
	// says nothing — the Deliverer's retry budget bounds attempts, not the
	// wall time of one, and ctx bounds shutdown, not a hung POST. An ntfy
	// behind a reverse proxy that stalls is the shape this catches;
	// connection-refused, the only unreachable this package used to be
	// tested against, never reaches a timer at all.
	Timeout time.Duration
	// Client overrides the HTTP client (tests). Timeout still applies: it
	// is copied onto the client rather than dropped, because a declared
	// bound that silently does nothing is worse than no field.
	Client *http.Client
}

// defaultAttemptTimeout bounds one delivery attempt when the caller
// declares none. The production caller (daemon.startNotifyLoop) declares
// none, so this is the number that actually runs.
const defaultAttemptTimeout = 10 * time.Second

// WebhookSink posts notifications to one endpoint.
type WebhookSink struct {
	url      string
	token    string
	format   string
	hc       *http.Client
	redacted string
}

// NewWebhookSink validates the endpoint and builds the sink. An
// unparseable or non-HTTP URL fails HERE, with a message that does not
// echo the value back.
func NewWebhookSink(cfg WebhookConfig) (*WebhookSink, error) {
	raw := strings.TrimSpace(cfg.URL)
	if raw == "" {
		return nil, errors.New("notify url is empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("notify url is not a valid URL (value withheld: it is a credential)")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("notify url scheme %q is not http or https", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("notify url has no host")
	}
	format := cfg.Format
	if format == "" {
		format = FormatText
	}
	if format != FormatText && format != FormatJSON {
		return nil, fmt.Errorf("notify format %q must be %s or %s", format, FormatText, FormatJSON)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultAttemptTimeout
	}
	// An injected client must still end up bounded. Copy rather than
	// mutate — the client belongs to the caller and its Transport is meant
	// to be shared — and let an explicit cfg.Timeout win over whatever the
	// client carries, because a declared bound that silently does nothing
	// is worse than no field at all. A client that already declares its own
	// deadline and a config that declares none is left exactly as handed
	// over.
	hc := cfg.Client
	switch {
	case hc == nil:
		hc = &http.Client{Timeout: timeout}
	case cfg.Timeout > 0 && hc.Timeout != cfg.Timeout, hc.Timeout <= 0:
		cp := *hc
		cp.Timeout = timeout
		hc = &cp
	}
	if u.Scheme == "http" && u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1" {
		// The topic is IN the path, so plaintext puts a bearer-equivalent
		// credential on the wire in every request. Named, not refused: the
		// house posture is LAN/VPN, and a self-hosted ntfy behind a
		// reverse proxy is a legitimate deployment.
		slog.Warn("fleet notify endpoint is plaintext http; the webhook path is a credential and travels unencrypted",
			"endpoint", Redact(raw))
	}
	return &WebhookSink{
		url:      raw,
		token:    strings.TrimSpace(cfg.Token),
		format:   format,
		hc:       hc,
		redacted: Redact(raw),
	}, nil
}

// Endpoint returns the redacted endpoint.
func (s *WebhookSink) Endpoint() string { return s.redacted }

// Scrub removes this sink's secrets from an arbitrary string. The URL's
// PARTS are scrubbed as well as the whole URL — see ScrubURL — because
// an ntfy topic is a path segment and a `?auth=` query is a bearer, and
// a leaked one of those is a leaked credential on its own.
// URL first, then token: a token that also appears INSIDE the URL would
// otherwise be replaced first, leaving the whole-URL match to fail
// against a string that no longer contains it.
func (s *WebhookSink) Scrub(msg string) string {
	return scrubToken(ScrubURL(s.url, msg), s.token)
}

// ScrubURL removes rawURL — and, separately, each PART of it that can
// carry the credential — from an arbitrary string.
//
// The whole-URL match alone is not enough, and the reason is the shape of
// the messages this runs over: the far side quotes a FRAGMENT of the
// request, not the string vibe sent. `r.RequestURI` in a 404 body is
// path+query with no scheme and no host; a proxy names the path; a log
// line names the query. None of those contain the whole URL, so none of
// them are matched by it.
//
// So the parts go too, longest first (a shorter piece replaced first
// would break the longer match that contains it):
//
//   - path+query, which is what an echoed RequestURI looks like;
//   - the path, in both its escaped and decoded spellings — for every
//     incoming-webhook dialect this repo speaks (ntfy topics, Slack
//     `/services/T…/B…/…`, Discord `/api/webhooks/<id>/<token>`) the
//     credential IS a path segment;
//   - the query, because `?auth=<token>` is a real webhook shape and is
//     the one the path fallback cannot reach at all when the path is
//     bare "/" — there the whole-URL match used to be the only guard,
//     and a quoted fragment walked straight past it onto
//     GET /api/fleet/state, which is an AccessGuest route;
//   - userinfo, which is where `https://bot:pass@host/hook` keeps it.
//
// A one-character path ("/") is skipped deliberately: redacting every
// slash in the message would destroy the operator's diagnosis, and a "/"
// is not a credential. Same reasoning for an absent query or userinfo.
//
// Exported because the trap it closes is not specific to a Sink: any
// caller holding a webhook URL — vamp's webhook stage renders a fresh
// one per stage from a template and never builds a Sink — must be able
// to close it with THIS code rather than a second implementation that
// drifts. (*WebhookSink).Scrub is a thin wrapper over it for exactly
// that reason.
//
// An empty rawURL is a no-op: there is nothing to hide, and replacing
// the empty string would otherwise redact every character in msg.
func ScrubURL(rawURL, msg string) string {
	raw := strings.TrimSpace(rawURL)
	if raw == "" {
		return msg
	}
	out := strings.ReplaceAll(msg, raw, redacted)
	u, err := url.Parse(raw)
	if err != nil {
		// Unparseable: the whole-string match is all there is, and it has
		// already run. Better than nothing and strictly not worse than
		// the alternative of guessing at parts.
		return out
	}
	for _, part := range scrubbableParts(u) {
		out = strings.ReplaceAll(out, part, redacted)
	}
	return out
}

// scrubbableParts lists the substrings of u that may carry the
// credential, longest first. Nothing shorter than two characters is
// returned: a bare "/" path or a one-character query is not a secret and
// replacing it would redact the message rather than the credential.
func scrubbableParts(u *url.URL) []string {
	var parts []string
	add := func(s string) {
		if len(s) > 1 {
			parts = append(parts, s)
		}
	}
	// RequestURI is path+query — the echoed-fragment shape — so it is
	// both the longest and the one the old code could not see.
	add(u.RequestURI())
	add(u.EscapedPath())
	add(u.Path)
	add(u.RawQuery)
	if u.User != nil {
		// String() is "user:pass" (percent-encoded), and the password on
		// its own, because a message may quote either.
		add(u.User.String())
		if pw, ok := u.User.Password(); ok {
			add(pw)
		}
	}
	sort.SliceStable(parts, func(i, j int) bool { return len(parts[i]) > len(parts[j]) })
	return parts
}

// scrubToken removes a bearer token from msg. Empty token is a no-op,
// for the same reason ScrubURL's empty-URL case is.
func scrubToken(msg, token string) string {
	if token == "" {
		return msg
	}
	return strings.ReplaceAll(msg, token, redacted)
}

const redacted = "<redacted>"

// maxHeaderLen caps header values. Cell names and model ids come off the
// wire (the fleet token is every cell's voice), so they are cleaned
// before they are set — a control character would otherwise make Go's
// transport REJECT the request, turning a hostile announce into a muted
// pager.
const maxHeaderLen = 200

// Send posts one notification.
func (s *WebhookSink) Send(ctx context.Context, n Notification) error {
	body, contentType := s.body(n)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		// Never err.Error(): NewRequestWithContext wraps the URL.
		return &SendError{Msg: "building the request failed", Retryable: false}
	}
	req.Header.Set("Content-Type", contentType)
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	if s.format == FormatText {
		req.Header.Set("Title", headerSafe(n.Title))
		if n.Priority > 0 {
			req.Header.Set("Priority", strconv.Itoa(n.Priority))
		}
		req.Header.Set("Tags", headerSafe(tagsFor(n)))
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		// *url.Error's Error() embeds the full URL. Unwrap to the cause,
		// then scrub what is left: this is the single most likely place
		// for a topic to reach a log file.
		var ue *url.Error
		msg := err.Error()
		if errors.As(err, &ue) && ue.Err != nil {
			msg = ue.Err.Error()
		}
		return &SendError{Msg: s.Scrub(msg), Retryable: true}
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return &SendError{
		Status:    resp.StatusCode,
		Msg:       s.Scrub(headerSafe(string(snippet))),
		Retryable: resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500,
	}
}

func (s *WebhookSink) body(n Notification) ([]byte, string) {
	if s.format == FormatJSON {
		data, err := json.Marshal(n)
		if err != nil {
			return []byte(`{"state":"error","message":"notification not serializable"}`), "application/json"
		}
		return data, "application/json"
	}
	msg := n.Message
	if msg == "" {
		msg = n.Title
	}
	return []byte(msg), "text/plain; charset=utf-8"
}

// tagsFor labels the message with its own metadata. Deliberately plain
// words rather than ntfy's emoji shortcodes (house rule: no emojis).
func tagsFor(n Notification) string {
	parts := []string{"vibe-fleet", n.State}
	if n.Kind != "" {
		parts = append(parts, string(n.Kind))
	}
	return strings.Join(parts, ",")
}

// headerSafe makes an arbitrary string legal in an HTTP header value.
func headerSafe(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if !unicode.IsPrint(r) {
			return -1
		}
		return r
	}, s)
	if len(s) > maxHeaderLen {
		s = s[:maxHeaderLen]
	}
	return strings.TrimSpace(s)
}

// Redact renders a webhook URL loggable: scheme, host, and eight hex of
// its SHA-256 so two endpoints are distinguishable without either being
// reconstructable. The path is dropped (an ntfy topic IS the path), and
// so are the query and any userinfo — plenty of webhooks carry their
// token there.
func Redact(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	id := hex.EncodeToString(sum[:4])
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "(unparseable webhook url) (id " + id + ")"
	}
	// u.Host keeps the port (informative) and excludes userinfo (u.User),
	// which is where a webhook's credential half sometimes lives.
	return u.Scheme + "://" + u.Host + "/... (id " + id + ")"
}
