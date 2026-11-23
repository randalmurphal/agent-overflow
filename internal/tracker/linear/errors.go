package linear

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"agent-overflow/internal/tracker/trackerapi"
)

// GraphQLError is one entry of an answer's errors array. Its text has
// the request's credential scrubbed out.
type GraphQLError struct {
	// Message is Linear's user-presentable message when it sent one,
	// else the error's message.
	Message string
	// Code is extensions.code, such as INPUT_ERROR.
	Code string
	// Type is extensions.type, such as "invalid input".
	Type string
	// UserError is extensions.userError: the error was caused by the
	// request's input rather than by Linear. False when Linear did not
	// say, which no outcome is decided on alone.
	UserError bool
	// Path is where in the answer the error arose, by response key; list
	// indexes are written as decimal numbers.
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

// notFound reports an error saying that the entity at root, the response
// key of the resource a call asked for, does not exist or is not visible
// to the key. Linear has no code of its own for it: it answers
// INPUT_ERROR with the message "Entity not found: " and the entity's
// type at that path (measured for nine types;
// docs/references/tracker-api-measurements.md). The same message deeper
// in the answer is a related entity, not the resource, and a write whose
// input names a missing entity is missingReference.
func (e GraphQLError) notFound(root string) bool {
	return root != "" && e.Code == CodeInput && len(e.Path) == 1 && e.Path[0] == root &&
		strings.HasPrefix(e.rawMessage, "Entity not found: ")
}

// missingReference is the input member of a write that names, by ID, an
// entity that does not exist or is not visible to the key: Linear
// answers INPUT_ERROR with the message "Entity not found in
// validateAccess: " and the member (measured for issueUpdate's stateId,
// assigneeId, labelIds, cycleId, projectId and parentId).
func (e GraphQLError) missingReference() (string, bool) {
	member, ok := strings.CutPrefix(e.rawMessage, "Entity not found in validateAccess: ")
	return member, ok && e.Code == CodeInput && member != ""
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

// typed converts w, passing every string through scrub.
func (w wireError) typed(scrub func(string) string) GraphQLError {
	e := GraphQLError{
		Message:    scrub(w.Message),
		Code:       scrub(w.Extensions.Code),
		Type:       scrub(w.Extensions.Type),
		UserError:  w.Extensions.UserError,
		rawMessage: scrub(w.Message),
	}
	if w.Extensions.UserPresentableMessage != "" {
		e.Message = scrub(w.Extensions.UserPresentableMessage)
	}
	for _, p := range w.Path {
		var s string
		if json.Unmarshal(p, &s) == nil {
			e.Path = append(e.Path, scrub(s))
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
// whose payload did not report success: true.
//
// A read fails on any error, even when the answer also carries data:
// the typed result cannot say which of its fields an error left out. A
// mutation's outcome is in Refused.
//
// errors.Is(err, trackerapi.ErrNotFound) matches an error saying that
// the resource the call named does not exist or is not visible; a
// missing related entity deeper in the answer does not match.
type Error struct {
	// Op is the GraphQL operation name.
	Op string
	// Status is the HTTP status of the answer.
	Status int
	// Errors are the answer's errors, kept with success: false too.
	Errors []GraphQLError
	// Unsuccessful is a mutation payload's success: false.
	Unsuccessful bool
	// Refused reports that the answer proves a write was not applied:
	// success: false (with or without errors), an HTTP 4xx, or a
	// mutation answered with no payload and only user errors. A write
	// with Refused false and an error has an unknown outcome; read the
	// entity again before offering it again.
	Refused bool
	// Err is the transport's error for an answer of HTTP 400 or above,
	// or what made a payload's outcome unknown.
	Err error

	notFound bool
}

func (e *Error) Error() string {
	msg := "linear " + e.Op + ": "
	switch {
	case e.Unsuccessful:
		msg += "Linear reported the mutation as not successful"
		if len(e.Errors) > 0 {
			msg += ": " + joinErrors(e.Errors)
		}
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

// Is matches trackerapi.ErrNotFound for a not-found error at the
// resource's own path.
func (e *Error) Is(target error) bool {
	return target == trackerapi.ErrNotFound && e.notFound
}

// MissingReference is the input member of the write that names, by ID,
// an entity that does not exist or is not visible to the key, such as
// "stateId": a value picked from a list that has changed since. Linear
// applied nothing of the write.
func (e *Error) MissingReference() (member string, ok bool) {
	for _, g := range e.Errors {
		if member, ok := g.missingReference(); ok {
			return member, true
		}
	}
	return "", false
}

// UnchangedError is an issue's part of a write of several that failed,
// which the read made right after the failure found without what was
// written, or did not list: Linear did not apply it. Write is the failed
// request's error.
type UnchangedError struct {
	IssueID string
	// Listed is false when the read did not list the issue at all.
	Listed bool
	Write  error
}

func (e *UnchangedError) Error() string {
	state := "does not hold the value written"
	if !e.Listed {
		state = "is not listed"
	}
	return fmt.Sprintf("linear: issue %s %s after the write failed: %v", e.IssueID, state, e.Write)
}

func (e *UnchangedError) Unwrap() error { return e.Write }

// OutcomeUnknownError is an issue's part of a write of several that was
// sent and failed, and whose read meant to settle it failed too: nothing
// proves whether Linear applied it, whatever Write and Read say of their
// own requests, so WriteNotApplied is false for it.
type OutcomeUnknownError struct {
	IssueID     string
	Write, Read error
}

func (e *OutcomeUnknownError) Error() string {
	return fmt.Sprintf("linear: the outcome of the write of issue %s is unknown: the write failed (%v) and the read after it failed (%v)",
		e.IssueID, e.Write, e.Read)
}

func (e *OutcomeUnknownError) Unwrap() []error { return []error{e.Write, e.Read} }

// WriteNotApplied reports whether err, the error of one write, proves
// Linear did not apply it: trackerapi.WriteNotApplied, an *Error with
// Refused set, or an *UnchangedError. An *OutcomeUnknownError and any
// other failure leave the outcome unknown.
func WriteNotApplied(err error) bool {
	if _, ok := errors.AsType[*OutcomeUnknownError](err); ok {
		return false
	}
	if _, ok := errors.AsType[*UnchangedError](err); ok {
		return true
	}
	if e, ok := errors.AsType[*Error](err); ok && e.Refused {
		return true
	}
	return trackerapi.WriteNotApplied(err)
}
