package jiraprovider

import (
	"errors"
	"fmt"
	"maps"
	"slices"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/adf"
	"agent-overflow/internal/tracker/jira"
	"agent-overflow/internal/tracker/tempo"
	"agent-overflow/internal/tracker/trackerapi"
)

// outcome is what became of one request of a write, read from the error
// the client returned for it. It is the one place a client error becomes
// a tracker.Outcome; every write's parts are judged here, except the
// per-issue outcomes a batch reports itself (see batchParts).
//
//   - nil, or a step the client reports applied with an answer it could
//     not read in full: applied.
//   - an error trackerapi.WriteNotApplied does not prove unsent or
//     refused (a timeout, a 5xx, a 2xx that did not read): unknown. The
//     provider never sends it again.
//   - not applied, and its input refused before sending, by a client
//     (trackerapi.ErrInvalidInput) or by this package's own checks
//     (tracker.ErrInvalidInput): refused, with the reason the error's
//     type carries (refusal).
//   - not applied, and Jira or Tempo answered a 4xx other than a rate
//     limit, to the write or to a read it depended on: refused by the
//     tracker.
//   - any other write not applied, which nobody judged (the call ended,
//     the client was closed, the credential could not be read, the rate
//     gate was closed, the connection was never made, a read it depended
//     on failed with a 5xx): not sent.
func outcome(err error) tracker.Outcome {
	switch {
	case err == nil || applied(err):
		return tracker.Applied()
	case !trackerapi.WriteNotApplied(err):
		return tracker.Unknown(err)
	case errors.Is(err, trackerapi.ErrInvalidInput) || errors.Is(err, tracker.ErrInvalidInput) || answered4xx(err):
		return tracker.Refused(refusal(err))
	}
	return tracker.NotSent(err)
}

// stale is a check of this package's that found the starting point of an
// edit changed: a refusal of the input, as the client's own are.
func stale(issue, what string) error {
	return trackerapi.InvalidInput(&jira.StaleError{Issue: issue, What: what})
}

// beforeSend is the outcome of a write that failed before its request
// left: err is a read or check the write depended on, which outcome must
// not read as the write's own answer.
func beforeSend(err error) tracker.Outcome {
	return outcome(&trackerapi.NotSentError{Err: err})
}

// applied reports a write the client says Jira applied although its
// answer did not read in full.
func applied(err error) bool {
	if se, ok := errors.AsType[*jira.StepError](err); ok {
		return se.Applied()
	}
	ae, ok := errors.AsType[*jira.AnswerError](err)
	return ok && ae.Applied
}

// answered4xx reports an error carrying the tracker's 4xx answer: a
// refused credential, or any other status from 400 to 499. A rate limit
// is not one: it is a *trackerapi.RateLimitedError, which judges nothing.
func answered4xx(err error) bool {
	if _, ok := errors.AsType[*trackerapi.AuthError](err); ok {
		return true
	}
	se, ok := errors.AsType[*trackerapi.StatusError](err)
	return ok && se.Status >= 400 && se.Status <= 499
}

// refusal is why a write that was not applied was refused. A client
// check names its reason by its type; a tracker's answer is
// ReasonTracker; any other refusal the client made before sending is
// ReasonInvalid.
func refusal(err error) tracker.Refusal {
	r := tracker.Refusal{Reason: tracker.ReasonInvalid, Message: err.Error(), Err: err}
	if e, ok := errors.AsType[*jira.StaleError](err); ok {
		r.Reason, r.Message = tracker.ReasonStale, e.Error()
		return r
	}
	if e, ok := errors.AsType[*adf.LossError](err); ok {
		r.Reason, r.Message = tracker.ReasonLoss, e.Error()
		for _, w := range e.Lost {
			r.Lost = append(r.Lost, w.String())
		}
		for _, k := range e.Kinds {
			r.Lost = append(r.Lost, k.Keep())
		}
		return r
	}
	if e, ok := errors.AsType[*jira.MatchError](err); ok {
		r.Reason, r.Message = tracker.ReasonNoMatch, e.Error()
		if e.Ambiguous {
			r.Reason = tracker.ReasonAmbiguous
		}
		for _, c := range e.Candidates {
			r.Candidates = append(r.Candidates, tracker.Choice{ID: c.ID, Name: c.Name})
		}
		return r
	}
	if e, ok := errors.AsType[*jira.UnprovenMatchError](err); ok {
		r.Reason, r.Message = tracker.ReasonAmbiguous, e.Error()
		return r
	}
	if e, ok := errors.AsType[*jira.MissingFieldsError](err); ok {
		r.Reason, r.Message = tracker.ReasonMissing, e.Error()
		for _, f := range e.Fields {
			r.Fields = append(r.Fields, tracker.FieldMessage{Field: neutralField(f.ID), Message: f.Name + " is required"})
		}
		return r
	}
	if e, ok := errors.AsType[*tempo.InvalidError](err); ok {
		r.Message = e.Error()
		return r
	}
	if e, ok := errors.AsType[*notOnIssueError](err); ok {
		// Before the status: the not found it carries is the reason, not
		// the tracker's refusal of a write.
		r.Message = e.Error()
		return r
	}
	if e, ok := errors.AsType[*trackerapi.AuthError](err); ok {
		r.Reason, r.Message = tracker.ReasonTracker, fmt.Sprintf("the tracker refused the credential (HTTP %d)", e.Status)
		return r
	}
	if e, ok := errors.AsType[*trackerapi.StatusError](err); ok {
		r.Reason, r.Message = tracker.ReasonTracker, e.Message
		if r.Message == "" {
			r.Message = fmt.Sprintf("the tracker refused the write (HTTP %d)", e.Status)
		}
		for _, id := range slices.Sorted(maps.Keys(e.Fields)) {
			r.Fields = append(r.Fields, tracker.FieldMessage{Field: neutralField(id), Message: e.Fields[id]})
		}
	}
	return r
}

// The card's fields that Jira names otherwise; every other field keeps
// Jira's ID, which tracker reserves none of (assignee, priority, labels,
// parent and description are the same on both sides).
const (
	jiraSummary   = "summary"
	jiraIssueType = "issuetype"
)

// jiraField is the Jira field ID of a neutral field ID.
func jiraField(f tracker.FieldID) string {
	switch f {
	case tracker.FieldTitle:
		return jiraSummary
	case tracker.FieldType:
		return jiraIssueType
	}
	return string(f)
}

// neutralField is the neutral ID of a Jira field ID.
func neutralField(id string) tracker.FieldID {
	switch id {
	case jiraSummary:
		return tracker.FieldTitle
	case jiraIssueType:
		return tracker.FieldType
	}
	return tracker.FieldID(id)
}
