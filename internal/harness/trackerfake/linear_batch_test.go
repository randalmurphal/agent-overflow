package trackerfake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// clockRig is a rig whose engine clock the test moves.
type clockRig struct {
	*rig
	mu  sync.Mutex
	now time.Time
}

func newClockRig(t *testing.T, f Fixture, start time.Time) *clockRig {
	t.Helper()
	c := &clockRig{now: start}
	g := &rig{e: New(Options{Now: c.clock}), h: map[Origin]http.Handler{}}
	t.Cleanup(g.e.Close)
	if err := g.e.Seed(f); err != nil {
		t.Fatal(err)
	}
	h, err := g.e.Handler(linearOrigin)
	if err != nil {
		t.Fatal(err)
	}
	g.h[linearOrigin] = h
	c.rig = g
	return c
}

func (c *clockRig) clock() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clockRig) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *clockRig) issue(t *testing.T, id string) LinearIssue {
	t.Helper()
	for _, is := range c.e.Snapshot().Linear[0].Issues {
		if is.ID == id {
			return is
		}
	}
	t.Fatalf("no issue %s", id)
	return LinearIssue{}
}

// issueBatchUpdate applies its input to every issue, each stamped with
// its own update time (measured: three issues, three distinct times);
// 51 IDs answer INVALID_INPUT and a missing ID answers INPUT_ERROR, both
// changing nothing (measured).
func TestLinearBatchUpdate(t *testing.T) {
	g := newClockRig(t, txnFixture(), testStart.Add(time.Hour))
	before, err := json.Marshal(g.e.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, linearMaxBatch+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("%q", "i1")
	}
	for name, c := range map[string]struct{ ids, code string }{
		"51 IDs":     {strings.Join(ids, ", "), "INVALID_INPUT"},
		"missing ID": {`"i1", "missing"`, "INPUT_ERROR"},
	} {
		rec := g.do(linearReq("M", `mutation M { issueBatchUpdate(ids: [`+c.ids+`], input: {title: "Changed"}) { success } }`))
		after, err := json.Marshal(g.e.Snapshot())
		if err != nil {
			t.Fatal(err)
		}
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), c.code) || string(after) != string(before) {
			t.Errorf("%s: answered %d %s", name, rec.Code, rec.Body)
		}
	}
	rec := g.do(linearReq("M", `mutation M { issueBatchUpdate(ids: ["i1", "i2"], input: {title: "Changed"}) { success issues { id title updatedAt } } }`))
	if rec.Code != 200 || strings.Contains(rec.Body.String(), `"errors"`) {
		t.Fatalf("answered %d %s", rec.Code, rec.Body)
	}
	one, two := g.issue(t, "i1"), g.issue(t, "i2")
	if one.Title != "Changed" || two.Title != "Changed" || !one.UpdatedAt.Before(two.UpdatedAt) {
		t.Errorf("stored %+v and %+v", one, two)
	}
	// The clock never moves back: the next write is stamped no earlier
	// than the batch's last.
	g.do(linearReq("M", `mutation M { issueUpdate(id: "i1", input: {title: "Again"}) { success } }`))
	if again := g.issue(t, "i1"); again.UpdatedAt.Before(two.UpdatedAt) {
		t.Errorf("the next write stamped %v, the batch's last %v", again.UpdatedAt, two.UpdatedAt)
	}
}

