package tempo

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// The checks that need a read of the site. The checks from the op alone
// are in rules.go.

// attributeRules checks p's attribute values against the site's
// attributes: known keys, values their types allow, required attributes
// present. A value valueRules flagged is not judged again. Account values
// are checked by checkAccounts.
func attributeRules(p *prepared, attrs []Attribute) {
	values := p.draft.Attributes
	for i, v := range values {
		if !judged(values, i) {
			continue
		}
		at := slices.IndexFunc(attrs, func(a Attribute) bool { return a.Key == v.Key })
		if at < 0 {
			p.problem(FieldAttribute, v.Key, UnknownAttribute, "no work attribute %s", v.Key)
			continue
		}
		a := attrs[at]
		switch a.Type {
		case AttributeList:
			if !slices.ContainsFunc(a.Options, func(o Option) bool { return o.Value == v.Value }) {
				p.problem(FieldAttribute, v.Key, NotAllowed, "%q is not an option of %s; options: %s", v.Value, a.Name, a.optionsText)
			}
		case AttributeCheckbox:
			if v.Value != "true" && v.Value != "false" {
				p.problem(FieldAttribute, v.Key, NotAllowed, "%s takes true or false, not %q", a.Name, v.Value)
			}
		case AttributeNumber:
			if n, err := strconv.ParseFloat(v.Value, 64); err != nil || math.IsInf(n, 0) || math.IsNaN(n) {
				p.problem(FieldAttribute, v.Key, NotAllowed, "%s takes a number, not %q", a.Name, v.Value)
			}
		}
	}
	for _, a := range attrs {
		if a.Required && !slices.ContainsFunc(values, func(v AttributeValue) bool { return v.Key == a.Key }) {
			p.problem(FieldAttribute, a.Key, Missing, "work attribute %s is required", a.Name)
		}
	}
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

// accountValue is one account attribute value in a checked op.
type accountValue struct {
	op  int
	key string // the attribute's key
	ref string // the account key it names
}

// checkAccounts checks the account attribute values of the writes with
// one account search, reading the settings only when a closed or archived
// account needs them. attrs and attrsErr are the attributes read: when
// Tempo answered it with 403, account values cannot be told apart, so
// every write with attribute values lists the account check as not run;
// when it failed otherwise, those writes already carry the error.
func (c *Client) checkAccounts(ctx context.Context, out []prepared, writes []int, attrs []Attribute, attrsErr error) {
	var values []accountValue
	var refs []string
	for _, i := range writes {
		list := out[i].draft.Attributes
		if attrsErr != nil {
			if len(list) > 0 && forbidden(attrsErr) {
				out[i].skip(CheckAccounts)
			}
			continue
		}
		for n, v := range list {
			at := slices.IndexFunc(attrs, func(a Attribute) bool { return a.Key == v.Key })
			if !judged(list, n) || at < 0 || attrs[at].Type != AttributeAccount {
				continue
			}
			values = append(values, accountValue{op: i, key: v.Key, ref: v.Value})
			if !slices.Contains(refs, v.Value) {
				refs = append(refs, v.Value)
			}
		}
	}
	if len(refs) == 0 {
		return
	}
	accounts, err := c.accounts(ctx, AccountQuery{Keys: refs})
	if err != nil {
		for _, v := range values {
			if forbidden(err) {
				out[v.op].skip(CheckAccounts)
			} else {
				out[v.op].fail(fmt.Errorf("read accounts: %w", err))
			}
		}
		return
	}
	var settings *Settings // read on the first closed or archived account
	var settingsErr error
	for _, v := range values {
		p := &out[v.op]
		at := slices.IndexFunc(accounts, func(a Account) bool { return a.Key == v.ref })
		switch {
		case at < 0:
			p.problem(FieldAttribute, v.key, NotFound, "no account %s", v.ref)
			continue
		case accounts[at].Status != AccountClosed && accounts[at].Status != AccountArchived:
			// Open, or a status the API reference does not list: Tempo
			// judges it.
			continue
		}
		if settings == nil && settingsErr == nil {
			s, err := c.settings(ctx)
			settings, settingsErr = &s, err
		}
		switch {
		case forbidden(settingsErr):
			p.skip(CheckSettings)
		case settingsErr != nil:
			p.fail(fmt.Errorf("read settings: %w", settingsErr))
		case !settings.InactiveAccountsAllowed:
			p.problem(FieldAttribute, v.key, NotAllowed, "account %s is %s", v.ref, inactiveName(accounts[at].Status))
		}
	}
}

func inactiveName(s AccountStatus) string {
	if s == AccountArchived {
		return "archived"
	}
	return "closed"
}

// authorDay is one author's timesheet on one day.
type authorDay struct {
	author string
	day    Date
}

// checkedDay is a date an op is judged on; current marks an update's
// current start date rather than the one it sets.
type checkedDay struct {
	day     Date
	current bool
}

// approvalDays are the dates the approval check judges an op on: its
// start date, and for an update a different current one.
func approvalDays(op Op, d Draft) []checkedDay {
	if d.Author == "" {
		return nil
	}
	var out []checkedDay
	if !d.Start.Date.IsZero() {
		out = append(out, checkedDay{day: d.Start.Date})
	}
	if from := op.change.From.Start.Date; op.kind == opUpdate && !from.IsZero() && from != d.Start.Date {
		out = append(out, checkedDay{day: from, current: true})
	}
	return out
}

// checkApprovals adds a problem to each write whose start date, or for an
// update whose current start date, falls in a timesheet that is approved
// or waiting for approval. It first reads one approval per author and
// period, for each date no period read so far holds, then judges every
// date against all the periods read. A date only fails when no period
// read holds it and the read for that date itself failed: with 403 the
// op lists the check as not run, otherwise the failure is the op's error.
func (c *Client) checkApprovals(ctx context.Context, ops []Op, out []prepared, writes []int) {
	known := map[string][]Approval{}
	failed := map[authorDay]error{}
	period := func(author string, day Date) (Approval, bool) {
		for _, a := range known[author] {
			if !day.Before(a.From) && !day.After(a.To) {
				return a, true
			}
		}
		return Approval{}, false
	}
	for _, i := range writes {
		author := out[i].draft.Author
		for _, d := range approvalDays(ops[i], out[i].draft) {
			k := authorDay{author, d.day}
			if _, ok := period(author, d.day); ok || failed[k] != nil {
				continue
			}
			a, err := c.approval(ctx, author, d.day)
			if err != nil {
				failed[k] = err
				continue
			}
			known[author] = append(known[author], a)
		}
	}
	for _, i := range writes {
		p := &out[i]
		for _, d := range approvalDays(ops[i], p.draft) {
			a, ok := period(p.draft.Author, d.day)
			err := failed[authorDay{p.draft.Author, d.day}]
			switch {
			case ok && a.locked():
				p.problems = append(p.problems, approvalProblem(a, d.day, d.current))
			case ok:
			case forbidden(err):
				p.skip(CheckApprovals)
			default:
				p.fail(fmt.Errorf("read the timesheet approval of %s on %s: %w", p.draft.Author, d.day, err))
			}
		}
	}
}

// approvalProblem is a date in the locked timesheet a; current marks an
// update's current date rather than the one it sets.
func approvalProblem(a Approval, day Date, current bool) Problem {
	what := "the start date " + day.String()
	if current {
		what = "the worklog's current start date " + day.String()
	}
	if a.Status == ApprovalInReview {
		return Problem{Field: FieldStartDate, Code: PeriodSubmitted, Message: fmt.Sprintf("%s is in the timesheet from %s to %s, which is waiting for approval", what, a.From, a.To)}
	}
	return Problem{Field: FieldStartDate, Code: PeriodApproved, Message: fmt.Sprintf("%s is in the timesheet from %s to %s, which is approved", what, a.From, a.To)}
}
