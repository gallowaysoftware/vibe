package profile

import (
	"slices"
	"strings"
	"testing"
)

// http_server.bind exists so a service the rest of the fleet depends on can
// actually be reached. The default must stay loopback — a sidecar that
// silently started listening on every interface would be a regression with
// security consequences, not a convenience.
func TestHTTPServerBindDefaultsToLoopback(t *testing.T) {
	p := &Profile{Backend: Backend{HTTPServer: &HTTPServerBackend{
		Image: "example/img:latest", Port: 14002,
	}}}
	spec, err := HTTPServerSpec(p, "svc")
	if err != nil {
		t.Fatalf("build spec: %v", err)
	}
	if got := publishFlag(spec.Args); got != "127.0.0.1:14002:14002" {
		t.Errorf("publish flag = %q, want loopback default", got)
	}
}

func TestHTTPServerBindOverridesPublishAddress(t *testing.T) {
	p := &Profile{Backend: Backend{HTTPServer: &HTTPServerBackend{
		Image: "example/img:latest", Port: 14003, ContainerPort: 14003, Bind: "0.0.0.0",
	}}}
	spec, err := HTTPServerSpec(p, "svc")
	if err != nil {
		t.Fatalf("build spec: %v", err)
	}
	if got := publishFlag(spec.Args); got != "0.0.0.0:14003:14003" {
		t.Errorf("publish flag = %q, want the configured bind address", got)
	}
}

// Binary mode has no docker publish to shape, so accepting bind there would
// look like it worked while changing nothing.
func TestHTTPServerBindRejectedInBinaryMode(t *testing.T) {
	err := validateHTTPServer(&HTTPServerBackend{Binary: "/usr/bin/true", Port: 14004, Bind: "0.0.0.0"})
	if err == nil {
		t.Fatal("expected an error for bind in binary mode")
	}
	if !strings.Contains(err.Error(), "docker mode") {
		t.Errorf("err = %v, want it to name the docker-mode constraint", err)
	}
}

// "0.0.0.0:8080" is the natural typo; caught here it names the field, caught
// by docker it is an opaque parse error at start time.
func TestHTTPServerBindRejectsHostPortForm(t *testing.T) {
	for _, bad := range []string{"0.0.0.0:8080", "http://0.0.0.0", "0.0.0.0 "} {
		err := validateHTTPServer(&HTTPServerBackend{Image: "x:1", Port: 1, Bind: bad})
		if err == nil {
			t.Errorf("bind %q was accepted, want a validation error", bad)
		}
	}
}

func TestHTTPServerBindValidInDockerMode(t *testing.T) {
	if err := validateHTTPServer(&HTTPServerBackend{Image: "x:1", Port: 1, Bind: "0.0.0.0"}); err != nil {
		t.Errorf("bind in docker mode should be valid, got %v", err)
	}
}

// ${VIBE_SEARCH} lets one profile point a harness at all three pieces of
// infrastructure: models, search, and fetch.
func TestExpandVibeSearch(t *testing.T) {
	ctx := ExpandContext{VibeAPI: "http://hum:9000/v1", VibeSearch: "http://search:14003"}
	out, err := ExpandTemplate(map[string]any{
		"searxng": map[string]any{"endpoint": "${VIBE_SEARCH}"},
		"mcp":     map[string]any{"url": "${VIBE_SEARCH}/mcp"},
	}, ctx)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if got := out["searxng"].(map[string]any)["endpoint"]; got != "http://search:14003" {
		t.Errorf("endpoint = %v", got)
	}
	if got := out["mcp"].(map[string]any)["url"]; got != "http://search:14003/mcp" {
		t.Errorf("mcp url = %v", got)
	}
}

// An unconfigured retrieval plane must fail the render, not quietly produce
// an empty URL. A harness pointed at "" is far harder to diagnose than a
// start that refuses and says why.
func TestExpandVibeSearchUnconfiguredFailsWithHint(t *testing.T) {
	_, err := ExpandTemplate(map[string]any{"endpoint": "${VIBE_SEARCH}"}, ExpandContext{})
	if err == nil {
		t.Fatal("expected an error when search_url is unset")
	}
	if !strings.Contains(err.Error(), "not configured") || !strings.Contains(err.Error(), "search_url") {
		t.Errorf("err = %v, want it to name search_url as the fix", err)
	}
}

// A genuine typo must not be reported as "not configured" — different
// problem, different fix.
func TestExpandUnknownVariableStillReadsAsTypo(t *testing.T) {
	_, err := ExpandTemplate(map[string]any{"x": "${VIBE_SERCH}"}, ExpandContext{})
	if err == nil {
		t.Fatal("expected an error for an unknown variable")
	}
	if !strings.Contains(err.Error(), "unknown template variable") {
		t.Errorf("err = %v, want an unknown-variable error", err)
	}
}

func TestExpandVibeSearchInFrontendEnv(t *testing.T) {
	p := &Profile{Frontend: Frontend{Env: map[string]string{
		"SEARXNG_ENDPOINT": "${VIBE_SEARCH}",
	}}}
	env, err := p.ExpandEnv(ExpandContext{VibeSearch: "http://search:14003"})
	if err != nil {
		t.Fatalf("ExpandEnv: %v", err)
	}
	if env["SEARXNG_ENDPOINT"] != "http://search:14003" {
		t.Errorf("env = %v", env)
	}
}

func publishFlag(args []string) string {
	i := slices.Index(args, "-p")
	if i < 0 || i+1 >= len(args) {
		return ""
	}
	return args[i+1]
}
