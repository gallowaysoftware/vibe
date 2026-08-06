// Package observed carries measurements whose ZERO VALUE is "nobody
// looked" (fleet-control C20).
//
// The fleet has lost the same argument six times in twelve phases, always
// in the same shape: a value is paired with a "reported"/"seen"/"known"
// boolean, some later reader takes the value and drops the boolean, and
// the zero that falls out is read as a healthy measurement. C4's warm
// policy treated no-activity as a full idle floor; the v247 wire change
// made len(requests)==0 read as a REPORTED zero and silently disarmed
// eight busy guards; C10's --idle, C16's versions.llama_swap and C13's
// defs parity each found their own version of it.
//
// The pattern is a defect generator rather than a defect: every one of
// those was a correct pair somewhere and a dropped bit somewhere else,
// and every review found it by reading. This type removes the shape.
//
//   - The zero value is UNKNOWN, so a freshly-declared field, a map miss
//     and a `delete` all mean "no evidence" without anyone writing code.
//   - The value is unexported, so there is no way to read it without
//     answering the question: Observed returns the pair, IsKnown asks
//     only about evidence, and OrElse forces the caller to WRITE DOWN the
//     number it wants absence to mean.
//
// The remaining hole is `v, _ := x.Observed()`. That is a deliberate,
// greppable act rather than an accident, and TestObservedIsNeverReadWithA
// DiscardedKnownBit in this package fails on it across the fleet
// packages.
//
// Stdlib only, no reflection, comparable when T is.
package observed

import (
	"encoding/json"
	"fmt"
)

// Value is a measurement that may not have been taken. Its zero value is
// the unknown one; that is the entire point of the type.
type Value[T any] struct {
	v     T
	known bool
}

// Known wraps a measurement that was actually taken. There is
// deliberately no Unknown constructor: the zero value already is one, and
// a constructor would invite `observed.Unknown[int]()` in places a plain
// declaration says the same thing more clearly.
func Known[T any](v T) Value[T] { return Value[T]{v: v, known: true} }

// Observed returns the measurement and whether there is one. It is the
// only way to reach the value with the bit still attached.
func (o Value[T]) Observed() (T, bool) { return o.v, o.known }

// IsKnown reports whether anything was measured. For guards that care
// only about the presence of evidence — "is this cell watched at all" —
// and never about the number.
func (o Value[T]) IsKnown() bool { return o.known }

// OrElse reads an unknown as fallback. Every caller of this is asserting
// that absence and fallback are genuinely the same thing HERE, which is
// occasionally true (a display that renders 0 requests) and is the exact
// mistake this package exists to stop being invisible. Name the fallback
// at the call site and the reviewer can see the claim.
func (o Value[T]) OrElse(fallback T) T {
	if !o.known {
		return fallback
	}
	return o.v
}

// String renders an unknown as "unknown" rather than as a zero, because
// a status line is one of the surfaces the original defect reached.
func (o Value[T]) String() string {
	if !o.known {
		return "unknown"
	}
	return fmt.Sprint(o.v)
}

// MarshalJSON emits null for an unknown. A state document that spelled it
// 0 would hand every downstream reader the same false measurement in a
// format that has a perfectly good word for absence.
func (o Value[T]) MarshalJSON() ([]byte, error) {
	if !o.known {
		return []byte("null"), nil
	}
	return json.Marshal(o.v)
}

// UnmarshalJSON reads null (and an absent field, which never calls this)
// back as unknown.
func (o *Value[T]) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*o = Value[T]{}
		return nil
	}
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*o = Known(v)
	return nil
}
