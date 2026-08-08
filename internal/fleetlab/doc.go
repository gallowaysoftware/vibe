// Package fleetlab holds the mechanical gate for scripts/fleetlab's port
// isolation (fleet-control C23).
//
// The harness under scripts/fleetlab is shell, and the property that
// matters about it is not one a shell rig can assert about itself: that
// ONE lab instance's `lab.sh down` cannot reach ANOTHER instance's
// processes. Proving that needs two instances, real processes wearing the
// command lines the sweep matches on, and a teardown watched from
// outside. That is a test, and Go is where this repo's tests are.
//
// Why it is worth a package at all. `down`'s sweep used to reap
// llama-server children by a port-range regex — `--port
// (59[89][0-9]|60[01][0-9])` — that every instance shared, because every
// instance had the same ports. A second lab on the box was therefore
// entitled to kill the first's upstreams, and the damage does not surface
// where it happened: it surfaces in the other operator's session as a
// gate that fails for a reason nowhere in their diff. It cost this plan
// C16's L4 gate outright. FLEETLAB_PORT_BASE gives each instance its own
// derived window; this package is what makes "cannot reach" a checked
// claim rather than a comment in a shell script.
package fleetlab
