// Package sample is fixture source for astscan's own tests. It lives
// under testdata so the toolchain never builds it; astscan parses it.
package sample

func guard() bool { return true }
func send()       {}

// Guarded is the shape the rule wants: it sends, and it checks first.
func Guarded() {
	if guard() {
		return
	}
	send()
}

// Unguarded is the defect the rule exists to find.
func Unguarded() {
	send()
}

// ExemptOnPurpose triggers and does not guard, on purpose.
func ExemptOnPurpose() {
	send()
}

// Bystander neither triggers nor guards, and must not be counted as a
// producer — a scan whose denominator includes every function in the
// package cannot have a meaningful floor.
func Bystander() {}
