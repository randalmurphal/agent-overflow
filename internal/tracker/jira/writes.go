package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"

	"agent-overflow/internal/tracker/trackerapi"
)

// A write's outcome is decided in one place, write below, by one rule:
//
//   - A write is applied when its answer is one of the success forms the
//     spec documents for the call and names what the spec says it returns
//     (or is the documented empty success, such as a 204). An answer that
//     names it but fails to decode elsewhere is still applied: the
//     *AnswerError says so (Applied), and the result carries the identity
//     and what else decoded.
//   - Any other 2xx (an undocumented status, a body that names nothing
//     written, a body that is not JSON) leaves the outcome unknown: such an
//     answer may come from something that is not the tracker. The
//     *AnswerError is not Applied, and nothing is reported as created,
//     stored or closed.
//   - A transport failure is the transport's: trackerapi.WriteNotApplied
//     tells a refusal from an unknown outcome.

// written is the body of a write's answer, read leniently for the
// identity of what was written: the top-level object or the list of
// objects.
type written struct {
	top  map[string]json.RawMessage
	list []map[string]json.RawMessage
}

// id is the answer's top-level id, a string or a number.
func (w written) id() string { return idText(w.top["id"]) }

// key is the answer's top-level key.
func (w written) key() string { return idText(w.top["key"]) }

// ids is the id of each object of a list answer.
func (w written) ids() []string {
	out := make([]string, len(w.list))
	for i, item := range w.list {
		out[i] = idText(item["id"])
	}
	return out
}

// text is the top-level member name when it is a JSON string; any other
// value fails to decode and leaves it empty.
func (w written) text(name string) string {
	var s string
	_ = json.Unmarshal(w.top[name], &s)
	return s
}

// writeAnswer is the success answers the spec documents for one write:
// the statuses with no body to read, the statuses whose body is read, and
// what that body must name.
type writeAnswer struct {
	empty []int
	body  []int
	names func(written) bool
}

// emptyAnswer is a write documented to answer with no body to read.
func emptyAnswer(status ...int) writeAnswer { return writeAnswer{empty: status} }

// bodyAnswer is a write documented to answer with a body naming what was
// written, as names decides.
func bodyAnswer(names func(written) bool, status ...int) writeAnswer {
	return writeAnswer{body: status, names: names}
}

// namesID accepts a body whose id is a numeric ID.
func namesID(w written) bool { return numericRE.MatchString(w.id()) }

// namesIssue accepts a body naming an issue by numeric id and key.
func namesIssue(w written) bool { return namesID(w) && issueKeyRE.MatchString(w.key()) }

// namesThisID accepts a body whose id is id.
func namesThisID(id string) func(written) bool {
	return func(w written) bool { return w.id() == id }
}

// namesEveryID accepts a list body, not empty, each of whose objects has
// a numeric ID.
func namesEveryID(w written) bool {
	ids := w.ids()
	return len(ids) > 0 && !slices.ContainsFunc(ids, func(id string) bool { return !numericRE.MatchString(id) })
}

// write sends r, a write, and reads its answer by the rule above: out (nil
// skips it) receives a body answer. The written body is returned for the
// caller's identity, also with an Applied *AnswerError; with any other
// error it is empty and out is untouched, so nothing reads as stored.
func (c *Client) write(ctx context.Context, r trackerapi.Request, want writeAnswer, out any) (written, error) {
	resp, err := c.call(ctx, r, nil)
	if err != nil {
		return written{}, err
	}
	switch {
	case slices.Contains(want.empty, resp.Status):
		return written{}, nil
	case !slices.Contains(want.body, resp.Status):
		return written{}, &AnswerError{Message: "the answer's status " + strconv.Itoa(resp.Status) + " is not one this write documents, so whether it was applied is unknown"}
	}
	body, err := scrubbedBody(resp)
	if err != nil {
		return written{}, unknownAnswer(err)
	}
	var w written
	if json.Unmarshal(body, &w.top) != nil {
		w.top = nil
		_ = json.Unmarshal(body, &w.list)
	}
	if !want.names(w) {
		return written{}, &AnswerError{Message: "the answer does not name what was written, so whether it was applied is unknown"}
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return w, &AnswerError{Applied: true, Message: "the write was applied, but its answer did not decode: " + resp.Scrub(err.Error())}
		}
	}
	return w, nil
}

// unknownAnswer is err, an answer that could not be read at all, as a
// write's unknown outcome.
func unknownAnswer(err error) error {
	return &AnswerError{Message: fmt.Sprintf("%v, so whether the write was applied is unknown", err)}
}

// outcomeOf is the error in err's chain that decides a write's outcome:
// the first, in the order errors.As searches, that is a transport outcome
// (*trackerapi.NotSentError, *StatusError, *AuthError, *RateLimitedError)
// or an *AnswerError, as trackerapi.WriteNotApplied reads the outermost.
// An *UnconfirmedError counts as the 404 it holds. Nil when none is.
func outcomeOf(err error) error {
	for err != nil {
		switch e := err.(type) {
		case *trackerapi.NotSentError, *trackerapi.StatusError, *trackerapi.AuthError, *trackerapi.RateLimitedError, *AnswerError:
			return err
		case *UnconfirmedError:
			if e.NotFound != nil {
				return e.NotFound
			}
		case interface{ Unwrap() []error }:
			for _, inner := range e.Unwrap() {
				if found := outcomeOf(inner); found != nil {
					return found
				}
			}
			return nil
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return nil
		}
		err = u.Unwrap()
	}
	return nil
}

// neverSent reports a write whose own outcome is that it was not sent; a
// follow-up that failed before sending, nested under the write's answer,
// does not count.
func neverSent(err error) bool {
	_, ok := outcomeOf(err).(*trackerapi.NotSentError)
	return ok
}

// answerApplied reports a write whose outcome is an Applied *AnswerError.
func answerApplied(err error) bool {
	ae, ok := outcomeOf(err).(*AnswerError)
	return ok && ae.Applied
}
