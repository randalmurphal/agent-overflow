package jira

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"agent-overflow/internal/tracker/trackerapi"
)

// TestEmptyAnswerWrites proves each write documented to answer with no
// body is applied only on its documented status: any other 2xx is an
// unknown outcome, not Applied and not refused.
func TestEmptyAnswerWrites(t *testing.T) {
	cases := []struct {
		name, method, path, query, body string
		documented                      []int
		call                            func(*Client) error
	}{
		{"LinkIssues", "POST", "/rest/api/3/issueLink", "", `{"type":{"id":"10000"},"outwardIssue":{"key":"WEB-1"},"inwardIssue":{"key":"WEB-2"}}`, []int{201},
			func(c *Client) error {
				return c.LinkIssues(t.Context(), LinkInput{Type: "Blocks", Outward: "WEB-1", Inward: "WEB-2"})
			}},
		{"UnlinkIssues", "DELETE", "/rest/api/3/issueLink/10", "", "", []int{200, 204},
			func(c *Client) error { return c.UnlinkIssues(t.Context(), "10") }},
		{"RemoveRemoteLink", "DELETE", "/rest/api/3/issue/WEB-1/remotelink/10000", "", "", []int{204},
			func(c *Client) error { return c.RemoveRemoteLink(t.Context(), "WEB-1", "10000") }},
		{"AddWatcher", "POST", "/rest/api/3/issue/WEB-1/watchers", "", `"acc-user1"`, []int{204},
			func(c *Client) error { return c.AddWatcher(t.Context(), "WEB-1", "acc-user1") }},
		{"RemoveWatcher", "DELETE", "/rest/api/3/issue/WEB-1/watchers", "accountId=acc-user1", "", []int{204},
			func(c *Client) error { return c.RemoveWatcher(t.Context(), "WEB-1", "acc-user1") }},
		{"Assign", "PUT", "/rest/api/3/issue/WEB-1/assignee", "", `{"accountId":"acc-user1"}`, []int{204},
			func(c *Client) error { return c.Assign(t.Context(), "WEB-1", "acc-user1") }},
		{"Transition", "POST", "/rest/api/3/issue/WEB-1/transitions", "", `{"transition":{"id":"21"}}`, []int{204},
			func(c *Client) error {
				_, err := c.Transition(t.Context(), TransitionInput{Issue: "WEB-1", Transition: "Start"})
				return err
			}},
		{"DeleteComment", "DELETE", "/rest/api/3/issue/WEB-1/comment/900", "", "", []int{204},
			func(c *Client) error { return c.DeleteComment(t.Context(), "WEB-1", "900") }},
		{"DeleteAttachment", "DELETE", "/rest/api/3/attachment/501", "", "", []int{204},
			func(c *Client) error { return c.DeleteAttachment(t.Context(), "501") }},
		{"DeleteWorklog", "DELETE", "/rest/api/3/issue/WEB-1/worklog/300", "adjustEstimate=auto", "", []int{204},
			func(c *Client) error { return c.DeleteWorklog(t.Context(), "WEB-1", "300", AdjustAuto()) }},
		{"DeleteSprint", "DELETE", "/rest/agile/1.0/sprint/7", "", "", []int{204},
			func(c *Client) error { return c.DeleteSprint(t.Context(), 7) }},
	}
	for _, tc := range cases {
		undocumented := http.StatusOK
		if tc.documented[0] == http.StatusOK {
			undocumented = http.StatusAccepted
		}
		for _, status := range append([]int{undocumented}, tc.documented...) {
			f := newFake(t)
			c := newTestClient(t, f)
			f.on("GET", "/rest/api/3/issueLinkType", reply(200, `{"issueLinkTypes":[{"id":"10000","name":"Blocks","inward":"is blocked by","outward":"blocks"}]}`))
			f.on("GET", "/rest/api/3/issue/WEB-1/transitions", reply(200, transitionsJSON))
			f.on(tc.method, tc.path, func(w http.ResponseWriter, r *http.Request, b []byte) {
				if r.URL.RawQuery != tc.query {
					t.Errorf("%s: query %q", tc.name, r.URL.RawQuery)
				}
				if tc.body == "" && len(b) != 0 {
					t.Errorf("%s: a body %s", tc.name, b)
				} else if tc.body != "" {
					wantJSON(t, b, tc.body)
				}
				w.WriteHeader(status)
			})
			err := tc.call(c)
			if status != undocumented {
				if err != nil {
					t.Errorf("%s answered %d: %v", tc.name, status, err)
				}
				continue
			}
			ae, ok := errors.AsType[*AnswerError](err)
			if !ok || ae.Applied || trackerapi.WriteNotApplied(err) {
				t.Errorf("%s answered %d: %v", tc.name, status, err)
			}
			if se, ok := errors.AsType[*StepError](err); ok && !se.OutcomeUnknown() {
				t.Errorf("%s answered %d: %v", tc.name, status, err)
			}
		}
	}
}

