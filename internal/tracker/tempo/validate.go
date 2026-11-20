package tempo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"agent-overflow/internal/tracker/trackerapi"
)

// Field names the part of a worklog a Problem is about. A problem from
// Tempo's own answer names no field, since Tempo gives none.
type Field string

// The fields the checks name.
const (
	FieldWorklog   Field = "worklog" // the worklog a change or delete names
	FieldIssue     Field = "issue"
	FieldAuthor    Field = "author"
	FieldStartDate Field = "startDate"
	FieldSpent     Field = "spent"
	FieldBillable  Field = "billable"
	FieldRemaining Field = "remainingEstimate"
	FieldAttribute Field = "attribute" // Problem.Attribute names which
)

// Code is the kind of a Problem.
type Code string

// The problem codes.
const (
	// Missing: a required value is absent or empty.
	Missing Code = "missing"
	// NotFound: the issue key or account key names nothing Tempo or the
	// IssueIDs know.
	NotFound Code = "not_found"
	// NotAllowed: a value the attribute does not allow.
	NotAllowed Code = "not_allowed"
	// UnknownAttribute: no work attribute has this key.
	UnknownAttribute Code = "unknown_attribute"
	// Duplicate: the same attribute is given twice.
	Duplicate Code = "duplicate"
	// Conflicting: a change both sets and clears an attribute.
	Conflicting Code = "conflicting"
	// NotPositive: the time spent is not more than zero whole seconds.
	NotPositive Code = "not_positive"
	// Negative: a billable time or remaining estimate below zero.
	Negative Code = "negative"
	// OverADay: the time spent is more than 24 hours.
	OverADay Code = "over_a_day"
	// PeriodApproved: the date is in an approved timesheet.
	PeriodApproved Code = "period_approved"
	// PeriodSubmitted: the date is in a timesheet waiting for approval.
	PeriodSubmitted Code = "period_submitted"
	// TempoRefused: Tempo refused the write; Message is Tempo's.
	TempoRefused Code = "refused"
	// IssueKept: Tempo applied an update but kept the worklog on its old
	// issue instead of the one the change named.
	IssueKept Code = "issue_kept"
	// Changed: the worklog an update names no longer holds what its From
	// has; read it again and build the change from that.
	Changed Code = "changed"
)

// Check names a check that needs a read from Tempo. When the token may
// not make that read (Tempo answers 403), the check is skipped for the
// ops it would judge, each of them lists it as unchecked, and Tempo alone
// judges the write.
type Check string

// The checks that read from Tempo.
const (
	// CheckAttributes: known attributes, allowed values, required ones.
	// Without it the account check cannot tell account values either.
	CheckAttributes Check = "attributes"
	// CheckAccounts: an account value names an existing usable account.
	CheckAccounts Check = "accounts"
	// CheckSettings: whether a closed or archived account is allowed.
	CheckSettings Check = "settings"
	// CheckApprovals: the date is not in an approved or submitted
	// timesheet.
	CheckApprovals Check = "approvals"
)

// Report is what Validate found for one op.
type Report struct {
	Problems []Problem
	// Unchecked are the checks the token may not make the reads for.
	Unchecked []Check
	// Err is a read this op's checks need that failed other than with 403
	// (IssueIDs, or a read from Tempo). Problems holds what the checks
	// that ran found; Apply does not send the op. Ops the failed read
	// does not judge are checked in full.
	Err error
}

// Problem is one reason a worklog write would be, or was, refused.
type Problem struct {
	Field Field
	// Attribute is the work attribute's key when Field is FieldAttribute.
	Attribute string
	Code      Code
	Message   string
}

// InvalidError is a write the checks refused before sending it.
type InvalidError struct{ Problems []Problem }

func (e *InvalidError) Error() string {
	parts := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		parts[i] = p.Message
	}
	return "the worklog would be refused: " + strings.Join(parts, "; ")
}

// Validate reports, for each op in order, every reason Tempo would refuse
// it that is known without sending it. From the op alone:
//   - a delete or change that names no worklog; an op not built by
//     CreateOp, UpdateOp or DeleteOp; a change that sets and clears one
//     attribute;
//   - no issue ID or key, no author, no start date;
//   - time spent not more than zero, or more than a day, in whole
//     seconds; a negative billable time or remaining estimate;
//   - an attribute value with no key, an attribute given twice, an empty
//     value.
//
// From reads of the site:
//   - an issue key IssueIDs does not know;
//   - an unknown attribute, a value its type does not allow (a static
//     list's options, true or false for a checkbox, a number, an existing
//     account that is open, or closed or archived where Settings allow
//     it), a required attribute missing;
//   - a start date, or for a change the current one, in a timesheet that
//     is approved or waiting for approval.
//
// Tempo's API does not expose closed periods, so a date in one is found
// only when Tempo refuses the write. The checks from the op alone always
// run. A site check whose read Tempo answers with 403 is skipped and
// listed in Report.Unchecked; a read that fails otherwise is the
// Report.Err of each op it would judge. The error is only that of a call
// on a closed Client or one New did not build. Apply runs the same checks
// before it writes.
func (c *Client) Validate(ctx context.Context, ops []Op) ([]Report, error) {
	if err := c.admit(); err != nil {
		return nil, err
	}
	checked := c.check(ctx, ops)
	out := make([]Report, len(ops))
	for i, p := range checked {
		out[i] = Report{Problems: p.problems, Unchecked: p.unchecked, Err: p.err}
	}
	return out, nil
}

