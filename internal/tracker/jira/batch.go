package jira

import (
	"context"
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
	// why when Jira's per-issue answer did, and is empty when Jira refused
	// the whole request, whose error the batch returns.
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
// gave, what happened, and Jira's reason for a refusal its per-issue
// answer gave.
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
	// multi reports a call documented to answer 207 with per-issue
	// results; every call is documented to answer 204 when all went well.
	multi bool
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
		case OutcomeDone:
		}
	}
	if stop == nil && e.Refused+e.Unknown+e.NotSent == 0 {
		return nil
	}
	return e
}

// sendGroup sends one group placed at pos and records each issue's
// outcome; an error stops the batch. The answer is read in the forms the
// spec documents: 204 (every issue done) or, where documented, a 207 read
// whole (see applyEntries). Any other 2xx leaves the whole group unknown.
// A failure is judged by its own outcome, the outermost in its chain.
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
		switch {
		case neverSent(err):
			outcome = OutcomeNotSent
		case trackerapi.WriteNotApplied(err):
			outcome = OutcomeRefused
		}
		// The error is the whole request's answer; it carries the request,
		// which no issue's reason repeats.
		setOutcome(group, outcome, "")
		return err
	}
	switch {
	case resp.Status == http.StatusNoContent:
		setOutcome(group, OutcomeDone, "")
		return nil
	case resp.Status == http.StatusMultiStatus && w.multi:
		return applyEntries(resp, group)
	}
	setOutcome(group, OutcomeUnknown, "")
	return &AnswerError{Message: fmt.Sprintf("the answer's status %d is not one this write documents, so which of %d issues Jira took is unknown", resp.Status, len(group))}
}

func setOutcome(group []IssueOutcome, o Outcome, reason string) {
	for i := range group {
		group[i].Outcome, group[i].Reason = o, reason
	}
}

// entryOutcome is what a 207 entry's status says of its issue: done for
// a 2xx; refused for a 4xx and for the 503 the spec's example shows for a
// rank Jira could not run then; anything else is a status this package
// does not know, so unknown.
func entryOutcome(status int) Outcome {
	switch {
	case status >= 200 && status <= 299:
		return OutcomeDone
	case status >= 400 && status <= 499 || status == http.StatusServiceUnavailable:
		return OutcomeRefused
	}
	return OutcomeUnknown
}

// applyEntries reads a 207's per-issue results into group. The answer is
// taken only whole: every entry must match exactly one issue of the
// group by its ID and key together, no two entries may name the same ID,
// key or issue, and every entry carries a status; otherwise which issues
// Jira took is unknown for the whole group. An issue with no entry, or an
// entry status this package does not know, is unknown. Any unknown issue
// stops the batch.
func applyEntries(resp *trackerapi.Response, group []IssueOutcome) error {
	var v struct {
		Entries *[]struct {
			IssueID  jsonID   `json:"issueId"`
			IssueKey string   `json:"issueKey"`
			Status   *int     `json:"status"`
			Errors   []string `json:"errors"`
		} `json:"entries"`
	}
	unknown := func(why string) error {
		setOutcome(group, OutcomeUnknown, "")
		// Not an *AnswerError: which issues Jira took is what is unknown.
		return fmt.Errorf("the per-issue answer %s, so which of %d issues Jira took is unknown", why, len(group))
	}
	if err := decodeAnswer(resp, &v); err != nil {
		return unknown("did not decode (" + err.Error() + ")")
	}
	if v.Entries == nil {
		return unknown("has no entries")
	}
	found := make([]bool, len(group))
	seen := map[string]bool{}
	for _, e := range *v.Entries {
		id, key := string(e.IssueID), strings.ToUpper(e.IssueKey)
		var hits []int
		for i, o := range group {
			if id != "" && id == o.Issue || key != "" && key == strings.ToUpper(o.Issue) {
				hits = append(hits, i)
			}
		}
		switch {
		case len(hits) == 0:
			return unknown("names an issue not in the request")
		case len(hits) > 1:
			return unknown("has an entry whose ID and key name different issues of the request")
		case id != "" && seen["id "+id] || key != "" && seen["key "+key]:
			return unknown("names " + group[hits[0]].Issue + " twice")
		case e.Status == nil:
			return unknown("has an entry without a status")
		}
		i := hits[0]
		seen["id "+id], seen["key "+key] = true, true
		found[i] = true
		group[i].Outcome = entryOutcome(*e.Status)
		if group[i].Outcome != OutcomeDone {
			group[i].Reason = fmt.Sprintf("%d: %s", *e.Status, strings.Join(e.Errors, "; "))
		}
	}
	var open int
	for i := range group {
		if !found[i] {
			group[i].Outcome = OutcomeUnknown
		}
		if group[i].Outcome == OutcomeUnknown {
			open++
		}
	}
	if open > 0 {
		return fmt.Errorf("the per-issue answer leaves %d issues without a known outcome", open)
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
		issues: in.Issues, pos: in.Position, field: in.RankFieldID, multi: true,
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
