// Package callerbound is the methodgen fixture for the optional
// //ao:callerbound directive: one method carrying it, one without.
// Under testdata/, so the go tool never builds it.
package callerbound

import "context"

// Zeta is the fixture receiver.
type Zeta struct{}

// Writes keeps running after its caller leaves.
//
//ao:scope settings:write
//ao:route home
func (z *Zeta) Writes(ctx context.Context) {}

// Reads ends with its caller.
//
//ao:scope settings:write
//ao:route home
//ao:callerbound
func (z *Zeta) Reads(ctx context.Context) {}
