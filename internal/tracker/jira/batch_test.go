package jira

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker/trackerapi"
)

const rankURL = "/rest/agile/1.0/issue/rank"

// webIssues is WEB-from to WEB-to.
func webIssues(from, to int) []string {
	var out []string
	for i := from; i <= to; i++ {
		out = append(out, fmt.Sprintf("WEB-%d", i))
	}
	return out
}

// batchBody is a rank or move request as the fake reads it.
type batchBody struct {
	Issues []string `json:"issues"`
	Before string   `json:"rankBeforeIssue"`
	After  string   `json:"rankAfterIssue"`
	Field  int64    `json:"rankCustomFieldId"`
}

// batchSite records each rank or move request and answers the nth (from
// 0) with answer(n, body).
type batchSite struct {
	t      *testing.T
	mu     sync.Mutex
	bodies []batchBody
	answer func(n int, b batchBody) (int, string)
}

func (s *batchSite) handle(w http.ResponseWriter, r *http.Request, raw []byte) {
	var b batchBody
	if err := json.Unmarshal(raw, &b); err != nil {
		s.t.Errorf("a batch body that is not JSON: %s", raw)
	}
	if len(b.Issues) > maxBatch {
		s.t.Errorf("a group of %d issues", len(b.Issues))
	}
	s.mu.Lock()
	n := len(s.bodies)
	s.bodies = append(s.bodies, b)
	s.mu.Unlock()
	status, body := s.answer(n, b)
	if status == http.StatusNoContent {
		w.WriteHeader(status)
		return
	}
	reply(status, body)(w, r, raw)
}

// multiStatus is a 207 answering issues, refusing those in refuse; key
// writes each entry's issueKey through it.
func multiStatus(issues []string, refuse map[string]bool, key func(string) string) string {
	var entries []string
	for _, is := range issues {
		status, errs := 204, `[]`
		if refuse[is] {
			status, errs = 400, `["The issue cannot be ranked"]`
		}
		entries = append(entries, fmt.Sprintf(`{"issueId":%s,"issueKey":%q,"status":%d,"errors":%s}`,
			strings.TrimPrefix(is, "WEB-"), key(is), status, errs))
	}
	return `{"entries":[` + strings.Join(entries, ",") + `]}`
}

// TestRankChainsGroups ranks 120 issues in three groups, each answered
// 207 with refusals: the first group goes before the anchor, each later
// one after the last issue ranked so far, and every issue's outcome is
// reported in the caller's order.
func TestRankChainsGroups(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	refused := map[string]bool{"WEB-50": true, "WEB-51": true, "WEB-100": true, "WEB-120": true}
	site := &batchSite{t: t, answer: func(n int, b batchBody) (int, string) {
		key := func(s string) string { return s }
		if n == 2 {
			key = strings.ToLower // keys match whatever their case
		}
		return http.StatusMultiStatus, multiStatus(b.Issues, refused, key)
	}}
	f.on("PUT", rankURL, site.handle)
	issues := webIssues(1, 120)
	res, err := c.Rank(t.Context(), RankInput{Issues: issues, Position: RankBefore("WEB-500"), RankFieldID: 10019})
	be, ok := errors.AsType[*BatchError](err)
	if !ok || be.Refused != 4 || be.Unknown != 0 || be.NotSent != 0 || be.Err != nil || be.Step != StepRank {
		t.Fatalf("got %v", err)
	}
	if trackerapi.WriteNotApplied(err) {
		t.Error("a batch that ranked issues reads as not applied")
	}
	want := []batchBody{
		{Issues: webIssues(1, 50), Before: "WEB-500", Field: 10019},
		{Issues: webIssues(51, 100), After: "WEB-49", Field: 10019},
		{Issues: webIssues(101, 120), After: "WEB-99", Field: 10019},
	}
	if fmt.Sprint(site.bodies) != fmt.Sprint(want) {
		t.Errorf("sent %v\nwant %v", site.bodies, want)
	}
	if len(res.Issues) != 120 || len(res.Done()) != 116 {
		t.Fatalf("%d outcomes, %d done", len(res.Issues), len(res.Done()))
	}
	for i, o := range res.Issues {
		wantOutcome := OutcomeDone
		if refused[o.Issue] {
			wantOutcome = OutcomeRefused
		}
		if o.Issue != issues[i] || o.Outcome != wantOutcome || (wantOutcome == OutcomeRefused) != strings.Contains(o.Reason, "cannot be ranked") {
			t.Errorf("issue %d: %+v", i, o)
		}
	}
}

