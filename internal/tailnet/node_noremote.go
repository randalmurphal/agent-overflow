//go:build noremote

package tailnet

import (
	"context"
	"net"

	"agent-overflow/internal/buildvariant"
)

// Node is absent from a build without remote access: New refuses, so no
// value of it exists and tsnet is not linked. The methods keep callers
// compiling and refuse if reached.
type Node struct{}

// New refuses in a build without remote access.
func New(Options) (*Node, error) { return nil, buildvariant.ErrRemoteAccessUnavailable }

func (n *Node) Events() <-chan struct{} { return nil }

func (n *Node) Start() error { return buildvariant.ErrRemoteAccessUnavailable }

func (n *Node) Listen(int) (net.Listener, error) {
	return nil, buildvariant.ErrRemoteAccessUnavailable
}

func (n *Node) ListenTLS() (net.Listener, error) {
	return nil, buildvariant.ErrRemoteAccessUnavailable
}

func (n *Node) ListenTLSOn(int) (net.Listener, error) {
	return nil, buildvariant.ErrRemoteAccessUnavailable
}

func (n *Node) Status() Status { return Status{} }

func (n *Node) Close() error { return nil }

func (n *Node) DiscoverCandidates(context.Context) ([]Candidate, error) {
	return nil, buildvariant.ErrRemoteAccessUnavailable
}

func (n *Node) DialContext(context.Context, string, string) (net.Conn, error) {
	return nil, buildvariant.ErrRemoteAccessUnavailable
}
