package forgeapi

import (
	"errors"
	"fmt"
	"net"
	"net/http"
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
// (a *StatusError, a 403 or 429 rate limit, a refused login), or a GraphQL answer
// that refused it (GraphQLError.Refused). Every other error
// cannot say: a reset or timeout after the request was written, a body
// cut short, a 5xx, or a success whose body did not decode.
func WriteNotApplied(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := errors.AsType[*NotSentError](err); ok {
		return true
	}
	// Before the status search: a failed probe's own *StatusError (a 5xx)
	// would be found first.
	if _, ok := errors.AsType[*RefusalError](err); ok {
		return true
	}
	if status, ok := errors.AsType[*StatusError](err); ok {
		return status.Status >= 400 && status.Status < 500
	}
	if limited, ok := errors.AsType[*RateLimitedError](err); ok {
		// The quota header never decides whether a write ran: only the
		// Service's own refusal (never sent; NotSentError above), a 403 or
		// 429, or a GraphQL answer the forge refused in full does.
		if limited.GraphQL != nil {
			return limited.GraphQL.Refused()
		}
		return limited.Status == 0 || limited.Status == http.StatusForbidden || limited.Status == http.StatusTooManyRequests
	}
	if gql, ok := errors.AsType[*GraphQLError](err); ok {
		return gql.Refused()
	}
	if setup, ok := errors.AsType[*SetupError](err); ok {
		return setup.Kind == SetupUnauthenticated || setup.Kind == SetupMissing
	}
	return false
}

// RefusalError is a write the forge refused in full with a 401 (Refusal,
// its *StatusError) whose recovery then failed (Err): re-reading the login
// (RefusalStepCredential), or checking it with GET user after the
// re-read left the 401 standing (RefusalStepProbe). The refusal is a fact
// about the write that the recovery's failure does not change; Err is
// what the user can act on. errors.As finds Err's types first, so the
// recovery is classified as it was alone, and the refusal's status stays
// readable after it.
type RefusalError struct {
	Refusal error
	Step    string
	Err     error
}

// RefusalError steps.
const (
	RefusalStepCredential = "credential"
	RefusalStepProbe      = "probe"
)

func (e *RefusalError) Error() string {
	if e.Step == RefusalStepProbe {
		return fmt.Sprintf("checking the login failed: %v (the forge had refused the request: %v)", e.Err, e.Refusal)
	}
	return fmt.Sprintf("reading the login again failed: %v (the forge had refused the request: %v)", e.Err, e.Refusal)
}

func (e *RefusalError) Unwrap() []error { return []error{e.Err, e.Refusal} }

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
