package jira

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/tracker/trackerapi"
)

const bulkChangelogURL = "/rest/api/3/changelog/bulkfetch"

func TestChangelog(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("GET", "/rest/api/3/issue/WEB-1/changelog", func(w http.ResponseWriter, r *http.Request, b []byte) {
		wantQuery(t, r, "maxResults", "100")
		if r.URL.Query().Get("startAt") == "0" {
			reply(200, `{"startAt":0,"total":2,"isLast":false,"values":[{"id":"100","author":`+userOneJSON+`,"created":"2026-11-02T09:00:00.000+0000",
				"items":[{"field":"status","fieldId":"status","fieldtype":"jira","from":"1","fromString":"To Do","to":"3","toString":"In Progress"}]}]}`)(w, r, b)
			return
		}
		reply(200, `{"startAt":1,"total":2,"isLast":true,"values":[{"id":"101","created":"2026-11-03T09:00:00.000+0000",
			"items":[{"field":"assignee","fieldId":"assignee","fieldtype":"jira","from":null,"to":"acc-user1","toString":"User 1"}]}]}`)(w, r, b)
	})
	log, err := c.Changelog(t.Context(), "WEB-1")
	if err != nil || !log.Complete || len(log.Entries) != 2 {
		t.Fatalf("got %+v %v", log, err)
	}
	from, to, ok := log.Entries[0].Items[0].StatusChange()
	if !ok || from != "1" || to != "3" || log.Entries[0].Author == nil || log.Entries[1].Author != nil {
		t.Errorf("status change %q %q %v", from, to, ok)
	}
	if from, to, ok := log.Entries[1].Items[0].AssigneeChange(); !ok || from != "" || to != "acc-user1" {
		t.Errorf("assignee change %q %q %v", from, to, ok)
	}
	if _, _, ok := log.Entries[1].Items[0].StatusChange(); ok {
		t.Error("an assignee change read as a status change")
	}
	changes := FieldChanges(log.Entries, "assignee")
	if len(changes) != 1 || !changes[0].When.Equal(time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("field changes %+v", changes)
	}
}

// bulkRequest is a bulk changelog request as the fake reads it.
type bulkRequest struct {
	Issues []string `json:"issueIdsOrKeys"`
	Fields []string `json:"fieldIds"`
	Max    int      `json:"maxResults"`
	Token  string   `json:"nextPageToken"`
}

// TestBulkChangelogsSplits asks for 1001 issues and 11 fields: four
// requests (every issue group with every field group), each read to its
// last page, and an entry split across field groups joined again.
func TestBulkChangelogsSplits(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	issues := webIssues(1, 1001)
	var fields []string
	for i := range 11 {
		fields = append(fields, fmt.Sprintf("customfield_%d", 10001+i))
	}
	var mu sync.Mutex
	var seen []string
	f.on("POST", bulkChangelogURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		var req bulkRequest
		if err := json.Unmarshal(b, &req); err != nil {
			t.Fatal(err)
		}
		if len(req.Issues) > 1000 || len(req.Fields) > 10 || len(req.Fields) == 0 {
			t.Errorf("a request of %d issues and %d fields", len(req.Issues), len(req.Fields))
		}
		mu.Lock()
		seen = append(seen, fmt.Sprintf("%d issues, %d fields, token %q", len(req.Issues), len(req.Fields), req.Token))
		mu.Unlock()
		// Entry 500 of issue 10001 changed a field of each group; the
		// first page of each request holds it, a second page another.
		field := req.Fields[0]
		if req.Token == "" {
			reply(200, fmt.Sprintf(`{"issueChangeLogs":[{"issueId":"10001","changeHistories":[
				{"id":"500","created":1762074000000,"items":[{"fieldId":%q,"field":"F","to":"x"}]}]}],"nextPageToken":"more"}`, field))(w, r, b)
			return
		}
		reply(200, fmt.Sprintf(`{"issueChangeLogs":[{"issueId":"10001","changeHistories":[
			{"id":"499","created":"2025-11-03T09:00:00.000+0000","items":[{"fieldId":%q,"field":"F","to":"y"}]}]}]}`, field))(w, r, b)
	})
	res, err := c.BulkChangelogs(t.Context(), ChangelogQuery{Issues: issues, Fields: fields})
	if err != nil || !res.Complete {
		t.Fatalf("got %v", err)
	}
	want := []string{
		`1000 issues, 10 fields, token ""`, `1000 issues, 10 fields, token "more"`,
		`1000 issues, 1 fields, token ""`, `1000 issues, 1 fields, token "more"`,
		`1 issues, 10 fields, token ""`, `1 issues, 10 fields, token "more"`,
		`1 issues, 1 fields, token ""`, `1 issues, 1 fields, token "more"`,
	}
	if strings.Join(seen, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests\n%s", strings.Join(seen, "\n"))
	}
	entries := res.ByIssue["10001"]
	if len(entries) != 2 || entries[0].ID != "500" || entries[1].ID != "499" {
		t.Fatalf("entries %+v", entries)
	}
	var got []string
	for _, it := range entries[0].Items {
		got = append(got, it.FieldID)
	}
	slices.Sort(got)
	if strings.Join(got, ",") != "customfield_10001,customfield_10011" {
		t.Errorf("entry 500 holds %v", got)
	}
	if !entries[0].Created.Equal(time.UnixMilli(1762074000000)) {
		t.Errorf("a time in milliseconds read as %v", entries[0].Created)
	}
}

