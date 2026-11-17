package jira

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
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

// TestRankEntriesByID proves a 207 entry is matched to an issue the
// caller named by ID through its issueId, a number in the answer, and a
// refused entry, 503 included, is refused.
func TestRankEntriesByID(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("PUT", rankURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantJSON(t, b, `{"issues":["10","11","12"],"rankAfterIssue":"WEB-500"}`)
		reply(207, `{"entries":[{"issueId":10,"issueKey":"WEB-10","status":204},{"issueId":11,"issueKey":"WEB-11","status":400,"errors":["no"]},
			{"issueId":12,"issueKey":"WEB-12","status":503,"errors":["later"]}]}`)(w, r, b)
	})
	res, err := c.Rank(t.Context(), RankInput{Issues: []string{"10", "11", "12"}, Position: RankAfter("WEB-500")})
	if be, ok := errors.AsType[*BatchError](err); !ok || be.Refused != 2 || be.Unknown != 0 {
		t.Fatalf("got %v", err)
	}
	// The spec's example answers 503 for a rank Jira could not run then.
	if res.Issues[0].Outcome != OutcomeDone || res.Issues[1].Outcome != OutcomeRefused || res.Issues[2].Outcome != OutcomeRefused || res.Issues[2].Reason != "503: later" {
		t.Errorf("outcomes %+v", res.Issues)
	}
}

// TestRankAllDone is the plain success: 204 for each group, no error.
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

// rankEntry is a 207 entry for issue WEB-n with status; a status of 0
// leaves the status out.
func rankEntry(issue string, status int) string {
	if status == 0 {
		return fmt.Sprintf(`{"issueId":%s,"issueKey":%q}`, strings.TrimPrefix(issue, "WEB-"), issue)
	}
	return fmt.Sprintf(`{"issueId":%s,"issueKey":%q,"status":%d,"errors":[]}`, strings.TrimPrefix(issue, "WEB-"), issue, status)
}

// rankEntries is a 207 with a 204 entry for every issue but the last,
// followed by the entries in last.
func rankEntries(issues []string, last ...string) string {
	var out []string
	for _, is := range issues[:len(issues)-1] {
		out = append(out, rankEntry(is, 204))
	}
	return `{"entries":[` + strings.Join(append(out, last...), ",") + `]}`
}

// TestBatchStopsAtAGroupNotAnsweredInFull proves a group whose outcome
// is unknown, or which Jira refuses whole, stops the batch: the groups
// after it are not sent, and each issue says which happened to it. A
// rank's answer is recognized only as a 204 or a 207 whose every entry
// names an issue of the group, once, with a status; any other answer
// leaves the whole group unknown, and an entry status this package does
// not know leaves its issue unknown.
func TestBatchStopsAtAGroupNotAnsweredInFull(t *testing.T) {
	last := func(issues []string) string { return issues[len(issues)-1] }
	fixed := func(body string) func([]string) string { return func([]string) string { return body } }
	cases := map[string]struct {
		status     int
		body       func(issues []string) string
		want       Outcome // of the second group's issues
		lastOnly   bool    // only the group's last issue has the outcome want
		unknown    int
		notApplied bool
	}{
		"a 500":                 {500, fixed(`{}`), OutcomeUnknown, false, 50, false},
		"a 207 not JSON":        {207, fixed(`{"entries":[`), OutcomeUnknown, false, 50, false},
		"a 207 without entries": {207, fixed(`{"entries":null}`), OutcomeUnknown, false, 50, false},
		"a 200":                 {200, func(is []string) string { return rankEntries(is, rankEntry(last(is), 204)) }, OutcomeUnknown, false, 50, false},
		"a 200 page":            {200, fixed(`<html>ranked</html>`), OutcomeUnknown, false, 50, false},
		"a 202":                 {202, fixed(`{}`), OutcomeUnknown, false, 50, false},
		"an entry without a status": {207, func(is []string) string {
			return rankEntries(is, rankEntry(last(is), 0))
		}, OutcomeUnknown, false, 50, false},
		"an entry for another issue": {207, func(is []string) string {
			return rankEntries(is, rankEntry(last(is), 204), rankEntry("WEB-500", 204))
		}, OutcomeUnknown, false, 50, false},
		"an entry twice": {207, func(is []string) string {
			return rankEntries(is, rankEntry(last(is), 204), rankEntry(is[0], 400))
		}, OutcomeUnknown, false, 50, false},
		"an entry left out": {207, func(is []string) string { return rankEntries(is) }, OutcomeUnknown, true, 1, false},
		"an unknown entry status": {207, func(is []string) string {
			return rankEntries(is, rankEntry(last(is), 500))
		}, OutcomeUnknown, true, 1, false},
		"a group refused":      {400, fixed(`{"errorMessages":["no"]}`), OutcomeRefused, false, 0, true},
		"a credential refused": {401, fixed(`{}`), OutcomeRefused, false, 0, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			c := newTestClient(t, f)
			f.on("GET", myselfURL, reply(200, userOneJSON))
			site := &batchSite{t: t, answer: func(n int, b batchBody) (int, string) {
				if n != 1 {
					return http.StatusNoContent, ""
				}
				return tc.status, tc.body(b.Issues)
			}}
			f.on("PUT", rankURL, site.handle)
			res, err := c.Rank(t.Context(), RankInput{Issues: webIssues(1, 120), Position: RankBefore("WEB-500")})
			be, ok := errors.AsType[*BatchError](err)
			if !ok || be.Err == nil || be.Unknown != tc.unknown || be.NotSent != 20 {
				t.Fatalf("got %+v", err)
			}
			want := []batchBody{{Issues: webIssues(1, 50), Before: "WEB-500"}, {Issues: webIssues(51, 100), After: "WEB-50"}}
			if fmt.Sprint(site.bodies) != fmt.Sprint(want) {
				t.Errorf("sent %v\nwant %v", site.bodies, want)
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
				case i == 99 || i >= 50 && !tc.lastOnly:
					want = tc.want
				}
				if o.Outcome != want {
					t.Fatalf("issue %d: %v, want %v", i, o.Outcome, want)
				}
			}
		})
	}
}

