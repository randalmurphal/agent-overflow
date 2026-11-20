package jiraprovider

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
	"agent-overflow/internal/tracker/jira/jiratest"
)

const updatedAt = "2026-11-10T10:00:00.000+0000"

// lookupSite answers single issue reads: WEB-1 and APP-5 exist, WEB-5 is
// APP-5's key before it moved, and any other key is not found.
func lookupSite(t *testing.T) *jiratest.Server {
	f, _ := newSite(t)
	moved := issue("10005", "APP-5", updatedAt, map[string]any{"project": map[string]any{"id": "10001", "key": "APP"}})
	for key, is := range map[string]map[string]any{"WEB-1": issue("10001", "WEB-1", updatedAt, nil), "web-1": issue("10001", "WEB-1", updatedAt, nil), "WEB-5": moved, "APP-5": moved} {
		f.On("GET", "/rest/api/3/issue/"+key, jiratest.Reply(200, mustJSON(is)))
	}
	f.On("GET", "/rest/api/3/issue/WEB-9", jiratest.Reply(404, `{"errorMessages":["Issue does not exist or you do not have permission to see it."]}`))
	return f
}

// TestLookup proves one bulk fetch of the shaped keys finds the issues
// returned under a key asked, case ignored; that a key it leaves out is
// read alone, finding an issue by its old key or nothing; and that two
// keys of one issue, its old and its current one, both find it.
func TestLookup(t *testing.T) {
	f := lookupSite(t)
	p := newProvider(t, f, nil)
	f.On("POST", bulkFetchPath, func(w http.ResponseWriter, r *http.Request, b []byte) {
		v := bodyOf(t, b)
		if keys := fmt.Sprint(v["issueIdsOrKeys"]); keys != "[web-1 WEB-5 WEB-9 APP-5]" {
			t.Errorf("keys %v", keys)
		}
		if fields := fmt.Sprint(v["fields"]); !strings.Contains(fields, "summary") {
			t.Errorf("fields %v", fields)
		}
		// As documented, the old key WEB-5 finds APP-5 under its current
		// key; WEB-9 names nothing and is left out (measured).
		jiratest.Reply(200, mustJSON(map[string]any{
			"issues":      []any{issue("10001", "WEB-1", updatedAt, nil), issue("10005", "APP-5", updatedAt, nil)},
			"issueErrors": []any{},
		}))(w, r, b)
	})
	got, err := p.Lookup(t.Context(), []string{"web-1", "WEB-5", "WEB-9", "10001", "WEB_x", "APP-5"})
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, m := range got.Found {
		found = append(found, m.Key+"="+m.Card.Key)
	}
	if !slices.Equal(found, []string{"web-1=WEB-1", "WEB-5=APP-5", "APP-5=APP-5"}) || !slices.Equal(got.Unmatched, []string{"WEB-9", "10001", "WEB_x"}) {
		t.Errorf("found %v unmatched %v", found, got.Unmatched)
	}
	reads := map[string]int{"/rest/api/3/issue/web-1": 0, "/rest/api/3/issue/APP-5": 0, "/rest/api/3/issue/WEB-5": 1, "/rest/api/3/issue/WEB-9": 1, "/rest/api/3/issue/10001": 0}
	for path, want := range reads {
		if n := f.Count("GET", path); n != want {
			t.Errorf("%s read alone %d times, want %d", path, n, want)
		}
	}
}

// TestLookupFailures proves a bulk fetch that fails, and a read alone
// that fails other than with a 404, fail the lookup, and that a key the
// bulk fetch could not return (an issueErrors entry) is read alone.
func TestLookupFailures(t *testing.T) {
	f := lookupSite(t)
	p := newProvider(t, f, nil)
	f.On("POST", bulkFetchPath, jiratest.Reply(400, `{"errorMessages":["Bad request."]}`))
	if _, err := p.Lookup(t.Context(), []string{"WEB-1"}); err == nil {
		t.Error("a refused bulk fetch read as nothing found")
	}
	f.On("POST", bulkFetchPath, jiratest.Reply(200, `{"issues":[],"issueErrors":[{"id":"WEB-2","errorMessage":"retry"}]}`))
	f.On("GET", "/rest/api/3/issue/WEB-2", jiratest.Reply(403, `{"errorMessages":["no"]}`))
	if _, err := p.Lookup(t.Context(), []string{"WEB-2"}); err == nil {
		t.Error("a refused read read as unmatched")
	}
	if _, err := p.Lookup(t.Context(), nil); !errors.Is(err, tracker.ErrInvalidInput) {
		t.Errorf("no keys: %v", err)
	}
}

// TestSearchEscapesText proves the search text reaches JQL only as
// escaped text search: operators, wildcards and quotes are words.
func TestSearchEscapesText(t *testing.T) {
	f, p := newSite(t)
	const text = `"login\" OR * page-2`
	f.On("POST", searchPath, func(w http.ResponseWriter, r *http.Request, b []byte) {
		v := bodyOf(t, b)
		want := "text ~ " + jira.QuoteJQLText(text) + ` AND project = "10000" ORDER BY updated DESC`
		if strings.Contains(want, " OR ") || !strings.Contains(want, `\\*`) {
			t.Errorf("the text was not escaped: %s", want)
		}
		if v["jql"] != want || v["maxResults"] != 20.0 || v["nextPageToken"] != "p2" {
			t.Errorf("search %s\nwant jql %s", b, want)
		}
		jiratest.Reply(200, page("p3", issue("10001", "WEB-1", updatedAt, nil)))(w, r, b)
	})
	res, err := p.Search(t.Context(), tracker.SearchQuery{Text: text, Project: "10000", Limit: 20, Page: "p2"})
	if err != nil || len(res.Cards) != 1 || res.Next != "p3" || len(res.Users) != 1 {
		t.Fatalf("got %+v %v", res, err)
	}
	if _, err := p.Search(t.Context(), tracker.SearchQuery{Text: "x", Project: "WEB"}); !errors.Is(err, tracker.ErrInvalidInput) {
		t.Errorf("a project key: %v", err)
	}
}

// TestDownloadChecksTheIssue proves an attachment is streamed only when
// it is the named issue's.
func TestDownloadChecksTheIssue(t *testing.T) {
	f, p := newSite(t)
	f.On("GET", issuePath, func(w http.ResponseWriter, r *http.Request, b []byte) {
		jiratest.WantQuery(t, r, "fields", "attachment")
		jiratest.Reply(200, `{"id":"10001","key":"WEB-1","fields":{"attachment":[{"id":"501","filename":"a.txt","mimeType":"text/plain","size":5,"created":"2026-11-10T10:00:00.000+0000"}]}}`)(w, r, b)
	})
	f.On("GET", "/rest/api/3/attachment/content/501", func(w http.ResponseWriter, r *http.Request, b []byte) {
		_, _ = w.Write([]byte("hello"))
	})
	var buf bytes.Buffer
	if err := p.Download(t.Context(), tracker.DownloadInput{Issue: "10001", Attachment: "501", To: &buf, Limit: 10}); err != nil || buf.String() != "hello" {
		t.Fatalf("got %q %v", buf.String(), err)
	}
	err := p.Download(t.Context(), tracker.DownloadInput{Issue: "10001", Attachment: "502", To: &buf, Limit: 10})
	if !errors.Is(err, tracker.ErrInvalidInput) || !strings.Contains(err.Error(), "not issue 10001's") {
		t.Errorf("another issue's attachment: %v", err)
	}
	if n := f.Count("GET", "/rest/api/3/attachment/content/502"); n != 0 {
		t.Error("another issue's attachment was downloaded")
	}
}
