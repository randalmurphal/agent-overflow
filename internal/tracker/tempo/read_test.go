package tempo

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"testing"
	"time"

	"agent-overflow/internal/tracker/tempo/tempotest"
)

func TestDate(t *testing.T) {
	d := mustDate(t, "2026-11-09")
	if d.Year() != 2026 || d.Month() != time.November || d.Day() != 9 || d.Weekday() != time.Monday {
		t.Fatalf("parts of %s", d)
	}
	if got := d.AddDays(-9).String(); got != "2026-10-31" {
		t.Errorf("AddDays %s", got)
	}
	if !d.Before(d.AddDays(1)) || d.Compare(mustDate(t, "2026-11-09")) != 0 || !d.After(Date{}) {
		t.Error("ordering")
	}
	if d.In(time.UTC) != time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC) {
		t.Error("In")
	}
	for _, bad := range []string{"2026-02-30", "2026-11-9", "09/11/2026", ""} {
		if _, err := ParseDate(bad); err == nil {
			t.Errorf("ParseDate(%q) accepted", bad)
		}
	}
	if _, err := NewDate(2026, time.February, 29); err == nil {
		t.Error("NewDate accepted February 29 of a common year")
	}
	if got, err := NewDate(2028, time.February, 29); err != nil || got.String() != "2028-02-29" {
		t.Errorf("NewDate leap day %s %v", got, err)
	}
	b, err := json.Marshal(struct{ D Date }{d})
	if err != nil || string(b) != `{"D":"2026-11-09"}` {
		t.Errorf("marshal %s %v", b, err)
	}
	var back struct{ D Date }
	if err := json.Unmarshal(b, &back); err != nil || back.D != d {
		t.Errorf("unmarshal %v %v", back, err)
	}
	if (Date{}).String() != "" || !(Date{}).AddDays(3).IsZero() {
		t.Error("zero Date")
	}
}

