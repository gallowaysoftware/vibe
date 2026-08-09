package router

// A cloud peer's catalog ids, and the collision that used to be silent.
//
// aliasClaimants excluded cloud_peer defs entirely, so peer model ids
// participated in nothing: not the alias claim set (correct — Render emits
// no aliases for a peer stanza) and not the RESERVATION set (the defect).
// A llama_server def whose alias equalled a peer's model id therefore
// resolved cleanly and rendered two entries under one client-facing id,
// with llama-swap left to serve whichever won.
//
// This is PR #14's rule met a second time: a cloud peer's catalog ids are
// its cloud_peer.models entries, never its def name — so code keyed by
// def.Name and looked up by catalog id works for every other backend kind
// and silently misses for peers. The peers-map clash check in Render covers
// def.Name and only def.Name.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
)

const peerCatalogHosts = `
cells:
  front: {url: "http://front.lan:9000", class: always_on}
  gpu:   {url: "http://gpu.lan:9000", class: always_on}
`

// peerDef builds a cloud_peer def serving the given catalog ids.
func peerDef(name string, models ...string) *profile.BackendDef {
	return &profile.BackendDef{
		Name: name,
		Backend: profile.Backend{
			External: true,
			CloudPeer: &profile.CloudPeerBackend{
				BaseURL:   "https://api.example.com",
				APIKeyEnv: "EXAMPLE_API_KEY",
				Models:    models,
			},
		},
	}
}

// TestResolveAliases_APeerModelIDIsCanonicalAndCannotBeAliased is the
// finding, at the layer that owns it.
//
// The error rather than router.alias_owner arbitration is the decision:
// alias_owner settles which of two ALIAS claimants keeps an alias, and has
// never had anything to say about an alias that equals a canonical id —
// `names[a]` (an alias equal to a def name) has always been unresolvable
// for exactly that reason. A peer's model ids are canonical in the same
// sense, so they take the same path.
func TestResolveAliases_APeerModelIDIsCanonicalAndCannotBeAliased(t *testing.T) {
	peer := peerDef("anthropic", "claude-sonnet-5")
	local := claimDef("gpu-coder", "gpu", false, "claude-sonnet-5")

	_, err := ResolveAliases([]*profile.BackendDef{peer, local})
	if err == nil {
		t.Fatal("an alias equal to a cloud peer's model id resolved cleanly: the render then carries a models: alias and a peers: model under one id, and llama-swap serves whichever wins")
	}
	for _, want := range []string{"gpu-coder", "claude-sonnet-5", "anthropic"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q — an operator has to be able to find both ends of the collision", err, want)
		}
	}

	// Setting alias_owner must NOT make it resolve: an owner declaration
	// arbitrates between claimants, and the peer is not a claimant.
	owned := claimDef("gpu-coder", "gpu", true, "claude-sonnet-5")
	if _, err := ResolveAliases([]*profile.BackendDef{peer, owned}); err == nil {
		t.Fatal("router.alias_owner resolved an alias-versus-canonical-id collision: it would have taken an id off the peer that still renders it")
	}
}

// TestResolveAliases_APeerStillClaimsNoAliases. The exclusion's other half
// stands, and deliberately: Render emits no aliases for a peer stanza, so a
// peer that WON an alias would take it off the def that would actually have
// served it and advertise nothing in its place — the "id nothing can route
// to" failure, arrived at from the other side.
func TestResolveAliases_APeerStillClaimsNoAliases(t *testing.T) {
	peer := peerDef("anthropic", "claude-sonnet-5")
	peer.Router = &profile.RouterOpts{Aliases: []string{"best-coder"}, AliasOwner: true}
	local := claimDef("gpu-coder", "gpu", false, "best-coder")

	winners, err := ResolveAliases([]*profile.BackendDef{peer, local})
	if err != nil {
		t.Fatalf("ResolveAliases: %v", err)
	}
	if got := winners["anthropic"]; len(got) != 0 {
		t.Errorf("the peer won aliases %v; Render emits none for a peer stanza, so the alias would vanish from the catalog entirely", got)
	}
	if got := winners["gpu-coder"]; len(got) != 1 || got[0] != "best-coder" {
		t.Errorf("winners[gpu-coder] = %v, want [best-coder]: the def that can actually serve the alias must keep it", got)
	}
}