// TestRankAllDone is the plain success: 204 for each group, no error.
// TestRankEntriesByID proves a 207 entry is matched to an issue the
// caller named by ID through its issueId, a number in the answer.
func TestRankEntriesByID(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("PUT", rankURL, reply(207, `{"entries":[{"issueId":10,"issueKey":"WEB-10","status":204},{"issueId":11,"issueKey":"WEB-11","status":400,"errors":["no"]}]}`))
	res, err := c.Rank(t.Context(), RankInput{Issues: []string{"10", "11"}, Position: RankAfter("WEB-500")})
	if be, ok := errors.AsType[*BatchError](err); !ok || be.Refused != 1 || be.Unknown != 0 {
		t.Fatalf("got %v", err)
	}
	if res.Issues[0].Outcome != OutcomeDone || res.Issues[1].Outcome != OutcomeRefused {
		t.Errorf("outcomes %+v", res.Issues)
	}
}

func TestRankAllDone(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	site := &batchSite{t: t, answer: func(int, batchBody) (int, string) { return http.StatusNoContent, "" }}
	f.on("PUT", rankURL, site.handle)
	res, err := c.Rank(t.Context(), RankInput{Issues: webIssues(1, 60), Position: RankAfter("WEB-500")})
	if err != nil || len(res.Done()) != 60 {
		t.Fatalf("got %+v %v", res, err)
	}
	if site.bodies[0].After != "WEB-500" || site.bodies[1].After != "WEB-50" || site.bodies[0].Field != 0 {
		t.Errorf("sent %+v", site.bodies)
	}
}

// TestBatchStopsAtAGroupNotAnsweredInFull proves a group whose outcome
// is unknown, or which Jira refuses whole, stops the batch: the groups
// after it are not sent, and each issue says which happened to it.
func TestBatchStopsAtAGroupNotAnsweredInFull(t *testing.T) {
	cases := map[string]struct {
		status        int
		body          string
		want          Outcome // of the second group's issues
		unknown, sent int
		notApplied    bool
	}{
		"a 500":                {500, `{}`, OutcomeUnknown, 50, 2, false},
		"a 207 not JSON":       {207, `{"entries":[`, OutcomeUnknown, 50, 2, false},
		"a group refused":      {400, `{"errorMessages":["no"]}`, OutcomeRefused, 0, 2, true},
		"an entry left out":    {207, "", OutcomeDone, 1, 2, false},
		"a credential refused": {401, `{}`, OutcomeRefused, 0, 2, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			c := newTestClient(t, f)
			f.on("GET", myselfURL, reply(200, userOneJSON))
			site := &batchSite{t: t, answer: func(n int, b batchBody) (int, string) {
				switch {
				case n != 1:
					return http.StatusNoContent, ""
				case tc.body == "":
					return 207, multiStatus(b.Issues[:49], nil, func(s string) string { return s })
				}
				return tc.status, tc.body
			}}
			f.on("PUT", rankURL, site.handle)
			res, err := c.Rank(t.Context(), RankInput{Issues: webIssues(1, 120), Position: RankBefore("WEB-500")})
			be, ok := errors.AsType[*BatchError](err)
			if !ok || be.Err == nil || be.Unknown != tc.unknown || be.NotSent != 20 {
				t.Fatalf("got %+v", err)
			}
			if len(site.bodies) != tc.sent {
				t.Errorf("%d groups sent", len(site.bodies))
			}
			// As for a StepError, it speaks for the group that stopped the
			// batch; the result says what the earlier groups did.
			if trackerapi.WriteNotApplied(err) != tc.notApplied {
				t.Errorf("WriteNotApplied %v: %v", !tc.notApplied, err)
			}
			for i, o := range res.Issues {
				want := OutcomeDone
				switch {
				case i >= 100:
					want = OutcomeNotSent
				case i == 99 && tc.body == "":
					want = OutcomeUnknown
				case i >= 50:
					want = tc.want
				}
				if o.Outcome != want {
					t.Fatalf("issue %d: %v, want %v", i, o.Outcome, want)
				}
			}
		})
	}
}

