// Package unclassified is the methodgen fixture for the fault the
// generator exists to refuse: a bound method nobody classified. Under
// testdata/, so the go tool never builds it.
package unclassified

// Delta is the fixture receiver.
type Delta struct{}

// Annotated is the control — a correctly classified neighbour, so the
// refusal names only the methods actually at fault.
//
//ao:scope threads:read
func (d *Delta) Annotated() {}

// Unannotated carries prose and no directive, which is the shape a new
// method lands in when nobody classified it.
func (d *Delta) Unannotated() {}

// AlsoUnannotated has no doc comment at all — the other half of the
// same fault, and the reason the report lists every name rather than
// the first one.
func (d *Delta) AlsoUnannotated() {}
