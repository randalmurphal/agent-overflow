// Package unrouted is the methodgen fixture for the route half of the
// completeness gate: a scoped method whose call belongs to no backend
// anybody named. Under testdata/, so the go tool never builds it.
package unrouted

// Theta is the fixture receiver.
type Theta struct{}

// Routed is the control — inferred, so the refusal names only the
// methods actually at fault.
//
//ao:scope threads:read
func (t *Theta) Routed(threadID string) {}

// Unrouted names an id no inference reads and declares nothing.
//
//ao:scope threads:read
func (t *Theta) Unrouted(itemID string) {}

// AlsoUnrouted takes no parameters at all, which is the other shape the
// inference cannot answer.
//
//ao:scope threads:read
func (t *Theta) AlsoUnrouted() {}
