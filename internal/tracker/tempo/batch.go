package tempo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"

	"agent-overflow/internal/tracker/trackerapi"
)

type opKind uint8

const (
	opCreate opKind = iota + 1
	opUpdate
	opDelete
)

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
	// a 3xx, a 5xx, an answer cut short). It is not sent again; read the
	// worklogs before offering it again.
	Unknown
	// NotAttempted: the context ended before the write was sent.
	NotAttempted
	// NotMoved: Tempo applied an update that named another issue, but
	// the stored worklog is still on its old issue. Stored is the record
	// as stored and Err a *NotMovedError. Not safe to send again as is.
	NotMoved
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
	case NotMoved:
		return "not moved"
	}
	return "Status(" + strconv.Itoa(int(s)) + ")"
}

// Outcome is the result of one op.
type Outcome struct {
	Status Status
	// Stored is the worklog as Tempo stored it, for an applied or
	// NotMoved update and an applied create. Its Issue.Key is the key
	// the op named or From carried for that issue.
	Stored Worklog
	// Problems are the reasons a write was refused or not moved: the
	// checks made before sending, or Tempo's own messages (Code
	// TempoRefused).
	Problems []Problem
	// Unchecked are the checks not run for this op because the token may
	// not make their read (Tempo answered 403); the write was sent
	// without them and Tempo judged it.
	Unchecked []Check
	// Err is nil for an applied op, and otherwise the error a single
	// write call would return.
	Err error
}

// NotMovedError is an update Tempo applied while keeping the worklog on
// an issue other than the one the change named. It is not a refusal:
// trackerapi.WriteNotApplied is false for it.
type NotMovedError struct {
	Stored Worklog // the record as Tempo stored it
	Asked  int64   // the issue ID the change named
}

func (e *NotMovedError) Error() string {
	return fmt.Sprintf("tempo: update worklog %d: updated, but Tempo kept it on issue %d instead of moving it to issue %d",
		e.Stored.ID, e.Stored.Issue.ID, e.Asked)
}

// Create creates one worklog and returns it as Tempo stored it. A draft
// the checks refuse is not sent: its error wraps a *trackerapi.NotSentError
// and an *InvalidError. An error that leaves the outcome unknown fails
// trackerapi.WriteNotApplied. Apply also reports checks not run.
func (c *Client) Create(ctx context.Context, d Draft) (Worklog, error) {
	out := c.Apply(ctx, []Op{CreateOp(d)})[0]
	return out.Stored, out.Err
}

// Update applies ch and returns the worklog as Tempo stored it. Errors
// are as Create's, and an update that did not move the worklog returns
// the stored record with a *NotMovedError.
func (c *Client) Update(ctx context.Context, ch Change) (Worklog, error) {
	out := c.Apply(ctx, []Op{UpdateOp(ch)})[0]
	return out.Stored, out.Err
}

// Delete deletes one worklog. A worklog that does not exist fails with
// an error matching trackerapi.ErrNotFound.
func (c *Client) Delete(ctx context.Context, id int64) error {
	return c.Apply(ctx, []Op{DeleteOp(id)})[0].Err
}

// Apply checks every create and update as Validate does (one
// IssueIDs.IDs call, and the reads the checks need, for the whole batch),
// then sends the ops one at a time in order and reports each one's
// outcome, in order. An op the checks refuse, or that Tempo refuses, does
// not stop the others. An op whose outcome is unknown is not sent again.
// When ctx ends, the ops not yet sent are NotAttempted.
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
		if p != nil {
			out[i].Unchecked = p.unchecked
		}
	}
	return out
}

func notAttempted(err error) Outcome {
	return Outcome{Status: NotAttempted, Err: &trackerapi.NotSentError{Err: fmt.Errorf("tempo: not attempted: %w", err)}}
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
	var r trackerapi.Request
	switch op.kind { // check refused any other kind
	case opCreate:
		r = trackerapi.Request{Method: http.MethodPost, Path: "4/worklogs", Body: p.body}
	case opUpdate:
		what += " " + strconv.FormatInt(op.change.From.ID, 10)
		r = trackerapi.Request{Method: http.MethodPut, Path: path("4/worklogs", strconv.FormatInt(op.change.From.ID, 10)), Body: p.body}
	case opDelete:
		what += " " + strconv.FormatInt(op.id, 10)
		r = trackerapi.Request{Method: http.MethodDelete, Path: path("4/worklogs", strconv.FormatInt(op.id, 10))}
	}
	var raw worklogJSON
	var into any = &raw
	if op.kind == opDelete {
		into = nil
	}
	resp, err := c.http.JSON(ctx, r, into)
	if err != nil {
		return c.failed(ctx, what, err)
	}
	if op.kind == opDelete {
		return Outcome{Status: Applied}
	}
	stored, err := raw.worklog(resp)
	if err != nil {
		// Tempo answered 2xx: the write happened, but its record does
		// not read.
		return Outcome{Status: Unknown, Err: fmt.Errorf("tempo: %s: read the stored worklog: %w", what, err)}
	}
	return stored.outcome(op, p)
}

// outcome is the result of a write Tempo stored as stored: Applied, or
// NotMoved when an update named another issue and stored is not on it.
func (stored Worklog) outcome(op Op, p *prepared) Outcome {
	asked := p.body.IssueID
	switch {
	case stored.Issue.ID == asked:
		stored.Issue.Key = p.issueKey
	case op.kind == opUpdate && stored.Issue.ID == op.change.From.Issue.ID:
		stored.Issue.Key = op.change.From.Issue.Key
	}
	if op.kind != opUpdate || op.change.Issue == nil || stored.Issue.ID == asked {
		return Outcome{Status: Applied, Stored: stored}
	}
	err := &NotMovedError{Stored: stored, Asked: asked}
	return Outcome{
		Status: NotMoved, Stored: stored, Err: err,
		Problems: []Problem{{Field: FieldIssue, Code: IssueKept, Message: fmt.Sprintf("the worklog is still on issue %d, not on issue %d", stored.Issue.ID, asked)}},
	}
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

// tempoProblems are Tempo's own reasons for refusing a write, as the
// transport read and scrubbed them.
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
