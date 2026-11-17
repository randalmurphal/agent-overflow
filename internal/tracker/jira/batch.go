package jira

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"agent-overflow/internal/tracker/trackerapi"
)

// maxBatch is the most issues one rank or move request takes.
const maxBatch = 50

// Outcome is what a batch write did to one issue.
type Outcome uint8

const (
	// OutcomeNotSent is an issue no request named: the batch stopped
	// before its group.
	OutcomeNotSent Outcome = iota
	// OutcomeDone is an issue Jira ranked or moved.
	OutcomeDone
	// OutcomeRefused is an issue Jira did not rank or move; Reason says
	// why.
	OutcomeRefused
	// OutcomeUnknown is an issue whose request was sent but not answered
	// in full: read it before offering the write again.
	OutcomeUnknown
)

func (o Outcome) String() string {
	switch o {
	case OutcomeNotSent:
		return "not sent"
	case OutcomeDone:
		return "done"
	case OutcomeRefused:
		return "refused"
	case OutcomeUnknown:
		return "unknown"
	}
	return fmt.Sprintf("Outcome(%d)", uint8(o))
}

// IssueOutcome is one issue of a batch write: the reference the caller
// gave, what happened, and Jira's reason for a refusal.
type IssueOutcome struct {
	Issue   string
	Outcome Outcome
	Reason  string
}

// BatchResult reports every issue of a batch write, in the caller's
// order.
type BatchResult struct {
	Issues []IssueOutcome
}

// Done lists the issues Jira ranked or moved, in the caller's order.
func (r BatchResult) Done() []string {
	var out []string
	for _, o := range r.Issues {
		if o.Outcome == OutcomeDone {
			out = append(out, o.Issue)
		}
	}
	return out
}

// BatchError is a batch write that did not rank or move every issue; the
// BatchResult returned with it says which. Err is the failure of the
// group that stopped the batch, nil when every group was answered and
// Jira refused some issues one by one.
type BatchError struct {
	Step                      Step
	Refused, Unknown, NotSent int
	Err                       error
}

func (e *BatchError) Error() string {
	msg := fmt.Sprintf("jira: %s: %d refused, %d with an unknown outcome, %d not sent", e.Step, e.Refused, e.Unknown, e.NotSent)
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *BatchError) Unwrap() error { return e.Err }

// RankPosition is where ranked or moved issues go: before or after an
// issue. The zero RankPosition leaves the rank alone.
type RankPosition struct {
	before, after string
}

// RankBefore places the issues, in order, just before issue.
func RankBefore(issue string) RankPosition { return RankPosition{before: issue} }

// RankAfter places the issues, in order, just after issue.
func RankAfter(issue string) RankPosition { return RankPosition{after: issue} }

func (p RankPosition) set() bool { return p.before != "" || p.after != "" }

func (p RankPosition) anchor() string { return p.before + p.after }

// batchWrite is one rank or move of issues, sent in groups of maxBatch.
type batchWrite struct {
	step   Step
	path   string // from the site root
	method string
	issues []string
	pos    RankPosition
	field  int64 // the rank field; zero for Jira's default
}

// checkBatch refuses issues that are not issue references, repeat, or
// hold the position's anchor, and a rank field without a position.
func checkBatch(issues []string, pos RankPosition, field int64) error {
	if len(issues) == 0 {
		return notSent("no issues given")
	}
	seen := map[string]bool{}
	for _, ref := range issues {
		if err := checkIssue(ref); err != nil {
			return err
		}
		k := strings.ToUpper(ref)
		if seen[k] {
			return notSent("%s is named twice", ref)
		}
		seen[k] = true
	}
	switch {
	case field < 0:
		return notSent("rank field %d is not a field number", field)
	case !pos.set() && field != 0:
		return notSent("a rank field without a position")
	case !pos.set():
		return nil
	case seen[strings.ToUpper(pos.anchor())]:
		return notSent("%s cannot be ranked against itself", pos.anchor())
	}
	return checkIssue(pos.anchor())
}

// jsonID reads an ID Jira sends as a number or a string.
type jsonID string

func (id *jsonID) UnmarshalJSON(b []byte) error {
	*id = jsonID(idText(b))
	return nil
}

// runBatch sends w's issues in groups of maxBatch, in order. With a
// position, the first group goes there and each later group goes just
// after the last issue ranked so far, so the issues Jira takes end up
// together in the caller's order, and an issue Jira refuses stays where
// it was. It stops at the first group Jira refuses as a whole or does not
// answer in full.
func (c *Client) runBatch(ctx context.Context, w batchWrite) (BatchResult, error) {
	res := BatchResult{Issues: make([]IssueOutcome, len(w.issues))}
	for i, ref := range w.issues {
		res.Issues[i].Issue = ref
	}
	pos := w.pos
	var stop error
	for start := 0; start < len(w.issues) && stop == nil; start += maxBatch {
		group := res.Issues[start:min(start+maxBatch, len(w.issues))]
		stop = c.sendGroup(ctx, w, pos, group)
		for _, o := range group {
			if o.Outcome == OutcomeDone && pos.set() {
				pos = RankAfter(o.Issue)
			}
		}
	}
	return res, batchErr(w.step, res, stop)
}

