// Package remote is the methodgen fixture for the optional //ao:remote
// directive: one method carrying it, one without.
// Under testdata/, so the go tool never builds it.
package remote

// Zeta is the fixture receiver.
type Zeta struct{}

// Local is available in every build.
//
//ao:scope settings:write
//ao:route home
func (z *Zeta) Local() {}

// PairsAComputer exists only for remote access.
//
//ao:scope settings:write
//ao:route home
//ao:remote
func (z *Zeta) PairsAComputer() {}
