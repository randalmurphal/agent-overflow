// Package alpha is a methodgen scan fixture: one pointer receiver
// with the full mix of collected and skipped declaration shapes.
// Under testdata/, so the go tool never builds it.
package alpha

// Alpha stands in for a receiver registered under its own type name.
type Alpha struct{}

// DoAlpha is collected.
func (a *Alpha) DoAlpha() error { return nil }

// SharedName is collected here and deliberately re-declared on the
// beta fixture's Gamma, to exercise the cross-spec collision refusal.
func (a *Alpha) SharedName() {}

// unexportedAlpha is skipped: not exported.
func (a *Alpha) unexportedAlpha() {}

//wails:ignore
func (a *Alpha) IgnoredAlpha() {}

// ValueReceiverAlpha is skipped: value receiver, which Wails' own
// generator also refuses.
func (a Alpha) ValueReceiverAlpha() {}

// Startup is skipped by the internal-service skip set.
func (a *Alpha) Startup() {}

// Other is a second type in the same directory; its methods must not
// be collected under the Alpha spec.
type Other struct{}

// DoOther must never appear under an Alpha spec.
func (o *Other) DoOther() {}

// BareAlphaFunc has no receiver at all.
func BareAlphaFunc() {}
