package forgeapi

import (
	"errors"
	"net"
)

// NotSentError is a call that failed before its request left this
// process: the client was unusable, the request could not be built, the
// wait for a host slot ended, or the connection was never made (a dial or
// name lookup failure). The forge never saw it.
type NotSentError struct{ Err error }

func (e *NotSentError) Error() string { return e.Err.Error() }

func (e *NotSentError) Unwrap() error { return e.Err }

// WriteNotApplied reports whether err, the error of one write (POST, PUT,
// PATCH, DELETE), proves the forge did not apply it: the request was
// never sent (NotSentError), or the forge answered it in full with a 4xx
// (a *StatusError, a rate limit, a refused login). Every other error
// cannot say: a reset or timeout after the request was written, a body
// cut short, a 5xx, or a success whose body did not decode.
func WriteNotApplied(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := errors.AsType[*NotSentError](err); ok {
		return true
	}
	if status, ok := errors.AsType[*StatusError](err); ok {
		return status.Status >= 400 && status.Status < 500
	}
	if _, ok := errors.AsType[*RateLimitedError](err); ok {
		return true
	}
	if setup, ok := errors.AsType[*SetupError](err); ok {
		return setup.Kind == SetupUnauthenticated || setup.Kind == SetupMissing
	}
	return false
}

// notSent marks err, a failure before the request left this process.
func notSent(err error) error {
	if err == nil {
		return nil
	}
	return &NotSentError{Err: err}
}

// neverConnected reports whether a transport failure happened before a
// connection existed, so no byte of the request reached the forge.
func neverConnected(cause error) bool {
	if _, ok := errors.AsType[*net.DNSError](cause); ok {
		return true
	}
	if op, ok := errors.AsType[*net.OpError](cause); ok {
		return op.Op == "dial"
	}
	return false
}
