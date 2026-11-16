package tempo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	"agent-overflow/internal/tracker/trackerapi"
)

// Draft is a worklog to create, whole.
type Draft struct {
	// Issue names the issue by ID, or by Key alone, which the client
	// resolves through IssueIDs.
	Issue Issue
	// Author is the Atlassian account ID the worklog is logged for.
	Author string
	Start  Start
	// Spent is the time worked, sent as whole seconds: more than zero and
	// at most a day.
	Spent time.Duration
	// Billable is the billable part of Spent; nil leaves it to Tempo,
	// which bills the whole time spent.
	Billable *time.Duration
	// RemainingEstimate sets the issue's remaining estimate; nil sends
	// none.
	RemainingEstimate *time.Duration
	Description       string
	// Attributes are the worklog's work attribute values, one per key,
	// none empty.
	Attributes []AttributeValue
}

// Change is an update of one worklog. Tempo replaces the whole record on
// an update, so a change starts from the record as read: every field
// the change leaves nil is sent as From has it.
type Change struct {
	// From is the worklog as read; its ID names the worklog to change.
	From Worklog
	// Issue moves the worklog to another issue, by ID or by Key alone.
	// The v4 API reference lists no issue on an update; Tempo's v4
	// migration notes say an update takes issueId. The returned record
	// shows which issue Tempo kept.
	Issue *Issue
	Start *Start
	Spent *time.Duration
	// Billable sets the billable time. Left nil, a fully billable
	// worklog stays fully billable at a new Spent, and any other keeps
	// its billable time, cut to Spent.
	Billable          *time.Duration
	RemainingEstimate *time.Duration
	Description       *string
	// SetAttributes sets these values, replacing a value From has for the
	// same key; ClearAttributes removes these keys. A key in both is a
	// problem.
	SetAttributes   []AttributeValue
	ClearAttributes []string
}

// draft is the whole record the change sends.
func (ch Change) draft() Draft {
	from := ch.From
	d := Draft{
		Issue:       from.Issue,
		Author:      from.Author,
		Start:       from.Start,
		Spent:       from.Spent,
		Description: from.Description,
	}
	if ch.Issue != nil {
		d.Issue = *ch.Issue
	}
	if ch.Start != nil {
		d.Start = *ch.Start
	}
	if ch.Spent != nil {
		d.Spent = *ch.Spent
	}
	if ch.Description != nil {
		d.Description = *ch.Description
	}
	d.RemainingEstimate = ch.RemainingEstimate
	switch {
	case ch.Billable != nil:
		d.Billable = ch.Billable
	case from.Billable != nil && *from.Billable == from.Spent:
		spent := d.Spent
		d.Billable = &spent
	case from.Billable != nil:
		billable := min(*from.Billable, d.Spent)
		d.Billable = &billable
	}
	for _, a := range from.Attributes {
		if !slices.Contains(ch.ClearAttributes, a.Key) && !slices.ContainsFunc(ch.SetAttributes, func(s AttributeValue) bool { return s.Key == a.Key }) {
			d.Attributes = append(d.Attributes, a)
		}
	}
	d.Attributes = append(d.Attributes, ch.SetAttributes...)
	return d
}

type opKind uint8

const (
	opCreate opKind = iota + 1
	opUpdate
	opDelete
)

// Op is one write of a batch, built by CreateOp, UpdateOp or DeleteOp.
type Op struct {
	kind   opKind
	draft  Draft
	change Change
	id     int64
}

// CreateOp creates d.
func CreateOp(d Draft) Op { return Op{kind: opCreate, draft: d} }

// UpdateOp applies ch to the worklog ch.From names.
func UpdateOp(ch Change) Op { return Op{kind: opUpdate, change: ch} }

// DeleteOp deletes the worklog with Tempo ID id. It is not checked
// against approvals before sending, since it names no record; Tempo
// refuses a delete in an approved or closed period.
func DeleteOp(id int64) Op { return Op{kind: opDelete, id: id} }

// Status is what became of one op of a batch.
type Status uint8