// TestRankEntryIdentities proves a 207 entry must match exactly one
// issue of the group by its ID and key together: an entry whose ID and
// key name different requested issues, or two entries with the same ID
// or the same key, leave the whole group unknown and stop the batch. The
// group names two issues by ID (10001, 10002, whose keys are WEB-1 and
// WEB-2) and the rest by key; an entry for WEB-n carries ID 10000+n.
func TestRankEntryIdentities(t *testing.T) {
	issues := append([]string{"10001", "10002"}, webIssues(3, 60)...)
	entry := func(id int, key string, status int) string {
		return fmt.Sprintf(`{"issueId":%d,"issueKey":%q,"status":%d}`, id, key, status)
	}
	// normal answers the group's issues from the third on, but drop.
	normal := func(group []string, drop string) []string {
		var out []string
		for _, is := range group[2:] {
			if is == drop {
				continue
			}
			n, _ := strconv.Atoi(strings.TrimPrefix(is, "WEB-"))
			out = append(out, entry(10000+n, is, 204))
		}
		return out
	}
	for name, tc := range map[string]struct {
		first []string // the entries for 10001 and 10002
		last  string   // replaces the entry for WEB-50
		drop  string   // an issue left without its own entry
		sent  int
	}{
		"every entry its own issue":     {[]string{entry(10001, "WEB-1", 204), entry(10002, "WEB-2", 204)}, "", "", 2},
		"an ID and a key of two issues": {[]string{entry(10001, "WEB-3", 200), entry(10002, "WEB-2", 204)}, "", "WEB-3", 1},
		"the same key twice":            {[]string{entry(10001, "WEB-1", 204), entry(10002, "WEB-1", 204)}, "", "", 1},
		"the same ID twice":             {[]string{entry(10001, "WEB-1", 204), entry(10002, "WEB-2", 204)}, entry(10003, "WEB-50", 204), "", 1},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			c := newTestClient(t, f)
			site := &batchSite{t: t, answer: func(n int, b batchBody) (int, string) {
				if n > 0 {
					return http.StatusNoContent, ""
				}
				entries := append(slices.Clone(tc.first), normal(b.Issues, tc.drop)...)
				if tc.last != "" {
					entries[len(entries)-1] = tc.last
				}
				return http.StatusMultiStatus, `{"entries":[` + strings.Join(entries, ",") + `]}`
			}}
			f.on("PUT", rankURL, site.handle)
			res, err := c.Rank(t.Context(), RankInput{Issues: issues, Position: RankBefore("WEB-500")})
			if want := (batchBody{Issues: issues[:50], Before: "WEB-500"}); len(site.bodies) == 0 || fmt.Sprint(site.bodies[0]) != fmt.Sprint(want) {
				t.Errorf("sent %v", site.bodies)
			}
			if tc.sent == 2 {
				if err != nil || len(res.Done()) != len(issues) {
					t.Fatalf("got %+v %v", res, err)
				}
				return
			}
			be, ok := errors.AsType[*BatchError](err)
			if !ok || be.Unknown != 50 || be.NotSent != 10 || be.Refused != 0 || len(site.bodies) != 1 || trackerapi.WriteNotApplied(err) {
				t.Fatalf("got %+v %v", be, err)
			}
		})
	}
}

// TestRankRefusedGroupAfterAFailedCheck proves a group Jira answered 404
// is refused even when the sign-in check that follows the 404 fails
// before it is sent: the outcome is the write's own, not the check's.
func TestRankRefusedGroupAfterAFailedCheck(t *testing.T) {
	f := newFake(t)
	c := markClient(t, f, &failingReread{}, nil)
	site := &batchSite{t: t, answer: func(int, batchBody) (int, string) { return http.StatusNotFound, `{"errorMessages":["no"]}` }}
	f.on("PUT", rankURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		c.ForgetCredential()
		site.handle(w, r, b)
	})
	res, err := c.Rank(t.Context(), RankInput{Issues: webIssues(1, 120), Position: RankBefore("WEB-500")})
	be, ok := errors.AsType[*BatchError](err)
	if _, unconfirmed := errors.AsType[*UnconfirmedError](err); !ok || !unconfirmed || be.Refused != 50 || be.NotSent != 70 || !trackerapi.WriteNotApplied(err) {
		t.Fatalf("got %+v %v", be, err)
	}
	if res.Issues[0].Outcome != OutcomeRefused || res.Issues[50].Outcome != OutcomeNotSent || f.count("GET", myselfURL) != 0 {
		t.Errorf("outcomes %v %v", res.Issues[0], res.Issues[50])
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
	if want := []batchBody{{Issues: webIssues(1, 50), Before: "WEB-500"}}; fmt.Sprint(site.bodies) != fmt.Sprint(want) {
		t.Errorf("sent %v", site.bodies)
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
