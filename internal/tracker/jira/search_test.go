package jira

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }

// searchPages answers search/jql with three pages of two issues, failing
// on the page after token failOn when it is set, and handing out the
// first token again when repeat is set.
func searchPages(t *testing.T, failOn string, repeat bool) handler {
	return func(w http.ResponseWriter, r *http.Request, b []byte) {
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
		reply(200, fmt.Sprintf(`{"issues":[{"id":"%d","key":"WEB-%d","fields":{"summary":"a"}},{"id":"%d","key":"WEB-%d","fields":{"summary":"b"}}],
			"nextPageToken":%s,"isLast":%v,"warnings":[{"type":"CLAUSE_LIMIT_EXCEEDED","message":"cut"}]}`,
			2*page, 2*page, 2*page+1, 2*page+1, next, last))(w, r, b)
	}
}

func TestSearchAllPagesToTheEnd(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f)
	f.on("POST", "/rest/api/3/search/jql", searchPages(t, "", false))
	q := SearchQuery{JQL: "project = " + QuoteJQL("WEB"), Fields: []string{"summary", "status"}}
	page, err := c.Search(t.Context(), q)
	if err != nil || len(page.Issues) != 2 || page.NextPageToken != "t1" || page.Warnings[0] != "cut" {
		t.Fatalf("got %+v %v", page, err)
	}
	all, err := c.SearchAll(t.Context(), q, 0)
	if err != nil || len(all) != 6 || all[5].Key != "WEB-5" {
		t.Fatalf("got %d issues, %v", len(all), err)
	}
	some, err := c.SearchAll(t.Context(), q, 3)
	if err != nil || len(some) != 3 {
		t.Errorf("limit: %d %v", len(some), err)
	}
}

func TestSearchAllFailureMidWayKeepsWhatWasRead(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f)
	f.on("POST", "/rest/api/3/search/jql", searchPages(t, "t2", false))
	q := SearchQuery{JQL: `project = "WEB"`, Fields: []string{"summary", "status"}}
	all, err := c.SearchAll(t.Context(), q, 0)
	if err == nil || len(all) != 4 {
		t.Fatalf("got %d issues and %v", len(all), err)
	}
}

func TestSearchAllRefusesARepeatedToken(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f)
	f.on("POST", "/rest/api/3/search/jql", searchPages(t, "", true))
	q := SearchQuery{JQL: `project = "WEB"`, Fields: []string{"summary", "status"}}
	if _, err := c.SearchAll(t.Context(), q, 0); err == nil || !strings.Contains(err.Error(), "repeated") {
		t.Fatalf("got %v", err)
	}
	if n := f.count("POST", "/rest/api/3/search/jql"); n != 2 {
		t.Errorf("%d pages, want 2", n)
	}
}

func TestSearchCancellation(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f)
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
	c, _ := newTestClient(t, f)
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
	c, _ := newTestClient(t, f)
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
	if err != nil || len(res.Issues) != 2 || len(res.Failed) != 1 || res.Failed[0].Message != "retry" {
		t.Fatalf("got %+v %v", res, err)
	}
	if n := f.count("POST", "/rest/api/3/issue/bulkfetch"); n != 2 {
		t.Errorf("%d requests, want 2", n)
	}
	if _, err := c.BulkFetch(t.Context(), []string{"WEB-1", "x/y"}); err == nil {
		t.Error("a bad ref was sent")
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
