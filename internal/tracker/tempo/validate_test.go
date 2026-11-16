package tempo

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

// siteFixture serves the attributes, and the approvals of the three
// weeks the checks read: approved, open and waiting for approval.
func siteFixture(f *fakeTempo) {
	f.on("GET", "/4/work-attributes", q("offset", "0", "limit", "1000"), "", 200, attributesJSON)
	f.on("GET", "/4/timesheet-approvals/user/acc-user1", q("from", "2026-11-03", "to", "2026-11-03"), "", 200, approvalJSONFor("2026-11-02", "2026-11-08", "APPROVED"))
	f.on("GET", "/4/timesheet-approvals/user/acc-user1", q("from", "2026-11-10", "to", "2026-11-10"), "", 200, approvalJSONFor("2026-11-09", "2026-11-15", "OPEN"))
	f.on("GET", "/4/timesheet-approvals/user/acc-user1", q("from", "2026-11-17", "to", "2026-11-17"), "", 200, approvalJSONFor("2026-11-16", "2026-11-22", "IN_REVIEW"))
}

type want struct {
	field Field
	attr  string
	code  Code
}

func kinds(ps []Problem) []want {
	var out []want
	for _, p := range ps {
		out = append(out, want{p.Field, p.Attribute, p.Code})
	}
	return out
}

