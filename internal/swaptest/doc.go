// Package swaptest is a wire-versioned llama-swap double: one fake that
// every package in this module can drive, plus the recorded bytes that
// prove the fake still resembles the real thing.
//
// # Why a shared package rather than a test helper
//
// A helper in a _test.go file is invisible outside its own package, and
// this module has ~50 test files that each hand-rolled a llama-swap
// stand-in. They disagreed with each other and with reality — the inflight
// frames they served carried no request `id`, which the real v239 wire
// always sends and which v240+ made load-bearing. A non-test package is
// compiled by `go build ./...` and linted like any other file, so it
// cannot rot quietly.
//
// Two constraints follow from being a non-test package and are deliberate:
//
//   - It MUST NOT import "testing". Doing so registers the -test.* flag set
//     into every binary that links it. TB below is the small slice of
//     *testing.T this package needs, and *testing.T satisfies it.
//   - It uses net.Listen + http.Server rather than httptest.NewServer.
//     httptest registers an -httptest.serve flag on the default FlagSet,
//     and fault injection needs the raw listener anyway: closing a
//     connection between two SSE frames is not something httptest exposes.
//
// # The fidelity contract
//
// Fixtures under fixtures/<version>/ are RAW RECORDED BYTES, captured off
// the wire by TestRecord and checked in verbatim. They are deliberately not
// re-encoded production structs: a fixture that round-trips through the
// same json tags the decoder reads cancels out a tag typo, and any field
// llama-swap sends that our struct omits is structurally invisible.
//
// The double GENERATES frames (it has to — a replayed recording cannot
// answer a request a test just made), but it generates them in the recorded
// encoding, and TestFixtureShapeMatchesGenerated fails if the two drift.
// The recording is the authority; the generator is the convenience.
//
// # Wire versions
//
// Wire selects which llama-swap protocol the double speaks. The difference
// that matters is the inflight frame:
//
//	WireV239  {"requests":[{...},{...}]}                       full list, every edge
//	WireV247  {"operation":"upsert","request":{...}}            delta
//	          {"operation":"remove","id":"93376"}               delta
//	          {"operation":"snapshot","requests":[...]}         at connect
//
// Flipping swaptest.Default re-runs every test in the module against the
// other contract. That is the whole point of the package: "what breaks when
// we upgrade the fleet?" should be a one-line experiment, not a field trip.
package swaptest