// batchErr is the error of a batch whose outcomes res holds; nil when
// every issue is done.
func batchErr(step Step, res BatchResult, stop error) error {
	e := &BatchError{Step: step, Err: stop}
	for _, o := range res.Issues {
		switch o.Outcome {
		case OutcomeRefused:
			e.Refused++
		case OutcomeUnknown:
			e.Unknown++
		case OutcomeNotSent:
			e.NotSent++
		}
	}
	if stop == nil && e.Refused+e.Unknown+e.NotSent == 0 {
		return nil
	}
	return e
}

// sendGroup sends one group placed at pos and records each issue's
// outcome; an error stops the batch.
func (c *Client) sendGroup(ctx context.Context, w batchWrite, pos RankPosition, group []IssueOutcome) error {
	refs := make([]string, len(group))
	for i, o := range group {
		refs[i] = o.Issue
	}
	body := map[string]any{"issues": refs}
	if pos.before != "" {
		body["rankBeforeIssue"] = pos.before
	}
	if pos.after != "" {
		body["rankAfterIssue"] = pos.after
	}
	if w.field > 0 {
		body["rankCustomFieldId"] = w.field
	}
	resp, err := c.call(ctx, trackerapi.Request{Method: w.method, Path: w.path, Body: body}, nil)
	if err != nil {
		outcome := OutcomeUnknown
		if _, unsent := errors.AsType[*trackerapi.NotSentError](err); unsent {
			// Refused before sending, as an ended context is: Jira never
			// saw the group.
			outcome = OutcomeNotSent
		} else if trackerapi.WriteNotApplied(err) {
			outcome = OutcomeRefused
		}
		for i := range group {
			group[i].Outcome, group[i].Reason = outcome, err.Error()
		}
		return err
	}
	if resp.Status != http.StatusMultiStatus {
		for i := range group {
			group[i].Outcome = OutcomeDone
		}
		return nil
	}
	return applyEntries(resp, group)
}

// applyEntries reads a 207's per-issue results into group. An answer
// that does not decode, or leaves an issue out, makes those issues'
// outcome unknown and stops the batch.
func applyEntries(resp *trackerapi.Response, group []IssueOutcome) error {
	var v struct {
		Entries []struct {
			IssueID  jsonID   `json:"issueId"`
			IssueKey string   `json:"issueKey"`
			Status   int      `json:"status"`
			Errors   []string `json:"errors"`
		} `json:"entries"`
	}
	if err := decodeAnswer(resp, &v); err != nil {
		for i := range group {
			group[i].Outcome = OutcomeUnknown
		}
		// Not an *AnswerError: which issues Jira took is what is unknown.
		return fmt.Errorf("the per-issue answer did not decode, so which of %d issues Jira took is unknown: %v", len(group), err)
	}
	var missing int
	for i := range group {
		ref := group[i].Issue
		found := false
		for _, e := range v.Entries {
			if !strings.EqualFold(e.IssueKey, ref) && string(e.IssueID) != ref {
				continue
			}
			found = true
			if e.Status >= 200 && e.Status < 300 {
				group[i].Outcome = OutcomeDone
			} else {
				group[i].Outcome = OutcomeRefused
				group[i].Reason = fmt.Sprintf("%d: %s", e.Status, strings.Join(e.Errors, "; "))
			}
			break
		}
		if !found {
			group[i].Outcome = OutcomeUnknown
			missing++
		}
	}
	if missing > 0 {
		return fmt.Errorf("the per-issue answer left out %d issues, whose outcome is unknown", missing)
	}
	return nil
}

// Rank orders issues, in the given order, before or after an issue, in
// groups of 50; see runBatch for how groups chain and when the call
// stops. A failure is a *BatchError with the result saying what happened
// to each issue.
func (c *Client) Rank(ctx context.Context, in RankInput) (BatchResult, error) {
	if err := c.admit(ctx); err != nil {
		return BatchResult{}, err
	}
	if !in.Position.set() {
		return BatchResult{}, notSent("rank: no position")
	}
	if err := checkBatch(in.Issues, in.Position, in.RankFieldID); err != nil {
		return BatchResult{}, err
	}
	return c.runBatch(ctx, batchWrite{
		step: StepRank, path: agilePath + "issue/rank", method: http.MethodPut,
		issues: in.Issues, pos: in.Position, field: in.RankFieldID,
	})
}

// RankInput ranks Issues, in order, at Position.
type RankInput struct {
	Issues   []string
	Position RankPosition
	// RankFieldID is the number of the rank field (BoardConfig's); zero
	// uses Jira's default rank field.
	RankFieldID int64
}
