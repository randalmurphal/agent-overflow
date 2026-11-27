package tempo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker/trackerapi"
)

// tokenTurns hands out user a's token on the first read, b's on the
// second and c's on every later one.
type tokenTurns struct{ reads atomic.Int32 }

func (s *tokenTurns) Credential(context.Context) (*trackerapi.Credential, error) {
	user := string(rune('a' + min(s.reads.Add(1)-1, 2)))
	return trackerapi.Bearer(secret.New([]byte("fake-tempo-" + user))), nil
}

// walkServer is a fake Tempo that answers each request by the user whose
// token it carries ("a", "b" or "c"), and refuses with 401 a request of a
// user who used up its budget of answered requests, so the transport
// reads the token again and asks as the next user. answer returns the
// results of the page at offset and whether another follows.
func walkServer(t *testing.T, budget map[string]int, answer func(user, path string, offset int) (results []string, more bool)) *Client {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		user := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer fake-tempo-")
		mu.Lock()
		left, limited := budget[user]
		if limited {
			budget[user] = left - 1
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if limited && left <= 0 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"errors":[{"message":"unauthorized"}]}`)
			return
		}
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		results, more := answer(user, r.URL.Path, offset)
		next := ""
		if more {
			next = "https://api.example.com/next"
		}
		_, _ = fmt.Fprintf(w, `{"metadata":{"count":%d,"offset":%d,"next":%q},"results":[%s]}`, len(results), offset, next, strings.Join(results, ","))
	}))
	t.Cleanup(srv.Close)
	opts, err := transportOptions(Options{BaseURL: srv.URL, Version: "test", Credentials: &tokenTurns{}, Issues: newIssues()})
	if err != nil {
		t.Fatal(err)
	}
	opts.MaxPerSecond = 0
	c, err := newClient(opts, newIssues())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

// uid is item n of user's listing: 101 is a's first, 202 b's second.
func uid(user string, n int) int64 { return int64(user[0]-'a'+1)*100 + int64(n) }

// twoPages answers path with two pages of one item each.
func twoPages(path string, item func(id int64) string) func(user, p string, offset int) ([]string, bool) {
	return func(user, p string, offset int) ([]string, bool) {
		if p != path {
			return nil, false
		}
		return []string{item(uid(user, offset+1))}, offset == 0
	}
}

func worklogItem(id int64) string {
	return wl{id: id, issue: 10001, author: "acc-1", date: "2026-11-10", spent: 60}.json()
}

var walkQuery = func() WorklogQuery {
	from, err := ParseDate("2026-11-01")
	if err != nil {
		panic(err)
	}
	return WorklogQuery{Author: "acc-1", From: from, To: from.AddDays(29)}
}()

// walkCases are every walk of the client: each read composed of several
// pages whose answers must be one tenant's. want is user b's whole answer.
var walkCases = map[string]struct {
	path string
	item func(id int64) string
	read func(ctx context.Context, c *Client) (string, error)
	want string
}{
	"Attributes": {
		"/4/work-attributes",
		func(id int64) string {
			return fmt.Sprintf(`{"key":"_K%d_","name":"N","type":"INPUT_FIELD","required":false}`, id)
		},
		func(ctx context.Context, c *Client) (string, error) {
			l, err := c.Attributes(ctx)
			keys := make([]string, len(l))
			for i, a := range l {
				keys[i] = a.Key
			}
			return strings.Join(keys, ","), err
		}, "_K201_,_K202_",
	},
	"Accounts": {
		"/4/accounts/search",
		func(id int64) string { return fmt.Sprintf(`{"id":%d,"key":"K%d","name":"N","status":"OPEN"}`, id, id) },
		func(ctx context.Context, c *Client) (string, error) {
			l, err := c.Accounts(ctx, AccountQuery{})
			return fmt.Sprint(slices.Collect(func(yield func(int64) bool) {
				for _, a := range l {
					yield(a.ID)
				}
			})), err
		}, "[201 202]",
	},
	"IssueWorklogs": {
		"/4/worklogs/search", worklogItem,
		func(ctx context.Context, c *Client) (string, error) {
			l, err := c.IssueWorklogs(ctx, IssueWorklogQuery{Issues: []int64{10101}})
			return fmt.Sprint(slices.Collect(func(yield func(int64) bool) {
				for _, w := range l {
					yield(w.ID)
				}
			})), err
		}, "[201 202]",
	},
	"Worklogs": {
		"/4/worklogs/user/acc-1", worklogItem,
		func(ctx context.Context, c *Client) (string, error) {
			l, err := c.Worklogs(ctx, walkQuery)
			return fmt.Sprint(slices.Collect(func(yield func(int64) bool) {
				for _, w := range l {
					yield(w.ID)
				}
			})), err
		}, "[201 202]",
	},
}

// TestWalkStartsAgainUnderOneCredential runs every walk while the token
// changes between its first and second page (user a's is refused after
// one answer, and the transport reads user b's): the walk starts again
// under b and returns b's whole list, never a's first page with b's
// second, which would be two tenants' lists as one.
func TestWalkStartsAgainUnderOneCredential(t *testing.T) {
	for name, wc := range walkCases {
		t.Run(name, func(t *testing.T) {
			c := walkServer(t, map[string]int{"a": 1}, twoPages(wc.path, wc.item))
			got, err := wc.read(t.Context(), c)
			if err != nil || got != wc.want {
				t.Fatalf("got %q, %v; want %q", got, err, wc.want)
			}
		})
	}
}

// TestWalkFailsWhenTheCredentialChangesAgain: a walk whose start over
// sees the token change again fails with an error asking again
// resolves, and returns nothing.
func TestWalkFailsWhenTheCredentialChangesAgain(t *testing.T) {
	// a answers the first page; b the second, so the walk starts again,
	// and the first page again; c the second again.
	c := walkServer(t, map[string]int{"a": 1, "b": 2}, twoPages("/4/worklogs/user/acc-1", worklogItem))
	l, err := c.Worklogs(t.Context(), walkQuery)
	if !errors.Is(err, trackerapi.ErrCredentialChanged) || l != nil {
		t.Fatalf("got %d worklogs, %v; want none and ErrCredentialChanged", len(l), err)
	}
}

// TestWorklogPagesFailsWhenTheCredentialChanges: WorklogPages hands each
// page over as it reads it, so it cannot start again; a page under
// another token than the first fails the listing instead of handing
// over another tenant's page as the rest of the list.
func TestWorklogPagesFailsWhenTheCredentialChanges(t *testing.T) {
	c := walkServer(t, map[string]int{"a": 1}, twoPages("/4/worklogs/user/acc-1", worklogItem))
	var seen []int64
	err := c.WorklogPages(t.Context(), walkQuery, func(page []Worklog) error {
		for _, w := range page {
			seen = append(seen, w.ID)
		}
		return nil
	})
	if !errors.Is(err, trackerapi.ErrCredentialChanged) || !slices.Equal(seen, []int64{101}) {
		t.Fatalf("handed over %v, %v; want [101] and ErrCredentialChanged", seen, err)
	}
}

// The other exported Client methods, by why they are no walk. A method
// added to Client fails TestEveryWalkIsListed until it is named in
// walkCases or here.
var (
	// pagesToTheCaller hand each page over as it is read; see
	// TestWorklogPagesFailsWhenTheCredentialChanges.
	pagesToTheCaller = []string{"WorklogPages"}
	// oneRequest reads answer from one request (or the site cache).
	oneRequest = []string{"Approval", "Schedule", "Settings", "Worklog"}
	// writes and their checks compose a write, not a read's answer; the
	// walks they read through are walks of their own.
	writes    = []string{"Apply", "Create", "Delete", "Update", "Validate"}
	noRequest = []string{"Close", "DropCache", "ForgetCredential"}
)

// TestEveryWalkIsListed fails for an exported Client method that is
// neither a walk with a case in walkCases nor named in a list of the
// methods that are not walks.
func TestEveryWalkIsListed(t *testing.T) {
	named := map[string]string{}
	for name := range walkCases {
		named[name] = "walk"
	}
	for kind, list := range map[string][]string{"pages": pagesToTheCaller, "one": oneRequest, "write": writes, "none": noRequest} {
		for _, name := range list {
			if prev, ok := named[name]; ok {
				t.Errorf("%s is listed as %s and %s", name, prev, kind)
			}
			named[name] = kind
		}
	}
	typ := reflect.TypeFor[*Client]()
	for i := range typ.NumMethod() {
		if name := typ.Method(i).Name; named[name] == "" {
			t.Errorf("Client.%s is neither a walk with a case nor listed as no walk", name)
		}
	}
	for name := range named {
		if _, ok := typ.MethodByName(name); !ok {
			t.Errorf("%s names no Client method", name)
		}
	}
}

// twoMarks are the credential reads of two tokens of one transport.
func twoMarks(t *testing.T) (a, b trackerapi.CredentialMark) {
	c := walkServer(t, map[string]int{}, func(string, string, int) ([]string, bool) { return nil, false })
	read := func() trackerapi.CredentialMark {
		var v map[string]any
		resp, err := c.http.JSON(t.Context(), trackerapi.Request{Path: "4/globalconfiguration"}, &v)
		if err != nil {
			t.Fatal(err)
		}
		return resp.CredentialMark()
	}
	a = read()
	c.ForgetCredential()
	b = read()
	if a == b || a == (trackerapi.CredentialMark{}) || b == (trackerapi.CredentialMark{}) {
		t.Fatalf("marks %v and %v are not two reads", a, b)
	}
	return a, b
}

// TestSharedLoadIsTheCurrentCredentials: a site cache read drawn under
// token a whose delivery comes after the transport moved to token b is
// taken by none of its callers, the one that started it under a or the
// one that joined it under b; both read again and get b's value. The
// transport fails an answer that arrives after its token was dropped, so
// what this covers is an answer read under a whose delivery the change
// overtakes, which the load here stands in for by being held open.
func TestSharedLoadIsTheCurrentCredentials(t *testing.T) {
	markA, markB := twoMarks(t)
	synctest.Test(t, func(t *testing.T) {
		var (
			mu    sync.Mutex
			held  = markA
			loads atomic.Int32
		)
		now := func() trackerapi.CredentialMark {
			mu.Lock()
			defer mu.Unlock()
			return held
		}
		release := make(chan struct{})
		load := func(context.Context) (string, trackerapi.CredentialMark, error) {
			if loads.Add(1) == 1 {
				<-release
				return "a", markA, nil
			}
			return "b", markB, nil
		}
		var c cached[string]
		got := make(chan string, 2)
		get := func() {
			v, err := c.get(context.Background(), now, load)
			if err != nil {
				t.Error(err)
			}
			got <- v
		}
		go get() // starts a's read
		synctest.Wait()
		mu.Lock()
		held = markB
		mu.Unlock()
		go get() // joins it under b
		synctest.Wait()
		if n := loads.Load(); n != 1 {
			t.Fatalf("%d reads before the release, want 1: the second caller did not join", n)
		}
		close(release)
		for range 2 {
			if v := <-got; v != "b" {
				t.Errorf("a caller got %q, want b's value", v)
			}
		}
		c.shut()
	})
}