// TestRankCancelledMidSequence cancels while the second group is in
// flight: its outcome is unknown and the third is not sent.
func TestRankCancelledMidSequence(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	ctx, cancel := context.WithCancel(t.Context())
	site := &batchSite{t: t, answer: func(int, batchBody) (int, string) { return http.StatusNoContent, "" }}
	f.on("PUT", rankURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		if site.n() == 1 {
			site.handle(httpDiscard{}, r, b)
			cancel()
			<-r.Context().Done()
			return
		}
		site.handle(w, r, b)
	})
	res, err := c.Rank(ctx, RankInput{Issues: webIssues(1, 120), Position: RankBefore("WEB-500")})
	be, ok := errors.AsType[*BatchError](err)
	if !ok || !errors.Is(err, context.Canceled) || be.Unknown != 50 || be.NotSent != 20 {
		t.Fatalf("got %v", err)
	}
	if res.Issues[49].Outcome != OutcomeDone || res.Issues[50].Outcome != OutcomeUnknown || res.Issues[119].Outcome != OutcomeNotSent {
		t.Errorf("outcomes %v %v %v", res.Issues[49], res.Issues[50], res.Issues[119])
	}
	if n := f.count("PUT", rankURL); n != 2 {
		t.Errorf("%d groups sent", n)
	}
}

// failingReread hands out the credential once; every later read fails.
type failingReread struct{ reads atomic.Int32 }

func (s *failingReread) Credential(context.Context) (*trackerapi.Credential, error) {
	if s.reads.Add(1) == 1 {
		return trackerapi.Basic("user1@example.com", secret.New([]byte(testToken))), nil
	}
	return nil, errors.New("the credential store is locked")
}

// TestRankGroupNotSent proves a group the transport refuses before
// sending (here the credential cannot be read again after the first
// group dropped it) is not sent, not refused, and stops the batch.
func TestRankGroupNotSent(t *testing.T) {
	f := newFake(t)
	c := markClient(t, f, &failingReread{}, nil)
	site := &batchSite{t: t, answer: func(int, batchBody) (int, string) { return http.StatusNoContent, "" }}
	f.on("PUT", rankURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		c.ForgetCredential()
		site.handle(w, r, b)
	})
	res, err := c.Rank(t.Context(), RankInput{Issues: webIssues(1, 120), Position: RankBefore("WEB-500")})
	be, ok := errors.AsType[*BatchError](err)
	if !ok || be.NotSent != 70 || be.Refused != 0 || be.Unknown != 0 || !trackerapi.WriteNotApplied(err) {
		t.Fatalf("got %v", err)
	}
	if res.Issues[49].Outcome != OutcomeDone || res.Issues[50].Outcome != OutcomeNotSent {
		t.Errorf("outcomes %v %v", res.Issues[49], res.Issues[50])
	}
	if n := f.count("PUT", rankURL); n != 1 {
		t.Errorf("%d groups sent", n)
	}
}

func (s *batchSite) n() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

// httpDiscard is a ResponseWriter that keeps nothing, for a request whose
// answer never goes out.
type httpDiscard struct{}

func (httpDiscard) Header() http.Header         { return http.Header{} }
func (httpDiscard) Write(b []byte) (int, error) { return len(b), nil }
func (httpDiscard) WriteHeader(int)             {}

