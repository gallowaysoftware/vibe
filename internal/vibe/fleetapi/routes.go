package fleetapi

import "net/http"

// The route registry (fleet-control C12). One table is both what
// Register mounts and what each route grants to a caller who does not
// hold the daemon's control-plane token. The two used to be different
// things in different packages: routes were registered here and the
// single bearer exemption was a string literal in daemon/auth.go, which
// is fine for one exemption and silently wrong the moment there are two
// lists that must agree.
//
// The enforcement still lives in the daemon's bearer middleware — this
// package has never known about tokens and does not now. What it owns is
// the DECLARATION, next to the mount, so adding a route and deciding
// what it grants is one edit in one file.

// Access is what a route grants to a caller without the control-plane
// token.
//
// There is deliberately no safe zero value. A route added without a
// decision is AccessUndecided, which AccessFor treats as token-only and
// TestRoutes_EveryRouteDeclaresAnAccessLevel fails on: "the next agent
// forgot" must not be spelled the same way as "the next agent decided".
type Access int

const (
	// AccessUndecided is the zero value and is never a valid declaration.
	AccessUndecided Access = iota
	// AccessTokenOnly is the default posture: the control-plane token and
	// nothing else. Every route not in this table is treated as this.
	AccessTokenOnly
	// AccessGuest additionally honors the guest read-only bearer (C12).
	// Instantaneous fleet STATE only — never history (the usage ledger,
	// the savings screen), never a mutation, never the announce endpoint
	// (design §6: a cell's voice is a fleet-wide write dressed as a
	// read).
	AccessGuest
	// AccessPublic needs no token at all. Exactly one route is public —
	// the fleet page, a static asset with no fleet data in it, which must
	// load in a bare browser tab so its own token prompt can run (C5).
	// Widening this is how the one hole becomes several.
	AccessPublic
)

func (a Access) String() string {
	switch a {
	case AccessTokenOnly:
		return "token-only"
	case AccessGuest:
		return "guest"
	case AccessPublic:
		return "public"
	default:
		return "UNDECIDED"
	}
}

// Route is one mounted endpoint and what it grants. Method and Path are
// matched EXACTLY: the middleware compares them against r.Method and
// r.URL.Path before the mux cleans anything, so a trailing slash, a
// traversal segment, a percent-encoded separator and a doubled leading
// slash are all misses, and a miss is a 401.
type Route struct {
	Method string
	Path   string
	Access Access
}

type route struct {
	Route
	handler func(*Server) http.HandlerFunc
	// enabled reports whether this route exists on a given server. Nil
	// means always. The access declaration does not depend on it: a route
	// that is not mounted 404s, and a route that is mounted grants what
	// the table says regardless of role.
	enabled func(*Server) bool
}

func fleetdRole(s *Server) bool    { return s.intentPath != "" }
func leaseStore(s *Server) bool    { return s.leasePath != "" }
func usageLedgerOn(s *Server) bool { return s.usage != nil }

