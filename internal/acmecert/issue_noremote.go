//go:build noremote

package acmecert

import (
	"context"

	"agent-overflow/internal/buildvariant"
)

// Issuer is absent from a build without remote access: New refuses and no
// ACME client is linked.
type Issuer struct{}

// New refuses in a build without remote access.
func New(Config) (*Issuer, error) { return nil, buildvariant.ErrRemoteAccessUnavailable }

func (i *Issuer) Domain() string { return "" }

func (i *Issuer) Issue(context.Context) (Material, error) {
	return Material{}, buildvariant.ErrRemoteAccessUnavailable
}
