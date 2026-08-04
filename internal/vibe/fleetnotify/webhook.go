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
	"net/http"
	"net/url"
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
	// Timeout bounds one attempt (default 10s).
	Timeout time.Duration
	// Client overrides the HTTP client (tests).
	Client *http.Client
}

// WebhookSink posts notifications to one endpoint.
type WebhookSink struct {
	url      string
	path     string
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
		timeout = 10 * time.Second
	}
	hc := cfg.Client
	if hc == nil {
		hc = &http.Client{Timeout: timeout}
	}
	return &WebhookSink{
		url:      raw,
		path:     u.Path,
		token:    strings.TrimSpace(cfg.Token),
		format:   format,
		hc:       hc,
		redacted: Redact(raw),
	}, nil
}

// Endpoint returns the redacted endpoint.
func (s *WebhookSink) Endpoint() string { return s.redacted }

// Scrub removes this sink's secrets from an arbitrary string. The path
// is scrubbed as well as the whole URL because an ntfy topic is a path
// segment, and a leaked topic is a leaked credential on its own.
func (s *WebhookSink) Scrub(msg string) string {
	out := strings.ReplaceAll(msg, s.url, redacted)
	if len(s.path) > 1 {
		out = strings.ReplaceAll(out, s.path, redacted)
	}
	if s.token != "" {
		out = strings.ReplaceAll(out, s.token, redacted)
	}
	return out
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
