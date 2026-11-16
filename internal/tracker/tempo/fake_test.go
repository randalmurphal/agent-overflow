package tempo

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"sync"
	"testing"

	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker/trackerapi"
)

const testToken = "fake-tempo-token"

// exchange is one request the fake expects and its answer.
type exchange struct {
	method string
	path   string
	query  url.Values // exact; nil expects none
	body   string     // JSON compared by value; "" expects no body
	status int
	resp   string
	header http.Header
	// repeat answers every matching request instead of only the first.
	repeat bool
	// during runs while the request is being answered, before the answer
	// is written.
	during func()
	used   int
}

// fakeTempo is a loopback Tempo that answers only the exchanges a test
// names and fails the test on any other request or an unused exchange.
type fakeTempo struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	exchanges []*exchange
	log       []string
}

func newFake(t *testing.T) *fakeTempo {
	t.Helper()
	f := &fakeTempo{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(func() {
		f.srv.Close()
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, e := range f.exchanges {
			if e.used == 0 {
				t.Errorf("expected %s %s?%s was never requested", e.method, e.path, e.query.Encode())
			}
		}
	})
	return f
}

// on adds an exchange answered with status and resp.
func (f *fakeTempo) on(method, path string, query url.Values, body string, status int, resp string) *exchange {
	e := &exchange{method: method, path: path, query: query, body: body, status: status, resp: resp}
	f.mu.Lock()
	f.exchanges = append(f.exchanges, e)
	f.mu.Unlock()
	return e
}

func (f *fakeTempo) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
		f.t.Errorf("%s %s: Authorization %q", r.Method, r.URL.Path, got)
	}
	f.mu.Lock()
	f.log = append(f.log, r.Method+" "+r.URL.EscapedPath())
	var match *exchange
	for _, e := range f.exchanges {
		if (e.used == 0 || e.repeat) && e.method == r.Method && e.path == r.URL.EscapedPath() && sameQuery(e.query, r.URL.Query()) && sameJSON(e.body, raw) {
			match = e
			e.used++
			break
		}
	}
	f.mu.Unlock()
	if match == nil {
		f.t.Errorf("unexpected request %s %s?%s body %s", r.Method, r.URL.Path, r.URL.RawQuery, raw)
		w.WriteHeader(http.StatusTeapot)
		return
	}
	if match.during != nil {
		match.during()
	}
	for k, v := range match.header {
		w.Header()[k] = v
	}
	if match.resp != "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(match.status)
	_, _ = io.WriteString(w, match.resp)
}

func (f *fakeTempo) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.log)
}

func sameQuery(want, got url.Values) bool {
	if len(want) == 0 && len(got) == 0 {
		return true
	}
	return reflect.DeepEqual(want, got)
}

func sameJSON(want string, got []byte) bool {
	if want == "" || len(got) == 0 {
		return want == "" && len(got) == 0
	}
	var a, b any
	if json.Unmarshal([]byte(want), &a) != nil || json.Unmarshal(got, &b) != nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}

// fakeIssues is an IssueIDs over fixed maps that counts its calls.
type fakeIssues struct {
	mu       sync.Mutex
	ids      map[string]int64
	keys     map[int64]string
	err      error
	idCalls  [][]string
	keyCalls [][]int64
}

func (f *fakeIssues) IDs(_ context.Context, keys []string) (map[string]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.idCalls = append(f.idCalls, slices.Clone(keys))
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]int64{}
	for _, k := range keys {
		if id, ok := f.ids[k]; ok {
			out[k] = id
		}
	}
	return out, nil
}

func (f *fakeIssues) Keys(_ context.Context, ids []int64) (map[int64]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keyCalls = append(f.keyCalls, slices.Clone(ids))
	if f.err != nil {
		return nil, f.err
	}
	out := map[int64]string{}
	for _, id := range ids {
		if k, ok := f.keys[id]; ok {
			out[id] = k
		}
	}
	return out, nil
}

func (f *fakeIssues) calls() (ids [][]string, keys [][]int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.idCalls), slices.Clone(f.keyCalls)
}

func newIssues() *fakeIssues {
	return &fakeIssues{
		ids:  map[string]int64{"WEB-1": 10001, "WEB-2": 10002, "APP-7": 20007},
		keys: map[int64]string{10001: "WEB-1", 10002: "WEB-2", 20007: "APP-7"},
	}
}

// testClient is a Client on f with no pacing, so tests do not wait
// between requests.
func testClient(t *testing.T, f *fakeTempo, issues IssueIDs) *Client {
	t.Helper()
	opts, err := transportOptions(Options{
		BaseURL:     f.srv.URL,
		Version:     "test",
		Credentials: trackerapi.StaticCredentials(trackerapi.Bearer(secret.New([]byte(testToken)))),
		Issues:      issues,
	})
	if err != nil {
		t.Fatal(err)
	}
	opts.MaxPerSecond = 0
	c, err := newClient(opts, issues)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func mustDate(t *testing.T, s string) Date {
	t.Helper()
	d, err := ParseDate(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func mustClock(t *testing.T, s string) TimeOfDay {
	t.Helper()
	c, err := ParseTimeOfDay(s)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func q(pairs ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(pairs); i += 2 {
		v.Add(pairs[i], pairs[i+1])
	}
	return v
}

func ptr[T any](v T) *T { return &v }
