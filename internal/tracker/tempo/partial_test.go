package tempo

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"agent-overflow/internal/tracker/tempo/tempotest"
	"agent-overflow/internal/tracker/trackerapi"
)

const internalError = `{"errors":[{"message":"internal"}]}`

// dayDraft is a week draft on day.
func dayDraft(t *testing.T, desc, day string) Draft {
	d := weekDraft(t, "WEB-1", desc)
	d.Start = Start{Date: mustDate(t, day)}
	return d
}

func approvalQuery(day string) map[string][]string { return q("from", day, "to", day) }

// TestCheckFailureIsPerOp: a check read that fails other than with 403
// refuses, unsent, exactly the ops that read would judge; the other ops
// in the batch are checked in full and sent.
func TestCheckFailureIsPerOp(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *tempotest.Server)
		ops   func(t *testing.T) []Op
		body  string // the second op's create, which is sent
	}{
		{"accounts", func(f *tempotest.Server) {
			openWeek(f)
			f.On("POST", "/4/accounts/search", q("offset", "0", "limit", "1000"), `{"keys":["ACME"]}`, 500, internalError).Repeat = true
		}, func(t *testing.T) []Op {
			return []Op{CreateOp(accountDraft(t, "a", "ACME")), CreateOp(weekDraft(t, "WEB-1", "b"))}
		}, createBody(10001, "b")},
		{"settings", func(f *tempotest.Server) {
			openWeek(f)
			f.On("POST", "/4/accounts/search", q("offset", "0", "limit", "1000"), `{"keys":["OLD","ACME"]}`, 200, pageOf(0, false,
				`{"id":1,"key":"ACME","name":"Acme","status":"OPEN","global":false}`,
				`{"id":2,"key":"OLD","name":"Old","status":"CLOSED","global":false}`)).Repeat = true
			f.On("GET", "/4/globalconfiguration", nil, "", 500, internalError).Repeat = true
		}, func(t *testing.T) []Op {
			return []Op{CreateOp(accountDraft(t, "a", "OLD")), CreateOp(accountDraft(t, "b", "ACME"))}
		}, accountBody("b", "ACME")},
		{"approvals", func(f *tempotest.Server) {
			f.On("GET", "/4/work-attributes", q("offset", "0", "limit", "1000"), "", 200, attributesJSON)
			f.On("GET", "/4/timesheet-approvals/user/acc-user1", approvalQuery("2026-11-10"), "", 500, internalError).Repeat = true
			f.On("GET", "/4/timesheet-approvals/user/acc-user1", approvalQuery("2026-11-17"), "", 200, approvalJSONFor("2026-11-16", "2026-11-22", "OPEN")).Repeat = true
		}, func(t *testing.T) []Op {
			return []Op{CreateOp(dayDraft(t, "a", "2026-11-10")), CreateOp(dayDraft(t, "b", "2026-11-17"))}
		}, `{"authorAccountId":"acc-user1","issueId":10001,"startDate":"2026-11-17","timeSpentSeconds":3600,"description":"b","attributes":[{"key":"_Category_","value":"dev"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := tempotest.NewServer(t, testToken)
			c := testClient(t, f, newIssues())
			tc.setup(f)
			f.On("POST", "/4/worklogs", nil, tc.body, 200, stored(90))
			ops := tc.ops(t)
			reports, err := c.Validate(context.Background(), ops)
			if err != nil {
				t.Fatal(err)
			}
			if se, ok := errors.AsType[*trackerapi.StatusError](reports[0].Err); !ok || se.Status != 500 {
				t.Errorf("validate, the judged op: %+v", reports[0])
			}
			if reports[1].Err != nil || len(reports[1].Problems) != 0 {
				t.Errorf("validate, the other op: %+v", reports[1])
			}
			out := c.Apply(context.Background(), ops)
			if se, ok := errors.AsType[*trackerapi.StatusError](out[0].Err); out[0].Status != Refused || !ok || se.Status != 500 || !trackerapi.WriteNotApplied(out[0].Err) {
				t.Errorf("apply, the judged op: %+v", out[0])
			}
			if out[1].Status != Applied {
				t.Errorf("apply, the other op: %+v", out[1])
			}
		})
	}
}

// TestApprovalPeriodsStayKnown: a period already read judges every date
// in it, even after a read for another period was refused; only a date no
// period read holds, whose own read failed, goes unchecked. A refused
// date is read once.
func TestApprovalPeriodsStayKnown(t *testing.T) {
	f := tempotest.NewServer(t, testToken)
	c := testClient(t, f, newIssues())
	f.On("GET", "/4/work-attributes", q("offset", "0", "limit", "1000"), "", 200, attributesJSON)
	f.On("GET", "/4/timesheet-approvals/user/acc-user1", approvalQuery("2026-11-10"), "", 200, approvalJSONFor("2026-11-09", "2026-11-15", "APPROVED"))
	f.On("GET", "/4/timesheet-approvals/user/acc-user1", approvalQuery("2026-11-17"), "", 403, forbiddenHTML)
	ops := []Op{
		CreateOp(dayDraft(t, "a", "2026-11-10")),
		CreateOp(dayDraft(t, "b", "2026-11-17")),
		CreateOp(dayDraft(t, "c", "2026-11-11")),
		CreateOp(dayDraft(t, "d", "2026-11-17")),
	}
	reports, err := c.Validate(context.Background(), ops)
	if err != nil {
		t.Fatal(err)
	}
	approved := []want{{FieldStartDate, "", PeriodApproved}}
	for _, i := range []int{0, 2} {
		if r := reports[i]; !reflect.DeepEqual(kinds(r.Problems), approved) || len(r.Unchecked) != 0 || r.Err != nil {
			t.Errorf("op %d, in the approved week: %+v", i, r)
		}
	}
	for _, i := range []int{1, 3} {
		if r := reports[i]; len(r.Problems) != 0 || !reflect.DeepEqual(r.Unchecked, []Check{CheckApprovals}) || r.Err != nil {
			t.Errorf("op %d, in the refused week: %+v", i, r)
		}
	}
}

// TestFailedDateHeldByALaterPeriod: a date whose own read failed is still
// judged by a period another date's read returned.
func TestFailedDateHeldByALaterPeriod(t *testing.T) {
	f := tempotest.NewServer(t, testToken)
	c := testClient(t, f, newIssues())
	f.On("GET", "/4/work-attributes", q("offset", "0", "limit", "1000"), "", 200, attributesJSON)
	f.On("GET", "/4/timesheet-approvals/user/acc-user1", approvalQuery("2026-11-16"), "", 500, internalError).Repeat = true
	f.On("GET", "/4/timesheet-approvals/user/acc-user1", approvalQuery("2026-11-17"), "", 200, approvalJSONFor("2026-11-16", "2026-11-22", "APPROVED"))
	reports, err := c.Validate(context.Background(), []Op{CreateOp(dayDraft(t, "a", "2026-11-16")), CreateOp(dayDraft(t, "b", "2026-11-17"))})
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range reports {
		if !reflect.DeepEqual(kinds(r.Problems), []want{{FieldStartDate, "", PeriodApproved}}) || r.Err != nil {
			t.Errorf("op %d: %+v", i, r)
		}
	}
}

// TestOpRulesAlwaysRun: the checks decidable from the op alone run when
// the attributes read is refused or fails and when IssueIDs fails.
func TestOpRulesAlwaysRun(t *testing.T) {
	draft := func(t *testing.T) Draft {
		d := dayDraft(t, "x", "2026-11-10")
		d.Spent = 0
		d.Attributes = []AttributeValue{{"", "x"}, {"_Note_", "a"}, {"_Note_", "b"}, {"_Units_", ""}}
		return d
	}
	opOnly := []want{
		{FieldSpent, "", NotPositive},
		{FieldAttribute, "", Missing},
		{FieldAttribute, "_Note_", Duplicate},
		{FieldAttribute, "_Units_", Missing},
	}
	cases := []struct {
		name      string
		status    int
		resolver  error
		unchecked []Check
		want      []want
	}{
		{"attributes refused", 403, nil, []Check{CheckAttributes, CheckAccounts}, opOnly},
		{"attributes failed", 500, nil, nil, opOnly},
		{"resolver failed", 200, errors.New("jira is down"), nil, append(append([]want{}, opOnly...), want{FieldAttribute, "_Category_", Missing})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := tempotest.NewServer(t, testToken)
			issues := newIssues()
			issues.err = tc.resolver
			c := testClient(t, f, issues)
			resp := attributesJSON
			if tc.status != 200 {
				resp = internalError
			}
			f.On("GET", "/4/work-attributes", q("offset", "0", "limit", "1000"), "", tc.status, resp).Repeat = true
			f.On("GET", "/4/timesheet-approvals/user/acc-user1", approvalQuery("2026-11-10"), "", 200, approvalJSONFor("2026-11-09", "2026-11-15", "OPEN"))
			reports, err := c.Validate(context.Background(), []Op{CreateOp(draft(t))})
			if err != nil {
				t.Fatal(err)
			}
			r := reports[0]
			if !reflect.DeepEqual(kinds(r.Problems), tc.want) || !reflect.DeepEqual(r.Unchecked, tc.unchecked) {
				t.Errorf("%+v", r)
			}
			if failed := tc.status == 500 || tc.resolver != nil; failed != (r.Err != nil) {
				t.Errorf("error %v", r.Err)
			}
		})
	}
}

// TestClosedClientFailsAtAdmission: every exported call on a closed
// Client fails before any IssueIDs call or request with a NotSentError
// matching ErrClosed; on a Client New did not build, it fails the same
// way instead of panicking.
func TestClosedClientFailsAtAdmission(t *testing.T) {
	day := mustDate(t, "2026-11-10")
	from := Worklog{ID: 70, Issue: Issue{ID: 10001}, Author: "acc-user1", Start: Start{Date: day}, Spent: time.Hour}
	query := WorklogQuery{Author: "acc-user1", From: day, To: day}
	calls := func(c *Client) map[string]func() error {
		ctx := context.Background()
		return map[string]func() error{
			"Apply": func() error {
				out := c.Apply(ctx, []Op{CreateOp(weekDraft(t, "WEB-1", "x")), DeleteOp(5)})
				for _, o := range out {
					if o.Status != NotAttempted {
						return errors.New("not NotAttempted: " + o.Status.String())
					}
				}
				return errors.Join(out[0].Err, out[1].Err)
			},
			"Create":   func() error { _, err := c.Create(ctx, weekDraft(t, "WEB-1", "x")); return err },
			"Update":   func() error { _, err := c.Update(ctx, Change{From: from, Issue: &Issue{Key: "WEB-2"}}); return err },
			"Delete":   func() error { return c.Delete(ctx, 5) },
			"Validate": func() error { _, err := c.Validate(ctx, []Op{CreateOp(weekDraft(t, "WEB-1", "x"))}); return err },
			"Worklogs": func() error { _, err := c.Worklogs(ctx, query); return err },
			"WorklogPages": func() error {
				return c.WorklogPages(ctx, query, func([]Worklog) error { return nil })
			},
			"Worklog":    func() error { _, err := c.Worklog(ctx, 5); return err },
			"Attributes": func() error { _, err := c.Attributes(ctx); return err },
			"Settings":   func() error { _, err := c.Settings(ctx); return err },
			"Accounts":   func() error { _, err := c.Accounts(ctx, AccountQuery{}); return err },
			"Schedule":   func() error { _, err := c.Schedule(ctx, "acc-user1", day, day); return err },
			"Approval":   func() error { _, err := c.Approval(ctx, "acc-user1", day); return err },
		}
	}
	t.Run("closed", func(t *testing.T) {
		f := tempotest.NewServer(t, testToken)
		issues := newIssues()
		c := testClient(t, f, issues)
		c.Close()
		for name, call := range calls(c) {
			if err := call(); !errors.Is(err, ErrClosed) || !isType[*trackerapi.NotSentError](err) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if ids := issues.calls(); len(ids) != 0 {
			t.Errorf("IssueIDs called after Close: %v", ids)
		}
		if r := f.Requests(); len(r) != 0 {
			t.Errorf("requests after Close: %v", r)
		}
	})
	for name, c := range map[string]*Client{"nil": nil, "zero": {}} {
		t.Run(name, func(t *testing.T) {
			for call, run := range calls(c) {
				if err := run(); !errors.Is(err, errNotBuilt) || !isType[*trackerapi.NotSentError](err) {
					t.Errorf("%s: %v", call, err)
				}
			}
			c.ForgetCredential()
			c.DropCache()
			c.Close()
		})
	}
}
