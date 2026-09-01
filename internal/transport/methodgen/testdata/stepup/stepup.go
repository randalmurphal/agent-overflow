// Package stepup is the methodgen fixture for the optional //ao:stepup
// directive: one method carrying it beside its scope, one without.
// Under testdata/, so the go tool never builds it.
package stepup

// Zeta is the fixture receiver.
type Zeta struct{}

// Ordinary carries a scope and nothing else.
//
//ao:scope settings:write
//ao:route home
func (z *Zeta) Ordinary() {}

// Reconfigures carries both directives, in the order a real annotation
// spells them.
//
//ao:scope settings:write
//ao:route home
//ao:stepup
func (z *Zeta) Reconfigures() {}
