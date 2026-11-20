package linear

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"agent-overflow/internal/tracker/trackerapi"
)

// Writes of many issues in few requests. issueBatchUpdate applies one
// input to at most 50 issues and answers with them (3 points for three,
// measured); aliased issueUpdate fields in one document each apply
// their own input (7 points for three, measured). maxBatch bounds both:
// 51 IDs answer INVALID_INPUT (measured).
//
// One failing case of each was measured (a missing ID failed the batch
// whole and changed none; a missing issue at one alias nulled the data
// and neither other alias was applied), so a failed request proves
// nothing about its parts: after any failure of a request that was
// sent, one read of its issues by ID settles each part from what Linear
// holds then.
const maxBatch = 50

// IssueWrite is one issue's part of a write that named several.
type IssueWrite struct {
	IssueID string
	// Issue is the issue as stored when Err is nil: from the write's
	// answer, or from the read that settled a failed write.
	Issue *Issue
	// Err is nil when the issue holds what was written. Otherwise
	// WriteNotApplied(Err) reports a part Linear did not apply, a
	// *trackerapi.NotSentError one never sent (its request was refused
	// before leaving, or a request before it failed), and an
	// *OutcomeUnknownError one nothing settled.
	Err error
}

var opUpdateIssues = newMutation("UpdateIssues", `mutation UpdateIssues($ids: [UUID!]!, $input: IssueUpdateInput!) {
  result: issueBatchUpdate(ids: $ids, input: $input) { success value: issues { ...IssueFields } }
}
`+issueFields)

type batchVars struct {
	IDs   []string   `json:"ids"`
	Input issueInput `json:"input"`
}

// SetCycle moves the issues ids names into the cycle cycleID, or out of
// any cycle when it is empty (subject to the team's automatic cycle
// assignment), with one batch update per maxBatch issues, in order. ids
// are issue IDs (IsIssueID), each named once; anything else is refused
// before any request, with no parts. Otherwise there is one part per
// issue, in order; the issues after a request with a part not applied
// are not sent.
func (c *Client) SetCycle(ctx context.Context, ids []string, cycleID string) ([]IssueWrite, error) {
	if err := checkIssueIDs(ids); err != nil {
		return nil, invalid(opUpdateIssues.name, "%w", err)
	}
	cycle := Null[string]()
	if cycleID != "" {
		cycle = Set(cycleID)
	}
	holds := func(_ int, is Issue) bool { return is.CycleID == cycleID }
	return c.writeChunks(ctx, ids, holds, func(ctx context.Context, chunk []string, _ int) (map[string]Issue, error) {
		var data mutationData[[]wireIssue]
		_, err := c.call(ctx, opUpdateIssues, batchVars{chunk, issueInput{CycleID: cycle}}, &data)
		stored := map[string]Issue{}
		if err == nil && data.Result.Value != nil {
			for _, w := range *data.Result.Value {
				stored[w.ID] = w.typed()
			}
		}
		return stored, err
	}), nil
}

// renumberFamily sets one order field on each of up to maxBatch issues,
// each with its own value, as aliased issueUpdate fields.
var renumberFamily = newMutationFamily("RenumberIssues", maxBatch, func(n int) string {
	var b strings.Builder
	b.WriteString("mutation RenumberIssues(")
	for i := range n {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "$id%d: String!, $input%d: IssueUpdateInput!", i, i)
	}
	b.WriteString(") {\n")
	for i := range n {
		fmt.Fprintf(&b, "  i%d: issueUpdate(id: $id%d, input: $input%d) { success value: issue { ...IssueFields } }\n", i, i, i)
	}
	b.WriteString("}\n" + issueFields)
	return b.String()
})

// aliasData is the data of a family's answer, by alias.
type aliasData map[string]*payload[wireIssue]

// state is the document's outcome: succeeded only when every alias's
// payload says so.
func (d aliasData) state() payloadState {
	if len(d) == 0 {
		return payloadNone
	}
	out := payloadSucceeded
	for _, p := range d {
		var s payloadState
		if p != nil {
			s = successState(p.Success)
		}
		out = min(out, s)
	}
	return out
}

func alias(i int) string { return "i" + strconv.Itoa(i) }

// RenumberIssues writes Spread values of Field to the run, in the order
// listed, between the neighbors of the run, maxBatch issues per request
// as aliased updates. IssueIDs are issue IDs (Issue.ID, a lowercase
// UUID), never identifiers such as WEB-1, so that each issue has one
// spelling; any other form, an ID listed twice, an unknown Field and a
// run with no room (ErrNoRoom) are refused before any request, with no
// parts. Otherwise there is one part per issue, in order; the issues
// after a request with a part not applied are not sent.
func (c *Client) RenumberIssues(ctx context.Context, in RenumberInput) ([]IssueWrite, error) {
	if err := checkIssueIDs(in.IssueIDs); err != nil {
		return nil, invalid(renumberFamily.name, "%w", err)
	}
	if err := in.Field.set(&issueInput{}, 0); err != nil {
		return nil, invalid(renumberFamily.name, "%w", err)
	}
	values, err := Spread(in.Before, in.After, len(in.IssueIDs))
	if err != nil {
		return nil, invalid(renumberFamily.name, "%w", err)
	}
	holds := func(i int, is Issue) bool { return in.Field.of(is) == values[i] }
	return c.writeChunks(ctx, in.IssueIDs, holds, func(ctx context.Context, chunk []string, base int) (map[string]Issue, error) {
		vars := make(map[string]any, 2*len(chunk))
		for i, id := range chunk {
			var w issueInput
			_ = in.Field.set(&w, values[base+i]) // checked above
			vars["id"+strconv.Itoa(i)], vars["input"+strconv.Itoa(i)] = id, w
		}
		var data aliasData
		_, err := c.call(ctx, renumberFamily.op(len(chunk)), vars, &data)
		stored := map[string]Issue{}
		for i, id := range chunk {
			if p := data[alias(i)]; err == nil && p != nil && p.Value != nil && p.Value.ID == id {
				stored[id] = p.Value.typed()
			}
		}
		return stored, err
	}), nil
}

