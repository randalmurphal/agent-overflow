// Package dispatchrefusal says who refused a call when a computer's
// dispatcher refused it before any method ran: a call it does not have
// (method_not_found) or one whose arguments it cannot read (bad_params).
// Either way the call did not run, and the caller was often built from a
// different release.
//
// The dispatcher marks its own refusal with the frame's "dispatch" field
// (Refusal), which every reader takes as the answer when it knows the
// field's kind: a failure a method returned never carries it, whatever its
// code. A kind from a later release reads as no field. A frame from a release
// before the field (v0.0.14 to v0.0.19) is matched against those releases'
// exact dispatcher texts (Legacy), and anything else is a method's own
// failure, shown as it was sent.
//
// The dispatcher sends Unsupported or Unreadable as the refusal's message,
// so a reader already shipped shows them as they are. Every Go reader of a
// refusal (rpcclient, the Windows launcher's client, the harness client)
// reads it through Read and Message. The frontend has the same sentences
// and the same legacy table (frontend/src/lib/transport/dispatchRefusal.ts),
// pinned to these by this package's tests.
//
// Leaf package: it imports only the standard library, so a client that
// must not link the transport server can still use it.
package dispatchrefusal

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// The wire codes, mirroring transport.ErrCodeMethodNotFound and
// transport.ErrCodeBadParams (pinned by internal/transport's tests).
const (
	CodeMethodNotFound = "method_not_found"
	CodeBadParams      = "bad_params"
)

// The kinds of refusal.
const (
	// KindUnsupported is a call the computer does not have, or one hidden
	// from this caller: the two are the same refusal, naming nothing.
	KindUnsupported = "unsupported"
	// KindUnreadable is a call whose arguments the computer cannot read.
	KindUnreadable = "unreadable"
)

// Unsupported is what a call the computer does not have reads as.
const Unsupported = "The computer this reached does not support this action. It may run a different version of Agent Overflow. If so, update the older one, or reload a page left open during an update."

// Unreadable is what a call whose arguments the computer could not read
// reads as.
const Unreadable = "The computer this reached could not read this request. It may run a different version of Agent Overflow. If so, update the older one, or reload a page left open during an update."

// ForwardedUnsupported and ForwardedUnreadable are what a computer says when
// another computer it called for the person refused that way: the message
// it sends names that computer first, and reloading a page does not reach
// it, so the advice is about the two computers.
const (
	ForwardedUnsupported = "That computer does not support this action. It may run a different version of Agent Overflow than this one. If so, update the older one."
	ForwardedUnreadable  = "That computer could not read this request. It may run a different version of Agent Overflow than this one. If so, update the older one."
)

// Forwarded is the forwarded sentence for a refusal of kind.
func Forwarded(kind string) string {
	if kind == KindUnreadable {
		return ForwardedUnreadable
	}
	return ForwardedUnsupported
}

// Refusal is the frame's "dispatch" field: the dispatcher refused the call
// before any method ran. Only the dispatcher sets it. An unsupported
// refusal carries nothing else. An unreadable one names the method, which
// the caller can see (the checks run only after the method resolved for
// this caller), and a problem built from a closed set of shapes, never from
// an error's text: a custom decoder may echo the value it refused.
type Refusal struct {
	Kind string `json:"refusal"`
	// Method is the method's name, for an unreadable refusal.
	Method string `json:"method,omitempty"`
	// Param is the 1-based argument that did not decode, or 0 when the
	// number of arguments was wrong.
	Param int `json:"param,omitempty"`
	// Problem is what did not fit, when the dispatcher can say it without
	// repeating anything the caller sent: the argument counts, or the type
	// expected and the JSON kind received.
	Problem string `json:"problem,omitempty"`
}

// MaxProblem bounds Problem. The dispatcher builds it from type names and
// numbers, which stay far below this.
const MaxProblem = 160

// UnsupportedRefusal is the refusal of a call the computer does not have.
func UnsupportedRefusal() *Refusal { return &Refusal{Kind: KindUnsupported} }

// CountRefusal is the refusal of a call to method with the wrong number of
// arguments. atLeast marks a variadic method's minimum and atMost the
// dispatcher's own maximum.
func CountRefusal(method string, expected, received int, atLeast, atMost bool) *Refusal {
	bound := ""
	switch {
	case atLeast:
		bound = "at least "
	case atMost:
		bound = "at most "
	}
	return &Refusal{Kind: KindUnreadable, Method: method, Problem: fmt.Sprintf("expected %s%d arguments, got %d", bound, expected, received)}
}

