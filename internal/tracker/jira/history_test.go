package jira

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/tracker/jira/jiratest"
	"agent-overflow/internal/tracker/trackerapi"
)

const bulkChangelogURL = "/rest/api/3/changelog/bulkfetch"

func TestChangelog(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	f.On("GET", "/rest/api/3/issue/WEB-1/changelog", func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantQuery(t, r, "maxResults", "100")
		if r.URL.Query().Get("startAt") == "0" {
			jiratest.Reply(200, `{"startAt":0,"total":2,"isLast":false,"values":[{"id":"100","author":`+userOneJSON+`,"created":"2026-11-02T09:00:00.000+0000",
				"items":[{"field":"status","fieldId":"status","fieldtype":"jira","from":"1","fromString":"To Do","to":"3","toString":"In Progress"}]}]}`)(w, r, b)
			return
		}
		jiratest.Reply(200, `{"startAt":1,"total":2,"isLast":true,"values":[{"id":"101","created":"2026-11-03T09:00:00.000+0000",
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

// bulkSite answers a bulk changelog request for exactly the issues and
// fields it names: issue WEB-n is issue ID 10000+n with one change, entry
// n, holding one item per field asked for, whose value names the issue
// and the field. The first page answers the first half of the issues and
// names a next page for the rest. Entry n's time is milliseconds for an
// odd n, as the bulk fetch sends it, and a timestamp string for an even
// one. Every (issue, field) asked for is recorded.
type bulkSite struct {
	t      *testing.T
	mu     sync.Mutex
	asked  map[string]int // "WEB-n customfield_x" to the times asked
	shapes []string
}

func (s *bulkSite) handle(w http.ResponseWriter, r *http.Request, b []byte) {
	var req bulkRequest
	if err := json.Unmarshal(b, &req); err != nil {
		s.t.Fatal(err)
	}
	if len(req.Issues) == 0 || len(req.Issues) > 1000 || len(req.Fields) == 0 || len(req.Fields) > 10 || req.Max != 1000 {
		s.t.Errorf("a request of %d issues and %d fields, max %d", len(req.Issues), len(req.Fields), req.Max)
	}
	half := (len(req.Issues) + 1) / 2
	page, next := req.Issues[:half], `,"nextPageToken":"more"`
	switch req.Token {
	case "":
		s.mu.Lock()
		s.shapes = append(s.shapes, fmt.Sprintf("%d issues, %d fields", len(req.Issues), len(req.Fields)))
		for _, is := range req.Issues {
			for _, f := range req.Fields {
				s.asked[is+" "+f]++
			}
		}
		s.mu.Unlock()
	case "more":
		page, next = req.Issues[half:], ""
	default:
		s.t.Errorf("token %q", req.Token)
	}
	if half == len(req.Issues) {
		next = ""
	}
	var logs []string
	for _, is := range page {
		n, err := strconv.Atoi(strings.TrimPrefix(is, "WEB-"))
		if err != nil {
			s.t.Fatalf("issue %q", is)
		}
		var items []string
		for _, f := range req.Fields {
			items = append(items, fmt.Sprintf(`{"fieldId":%q,"field":"F","to":%q}`, f, is+"/"+f))
		}
		created := strconv.FormatInt(1762074000000+int64(n)*1000, 10)
		if n%2 == 0 {
			created = `"2026-11-02T09:00:00.000+0000"`
		}
		logs = append(logs, fmt.Sprintf(`{"issueId":"%d","changeHistories":[{"id":"%d","created":%s,"items":[%s]}]}`,
			10000+n, n, created, strings.Join(items, ",")))
	}
	jiratest.Reply(200, `{"issueChangeLogs":[`+strings.Join(logs, ",")+`]`+next+`}`)(w, r, b)
}

// TestBulkChangelogsSplits asks for 1001 issues and 11 fields: four
// requests (every issue group with every field group), each read to its
// last page, every issue and field asked for once, and each issue's
// entry, split across the field groups, joined again with one item per
// field.
func TestBulkChangelogsSplits(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	issues := webIssues(1, 1001)
	var fields []string
	for i := range 11 {
		fields = append(fields, fmt.Sprintf("customfield_%d", 10001+i))
	}
	site := &bulkSite{t: t, asked: map[string]int{}}
	f.On("POST", bulkChangelogURL, site.handle)
	res, err := c.BulkChangelogs(t.Context(), ChangelogQuery{Issues: issues, Fields: fields})
	if err != nil || !res.Complete {
		t.Fatalf("got %v", err)
	}
	want := []string{"1000 issues, 10 fields", "1000 issues, 1 fields", "1 issues, 10 fields", "1 issues, 1 fields"}
	if strings.Join(site.shapes, "\n") != strings.Join(want, "\n") || f.Count("POST", bulkChangelogURL) != 6 {
		t.Errorf("requests\n%s", strings.Join(site.shapes, "\n"))
	}
	if len(site.asked) != len(issues)*len(fields) || len(res.ByIssue) != len(issues) {
		t.Fatalf("asked for %d issue fields, got %d issues", len(site.asked), len(res.ByIssue))
	}
	for n, is := range issues {
		n++
		entries := res.ByIssue[strconv.Itoa(10000+n)]
		if len(entries) != 1 || entries[0].ID != strconv.Itoa(n) || len(entries[0].Items) != len(fields) || len(entries[0].Problems) != 0 {
			t.Fatalf("%s: entries %+v", is, entries)
		}
		for k, f := range fields {
			if site.asked[is+" "+f] != 1 {
				t.Errorf("%s %s asked for %d times", is, f, site.asked[is+" "+f])
			}
			if it := entries[0].Items[k]; it.FieldID != f || it.To != is+"/"+f {
				t.Errorf("%s: item %d is %+v", is, k, it)
			}
		}
	}
	if got := res.ByIssue["10001"][0].Created; !got.Equal(time.UnixMilli(1762074001000)) {
		t.Errorf("a time in milliseconds read as %v", got)
	}
	if got := res.ByIssue["10002"][0].Created; !got.Equal(time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("a timestamp read as %v", got)
	}
}

// TestBulkChangelogsJoinKeepsProblems proves an entry split across field
// groups keeps a problem found in any part: here its time does not parse
// in the second answer.
func TestBulkChangelogsJoinKeepsProblems(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	var fields []string
	for i := range 11 {
		fields = append(fields, fmt.Sprintf("customfield_%d", 10001+i))
	}
	f.On("POST", bulkChangelogURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		var req bulkRequest
		if err := json.Unmarshal(b, &req); err != nil || strings.Join(req.Issues, ",") != "WEB-1" || req.Max != 1000 || req.Token != "" {
			t.Errorf("a request %s", b)
		}
		created := `1762074000000`
		switch strings.Join(req.Fields, ",") {
		case strings.Join(fields[:10], ","):
		case fields[10]:
			created = `"later"`
		default:
			t.Errorf("fields %v", req.Fields)
		}
		jiratest.Reply(200, fmt.Sprintf(`{"issueChangeLogs":[{"issueId":"10001","changeHistories":[{"id":"1","created":%s,"items":[{"fieldId":%q,"to":"x"}]}]}]}`,
			created, req.Fields[0]))(w, r, b)
	})
	res, err := c.BulkChangelogs(t.Context(), ChangelogQuery{Issues: []string{"WEB-1"}, Fields: fields})
	entries := res.ByIssue["10001"]
	if err != nil || len(entries) != 1 || len(entries[0].Items) != 2 || problemIn(entries[0].Problems, "created") == nil || entries[0].Created.IsZero() {
		t.Fatalf("got %+v %v", res, err)
	}
}

// TestBulkChangelogsOrder proves an issue's entries come back oldest
// first, whatever order the pages hold them in, and entries of the same
// time in numeric ID order.
func TestBulkChangelogsOrder(t *testing.T) {
	f := jiratest.NewServer(t)
	c := newTestClient(t, f)
	entry := func(id, created string) string {
		return fmt.Sprintf(`{"id":%q,"created":%q,"items":[{"fieldId":"status","to":"3"}]}`, id, created)
	}
	f.On("POST", bulkChangelogURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		var req bulkRequest
		if err := json.Unmarshal(b, &req); err != nil || strings.Join(req.Issues, ",") != "WEB-1" || len(req.Fields) != 0 || req.Max != 1000 {
			t.Errorf("a request %s", b)
		}
		if req.Token == "" {
			jiratest.Reply(200, `{"issueChangeLogs":[{"issueId":"10001","changeHistories":[`+
				entry("10", "2026-11-02T09:00:00.000+0000")+","+entry("11", "2026-11-01T09:00:00.000+0000")+`]}],"nextPageToken":"more"}`)(w, r, b)
			return
		}
		if req.Token != "more" {
			t.Errorf("token %q", req.Token)
		}
		jiratest.Reply(200, `{"issueChangeLogs":[{"issueId":"10001","changeHistories":[`+entry("9", "2026-11-02T09:00:00.000+0000")+`]}]}`)(w, r, b)
	})
	res, err := c.BulkChangelogs(t.Context(), ChangelogQuery{Issues: []string{"WEB-1"}})
	var ids []string
	for _, e := range res.ByIssue["10001"] {
		ids = append(ids, e.ID)
	}
	if err != nil || strings.Join(ids, ",") != "11,9,10" {
		t.Errorf("got %v %v", ids, err)
	}
}

// TestChangeEntryTimes proves a change's time is read as milliseconds or
// a timestamp, never guessed from a number's size, and one that is
// neither is the entry's problem, with the rest of the entry kept.
func TestChangeEntryTimes(t *testing.T) {
	for raw, want := range map[string]time.Time{
		`1762074000000`:                  time.UnixMilli(1762074000000),
		`1762074000`:                     time.UnixMilli(1762074000),
		`"2026-11-02T09:00:00.000+0000"`: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC),
		`null`:                           {},
		``:                               {},
	} {
		e := changeEntryJSON{ID: "1", Created: json.RawMessage(raw)}.entry()
		if !e.Created.Equal(want) || len(e.Problems) != 0 {
			t.Errorf("%s: %+v", raw, e)
		}
	}
	for _, raw := range []string{`"yesterday"`, `true`, `1.5e12`, `{"ms":1}`} {
		e := changeEntryJSON{ID: "1", Created: json.RawMessage(raw), Items: []changeItemJSON{{FieldID: "status"}}}.entry()
		if p := problemIn(e.Problems, "created"); p == nil || !e.Created.IsZero() || len(e.Items) != 1 {
			t.Errorf("%s: %+v", raw, e)
		}
	}
}

// TestBulkChangelogsPageEnds proves a repeated token or an empty page
// that names a next one fails the walk, keeping what was read.
func TestBulkChangelogsPageEnds(t *testing.T) {
	for name, second := range map[string]string{
		"a repeated token": `{"issueChangeLogs":[{"issueId":"10001","changeHistories":[{"id":"2","created":"2026-11-01T09:00:00.000+0000","items":[]}]}],"nextPageToken":"t1"}`,
		"an empty page":    `{"issueChangeLogs":[],"nextPageToken":"t2"}`,
	} {
		f := jiratest.NewServer(t)
		c := newTestClient(t, f)
		n := 0
		f.On("POST", bulkChangelogURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
			if n++; n > 3 {
				t.Error("the client kept asking for pages")
				jiratest.Reply(400, `{}`)(w, r, b)
				return
			}
			switch n {
			case 1:
				jiratest.Reply(200, `{"issueChangeLogs":[{"issueId":"10001","changeHistories":[{"id":"1","created":"2026-11-01T09:00:00.000+0000","items":[]}]}],"nextPageToken":"t1"}`)(w, r, b)
				return
			case 3:
				// A last page: only the guard against the bad second page
				// fails the walk.
				jiratest.Reply(200, `{"issueChangeLogs":[]}`)(w, r, b)
				return
			}
			jiratest.Reply(200, second)(w, r, b)
		})
		res, err := c.BulkChangelogs(t.Context(), ChangelogQuery{Issues: []string{"WEB-1"}})
		if err == nil || res.Complete || len(res.ByIssue["10001"]) == 0 {
			t.Errorf("%s: %+v %v", name, res, err)
		}
	}
	f := jiratest.NewServer(t)
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