func TestTimeOfDay(t *testing.T) {
	for in, want := range map[string]string{"09:00": "09:00:00", "9:05:07": "09:05:07", "23:59:59": "23:59:59", "00:00:00": "00:00:00"} {
		got, err := ParseTimeOfDay(in)
		if err != nil || got.String() != want || !got.IsSet() {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"24:00", "12:60", "12", "12:00:00:00", "ab:cd", "-1:00", "12:5"} {
		if _, err := ParseTimeOfDay(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	midnight, _ := Clock(0, 0, 0)
	if !midnight.IsSet() || (TimeOfDay{}).IsSet() || midnight.SinceMidnight() != 0 {
		t.Error("midnight is a time; the zero TimeOfDay is none")
	}
	if c, _ := Clock(13, 30, 15); c.SinceMidnight() != 13*time.Hour+30*time.Minute+15*time.Second {
		t.Error("SinceMidnight")
	}
	for d, want := range map[time.Duration]int64{1499 * time.Millisecond: 1, 1500 * time.Millisecond: 2, 90 * time.Minute: 5400, -time.Second: -1} {
		if got := seconds(d); got != want {
			t.Errorf("seconds(%v) = %d, want %d", d, got, want)
		}
	}
}

func TestSchedule(t *testing.T) {
	f := tempotest.NewServer(t, testToken)
	c := testClient(t, f, newIssues())
	f.On("GET", "/4/user-schedule/acc-user1", q("from", "2026-12-24", "to", "2026-12-26"), "", 200, `{"self":"x","metadata":{"count":3},"results":[
	 {"date":"2026-12-24","requiredSeconds":14400,"type":"WORKING_DAY"},
	 {"date":"2026-12-25","requiredSeconds":0,"type":"HOLIDAY","holiday":{"self":"x","id":3,"schemeId":1,"name":"Holiday 1","description":"A day off","durationSeconds":28800,"date":"2026-12-25","type":"FIXED"}},
	 {"date":"2026-12-26","requiredSeconds":0,"type":"NON_WORKING_DAY"}]}`)
	got, err := c.Schedule(context.Background(), "acc-user1", mustDate(t, "2026-12-24"), mustDate(t, "2026-12-26"))
	if err != nil {
		t.Fatal(err)
	}
	want := []ScheduleDay{
		{Date: mustDate(t, "2026-12-24"), Kind: WorkingDay, Required: 4 * time.Hour},
		{Date: mustDate(t, "2026-12-25"), Kind: HolidayDay, Holiday: &Holiday{Name: "Holiday 1", Description: "A day off", Duration: 8 * time.Hour}},
		{Date: mustDate(t, "2026-12-26"), Kind: NonWorkingDay},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("schedule\n got %+v\nwant %+v", got, want)
	}
	if _, err := c.Schedule(context.Background(), "", mustDate(t, "2026-12-24"), mustDate(t, "2026-12-26")); err == nil {
		t.Error("a schedule without an account ID was sent")
	}
}

func TestApproval(t *testing.T) {
	f := tempotest.NewServer(t, testToken)
	c := testClient(t, f, newIssues())
	f.On("GET", "/4/timesheet-approvals/user/acc-user1", q("from", "2026-11-04", "to", "2026-11-04"), "", 200, approvalJSONFor("2026-11-02", "2026-11-08", "APPROVED"))
	got, err := c.Approval(context.Background(), "acc-user1", mustDate(t, "2026-11-04"))
	if err != nil {
		t.Fatal(err)
	}
	want := Approval{From: mustDate(t, "2026-11-02"), To: mustDate(t, "2026-11-08"), Status: ApprovalApproved,
		Required: 40 * time.Hour, Spent: 20 * time.Hour, Reviewer: "acc-user2", Actor: "acc-user1",
		Changed: time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)}
	if got != want {
		t.Errorf("approval\n got %+v\nwant %+v", got, want)
	}
}

func TestAccounts(t *testing.T) {
	f := tempotest.NewServer(t, testToken)
	c := testClient(t, f, newIssues())
	f.On("POST", "/4/accounts/search", q("offset", "0", "limit", "1000"), `{"statuses":["OPEN"]}`, 200, pageOf(0, true,
		`{"self":"x","id":1,"key":"ACME","name":"Acme","status":"OPEN","global":false,"lead":{"accountId":"acc-user1"}}`))
	f.On("POST", "/4/accounts/search", q("offset", "1", "limit", "1000"), `{"statuses":["OPEN"]}`, 200, pageOf(1, false,
		`{"self":"x","id":2,"key":"INT","name":"Internal","status":"OPEN","global":true}`))
	got, err := c.Accounts(context.Background(), AccountQuery{Statuses: []AccountStatus{AccountOpen}})
	if err != nil {
		t.Fatal(err)
	}
	want := []Account{{ID: 1, Key: "ACME", Name: "Acme", Status: AccountOpen}, {ID: 2, Key: "INT", Name: "Internal", Status: AccountOpen, Global: true}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("accounts %+v", got)
	}
}

func TestAccountsBound(t *testing.T) {
	f := tempotest.NewServer(t, testToken)
	c := testClient(t, f, newIssues())
	items := make([]string, maxAccounts+1)
	for i := range items {
		items[i] = `{"id":1,"key":"A","name":"A","status":"OPEN"}`
	}
	f.On("POST", "/4/accounts/search", q("offset", "0", "limit", "1000"), `{}`, 200, pageOf(0, false, items...))
	if _, err := c.Accounts(context.Background(), AccountQuery{}); err == nil {
		t.Fatal("a listing over the bound was returned")
	}
}

// TestDateRange: every parse goes through NewDate's range, so a nonempty
// string never reads as the zero Date.
func TestDateRange(t *testing.T) {
	for _, s := range []string{"0000-12-31", "0000-01-01"} {
		if d, err := ParseDate(s); err == nil {
			t.Errorf("ParseDate(%q) = %v with no error", s, d)
		}
	}
	if d, err := ParseDate("0001-01-01"); err != nil || d.IsZero() || d.String() != "0001-01-01" {
		t.Errorf("first day: %v %v", d, err)
	}
	if d, err := ParseDate("9999-12-31"); err != nil || d.String() != "9999-12-31" {
		t.Errorf("last day: %v %v", d, err)
	}
	// AddDays past either end is the zero Date, never a day outside the
	// range nor one wrapped back into it.
	first, last := mustDate(t, "0001-01-01"), mustDate(t, "9999-12-31")
	for _, tc := range []struct {
		d Date
		n int
	}{
		{last, 1}, {first, -1}, {first, lastDay}, {last, -lastDay},
		{first, math.MaxInt}, {last, math.MinInt}, {Date{}, 1},
	} {
		if got := tc.d.AddDays(tc.n); !got.IsZero() {
			t.Errorf("%v.AddDays(%d) = %v, want the zero Date", tc.d, tc.n, got)
		}
	}
	if got := first.AddDays(lastDay - 1); got != last {
		t.Errorf("the whole range: %v", got)
	}
	if got := last.AddDays(-(lastDay - 1)); got != first {
		t.Errorf("the whole range back: %v", got)
	}
	if got := (Date{}).In(time.UTC); !got.IsZero() {
		t.Errorf("the zero Date in UTC is %v", got)
	}
}

// TestTimestamps: Tempo's timestamps are plain strings; the approval
// status example is a date alone, the worklog examples RFC 3339 times.
func TestTimestamps(t *testing.T) {
	for in, want := range map[string]time.Time{
		"2026-11-08T10:00:00Z":      time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC),
		"2026-11-08T10:00:00+01:00": time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC),
		"2022-01-01":                time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC),
	} {
		got, err := parseTimestamp(in)
		if err != nil || !got.Equal(want) {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"01/01/2022", "2022-01-01 10:00", "0000-01-01"} {
		if _, err := parseTimestamp(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}

	f := tempotest.NewServer(t, testToken)
	c := testClient(t, f, newIssues())
	approval := `{"self":"x","actions":{},"worklogs":{"self":"x"},"user":{"accountId":"acc-user1"},"period":{"from":"2026-11-02","to":"2026-11-08"},
"requiredSeconds":0,"timeSpentSeconds":0,"status":{"key":"OPEN","updatedAt":"2022-01-01"}}`
	f.On("GET", "/4/timesheet-approvals/user/acc-user1", q("from", "2026-11-04", "to", "2026-11-04"), "", 200, approval)
	got, err := c.Approval(context.Background(), "acc-user1", mustDate(t, "2026-11-04"))
	if err != nil || !got.Changed.Equal(time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("approval with a date-only updatedAt: %+v %v", got, err)
	}
	f.On("GET", "/4/worklogs/44", nil, "", 200, `{"tempoWorklogId":44,"issue":{"id":10001},"author":{"accountId":"acc-user1"},
"startDate":"2026-11-04","timeSpentSeconds":60,"createdAt":"2026-11-04","updatedAt":"2026-11-05T08:00:00Z","attributes":{"values":[]}}`)
	w, err := c.Worklog(context.Background(), 44)
	if err != nil || !w.Created.Equal(time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("worklog with a date-only createdAt: %+v %v", w, err)
	}
}