// TestEditAnswers proves an issue edit is applied on a 204, or on a 200
// naming the issue (with what decoded when the rest does not), and any
// other answer is an unknown outcome that reports no issue.
func TestEditAnswers(t *testing.T) {
	for _, tc := range []struct {
		status        int
		body          string
		issue         bool
		key           string // of the issue reported
		applied, fail bool
	}{
		{204, ``, false, "", false, false},
		{200, `{"id":"10001","key":"WEB-1","fields":{"description":null}}`, true, "WEB-1", false, false},
		{200, `{"id":"10001","key":"WEB-1","fields":"none"}`, true, "WEB-1", true, true},
		{200, `{"id":10001,"key":"WEB-1","fields":{}}`, true, "WEB-1", true, true},
		{200, `{"id":10001,"key":5,"fields":{}}`, true, "", true, true},
		{200, `{"id":"10002","key":"WEB-2","fields":{}}`, false, "", false, true},
		{200, `<html>saved</html>`, false, "", false, true},
		{201, `{"id":"10001","key":"WEB-1","fields":{}}`, false, "", false, true},
	} {
		desc := storedDesc(t, nil)
		f, c := editSite(t, desc)
		plan, err := c.PlanEdit(t.Context(), EditInput{Issue: "WEB-1", Fields: []FieldInput{{Field: "Description", Value: Text("new")}}})
		if err != nil {
			t.Fatal(err)
		}
		f.on("PUT", "/rest/api/3/issue/10001", func(w http.ResponseWriter, r *http.Request, b []byte) {
			wantQuery(t, r, "returnIssue", "true")
			wantJSON(t, b, `{"fields":{"description":{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"new"}]}]}}}`)
			if tc.status == http.StatusNoContent {
				w.WriteHeader(tc.status)
				return
			}
			reply(tc.status, tc.body)(w, r, b)
		})
		res, err := c.ApplyEdit(t.Context(), plan)
		if (err != nil) != tc.fail || (res.Issue != nil) != tc.issue {
			t.Errorf("%d %s: got %+v %v", tc.status, tc.body, res, err)
			continue
		}
		if tc.issue && (res.Issue.ID != "10001" || res.Issue.Key != tc.key) {
			t.Errorf("%d %s: the issue %+v", tc.status, tc.body, res.Issue)
		}
		if err == nil {
			continue
		}
		se, ok := errors.AsType[*StepError](err)
		if !ok || se.Step != StepEdit || se.Applied() != tc.applied || se.OutcomeUnknown() == tc.applied {
			t.Errorf("%d %s: %v", tc.status, tc.body, err)
		}
	}
}

// TestRankAnswerReadFailsOnTheCredential drives the transport's
// multi-error: a rank answered 200 whose body read fails on a trailer
// that repeats the request's Authorization header. The transport replaces
// that cause with a scrubbed error unwrapping to several sentinels, under
// a transient failure; the group's outcome is unknown, the batch stops,
// and no error text repeats the credential.
func TestRankAnswerReadFailsOnTheCredential(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("PUT", rankURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantJSON(t, b, `{"issues":["`+strings.Join(webIssues(1, 50), `","`)+`"],"rankBeforeIssue":"WEB-500"}`)
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer func() {
			if err := conn.Close(); err != nil {
				t.Errorf("close: %v", err)
			}
		}()
		answer := "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n2\r\n{}\r\n0\r\n" + r.Header.Get("Authorization") + "\r\n\r\n"
		if _, err := rw.WriteString(answer); err != nil {
			t.Errorf("write: %v", err)
		}
		if err := rw.Flush(); err != nil {
			t.Errorf("flush: %v", err)
		}
	})
	res, err := c.Rank(t.Context(), RankInput{Issues: webIssues(1, 60), Position: RankBefore("WEB-500")})
	be, ok := errors.AsType[*BatchError](err)
	if _, transient := errors.AsType[*trackerapi.TransientError](err); !ok || !transient || be.Unknown != 50 || be.NotSent != 10 || trackerapi.WriteNotApplied(err) {
		t.Fatalf("got %+v %v", be, err)
	}
	if res.Issues[0].Outcome != OutcomeUnknown || neverSent(err) || answerApplied(err) || outcomeOf(err) != nil {
		t.Errorf("outcome %v, read as %v", res.Issues[0].Outcome, outcomeOf(err))
	}
	joined := false
	for stack := []error{err}; len(stack) > 0; {
		e := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		noToken(t, "the error", e.Error())
		switch u := e.(type) {
		case interface{ Unwrap() error }:
			if inner := u.Unwrap(); inner != nil {
				stack = append(stack, inner)
			}
		case interface{ Unwrap() []error }:
			joined = true
			stack = append(stack, u.Unwrap()...)
		}
	}
	if !joined {
		t.Error("the transport's error holds no multi-error, so this test proves nothing about walking one")
	}
	if n := f.count("PUT", rankURL); n != 1 {
		t.Errorf("%d groups sent", n)
	}
}

// TestOutcomeOfWalksMultiErrors proves the walk reads a multi-error as
// errors.As does: an outcome held by one of its errors is found, in
// order, so a write's outcome agrees with trackerapi.WriteNotApplied for
// any error shape. The transport's own multi-error holds only sentinels
// (see TestRankAnswerReadFailsOnTheCredential), so this shape is built by
// hand.
func TestOutcomeOfWalksMultiErrors(t *testing.T) {
	notSent := &trackerapi.NotSentError{Err: io.EOF}
	err := fmt.Errorf("jira: rank: %w", errors.Join(io.EOF, notSent, &AnswerError{Applied: true}))
	if got := outcomeOf(err); got != notSent || !neverSent(err) || answerApplied(err) || !trackerapi.WriteNotApplied(err) {
		t.Errorf("got %v", got)
	}
	if got := outcomeOf(fmt.Errorf("x: %w", errors.Join(io.EOF, context.Canceled))); got != nil {
		t.Errorf("sentinels read as %v", got)
	}
}