// TestRender_TheAliasPeerCollisionSurfacesFromRender walks the whole path:
// the error has to reach `vibe router render`, not just the resolver.
// Checked on both render shapes because a front and a cell build the
// catalog differently (peers: keyed by cell versus models: keyed by def).
//
// It asserts the RESOLVER's wording, not merely that something failed. The
// catalog backstop below would refuse this render too, and a test satisfied
// by either would have gone green with the reservation deleted — the
// mutation harness caught exactly that on the first run. The difference is
// not cosmetic: the resolver's message names the peer and the edit that
// fixes it, and it is the only one that fires for ResolveAliases, which
// fleetd runs over the whole checkout with no render in sight.
func TestRender_TheAliasPeerCollisionSurfacesFromRender(t *testing.T) {
	for _, cell := range []string{fleetcfg.FrontCell, "gpu"} {
		t.Run(cell, func(t *testing.T) {
			defs := []*profile.BackendDef{
				peerDef("anthropic", "claude-sonnet-5"),
				claimDef("gpu-coder", "gpu", false, "claude-sonnet-5"),
			}
			out, err := Render(defs, Options{
				Cell: cell, Hosts: testHosts(t, peerCatalogHosts), LlamaServerBinary: testBinary,
			})
			if err == nil {
				t.Fatalf("render succeeded with two entries claiming \"claude-sonnet-5\":\n%s", out)
			}
			if !strings.Contains(err.Error(), "cloud peer") {
				t.Fatalf("render failed with %q, which is not the alias-versus-peer-id error: the "+
					"collision was caught downstream by the catalog backstop instead of at resolution, "+
					"so ResolveAliases — fleetd's checkout-wide pass, where no render happens — still "+
					"accepts it", err)
			}
		})
	}
}

