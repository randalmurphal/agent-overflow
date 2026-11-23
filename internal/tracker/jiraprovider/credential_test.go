package jiraprovider

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
	"agent-overflow/internal/tracker/jira/jiratest"
	"agent-overflow/internal/tracker/trackerapi"
)

// userTurns hands out user a's credential on the first read, b's on the
// second and c's on every later one: three accounts of one site.
type userTurns struct{ reads atomic.Int32 }

func (s *userTurns) Credential(context.Context) (*trackerapi.Credential, error) {
	user := string(rune('a' + min(s.reads.Add(1)-1, 2)))
	return trackerapi.Basic(user+"@example.com", secret.New([]byte("fake-token-"+user))), nil
}

// accountSite is a fake Jira that answers by the account a request is
// made as, and refuses with 401 a request of an account that used up its
// budget of answered requests, so the transport reads the credential
// again and asks as the next account.
type accountSite struct {
	*jiratest.Server
	mu     sync.Mutex
	budget map[string]int
}

func (s *accountSite) on(method, path string, answer func(user string, r *http.Request) string) {
	s.reply(method, path, func(user string, r *http.Request) (int, string) { return 200, answer(user, r) })
}

// reply is on with the answer's status.
func (s *accountSite) reply(method, path string, answer func(user string, r *http.Request) (int, string)) {
	s.On(method, path, func(w http.ResponseWriter, r *http.Request, b []byte) {
		name, _, _ := r.BasicAuth()
		user, _, _ := strings.Cut(name, "@")
		s.mu.Lock()
		left, limited := s.budget[user]
		if limited {
			s.budget[user] = left - 1
		}
		s.mu.Unlock()
		if limited && left <= 0 {
			jiratest.Reply(401, `{"errorMessages":["Client must be authenticated"]}`)(w, r, b)
			return
		}
		status, body := answer(user, r)
		jiratest.Reply(status, body)(w, r, b)
	})
}