func TestValidateRules(t *testing.T) {
	f := newFake(t)
	issues := newIssues()
	c := testClient(t, f, issues)
	siteFixture(f)
	f.on("POST", "/4/accounts/search", q("offset", "0", "limit", "1000"), `{"keys":["ACME","OLD","GONE","NOPE"]}`, 200, pageOf(0, false,
		`{"id":1,"key":"ACME","name":"Acme","status":"OPEN","global":false}`,
		`{"id":2,"key":"OLD","name":"Old","status":"CLOSED","global":false}`,
		`{"id":3,"key":"GONE","name":"Gone","status":"ARCHIVED","global":true}`))
	f.on("GET", "/4/globalconfiguration", nil, "", 200, `{"allowLoggingOnClosedAccount":false,"approvalPeriod":"WEEK","approvalWeekStart":1,"numberOfDaysAllowedIntoFuture":30,"planApprovalEnabled":false,"weekStart":1}`)

	day := Start{Date: mustDate(t, "2026-11-10"), Time: mustClock(t, "09:00:00")}
	cat := AttributeValue{Key: "_Category_", Value: "dev"}
	base := func(edit func(*Draft)) Op {
		d := Draft{Issue: Issue{Key: "WEB-1"}, Author: "acc-user1", Start: day, Spent: time.Hour, Attributes: []AttributeValue{cat}}
		edit(&d)
		return CreateOp(d)
	}
	with := func(extra ...AttributeValue) func(*Draft) {
		return func(d *Draft) { d.Attributes = append(d.Attributes, extra...) }
	}
	approvedRecord := Worklog{ID: 70, Issue: Issue{ID: 10001}, Author: "acc-user1", Start: Start{Date: mustDate(t, "2026-11-03")}, Spent: time.Hour, Attributes: []AttributeValue{cat}}

	cases := []struct {
		name string
		op   Op
		want []want
	}{
		{"valid", base(func(*Draft) {}), nil},
		{"valid with every type", base(with(AttributeValue{"_Overtime_", "true"}, AttributeValue{"_Units_", "1.5"}, AttributeValue{"_Account_", "ACME"}, AttributeValue{"_Note_", "free text"})), nil},
		{"a full day", base(func(d *Draft) { d.Spent = 24 * time.Hour }), nil},
		{"no issue", base(func(d *Draft) { d.Issue = Issue{} }), []want{{FieldIssue, "", Missing}}},
		{"unknown key", base(func(d *Draft) { d.Issue = Issue{Key: "WEB-404"} }), []want{{FieldIssue, "", NotFound}}},
		{"no author", base(func(d *Draft) { d.Author = "" }), []want{{FieldAuthor, "", Missing}}},
		{"no date", base(func(d *Draft) { d.Start = Start{} }), []want{{FieldStartDate, "", Missing}}},
		{"zero spent", base(func(d *Draft) { d.Spent = 0 }), []want{{FieldSpent, "", NotPositive}}},
		{"under half a second", base(func(d *Draft) { d.Spent = 400 * time.Millisecond }), []want{{FieldSpent, "", NotPositive}}},
		{"over a day", base(func(d *Draft) { d.Spent = 24*time.Hour + time.Second }), []want{{FieldSpent, "", OverADay}}},
		{"negative billable", base(func(d *Draft) { d.Billable = ptr(-time.Second) }), []want{{FieldBillable, "", Negative}}},
		{"negative remaining", base(func(d *Draft) { d.RemainingEstimate = ptr(-time.Minute) }), []want{{FieldRemaining, "", Negative}}},
		{"required missing", base(func(d *Draft) { d.Attributes = nil }), []want{{FieldAttribute, "_Category_", Missing}}},
		{"not an option", base(func(d *Draft) { d.Attributes = []AttributeValue{{"_Category_", "Development"}} }), []want{{FieldAttribute, "_Category_", NotAllowed}}},
		{"unknown attribute", base(with(AttributeValue{"_Nope_", "x"})), []want{{FieldAttribute, "_Nope_", UnknownAttribute}}},
		{"twice", base(with(AttributeValue{"_Note_", "a"}, AttributeValue{"_Note_", "b"})), []want{{FieldAttribute, "_Note_", Duplicate}}},
		{"empty value", base(with(AttributeValue{"_Note_", ""})), []want{{FieldAttribute, "_Note_", Missing}}},
		{"checkbox", base(with(AttributeValue{"_Overtime_", "yes"})), []want{{FieldAttribute, "_Overtime_", NotAllowed}}},
		{"number", base(with(AttributeValue{"_Units_", "NaN"})), []want{{FieldAttribute, "_Units_", NotAllowed}}},
		{"closed account", base(with(AttributeValue{"_Account_", "OLD"})), []want{{FieldAttribute, "_Account_", NotAllowed}}},
		{"archived account", base(with(AttributeValue{"_Account_", "GONE"})), []want{{FieldAttribute, "_Account_", NotAllowed}}},
		{"no such account", base(with(AttributeValue{"_Account_", "NOPE"})), []want{{FieldAttribute, "_Account_", NotFound}}},
		{"approved", base(func(d *Draft) { d.Start = Start{Date: mustDate(t, "2026-11-03")} }), []want{{FieldStartDate, "", PeriodApproved}}},
		{"waiting for approval", base(func(d *Draft) { d.Start = Start{Date: mustDate(t, "2026-11-17")} }), []want{{FieldStartDate, "", PeriodSubmitted}}},
		{"same open week, no new read", base(func(d *Draft) { d.Start = Start{Date: mustDate(t, "2026-11-12")} }), nil},
		{"moved out of an approved week", UpdateOp(Change{From: approvedRecord, Start: &day}), []want{{FieldStartDate, "", PeriodApproved}}},
		{"change without an ID", UpdateOp(Change{From: Worklog{Issue: Issue{ID: 10001}, Author: "acc-user1", Start: day, Spent: time.Hour, Attributes: []AttributeValue{cat}}}), []want{{FieldWorklog, "", Missing}}},
		{"set and cleared", UpdateOp(Change{From: Worklog{ID: 71, Issue: Issue{ID: 10001}, Author: "acc-user1", Start: day, Spent: time.Hour, Attributes: []AttributeValue{cat}},
			SetAttributes: []AttributeValue{{"_Note_", "x"}}, ClearAttributes: []string{"_Note_"}}), []want{{FieldAttribute, "_Note_", Conflicting}}},
		{"delete without an ID", DeleteOp(0), []want{{FieldWorklog, "", Missing}}},
		{"delete", DeleteOp(5), nil},
	}
	ops := make([]Op, len(cases))
	for i, tc := range cases {
		ops[i] = tc.op
	}
	got, err := c.Validate(context.Background(), ops)
	if err != nil {
		t.Fatal(err)
	}
	for i, tc := range cases {
		if !reflect.DeepEqual(kinds(got[i].Problems), tc.want) {
			t.Errorf("%s: %+v, want %v", tc.name, got[i], tc.want)
		}
		for _, p := range got[i].Problems {
			if p.Message == "" {
				t.Errorf("%s: a problem without a message: %+v", tc.name, p)
			}
		}
		if len(got[i].Unchecked) != 0 {
			t.Errorf("%s: unchecked %v with every read allowed", tc.name, got[i].Unchecked)
		}
	}
	idCalls, _ := issues.calls()
	if !reflect.DeepEqual(idCalls, [][]string{{"WEB-1", "WEB-404"}}) {
		t.Errorf("ID lookups %v", idCalls)
	}
	if msg := got[13].Problems[0].Message; !strings.Contains(msg, "Development (dev)") {
		t.Errorf("a not-allowed option names the options: %q", msg)
	}
	if msg := got[25].Problems[0].Message; !strings.Contains(msg, "current start date 2026-11-03") {
		t.Errorf("a move out of an approved week names the current date: %q", msg)
	}
}

