package tempo

import "slices"

// The checks decidable from an op alone. They need no read, so they run
// for every op whatever the site reads do; the site checks are in
// validate_remote.go.

// opRules runs the checks from op alone into p and fills p's draft. It
// reports whether op is a create or update, which the site checks judge
// too.
func opRules(op Op, p *prepared) bool {
	switch op.kind {
	case opCreate:
		p.draft = op.draft
	case opUpdate:
		p.draft = op.change.draft()
		changeRules(p, op.change)
	case opDelete:
		if op.id <= 0 {
			p.problem(FieldWorklog, "", Missing, "%d is not a worklog ID", op.id)
		}
		return false
	default:
		p.problem(FieldWorklog, "", Missing, "the op was not built by CreateOp, UpdateOp or DeleteOp")
		return false
	}
	is := p.draft.Issue
	p.issueKey = is.Key
	switch {
	case is.ID > 0:
		p.issueID = is.ID
	case is.Key == "":
		p.problem(FieldIssue, "", Missing, "no issue")
	}
	draftRules(p, p.draft)
	valueRules(p, p.draft.Attributes)
	return true
}

func changeRules(p *prepared, ch Change) {
	if ch.From.ID <= 0 {
		p.problem(FieldWorklog, "", Missing, "the change names no worklog: From has no ID")
	}
	for _, s := range ch.SetAttributes {
		if slices.Contains(ch.ClearAttributes, s.Key) {
			p.problem(FieldAttribute, s.Key, Conflicting, "work attribute %s is both set and cleared", s.Key)
		}
	}
}

func draftRules(p *prepared, d Draft) {
	if d.Author == "" {
		p.problem(FieldAuthor, "", Missing, "no author")
	}
	if d.Start.Date.IsZero() {
		p.problem(FieldStartDate, "", Missing, "no start date")
	}
	switch spent := seconds(d.Spent); {
	case spent <= 0:
		p.problem(FieldSpent, "", NotPositive, "time spent %v is not more than zero seconds", d.Spent)
	case spent > 24*60*60:
		p.problem(FieldSpent, "", OverADay, "time spent %v is more than a day", d.Spent)
	}
	if d.Billable != nil && seconds(*d.Billable) < 0 {
		p.problem(FieldBillable, "", Negative, "billable time %v is negative", *d.Billable)
	}
	if d.RemainingEstimate != nil && seconds(*d.RemainingEstimate) < 0 {
		p.problem(FieldRemaining, "", Negative, "remaining estimate %v is negative", *d.RemainingEstimate)
	}
}

// valueRules checks the shape of attribute values: a key, given once, a
// value.
func valueRules(p *prepared, values []AttributeValue) {
	for i, v := range values {
		switch {
		case v.Key == "":
			p.problem(FieldAttribute, "", Missing, "an attribute value names no work attribute")
		case slices.ContainsFunc(values[:i], func(e AttributeValue) bool { return e.Key == v.Key }):
			p.problem(FieldAttribute, v.Key, Duplicate, "work attribute %s is given twice", v.Key)
		case v.Value == "":
			p.problem(FieldAttribute, v.Key, Missing, "work attribute %s has an empty value; leave it out to leave it unset", v.Key)
		}
	}
}

// judged reports whether the site checks judge values[i]: its shape
// passed valueRules and it is the first value with its key.
func judged(values []AttributeValue, i int) bool {
	v := values[i]
	return v.Key != "" && v.Value != "" &&
		!slices.ContainsFunc(values[:i], func(e AttributeValue) bool { return e.Key == v.Key })
}
