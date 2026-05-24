package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vamp"
)

func TestEnsureServicesUp_AllReachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	p := &vamp.Pipeline{
		Name: "test",
		RequiredServices: []vamp.ServiceRequirement{
			{Name: "svc-a", URL: srv.URL, SetupHint: "vibe start svc-a"},
		},
	}
	var buf bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ensureServicesUp(ctx, p, &buf); err != nil {
		t.Fatalf("ensureServicesUp returned %v; want nil (service is up)", err)
	}
	if strings.Contains(buf.String(), "starting now") {
		t.Errorf("ensureServicesUp should not have logged a start when service was already up; got: %q", buf.String())
	}
}

func TestEnsureServicesUp_NoServices(t *testing.T) {
	p := &vamp.Pipeline{Name: "no-svc"}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	var buf bytes.Buffer
	if err := ensureServicesUp(ctx, p, &buf); err != nil {
		t.Fatalf("ensureServicesUp(no services) returned %v; want nil", err)
	}
}

func TestEnsureServicesUp_UnreachableNoHint(t *testing.T) {
	p := &vamp.Pipeline{
		Name: "no-hint",
		RequiredServices: []vamp.ServiceRequirement{
			// Reserved-for-test port that won't answer.
			{Name: "svc-unknown", URL: "http://127.0.0.1:1", SetupHint: "docker compose up -d svc-unknown"},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var buf bytes.Buffer
	err := ensureServicesUp(ctx, p, &buf)
	if err == nil {
		t.Fatalf("ensureServicesUp should fail on unreachable service with no parseable hint; got nil")
	}
	if !strings.Contains(err.Error(), "svc-unknown") || !strings.Contains(err.Error(), "docker compose") {
		t.Errorf("ensureServicesUp error should surface the service name + the verbatim setup_hint; got: %v", err)
	}
}

func TestEnsureServicesUp_EmptyURLSkipped(t *testing.T) {
	// Services without a URL can't be probed — vamp treats them as
	// "informational" (e.g. an external API the user owns). Don't fail.
	p := &vamp.Pipeline{
		Name: "no-url",
		RequiredServices: []vamp.ServiceRequirement{
			{Name: "svc-informational", URL: "", SetupHint: "vibe start whatever"},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	var buf bytes.Buffer
	if err := ensureServicesUp(ctx, p, &buf); err != nil {
		t.Fatalf("ensureServicesUp(empty URL) returned %v; want nil", err)
	}
}