// writeChunks writes ids maxBatch at a time, in order, through send,
// which returns the issues its answer carried as stored, by ID, and the
// request's error; holds reports whether the issue at index i holds
// what was written. Each part the answer does not prove applied is
// settled by one read of the chunk's issues; once a chunk has a part not
// applied, the chunks after it are not sent.
func (c *Client) writeChunks(ctx context.Context, ids []string, holds func(i int, is Issue) bool,
	send func(ctx context.Context, chunk []string, base int) (map[string]Issue, error)) []IssueWrite {
	out := make([]IssueWrite, 0, len(ids))
	for base := 0; base < len(ids); base += maxBatch {
		chunk := ids[base:min(base+maxBatch, len(ids))]
		stored, err := send(ctx, chunk, base)
		parts := make([]IssueWrite, len(chunk))
		settled := true
		for i, id := range chunk {
			parts[i].IssueID = id
			if is, ok := stored[id]; ok && holds(base+i, is) {
				parts[i].Issue = &is
				continue
			}
			settled = false
		}
		if !settled {
			c.settle(ctx, parts, base, err, holds)
		}
		out = append(out, parts...)
		if failed := firstFailed(parts); failed != "" {
			why := &trackerapi.NotSentError{Err: fmt.Errorf("linear: not sent: the write of issue %s before it was not applied", failed)}
			for _, id := range ids[base+len(chunk):] {
				out = append(out, IssueWrite{IssueID: id, Err: why})
			}
			break
		}
	}
	return out
}

// settle decides the parts the answer to a chunk's write did not prove
// applied. A write the transport proves never left (*NotSentError) or
// that the rate limit refused before Linear applied anything
// (*RateLimitedError) leaves each of them with that error. Any other
// failure, or a success whose answer did not show a part holding its
// value, is settled by one read of the chunk's issues by ID, archived
// included: a part the read shows holding its value is applied, one it
// shows otherwise or does not list was not applied, and every part is
// unknown when the read fails.
func (c *Client) settle(ctx context.Context, parts []IssueWrite, base int, writeErr error, holds func(int, Issue) bool) {
	var notSent *trackerapi.NotSentError
	var limited *trackerapi.RateLimitedError
	if errors.As(writeErr, &notSent) || errors.As(writeErr, &limited) {
		for i := range parts {
			if parts[i].Issue == nil {
				parts[i].Err = writeErr
			}
		}
		return
	}
	if writeErr == nil {
		writeErr = errors.New("linear: the answer did not show the issue holding the value written")
	}
	ids := make([]string, len(parts))
	for i, p := range parts {
		ids[i] = p.IssueID
	}
	read, readErr := c.ListIssues(ctx, IssueQuery{Filter: IssueFilter{IDs: ids}, IncludeArchived: true, Limit: len(ids)}, "")
	if readErr == nil && read.HasNextPage && len(read.Items) < len(ids) {
		readErr = fmt.Errorf("linear %s: %d IDs answered with more than one page", opIssues.name, len(ids))
	}
	found := map[string]Issue{}
	for _, is := range read.Items {
		found[is.ID] = is
	}
	for i := range parts {
		p := &parts[i]
		if p.Issue != nil {
			continue
		}
		is, ok := found[p.IssueID]
		switch {
		case readErr != nil:
			p.Err = &OutcomeUnknownError{IssueID: p.IssueID, Write: writeErr, Read: readErr}
		case ok && holds(base+i, is):
			p.Issue = &is
		default:
			p.Err = &UnchangedError{IssueID: p.IssueID, Listed: ok, Write: writeErr}
		}
	}
}

// firstFailed is the issue of the first part not applied, empty when
// every part was.
func firstFailed(parts []IssueWrite) string {
	for _, p := range parts {
		if p.Err != nil {
			return p.IssueID
		}
	}
	return ""
}

// checkIssueIDs refuses anything but distinct issue IDs.
func checkIssueIDs(ids []string) error {
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		switch {
		case !IsIssueID(id):
			return fmt.Errorf("%q is not an issue ID (a lowercase UUID)", id)
		case seen[id]:
			return fmt.Errorf("%s is named twice", id)
		}
		seen[id] = true
	}
	return nil
}
