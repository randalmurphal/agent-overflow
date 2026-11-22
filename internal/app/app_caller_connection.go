package app

import (
	"context"
	"errors"

	"agent-overflow/internal/transport"
)

// A bound method that arms something for its client (a subscription, a
// recording, a mount, a watch, a held diff, a claim on a shared channel)
// ties its release to the connection the call came over, so the
// connection's end gives it back. callerConnection is the one way in: the
// method calls it first, so a call that came over no connection is refused
// before anything is armed or spent, and the callerConn it answers is the
// only way to tie a release to the connection.
// TestArmingMethodsAreTiedToTheirConnection holds every arming method to
// it.

// errNoCallerConnection refuses a call that would arm something for its
// connection when it came over none: nothing would ever release it.
var errNoCallerConnection = errors.New("this call opens something for its client connection to release, and it came over no connection")

// errCallerConnectionClosing is a tie refused because the connection was
// already closing; what it would have released has been released.
var errCallerConnectionClosing = errors.New("the connection closed while this call was opening something for it")

// callerConn is the connection a bound method arms a resource for. Only
// callerConnection makes one, so state is never nil.
type callerConn struct {
	state *transport.ConnState
}

// callerConnection answers the connection ctx's call came over, or
// errNoCallerConnection when it came over none.
func callerConnection(ctx context.Context) (callerConn, error) {
	state := transport.ConnStateFromContext(ctx)
	if state == nil {
		return callerConn{}, errNoCallerConnection
	}
	return callerConn{state: state}, nil
}

// bindRelease ties release to the connection under key, for a resource the
// client can also release by id: its release method unbinds key on its own
// call's connection. When the connection is already closing, release runs
// now and bindRelease fails.
func (c callerConn) bindRelease(key string, release func()) error {
	if c.state.BindCleanup(key, release) {
		return nil
	}
	release()
	return errCallerConnectionClosing
}

// registerRelease ties release to the connection for the rest of its life.
// When the connection is already closing, release runs now and
// registerRelease fails.
func (c callerConn) registerRelease(release func()) error {
	if c.state.RegisterCleanup(release) {
		return nil
	}
	release()
	return errCallerConnectionClosing
}
