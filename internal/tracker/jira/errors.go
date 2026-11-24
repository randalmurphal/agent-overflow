package jira

import (
	"fmt"
	"net/http"
	"strings"

	"agent-overflow/internal/tracker/trackerapi"
)

// Candidate is one thing a name could have meant: an option, a user, a
// field, an issue type, a transition.
type Candidate struct {
	ID   string
	Name string
}

func (c Candidate) String() string {
	if c.ID == "" || c.ID == c.Name {
		return c.Name
	}
	return c.Name + " (" + c.ID + ")"
}

// MatchError is text that names several candidates or none, or a picked
// ID (Picked) that names none: a value picked from a list that changed
// since. What says what was looked up ("Priority value", "field",
// "user"); Candidates are the matches when Ambiguous, and otherwise every
// value that was allowed or suggested (empty when the tracker offered
// none).
type MatchError struct {
	What       string
	Input      string
	Ambiguous  bool
	Picked     bool
	Candidates []Candidate
}

// maxListed bounds the candidates an error message names; Candidates
// holds them all.
const maxListed = 20

func (e *MatchError) Error() string {
	names := make([]string, 0, min(len(e.Candidates), maxListed))
	for i, c := range e.Candidates {
		if i == maxListed {
			names = append(names, fmt.Sprintf("and %d more", len(e.Candidates)-maxListed))
			break
		}
		names = append(names, c.String())
	}
	switch {
	case e.Ambiguous:
		return fmt.Sprintf("jira: %q matches %d of %s: %s", e.Input, len(e.Candidates), e.What, strings.Join(names, ", "))
	case e.Picked && len(names) == 0:
		return fmt.Sprintf("jira: no %s has ID %q now; pick it again", e.What, e.Input)
	case e.Picked:
		return fmt.Sprintf("jira: no %s has ID %q now; pick again from %s", e.What, e.Input, strings.Join(names, ", "))
	}
	if len(names) == 0 {
		return fmt.Sprintf("jira: no %s matches %q", e.What, e.Input)
	}
	return fmt.Sprintf("jira: no %s matches %q; candidates: %s", e.What, e.Input, strings.Join(names, ", "))
}

// matchID finds the candidate whose ID is exactly id, as picked; no name
// is compared. A miss is a *MatchError with Picked set, listing cands.
func matchID(what, id string, cands []Candidate) (Candidate, error) {
	for _, c := range cands {
		if c.ID != "" && c.ID == id {
			return c, nil
		}
	}
	return Candidate{}, &MatchError{What: what, Input: id, Picked: true, Candidates: cands}
}

// match finds want among cands. An exact ID wins first and is never
// ambiguous; names are compared, case ignored, only when no ID is want.
// One name hit wins; among several, the one spelled exactly as want wins
// when only one is. Otherwise the error lists the hits, or every
// candidate when nothing matched.
func match(what, want string, cands []Candidate) (Candidate, error) {
	for _, c := range cands {
		if c.ID != "" && c.ID == want {
			return c, nil
		}
	}
	var hits, exact []Candidate
	for _, c := range cands {
		if strings.EqualFold(c.Name, want) {
			hits = append(hits, c)
			if c.Name == want {
				exact = append(exact, c)
			}
		}
	}
	switch {
	case len(hits) == 1:
		return hits[0], nil
	case len(exact) == 1:
		return exact[0], nil
	case len(hits) > 1:
		return Candidate{}, &MatchError{What: what, Input: want, Ambiguous: true, Candidates: hits}
	}
	return Candidate{}, &MatchError{What: what, Input: want, Candidates: cands}
}

// StaleError is a plan or an edit whose starting point changed on the
// tracker before it was sent: a rich text field's stored document, or a
// local image's content. Nothing was sent for it.
type StaleError struct {
	Issue string
	What  string // the field name or image reference
}

func (e *StaleError) Error() string {
	if e.Issue == "" {
		return fmt.Sprintf("jira: %s changed since the write was planned; plan it again", e.What)
	}
	return fmt.Sprintf("jira: %s on %s changed since the write was planned; plan it again", e.What, e.Issue)
}

// MissingFieldsError is a create that leaves required fields unset.
type MissingFieldsError struct {
	Fields []FieldMeta
}

func (e *MissingFieldsError) Error() string {
	names := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		names[i] = f.Name
	}
	return "jira: required fields are not set: " + strings.Join(names, ", ")
}

