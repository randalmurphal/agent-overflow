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

const forbiddenHTML = `<html>Forbidden</html>`

// accountDraft is a week draft naming account on the account attribute.
func accountDraft(t *testing.T, desc, account string) Draft {
	d := weekDraft(t, "WEB-1", desc)
	d.Attributes = append(d.Attributes, AttributeValue{"_Account_", account})
	return d
}

func accountBody(desc, account string) string {
	return `{"authorAccountId":"acc-user1","issueId":10001,"startDate":"2026-11-10","timeSpentSeconds":3600,"description":"` + desc +
		`","attributes":[{"key":"_Category_","value":"dev"},{"key":"_Account_","value":"` + account + `"}]}`
}

func stored(id int64) string {
	return wl{id: id, issue: 10001, author: "acc-user1", date: "2026-11-10", spent: 3600}.json()
}

// TestForbiddenChecksAreSkipped: a check read the token may not make
// (403) is skipped, the op lists it, and the write is sent for Tempo to
// judge. Validate reports the same.
func TestForbiddenChecksAreSkipped(t *testing.T) {
	approval := q("from", "2026-11-10", "to", "2026-11-10")
	cases := []struct {
		name  string
		setup func(f *tempotest.Server)
		draft func(t *testing.T) Draft
		body  string
		want  []Check
	}{
		{"attributes", func(f *tempotest.Server) {
			f.On("GET", "/4/work-attributes", q("offset", "0", "limit", "1000"), "", 403, forbiddenHTML).Repeat = true
			f.On("GET", "/4/timesheet-approvals/user/acc-user1", approval, "", 200, approvalJSONFor("2026-11-09", "2026-11-15", "OPEN")).Repeat = true
		}, func(t *testing.T) Draft {
			d := accountDraft(t, "a", "ACME")
			d.Attributes[0].Value = "lunch" // not an option: unknown without the attributes
			return d
		}, `{"authorAccountId":"acc-user1","issueId":10001,"startDate":"2026-11-10","timeSpentSeconds":3600,"description":"a","attributes":[{"key":"_Category_","value":"lunch"},{"key":"_Account_","value":"ACME"}]}`,
			[]Check{CheckAttributes, CheckAccounts}},
		{"approvals", func(f *tempotest.Server) {
			f.On("GET", "/4/work-attributes", q("offset", "0", "limit", "1000"), "", 200, attributesJSON)
			f.On("GET", "/4/timesheet-approvals/user/acc-user1", approval, "", 403, forbiddenHTML).Repeat = true
		}, func(t *testing.T) Draft { return weekDraft(t, "WEB-1", "b") }, createBody(10001, "b"), []Check{CheckApprovals}},
		{"accounts", func(f *tempotest.Server) {
			openWeek(f)
			f.On("POST", "/4/accounts/search", q("offset", "0", "limit", "1000"), `{"keys":["ACME"]}`, 403, forbiddenHTML).Repeat = true
		}, func(t *testing.T) Draft { return accountDraft(t, "c", "ACME") }, accountBody("c", "ACME"), []Check{CheckAccounts}},
		{"settings", func(f *tempotest.Server) {
			openWeek(f)
			f.On("POST", "/4/accounts/search", q("offset", "0", "limit", "1000"), `{"keys":["OLD"]}`, 200, pageOf(0, false,
				`{"id":2,"key":"OLD","name":"Old","status":"CLOSED","global":false}`)).Repeat = true
			f.On("GET", "/4/globalconfiguration", nil, "", 403, forbiddenHTML).Repeat = true
		}, func(t *testing.T) Draft { return accountDraft(t, "d", "OLD") }, accountBody("d", "OLD"), []Check{CheckSettings}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := tempotest.NewServer(t, testToken)
			c := testClient(t, f, newIssues())
			tc.setup(f)
			f.On("POST", "/4/worklogs", nil, tc.body, 200, stored(80))
			op := CreateOp(tc.draft(t))
			reports, err := c.Validate(context.Background(), []Op{op})
			if err != nil || len(reports[0].Problems) != 0 || !reflect.DeepEqual(reports[0].Unchecked, tc.want) {
				t.Fatalf("validate: %+v %v", reports, err)
			}
			out := c.Apply(context.Background(), []Op{op})[0]
			if out.Status != Applied || !reflect.DeepEqual(out.Unchecked, tc.want) {
				t.Fatalf("apply: %+v", out)
			}
		})
	}
}

// TestCheckReadFailureRefuses: a check read that fails other than with a
// 403 refuses the write as not sent, and the transport's outermost
// *NotSentError says so although a 5xx answer is inside it.
func TestCheckReadFailureRefuses(t *testing.T) {
	f := tempotest.NewServer(t, testToken)
	c := testClient(t, f, newIssues())
	f.On("GET", "/4/work-attributes", q("offset", "0", "limit", "1000"), "", 500, `{"errors":[{"message":"internal"}]}`)
	f.On("GET", "/4/timesheet-approvals/user/acc-user1", q("from", "2026-11-10", "to", "2026-11-10"), "", 200, approvalJSONFor("2026-11-09", "2026-11-15", "OPEN"))
	f.On("DELETE", "/4/worklogs/3", nil, "", 204, "")
	out := c.Apply(context.Background(), []Op{CreateOp(weekDraft(t, "WEB-1", "x")), DeleteOp(3)})
	if se, ok := errors.AsType[*trackerapi.StatusError](out[0].Err); out[0].Status != Refused || !ok || se.Status != 500 || !trackerapi.WriteNotApplied(out[0].Err) {
		t.Errorf("create without its checks: %+v", out[0])
	}
	if out[1].Status != Applied {
		t.Errorf("delete: %+v", out[1])
	}
}

// blockingIssues is an IssueIDs whose calls wait for their context to
// end.
type blockingIssues struct{ started chan string }

func (b blockingIssues) IDs(ctx context.Context, _ []string) (map[string]int64, error) {
	b.started <- "IDs"
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestCloseEndsIssueIDs: Close cancels the context of a call into the
// caller's IssueIDs, so the write waiting on it returns.
func TestCloseEndsIssueIDs(t *testing.T) {
	f := tempotest.NewServer(t, testToken)
	issues := blockingIssues{started: make(chan string, 1)}
	c := testClient(t, f, issues)
	done := make(chan error, 1)
	go func() {
		out := c.Apply(context.Background(), []Op{CreateOp(weekDraft(t, "WEB-1", "x"))})[0]
		if out.Status != NotAttempted {
			t.Errorf("status %v", out.Status)
		}
		done <- out.Err
	}()
	<-issues.started
	c.Close()
	select {
	case err := <-done:
		// Apply stops at admission once Close ended its checks.
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("err %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the call into IssueIDs outlived Close")
	}
}
