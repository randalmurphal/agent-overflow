package linearprovider

import (
	"errors"
	"fmt"
	"strings"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear"
	"agent-overflow/internal/tracker/trackerapi"
)

// outcome is what err, the error of one write request, proves about it,
// by the client's mutation outcome table:
//
//   - nil: applied;
//   - the client refused the input before sending (trackerapi.ErrInvalidInput):
//     refused as invalid;
//   - the request never left (*trackerapi.NotSentError: the call ended,
//     the rate gate was shut, the client was closed) or Linear's rate
//     limit refused it (*trackerapi.RateLimitedError): not sent, since
//     nobody judged it;
//   - linear.WriteNotApplied: a present success: false, an HTTP 4xx, or
//     user errors with no payload: refused by the tracker;
//   - anything else, a success: true without the stored entity included:
//     unknown.
func outcome(err error) tracker.Outcome {
	if err == nil {
		return tracker.Applied()
	}
	if errors.Is(err, trackerapi.ErrInvalidInput) {
		return tracker.Refused(tracker.Refusal{Reason: tracker.ReasonInvalid, Message: err.Error(), Err: err})
	}
	if _, ok := errors.AsType[*trackerapi.NotSentError](err); ok {
		return tracker.NotSent(err)
	}
	if _, ok := errors.AsType[*trackerapi.RateLimitedError](err); ok {
		return tracker.NotSent(err)
	}
	if linear.WriteNotApplied(err) {
		return tracker.Refused(tracker.Refusal{Reason: tracker.ReasonTracker, Message: refusalMessage(err), Err: err})
	}
	return tracker.Unknown(err)
}

// refusalMessage is Linear's own reason for a refusal: its errors'
// messages, or the transport's reading of the answer.
func refusalMessage(err error) string {
	if e, ok := errors.AsType[*linear.Error](err); ok && len(e.Errors) > 0 {
		msgs := make([]string, 0, len(e.Errors))
		for _, g := range e.Errors {
			msgs = append(msgs, g.Message)
		}
		return strings.Join(msgs, "; ")
	}
	if e, ok := errors.AsType[*trackerapi.StatusError](err); ok && e.Message != "" {
		return e.Message
	}
	if _, ok := errors.AsType[*trackerapi.AuthError](err); ok {
		return "Linear refused the key"
	}
	return "Linear refused the write"
}

// part is one request's part: its outcome from err, and what it may have
// changed.
func part(issue tracker.IssueID, step tracker.Step, err error, changed ...tracker.Resource) tracker.Part {
	return tracker.Part{Issue: issue, Step: step, Outcome: outcome(err), Changed: changed}
}

// one is a result of one request.
func one(issue tracker.IssueID, step tracker.Step, err error, changed ...tracker.Resource) tracker.Result {
	return tracker.Result{Parts: []tracker.Part{part(issue, step, err, changed...)}}
}

// unsupported refuses a write Linear cannot make, before any request.
func unsupported(step tracker.Step, what string, issues ...tracker.IssueID) tracker.Result {
	return refuse(step, tracker.Refusal{Reason: tracker.ReasonUnsupported, Message: "Linear does not support " + what}, issues...)
}

// refuse is a result refused before any request, for the reason r.
func refuse(step tracker.Step, r tracker.Refusal, issues ...tracker.IssueID) tracker.Result {
	if r.Err == nil {
		r.Err = refusalErr(r.Reason, r.Message, nil)
	}
	return tracker.Refuse(step, r, issues...)
}

// refusalErr is the error of a refusal the provider made, saying msg: it
// matches tracker.ErrInvalidInput or tracker.ErrUnsupported for those
// reasons, and wraps cause, the tracker's answer it rests on, when there
// is one.
func refusalErr(reason tracker.Reason, msg string, cause error) error {
	var err error
	switch reason {
	case tracker.ReasonInvalid:
		err = fmt.Errorf("linearprovider: %w: %s", tracker.ErrInvalidInput, msg)
	case tracker.ReasonUnsupported:
		err = fmt.Errorf("linearprovider: %w: %s", tracker.ErrUnsupported, msg)
	default:
		err = errors.New("linearprovider: " + msg)
	}
	if cause != nil {
		err = fmt.Errorf("%w: %w", err, cause)
	}
	return err
}

// notIssueID refuses, before any request, an ID that is not a Linear
// issue ID.
func notIssueID(step tracker.Step, bad tracker.IssueID, issues ...tracker.IssueID) tracker.Result {
	return refuse(step, tracker.Refusal{Reason: tracker.ReasonInvalid, Message: fmt.Sprintf("%q is not a Linear issue ID", bad)}, issues...)
}

// beforeWrite is a result for a read the write needed that failed: the
// write was not sent. What Linear finds missing is a refusal the tracker
// made; a client's refusal of the read's input is a refusal as invalid.
func beforeWrite(step tracker.Step, err error, issues ...tracker.IssueID) tracker.Result {
	if errors.Is(err, trackerapi.ErrNotFound) {
		return refuse(step, tracker.Refusal{Reason: tracker.ReasonTracker, Message: "Linear finds no such entity, or the key cannot see it: " + refusalMessage(err), Err: err}, issues...)
	}
	if errors.Is(err, tracker.ErrInvalidInput) {
		return tracker.Invalid(step, err, issues...)
	}
	sent := fmt.Errorf("a read the write needed failed: %w", err)
	if len(issues) == 0 {
		return tracker.Result{Parts: []tracker.Part{{Step: step, Outcome: tracker.NotSent(sent)}}}
	}
	parts := make([]tracker.Part, len(issues))
	for i, id := range issues {
		parts[i] = tracker.Part{Issue: id, Step: step, Outcome: tracker.NotSent(sent)}
	}
	return tracker.Result{Parts: parts}
}
