package jira

import (
	"fmt"
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

// MatchError is text that names several candidates or none. What says
// what was looked up ("Priority value", "field", "user"); Candidates are
// the matches when Ambiguous, and otherwise every value that was allowed
// or suggested (empty when the tracker offered none).
type MatchError struct {
	What       string
	Input      string
	Ambiguous  bool
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
	if e.Ambiguous {
		return fmt.Sprintf("jira: %q matches %d of %s: %s", e.Input, len(e.Candidates), e.What, strings.Join(names, ", "))
	}
	if len(names) == 0 {
		return fmt.Sprintf("jira: no %s matches %q", e.What, e.Input)
	}
	return fmt.Sprintf("jira: no %s matches %q; candidates: %s", e.What, e.Input, strings.Join(names, ", "))
}

// match finds want among cands by ID, then by name ignoring case. One hit
// wins; among several, the one whose ID or name is exactly want wins
// when only one is. Otherwise the error lists the hits, or every
// candidate when nothing matched.
func match(what, want string, cands []Candidate) (Candidate, error) {
	var hits, exact []Candidate
	for _, c := range cands {
		switch {
		case c.ID != "" && c.ID == want:
			hits, exact = append(hits, c), append(exact, c)
		case strings.EqualFold(c.Name, want):
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
	return fmt.Sprintf("jira: %s on %s changed since the edit was planned; plan it again", e.What, e.Issue)
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
	// StepUpload attaches a local image the Markdown embeds.
	StepUpload Step = "upload"
	// StepCreate creates the issue.
	StepCreate Step = "create"
	// StepEdit writes the issue's fields.
	StepEdit Step = "edit"
	// StepFinish writes, after a create, the rich text that embeds the
	// images uploaded to the new issue.
	StepFinish Step = "finish"
	// StepTransition moves the issue.
	StepTransition Step = "transition"
	// StepComment adds a comment.
	StepComment Step = "comment"
)

// StepError is the failure of one step of a multi-request write. The
// result returned with it says what the earlier steps did.
type StepError struct {
	Step Step
	Err  error
}

func (e *StepError) Error() string { return fmt.Sprintf("jira: %s step: %v", e.Step, e.Err) }
func (e *StepError) Unwrap() error { return e.Err }

// OutcomeUnknown reports whether the failed step may have been applied:
// a write the tracker did not answer in full. A caller reads the issue
// before it offers the write again.
func (e *StepError) OutcomeUnknown() bool {
	return e.Step != StepCheck && !trackerapi.WriteNotApplied(e.Err)
}
