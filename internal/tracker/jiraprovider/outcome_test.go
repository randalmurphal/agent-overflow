package jiraprovider

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/adf"
	"agent-overflow/internal/tracker/jira"
	"agent-overflow/internal/tracker/tempo"
	"agent-overflow/internal/tracker/trackerapi"
)

// TestOutcomeClasses proves the one outcome helper reads each class of
// client error as the state and reason the neutral model gives it.
func TestOutcomeClasses(t *testing.T) {
	status := func(code int) *trackerapi.StatusError {
		return &trackerapi.StatusError{Tracker: "jira", Status: code, Message: "", Fields: nil}
	}
	refused := &trackerapi.StatusError{Tracker: "jira", Status: 400, Message: "Field errors", Fields: map[string]string{"summary": "too long", "customfield_10050": "bad"}}
	notSent := func(err error) error { return &trackerapi.NotSentError{Err: err} }
	invalid := trackerapi.InvalidInput
	for _, tc := range []struct {
		name   string
		err    error
		state  tracker.State
		reason tracker.Reason
	}{
		{"success", nil, tracker.StateApplied, 0},
		{"a step applied with an unread answer", &jira.StepError{Step: jira.StepCreate, Err: &jira.AnswerError{Applied: true, Message: "x"}}, tracker.StateApplied, 0},
		{"an answer applied but unread", fmt.Errorf("wrap: %w", &jira.AnswerError{Applied: true, Message: "x"}), tracker.StateApplied, 0},
		{"an answer not known applied", &jira.AnswerError{Message: "x"}, tracker.StateUnknown, 0},
		{"a 5xx answer", status(502), tracker.StateUnknown, 0},
		{"a transient failure", &trackerapi.TransientError{Err: errors.New("reset")}, tracker.StateUnknown, 0},
		{"a plain error", errors.New("odd"), tracker.StateUnknown, 0},
		{"a cancelled call", notSent(context.Canceled), tracker.StateNotSent, 0},
		{"a deadline", notSent(context.DeadlineExceeded), tracker.StateNotSent, 0},
		{"a closed Tempo client", notSent(tempo.ErrClosed), tracker.StateNotSent, 0},
		{"a rate limit answer", &trackerapi.RateLimitedError{Tracker: "jira", Status: 429, Until: time.Now()}, tracker.StateNotSent, 0},
		{"a read before the write failed with a 5xx", notSent(status(503)), tracker.StateNotSent, 0},
		{"a read before the write failed in transit", notSent(&trackerapi.TransientError{Err: errors.New("reset")}), tracker.StateNotSent, 0},
		{"a 400 with field messages", refused, tracker.StateRefused, tracker.ReasonTracker},
		{"a 403", &trackerapi.AuthError{Tracker: "jira", Status: 403}, tracker.StateRefused, tracker.ReasonTracker},
		{"a stale field", invalid(&jira.StaleError{Issue: "WEB-1", What: "Description"}), tracker.StateRefused, tracker.ReasonStale},
		{"a loss", invalid(&adf.LossError{Kinds: []adf.LossKind{adf.LossMarkers}}), tracker.StateRefused, tracker.ReasonLoss},
		{"no match", invalid(&jira.MatchError{What: "priority", Input: "Urgent", Candidates: []jira.Candidate{{ID: "1", Name: "High"}}}), tracker.StateRefused, tracker.ReasonNoMatch},
		{"several matches", invalid(&jira.MatchError{What: "user", Input: "Pat", Ambiguous: true, Candidates: []jira.Candidate{{ID: "a", Name: "Pat"}, {ID: "b", Name: "pat"}}}), tracker.StateRefused, tracker.ReasonAmbiguous},
		{"an unproven match", invalid(&jira.UnprovenMatchError{What: "user", Input: "P", Limit: 1000}), tracker.StateRefused, tracker.ReasonAmbiguous},
		{"missing fields", invalid(&jira.MissingFieldsError{Fields: []jira.FieldMeta{{ID: "customfield_10050", Name: "Severity"}}}), tracker.StateRefused, tracker.ReasonMissing},
		{"a Tempo refusal", invalid(&tempo.InvalidError{Problems: []tempo.Problem{{Message: "bad day"}}}), tracker.StateRefused, tracker.ReasonInvalid},
		{"a client check", invalid(errors.New("an issue named twice")), tracker.StateRefused, tracker.ReasonInvalid},
		{"a check of this package", notSent(invalidf("not an ID")), tracker.StateRefused, tracker.ReasonInvalid},
		{"a read before the write refused with a 404", notSent(status(404)), tracker.StateRefused, tracker.ReasonTracker},
		{"a credential that could not be read", notSent(errors.New("the keyring is locked")), tracker.StateNotSent, 0},
		{"a typed refusal not marked as one", notSent(&jira.StaleError{Issue: "WEB-1", What: "Description"}), tracker.StateNotSent, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := outcome(tc.err)
			if o.State() != tc.state {
				t.Fatalf("state %s, want %s", o.State(), tc.state)
			}
			if r, ok := o.Refusal(); ok && r.Reason != tc.reason {
				t.Errorf("reason %d, want %d", r.Reason, tc.reason)
			}
			if tc.state != tracker.StateApplied && !errors.Is(o.Err(), tc.err) {
				t.Errorf("Err %v does not carry %v", o.Err(), tc.err)
			}
		})
	}
}

