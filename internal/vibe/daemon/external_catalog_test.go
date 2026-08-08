package daemon_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gallowaysoftware/vibe/internal/vibe/daemon"
)

// client_api_url points clients at a fleet front instead of this box's own
// router. An external backend has no local process, so readiness is a
// statement about where a rendered frontend will send its requests — the
// front's catalog, not the local cell's. A model only the front serves (a
// cloud peer, another cell's weights) must not fail a check it would pass.
func TestDaemon_ExternalBackend_ReadyFromClientAPIURL(t *testing.T) {
	setupXDG(t)
	stub := stubModel(t)
	writeProfile(t, "ext", externalRouterProfile("ext", stub, "fleet-only"))

	// The local cell knows nothing about this model; the front serves it.
	localPort := fakeRouter(t, "local-cell-model")
	frontPort := fakeRouter(t, "local-cell-model", "fleet-only")

	cfg := externalDaemonConfig(t, localPort)
	cfg.ClientAPIURL = fmt.Sprintf("http://127.0.0.1:%d", frontPort)
	client, _ := startDaemon(t, daemon.New(cfg))

	startCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := client.Start(startCtx, "ext")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _, _ = client.Stop(context.Background()) })
	if res.Status == nil || !res.Status.Ready {
		t.Fatalf("status after Start = %+v; want ready", res.Status)
	}
}

// The mirror image: a local cell that happens to serve the id must not make a
// profile look ready when the front the clients actually dial does not.
func TestDaemon_ExternalBackend_LocalCatalogDoesNotSatisfyClientAPIURL(t *testing.T) {
	setupXDG(t)
	stub := stubModel(t)
	writeProfile(t, "ext", externalRouterProfile("ext", stub, "shadowed"))

	localPort := fakeRouter(t, "shadowed")
	frontPort := fakeRouter(t, "something-else")

	cfg := externalDaemonConfig(t, localPort)
	cfg.ClientAPIURL = fmt.Sprintf("http://127.0.0.1:%d", frontPort)
	client, _ := startDaemon(t, daemon.New(cfg))

	startCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := client.Start(startCtx, "ext")
	if err == nil {
		t.Fatal("Start succeeded on a catalog no client will read")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("127.0.0.1:%d", frontPort)) {
		t.Errorf("error %q must name the front it actually probed", err.Error())
	}
}

// Without client_api_url the probe stays on the loopback router, which is the
// single-box shape every existing deployment runs.
func TestDaemon_ExternalBackend_DefaultsToLoopbackRouter(t *testing.T) {
	setupXDG(t)
	stub := stubModel(t)
	writeProfile(t, "ext", externalRouterProfile("ext", stub, "local-only"))

	routerPort := fakeRouter(t, "local-only")
	client, _ := startDaemon(t, externalDaemon(t, routerPort))

	startCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.Start(startCtx, "ext"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _, _ = client.Stop(context.Background()) })
}
