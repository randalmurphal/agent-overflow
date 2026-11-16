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
// account search, reading the settings only when a closed account needs
// them.
func (c *Client) checkAccounts(ctx context.Context, out []prepared, attrs []Attribute) error {
	var values []accountValue
	var refs []string
	for i := range out {
		for _, v := range out[i].draft.Attributes {
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
	if err != nil {
		return fmt.Errorf("read accounts: %w", err)
	}
	var closedAllowed *bool
	for _, v := range values {
		at := slices.IndexFunc(accounts, func(a Account) bool { return a.Key == v.ref })
		problem := Problem{Field: FieldAttribute, Attribute: v.key, Code: NotAllowed}
		switch {
		case at < 0:
			problem.Code, problem.Message = NotFound, fmt.Sprintf("no account %s", v.ref)
		case accounts[at].Status == AccountOpen:
			continue
		case accounts[at].Status == AccountClosed:
			if closedAllowed == nil {
				s, err := c.settings(ctx)
				if err != nil {
					return err
				}
				closedAllowed = &s.ClosedAccountsAllowed
			}
			if *closedAllowed {
				continue
			}
			problem.Message = fmt.Sprintf("account %s is closed", v.ref)
		default:
			problem.Message = fmt.Sprintf("account %s is %s, not open", v.ref, accounts[at].Status)
		}
		out[v.op].problems = append(out[v.op].problems, problem)
	}
	return nil
}

// checkApprovals adds a problem to each create or update whose start date,
// or for an update whose current start date, falls in a timesheet that
// is approved or waiting for approval. It reads one approval per author
// and period.
func (c *Client) checkApprovals(ctx context.Context, ops []Op, out []prepared) error {
	known := map[string][]Approval{}
	lookup := func(author string, day Date) (Approval, error) {
		for _, a := range known[author] {
			if !day.Before(a.From) && !day.After(a.To) {
				return a, nil
			}
		}
		a, err := c.approval(ctx, author, day)
		if err != nil {
			return Approval{}, fmt.Errorf("read the timesheet approval of %s on %s: %w", author, day, err)
		}
		known[author] = append(known[author], a)
		return a, nil
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
			a, err := lookup(d.Author, day)
			if err != nil {
				return err
			}
			if !a.locked() {
				continue
			}
			what := "the start date " + day.String()
			if n > 0 {
				what = "the worklog's current start date " + day.String()
			}
			problem := Problem{Field: FieldStartDate, Code: PeriodApproved, Message: fmt.Sprintf("%s is in the timesheet from %s to %s, which is approved", what, a.From, a.To)}
			if a.Status == ApprovalInReview {
				problem.Code = PeriodSubmitted
				problem.Message = fmt.Sprintf("%s is in the timesheet from %s to %s, which is waiting for approval", what, a.From, a.To)
			}
			out[i].problems = append(out[i].problems, problem)
		}
	}
	return nil
}
