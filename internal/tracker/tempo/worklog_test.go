package tempo

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/tracker/trackerapi"
)

func TestWorklogPagesToTheEnd(t *testing.T) {
	f := newFake(t)
	issues := newIssues()
	c := testClient(t, f, issues)
	base := q("from", "2026-11-09", "to", "2026-11-15", "limit", "1000")
	at := func(offset string) map[string][]string {
		v := q("offset", offset)
		for k, vs := range base {
			v[k] = vs
		}
		return v
	}
	f.on("GET", "/4/worklogs/user/acc-user1", at("0"), "", 200, pageOf(0, true,
		wl{id: 1, issue: 10001, author: "acc-user1", date: "2026-11-09", start: "09:00:00", spent: 3600, billable: seconds64(3600), desc: "Review", attrs: [][2]string{{"_Category_", "dev"}}}.json(),
		wl{id: 2, issue: 10001, author: "acc-user1", date: "2026-11-09", start: "10:00:00", spent: 1800}.json()))
	f.on("GET", "/4/worklogs/user/acc-user1", at("2"), "", 200, pageOf(2, true,
		wl{id: 3, issue: 10001, author: "acc-user1", date: "2026-11-10", spent: 900}.json(),
		wl{id: 4, issue: 99999, author: "acc-user1", date: "2026-11-10", start: "13:15:00", spent: 600}.json()))
	f.on("GET", "/4/worklogs/user/acc-user1", at("4"), "", 200, pageOf(4, false,
		wl{id: 5, issue: 20007, author: "acc-user1", date: "2026-11-11", start: "08:00:00", spent: 7200}.json()))

	var sizes []int
	var all []Worklog
	err := c.WorklogPages(context.Background(), WorklogQuery{Author: "acc-user1", From: mustDate(t, "2026-11-09"), To: mustDate(t, "2026-11-15")},
		func(page []Worklog) error {
			sizes = append(sizes, len(page))
			all = append(all, page...)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sizes, []int{2, 2, 1}) {
		t.Fatalf("pages %v", sizes)
	}
	_, keyCalls := issues.calls()
	if !reflect.DeepEqual(keyCalls, [][]int64{{10001}, {10001, 99999}, {20007}}) {
		t.Errorf("key lookups %v", keyCalls)
	}
	first := all[0]
	want := Worklog{
		ID: 1, Issue: Issue{ID: 10001, Key: "WEB-1"}, Author: "acc-user1",
		Start:    Start{Date: mustDate(t, "2026-11-09"), Time: mustClock(t, "09:00:00")},
		StartUTC: time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC),
		Spent:    time.Hour, Billable: ptr(time.Hour), Description: "Review",
		Attributes: []AttributeValue{{Key: "_Category_", Value: "dev"}},
		Created:    time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC),
		Updated:    time.Date(2026, 11, 3, 9, 30, 0, 0, time.UTC),
	}
	if !reflect.DeepEqual(first, want) {
		t.Errorf("first worklog\n got %+v\nwant %+v", first, want)
	}
	if all[2].Start.Time.IsSet() || all[2].Billable != nil {
		t.Errorf("worklog 3 without start time or billable: %+v", all[2])
	}
	if all[3].Issue.Key != "" {
		t.Errorf("an ID the resolver does not know got key %q", all[3].Issue.Key)
	}
}

func TestWorklogsFailureMidPage(t *testing.T) {
	f := newFake(t)
	c := testClient(t, f, newIssues())
	f.on("GET", "/4/worklogs/user/acc-user1", q("from", "2026-11-09", "to", "2026-11-09", "offset", "0", "limit", "1000"), "", 200,
		pageOf(0, true, wl{id: 1, issue: 10001, author: "acc-user1", date: "2026-11-09", spent: 60}.json()))
	f.on("GET", "/4/worklogs/user/acc-user1", q("from", "2026-11-09", "to", "2026-11-09", "offset", "1", "limit", "1000"), "", 400,
		`{"errors":[{"message":"offset out of range"}]}`)
	day := mustDate(t, "2026-11-09")
	var seen int
	err := c.WorklogPages(context.Background(), WorklogQuery{Author: "acc-user1", From: day, To: day}, func(p []Worklog) error {
		seen += len(p)
		return nil
	})
	se, ok := errors.AsType[*trackerapi.StatusError](err)
	if !ok || se.Message != "offset out of range" || seen != 1 {
		t.Fatalf("seen %d, err %v", seen, err)
	}
}

func TestWorklogsRefusesAStalledPage(t *testing.T) {
	f := newFake(t)
	c := testClient(t, f, newIssues())
	f.on("GET", "/4/worklogs/user/acc-user1", q("from", "2026-11-09", "to", "2026-11-09", "offset", "0", "limit", "1000"), "", 200,
		pageOf(0, true))
	day := mustDate(t, "2026-11-09")
	if _, err := c.Worklogs(context.Background(), WorklogQuery{Author: "acc-user1", From: day, To: day}); err == nil {
		t.Fatal("a page naming a next page with no results was accepted")
	}
}

func TestWorklogPagesCallbackErrorStops(t *testing.T) {
	f := newFake(t)
	c := testClient(t, f, newIssues())
	f.on("GET", "/4/worklogs/user/acc%2Fuser1", q("from", "2026-11-09", "to", "2026-11-09", "offset", "0", "limit", "1000", "updatedFrom", "2026-11-09T07:00:00Z"), "", 200,
		pageOf(0, true, wl{id: 1, issue: 10001, author: "acc/user1", date: "2026-11-09", spent: 60}.json()))
	day := mustDate(t, "2026-11-09")
	stop := errors.New("stop")
	err := c.WorklogPages(context.Background(), WorklogQuery{
		Author: "acc/user1", From: day, To: day,
		UpdatedFrom: time.Date(2026, 11, 9, 8, 0, 0, 0, time.FixedZone("CET", 3600)),
	}, func([]Worklog) error { return stop })
	if err != stop {
		t.Fatalf("err %v", err)
	}
}

func TestWorklogQueryChecks(t *testing.T) {
	f := newFake(t)
	c := testClient(t, f, newIssues())
	d1, d2 := mustDate(t, "2026-11-09"), mustDate(t, "2026-11-10")
	for _, query := range []WorklogQuery{
		{From: d1, To: d2},
		{Author: "acc-user1", To: d2},
		{Author: "acc-user1", From: d2, To: d1},
	} {
		if _, err := c.Worklogs(context.Background(), query); !isType[*trackerapi.NotSentError](err) {
			t.Errorf("%+v: %v", query, err)
		}
	}
}

func TestWorklogResolverFailure(t *testing.T) {
	f := newFake(t)
	issues := newIssues()
	issues.err = errors.New("jira is down")
	c := testClient(t, f, issues)
	f.on("GET", "/4/worklogs/42", nil, "", 200, wl{id: 42, issue: 10001, author: "acc-user1", date: "2026-11-09", spent: 60}.json())
	if _, err := c.Worklog(context.Background(), 42); !errors.Is(err, issues.err) {
		t.Fatalf("err %v", err)
	}
}

func TestWorklogRefusesABadRecord(t *testing.T) {
	f := newFake(t)
	c := testClient(t, f, newIssues())
	f.on("GET", "/4/worklogs/43", nil, "", 200, wl{id: 43, issue: 10001, author: "acc-user1", date: "09/11/2026", spent: 60}.json())
	if _, err := c.Worklog(context.Background(), 43); err == nil {
		t.Fatal("a worklog with a bad date was accepted")
	}
}