// A page cursor follows its issue (measured): continuing an
// oldest-update-first walk after an issue that was updated since skips
// every issue between its old place and its new one.
func TestLinearCursorFollowsItsIssue(t *testing.T) {
	g := newClockRig(t, Fixture{Linear: []LinearWorkspace{pagingLinear()}}, testStart.Add(time.Hour))
	page := func(after string) string {
		args := "first: 1"
		if after != "" {
			args += `, after: "` + after + `"`
		}
		return g.ok(t, linearReq("P", `query P { issues(sort: [{updatedAt: {order: Ascending}}], `+args+`) { nodes { id } pageInfo { hasNextPage endCursor } } }`))
	}
	first := page("")
	if got := ids(t, first, "data.issues.nodes[].id"); len(got) != 1 || got[0] != "i1" {
		t.Fatalf("first page %v", got)
	}
	cursor := ids(t, first, "data.issues.pageInfo.endCursor")[0]
	g.ok(t, linearReq("M", `mutation M { issueUpdate(id: "i1", input: {title: "Moved"}) { success } }`))
	rest := page(cursor)
	if got := ids(t, rest, "data.issues.nodes[].id"); len(got) != 0 || ids(t, rest, "data.issues.pageInfo.hasNextPage")[0] != "false" {
		t.Errorf("after the moved issue: %s", rest)
	}
}

// Every answer's Date is the engine clock; an issue lists hide until
// the clock reaches its ShownAt, while a read by ID finds it.
func TestLinearDateAndShownAt(t *testing.T) {
	f := txnFixture()
	start := testStart.Add(time.Hour)
	f.Linear[0].Issues[1].ShownAt = start.Add(time.Minute)
	g := newClockRig(t, f, start)
	list := func() []string {
		rec := g.do(linearReq("P", `query P { issues { nodes { id } } }`))
		if got := rec.Header().Get("Date"); got != g.clock().UTC().Format(http.TimeFormat) {
			t.Errorf("Date %q at %v", got, g.clock())
		}
		return ids(t, rec.Body.String(), "data.issues.nodes[].id")
	}
	if got := list(); len(got) != 1 || got[0] != "i1" {
		t.Errorf("before ShownAt: %v", got)
	}
	if body := g.ok(t, linearReq("P", `query P { issue(id: "i2") { id } }`)); !strings.Contains(body, `"i2"`) {
		t.Errorf("read by ID: %s", body)
	}
	g.advance(time.Minute)
	if got := list(); len(got) != 2 {
		t.Errorf("at ShownAt: %v", got)
	}
}

// A move to another team gives an issue the new team's next identifier;
// issue(id:) still answers the old one, an identifier filter does not
// match it, and a seed naming a former identifier another issue holds is
// refused.
func TestLinearFormerIdentifier(t *testing.T) {
	f := txnFixture()
	f.Linear[0].Teams = append(f.Linear[0].Teams, LinearTeam{ID: "t2", Key: "APP", Name: "App"})
	f.Linear[0].States = append(f.Linear[0].States, LinearState{ID: "s2", Team: "t2", Name: "Todo", Type: "unstarted"})
	f.Linear[0].Teams[1].DefaultState = "s2"
	g := newClockRig(t, f, testStart.Add(time.Hour))
	g.ok(t, linearReq("M", `mutation M { issueUpdate(id: "WEB-1", input: {teamId: "t2"}) { success } }`))
	if is := g.issue(t, "i1"); !slices.Equal(is.FormerKeys, []string{"WEB-1"}) {
		t.Errorf("former keys %v", is.FormerKeys)
	}
	for _, key := range []string{"WEB-1", "web-1", "APP-1"} {
		body := g.ok(t, linearReq("P", `query P { issue(id: "`+key+`") { id identifier } }`))
		if got := ids(t, body, "data.issue.identifier"); len(got) != 1 || got[0] != "APP-1" {
			t.Errorf("issue(id: %s): %s", key, body)
		}
	}
	body := g.ok(t, linearReq("P", `query P { issues(filter: {team: {key: {eqIgnoreCase: "WEB"}}, number: {in: [1]}}) { nodes { id } } }`))
	if got := ids(t, body, "data.issues.nodes[].id"); len(got) != 0 {
		t.Errorf("a filter by the former identifier matched %v", got)
	}
	bad := txnFixture()
	bad.Linear[0].Issues[1].FormerKeys = []string{"WEB-1"}
	e := New(Options{})
	defer e.Close()
	if err := e.Seed(bad); err == nil {
		t.Error("a former identifier another issue holds was seeded")
	}
}