// routeTable is the single source of truth for the fleet HTTP surface.
// Add a route here or it is not mounted (test-pinned: no other file in
// this package may call mux.HandleFunc).
func routeTable() []route {
	return []route{
		{
			Route:   Route{Method: http.MethodGet, Path: "/api/fleet/state", Access: AccessGuest},
			handler: func(s *Server) http.HandlerFunc { return s.handleState },
		},
		{
			Route:   Route{Method: http.MethodGet, Path: "/api/fleet/events", Access: AccessGuest},
			handler: func(s *Server) http.HandlerFunc { return s.handleEvents },
		},
		// The intent endpoint exists only when the intent store is enabled
		// (fleetd role) — a single-box daemon has no fleet intent to record.
		{
			Route:   Route{Method: http.MethodPost, Path: "/api/fleet/intent", Access: AccessTokenOnly},
			handler: func(s *Server) http.HandlerFunc { return s.handleIntent },
			enabled: fleetdRole,
		},
		{
			Route:   Route{Method: http.MethodPost, Path: "/api/fleet/wake", Access: AccessTokenOnly},
			handler: func(s *Server) http.HandlerFunc { return s.handleWake },
			enabled: fleetdRole,
		},
		{
			Route:   Route{Method: http.MethodPost, Path: "/api/fleet/announce", Access: AccessTokenOnly},
			handler: func(s *Server) http.HandlerFunc { return s.handleAnnounce },
			enabled: fleetdRole,
		},
		// C9. The scope route exists whether or not a webhook is
		// configured (declaring away before wiring a sink is harmless);
		// the send route answers 503 without one.
		{
			Route:   Route{Method: http.MethodPost, Path: "/api/fleet/notify/scope", Access: AccessTokenOnly},
			handler: func(s *Server) http.HandlerFunc { return s.handleNotifyScope },
			enabled: fleetdRole,
		},
		{
			Route:   Route{Method: http.MethodPost, Path: "/api/fleet/notify/send", Access: AccessTokenOnly},
			handler: func(s *Server) http.HandlerFunc { return s.handleNotifySend },
			enabled: fleetdRole,
		},
		{
			Route:   Route{Method: http.MethodGet, Path: fleetPagePath, Access: AccessPublic},
			handler: fleetPageHandler,
			enabled: fleetdRole,
		},
		{
			Route:   Route{Method: http.MethodGet, Path: "/api/fleet/leases", Access: AccessTokenOnly},
			handler: func(s *Server) http.HandlerFunc { return s.handleLeases },
			enabled: leaseStore,
		},
		{
			Route:   Route{Method: http.MethodPost, Path: "/api/fleet/lease", Access: AccessTokenOnly},
			handler: func(s *Server) http.HandlerFunc { return s.handleLeaseMutate },
			enabled: leaseStore,
		},
		{
			Route:   Route{Method: http.MethodDelete, Path: "/api/fleet/lease", Access: AccessTokenOnly},
			handler: func(s *Server) http.HandlerFunc { return s.handleLeaseMutate },
			enabled: leaseStore,
		},
		// C7a's ledger and C7b's pricing of it are the fleet's HISTORY:
		// tokens per cell per day is a record of when this house works and
		// when it was away, and the savings screen adds what the hardware
		// cost and what the power costs. A guest sees what the fleet is
		// doing now, not what the household has done over time.
		{
			Route:   Route{Method: http.MethodGet, Path: "/api/fleet/usage", Access: AccessTokenOnly},
			handler: func(s *Server) http.HandlerFunc { return s.handleUsage },
			enabled: usageLedgerOn,
		},
		// Deliberately its own route rather than a field on
		// /api/fleet/state: every SSE line debounce-refreshes state, and a
		// ledger aggregate would recompute for a screen nobody is looking
		// at. Every mutation still goes through POST /mcp.
		{
			Route:   Route{Method: http.MethodGet, Path: "/api/fleet/savings", Access: AccessTokenOnly},
			handler: func(s *Server) http.HandlerFunc { return s.handleSavings },
			enabled: usageLedgerOn,
		},
	}
}

// accessByRoute is the middleware's lookup: exact (method, path) keys,
// built once from the table.
var accessByRoute = func() map[string]Access {
	m := make(map[string]Access)
	for _, rt := range routeTable() {
		m[rt.Method+" "+rt.Path] = rt.Access
	}
	return m
}()

// AccessFor reports what the route at (method, path) grants. It is a
// positive allowlist: anything not declared — every non-fleetapi route,
// /mcp and the whole Connect RPC mount included, plus every route a
// future phase adds without a decision — is AccessTokenOnly.
//
// The comparison is exact on both fields and performed on the RAW
// request path, before the mux cleans it. Do not add prefix matching,
// path.Clean, case folding or a regexp here: each of those turns one
// declared hole into a family of them (C5, and its six pinned bypass
// attempts).
func AccessFor(method, path string) Access {
	a, ok := accessByRoute[method+" "+path]
	if !ok || a == AccessUndecided {
		return AccessTokenOnly
	}
	return a
}

// Routes returns the declared surface. Tests walk it so that a route
// added by a later phase is probed against the guest boundary without
// anyone remembering to extend a hand-maintained list.
func Routes() []Route {
	tbl := routeTable()
	out := make([]Route, 0, len(tbl))
	for _, rt := range tbl {
		out = append(out, rt.Route)
	}
	return out
}

// Register mounts the fleet endpoints on the daemon's existing mux. Auth
// is the mux wrapper's business (bearer middleware on TCP, none on
// unix); what this package declares is which of the two tokens each
// route answers to.
func (s *Server) Register(mux *http.ServeMux) {
	for _, rt := range routeTable() {
		if rt.enabled != nil && !rt.enabled(s) {
			continue
		}
		mux.HandleFunc(rt.Method+" "+rt.Path, rt.handler(s))
	}
}
