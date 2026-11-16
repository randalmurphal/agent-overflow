package linear

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"agent-overflow/internal/tracker/trackerapi"
)

// GraphQLError is one entry of an answer's errors array.
type GraphQLError struct {
	// Message is Linear's user-presentable message when it sent one,
	// else the error's message.
	Message string
	// Code is extensions.code, such as INPUT_ERROR.
	Code string
	// Type is extensions.type, such as "invalid input".
	Type string
	// UserError is extensions.userError: the error was caused by the
	// request's input rather than by Linear.
	UserError bool
	// Path is where in the answer the error arose; list indexes are
	// written as decimal numbers.
	Path []string

	// rawMessage is the message field as sent, which notFound reads.
	rawMessage string
}

func (e GraphQLError) String() string {
	s := e.Message
	if s == "" {
		s = "unnamed error"
	}
	if e.Code != "" {
		s += " (" + e.Code + ")"
	}
	if len(e.Path) > 0 {
		s += " at " + strings.Join(e.Path, ".")
	}
	return s
}

// notFound reports an error that says the entity the call named does not
// exist or is not visible to the key. Linear documents no code for it;
// the message prefix is what its API answers (see the package's live
// check, which verifies it).
func (e GraphQLError) notFound() bool {
	return strings.HasPrefix(strings.ToLower(e.rawMessage), "entity not found")
}

// wireError is a GraphQL error as sent.
type wireError struct {
	Message    string            `json:"message"`
	Path       []json.RawMessage `json:"path"`
	Extensions struct {
		Code                   string `json:"code"`
		Type                   string `json:"type"`
		UserError              bool   `json:"userError"`
		UserPresentableMessage string `json:"userPresentableMessage"`
	} `json:"extensions"`
}

func (w wireError) typed() GraphQLError {
	e := GraphQLError{
		Message:    w.Message,
		Code:       w.Extensions.Code,
		Type:       w.Extensions.Type,
		UserError:  w.Extensions.UserError,
		rawMessage: w.Message,
	}
	if w.Extensions.UserPresentableMessage != "" {
		e.Message = w.Extensions.UserPresentableMessage
	}
	for _, p := range w.Path {
		var s string
		if json.Unmarshal(p, &s) == nil {
			e.Path = append(e.Path, s)
			continue
		}
		var n int64
		if json.Unmarshal(p, &n) == nil {
			e.Path = append(e.Path, strconv.FormatInt(n, 10))
		}
	}
	return e
}

// Error is a call Linear answered with GraphQL errors, or a mutation
// whose payload reported success: false.
//
// A read fails on any error, even when the answer also carries data:
// the typed result cannot say which of its fields an error left out. A
// mutation's outcome is in Refused.
//
// errors.Is(err, trackerapi.ErrNotFound) matches an error that says the
// named entity does not exist or is not visible.
type Error struct {
	// Op is the GraphQL operation name.
	Op string
	// Status is the HTTP status of the answer.
	Status int
	// Errors are the answer's errors; empty for success: false.
	Errors []GraphQLError
	// Unsuccessful is a mutation payload's success: false.
	Unsuccessful bool
	// Refused reports that the answer proves a write was not applied:
	// an HTTP 4xx, a mutation answered with no payload and only user
	// errors, or success: false. A write with Refused false and an error
	// has an unknown outcome; read the entity again before offering it
	// again.
	Refused bool
	// Err is the transport's error for an answer of HTTP 400 or above.
	Err error
}

func (e *Error) Error() string {
	msg := "linear " + e.Op + ": "
	switch {
	case e.Unsuccessful:
		msg += "Linear reported the mutation as not successful"
	case len(e.Errors) > 0:
		msg += joinErrors(e.Errors)
	case e.Err != nil:
		msg += e.Err.Error()
	default:
		msg += "the answer carried no data"
	}
	if e.Status != 0 && e.Status != 200 {
		msg += fmt.Sprintf(" (HTTP %d)", e.Status)
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// Is matches trackerapi.ErrNotFound for a not-found GraphQL error.
func (e *Error) Is(target error) bool {
	if target != trackerapi.ErrNotFound {
		return false
	}
	for _, g := range e.Errors {
		if g.notFound() {
			return true
		}
	}
	return false
}

// WriteNotApplied reports whether err, the error of one write, proves
// Linear did not apply it: trackerapi.WriteNotApplied, or an *Error with
// Refused set. Any other failure leaves the outcome unknown.
func WriteNotApplied(err error) bool {
	if e, ok := errors.AsType[*Error](err); ok && e.Refused {
		return true
	}
	return trackerapi.WriteNotApplied(err)
}