const (
	// Applied: Tempo stored the write.
	Applied Status = iota + 1
	// Refused: the write was not applied. Problems say what was wrong
	// with it when that is known; Err says why.
	Refused
	// Unknown: the write was sent and its outcome is unknown (a timeout,
	// a 5xx, an answer cut short). It is not sent again; read the
	// worklogs before offering it again.
	Unknown
	// NotAttempted: the context ended before the write was sent.
	NotAttempted
)

func (s Status) String() string {
	switch s {
	case Applied:
		return "applied"
	case Refused:
		return "refused"
	case Unknown:
		return "unknown"
	case NotAttempted:
		return "not attempted"
	}
	return "Status(" + strconv.Itoa(int(s)) + ")"
}

// Outcome is the result of one op.
type Outcome struct {
	Status Status
	// Stored is the worklog as Tempo stored it, for an applied create or
	// update. Its Issue.Key is the key the op named or From carried.
	Stored Worklog
	// Problems are the reasons a refused write was refused: the checks
	// made before sending, or Tempo's own messages (Code TempoRefused).
	Problems []Problem
	// Err is nil for an applied op, and otherwise the error a single
	// write call would return.
	Err error
}

// Create creates one worklog and returns it as Tempo stored it. A draft
// the checks refuse is not sent: its error wraps a *trackerapi.NotSentError
// and an *InvalidError. An error that leaves the outcome unknown fails
// trackerapi.WriteNotApplied.
func (c *Client) Create(ctx context.Context, d Draft) (Worklog, error) {
	out := c.Apply(ctx, []Op{CreateOp(d)})[0]
	return out.Stored, out.Err
}

// Update applies ch and returns the worklog as Tempo stored it. Errors
// are as Create's.
func (c *Client) Update(ctx context.Context, ch Change) (Worklog, error) {
	out := c.Apply(ctx, []Op{UpdateOp(ch)})[0]
	return out.Stored, out.Err
}

// Delete deletes one worklog. A worklog that does not exist fails with
// an error matching trackerapi.ErrNotFound.
func (c *Client) Delete(ctx context.Context, id int64) error {
	return c.Apply(ctx, []Op{DeleteOp(id)})[0].Err
}

// Apply checks every create and update (one IssueIDs.IDs call, and the
// reads the checks need, for the whole batch), then sends the ops one at
// a time in order and reports each one's outcome, in order. An op the
// checks refuse, or that Tempo refuses, does not stop the others. An op
// whose outcome is unknown is not sent again. When ctx ends, the ops not
// yet sent are NotAttempted.
func (c *Client) Apply(ctx context.Context, ops []Op) []Outcome {
	out := make([]Outcome, len(ops))
	checked, checkErr := c.check(ctx, ops)
	for i, op := range ops {
		if err := ctx.Err(); err != nil {
			out[i] = notAttempted(err)
			continue
		}
		if op.kind != opDelete && checkErr != nil {
			out[i] = Outcome{Status: Refused, Err: &trackerapi.NotSentError{Err: fmt.Errorf("tempo: %s: %w", op.kind, checkErr)}}
			continue
		}
		var p *prepared
		if checked != nil {
			p = &checked[i]
		}
		out[i] = c.send(ctx, op, p)
	}
	return out
}

func notAttempted(err error) Outcome {
	return Outcome{Status: NotAttempted, Err: &trackerapi.NotSentError{Err: fmt.Errorf("tempo: not attempted: %w", err)}}
}

func (k opKind) String() string {
	switch k {
	case opCreate:
		return "create worklog"
	case opUpdate:
		return "update worklog"
	case opDelete:
		return "delete worklog"
	}
	return "invalid op"
}