// TestBatchRefusals proves what a batch refuses before sending.
func TestBatchRefusals(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	for name, in := range map[string]RankInput{
		"no issues":        {Position: RankBefore("WEB-9")},
		"no position":      {Issues: []string{"WEB-1"}},
		"a bad reference":  {Issues: []string{"WEB-1", "x/y"}, Position: RankBefore("WEB-9")},
		"an issue twice":   {Issues: []string{"WEB-1", "web-1"}, Position: RankBefore("WEB-9")},
		"against itself":   {Issues: []string{"WEB-1", "WEB-2"}, Position: RankAfter("web-2")},
		"a bad anchor":     {Issues: []string{"WEB-1"}, Position: RankAfter("../x")},
		"a negative field": {Issues: []string{"WEB-1"}, Position: RankAfter("WEB-9"), RankFieldID: -1},
	} {
		if _, err := c.Rank(t.Context(), in); !trackerapi.WriteNotApplied(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, in := range map[string]BacklogMove{
		"a field without a position": {Issues: []string{"WEB-1"}, RankFieldID: 10019},
		"a position without a board": {Issues: []string{"WEB-1"}, Position: RankAfter("WEB-9")},
		"a negative board":           {Issues: []string{"WEB-1"}, Board: -3},
	} {
		if _, err := c.MoveToBacklog(t.Context(), in); !trackerapi.WriteNotApplied(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("%d requests sent", n)
	}
}

// TestMoveToBacklog proves the plain move takes no rank fields and the
// board's backlog takes them.
func TestMoveToBacklog(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("POST", "/rest/agile/1.0/backlog/issue", func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantJSON(t, b, `{"issues":["WEB-1","WEB-2"]}`)
		w.WriteHeader(http.StatusNoContent)
	})
	f.on("POST", "/rest/agile/1.0/backlog/3/issue", func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantJSON(t, b, `{"issues":["WEB-1"],"rankBeforeIssue":"WEB-9","rankCustomFieldId":10019}`)
		w.WriteHeader(http.StatusNoContent)
	})
	if res, err := c.MoveToBacklog(t.Context(), BacklogMove{Issues: []string{"WEB-1", "WEB-2"}}); err != nil || len(res.Done()) != 2 {
		t.Fatalf("got %+v %v", res, err)
	}
	if _, err := c.MoveToBacklog(t.Context(), BacklogMove{Board: 3, Issues: []string{"WEB-1"}, Position: RankBefore("WEB-9"), RankFieldID: 10019}); err != nil {
		t.Fatal(err)
	}
}

// TestMoveToSprint proves the sprint is read first, a closed one refused
// before any move, and an open one moved into.
func TestMoveToSprint(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("GET", "/rest/agile/1.0/sprint/7", reply(200, `{"id":7,"name":"Sprint 1","state":"active","originBoardId":3}`))
	f.on("GET", "/rest/agile/1.0/sprint/6", reply(200, `{"id":6,"name":"Sprint 0","state":"closed","originBoardId":3}`))
	f.on("POST", "/rest/agile/1.0/sprint/7/issue", func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantJSON(t, b, `{"issues":["WEB-1"],"rankAfterIssue":"WEB-9"}`)
		w.WriteHeader(http.StatusNoContent)
	})
	if res, err := c.MoveToSprint(t.Context(), SprintMove{Sprint: 7, Issues: []string{"WEB-1"}, Position: RankAfter("WEB-9")}); err != nil || len(res.Done()) != 1 {
		t.Fatalf("got %+v %v", res, err)
	}
	if _, err := c.MoveToSprint(t.Context(), SprintMove{Sprint: 6, Issues: []string{"WEB-1"}}); !trackerapi.WriteNotApplied(err) {
		t.Errorf("a closed sprint: %v", err)
	}
	if n := f.count("POST", "/rest/agile/1.0/sprint/6/issue"); n != 0 {
		t.Errorf("%d moves into a closed sprint", n)
	}
}
