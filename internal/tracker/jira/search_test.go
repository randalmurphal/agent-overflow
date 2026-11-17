package jira

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }

// searchPages answers search/jql with three pages of two issues, failing
// on the page after token failOn when it is set, handing out the first
// token again when repeat is set, and warning on the page warnOn names
// (-1 for none). A client that asks for more than ten pages is looping:
// the test fails and the page is refused, so the loop ends.
func searchPages(t *testing.T, failOn string, repeat bool, warnOn int) handler {
	var calls atomic.Int32
	return func(w http.ResponseWriter, r *http.Request, b []byte) {
		if calls.Add(1) > 10 {
			t.Error("the client kept asking for pages")
			reply(400, `{}`)(w, r, b)
			return
		}
		var req struct {
			JQL           string   `json:"jql"`
			Fields        []string `json:"fields"`
			MaxResults    int      `json:"maxResults"`
			NextPageToken string   `json:"nextPageToken"`
		}
		if err := json.Unmarshal(b, &req); err != nil {
			t.Fatal(err)
		}
		if req.JQL != `project = "WEB"` || strings.Join(req.Fields, ",") != "summary,status" {
			t.Errorf("request %s", b)
		}
		if failOn != "" && req.NextPageToken == failOn {
			reply(400, `{"errorMessages":["The token is not valid"]}`)(w, r, b)
			return
		}
		page := map[string]int{"": 0, "t1": 1, "t2": 2}[req.NextPageToken]
		next := fmt.Sprintf(`"t%d"`, page+1)
		if repeat {
			next = `"t1"`
		}
		last := page == 2 && !repeat
		if last {
			next = "null"
		}
		warnings := "[]"
		if page == warnOn {
			warnings = `[{"type":"RESULTS_TRUNCATED","message":"cut"}]`
		}
		reply(200, fmt.Sprintf(`{"issues":[{"id":"%d","key":"WEB-%d","fields":{"summary":"a"}},{"id":"%d","key":"WEB-%d","fields":{"summary":"b"}}],
			"nextPageToken":%s,"isLast":%v,"warnings":%s}`,
			2*page, 2*page, 2*page+1, 2*page+1, next, last, warnings))(w, r, b)
	}
}

func TestSearchAllPagesToTheEnd(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("POST", "/rest/api/3/search/jql", searchPages(t, "", false, 0))
	q := SearchQuery{JQL: "project = " + QuoteJQL("WEB"), Fields: []string{"summary", "status"}}
	page, err := c.Search(t.Context(), q)
	if err != nil || len(page.Issues) != 2 || page.NextPageToken != "t1" || page.Warnings[0] != (SearchWarning{"RESULTS_TRUNCATED", "cut"}) {
		t.Fatalf("got %+v %v", page, err)
	}
	f.on("POST", "/rest/api/3/search/jql", searchPages(t, "", false, -1))
	all, err := c.SearchAll(t.Context(), q, 0)
	if err != nil || len(all.Issues) != 6 || all.Issues[5].Key != "WEB-5" || !all.Complete {
		t.Fatalf("got %d issues, complete %v, %v", len(all.Issues), all.Complete, err)
	}
	// At 5 the last page holds more than was asked for, and is cut.
	for limit, complete := range map[int]bool{3: false, 5: false, 6: true, 7: true} {
		f.on("POST", "/rest/api/3/search/jql", searchPages(t, "", false, -1))
		some, err := c.SearchAll(t.Context(), q, limit)
		if err != nil || len(some.Issues) != min(limit, 6) || some.Complete != complete {
			t.Errorf("limit %d: %d issues, complete %v, %v", limit, len(some.Issues), some.Complete, err)
		}
	}
}

// TestSearchAllKeepsWarnings proves a warning on the last page, such as
// results cut short, reaches the caller and marks the result incomplete.
func TestSearchAllKeepsWarnings(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("POST", "/rest/api/3/search/jql", searchPages(t, "", false, 2))
	all, err := c.SearchAll(t.Context(), SearchQuery{JQL: `project = "WEB"`, Fields: []string{"summary", "status"}}, 0)
	if err != nil || len(all.Issues) != 6 || all.Complete || len(all.Warnings) != 1 || all.Warnings[0].Message != "cut" {
		t.Fatalf("got %+v %v", all, err)
	}
}

