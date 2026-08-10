package search

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
)

// ── the front door's credential compare ──────────────────────────────

// TestBearerCompareIsConstantTime is a STRUCTURAL test, and it is
// structural because the property is not observable from outside.
//
// A timing test would have to measure the difference between a compare
// that stops at byte 1 and one that stops at byte 2 across an HTTP round
// trip on a shared CI runner. That measurement is noise, so a behavioural
// test here would either be flaky or — far worse — pass regardless, which
// is the same as not having one. The shape of the comparison is the whole
// of the property, so the shape is what is asserted.
//
// Two halves, because either alone is satisfiable by the defect:
// ConstantTimeCompare must be CALLED (mentioning the symbol is not using
// it), and no `==`/`!=` may compare the token against another value. The
// `s.Token == ""` configured/not-configured check is deliberately still
// allowed — it compares against a LITERAL, leaks nothing, and refusing it
// would push the next author into a worse spelling.
func TestBearerCompareIsConstantTime(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "server.go", nil, 0)
	if err != nil {
		t.Fatalf("parse server.go: %v", err)
	}
	var withAuth *ast.FuncDecl
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "withAuth" {
			withAuth = fn
		}
	}
	if withAuth == nil {
		// The inertness floor: a scan that finds nothing must not pass.
		t.Fatal("withAuth is not declared in server.go — this test has come detached from the code it guards")
	}

	var constantTimeCalls int
	var leakyCompares []string
	ast.Inspect(withAuth, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			if sel, ok := node.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "ConstantTimeCompare" {
				constantTimeCalls++
			}
		case *ast.BinaryExpr:
			if node.Op != token.EQL && node.Op != token.NEQ {
				return true
			}
			for _, pair := range [][2]ast.Expr{{node.X, node.Y}, {node.Y, node.X}} {
				sel, ok := pair[0].(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Token" {
					continue
				}
				if _, isLiteral := pair[1].(*ast.BasicLit); isLiteral {
					// `s.Token == ""` — the is-auth-configured check.
					continue
				}
				leakyCompares = append(leakyCompares, fset.Position(node.Pos()).String())
			}
		}
		return true
	})

	if constantTimeCalls == 0 {
		t.Error("withAuth does not CALL subtle.ConstantTimeCompare. A byte-wise `==` on the bearer " +
			"returns at the first differing byte, so the endpoint answers a near-miss measurably " +
			"later than a miss — 32*256 requests to walk a 32-byte token out of a service whose " +
			"documented deployment is --bind 0.0.0.0 and whose credential unlocks fetch_url and the " +
			"operator's paid search quota.")
	}
	for _, pos := range leakyCompares {
		t.Errorf("%s: the bearer token is compared with == or != against a non-literal. That is the "+
			"variable-time compare this test exists to keep out; use subtle.ConstantTimeCompare.", pos)
	}
}

// ── the caller's URL never lands in a log ────────────────────────────

const (
	// A presigned link's signature: the credential shape that rides in a
	// QUERY PARAMETER, which is exactly what url.URL.Redacted() does NOT
	// touch.
	testSignature = "sig-9f8e7d6c5b4a3e2d1c"
	// And the shape Redacted() does touch.
	testPassword = "pw-s3cr3t-4f2a"
)

func credentialURL(host string) string {
	return "http://svc:" + testPassword + "@" + host + "/reports/q3.pdf?X-Amz-Signature=" + testSignature + "&page=2"
}

