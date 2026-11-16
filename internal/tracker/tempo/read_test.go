package tempo

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"
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
	if !DateOf(time.Date(2026, 11, 9, 23, 30, 0, 0, time.FixedZone("X", -5*3600))).Before(DateOf(time.Date(2026, 11, 10, 4, 30, 0, 0, time.UTC))) {
		t.Error("DateOf reads the time's own zone")
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
	f := newFake(t)
	c := testClient(t, f, newIssues())
	f.on("GET", "/4/user-schedule/acc-user1", q("from", "2026-12-24", "to", "2026-12-26"), "", 200, `{"self":"x","metadata":{"count":3},"results":[
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
	f := newFake(t)
	c := testClient(t, f, newIssues())
	f.on("GET", "/4/timesheet-approvals/user/acc-user1", q("from", "2026-11-04", "to", "2026-11-04"), "", 200, approvalJSONFor("2026-11-02", "2026-11-08", "APPROVED"))
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
	f := newFake(t)
	c := testClient(t, f, newIssues())
	f.on("POST", "/4/accounts/search", q("offset", "0", "limit", "1000"), `{"statuses":["OPEN"]}`, 200, pageOf(0, true,
		`{"self":"x","id":1,"key":"ACME","name":"Acme","status":"OPEN","global":false,"lead":{"accountId":"acc-user1"}}`))
	f.on("POST", "/4/accounts/search", q("offset", "1", "limit", "1000"), `{"statuses":["OPEN"]}`, 200, pageOf(1, false,
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
	f := newFake(t)
	c := testClient(t, f, newIssues())
	items := make([]string, maxAccounts+1)
	for i := range items {
		items[i] = `{"id":1,"key":"A","name":"A","status":"OPEN"}`
	}
	f.on("POST", "/4/accounts/search", q("offset", "0", "limit", "1000"), `{}`, 200, pageOf(0, false, items...))
	if _, err := c.Accounts(context.Background(), AccountQuery{}); err == nil {
		t.Fatal("a listing over the bound was returned")
	}
}