func TestSearchAllFailureMidWayKeepsWhatWasRead(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("POST", "/rest/api/3/search/jql", searchPages(t, "t2", false, 0))
	q := SearchQuery{JQL: `project = "WEB"`, Fields: []string{"summary", "status"}}
	all, err := c.SearchAll(t.Context(), q, 0)
	if err == nil || len(all.Issues) != 4 || all.Complete || len(all.Warnings) != 1 {
		t.Fatalf("got %+v and %v", all, err)
	}
}

func TestSearchAllRefusesARepeatedToken(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("POST", "/rest/api/3/search/jql", searchPages(t, "", true, -1))
	q := SearchQuery{JQL: `project = "WEB"`, Fields: []string{"summary", "status"}}
	if _, err := c.SearchAll(t.Context(), q, 0); err == nil || !strings.Contains(err.Error(), "repeated") {
		t.Fatalf("got %v", err)
	}
	if n := f.count("POST", "/rest/api/3/search/jql"); n != 2 {
		t.Errorf("%d pages, want 2", n)
	}
}

// TestSearchPageEnds proves only isLast ends a search: a page that is not
// the last and names no next page fails, and so does an empty page that
// names one, since either would read as a finished search.
func TestSearchPageEnds(t *testing.T) {
	q := SearchQuery{JQL: `project = "WEB"`}
	for name, body := range map[string]string{
		"no token":      `{"issues":[{"id":"1","key":"WEB-1"}],"isLast":false}`,
		"null token":    `{"issues":[{"id":"1","key":"WEB-1"}],"nextPageToken":null,"isLast":false}`,
		"no isLast":     `{"issues":[{"id":"1","key":"WEB-1"}]}`,
		"empty, a next": `{"issues":[],"nextPageToken":"t1","isLast":false}`,
	} {
		f := newFake(t)
		c := newTestClient(t, f)
		f.on("POST", "/rest/api/3/search/jql", reply(200, body))
		all, err := c.SearchAll(t.Context(), q, 0)
		if err == nil || all.Complete {
			t.Errorf("%s: got %+v %v", name, all, err)
		}
		if n := f.count("POST", "/rest/api/3/search/jql"); n != 1 {
			t.Errorf("%s: %d pages", name, n)
		}
		if name == "empty, a next" {
			continue
		}
		if _, err := c.Search(t.Context(), q); !isAnswerErr(err) {
			t.Errorf("%s: one page: %v", name, err)
		}
	}
}

func isAnswerErr(err error) bool {
	_, ok := errors.AsType[*AnswerError](err)
	return ok
}

func TestSearchCancellation(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	ctx, cancel := context.WithCancel(t.Context())
	f.on("POST", "/rest/api/3/search/jql", func(w http.ResponseWriter, r *http.Request, b []byte) {
		cancel()
		<-r.Context().Done()
	})
	if _, err := c.Search(ctx, SearchQuery{JQL: `project = "WEB"`}); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestCountIssues(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("POST", "/rest/api/3/search/approximate-count", func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantJSON(t, b, `{"jql":"assignee = currentUser()"}`)
		reply(200, `{"count":1234}`)(w, r, b)
	})
	n, err := c.CountIssues(t.Context(), "assignee = currentUser()")
	if err != nil || n != 1234 {
		t.Fatalf("got %d %v", n, err)
	}
}

func TestBulkFetchBatches(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	var refs []string
	for i := range 150 {
		refs = append(refs, fmt.Sprintf("WEB-%d", i+1))
	}
	f.on("POST", "/rest/api/3/issue/bulkfetch", func(w http.ResponseWriter, r *http.Request, b []byte) {
		var req struct {
			Refs   []string `json:"issueIdsOrKeys"`
			Fields []string `json:"fields"`
		}
		_ = json.Unmarshal(b, &req)
		if len(req.Refs) > bulkFetchBatch || strings.Join(req.Fields, ",") != "summary" {
			t.Errorf("request of %d refs, fields %v", len(req.Refs), req.Fields)
		}
		if req.Refs[0] == "WEB-101" {
			reply(200, `{"issues":[{"id":"1","key":"WEB-101","fields":{"summary":"x"}}],"issueErrors":[{"id":"2","errorMessage":"retry"}]}`)(w, r, b)
			return
		}
		reply(200, `{"issues":[{"id":"3","key":"WEB-1","fields":{"summary":"y"}}],"issueErrors":[]}`)(w, r, b)
	})
	res, err := c.BulkFetch(t.Context(), refs, "summary")
	if err != nil || len(res.Issues) != 2 || len(res.Failed) != 1 || res.Failed[0].Message != "retry" || res.Complete {
		t.Fatalf("got %+v %v", res, err)
	}
	if n := f.count("POST", "/rest/api/3/issue/bulkfetch"); n != 2 {
		t.Errorf("%d requests, want 2", n)
	}
	f.on("POST", "/rest/api/3/issue/bulkfetch", reply(200, `{"issues":[{"id":"3","key":"WEB-1"},{"id":"4","key":"WEB-2"}]}`))
	if res, err := c.BulkFetch(t.Context(), []string{"WEB-1", "4"}); err != nil || !res.Complete {
		t.Errorf("every issue returned: %+v %v", res, err)
	}
	if res, err := c.BulkFetch(t.Context(), []string{"WEB-1", "WEB-2", "WEB-3"}); err != nil || res.Complete {
		t.Errorf("an issue missing: %+v %v", res, err)
	}
	// A failure is incomplete even when the issues returned number the refs.
	f.on("POST", "/rest/api/3/issue/bulkfetch", reply(200, `{"issues":[{"id":"3","key":"WEB-1"},{"id":"4","key":"WEB-2"}],"issueErrors":[{"id":"5","errorMessage":"retry"}]}`))
	if res, err := c.BulkFetch(t.Context(), []string{"WEB-1", "WEB-2"}); err != nil || res.Complete {
		t.Errorf("an issue failed: %+v %v", res, err)
	}
	if _, err := c.BulkFetch(t.Context(), []string{"WEB-1", "x/y"}); err == nil {
		t.Error("a bad ref was sent")
	}
}

