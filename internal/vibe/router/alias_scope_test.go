package router

import (
	"slices"
	"testing"

	"github.com/gallowaysoftware/vibe/internal/vibe/fleetcfg"
	"github.com/gallowaysoftware/vibe/internal/vibe/profile"
)

// fleet-control C21. The rejected feature is an alias whose target moves
// on a membership transition; these pin the property that makes the
// rejection true in code — an alias is owned by the DECLARED def set, so
// every way a def leaves a render makes its alias DISAPPEAR rather than
// naming a different model.

const aliasScopeHosts = `
cells:
  front:  {url: "http://front.lan:9000", class: always_on}
  laptop: {url: "http://laptop.lan:9000", class: roaming}
  gpu:    {url: "http://gpu.lan:9000", class: always_on}
`

func claimDef(name, cell string, owner bool, aliases ...string) *profile.BackendDef {
	d := llamaDef(name, "")
	d.Cell = cell
	d.Router = &profile.RouterOpts{Aliases: aliases, AliasOwner: owner}
	return d
}

func peerModels(t *testing.T, out, peer string) []string {
	t.Helper()
	return parseRendered(t, out).Peers[peer].Models
}

// TestRenderFront_UnassignedOwnerTakesItsAliasOutOfTheCatalog: the front
// excludes unassigned defs (it owns no models). Before C21 that exclusion
// ran BEFORE alias resolution, so the cell-assigned co-claimant inherited
// an alias its owner still holds.
func TestRenderFront_UnassignedOwnerTakesItsAliasOutOfTheCatalog(t *testing.T) {
	owner := claimDef("legacy-coder", "", true, "best-coder")
	rival := claimDef("gpu-coder", "gpu", false, "best-coder")

	out, err := Render([]*profile.BackendDef{owner, rival}, Options{
		Cell: fleetcfg.FrontCell, Hosts: testHosts(t, aliasScopeHosts),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got, want := peerModels(t, out, "gpu"), []string{"gpu-coder"}; !slices.Equal(got, want) {
		t.Errorf("gpu peer models = %v, want %v (the excluded owner's alias must not transfer)", got, want)
	}
}

// TestRenderFront_TrialOwnerTakesItsAliasOutOfTheCatalog: C18 excludes
// trial defs from the front so an unpromoted candidate is not routable
// fleet-wide. Inheriting its alias would make it routable under another
// name pointing at a different model.
func TestRenderFront_TrialOwnerTakesItsAliasOutOfTheCatalog(t *testing.T) {
	trial := claimDef("candidate", "laptop", true, "best-coder")
	trial.Trial = true
	rival := claimDef("gpu-coder", "gpu", false, "best-coder")

	var warns warnCapture
	out, err := Render([]*profile.BackendDef{trial, rival}, Options{
		Cell: fleetcfg.FrontCell, Hosts: testHosts(t, aliasScopeHosts), Warnf: warns.warnf,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got, want := peerModels(t, out, "gpu"), []string{"gpu-coder"}; !slices.Equal(got, want) {
		t.Errorf("gpu peer models = %v, want %v (a trial def's alias must not transfer)", got, want)
	}
	if !warns.contains("candidate", "trial") {
		t.Errorf("trial exclusion must still warn, got %v", warns.msgs)
	}
}

// TestRenderCell_LoserDoesNotAnswerToAnotherCellsAlias: the cell half of
// the same defect. A cell render sees only its own defs, so the loser used
// to configure its llama-swap to answer to the contested alias — which is
// what let a front-side transfer resolve end to end.
func TestRenderCell_LoserDoesNotAnswerToAnotherCellsAlias(t *testing.T) {
	owner := claimDef("laptop-coder", "laptop", true, "best-coder")
	rival := claimDef("gpu-coder", "gpu", false, "best-coder")

	out, err := Render([]*profile.BackendDef{owner, rival}, Options{
		Cell: "gpu", Hosts: testHosts(t, aliasScopeHosts), LlamaServerBinary: testBinary,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := parseRendered(t, out).Models["gpu-coder"].Aliases; len(got) != 0 {
		t.Errorf("gpu-coder aliases = %v, want none (laptop-coder is the declared owner)", got)
	}

	ownerOut, err := Render([]*profile.BackendDef{owner, rival}, Options{
		Cell: "laptop", Hosts: testHosts(t, aliasScopeHosts), LlamaServerBinary: testBinary,
	})
	if err != nil {
		t.Fatalf("Render laptop: %v", err)
	}
	if got, want := parseRendered(t, ownerOut).Models["laptop-coder"].Aliases, []string{"best-coder"}; !slices.Equal(got, want) {
		t.Errorf("laptop-coder aliases = %v, want %v", got, want)
	}
}

// TestRenderFront_AliasWinnersDropsAnAbsentOwnersAlias is the seam fleetd
// uses: winners computed over the whole checkout, defs filtered afterwards.
// A winner absent from the render appears nowhere at all.
func TestRenderFront_AliasWinnersDropsAnAbsentOwnersAlias(t *testing.T) {
	owner := claimDef("laptop-coder", "laptop", true, "best-coder")
	rival := claimDef("gpu-coder", "gpu", false, "best-coder")

	winners, err := ResolveAliases([]*profile.BackendDef{owner, rival})
	if err != nil {
		t.Fatalf("ResolveAliases: %v", err)
	}
	if got, want := winners["laptop-coder"], []string{"best-coder"}; !slices.Equal(got, want) {
		t.Fatalf("winners[laptop-coder] = %v, want %v", got, want)
	}
	if got := winners["gpu-coder"]; len(got) != 0 {
		t.Fatalf("winners[gpu-coder] = %v, want none", got)
	}

	// The prune: the owner's def never reaches Render.
	out, err := Render([]*profile.BackendDef{rival}, Options{
		Cell: fleetcfg.FrontCell, Hosts: testHosts(t, aliasScopeHosts), AliasWinners: winners,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	cfg := parseRendered(t, out)
	if _, ok := cfg.Peers["laptop"]; ok {
		t.Errorf("pruned cell still has a peer stanza: %v", cfg.Peers)
	}
	if got, want := peerModels(t, out, "gpu"), []string{"gpu-coder"}; !slices.Equal(got, want) {
		t.Errorf("gpu peer models = %v, want %v (the alias must 404, not repoint)", got, want)
	}
}

// TestResolveAliases_CollisionStaysAnErrorWithOneClaimantMissing: the
// unresolvable collision must not heal by attrition. Resolving over the
// survivors made a two-claimant/no-owner config render fine the moment the
// roaming claimant left — the render error disappearing WAS the repoint.
//
// Both halves are exercised, because the delta is the whole point: the
// SURVIVOR set resolves cleanly and hands the alias to whoever is left
// (that is the defect, reproduced), while the DECLARED set — what fleetd
// now passes — still refuses.
func TestResolveAliases_CollisionStaysAnErrorWithOneClaimantMissing(t *testing.T) {
	departed := claimDef("laptop-coder", "laptop", false, "best-coder")
	survivor := claimDef("gpu-coder", "gpu", false, "best-coder")
	declared := []*profile.BackendDef{departed, survivor}
	survivors := []*profile.BackendDef{survivor}

	if _, err := ResolveAliases(declared); err == nil {
		t.Fatal("two claimants, no alias_owner: want an error")
	}
	// The defect, reproduced: over the survivors alone the misconfiguration
	// resolves itself, silently, in the co-claimant's favour.
	repointed, err := ResolveAliases(survivors)
	if err != nil {
		t.Fatalf("resolving over the survivors: %v", err)
	}
	if got, want := repointed["gpu-coder"], []string{"best-coder"}; !slices.Equal(got, want) {
		t.Fatalf("survivor-set resolution = %v, want %v — the premise of this test no longer holds", got, want)
	}
	// fleetd resolves over the checkout, so the error survives the prune.
	winners, err := ResolveAliases(declared)
	if err == nil {
		t.Fatal("want the same error after the roaming claimant is pruned")
	}
	if winners != nil {
		t.Errorf("winners = %v, want nil beside an error", winners)
	}
}

// TestResolveAliases_ComfyUIDefsAreClaimants: RouterOpts is accepted on
// any def and Render turns comfyui defs into models: entries like the
// other two kinds, so a comfyui def both claims its aliases and reserves
// its name. Dropping it from aliasClaimants would delete a declared alias
// from every catalog and let another def claim the name — and no test
// noticed until this one.
func TestResolveAliases_ComfyUIDefsAreClaimants(t *testing.T) {
	comfy := &profile.BackendDef{
		Name: "comfy", Cell: "gpu",
		Router:  &profile.RouterOpts{Aliases: []string{"images"}},
		Backend: profile.Backend{External: true, ComfyUI: &profile.ComfyUIBackend{Dir: "/srv/comfy", Port: 8188}},
	}
	rival := claimDef("gpu-coder", "gpu", false, "images")

	if _, err := ResolveAliases([]*profile.BackendDef{comfy, rival}); err == nil {
		t.Error("two claimants of \"images\" with no owner: want a collision error")
	}
	winners, err := ResolveAliases([]*profile.BackendDef{comfy})
	if err != nil {
		t.Fatalf("ResolveAliases: %v", err)
	}
	if got, want := winners["comfy"], []string{"images"}; !slices.Equal(got, want) {
		t.Errorf("winners[comfy] = %v, want %v (a comfyui def's declared alias must survive)", got, want)
	}
	if _, err := ResolveAliases([]*profile.BackendDef{comfy, claimDef("other", "gpu", false, "comfy")}); err == nil {
		t.Error("an alias equal to a comfyui def's NAME must stay an error (def names are canonical ids)")
	}
}

// TestResolveAliases_ReturnsANonNilMapWithNothingToResolve: Render treats
// a nil AliasWinners as "compute it yourself", which is the pre-C21
// behaviour. fleetd relies on the map being non-nil to keep that fallback
// unreachable, so a fleet that declares no aliases at all must still
// produce a map — otherwise the defect silently re-enables itself on
// exactly the fleets where nobody would look.
func TestResolveAliases_ReturnsANonNilMapWithNothingToResolve(t *testing.T) {
	for _, tc := range []struct {
		name string
		defs []*profile.BackendDef
	}{
		{"no defs", nil},
		{"defs with no aliases", []*profile.BackendDef{claimDef("gpu-coder", "gpu", false)}},
		{"only a cloud peer", []*profile.BackendDef{{
			Name: "anthropic",
			Backend: profile.Backend{External: true, CloudPeer: &profile.CloudPeerBackend{
				BaseURL: "https://api.anthropic.com", APIKeyEnv: "K", Models: []string{"claude-sonnet-5"},
			}},
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			winners, err := ResolveAliases(tc.defs)
			if err != nil {
				t.Fatalf("ResolveAliases: %v", err)
			}
			if winners == nil {
				t.Fatal("ResolveAliases returned a nil map; Render would fall back to resolving over the survivors")
			}
		})
	}
}

// TestResolveAliases_IgnoresCloudPeerDefs: cloud ids come from
// cloud_peer.models, so a cloud def neither claims aliases nor reserves
// its name against one.
func TestResolveAliases_IgnoresCloudPeerDefs(t *testing.T) {
	cloud := &profile.BackendDef{
		Name: "anthropic",
		Backend: profile.Backend{
			External: true,
			CloudPeer: &profile.CloudPeerBackend{
				BaseURL: "https://api.anthropic.com", APIKeyEnv: "ANTHROPIC_API_KEY",
				Models: []string{"claude-sonnet-5"},
			},
		},
	}
	local := claimDef("gpu-coder", "gpu", false, "anthropic")

	winners, err := ResolveAliases([]*profile.BackendDef{cloud, local})
	if err != nil {
		t.Fatalf("ResolveAliases: %v", err)
	}
	if got, want := winners["gpu-coder"], []string{"anthropic"}; !slices.Equal(got, want) {
		t.Errorf("winners[gpu-coder] = %v, want %v", got, want)
	}
}
