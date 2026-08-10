package search

// What a fetch URL is allowed to leave behind.
//
// Every URL this package puts in an error or a log line arrives from the
// CALLER — POST /fetch's body, GET /fetch's ?url=, the MCP fetch_url
// tool's argument — and a URL a model was handed is routinely a
// credential. The two shapes that matter here are both ordinary:
//
//   - userinfo, `https://user:pass@host/page`; and
//   - a signed or keyed QUERY, which is what a presigned S3 link
//     (X-Amz-Signature / X-Amz-Credential), a Dropbox or Drive share
//     link, or any `?token=`/`?api_key=` document URL looks like. Those
//     are exactly the URLs an agent gets handed and asked to read.
//
// The log those land in is the operator's, and on this fleet it is
// journald on a host whose logs get pasted into issues. Once a presigned
// URL is in a log line it is a bearer credential sitting in a file with
// different permissions and a different lifetime from the thing it opens.
//
// Two functions, because there are two ways a URL gets into a message and
// closing one does not close the other:
//
//   - redactURL is for the string we format ourselves;
//   - causeWithoutURL is for the copy net/http embeds STRUCTURALLY in
//     *url.Error. Measured: net/http's stripPassword replaces the
//     password and nothing else, so `Get "https://h/p?X-Amz-Signature=…":
//     dial tcp …` carries the signature into any message that wraps the
//     transport error with %w. Redacting our own format argument does not
//     touch it.
//
// This is the same two-guard shape internal/vibe/fleetnotify uses for
// webhook URLs (scrub the string AND unwrap *url.Error), for the same
// reason: neither half covers the other's case. It is NOT the same
// function, deliberately — fleetnotify.Redact drops the path, which is
// right for an ntfy topic (the topic IS the path) and wrong here, where
// the path is the whole diagnostic: an operator reading "fetch failed"
// needs to know WHICH page failed.

import (
	"errors"
	"net/url"
	"strings"
)

// redactedValue stands in for anything withheld. Spelled like
// fleetnotify's marker so a grep for leaked-credential handling finds
// both.
const redactedValue = "<redacted>"

// redactURL renders a caller-supplied URL safe to log or to put in an
// error, keeping the scheme, host and path — which is what identifies the
// page — and withholding the two places a credential rides.
//
// Query VALUES go wholesale rather than by a list of credential-shaped
// parameter names. A name list is a guess about the next provider: it
// would have to know `X-Amz-Signature`, `sig`, `auth`, `access_token`,
// `hmac` and whatever the next share-link dialect calls it, and the one
// it does not know is fail-OPEN and silent. The parameter NAMES are kept,
// so a reader can still tell a signed link from a paginated one, and the
// values are exactly the part nobody diagnoses a fetch failure from.
//
// A bare key with no `=`, and a key with an empty value, are left alone:
// there is nothing there to hide, and rendering `a=<redacted>` for `a=`
// would claim a secret that does not exist.
//
// An unparseable URL is not echoed back. url.Parse accepts nearly
// anything, so reaching this branch means the string is not a URL at all
// — and a string of unknown shape is the one case where "print it, it is
// probably fine" is how a credential gets out.
func redactURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "(unparseable url)"
	}
	return redactParsedURL(u)
}

// redactParsedURL is redactURL for a URL that is already parsed — the
// form a *url.URL reaches a log line in.
func redactParsedURL(u *url.URL) string {
	if u == nil {
		return "(no url)"
	}
	// Copy: this must not mutate a URL the caller still holds, least of
	// all one an in-flight http.Request is built from.
	c := *u
	if c.RawQuery != "" {
		parts := strings.Split(c.RawQuery, "&")
		for i, p := range parts {
			k, v, ok := strings.Cut(p, "=")
			if !ok || v == "" {
				continue
			}
			parts[i] = k + "=" + redactedValue
		}
		c.RawQuery = strings.Join(parts, "&")
	}
	// Redacted() is String() with any userinfo PASSWORD replaced. The
	// username survives, which is what the stdlib does and is the accepted
	// line — a username is an identifier, not a bearer.
	return c.Redacted()
}

// causeWithoutURL returns the transport failure inside a *url.Error,
// dropping the URL the stdlib embedded in it.
//
// The wrapper is unwrapped rather than string-scrubbed because the URL in
// it is not always the one we formatted: http.Client follows redirects
// and names the hop it failed on, so a scrub keyed on the caller's own
// URL cannot match it. Unwrapping does not depend on the two spellings
// agreeing.
//
// The returned error is the *cause*, so errors.Is/As keep working through
// it — url.Error's own Unwrap returns the same value.
func causeWithoutURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}
