package jiraprovider

import (
	"bytes"
	"errors"
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

// TestLookup proves a key the issue had before it moved finds the issue
// and a key that names nothing is unmatched, whether Jira answers the
// search naming it with a warning or a 400.
func TestLookup(t *testing.T) {
	for _, answer := range []string{"warned", "refused"} {
		t.Run(answer, func(t *testing.T) {
			f := lookupSite(t)
			p := newProvider(t, f, nil)
			f.On("POST", searchPath, func(w http.ResponseWriter, r *http.Request, b []byte) {
				if jql := bodyOf(t, b)["jql"]; jql != `key in ("web-1", "WEB-5", "WEB-9")` {
					t.Errorf("jql %v", jql)
				}
				if answer == "refused" {
					jiratest.Reply(400, `{"errorMessages":["An issue with key 'WEB-9' does not exist for field 'key'."]}`)(w, r, b)
					return
				}
				jiratest.Reply(200, mustJSON(map[string]any{"isLast": true,
					"issues":   []any{issue("10001", "WEB-1", updatedAt, nil), issue("10005", "APP-5", updatedAt, nil)},
					"warnings": []any{map[string]any{"type": "x", "message": "The issue key 'WEB-9' for field 'key' is invalid."}}}))(w, r, b)
			})
			got, err := p.Lookup(t.Context(), []string{"web-1", "WEB-5", "WEB-9", "10001", "WEB_x"})
			if err != nil {
				t.Fatal(err)
			}
			var found []string
			for _, m := range got.Found {
				found = append(found, m.Key+"="+m.Card.Key)
			}
			if !slices.Equal(found, []string{"web-1=WEB-1", "WEB-5=APP-5"}) || !slices.Equal(got.Unmatched, []string{"WEB-9", "10001", "WEB_x"}) {
				t.Errorf("found %v unmatched %v", found, got.Unmatched)
			}
			if n := f.Count("GET", "/rest/api/3/issue/10001"); n != 0 {
				t.Error("a number was read as an issue ID")
			}
		})
	}
}

// TestLookupFailures proves a search refused for another reason, and a
// single read that fails other than with a 404, fail the lookup.
func TestLookupFailures(t *testing.T) {
	f := lookupSite(t)
	p := newProvider(t, f, nil)
	f.On("POST", searchPath, jiratest.Reply(400, `{"errorMessages":["The JQL is too long."]}`))
	if _, err := p.Lookup(t.Context(), []string{"WEB-1"}); err == nil {
		t.Error("a refused search read as nothing found")
	}
	f.On("POST", searchPath, jiratest.Reply(200, `{"isLast":true,"issues":[]}`))
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