// TestBulkChangelogsPageEnds proves a repeated token or an empty page
// that names a next one fails the walk, keeping what was read.
func TestBulkChangelogsPageEnds(t *testing.T) {
	for name, second := range map[string]string{
		"a repeated token": `{"issueChangeLogs":[{"issueId":"10001","changeHistories":[{"id":"2","created":"2026-11-01T09:00:00.000+0000","items":[]}]}],"nextPageToken":"t1"}`,
		"an empty page":    `{"issueChangeLogs":[],"nextPageToken":"t2"}`,
	} {
		f := newFake(t)
		c := newTestClient(t, f)
		n := 0
		f.on("POST", bulkChangelogURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
			if n++; n > 3 {
				t.Error("the client kept asking for pages")
				reply(400, `{}`)(w, r, b)
				return
			}
			switch n {
			case 1:
				reply(200, `{"issueChangeLogs":[{"issueId":"10001","changeHistories":[{"id":"1","created":"2026-11-01T09:00:00.000+0000","items":[]}]}],"nextPageToken":"t1"}`)(w, r, b)
				return
			case 3:
				// A last page: only the guard against the bad second page
				// fails the walk.
				reply(200, `{"issueChangeLogs":[]}`)(w, r, b)
				return
			}
			reply(200, second)(w, r, b)
		})
		res, err := c.BulkChangelogs(t.Context(), ChangelogQuery{Issues: []string{"WEB-1"}})
		if err == nil || res.Complete || len(res.ByIssue["10001"]) == 0 {
			t.Errorf("%s: %+v %v", name, res, err)
		}
	}
	f := newFake(t)
	c := newTestClient(t, f)
	for name, q := range map[string]ChangelogQuery{
		"no issues":      {},
		"a bad issue":    {Issues: []string{"x/y"}},
		"an empty field": {Issues: []string{"WEB-1"}, Fields: []string{" "}},
	} {
		if _, err := c.BulkChangelogs(t.Context(), q); !trackerapi.WriteNotApplied(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestParseSprintIDs(t *testing.T) {
	for in, want := range map[string]string{
		"":                    "[]",
		"7":                   "[7]",
		"8, 7,7,,":            "[7 8]",
		" 12 , 3 ":            "[3 12]",
		"7,x":                 "error",
		"0":                   "error",
		"-3":                  "error",
		"Sprint 1, 7":         "error",
		"9223372036854775808": "error",
	} {
		ids, err := ParseSprintIDs(in)
		got := fmt.Sprint(ids)
		if ids == nil {
			got = "[]"
		}
		if err != nil {
			got = "error"
		}
		if got != want {
			t.Errorf("%q: got %s, want %s", in, got, want)
		}
	}
	from, to, err := ChangeItem{From: "7", To: "7, 8"}.SprintIDs()
	if err != nil || fmt.Sprint(from, to) != "[7] [7 8]" {
		t.Errorf("got %v %v %v", from, to, err)
	}
	if _, _, err := (ChangeItem{From: "7", To: "x"}).SprintIDs(); err == nil {
		t.Error("a bad sprint ID after the change")
	}
}
