// Package badscope is the methodgen fixture for an annotation naming a
// scope the vocabulary does not declare — what a typo in a real
// //ao:scope line looks like. Under testdata/, so the go tool never
// builds it.
package badscope

// Epsilon is the fixture receiver.
type Epsilon struct{}

// Fine is the control: a declared scope, so the refusal names only the
// typo.
//
//ao:scope threads:read
func (e *Epsilon) Fine() {}

// Typo names a scope one character off a real one, which is exactly the
// case a closed vocabulary exists to catch: it reads correct.
//
//ao:scope threads:reed
func (e *Epsilon) Typo() {}