// prepared is one op after the checks: for a create or update, the record
// it sends and the issue it names.
type prepared struct {
	draft     Draft
	issueID   int64 // 0 until known
	issueKey  string
	body      worklogBody
	problems  []Problem
	unchecked []Check
	// err joins the reads p's checks needed that failed other than with
	// 403; p is not sent while it is set.
	err error
}

// skip records that check was not run for p.
func (p *prepared) skip(check Check) {
	if !slices.Contains(p.unchecked, check) {
		p.unchecked = append(p.unchecked, check)
	}
}

// fail records a read p's checks needed that failed.
func (p *prepared) fail(err error) { p.err = errors.Join(p.err, err) }

// problem adds one problem to p.
func (p *prepared) problem(f Field, attribute string, code Code, format string, args ...any) {
	p.problems = append(p.problems, Problem{Field: f, Attribute: attribute, Code: code, Message: fmt.Sprintf(format, args...)})
}

// forbidden reports a check read Tempo answered with 403: the token may
// not make it.
func forbidden(err error) bool {
	se, ok := errors.AsType[*trackerapi.StatusError](err)
	return ok && se.Status == http.StatusForbidden
}

// check runs every check on ops. The checks from the op alone (opRules)
// run first and always. The site checks then make, for the whole batch,
// one IssueIDs.IDs call for the keys the ops name (under a context Close
// also ends), the cached attributes read, one account search, the
// settings read only when a closed or archived account needs it, and one
// approval read per author and period. A read Tempo answers with 403
// skips its check for the ops it would judge. A read that fails otherwise
// is the error of exactly the ops it would judge.
func (c *Client) check(ctx context.Context, ops []Op) []prepared {
	out := make([]prepared, len(ops))
	var writes []int
	for i, op := range ops {
		if opRules(op, &out[i]) {
			writes = append(writes, i)
		}
	}
	if len(writes) == 0 {
		return out
	}
	c.checkIssues(ctx, out, writes)
	attrs, attrsErr := c.attributes(ctx)
	for _, i := range writes {
		p := &out[i]
		switch {
		case attrsErr == nil:
			attributeRules(p, attrs)
		case forbidden(attrsErr):
			p.skip(CheckAttributes)
		default:
			p.fail(fmt.Errorf("read work attributes: %w", attrsErr))
		}
	}
	c.checkAccounts(ctx, out, writes, attrs, attrsErr)
	c.checkApprovals(ctx, ops, out, writes)
	for _, i := range writes {
		if p := &out[i]; len(p.problems) == 0 && p.err == nil {
			p.body = bodyOf(p.draft, p.issueID)
		}
	}
	return out
}

// checkIssues resolves, with one IssueIDs.IDs call, the keys of the
// writes that name no issue ID. A key IssueIDs does not know is a
// problem; a failed call is the error of the writes that name a key.
func (c *Client) checkIssues(ctx context.Context, out []prepared, writes []int) {
	var keys []string
	for _, i := range writes {
		if is := out[i].draft.Issue; is.ID <= 0 && is.Key != "" && !slices.Contains(keys, is.Key) {
			keys = append(keys, is.Key)
		}
	}
	if len(keys) == 0 {
		return
	}
	ids, err := c.issueIDs(ctx, keys)
	for _, i := range writes {
		p := &out[i]
		is := p.draft.Issue
		switch {
		case is.ID > 0 || is.Key == "":
		case err != nil:
			p.fail(err)
		case ids[is.Key] <= 0:
			p.problem(FieldIssue, "", NotFound, "no issue %s", is.Key)
		default:
			p.issueID = ids[is.Key]
		}
	}
}

// issueIDs resolves keys with one IssueIDs.IDs call under a context
// Close also ends.
func (c *Client) issueIDs(ctx context.Context, keys []string) (map[string]int64, error) {
	ctx, done := c.bound(ctx)
	defer done()
	ids, err := c.issues.IDs(ctx, keys)
	if err != nil {
		return nil, fmt.Errorf("resolve issue IDs: %w", err)
	}
	return ids, nil
}
