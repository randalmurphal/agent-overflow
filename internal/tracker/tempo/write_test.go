package tempo

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"agent-overflow/internal/tracker/tempo/tempotest"
	"agent-overflow/internal/tracker/trackerapi"
)

// openWeek serves the attributes and an open approval for 2026-11-10.
func openWeek(f *tempotest.Server) {
	f.On("GET", "/4/work-attributes", q("offset", "0", "limit", "1000"), "", 200, attributesJSON)
	f.On("GET", "/4/timesheet-approvals/user/acc-user1", q("from", "2026-11-10", "to", "2026-11-10"), "", 200, approvalJSONFor("2026-11-09", "2026-11-15", "OPEN")).Repeat = true
}

func TestCreate(t *testing.T) {
	f := tempotest.NewServer(t, testToken)
	issues := newIssues()
	c := testClient(t, f, issues)
	openWeek(f)
	f.On("POST", "/4/worklogs", nil,
		`{"authorAccountId":"acc-user1","issueId":10001,"startDate":"2026-11-10","startTime":"09:00:00","timeSpentSeconds":5400,
		  "description":"Code review","attributes":[{"key":"_Category_","value":"dev"}]}`,
		200, wl{id: 100, issue: 10001, author: "acc-user1", date: "2026-11-10", start: "09:00:00", spent: 5400, billable: seconds64(5400), desc: "Code review", attrs: [][2]string{{"_Category_", "dev"}}}.json())
	got, err := c.Create(context.Background(), Draft{
		Issue: Issue{Key: "WEB-1"}, Author: "acc-user1",
		Start: Start{Date: mustDate(t, "2026-11-10"), Time: mustClock(t, "09:00")},
		Spent: 90*time.Minute + 200*time.Millisecond, Description: "Code review",
		Attributes: []AttributeValue{{"_Category_", "dev"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != 100 || got.Issue != (Issue{ID: 10001, Key: "WEB-1"}) || got.Spent != 90*time.Minute {
		t.Errorf("stored %+v", got)
	}
}

func TestCreateRefusedBeforeSending(t *testing.T) {
	f := tempotest.NewServer(t, testToken)
	c := testClient(t, f, newIssues())
	openWeek(f)
	_, err := c.Create(context.Background(), Draft{
		Issue: Issue{ID: 10001}, Author: "acc-user1", Start: Start{Date: mustDate(t, "2026-11-10")}, Spent: time.Hour,
	})
	invalid, ok := errors.AsType[*InvalidError](err)
	if !ok || !trackerapi.WriteNotApplied(err) || !errors.Is(err, trackerapi.ErrInvalidInput) || len(invalid.Problems) != 1 || invalid.Problems[0].Code != Missing {
		t.Fatalf("err %v", err)
	}
	if _, err := c.Worklog(context.Background(), -1); !errors.Is(err, trackerapi.ErrInvalidInput) {
		t.Errorf("a worklog ID that is not one: %v", err)
	}
}

// TestUpdateKeepsWhatItDoesNotChange reads a worklog and changes its time
// alone: everything else goes back as read.
func TestUpdateKeepsWhatItDoesNotChange(t *testing.T) {
	f := tempotest.NewServer(t, testToken)
	c := testClient(t, f, newIssues())
	openWeek(f)
	f.On("GET", "/4/worklogs/70", nil, "", 200, wl{id: 70, issue: 10002, author: "acc-user1", date: "2026-11-10", start: "13:00:00",
		spent: 3600, billable: seconds64(1800), desc: "Planning", attrs: [][2]string{{"_Category_", "mtg"}, {"_Note_", "keep"}}}.json()).Repeat = true
	f.On("PUT", "/4/worklogs/70", nil,
		`{"authorAccountId":"acc-user1","issueId":10002,"startDate":"2026-11-10","startTime":"13:00:00","timeSpentSeconds":7200,
		  "billableSeconds":1800,"description":"Planning","attributes":[{"key":"_Category_","value":"mtg"},{"key":"_Note_","value":"keep"}]}`,
		200, wl{id: 70, issue: 10002, author: "acc-user1", date: "2026-11-10", start: "13:00:00", spent: 7200, billable: seconds64(1800), desc: "Planning",
			attrs: [][2]string{{"_Category_", "mtg"}, {"_Note_", "keep"}}}.json())
	ctx := context.Background()
	read, err := c.Worklog(ctx, 70)
	if err != nil || read.Issue != (Issue{ID: 10002}) {
		t.Fatalf("read %+v %v", read, err)
	}
	stored, err := c.Update(ctx, Change{From: read, Spent: ptr(2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if stored.Spent != 2*time.Hour || stored.Issue != (Issue{ID: 10002}) {
		t.Errorf("stored %+v", stored)
	}
}

// TestUpdateReadsLastBeforeTheWrite changes the record during the
// checks' reads, after the caller read it: the update reads it again as
// the last request before its write and is refused, unsent, for each
// field the write carries over; a record still as read is written right
// after that read.
func TestUpdateReadsLastBeforeTheWrite(t *testing.T) {
	read := wl{id: 70, issue: 10002, author: "acc-user1", date: "2026-11-10", start: "13:00:00", spent: 3600, billable: seconds64(1800), desc: "Planning",
		attrs: [][2]string{{"_Category_", "mtg"}}}
	changes := map[string]func(w *wl){
		"none":        func(*wl) {},
		"issue":       func(w *wl) { w.issue = 10001 },
		"author":      func(w *wl) { w.author = "acc-user2" },
		"date":        func(w *wl) { w.date = "2026-11-11" },
		"start time":  func(w *wl) { w.start = "14:00:00" },
		"spent":       func(w *wl) { w.spent = 1800 },
		"billable":    func(w *wl) { w.billable = seconds64(600) },
		"description": func(w *wl) { w.desc = "Retro" },
		"attribute":   func(w *wl) { w.attrs = [][2]string{{"_Category_", "dev"}} },
		"attribute added": func(w *wl) {
			w.attrs = [][2]string{{"_Category_", "mtg"}, {"_Note_", "x"}}
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			f := tempotest.NewServer(t, testToken)
			c := testClient(t, f, newIssues())
			f.On("GET", "/4/worklogs/70", nil, "", 200, read.json())
			later := read
			change(&later)
			// The checks' reads; the record is changed while they run.
			openWeek(f)
			f.On("GET", "/4/worklogs/70", nil, "", 200, later.json())
			if name == "none" {
				f.On("PUT", "/4/worklogs/70", nil,
					`{"authorAccountId":"acc-user1","issueId":10002,"startDate":"2026-11-10","startTime":"13:00:00","timeSpentSeconds":3600,
					  "billableSeconds":1800,"description":"Planning v2","attributes":[{"key":"_Category_","value":"mtg"}]}`,
					200, read.json())
			}
			ctx := context.Background()
			from, err := c.Worklog(ctx, 70)
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.Update(ctx, Change{From: from, Description: ptr("Planning v2")})
			reqs := f.Requests()
			if name == "none" {
				if err != nil {
					t.Fatalf("an unchanged record was refused: %v", err)
				}
				if n := len(reqs); n < 2 || reqs[n-2] != "GET /4/worklogs/70" || reqs[n-1] != "PUT /4/worklogs/70" {
					t.Errorf("requests %v: the write does not follow its read", reqs)
				}
				return
			}
			invalid, ok := errors.AsType[*InvalidError](err)
			if !ok || !trackerapi.WriteNotApplied(err) || len(invalid.Problems) != 1 || invalid.Problems[0].Code != Changed {
				t.Errorf("a record changed since it was read: %v", err)
			}
			if slices.Contains(reqs, "PUT /4/worklogs/70") {
				t.Errorf("requests %v: the change was overwritten", reqs)
			}
		})
	}
}

func TestChangeDraft(t *testing.T) {
	from := Worklog{
		ID: 7, Issue: Issue{ID: 10001, Key: "WEB-1"}, Author: "acc-user1",
		Start: Start{Date: mustDate(t, "2026-11-10"), Time: mustClock(t, "09:00:00")},
		Spent: time.Hour, Billable: ptr(time.Hour), Description: "Before",
		Attributes: []AttributeValue{{"_Category_", "dev"}, {"_Note_", "a"}},
	}
	billable := func(b time.Duration) Worklog {
		w := from
		w.Billable = &b
		return w
	}
	unread := from
	unread.Billable = nil
	text := ptr("x")
	cases := []struct {
		name string
		ch   Change
		want *time.Duration
	}{
		// Spent is 1h in every From.
		{"text only, billable above spent", Change{From: billable(2 * time.Hour), Description: text}, ptr(2 * time.Hour)},
		{"text only, billable below spent", Change{From: billable(30 * time.Minute), Description: text}, ptr(30 * time.Minute)},
		{"text only, billable equal to spent", Change{From: from, Description: text}, ptr(time.Hour)},
		{"text only, non-billable", Change{From: billable(0), Description: text}, ptr(time.Duration(0))},
		{"text only, billable never read", Change{From: unread, Description: text}, nil},
		{"spent changed, billable equal follows it", Change{From: from, Spent: ptr(2 * time.Hour)}, ptr(2 * time.Hour)},
		{"spent cut, billable equal follows it", Change{From: from, Spent: ptr(15 * time.Minute)}, ptr(15 * time.Minute)},
		{"spent cut, billable above is kept", Change{From: billable(2 * time.Hour), Spent: ptr(30 * time.Minute)}, ptr(2 * time.Hour)},
		{"spent cut below a partial billable, billable stops at it", Change{From: billable(30 * time.Minute), Spent: ptr(15 * time.Minute)}, ptr(15 * time.Minute)},
		{"spent cut above a partial billable, billable is kept", Change{From: billable(30 * time.Minute), Spent: ptr(45 * time.Minute)}, ptr(30 * time.Minute)},
		{"spent raised, partial billable is kept", Change{From: billable(30 * time.Minute), Spent: ptr(2 * time.Hour)}, ptr(30 * time.Minute)},
		{"spent raised, billable above is kept", Change{From: billable(2 * time.Hour), Spent: ptr(3 * time.Hour)}, ptr(2 * time.Hour)},
		{"spent changed, non-billable stays non-billable", Change{From: billable(0), Spent: ptr(2 * time.Hour)}, ptr(time.Duration(0))},
		{"spent changed, billable never read", Change{From: unread, Spent: ptr(2 * time.Hour)}, nil},
		{"spent changed to the same, billable equal", Change{From: from, Spent: ptr(time.Hour)}, ptr(time.Hour)},
		{"set explicitly", Change{From: from, Billable: ptr(10 * time.Minute), Spent: ptr(2 * time.Hour)}, ptr(10 * time.Minute)},
	}
	for _, tc := range cases {
		got := tc.ch.draft().Billable
		if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
			t.Errorf("%s: billable %v, want %v", tc.name, deref(got), deref(tc.want))
		}
	}

	d := Change{From: from, SetAttributes: []AttributeValue{{"_Category_", "mtg"}}, ClearAttributes: []string{"_Note_"}}.draft()
	if !reflect.DeepEqual(d.Attributes, []AttributeValue{{"_Category_", "mtg"}}) {
		t.Errorf("attributes %v", d.Attributes)
	}
	moved := Change{From: from, Issue: &Issue{Key: "APP-7"}, Start: &Start{Date: mustDate(t, "2026-11-11")}}.draft()
	if moved.Issue != (Issue{Key: "APP-7"}) || moved.Start.Time.IsSet() || moved.Description != "Before" || moved.Author != "acc-user1" {
		t.Errorf("moved %+v", moved)
	}
	body := bodyOf(Change{From: from, ClearAttributes: []string{"_Category_", "_Note_"}}.draft(), 10001)
	if body.Attributes == nil || len(body.Attributes) != 0 {
		t.Errorf("clearing every attribute must send an empty list, got %#v", body.Attributes)
	}
}

func deref(d *time.Duration) any {
	if d == nil {
		return nil
	}
	return *d
}

func TestDelete(t *testing.T) {
	f := tempotest.NewServer(t, testToken)
	c := testClient(t, f, newIssues())
	f.On("DELETE", "/4/worklogs/71", nil, "", 204, "")
	f.On("DELETE", "/4/worklogs/72", nil, "", 404, `{"errors":{"message":"Worklog cannot be found"}}`)
	ctx := context.Background()
	if err := c.Delete(ctx, 71); err != nil {
		t.Fatal(err)
	}
	err := c.Delete(ctx, 72)
	if !errors.Is(err, trackerapi.ErrNotFound) || !trackerapi.WriteNotApplied(err) {
		t.Fatalf("missing worklog: %v", err)
	}
	if err := c.Delete(ctx, -1); !isType[*InvalidError](err) {
		t.Fatalf("bad ID: %v", err)
	}
}

// TestUpdateNotMoved: an update that names another issue and comes back
// stored on the old one is neither applied nor a refusal.
func TestUpdateNotMoved(t *testing.T) {
	f := tempotest.NewServer(t, testToken)
	c := testClient(t, f, newIssues())
	openWeek(f)
	from := Worklog{ID: 70, Issue: Issue{ID: 10002, Key: "WEB-2"}, Author: "acc-user1", Start: Start{Date: mustDate(t, "2026-11-10")},
		Spent: time.Hour, Attributes: []AttributeValue{{"_Category_", "dev"}}}
	body := `{"authorAccountId":"acc-user1","issueId":10001,"startDate":"2026-11-10","timeSpentSeconds":3600,"description":"","attributes":[{"key":"_Category_","value":"dev"}]}`
	for _, id := range []int64{70, 71} {
		f.On("GET", "/4/worklogs/"+strconv.FormatInt(id, 10), nil, "", 200, wl{id: id, issue: 10002, author: "acc-user1", date: "2026-11-10", spent: 3600, attrs: [][2]string{{"_Category_", "dev"}}}.json())
	}
	f.On("PUT", "/4/worklogs/70", nil, body, 200, wl{id: 70, issue: 10002, author: "acc-user1", date: "2026-11-10", spent: 3600}.json())
	f.On("PUT", "/4/worklogs/71", nil, body, 200, wl{id: 71, issue: 10001, author: "acc-user1", date: "2026-11-10", spent: 3600}.json())
	out := c.Apply(context.Background(), []Op{
		UpdateOp(Change{From: from, Issue: &Issue{Key: "WEB-1"}}),
		UpdateOp(Change{From: func() Worklog { w := from; w.ID = 71; return w }(), Issue: &Issue{Key: "WEB-1"}}),
	})
	kept := out[0]
	notMoved, ok := errors.AsType[*NotMovedError](kept.Err)
	if kept.Status != NotMoved || !ok || notMoved.Asked != 10001 || kept.Stored.Issue != (Issue{ID: 10002, Key: "WEB-2"}) ||
		trackerapi.WriteNotApplied(kept.Err) || len(kept.Problems) != 1 || kept.Problems[0].Code != IssueKept {
		t.Errorf("kept on the old issue: %+v", kept)
	}
	if out[1].Status != Applied || out[1].Stored.Issue != (Issue{ID: 10001, Key: "WEB-1"}) {
		t.Errorf("moved: %+v", out[1])
	}
}

func TestWriteRedirectIsUnknown(t *testing.T) {
	f := tempotest.NewServer(t, testToken)
	c := testClient(t, f, newIssues())
	e := f.On("DELETE", "/4/worklogs/73", nil, "", 302, "")
	e.Header = map[string][]string{"Location": {f.URL() + "/4/worklogs/74"}}
	out := c.Apply(context.Background(), []Op{DeleteOp(73)})[0]
	if se, ok := errors.AsType[*trackerapi.StatusError](out.Err); out.Status != Unknown || !ok || se.Status != 302 || trackerapi.WriteNotApplied(out.Err) {
		t.Fatalf("%+v", out)
	}
}