// accountProvider is a provider on an accountSite, every read of it one
// walk, whose account a answers budget["a"] requests.
func accountProvider(t *testing.T, budget map[string]int) (*accountSite, tracker.Provider) {
	t.Helper()
	s := &accountSite{Server: jiratest.NewServer(t), budget: budget}
	s.on("GET", fieldPath, func(string, *http.Request) string { return fieldsJSON })
	c, err := jira.New(jira.Options{BaseURL: s.URL(), Version: "test", Credentials: &userTurns{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	p, err := New(Options{Jira: c, Site: "https://example.atlassian.net", SprintsFrom: allSprints})
	if err != nil {
		t.Fatal(err)
	}
	return s, tracker.OneCredential(p)
}

// twoChunkRows serves board 3 to accounts a and b: a sees issue 10001,
// which b cannot, and both see issue 10501, in the second of the two
// requests a read of 10001 to 10501 makes. a answers budget requests.
func twoChunkRows(t *testing.T, budget map[string]int) (*accountSite, tracker.Provider, []tracker.IssueID) {
	t.Helper()
	s, p := accountProvider(t, budget)
	s.on("GET", board3Config, func(string, *http.Request) string { return boardConfig(3, "customfield_10016") })
	s.on("GET", board3Issues, func(user string, r *http.Request) string {
		if strings.Contains(r.URL.Query().Get("jql"), "10501") {
			return page("", issue("10501", "WEB-501", "2026-11-10T10:00:00.000+0000", nil))
		}
		if user == "a" {
			return page("", issue("10001", "WEB-1", "2026-11-10T10:00:00.000+0000", nil))
		}
		return page("")
	})
	// No account's board holds an issue its list left out.
	s.on("POST", "/rest/api/3/jql/match", func(string, *http.Request) string {
		return `{"matches":[{"matchedIssues":[],"errors":[]}]}`
	})
	var ids []tracker.IssueID
	for n := 10001; n <= 10501; n++ {
		ids = append(ids, tracker.IssueID(strconv.Itoa(n)))
	}
	return s, p, ids
}

// TestLookupUnderOneCredential: the bulk fetch answers under account a,
// which sees WEB-1, and the read alone of the key it left out is refused
// for a and answered not found under b, which sees neither issue. A
// not-found is an answer the lookup reports, so the lookup starts again
// under b and matches no key, never a's card beside b's not-found.
func TestLookupUnderOneCredential(t *testing.T) {
	// a answers the catalog and the bulk fetch.
	s, p := accountProvider(t, map[string]int{"a": 2})
	s.on("POST", bulkFetchPath, func(user string, _ *http.Request) string {
		if user == "a" {
			return mustJSON(map[string]any{"issues": []any{issue("10001", "WEB-1", updatedAt, nil)}, "issueErrors": []any{}})
		}
		return `{"issues":[],"issueErrors":[]}`
	})
	for _, key := range []string{"WEB-1", "WEB-9"} {
		s.reply("GET", "/rest/api/3/issue/"+key, func(string, *http.Request) (int, string) { return 404, notFoundJSON })
	}
	got, err := p.Lookup(t.Context(), []string{"WEB-1", "WEB-9"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Found) != 0 || len(got.Unmatched) != 2 {
		t.Fatalf("found %d and %d unmatched, want b's answer: neither key matched", len(got.Found), len(got.Unmatched))
	}
}

// TestNewRowsUnderOneCredential: board 3's configuration answers under
// account a, for which it maps no status, and board 4's is refused for a
// and answered under b, for which both boards hold the issue. NewRows
// keeps a board it could not read as Unknown and returns no error, which
// does not hide the change: the placement starts again under b.
func TestNewRowsUnderOneCredential(t *testing.T) {
	s, p := accountProvider(t, map[string]int{"a": 1})
	s.on("GET", board3Config, func(user string, _ *http.Request) string {
		if user == "a" {
			return `{"id":3,"name":"Board 3","type":"scrum","filter":{"id":"13"},"columnConfig":{"columns":[]},"ranking":{"rankCustomFieldId":10019}}`
		}
		return boardConfig(3, "customfield_10016")
	})
	s.on("GET", board4Config, func(string, *http.Request) string { return boardConfig(4, "customfield_10016") })
	s.on("POST", matchPath, func(string, *http.Request) string {
		return `{"matches":[{"matchedIssues":[10005],"errors":[]},{"matchedIssues":[10005],"errors":[]}]}`
	})
	s.on("POST", bulkFetchPath, func(string, *http.Request) string { return page("", listedIssue("api/3")) })
	got, err := p.NewRows(t.Context(), "10005", []tracker.BoardID{"3", "4"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Rows) != 2 || len(got.Off) != 0 || len(got.Unknown) != 0 {
		t.Fatalf("placement %+v, want b's: a row on both boards", got)
	}
}

// TestBoardRowsUnderOneCredential: the first request of a read of two
// requests answers under account a, the second is refused for a and
// answered under b. The read starts again under b and holds b's rows
// alone, never a's issue beside b's.
func TestBoardRowsUnderOneCredential(t *testing.T) {
	// a answers the board, the catalog and the first rows.
	_, p, ids := twoChunkRows(t, map[string]int{"a": 3})
	rows, err := p.BoardRows(t.Context(), "3", ids, tracker.Eligibility{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Rows) != 1 || rows.Rows[0].Card.ID != "10501" || len(rows.Missing) != 500 {
		got := make([]tracker.IssueID, len(rows.Rows))
		for i, r := range rows.Rows {
			got[i] = r.Card.ID
		}
		t.Fatalf("rows %v with %d missing, want b's 10501 alone", got, len(rows.Missing))
	}
}

// TestBoardRowsFailWhenTheCredentialChangesAgain: a credential that
// changes again during the read that started over fails it with an
// error asking again resolves.
func TestBoardRowsFailWhenTheCredentialChangesAgain(t *testing.T) {
	_, p, ids := twoChunkRows(t, map[string]int{"a": 3, "b": 4})
	rows, err := p.BoardRows(t.Context(), "3", ids, tracker.Eligibility{})
	if !errors.Is(err, trackerapi.ErrCredentialChanged) || len(rows.Rows) != 0 {
		t.Fatalf("got %d rows, %v; want none and ErrCredentialChanged", len(rows.Rows), err)
	}
}
