// Package budget is the methodgen fixture for the optional //ao:budget
// directive: one method carrying it, one without.
// Under testdata/, so the go tool never builds it.
package budget

// Eta is the fixture receiver.
type Eta struct{}

// Quick has the default budget.
//
//ao:scope settings:write
//ao:route home
func (e *Eta) Quick() {}

// Slow runs a hook.
//
//ao:scope settings:write
//ao:route home
//ao:budget 10m
func (e *Eta) Slow() {}
