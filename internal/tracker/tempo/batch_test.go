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

	"agent-overflow/internal/tracker/tempo/tempotest"
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
	f := tempotest.NewServer(t, testToken)
	issues := newIssues()
	c := testClient(t, f, issues)
	openWeek(f)
	record := Worklog{ID: 70, Issue: Issue{ID: 10002, Key: "WEB-2"}, Author: "acc-user1", Start: Start{Date: mustDate(t, "2026-11-10")},
		Spent: time.Hour, Attributes: []AttributeValue{{"_Category_", "dev"}}}
	f.On("POST", "/4/worklogs", nil, createBody(10001, "first"), 200, wl{id: 101, issue: 10001, author: "acc-user1", date: "2026-11-10", spent: 3600, desc: "first"}.json())
	f.On("PUT", "/4/worklogs/70", nil,
		`{"authorAccountId":"acc-user1","issueId":10002,"startDate":"2026-11-10","timeSpentSeconds":5400,"description":"","attributes":[{"key":"_Category_","value":"dev"}]}`,
		400, `{"errors":[{"message":"Period is closed"}]}`)
	f.On("POST", "/4/worklogs", nil, createBody(20007, "lost"), 500, `{"errors":[{"message":"internal"}]}`)
	f.On("POST", "/4/worklogs", nil, createBody(10001, "after"), 200, wl{id: 102, issue: 10001, author: "acc-user1", date: "2026-11-10", spent: 3600, desc: "after"}.json())
	f.On("DELETE", "/4/worklogs/71", nil, "", 204, "")

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
	idCalls := issues.calls()
	if !reflect.DeepEqual(idCalls, [][]string{{"WEB-1", "WEB-2", "APP-7"}}) {
		t.Errorf("the resolver was asked %v, want one call for the batch", idCalls)
	}
	writes := slices.DeleteFunc(f.Requests(), func(r string) bool { return r[:3] == "GET" })
	if want := []string{"POST /4/worklogs", "PUT /4/worklogs/70", "POST /4/worklogs", "POST /4/worklogs", "DELETE /4/worklogs/71"}; !slices.Equal(writes, want) {
		t.Errorf("writes %v, want %v", writes, want)
	}
}

func TestApplyStopsWhenCancelled(t *testing.T) {
	f := tempotest.NewServer(t, testToken)
	c := testClient(t, f, newIssues())
	openWeek(f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.On("POST", "/4/worklogs", nil, createBody(10001, "one"), 200, wl{id: 1, issue: 10001, author: "acc-user1", date: "2026-11-10", spent: 3600}.json()).During = cancel
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
	f := tempotest.NewServer(t, testToken)
	c := testClient(t, f, newIssues())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, o := range c.Apply(ctx, []Op{CreateOp(weekDraft(t, "WEB-1", "x")), DeleteOp(3)}) {
		if o.Status != NotAttempted {
			t.Errorf("%+v", o)
		}
	}
}

// TestApplyResolverFailure: a failed IssueIDs call refuses, unsent, only
// the ops that name an issue by key; an op naming its issue by ID is
// checked in full and sent.
func TestApplyResolverFailure(t *testing.T) {
	f := tempotest.NewServer(t, testToken)
	issues := newIssues()
	issues.err = errors.New("jira is down")
	c := testClient(t, f, issues)
	openWeek(f)
	f.On("POST", "/4/worklogs", nil, createBody(10002, "y"), 200, stored(81))
	byID := weekDraft(t, "", "y")
	byID.Issue = Issue{ID: 10002}
	out := c.Apply(context.Background(), []Op{CreateOp(weekDraft(t, "WEB-1", "x")), CreateOp(byID)})
	if out[0].Status != Refused || !errors.Is(out[0].Err, issues.err) || !trackerapi.WriteNotApplied(out[0].Err) {
		t.Errorf("by key: %+v", out[0])
	}
	if out[1].Status != Applied {
		t.Errorf("by ID: %+v", out[1])
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
	f := tempotest.NewServer(t, testToken)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts, err := transportOptions(Options{BaseURL: f.URL(), Version: "test", Credentials: cancellingSource{cancel}, Issues: newIssues()})
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

// closingSource closes the Client while the client reads the credential
// for a write that passed admission, so the write is never sent.
type closingSource struct {
	c      **Client
	closed chan struct{}
}

func (s closingSource) Credential(context.Context) (*trackerapi.Credential, error) {
	c := *s.c
	go func() {
		c.Close()
		close(s.closed)
	}()
	<-c.life.Done()
	return nil, errors.New("no credential")
}

// TestApplyClosedWhileWaitingToSend: a write Close stops before it is
// sent is NotAttempted, as the Close contract says, not Refused.
func TestApplyClosedWhileWaitingToSend(t *testing.T) {
	f := tempotest.NewServer(t, testToken)
	var c *Client
	src := closingSource{c: &c, closed: make(chan struct{})}
	opts, err := transportOptions(Options{BaseURL: f.URL(), Version: "test", Credentials: src, Issues: newIssues()})
	if err != nil {
		t.Fatal(err)
	}
	opts.MaxPerSecond = 0
	if c, err = newClient(opts, newIssues()); err != nil {
		t.Fatal(err)
	}
	out := c.Apply(context.Background(), []Op{DeleteOp(9)})[0]
	<-src.closed
	if out.Status != NotAttempted || !trackerapi.WriteNotApplied(out.Err) {
		t.Fatalf("%+v", out)
	}
}

func TestApplyStoredRecordUnreadable(t *testing.T) {
	f := tempotest.NewServer(t, testToken)
	c := testClient(t, f, newIssues())
	openWeek(f)
	var sent atomic.Int32
	f.On("POST", "/4/worklogs", nil, createBody(10001, "x"), 200, wl{id: 5, issue: 10001, author: "acc-user1", date: "10/11/2026", spent: 3600}.json()).During = func() { sent.Add(1) }
	out := c.Apply(context.Background(), []Op{CreateOp(weekDraft(t, "WEB-1", "x"))})[0]
	if out.Status != Unknown || trackerapi.WriteNotApplied(out.Err) || sent.Load() != 1 {
		t.Fatalf("%+v, sent %d", out, sent.Load())
	}
}
