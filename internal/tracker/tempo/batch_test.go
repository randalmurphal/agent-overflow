package tempo

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/tracker/trackerapi"
)

func weekDraft(t *testing.T, key, desc string) Draft {
	return Draft{
		Issue: Issue{Key: key}, Author: "acc-user1",
		Start: Start{Date: mustDate(t, "2026-11-10")}, Spent: time.Hour, Description: desc,
		Attributes: []AttributeValue{{"_Category_", "dev"}},
	}
}

func createBody(issue int64, desc string) string {
	return `{"authorAccountId":"acc-user1","issueId":` + strconv.FormatInt(issue, 10) + `,"startDate":"2026-11-10","timeSpentSeconds":3600,"description":"` + desc +
		`","attributes":[{"key":"_Category_","value":"dev"}]}`
}

func TestApplyOutcomes(t *testing.T) {
	f := newFake(t)
	issues := newIssues()
	c := testClient(t, f, issues)
	openWeek(f)
	record := Worklog{ID: 70, Issue: Issue{ID: 10002, Key: "WEB-2"}, Author: "acc-user1", Start: Start{Date: mustDate(t, "2026-11-10")},
		Spent: time.Hour, Attributes: []AttributeValue{{"_Category_", "dev"}}}
	f.on("POST", "/4/worklogs", nil, createBody(10001, "first"), 200, wl{id: 101, issue: 10001, author: "acc-user1", date: "2026-11-10", spent: 3600, desc: "first"}.json())
	f.on("PUT", "/4/worklogs/70", nil,
		`{"authorAccountId":"acc-user1","issueId":10002,"startDate":"2026-11-10","timeSpentSeconds":5400,"description":"","attributes":[{"key":"_Category_","value":"dev"}]}`,
		400, `{"errors":[{"message":"Period is closed"}]}`)
	f.on("POST", "/4/worklogs", nil, createBody(20007, "lost"), 500, `{"errors":[{"message":"internal"}]}`)
	f.on("POST", "/4/worklogs", nil, createBody(10001, "after"), 200, wl{id: 102, issue: 10001, author: "acc-user1", date: "2026-11-10", spent: 3600, desc: "after"}.json())
	f.on("DELETE", "/4/worklogs/71", nil, "", 204, "")

	bad := weekDraft(t, "WEB-2", "bad")
	bad.Attributes = []AttributeValue{{"_Category_", "lunch"}}
	out := c.Apply(context.Background(), []Op{
		CreateOp(weekDraft(t, "WEB-1", "first")),
		CreateOp(bad),
		UpdateOp(Change{From: record, Spent: ptr(90 * time.Minute)}),
		CreateOp(weekDraft(t, "APP-7", "lost")),
		CreateOp(weekDraft(t, "WEB-1", "after")),
		DeleteOp(71),
	})

	statuses := make([]Status, len(out))
	for i, o := range out {
		statuses[i] = o.Status
	}
	if want := []Status{Applied, Refused, Refused, Unknown, Applied, Applied}; !slices.Equal(statuses, want) {
		t.Fatalf("statuses %v, want %v", statuses, want)
	}
	if out[0].Stored.ID != 101 || out[0].Stored.Issue.Key != "WEB-1" || out[4].Stored.ID != 102 {
		t.Errorf("stored %+v / %+v", out[0].Stored, out[4].Stored)
	}
	if kinds(out[1].Problems)[0] != (want{FieldAttribute, "_Category_", NotAllowed}) || !isType[*InvalidError](out[1].Err) || !trackerapi.WriteNotApplied(out[1].Err) {
		t.Errorf("checked refusal %+v", out[1])
	}
	if !reflect.DeepEqual(out[2].Problems, []Problem{{Code: TempoRefused, Message: "Period is closed"}}) || !trackerapi.WriteNotApplied(out[2].Err) {
		t.Errorf("Tempo's refusal %+v", out[2])
	}
	if se, ok := errors.AsType[*trackerapi.StatusError](out[3].Err); !ok || se.Status != 500 || trackerapi.WriteNotApplied(out[3].Err) {
		t.Errorf("unknown outcome %+v", out[3])
	}
	for i, o := range out {
		if (o.Err == nil) != (o.Status == Applied) {
			t.Errorf("op %d: status %v with err %v", i, o.Status, o.Err)
		}
	}
	idCalls, _ := issues.calls()
	if !reflect.DeepEqual(idCalls, [][]string{{"WEB-1", "WEB-2", "APP-7"}}) {
		t.Errorf("the resolver was asked %v, want one call for the batch", idCalls)
	}
	writes := slices.DeleteFunc(f.requests(), func(r string) bool { return r[:3] == "GET" })
	if want := []string{"POST /4/worklogs", "PUT /4/worklogs/70", "POST /4/worklogs", "POST /4/worklogs", "DELETE /4/worklogs/71"}; !slices.Equal(writes, want) {
		t.Errorf("writes %v, want %v", writes, want)
	}
}

