// Package badroute is the methodgen fixture for a //ao:route naming a
// route the vocabulary does not declare. Under testdata/, so the go
// tool never builds it.
package badroute

// Iota is the fixture receiver.
type Iota struct{}

// Fine is the control: a declared route, so the refusal names only the
// typo.
//
//ao:scope threads:read
//ao:route home
func (i *Iota) Fine() {}

// Typo is one transposition off a real route, which reads correct in
// review and would otherwise land as a route no client can resolve.
//
//ao:scope threads:read
//ao:route hoem
func (i *Iota) Typo() {}
