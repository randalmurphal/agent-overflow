package tempo

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
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
)

// Check names a check that needs a read from Tempo. When the token may
// not make that read (Tempo answers 403), the check is skipped for the
// batch, each op it would have judged lists it as unchecked, and Tempo
// alone judges the write.
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
// it that is known without sending it:
//   - the issue: no ID or key, or a key IssueIDs does not know;
//   - no author, no start date;
//   - time spent not more than zero, or more than a day, in whole
//     seconds; a negative billable time or remaining estimate;
//   - an unknown attribute, an attribute given twice, an empty value, a
//     value its type does not allow (a static list's options, true or
//     false for a checkbox, a number, an existing account that is open,
//     or closed or archived where Settings allow it), a required attribute
//     missing;
//   - a start date, or for a change the current one, in a timesheet that
//     is approved or waiting for approval.
//
// Tempo's API does not expose closed periods, so a date in one is found
// only when Tempo refuses the write. A check whose read Tempo answers
// with 403 is skipped and listed in Report.Unchecked. The error is any
// other failure to read what the checks need. Apply runs the same checks
// before it writes.
func (c *Client) Validate(ctx context.Context, ops []Op) ([]Report, error) {
	checked, err := c.check(ctx, ops)
	if err != nil {
		return nil, fmt.Errorf("tempo: check worklogs: %w", err)
	}
	out := make([]Report, len(ops))
	for i := range checked {
		out[i] = Report{Problems: checked[i].problems, Unchecked: checked[i].unchecked}
	}
	return out, nil
}

// prepared is one op after the checks: for a create or update, the record
// it sends and the issue key it named.
type prepared struct {
	draft     Draft
	issueKey  string
	body      worklogBody
	problems  []Problem
	unchecked []Check
}

// skip records that check was not run for p.
func (p *prepared) skip(check Check) {
	if !slices.Contains(p.unchecked, check) {
		p.unchecked = append(p.unchecked, check)
	}
}

// forbidden reports a check read Tempo answered with 403: the token may
// not make it.
func forbidden(err error) bool {
	se, ok := errors.AsType[*trackerapi.StatusError](err)
	return ok && se.Status == http.StatusForbidden
}

// check runs every check on ops: one IssueIDs.IDs call for the keys they
// name (under a context Close also ends), the cached attributes, one
// account search, the settings only when a closed or archived account
// needs them, and one approval read per author and period. A read Tempo
// answers with 403 skips its check; any other failure is the error.
func (c *Client) check(ctx context.Context, ops []Op) ([]prepared, error) {
	out := make([]prepared, len(ops))
	var keys []string
	writes := false
	for i, op := range ops {
		switch op.kind {
		case opCreate:
			out[i].draft = op.draft
		case opUpdate:
			out[i].draft = op.change.draft()
		case opDelete:
			out[i].problems = deleteProblems(op.id)
			continue
		default:
			out[i].problems = []Problem{{Field: FieldWorklog, Code: Missing, Message: "the op was not built by CreateOp, UpdateOp or DeleteOp"}}
			continue
		}
		writes = true
		if is := out[i].draft.Issue; is.ID <= 0 && is.Key != "" && !slices.Contains(keys, is.Key) {
			keys = append(keys, is.Key)
		}
	}
	if !writes {
		return out, nil
	}
	ids, err := c.issueIDs(ctx, keys)
	if err != nil {
		return nil, err
	}
	attrs, err := c.attributes(ctx)
	attrsKnown := err == nil
	if err != nil && !forbidden(err) {
		return nil, err
	}
	issueIDs := make([]int64, len(ops))
	for i, op := range ops {
		if op.kind != opCreate && op.kind != opUpdate {
			continue
		}
		p := &out[i]
		issueIDs[i], p.issueKey = issueOf(p.draft.Issue, ids, &p.problems)
		if op.kind == opUpdate {
			p.problems = append(p.problems, changeProblems(op.change)...)
		}
		p.problems = append(p.problems, draftProblems(p.draft)...)
		if !attrsKnown {
			p.skip(CheckAttributes)
			continue
		}
		p.problems = append(p.problems, attributeProblems(p.draft.Attributes, attrs)...)
	}
	if err := c.checkAccounts(ctx, out, attrs, attrsKnown); err != nil {
		return nil, err
	}
	if err := c.checkApprovals(ctx, ops, out); err != nil {
		return nil, err
	}
	for i, op := range ops {
		if (op.kind == opCreate || op.kind == opUpdate) && len(out[i].problems) == 0 {
			out[i].body = bodyOf(out[i].draft, issueIDs[i])
		}
	}
	return out, nil
}

// issueIDs resolves keys with one IssueIDs.IDs call under a context
// Close also ends.
func (c *Client) issueIDs(ctx context.Context, keys []string) (map[string]int64, error) {
	if len(keys) == 0 {
		return map[string]int64{}, nil
	}
	ctx, done := c.bound(ctx)
	defer done()
	ids, err := c.issues.IDs(ctx, keys)
	if err != nil {
		return nil, fmt.Errorf("resolve issue IDs: %w", err)
	}
	return ids, nil
}

