//go:build noremote

package nearby

import (
	"context"

	"agent-overflow/internal/buildvariant"
)

// Server is absent from a build without remote access: Start refuses and
// no multicast library is linked.
type Server struct{}

// Start refuses in a build without remote access.
func Start(Advertisement) (*Server, error) { return nil, buildvariant.ErrRemoteAccessUnavailable }

func (s *Server) Close() error { return nil }

// Discover refuses in a build without remote access.
func Discover(context.Context) ([]Host, error) { return nil, buildvariant.ErrRemoteAccessUnavailable }
