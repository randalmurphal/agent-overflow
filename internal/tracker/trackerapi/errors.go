package trackerapi

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

// ErrNotFound matches, through errors.Is, a *StatusError whose status is
// 404. Whether a 404 means the resource is gone or the credential was
// refused is the tracker client's call.
var ErrNotFound = errors.New("trackerapi: not found")

// errClosed is a call on a closed Client, or one Close ended.
var errClosed = errors.New("trackerapi: client is closed")

// errRedirect is a redirect the client refused to follow.
var errRedirect = errors.New("redirect refused")

// maxErrorBody bounds StatusError.Body.
const maxErrorBody = 4 << 10

// NotSentError is a call whose request never left this process: the
// client was closed, the request was malformed, the credential could not
// be read, the rate gate was closed, the call ended while waiting to send
// (for the gate, the pacing or the credential), or the transport never
// held a connection for it (a dial, name lookup, proxy or TLS failure, or
// the call ending first). The tracker never saw it.
type NotSentError struct{ Err error }

func (e *NotSentError) Error() string { return e.Err.Error() }
func (e *NotSentError) Unwrap() error { return e.Err }

// AuthError is the tracker refusing the credential: a 401 after the
// credential was read again, or an answer the dialect marks as a refused
// login (Status is then the answer's, which can be a 2xx). Err is the
// failure to read the credential again, when that is what ended the call.
// Message has the credential the request carried replaced by
// secret.Redacted.
type AuthError struct {
	Tracker string
	Method  string
	URL     string // redacted
	Status  int
	Message string // the tracker's, from the dialect
	Err     error
}

func (e *AuthError) Error() string {
	msg := fmt.Sprintf("%s %s %s: the tracker refused the credential (HTTP %d)", e.Tracker, e.Method, e.URL, e.Status)
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if e.Err != nil {
		msg += "; reading the credential again failed: " + e.Err.Error()
	}
	return msg
}

func (e *AuthError) Unwrap() error { return e.Err }

// RateLimitedError is a call the tracker's rate limit refused: a 429, or
// an answer the dialect marks as a limit, once the retries are spent or at
// once for a background call. Status is the answer's; zero means the
// client's own gate refused the call before sending, and the error then
// comes wrapped in a *NotSentError. Until is when the gate opens.
type RateLimitedError struct {
	Tracker string
	Until   time.Time
	Status  int
}

func (e *RateLimitedError) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("%s: rate limited; requests resume at %s", e.Tracker, e.Until.UTC().Format(time.RFC3339))
	}
	return fmt.Sprintf("%s: rate limited (HTTP %d); requests resume at %s", e.Tracker, e.Status, e.Until.UTC().Format(time.RFC3339))
}

// StatusError is an answer of 400 or above that is neither a refused
// credential nor a rate limit, or a 3xx answering a call that follows no
// redirect (any method but GET and HEAD), for which a write's outcome is
// unknown. URL is redacted, Body holds at most 4 KiB of the answer, and
// Message and Fields are what the dialect read from it: Fields maps a
// field ID to its message. Body, Message and Fields have the credential
// the request carried replaced by secret.Redacted. Location is a 3xx's
// target, redacted; empty when it named none that parses. A 404 matches
// ErrNotFound.
type StatusError struct {
	Tracker  string
	Method   string
	URL      string
	Status   int
	Message  string
	Fields   map[string]string
	Body     string
	Location string
}

func (e *StatusError) Error() string {
	msg := fmt.Sprintf("%s %s %s: HTTP %d", e.Tracker, e.Method, e.URL, e.Status)
	if text := http.StatusText(e.Status); text != "" {
		msg += " " + text
	}
	if e.Status >= 300 && e.Status <= 399 {
		msg += ", a redirect not followed"
		if e.Location != "" {
			msg += " to " + e.Location
		}
	}
	detail := e.Message
	if len(e.Fields) > 0 {
		names := make([]string, 0, len(e.Fields))
		for name := range e.Fields {
			names = append(names, name)
		}
		slices.Sort(names)
		parts := make([]string, 0, len(names))
		for _, name := range names {
			parts = append(parts, name+": "+e.Fields[name])
		}
		detail = strings.TrimSpace(detail + " (" + strings.Join(parts, "; ") + ")")
	}
	if detail == "" {
		detail = strings.TrimSpace(e.Body)
	}
	if detail != "" {
		msg += ": " + detail
	}
	return msg
}

// Is makes errors.Is(err, ErrNotFound) true for a 404.
func (e *StatusError) Is(target error) bool {
	return target == ErrNotFound && e.Status == http.StatusNotFound
}

// TransientError is a failure the next attempt may not repeat: a
// timeout, a reset, a TLS failure, a stalled transfer, an answer cut
// short. Wrapped in a *NotSentError when the connection was never made.
type TransientError struct{ Err error }

func (e *TransientError) Error() string { return e.Err.Error() }
func (e *TransientError) Unwrap() error { return e.Err }

// BodyTooLargeError is an answer body over its bound, refused instead of
// read, or a Stream body that passed its limit after Limit bytes reached
// the destination.
type BodyTooLargeError struct {
	Tracker string
	Method  string
	URL     string
	Limit   int64
}

func (e *BodyTooLargeError) Error() string {
	return fmt.Sprintf("%s %s %s: the answer is over its %d-byte limit", e.Tracker, e.Method, e.URL, e.Limit)
}

// WriteNotApplied reports whether err, the error of one write, proves the
// tracker did not apply it: the request was never sent, or the tracker
// answered it in full with a 4xx (a refusal, a refused credential, a
// 429). Every other failure leaves the outcome unknown (a timeout or reset
// after the request may have been written, a 3xx, a 5xx, a 2xx cut short
// or that did not decode, a refused login marked on a 2xx), and the
// caller reads the tracker again before it offers the write again.
//
// The outermost of these errors in err's chain decides: a *NotSentError
// wrapping an answer's error (a check read that failed before the write)
// proves the write not sent, and an answer's error wrapping a
// *NotSentError (a failed re-read) is judged by its status.
func WriteNotApplied(err error) bool {
	if e, ok := errors.AsType[outcome](err); ok {
		return e.notApplied()
	}
	return false
}

// outcome is an error that tells whether a write was applied.
type outcome interface {
	error
	notApplied() bool
}

func (e *NotSentError) notApplied() bool { return true }
func (e *AuthError) notApplied() bool    { return clientError(e.Status) }
func (e *StatusError) notApplied() bool  { return clientError(e.Status) }

// notApplied is false for the gate's refusal (Status zero) unwrapped,
// which only a redirect hop of a read returns.
func (e *RateLimitedError) notApplied() bool { return clientError(e.Status) }

func clientError(status int) bool { return status >= 400 && status <= 499 }