// DecodeRefusal is the refusal of argument param (1-based) of method,
// which failed to decode with err. A JSON type mismatch says the type
// expected and the JSON kind received; any other failure says nothing more,
// and its text stays in the computer's log.
func DecodeRefusal(method string, param int, err error) *Refusal {
	refusal := &Refusal{Kind: KindUnreadable, Method: method, Param: param}
	var mismatch *json.UnmarshalTypeError
	if errors.As(err, &mismatch) && mismatch.Type != nil {
		// Value is "string", "object", or "number 1.5": the kind alone.
		kind, _, _ := strings.Cut(mismatch.Value, " ")
		where := ""
		if mismatch.Field != "" {
			where = " at " + mismatch.Field
		}
		problem := fmt.Sprintf("expected %s%s, got %s", mismatch.Type.String(), where, kind)
		if len(problem) <= MaxProblem {
			refusal.Problem = problem
		}
	}
	return refusal
}

// Diagnostic is what did not fit, for Details and for a client to print
// beside the sentence, or "" when the refusal says nothing more.
func (r *Refusal) Diagnostic() string {
	if r == nil || r.Kind != KindUnreadable || r.Method == "" {
		return ""
	}
	what := r.Method
	if r.Param > 0 {
		what = fmt.Sprintf("%s argument %d", r.Method, r.Param)
	}
	if r.Problem == "" {
		return what + " could not be read"
	}
	return what + ": " + r.Problem
}

// Sentence is the sentence for a refusal of kind.
func Sentence(kind string) string {
	if kind == KindUnreadable {
		return Unreadable
	}
	return Unsupported
}

// legacy is every text a dispatcher of v0.0.14 to v0.0.19 sent before a
// method ran, each the whole message: no method of those releases sent
// one of them as its own (checked at each tag). The FQN is
// "<package>.<Type>.<Method>".
var legacy = []struct {
	code, kind string
	text       *regexp.Regexp
}{
	// The dispatcher's missing or hidden method, and scopedtoken.go's
	// unlisted method, in every release from v0.0.14 to v0.0.19.
	{CodeMethodNotFound, KindUnsupported, regexp.MustCompile(`^method not registered$`)},
	{CodeBadParams, KindUnreadable, regexp.MustCompile(`^too many parameters$`)},
	{CodeBadParams, KindUnreadable, regexp.MustCompile(`^[A-Za-z0-9_]+\.[A-Za-z0-9_]+\.[A-Za-z0-9_]+: expected \d+ params, got \d+$`)},
	{CodeBadParams, KindUnreadable, regexp.MustCompile(`^[A-Za-z0-9_]+\.[A-Za-z0-9_]+\.[A-Za-z0-9_]+: variadic expects at least \d+ params, got \d+$`)},
	// v0.0.14 to v0.0.16 with no detail, v0.0.17 to v0.0.19 with detail.
	{CodeBadParams, KindUnreadable, regexp.MustCompile(`^bad parameter \d+$`)},
}

// Legacy is the kind of refusal a frame without the field is, when its
// whole message is one a dispatcher of v0.0.14 to v0.0.19 sent.
func Legacy(code, message string) (kind string, ok bool) {
	for _, entry := range legacy {
		if entry.code == code && entry.text.MatchString(message) {
			return entry.kind, true
		}
	}
	return "", false
}

// known is the field when its kind is one this release knows, or nil: a
// kind from a later release reads as no field, so the frame's message is
// the sender's own, and it is never taken for a call this computer lacks.
// The frontend's parseDispatchMark reads it the same way.
func known(field *Refusal) *Refusal {
	if field == nil {
		return nil
	}
	switch field.Kind {
	case KindUnsupported:
		return UnsupportedRefusal()
	case KindUnreadable:
		return field
	}
	return nil
}

// Read is the dispatcher refusal a frame carries: its field, else what an
// older release's exact text says, else nil for a method's own failure.
func Read(code, message string, field *Refusal) *Refusal {
	if marked := known(field); marked != nil {
		return marked
	}
	if kind, ok := Legacy(code, message); ok {
		return &Refusal{Kind: kind}
	}
	return nil
}

// Message is what a person reads for a refusal frame: for a dispatcher's
// refusal, its sentence followed by what did not fit, which for an older
// release is its own text; otherwise the frame's message as sent.
// An older release's missing method adds nothing to the sentence.
func Message(code, message string, field *Refusal) string {
	refusal := Read(code, message, field)
	if refusal == nil {
		return message
	}
	diagnostic := refusal.Diagnostic()
	if known(field) == nil && refusal.Kind == KindUnreadable {
		// An older release's text is what did not fit: "main.App.GitPush:
		// expected 1 params, got 2".
		diagnostic = message
	}
	if diagnostic == "" {
		return Sentence(refusal.Kind)
	}
	return Sentence(refusal.Kind) + " (" + diagnostic + ")"
}
