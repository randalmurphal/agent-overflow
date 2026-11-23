package linear

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker/linear/lineartest"
	"agent-overflow/internal/tracker/trackerapi"
)

// keyTurns hands out key a on the first read, b on the second and c on
// every later one: three users of one workspace.
type keyTurns struct{ reads atomic.Int32 }

func turnKey(user string) string { return "lin_api_fake_user_" + user }

func (s *keyTurns) Credential(context.Context) (*trackerapi.Credential, error) {
	user := string(rune('a' + min(s.reads.Add(1)-1, 2)))
	return trackerapi.Key(secret.New([]byte(turnKey(user)))), nil
}

// turnClient is an unbound client of f reading keys from src.
func turnClient(t *testing.T, f *fake, src trackerapi.CredentialSource) *Client {
	t.Helper()
	c, err := New(Options{BaseURL: f.URL(), Version: "test", Credentials: src})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

// refusedFor is ex answered 401 for user's key, which makes the transport
// read the key again and send the request once more.
func refusedFor(ex lineartest.Exchange, user string) lineartest.Exchange {
	ex.Key, ex.Status, ex.Body = turnKey(user), 401, `{"errors":[{"message":"Authentication required"}]}`
	return ex
}

// as is ex answered for user's key with body.
func as(ex lineartest.Exchange, user, body string) lineartest.Exchange {
	ex.Key, ex.Body = turnKey(user), body
	return ex
}

// historyBatches are the two requests of an IssueHistories read of
// historyBatchIssues+1 IDs, and the IDs.
func historyBatches() (ids []string, first, second lineartest.Exchange) {
	for i := 1; i <= historyBatchIssues+1; i++ {
		ids = append(ids, uuidN(i))
	}
	list := func(ids []string) string { return `["` + strings.Join(ids, `","`) + `"]` }
	first = lineartest.Exchange{Op: "IssueHistories", Vars: `{"filter":{"and":[{"id":{"in":` + list(ids[:historyBatchIssues]) + `}}]},"first":50,"entries":250}`}
	second = lineartest.Exchange{Op: "IssueHistories", Vars: `{"filter":{"and":[{"id":{"in":` + list(ids[historyBatchIssues:]) + `}}]},"first":1,"entries":250}`}
	return ids, first, second
}

// historyOf is a batch answer holding issue id alone, on team.
func historyOf(id, team string) string {
	return lineartest.Data(lineartest.PageJSON([]string{historiesNode(id, team, nil, false, "")}, false, ""))
}

// TestIssueHistoriesUnderOneKey: the first batch answers under key a,
// the second is refused for a and answered under b. The read starts
// again under b and returns only what b sees: never a's issue in a team
// only a can see beside b's.
func TestIssueHistoriesUnderOneKey(t *testing.T) {
	t.Parallel()
	ids, first, second := historyBatches()
	f := newFake(t,
		as(first, "a", historyOf(uuidN(1), "team-a-only")),
		refusedFor(second, "a"),
		as(second, "b", historyOf(uuidN(historyBatchIssues+1), "team-web")),
		as(first, "b", lineartest.Data(lineartest.PageJSON(nil, false, ""))),
		as(second, "b", historyOf(uuidN(historyBatchIssues+1), "team-web")),
	)
	got, err := turnClient(t, f, &keyTurns{}).IssueHistories(t.Context(), ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].IssueID != uuidN(historyBatchIssues+1) || got[0].TeamID != "team-web" {
		t.Fatalf("got %+v, want b's one issue alone", got)
	}
}

// TestCollectUnderOneKey: a catalog's second page answered under another
// key than its first starts the listing again under that key, so the
// list is one user's.
func TestCollectUnderOneKey(t *testing.T) {
	t.Parallel()
	team := func(key string) string { return lineartest.NewTeam(key).JSON() }
	page1 := lineartest.Exchange{Op: "Teams", Vars: `{"first":100}`}
	page2 := lineartest.Exchange{Op: "Teams", Vars: `{"first":100,"after":"cursor-1"}`}
	f := newFake(t,
		as(page1, "a", lineartest.Data(lineartest.PageJSON([]string{team("SECRET")}, true, "cursor-1"))),
		refusedFor(page2, "a"),
		as(page2, "b", lineartest.Data(lineartest.PageJSON([]string{team("APP")}, false, ""))),
		as(page1, "b", lineartest.Data(lineartest.PageJSON([]string{team("WEB")}, true, "cursor-1"))),
		as(page2, "b", lineartest.Data(lineartest.PageJSON([]string{team("APP")}, false, ""))),
	)
	got, err := turnClient(t, f, &keyTurns{}).Teams(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, tm := range got {
		keys = append(keys, tm.Key)
	}
	if strings.Join(keys, ",") != "WEB,APP" {
		t.Fatalf("teams %v, want b's WEB,APP", keys)
	}
}

// TestWalkFailsWhenTheKeyChangesAgain: a key that changes again during
// the read that started over fails it with an error asking again
// resolves, returning nothing.
func TestWalkFailsWhenTheKeyChangesAgain(t *testing.T) {
	t.Parallel()
	ids, first, second := historyBatches()
	f := newFake(t,
		as(first, "a", historyOf(uuidN(1), "team-web")),
		refusedFor(second, "a"),
		as(second, "b", historyOf(uuidN(historyBatchIssues+1), "team-web")),
		as(first, "b", historyOf(uuidN(1), "team-web")),
		refusedFor(second, "b"),
		as(second, "c", historyOf(uuidN(historyBatchIssues+1), "team-web")),
	)
	got, err := turnClient(t, f, &keyTurns{}).IssueHistories(t.Context(), ids)
	if !errors.Is(err, trackerapi.ErrCredentialChanged) || got != nil {
		t.Fatalf("got %+v, %v; want nothing and ErrCredentialChanged", got, err)
	}
}
