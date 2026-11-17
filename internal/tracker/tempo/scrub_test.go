package tempo

import (
	"context"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/secret"
)

// escToken is testToken as JSON writes it with its first letter escaped:
// scrubbing the raw answer would miss it, so only text scrubbed after it
// is decoded comes out clean.
// The escape is built here because a literal one does not survive every
// editor: "\x5c" is the backslash.
const escToken = "\x5c" + "u0066" + "ake-tempo-token"

// scrubbed fails t for each named value that holds the credential or
// lacks the redaction mark.
func scrubbed(t *testing.T, values map[string]string) {
	t.Helper()
	for name, v := range values {
		if strings.Contains(v, testToken) || !strings.Contains(v, secret.Redacted) {
			t.Errorf("%s: %q", name, v)
		}
	}
}

// TestDecodedTextIsScrubbed: every string a result keeps from an answer
// is scrubbed of the credential after it is decoded.
func TestDecodedTextIsScrubbed(t *testing.T) {
	f := newFake(t)
	c := testClient(t, f, newIssues())
	ctx := context.Background()
	f.on("GET", "/4/worklogs/46", nil, "", 200, `{"tempoWorklogId":46,"issue":{"id":10001},"author":{"accountId":"a`+escToken+`"},
"startDate":"2026-11-10","timeSpentSeconds":3600,"description":"d`+escToken+`",
"attributes":{"values":[{"key":"k`+escToken+`","value":"v`+escToken+`"}]},"createdAt":"2026-11-02T08:00:00Z","updatedAt":"2026-11-03T09:30:00Z"}`)
	f.on("GET", "/4/work-attributes", q("offset", "0", "limit", "1000"), "", 200, pageOf(0, false,
		`{"key":"k`+escToken+`","name":"n`+escToken+`","type":"t`+escToken+`","required":false,"values":["v`+escToken+`"],"names":{"v`+escToken+`":"o`+escToken+`"}}`))
	f.on("GET", "/4/globalconfiguration", nil, "", 200, `{"approvalPeriod":"p`+escToken+`"}`)
	f.on("POST", "/4/accounts/search", q("offset", "0", "limit", "1000"), "{}", 200, pageOf(0, false,
		`{"id":1,"key":"k`+escToken+`","name":"n`+escToken+`","status":"s`+escToken+`","global":false}`))
	f.on("GET", "/4/user-schedule/acc-user1", q("from", "2026-12-25", "to", "2026-12-25"), "", 200, `{"results":[
 {"date":"2026-12-25","requiredSeconds":0,"type":"t`+escToken+`","holiday":{"name":"n`+escToken+`","description":"d`+escToken+`","durationSeconds":28800,"type":"FIXED"}}]}`)
	f.on("GET", "/4/timesheet-approvals/user/acc-user1", q("from", "2026-11-04", "to", "2026-11-04"), "", 200, `{"period":{"from":"2026-11-02","to":"2026-11-08"},
"reviewer":{"accountId":"r`+escToken+`"},"status":{"key":"s`+escToken+`","actor":{"accountId":"a`+escToken+`"},"updatedAt":"2026-11-08"}}`)

	w, err := c.Worklog(ctx, 46)
	if err != nil {
		t.Fatal(err)
	}
	scrubbed(t, map[string]string{"Worklog.Author": w.Author, "Worklog.Description": w.Description,
		"AttributeValue.Key": w.Attributes[0].Key, "AttributeValue.Value": w.Attributes[0].Value})

	attrs, err := c.Attributes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a := attrs[0]
	scrubbed(t, map[string]string{"Attribute.Key": a.Key, "Attribute.Name": a.Name, "Attribute.Type": string(a.Type),
		"Option.Value": a.Options[0].Value, "Option.Name": a.Options[0].Name})

	s, err := c.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	scrubbed(t, map[string]string{"Settings.ApprovalPeriod": s.ApprovalPeriod})

	accounts, err := c.Accounts(ctx, AccountQuery{})
	if err != nil {
		t.Fatal(err)
	}
	scrubbed(t, map[string]string{"Account.Key": accounts[0].Key, "Account.Name": accounts[0].Name, "Account.Status": string(accounts[0].Status)})

	day := mustDate(t, "2026-12-25")
	days, err := c.Schedule(ctx, "acc-user1", day, day)
	if err != nil {
		t.Fatal(err)
	}
	scrubbed(t, map[string]string{"ScheduleDay.Kind": string(days[0].Kind), "Holiday.Name": days[0].Holiday.Name, "Holiday.Description": days[0].Holiday.Description})

	ap, err := c.Approval(ctx, "acc-user1", mustDate(t, "2026-11-04"))
	if err != nil {
		t.Fatal(err)
	}
	scrubbed(t, map[string]string{"Approval.Status": string(ap.Status), "Approval.Reviewer": ap.Reviewer, "Approval.Actor": ap.Actor})
}

// TestReadValuesInProblemsAreScrubbed: a value read from Tempo that a
// change sends back, and a check names in a problem, carries no
// credential; nor does Tempo's own refusal.
func TestReadValuesInProblemsAreScrubbed(t *testing.T) {
	f := newFake(t)
	c := testClient(t, f, newIssues())
	ctx := context.Background()
	f.on("GET", "/4/worklogs/47", nil, "", 200, `{"tempoWorklogId":47,"issue":{"id":10001},"author":{"accountId":"acc-user1"},
"startDate":"2026-11-10","timeSpentSeconds":3600,"description":"",
"attributes":{"values":[{"key":"_Category_","value":"dev"},{"key":"_Units_","value":"`+escToken+`"},{"key":"k`+escToken+`","value":"v"}]},
"createdAt":"2026-11-02T08:00:00Z","updatedAt":"2026-11-03T09:30:00Z"}`)
	openWeek(f)
	f.on("POST", "/4/worklogs", nil, createBody(10001, "x"), 400, `{"errors":[{"message":"refused `+escToken+`"}]}`)

	w, err := c.Worklog(ctx, 47)
	if err != nil {
		t.Fatal(err)
	}
	reports, err := c.Validate(ctx, []Op{UpdateOp(Change{From: w, Spent: ptr(2 * time.Hour)})})
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(reports[0].Problems); len(got) != 2 || got[0].code != NotAllowed || got[1].code != UnknownAttribute {
		t.Fatalf("%+v", reports[0])
	}
	scrubbed(t, map[string]string{"number message": reports[0].Problems[0].Message, "unknown message": reports[0].Problems[1].Message})

	out := c.Apply(ctx, []Op{CreateOp(weekDraft(t, "WEB-1", "x"))})[0]
	if out.Status != Refused || len(out.Problems) != 1 {
		t.Fatalf("%+v", out)
	}
	scrubbed(t, map[string]string{"Tempo's message": out.Problems[0].Message, "error": out.Err.Error()})
}