// send makes one write; p is its checked form, nil for a delete when the
// checks could not run.
func (c *Client) send(ctx context.Context, op Op, p *prepared) Outcome {
	what := op.kind.String()
	problems := deleteProblems(op.id)
	if p != nil {
		problems = p.problems
	}
	if len(problems) > 0 {
		return refusedBefore(what, problems)
	}
	if op.kind == opDelete {
		what += " " + strconv.FormatInt(op.id, 10)
	}
	var r trackerapi.Request
	switch op.kind { // check refused any other kind
	case opCreate:
		r = trackerapi.Request{Method: http.MethodPost, Path: "4/worklogs", Body: p.body}
	case opUpdate:
		what += " " + strconv.FormatInt(op.change.From.ID, 10)
		r = trackerapi.Request{Method: http.MethodPut, Path: path("4/worklogs", strconv.FormatInt(op.change.From.ID, 10)), Body: p.body}
	case opDelete:
		r = trackerapi.Request{Method: http.MethodDelete, Path: path("4/worklogs", strconv.FormatInt(op.id, 10))}
	}
	var raw worklogJSON
	var into any = &raw
	if op.kind == opDelete {
		into = nil
	}
	if _, err := c.http.JSON(ctx, r, into); err != nil {
		return c.failed(ctx, what, err)
	}
	if op.kind == opDelete {
		return Outcome{Status: Applied}
	}
	stored, err := raw.worklog()
	if err != nil {
		// Tempo answered 2xx: the write happened, but its record does
		// not read.
		return Outcome{Status: Unknown, Err: fmt.Errorf("tempo: %s: read the stored worklog: %w", what, err)}
	}
	if stored.Issue.ID == p.body.IssueID {
		stored.Issue.Key = p.issueKey
	}
	return Outcome{Status: Applied, Stored: stored}
}

func refusedBefore(what string, problems []Problem) Outcome {
	invalid := &InvalidError{Problems: problems}
	return Outcome{Status: Refused, Problems: problems, Err: fmt.Errorf("tempo: %s: %w", what, &trackerapi.NotSentError{Err: invalid})}
}

// failed classes a write's error.
func (c *Client) failed(ctx context.Context, what string, err error) Outcome {
	wrapped := fmt.Errorf("tempo: %s: %w", what, err)
	if !trackerapi.WriteNotApplied(err) {
		return Outcome{Status: Unknown, Err: wrapped}
	}
	if _, notSent := errors.AsType[*trackerapi.NotSentError](err); notSent && ctx.Err() != nil {
		return Outcome{Status: NotAttempted, Err: wrapped}
	}
	out := Outcome{Status: Refused, Err: wrapped}
	if se, ok := errors.AsType[*trackerapi.StatusError](err); ok {
		if se.Status == http.StatusBadRequest {
			// The cached rules may be what is stale.
			c.site.drop()
		}
		out.Problems = tempoProblems(se)
	}
	return out
}

// tempoProblems are Tempo's own reasons for refusing a write.
func tempoProblems(se *trackerapi.StatusError) []Problem {
	var out []Problem
	if se.Message != "" {
		out = append(out, Problem{Code: TempoRefused, Message: se.Message})
	}
	keys := make([]string, 0, len(se.Fields))
	for k := range se.Fields {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		out = append(out, Problem{Field: Field(k), Code: TempoRefused, Message: se.Fields[k]})
	}
	return out
}

// worklogBody is a worklog as a create or update sends it. Attributes are
// always sent: an update replaces them, so an empty list clears them.
type worklogBody struct {
	Author      string               `json:"authorAccountId"`
	IssueID     int64                `json:"issueId"`
	StartDate   string               `json:"startDate"`
	StartTime   string               `json:"startTime,omitempty"`
	Spent       int64                `json:"timeSpentSeconds"`
	Billable    *int64               `json:"billableSeconds,omitempty"`
	Remaining   *int64               `json:"remainingEstimateSeconds,omitempty"`
	Description string               `json:"description"`
	Attributes  []attributeValueJSON `json:"attributes"`
}

func bodyOf(d Draft, issueID int64) worklogBody {
	b := worklogBody{
		Author:      d.Author,
		IssueID:     issueID,
		StartDate:   d.Start.Date.String(),
		StartTime:   d.Start.Time.String(),
		Spent:       seconds(d.Spent),
		Description: d.Description,
		Attributes:  make([]attributeValueJSON, 0, len(d.Attributes)),
	}
	if d.Billable != nil {
		s := seconds(*d.Billable)
		b.Billable = &s
	}
	if d.RemainingEstimate != nil {
		s := seconds(*d.RemainingEstimate)
		b.Remaining = &s
	}
	for _, a := range d.Attributes {
		b.Attributes = append(b.Attributes, attributeValueJSON(a))
	}
	return b
}
