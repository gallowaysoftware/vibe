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
	"strings"
	"testing"

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
}