// TestRender_NoCatalogIDIsAdvertisedTwice is the backstop under the
// resolver, and the reason the fix is not only in aliasClaimants: the
// resolver sees DEFS, so it says nothing about a llama_server def NAMED
// after a peer's model id (no alias anywhere) or about two peers listing
// the same id. Both render two entries under one id in silence.
//
// The check runs over the rendered config — the artifact clients see — so
// it covers the class rather than the two known paths into it.
//
// "The rendered config" has two halves, which is what the extras subtests
// below are here for: what the defs render, and what the extras merge
// then folds into the same maps. This test asserted only the first for
// three phases, so the check it is the backstop for ran only over the
// first, and the front — which never renders without extras — was the
// host the invariant did not hold on.
func TestRender_NoCatalogIDIsAdvertisedTwice(t *testing.T) {
	t.Run("a def named after a peer's model id", func(t *testing.T) {
		// No alias involved anywhere, so alias resolution is not even
		// consulted; this is purely two entries in one catalog.
		defs := []*profile.BackendDef{
			peerDef("anthropic", "claude-sonnet-5"),
			llamaDef("claude-sonnet-5", ""),
		}
		out, err := Render(defs, Options{
			Cell: "gpu", Hosts: testHosts(t, peerCatalogHosts), LlamaServerBinary: testBinary,
		})
		if err == nil {
			t.Fatalf("a local model and a peer both advertise \"claude-sonnet-5\" and the render said nothing:\n%s", out)
		}
		if !strings.Contains(err.Error(), "claude-sonnet-5") {
			t.Errorf("error %q does not name the contested id", err)
		}
	})

	t.Run("two peers listing the same model id", func(t *testing.T) {
		defs := []*profile.BackendDef{
			peerDef("provider-a", "llama-4-405b"),
			peerDef("provider-b", "llama-4-405b"),
		}
		out, err := Render(defs, Options{
			Cell: "gpu", Hosts: testHosts(t, peerCatalogHosts), LlamaServerBinary: testBinary,
		})
		if err == nil {
			t.Fatalf("two peers advertise \"llama-4-405b\" and the render said nothing — the id resolves to whichever provider llama-swap picks:\n%s", out)
		}
		for _, want := range []string{"provider-a", "provider-b"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		}
	})

	t.Run("distinct ids render fine", func(t *testing.T) {
		// The ceiling on all of the above: a legitimate mixed catalog must
		// still render, or this guard is just a refusal to work.
		defs := []*profile.BackendDef{
			peerDef("anthropic", "claude-sonnet-5", "claude-haiku-5"),
			claimDef("gpu-coder", "gpu", false, "best-coder"),
		}
		out, err := Render(defs, Options{
			Cell: "gpu", Hosts: testHosts(t, peerCatalogHosts), LlamaServerBinary: testBinary,
		})
		if err != nil {
			t.Fatalf("a catalog with no duplicate ids was refused: %v", err)
		}
		cfg := parseRendered(t, out)
		if got := cfg.Models["gpu-coder"].Aliases; len(got) != 1 || got[0] != "best-coder" {
			t.Errorf("gpu-coder aliases = %v, want [best-coder]", got)
		}
		if got := cfg.Peers["anthropic"].Models; len(got) != 2 {
			t.Errorf("peer models = %v, want both ids", got)
		}
	})

	// ── the extras half ──────────────────────────────────────────────
	//
	// Every case above renders with no extras file, and that was the
	// gap between what this test's name claimed and what it asserted.
	// The check ran on the config built from defs, BEFORE mergeExtras
	// folded the extras file into the same models:/peers: maps — so the
	// invariant held on every host except the one it matters most on.
	// The front always renders with extras (fleet.front_extras is where
	// its apiKeys come from, AGENTS.md), which makes the front the one
	// config the whole fleet dials and the one config the namespace rule
	// did not cover.
	//
	// mergeExtras has a guard of its own, and it works — for the map KEYS
	// it merges, pinned below so nobody "hardens" the half that was never
	// broken. It looks at nothing INSIDE those keys: an alias list, or a
	// model id under an extras peer, went straight into the catalog.

	t.Run("extras: an alias collides with a def name", func(t *testing.T) {
		out, err := Render([]*profile.BackendDef{llamaDef("alpha", "alpha-legacy")}, Options{
			LlamaServerBinary: testBinary,
			ExtrasPath: extrasFile(t, `models:
  bravo:
    cmd: /bin/true
    ttl: 300
    aliases: [alpha]
`),
		})
		wantExtrasCollision(t, out, err, `"alpha"`, "models.alpha", "alias of models.bravo")
	})

	t.Run("extras: an alias collides with a def's alias", func(t *testing.T) {
		// The escape as filed: three entries advertise alpha-legacy (the
		// def's alias, the extras model's alias, the extras peer's model
		// id) and the render used to exit 0.
		out, err := Render([]*profile.BackendDef{llamaDef("alpha", "alpha-legacy")}, Options{
			LlamaServerBinary: testBinary,
			ExtrasPath: extrasFile(t, `models:
  bravo:
    cmd: /bin/true
    ttl: 300
    aliases: [alpha-legacy]
peers:
  simcell:
    proxy: http://127.0.0.1:9101
    models: [alpha-legacy]
`),
		})
		wantExtrasCollision(t, out, err, `"alpha-legacy"`, "alias of models.alpha", "alias of models.bravo")
	})

	t.Run("extras: two extras aliases collide with each other", func(t *testing.T) {
		// Neither claimant is a def, so nothing upstream of the merged
		// config can see this one at all.
		out, err := Render([]*profile.BackendDef{llamaDef("alpha", "alpha-legacy")}, Options{
			LlamaServerBinary: testBinary,
			ExtrasPath: extrasFile(t, `models:
  bravo:
    cmd: /bin/true
    ttl: 300
    aliases: [shared]
  charlie:
    cmd: /bin/true
    ttl: 300
    aliases: [shared]
`),
		})
		wantExtrasCollision(t, out, err, `"shared"`, "alias of models.bravo", "alias of models.charlie")
	})

	t.Run("extras: a peer's model id collides with a def name", func(t *testing.T) {
		out, err := Render([]*profile.BackendDef{llamaDef("alpha", "alpha-legacy")}, Options{
			LlamaServerBinary: testBinary,
			ExtrasPath: extrasFile(t, `peers:
  simcell:
    proxy: http://127.0.0.1:9101
    models: [alpha]
`),
		})
		wantExtrasCollision(t, out, err, `"alpha"`, "models.alpha", "peers.simcell")
	})

	t.Run("extras: a peer's model id collides with a def's alias", func(t *testing.T) {
		out, err := Render([]*profile.BackendDef{llamaDef("alpha", "alpha-legacy")}, Options{
			LlamaServerBinary: testBinary,
			ExtrasPath: extrasFile(t, `peers:
  simcell:
    proxy: http://127.0.0.1:9101
    models: [alpha-legacy]
`),
		})
		wantExtrasCollision(t, out, err, `"alpha-legacy"`, "alias of models.alpha", "peers.simcell")
	})

	t.Run("extras: a peer's model id collides on the front render", func(t *testing.T) {
		// The front, where this is not hypothetical: its whole catalog is
		// peer stanzas and it always merges extras.
		defs := []*profile.BackendDef{claimDef("gpu-coder", "gpu", false, "best-coder")}
		out, err := Render(defs, Options{
			Cell: fleetcfg.FrontCell, Hosts: testHosts(t, peerCatalogHosts), LlamaServerBinary: testBinary,
			ExtrasPath: extrasFile(t, `peers:
  simcell:
    proxy: http://127.0.0.1:9101
    models: [best-coder]
`),
		})
		wantExtrasCollision(t, out, err, `"best-coder"`, "peers.gpu", "peers.simcell")
	})

	t.Run("extras: a models key collision is still mergeExtras' catch", func(t *testing.T) {
		// The half that already worked, pinned: this must keep failing at
		// the merge, with the message that tells the operator to express
		// it in the def — not fall through to the catalog check.
		_, err := Render([]*profile.BackendDef{llamaDef("alpha", "alpha-legacy")}, Options{
			LlamaServerBinary: testBinary,
			ExtrasPath: extrasFile(t, `models:
  alpha:
    cmd: /bin/true
    ttl: 300
`),
		})
		if err == nil {
			t.Fatal("an extras models: key that shadows a rendered def merged silently")
		}
		for _, want := range []string{"models.alpha", "express it in the def instead"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q — the merge guard is the right layer for a key collision", err, want)
			}
		}
	})

	t.Run("extras: a legitimate file still renders, extras-only sections and all", func(t *testing.T) {
		// The ceiling on the whole extras half. A guard that refuses the
		// front's real extras file would take the fleet down harder than
		// the defect it fixes: apiKeys, arbitrary-cmd tenants, sim peers
		// and routing: all live in this file and none of them are things
		// swapConfig models.
		out, err := Render([]*profile.BackendDef{llamaDef("alpha", "alpha-legacy")}, Options{
			LlamaServerBinary: testBinary,
			ExtrasPath: extrasFile(t, `models:
  bravo:
    cmd: /bin/true
    ttl: 300
    aliases: [bravo-legacy]
peers:
  simcell:
    proxy: http://127.0.0.1:9101
    models: [remote-1]
apiKeys:
  - sk-front-key
routing:
  router:
    use: group
`),
		})
		if err != nil {
			t.Fatalf("a legitimate extras file was refused: %v", err)
		}
		cfg := parseRendered(t, out)
		if got := cfg.Models["alpha"].Aliases; len(got) != 1 || got[0] != "alpha-legacy" {
			t.Errorf("def alpha aliases = %v, want [alpha-legacy] (the merge must not disturb the render)", got)
		}
		if _, ok := cfg.Models["bravo"]; !ok {
			t.Error("extras model bravo missing from the merged config")
		}
		if got := cfg.Peers["simcell"].Models; len(got) != 1 || got[0] != "remote-1" {
			t.Errorf("extras peer models = %v, want [remote-1]", got)
		}
		var whole map[string]any
		if err := yaml.Unmarshal([]byte(out), &whole); err != nil {
			t.Fatalf("merged config is not YAML: %v", err)
		}
		// The reparse the check does is a plain Unmarshal for exactly
		// this reason: KnownFields(true) would reject both of these and
		// take the front's credential with it.
		for _, section := range []string{"routing", "apiKeys"} {
			if _, ok := whole[section]; !ok {
				t.Errorf("extras-only section %q was lost — the catalog check must not filter the merged config", section)
			}
		}
	})

	t.Run("extras: an empty model entry is refused, not panicked on", func(t *testing.T) {
		// `models:\n  ghost:` decodes to a nil *swapModel. The check now
		// reads user-authored bytes, so a hand-written extras file must
		// not be able to crash the renderer — llama-swap gets to have the
		// opinion about a model with no cmd.
		out, err := Render([]*profile.BackendDef{llamaDef("alpha", "alpha-legacy")}, Options{
			LlamaServerBinary: testBinary,
			ExtrasPath: extrasFile(t, `models:
  ghost:
`),
		})
		if err != nil {
			t.Fatalf("an empty extras model entry: %v", err)
		}
		if !strings.Contains(out, "ghost") {
			t.Errorf("the empty entry vanished from the merge:\n%s", out)
		}
	})
}

// extrasFile writes a router-extras file under t.TempDir() and returns
// its path.
//
// Always an explicit path: Options.ExtrasPath defaults, in the CLI, to
// the operator's live ~/.config/vibe/router-extras.yaml, and a unit test
// that reads the running fleet's config has bitten this package before.
func extrasFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "router-extras.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write extras fixture: %v", err)
	}
	return path
}

// wantExtrasCollision asserts the render refused, names the contested id
// and both claimants, and says the duplicate came from the EXTRAS file.
//
// The last part is not decoration. The pre-merge check's message is
// phrased for a defect in the defs; reused verbatim for a collision that
// only exists after the merge, it sends an operator to audit
// ~/.config/vibe/backends/, where every def is innocent and nothing they
// can change will fix it.
func wantExtrasCollision(t *testing.T, out string, err error, want ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("the extras merge put two entries in the catalog under one id and the render said nothing:\n%s", out)
	}
	for _, w := range append(want, "router extras", "front_extras") {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q does not mention %q", err, w)
		}
	}
}