func TestApplyStopsWhenCancelled(t *testing.T) {
	f := newFake(t)
	c := testClient(t, f, newIssues())
	openWeek(f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.on("POST", "/4/worklogs", nil, createBody(10001, "one"), 200, wl{id: 1, issue: 10001, author: "acc-user1", date: "2026-11-10", spent: 3600}.json()).during = cancel
	refused := weekDraft(t, "WEB-1", "refused")
	refused.Spent = 0
	out := c.Apply(ctx, []Op{
		CreateOp(weekDraft(t, "WEB-1", "one")),
		CreateOp(weekDraft(t, "WEB-1", "two")),
		CreateOp(refused),
		DeleteOp(9),
	})
	// The first was sent and its answer never read: unknown, not resent.
	if out[0].Status != Unknown || trackerapi.WriteNotApplied(out[0].Err) || !errors.Is(out[0].Err, context.Canceled) {
		t.Errorf("in flight: %+v", out[0])
	}
	for _, o := range out[1:] {
		if o.Status != NotAttempted || !trackerapi.WriteNotApplied(o.Err) || !errors.Is(o.Err, context.Canceled) {
			t.Errorf("after the cancel: %+v", o)
		}
	}
}

func TestApplyCancelledBeforeChecks(t *testing.T) {
	f := newFake(t)
	c := testClient(t, f, newIssues())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, o := range c.Apply(ctx, []Op{CreateOp(weekDraft(t, "WEB-1", "x")), DeleteOp(3)}) {
		if o.Status != NotAttempted {
			t.Errorf("%+v", o)
		}
	}
}

func TestApplyCheckReadFailure(t *testing.T) {
	f := newFake(t)
	c := testClient(t, f, newIssues())
	f.on("GET", "/4/work-attributes", q("offset", "0", "limit", "1000"), "", 403, `<html>Forbidden</html>`)
	f.on("DELETE", "/4/worklogs/3", nil, "", 204, "")
	out := c.Apply(context.Background(), []Op{CreateOp(weekDraft(t, "WEB-1", "x")), DeleteOp(3)})
	if out[0].Status != Refused || !isType[*trackerapi.StatusError](out[0].Err) || !trackerapi.WriteNotApplied(out[0].Err) {
		t.Errorf("create without its checks: %+v", out[0])
	}
	if out[1].Status != Applied {
		t.Errorf("delete: %+v", out[1])
	}
}

func TestApplyResolverFailure(t *testing.T) {
	f := newFake(t)
	issues := newIssues()
	issues.err = errors.New("jira is down")
	c := testClient(t, f, issues)
	out := c.Apply(context.Background(), []Op{CreateOp(weekDraft(t, "WEB-1", "x"))})
	if out[0].Status != Refused || !errors.Is(out[0].Err, issues.err) || !trackerapi.WriteNotApplied(out[0].Err) {
		t.Errorf("%+v", out[0])
	}
}

// cancellingSource cancels the call's context while the client reads the
// credential, so the write is never sent.
type cancellingSource struct{ cancel context.CancelFunc }

func (s cancellingSource) Credential(context.Context) (*trackerapi.Credential, error) {
	s.cancel()
	return nil, errors.New("no credential")
}

func TestApplyCancelledWhileWaitingToSend(t *testing.T) {
	f := newFake(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts, err := transportOptions(Options{BaseURL: f.srv.URL, Version: "test", Credentials: cancellingSource{cancel}, Issues: newIssues()})
	if err != nil {
		t.Fatal(err)
	}
	opts.MaxPerSecond = 0
	c, err := newClient(opts, newIssues())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	out := c.Apply(ctx, []Op{DeleteOp(9)})[0]
	if out.Status != NotAttempted || !trackerapi.WriteNotApplied(out.Err) {
		t.Fatalf("%+v", out)
	}
}

func TestApplyStoredRecordUnreadable(t *testing.T) {
	f := newFake(t)
	c := testClient(t, f, newIssues())
	openWeek(f)
	var sent atomic.Int32
	f.on("POST", "/4/worklogs", nil, createBody(10001, "x"), 200, wl{id: 5, issue: 10001, author: "acc-user1", date: "10/11/2026", spent: 3600}.json()).during = func() { sent.Add(1) }
	out := c.Apply(context.Background(), []Op{CreateOp(weekDraft(t, "WEB-1", "x"))})[0]
	if out.Status != Unknown || trackerapi.WriteNotApplied(out.Err) || sent.Load() != 1 {
		t.Fatalf("%+v, sent %d", out, sent.Load())
	}
}