// TestRedactURLWithholdsUserinfoAndQueryValues pins the helper itself.
func TestRedactURLWithholdsUserinfoAndQueryValues(t *testing.T) {
	t.Run("userinfo and query values go, the page identity stays", func(t *testing.T) {
		got := redactURL(credentialURL("files.example.com"))
		for _, secret := range []string{testPassword, testSignature} {
			if strings.Contains(got, secret) {
				t.Errorf("redactURL kept %q: %s", secret, got)
			}
		}
		// Withholding everything would be safe and useless. An operator
		// reading "fetch failed" has to be able to tell WHICH page failed,
		// and a reader has to be able to tell a signed link from a
		// paginated one, so the parameter NAMES stay.
		for _, want := range []string{"files.example.com", "/reports/q3.pdf", "X-Amz-Signature=", "page="} {
			if !strings.Contains(got, want) {
				t.Errorf("redactURL dropped %q, leaving nothing to diagnose from: %s", want, got)
			}
		}
	})

	t.Run("an empty value is not a secret", func(t *testing.T) {
		got := redactURL("https://example.com/p?a=&flag")
		if strings.Contains(got, redactedValue) {
			t.Errorf("got %s — `a=` and a bare `flag` carry nothing, and marking them redacted "+
				"claims a secret that is not there", got)
		}
	})

	t.Run("an unparseable string is not echoed back", func(t *testing.T) {
		// url.Parse accepts nearly anything, so reaching this branch means
		// the input is not a URL at all — the one case where "print it, it
		// is probably fine" is how a credential gets out.
		got := redactURL("http://example.com/p?token=" + testSignature + "%zz")
		if strings.Contains(got, testSignature) {
			t.Errorf("redactURL echoed an unparseable string containing a credential: %s", got)
		}
	})
}

// TestDirectFetchErrorDoesNotCarryTheCallersCredential is the half that
// redacting our own format argument does NOT cover.
//
// net/http embeds the request URL STRUCTURALLY in *url.Error, and its
// stripPassword replaces the password and nothing else — so the query
// survives into `Get "http://svc:***@host/p?X-Amz-Signature=…": dial tcp
// …` and into every message that wraps that error with %w. Measured with
// only the format argument redacted, the signature was still in the error
// text. See causeWithoutURL in redact.go.
//
// The target is a loopback port with nothing on it, so the failure is a
// deterministic connection refusal that never leaves the machine. The
// fetcher is built with allowPrivate because the dial guard would
// otherwise refuse loopback before the transport error this test is about
// could happen.
func TestDirectFetchErrorDoesNotCarryTheCallersCredential(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	_, err = newDirectFetcher(true).Fetch(t.Context(), credentialURL(addr))
	if err == nil {
		t.Fatal("fetch of a closed port succeeded; the rig proves nothing")
	}
	msg := err.Error()
	for _, secret := range []string{testPassword, testSignature} {
		if strings.Contains(msg, secret) {
			t.Errorf("the transport error carries %q — this string reaches the operator's journal "+
				"and the /fetch response body: %s", secret, msg)
		}
	}
	if !strings.Contains(msg, "/reports/q3.pdf") {
		t.Errorf("the error no longer says which page failed: %s", msg)
	}
}

// TestFetchLogsWithholdTheCallersCredential is the same property one layer
// out, at the two log lines the HTTP surface writes. Both the success and
// the failure line are asserted because they are separate call sites and
// this repo's recurring defect class is a guard present in one of N of
// them.
func TestFetchLogsWithholdTheCallersCredential(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fetcher *stubFetcher
	}{
		{"success", &stubFetcher{name: "direct", doc: &Document{Text: strings.Repeat("prose ", 200)}}},
		{"failure", &stubFetcher{name: "direct", err: &testError{msg: "upstream said no"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logbuf bytes.Buffer
			srv := &Server{
				Provider: &stubProvider{resp: &Response{}},
				Fetcher:  tc.fetcher,
				Log:      slog.New(slog.NewTextHandler(&logbuf, nil)),
			}
			ts := newTestServer(t, srv)

			target := credentialURL("files.example.com")
			body, _ := json.Marshal(fetchRequest{URL: target})
			resp, err := http.Post(ts.URL+"/fetch", "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatalf("POST /fetch: %v", err)
			}
			defer resp.Body.Close()

			logged := logbuf.String()
			if logged == "" {
				t.Fatal("nothing was logged; the assertion below would pass vacuously")
			}
			for _, secret := range []string{testPassword, testSignature} {
				if strings.Contains(logged, secret) {
					t.Errorf("the log carries %q: %s", secret, logged)
				}
			}
			if !strings.Contains(logged, "files.example.com") {
				t.Errorf("the log no longer names the host, so it diagnoses nothing: %s", logged)
			}
		})
	}
}