// Step is one request of a write that needs several.
type Step string

const (
	// StepCheck reads the issue again to see that a plan still applies.
	StepCheck Step = "check"
	// StepUpload is one attach request: of a local image the Markdown
	// embeds, or of the files of Attach.
	StepUpload Step = "upload"
	// StepMediaID reads the media ID of an image just attached, which the
	// document embedding it names it by. It writes nothing: its failure is
	// a *trackerapi.NotSentError, and the write that would have followed
	// was not sent.
	StepMediaID Step = "media ID"
	// StepCreate creates the issue.
	StepCreate Step = "create"
	// StepEdit writes the issue's fields.
	StepEdit Step = "edit"
	// StepFinish writes, after a create, the rich text that embeds the
	// images uploaded to the new issue.
	StepFinish Step = "finish"
	// StepTransition moves the issue.
	StepTransition Step = "transition"
	// StepComment adds or edits a comment.
	StepComment Step = "comment"
	// StepRemoteLink adds a remote link.
	StepRemoteLink Step = "remote link"
	// StepRank ranks a group of issues.
	StepRank Step = "rank"
	// StepMove moves a group of issues into a sprint or to the backlog.
	StepMove Step = "move"
	// StepSprint creates, changes, starts, closes or deletes a sprint.
	StepSprint Step = "sprint"
	// StepEstimate sets an issue's estimate through a board.
	StepEstimate Step = "estimate"
	// StepWorklog adds, updates or deletes a worklog.
	StepWorklog Step = "worklog"
)

// StepError is the failure of one step of a write: a step of a
// multi-request write, or a single write whose answer could not be read
// in full. The result returned with it says what the earlier steps did,
// and what the failed step stored when it was applied. Applied,
// OutcomeUnknown and trackerapi.WriteNotApplied judge the step by its
// own outcome, the outermost in Err's chain (see write).
type StepError struct {
	Step Step
	Err  error
}

// unsent is the failure of step, a write never sent because what it needed
// from the requests before it could not be read or built (an uploaded
// image's media ID, the document that embeds it).
func unsent(step Step, err error) *StepError {
	return &StepError{Step: step, Err: &trackerapi.NotSentError{Err: err}}
}

func (e *StepError) Error() string { return fmt.Sprintf("jira: %s step: %v", e.Step, e.Err) }
func (e *StepError) Unwrap() error { return e.Err }

// Applied reports a step Jira applied whose answer this package could not
// read in full (an Applied *AnswerError): the answer is a documented
// success naming what was written, and the result carries what decoded.
func (e *StepError) Applied() bool {
	return answerApplied(e.Err)
}

// OutcomeUnknown reports whether the failed step may have been applied:
// a write the tracker did not answer in full. A caller reads the issue
// before it offers the write again.
func (e *StepError) OutcomeUnknown() bool {
	return e.Step != StepCheck && !e.Applied() && !trackerapi.WriteNotApplied(e.Err)
}

// UnprovenMatchError is a name more candidates share than the tracker
// lists: the first Limit matched, so a second exact match may lie beyond
// them and the name cannot be shown to mean one. Naming the candidate by
// its ID resolves it.
type UnprovenMatchError struct {
	What  string
	Input string
	Limit int
}

func (e *UnprovenMatchError) Error() string {
	return fmt.Sprintf("jira: %q matches more than the %d %s candidates Jira lists, so it cannot be shown to name one; name it by ID", e.Input, e.Limit, e.What)
}

// QueryRefusedError is Jira's 400 to the request that carried a query's
// JQL: an issue list narrowed by JQL, or a JQL match. Jira refused the
// query as sent; why is not judged. It holds that request's own
// *trackerapi.StatusError, never the failure of another request the
// operation made (a sign-in check that proves an answer).
type QueryRefusedError struct {
	Err *trackerapi.StatusError
}

func (e *QueryRefusedError) Error() string {
	return fmt.Sprintf("jira: the query was refused: %v", e.Err)
}

func (e *QueryRefusedError) Unwrap() error { return e.Err }

// refusedQuery is err, the failure of the request that carried a query's
// JQL, as a *QueryRefusedError when that request's own answer was a 400.
func refusedQuery(err error) error {
	if se, ok := outcomeOf(err).(*trackerapi.StatusError); ok && se.Status == http.StatusBadRequest {
		return &QueryRefusedError{Err: se}
	}
	return err
}
