package tempo

import (
	"context"
	"fmt"
	"slices"
)

// accountValue is one account attribute value in a checked op.
type accountValue struct {
	op  int
	key string // the attribute's key
	ref string // the account key it names
}

// checkAccounts checks every account attribute value of out with one
// account search, reading the settings only when a closed or archived
// account needs them. Without the attributes (attrsKnown false) account
// values cannot be told apart, so every op with attribute values lists
// the account check as not run.
func (c *Client) checkAccounts(ctx context.Context, out []prepared, attrs []Attribute, attrsKnown bool) error {
	var values []accountValue
	var refs []string
	for i := range out {
		for _, v := range out[i].draft.Attributes {
			if !attrsKnown {
				out[i].skip(CheckAccounts)
				break
			}
			at := slices.IndexFunc(attrs, func(a Attribute) bool { return a.Key == v.Key })
			if at < 0 || attrs[at].Type != AttributeAccount || v.Value == "" {
				continue
			}
			values = append(values, accountValue{op: i, key: v.Key, ref: v.Value})
			if !slices.Contains(refs, v.Value) {
				refs = append(refs, v.Value)
			}
		}
	}
	if len(refs) == 0 {
		return nil
	}
	accounts, err := c.accounts(ctx, AccountQuery{Keys: refs})
	if forbidden(err) {
		for _, v := range values {
			out[v.op].skip(CheckAccounts)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read accounts: %w", err)
	}
	var inactiveAllowed *bool // nil until read; settings not readable leaves it nil
	settingsForbidden := false
	for _, v := range values {
		at := slices.IndexFunc(accounts, func(a Account) bool { return a.Key == v.ref })
		problem := Problem{Field: FieldAttribute, Attribute: v.key, Code: NotAllowed}
		switch {
		case at < 0:
			problem.Code, problem.Message = NotFound, fmt.Sprintf("no account %s", v.ref)
		case accounts[at].Status == AccountOpen:
			continue
		case accounts[at].Status == AccountClosed || accounts[at].Status == AccountArchived:
			if inactiveAllowed == nil && !settingsForbidden {
				s, err := c.settings(ctx)
				switch {
				case forbidden(err):
					settingsForbidden = true
				case err != nil:
					return err
				default:
					inactiveAllowed = &s.InactiveAccountsAllowed
				}
			}
			if settingsForbidden {
				out[v.op].skip(CheckSettings)
				continue
			}
			if *inactiveAllowed {
				continue
			}
			problem.Message = fmt.Sprintf("account %s is %s", v.ref, inactiveName(accounts[at].Status))
		default:
			// A status the API reference does not list: Tempo judges it.
			continue
		}
		out[v.op].problems = append(out[v.op].problems, problem)
	}
	return nil
}

func inactiveName(s AccountStatus) string {
	if s == AccountArchived {
		return "archived"
	}
	return "closed"
}

// checkApprovals adds a problem to each create or update whose start date,
// or for an update whose current start date, falls in a timesheet that
// is approved or waiting for approval. It reads one approval per author
// and period; an author whose approvals Tempo answers with 403 has the
// check skipped.
func (c *Client) checkApprovals(ctx context.Context, ops []Op, out []prepared) error {
	known := map[string][]Approval{}
	refused := map[string]bool{}
	// lookup is the approval holding day, or ok false when the token may
	// not read the author's approvals.
	lookup := func(author string, day Date) (a Approval, ok bool, err error) {
		if refused[author] {
			return Approval{}, false, nil
		}
		for _, a := range known[author] {
			if !day.Before(a.From) && !day.After(a.To) {
				return a, true, nil
			}
		}
		a, err = c.approval(ctx, author, day)
		if forbidden(err) {
			refused[author] = true
			return Approval{}, false, nil
		}
		if err != nil {
			return Approval{}, false, fmt.Errorf("read the timesheet approval of %s on %s: %w", author, day, err)
		}
		known[author] = append(known[author], a)
		return a, true, nil
	}
	for i, op := range ops {
		if op.kind != opCreate && op.kind != opUpdate {
			continue
		}
		d := out[i].draft
		days := []Date{d.Start.Date}
		if op.kind == opUpdate && op.change.From.Start.Date != d.Start.Date {
			days = append(days, op.change.From.Start.Date)
		}
		for n, day := range days {
			if d.Author == "" || day.IsZero() {
				continue
			}
			a, ok, err := lookup(d.Author, day)
			if err != nil {
				return err
			}
			if !ok {
				out[i].skip(CheckApprovals)
				continue
			}
			if !a.locked() {
				continue
			}
			out[i].problems = append(out[i].problems, approvalProblem(a, day, n > 0))
		}
	}
	return nil
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