// TestMCPFetchErrorWithholdsTheCallersCredential covers the third call
// site. A tool result is not a private channel — it is written into a
// transcript and harnesses log tool results where they log everything
// else.
func TestMCPFetchErrorWithholdsTheCallersCredential(t *testing.T) {
	ts := newTestServer(t, &Server{
		Provider: &stubProvider{resp: &Response{}},
		Fetcher:  &stubFetcher{name: "direct", err: &testError{msg: "upstream said no"}},
	})
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "fetch_url",
			"arguments": map[string]any{"url": credentialURL("files.example.com")},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := mcpCall(t, ts.URL, string(body))
	result, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %v", out)
	}
	if isErr, _ := result["isError"].(bool); !isErr {
		t.Fatalf("tool call did not fail; the assertion below would pass vacuously: %v", result)
	}
	text, _ := json.Marshal(result["content"])
	for _, secret := range []string{testPassword, testSignature} {
		if strings.Contains(string(text), secret) {
			t.Errorf("the tool result carries %q: %s", secret, text)
		}
	}
}

// ── the upstream the OPERATOR chose ──────────────────────────────────

// TestSearxngSearchErrorDoesNotCarryTheOperatorsUpstream is the tenth
// site, and the one whose URL is not the caller's.
//
// Every other URL in this package arrives from whoever is calling; this
// one is --search-upstream, a line of the operator's config that the
// shipped zero-cost deployment points at a SearXNG container on a private
// network and that a basic-auth-fronted instance carries userinfo in.
// `fmt.Errorf("searxng: %w", err)` published net/http's structural copy of
// it — scheme, userinfo, internal host, port, path and the caller's query
// — to three audiences at once: the operator's journal, the 502 body GET
// /search hands anyone holding the bearer token, and the MCP tool result
// that lands in a model's transcript.
//
// The rig is a closed loopback port, so the failure is a deterministic
// connection refusal that never leaves the machine.
//
// The stated limit, so nobody reads this test as promising more than it
// does: what is removed is the URL net/http embedded. Whatever the
// TRANSPORT names in its own message can remain — a dial failure quotes
// the resolved address, and a DNS failure quotes the name it could not
// resolve. Withholding that too would mean discarding the transport error
// entirely, which leaves "searxng: request failed" and nothing to act on.
func TestSearxngSearchErrorDoesNotCarryTheOperatorsUpstream(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// The operator's upstream, with the credential shape a basic-auth
	// SearXNG deployment actually uses.
	up := newSearxngUpstream("http://svc:" + testPassword + "@" + addr + "/searxng-private")
	_, err = up.Search(t.Context(), Query{Text: "quarterly-revenue-leak"})
	if err == nil {
		t.Fatal("search against a closed port succeeded; the rig proves nothing")
	}
	msg := err.Error()
	for _, secret := range []string{
		testPassword,
		// The username is half a credential and an identifier the caller
		// has no business learning.
		"svc:",
		// The operator's private path, which names their deployment.
		"/searxng-private",
		// And the caller is not the only caller: one client's query text
		// must not ride out in another's error.
		"quarterly-revenue-leak",
	} {
		if strings.Contains(msg, secret) {
			t.Errorf("the searxng transport error carries %q — this string is the 502 body, the MCP "+
				"tool result and the operator's journal line: %s", secret, msg)
		}
	}
	// Withholding everything would be safe and useless: the operator has
	// to be able to tell a refused upstream from a decode failure.
	if !strings.Contains(msg, "searxng") {
		t.Errorf("the error no longer says which provider failed: %s", msg)
	}
	if !strings.Contains(msg, "refused") {
		t.Errorf("the error no longer says what the transport did: %s", msg)
	}
}