// TestValidateInactiveAccountsAllowed: Tempo's one setting lets a
// worklog name a closed or an archived account.
func TestValidateInactiveAccountsAllowed(t *testing.T) {
	f := newFake(t)
	c := testClient(t, f, newIssues())
	f.on("GET", "/4/work-attributes", q("offset", "0", "limit", "1000"), "", 200, attributesJSON)
	f.on("POST", "/4/accounts/search", q("offset", "0", "limit", "1000"), `{"keys":["OLD","GONE"]}`, 200, pageOf(0, false,
		`{"id":2,"key":"OLD","name":"Old","status":"CLOSED","global":false}`,
		`{"id":3,"key":"GONE","name":"Gone","status":"ARCHIVED","global":false}`))
	f.on("GET", "/4/globalconfiguration", nil, "", 200, `{"allowLoggingOnClosedAccount":true,"weekStart":7}`)
	f.on("GET", "/4/timesheet-approvals/user/acc-user1", q("from", "2026-11-10", "to", "2026-11-10"), "", 200, approvalJSONFor("2026-11-09", "2026-11-15", "OPEN"))
	op := func(account string) Op {
		return CreateOp(Draft{
			Issue: Issue{ID: 10001}, Author: "acc-user1", Start: Start{Date: mustDate(t, "2026-11-10")}, Spent: time.Hour,
			Attributes: []AttributeValue{{"_Category_", "mtg"}, {"_Account_", account}},
		})
	}
	got, err := c.Validate(context.Background(), []Op{op("OLD"), op("GONE")})
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range got {
		if len(r.Problems) != 0 || len(r.Unchecked) != 0 {
			t.Errorf("op %d: %+v", i, r)
		}
	}
	s, err := c.Settings(context.Background())
	if err != nil || s.WeekStart != time.Sunday || !s.InactiveAccountsAllowed {
		t.Fatalf("settings from the cache %+v %v", s, err)
	}
}

func TestValidateReadFailure(t *testing.T) {
	f := newFake(t)
	c := testClient(t, f, newIssues())
	f.on("GET", "/4/work-attributes", q("offset", "0", "limit", "1000"), "", 200, attributesJSON)
	f.on("GET", "/4/timesheet-approvals/user/acc-user1", q("from", "2026-11-10", "to", "2026-11-10"), "", 200, approvalJSONFor("2026-11-02", "2026-11-08", "OPEN"))
	_, err := c.Validate(context.Background(), []Op{CreateOp(Draft{
		Issue: Issue{ID: 10001}, Author: "acc-user1", Start: Start{Date: mustDate(t, "2026-11-10")}, Spent: time.Hour,
		Attributes: []AttributeValue{{"_Category_", "dev"}},
	})})
	if err == nil || !strings.Contains(err.Error(), "does not hold 2026-11-10") {
		t.Fatalf("an approval for another period was accepted: %v", err)
	}
}