// TestRefusalDetails proves a refusal carries the field messages under
// neutral IDs, the candidates and the lost content with how to keep it.
func TestRefusalDetails(t *testing.T) {
	r, _ := outcome(&trackerapi.StatusError{Status: 400, Message: "Field errors", Fields: map[string]string{"summary": "too long", "customfield_10050": "bad"}}).Refusal()
	want := []tracker.FieldMessage{{Field: "customfield_10050", Message: "bad"}, {Field: tracker.FieldTitle, Message: "too long"}}
	if r.Message != "Field errors" || !slices.Equal(r.Fields, want) {
		t.Errorf("tracker refusal %+v", r)
	}
	r, _ = outcome(&trackerapi.StatusError{Status: 409}).Refusal()
	if r.Message != "the tracker refused the write (HTTP 409)" {
		t.Errorf("a refusal without a message reads %q", r.Message)
	}
	r, _ = outcome(trackerapi.InvalidInput(&jira.MatchError{What: "priority", Input: "Urgent", Candidates: []jira.Candidate{{ID: "1", Name: "High"}}})).Refusal()
	if !slices.Equal(r.Candidates, []tracker.Choice{{ID: "1", Name: "High"}}) {
		t.Errorf("candidates %+v", r.Candidates)
	}
	r, _ = outcome(trackerapi.InvalidInput(&adf.LossError{Kinds: []adf.LossKind{adf.LossMarkers}})).Refusal()
	if len(r.Lost) != 1 || r.Lost[0] != adf.LossMarkers.Keep() {
		t.Errorf("lost %q", r.Lost)
	}
	r, _ = outcome(trackerapi.InvalidInput(&jira.MissingFieldsError{Fields: []jira.FieldMeta{{ID: "issuetype", Name: "Issue Type"}}})).Refusal()
	if !slices.Equal(r.Fields, []tracker.FieldMessage{{Field: tracker.FieldType, Message: "Issue Type is required"}}) {
		t.Errorf("missing %+v", r.Fields)
	}
}

// TestClosedClientIsNotSent proves a write on a closed Jira client is not
// sent, not refused: the client was closed, its input was not judged.
func TestClosedClientIsNotSent(t *testing.T) {
	f, _ := newSite(t)
	c := newJira(t, f)
	p, err := New(Options{Jira: c, Site: "https://example.atlassian.net", SprintsFrom: allSprints})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	for name, res := range map[string]tracker.Result{
		"edit":     p.Edit(t.Context(), tracker.EditInput{Issue: "10001", Fields: []tracker.FieldEdit{{Field: "priority", Value: tracker.PickValues("1")}}}),
		"rank":     p.Rank(t.Context(), tracker.RankInput{Board: "3", Issues: []tracker.IssueID{"10001"}, Next: "10002"}),
		"estimate": p.SetEstimate(t.Context(), tracker.EstimateInput{Board: "3", Issue: "10001", Value: ptr(1.0)}),
		"link":     p.Link(t.Context(), tracker.LinkInput{Type: "10000", Outward: "10001", Inward: "10002"}),
		"worklog":  p.Time().DeleteWorklog(t.Context(), tracker.WorklogDelete{From: tracker.Worklog{ID: "300", Issue: "10001"}}),
	} {
		checked(t, res)
		if res.Tally().NotSent != len(res.Parts) {
			t.Errorf("%s: %s", name, states(res))
		}
	}
	if n := len(f.Requests()); n != 0 {
		t.Errorf("%d requests on a closed client", n)
	}
}