func deleteProblems(id int64) []Problem {
	if id > 0 {
		return nil
	}
	return []Problem{{Field: FieldWorklog, Code: Missing, Message: fmt.Sprintf("%d is not a worklog ID", id)}}
}

// issueOf is the issue ID and key a draft names, adding a problem when it
// names none or a key the resolver does not know.
func issueOf(is Issue, ids map[string]int64, problems *[]Problem) (int64, string) {
	switch {
	case is.ID > 0:
		return is.ID, is.Key
	case is.Key == "":
		*problems = append(*problems, Problem{Field: FieldIssue, Code: Missing, Message: "no issue"})
	case ids[is.Key] <= 0:
		*problems = append(*problems, Problem{Field: FieldIssue, Code: NotFound, Message: fmt.Sprintf("no issue %s", is.Key)})
	default:
		return ids[is.Key], is.Key
	}
	return 0, is.Key
}

func changeProblems(ch Change) []Problem {
	var out []Problem
	if ch.From.ID <= 0 {
		out = append(out, Problem{Field: FieldWorklog, Code: Missing, Message: "the change names no worklog: From has no ID"})
	}
	for _, s := range ch.SetAttributes {
		if slices.Contains(ch.ClearAttributes, s.Key) {
			out = append(out, Problem{Field: FieldAttribute, Attribute: s.Key, Code: Conflicting, Message: fmt.Sprintf("work attribute %s is both set and cleared", s.Key)})
		}
	}
	return out
}

func draftProblems(d Draft) []Problem {
	var out []Problem
	add := func(f Field, code Code, format string, args ...any) {
		out = append(out, Problem{Field: f, Code: code, Message: fmt.Sprintf(format, args...)})
	}
	if d.Author == "" {
		add(FieldAuthor, Missing, "no author")
	}
	if d.Start.Date.IsZero() {
		add(FieldStartDate, Missing, "no start date")
	}
	switch spent := seconds(d.Spent); {
	case spent <= 0:
		add(FieldSpent, NotPositive, "time spent %v is not more than zero seconds", d.Spent)
	case spent > 24*60*60:
		add(FieldSpent, OverADay, "time spent %v is more than a day", d.Spent)
	}
	if d.Billable != nil && seconds(*d.Billable) < 0 {
		add(FieldBillable, Negative, "billable time %v is negative", *d.Billable)
	}
	if d.RemainingEstimate != nil && seconds(*d.RemainingEstimate) < 0 {
		add(FieldRemaining, Negative, "remaining estimate %v is negative", *d.RemainingEstimate)
	}
	return out
}

// attributeProblems checks values against the site's attributes. Account
// values are checked by checkAccounts.
func attributeProblems(values []AttributeValue, attrs []Attribute) []Problem {
	var out []Problem
	add := func(key string, code Code, format string, args ...any) {
		out = append(out, Problem{Field: FieldAttribute, Attribute: key, Code: code, Message: fmt.Sprintf(format, args...)})
	}
	for i, v := range values {
		at := slices.IndexFunc(attrs, func(a Attribute) bool { return a.Key == v.Key })
		switch {
		case at < 0:
			add(v.Key, UnknownAttribute, "no work attribute %s", v.Key)
			continue
		case slices.ContainsFunc(values[:i], func(e AttributeValue) bool { return e.Key == v.Key }):
			add(v.Key, Duplicate, "work attribute %s is given twice", attrs[at].Name)
			continue
		case v.Value == "":
			add(v.Key, Missing, "work attribute %s has an empty value; leave it out to leave it unset", attrs[at].Name)
			continue
		}
		a := attrs[at]
		switch a.Type {
		case AttributeList:
			if !slices.ContainsFunc(a.Options, func(o Option) bool { return o.Value == v.Value }) {
				add(v.Key, NotAllowed, "%q is not an option of %s; options: %s", v.Value, a.Name, a.optionsText)
			}
		case AttributeCheckbox:
			if v.Value != "true" && v.Value != "false" {
				add(v.Key, NotAllowed, "%s takes true or false, not %q", a.Name, v.Value)
			}
		case AttributeNumber:
			if n, err := strconv.ParseFloat(v.Value, 64); err != nil || math.IsInf(n, 0) || math.IsNaN(n) {
				add(v.Key, NotAllowed, "%s takes a number, not %q", a.Name, v.Value)
			}
		}
	}
	for _, a := range attrs {
		if a.Required && !slices.ContainsFunc(values, func(v AttributeValue) bool { return v.Key == a.Key }) {
			add(a.Key, Missing, "work attribute %s is required", a.Name)
		}
	}
	return out
}

func optionNames(options []Option) string {
	names := make([]string, len(options))
	for i, o := range options {
		names[i] = o.Value
		if o.Name != "" && o.Name != o.Value {
			names[i] = o.Name + " (" + o.Value + ")"
		}
	}
	return strings.Join(names, ", ")
}
