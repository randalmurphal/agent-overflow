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
		jiratest.Reply(200, answer(user, r))(w, r, b)
	})
}

// twoChunkRows serves board 3 to accounts a and b: a sees issue 10001,
// which b cannot, and both see issue 10501, in the second of the two
// requests a read of 10001 to 10501 makes. a answers budget requests.
func twoChunkRows(t *testing.T, budget map[string]int) (*accountSite, tracker.Provider, []tracker.IssueID) {
	t.Helper()
	s := &accountSite{Server: jiratest.NewServer(t), budget: budget}
	s.on("GET", fieldPath, func(string, *http.Request) string { return fieldsJSON })
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
	c, err := jira.New(jira.Options{BaseURL: s.URL(), Version: "test", Credentials: &userTurns{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	p, err := New(Options{Jira: c, Site: "https://example.atlassian.net"})
	if err != nil {
		t.Fatal(err)
	}
	var ids []tracker.IssueID
	for n := 10001; n <= 10501; n++ {
		ids = append(ids, tracker.IssueID(strconv.Itoa(n)))
	}
	return s, tracker.OneCredential(p), ids
}

// TestBoardRowsUnderOneCredential: the first request of a read of two
// requests answers under account a, the second is refused for a and
// answered under b. The read starts again under b and holds b's rows
// alone, never a's issue beside b's.
func TestBoardRowsUnderOneCredential(t *testing.T) {
	// a answers the board, the catalog and the first rows.
	_, p, ids := twoChunkRows(t, map[string]int{"a": 3})
	rows, err := p.BoardRows(t.Context(), "3", ids)
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
	rows, err := p.BoardRows(t.Context(), "3", ids)
	if !errors.Is(err, trackerapi.ErrCredentialChanged) || len(rows.Rows) != 0 {
		t.Fatalf("got %d rows, %v; want none and ErrCredentialChanged", len(rows.Rows), err)
	}
}