// TestBulkFetchRepeatedReference proves a reference named again, in a
// later batch or another case, is sent once, so the issues returned
// cannot stand in for one that is missing.
func TestBulkFetchRepeatedReference(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	var refs []string
	for i := range 100 {
		refs = append(refs, fmt.Sprintf("WEB-%d", i+1))
	}
	// WEB-1 again in the second batch, beside WEB-101, which is missing.
	refs = append(refs, "web-1", "WEB-101")
	sent := map[string]int{}
	f.on("POST", "/rest/api/3/issue/bulkfetch", func(w http.ResponseWriter, r *http.Request, b []byte) {
		var req struct {
			Refs []string `json:"issueIdsOrKeys"`
		}
		if err := json.Unmarshal(b, &req); err != nil {
			t.Fatal(err)
		}
		var issues []string
		for _, ref := range req.Refs {
			sent[strings.ToUpper(ref)]++
			// Every issue answers but WEB-101; WEB-n has ID n.
			if id := strings.TrimPrefix(strings.ToUpper(ref), "WEB-"); id != "101" {
				issues = append(issues, fmt.Sprintf(`{"id":"%s","key":"WEB-%s"}`, id, id))
			}
		}
		reply(200, `{"issues":[`+strings.Join(issues, ",")+`]}`)(w, r, b)
	})
	res, err := c.BulkFetch(t.Context(), refs)
	if err != nil || res.Complete || len(res.Issues) != 100 {
		t.Fatalf("got %d issues, complete %v, %v", len(res.Issues), res.Complete, err)
	}
	if sent["WEB-1"] != 1 || sent["WEB-101"] != 1 || len(sent) != 101 {
		t.Errorf("WEB-1 sent %d times, WEB-101 %d, %d references", sent["WEB-1"], sent["WEB-101"], len(sent))
	}
	// An ID and a key naming one issue return it once, and read as
	// incomplete: one reference brought back nothing of its own.
	// Issue 3 by ID in the first batch and by key in the second.
	refs = []string{"3"}
	for i := range 99 {
		refs = append(refs, fmt.Sprintf("WEB-%d", 200+i))
	}
	refs = append(refs, "WEB-3")
	res, err = c.BulkFetch(t.Context(), refs)
	if err != nil || res.Complete || len(res.Issues) != 100 {
		t.Errorf("an issue named twice: %d issues, complete %v, %v", len(res.Issues), res.Complete, err)
	}
}

func TestQuoteJQL(t *testing.T) {
	for in, want := range map[string]string{
		`WEB`:               `"WEB"`,
		`say "hi"`:          `"say \"hi\""`,
		`back\slash`:        `"back\\slash"`,
		"two\nlines\ttab\r": `"two\nlines\ttab\r"`,
		`" OR project = X`:  `"\" OR project = X"`,
		``:                  `""`,
	} {
		if got := QuoteJQL(in); got != want {
			t.Errorf("QuoteJQL(%q) = %s, want %s", in, got, want)
		}
	}
	if got := QuoteJQLList("A", `B"`); got != `("A", "B\"")` {
		t.Errorf("list: %s", got)
	}
}
