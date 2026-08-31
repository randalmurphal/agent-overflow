// Package beta is the second methodgen scan fixture: a receiver in a
// DIFFERENT directory and package that nonetheless registers under a
// different type label, the shape root-decomposition.md describes for
// a service promoted out of the root while keeping main.App IDs.
// Under testdata/, so the go tool never builds it.
package beta

// Beta registers under a type label that is not its source name.
type Beta struct{}

// DoBeta is collected.
//
//ao:scope threads:read
func (b *Beta) DoBeta() (string, error) { return "", nil }

// AlsoBeta is collected.
//
//ao:scope files:read
func (b *Beta) AlsoBeta() {}

// Gamma exists to collide with the alpha fixture.
type Gamma struct{}

// SharedName duplicates Alpha.SharedName across specs.
//
//ao:scope host
func (g *Gamma) SharedName() {}

// AlsoGamma must never be collected under the Beta spec: Gamma shares
// beta's directory but is a different receiver.
//
//ao:scope host
func (g *Gamma) AlsoGamma() {}
