package linear

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker/trackerapi"
)

// testKey is the invented personal key every test client sends.
const testKey = "lin_api_fake0000000000"

// exchange is one request the fake expects and its answer.
type exchange struct {
	op string
	// vars is the exact variables object expected, as JSON; empty means
	// the request carries none.
	vars string
	// varsFn, when set, gives the exact expected variables at the time
	// the request arrives, for a script whose cursors depend on earlier
	// answers.
	varsFn func() string
	status int // zero is 200
	header map[string]string
	body   string
	// respond, when set, answers instead of body; it gets the request's
	// variables.
	respond func(vars json.RawMessage) string
	// wait, when set, holds the answer until the request ends, after
	// telling arrived that the request is in.
	wait    bool
	arrived chan<- struct{}
}

// fake is a loopback Linear. Each request must match an expected
// exchange by operation name and exact variables, and each exchange
// answers once; any other request fails the test. Every document it
// receives is checked against the schema.
type fake struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	pending []*exchange
	served  int
}

func newFake(t *testing.T, script ...exchange) *fake {
	t.Helper()
	f := &fake{t: t}
	for i := range script {
		f.pending = append(f.pending, &script[i])
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(func() {
		f.srv.Close()
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, e := range f.pending {
			t.Errorf("expected %s %s was never requested", e.op, e.vars)
		}
	})
	return f
}

func (f *fake) client(t *testing.T) *Client {
	t.Helper()
	c, err := New(Options{
		BaseURL:     f.srv.URL,
		Version:     "test",
		Credentials: trackerapi.StaticCredentials(trackerapi.Key(secret.New([]byte(testKey)))),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func (f *fake) serve(w http.ResponseWriter, r *http.Request) {
	fail := func(format string, args ...any) {
		f.t.Errorf("fake Linear: "+format, args...)
		http.Error(w, "unexpected request", http.StatusTeapot)
	}
	if r.Method != http.MethodPost || r.URL.Path != "/graphql" || r.URL.RawQuery != "" {
		fail("unexpected %s %s", r.Method, r.URL)
		return
	}
	if got := r.Header.Get("Authorization"); got != testKey {
		fail("Authorization %q, want the bare key", got)
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/json" {
		fail("Content-Type %q", ct)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		fail("read body: %v", err)
		return
	}
	var req struct {
		Query         string          `json:"query"`
		OperationName string          `json:"operationName"`
		Variables     json.RawMessage `json:"variables"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		fail("body is not a GraphQL request: %v", err)
		return
	}
	if problems := documentProblems(req.Query, req.OperationName); len(problems) > 0 {
		fail("document %s: %s", req.OperationName, strings.Join(problems, "; "))
		return
	}
	exercised.Store(req.OperationName, true)
	e := f.take(req.OperationName, req.Variables)
	if e == nil {
		fail("unexpected %s with variables %s", req.OperationName, req.Variables)
		return
	}
	if e.wait {
		if e.arrived != nil {
			e.arrived <- struct{}{}
		}
		<-r.Context().Done()
		return
	}
	for k, v := range e.header {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "application/json")
	status := e.status
	if status == 0 {
		status = http.StatusOK
	}
	answer := e.body
	if e.respond != nil {
		answer = e.respond(req.Variables)
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, answer)
}

// take removes and returns the first pending exchange for op whose
// variables equal vars.
func (f *fake) take(op string, vars json.RawMessage) *exchange {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, e := range f.pending {
		want := e.vars
		if e.varsFn != nil {
			want = e.varsFn()
		}
		if e.op == op && sameJSON(want, vars) {
			f.pending = append(f.pending[:i], f.pending[i+1:]...)
			f.served++
			return e
		}
	}
	return nil
}

func sameJSON(want string, got json.RawMessage) bool {
	if want == "" {
		return len(got) == 0 || string(got) == "null"
	}
	var a, b any
	if json.Unmarshal([]byte(want), &a) != nil || json.Unmarshal(got, &b) != nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}

// data wraps fields as a GraphQL answer's data.
func data(fields string) string { return `{"data":{` + fields + `}}` }

// pageJSON is a connection's JSON, aliased as page.
func pageJSON(nodes []string, hasNext bool, cursor string) string {
	return fmt.Sprintf(`"page":{"nodes":[%s],"pageInfo":{"hasNextPage":%t,"endCursor":%q}}`, strings.Join(nodes, ","), hasNext, cursor)
}

// issueJSON is an invented issue with the fields issueFields selects.
func issueJSON(id, identifier, updatedAt string, sortOrder float64) string {
	return fmt.Sprintf(`{"id":%q,"identifier":%q,"number":1,"title":"Fix the login page","description":"Steps:\n1. open","url":"https://linear.app/example/issue/%s","branchName":"user1/%s",`+
		`"priority":2,"estimate":3,"sortOrder":%v,"prioritySortOrder":-10.5,"subIssueSortOrder":null,`+
		`"team":{"id":"team-web"},"state":{"id":"state-todo"},"assignee":{"id":"user-1"},"creator":{"id":"user-1"},`+
		`"cycle":null,"project":{"id":"project-1"},"parent":null,"labelIds":["label-bug"],"dueDate":"2026-12-01",`+
		`"createdAt":"2026-11-01T09:00:00.000Z","updatedAt":%q,"startedAt":null,"completedAt":null,"canceledAt":null,"archivedAt":null,"trashed":null}`,
		id, identifier, identifier, identifier, sortOrder, updatedAt)
}
