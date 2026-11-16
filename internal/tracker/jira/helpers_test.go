package jira

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker/adf"
	"agent-overflow/internal/tracker/trackerapi"
)

// handler answers one request; body is the request body read whole.
type handler func(w http.ResponseWriter, r *http.Request, body []byte)

// fakeJira is a loopback test server that answers only the routes a test
// names ("METHOD /path") and fails the test on any other request.
type fakeJira struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	routes map[string]handler
	hits   map[string]int
	order  []string
}

func newFake(t *testing.T) *fakeJira {
	t.Helper()
	f := &fakeJira{t: t, routes: map[string]handler{}, hits: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("%s: read the body: %v", key, err)
		}
		f.mu.Lock()
		f.hits[key]++
		f.order = append(f.order, key)
		h, ok := f.routes[key]
		f.mu.Unlock()
		if !ok {
			t.Errorf("unexpected request %s?%s", key, r.URL.RawQuery)
			http.Error(w, "unexpected", http.StatusTeapot)
			return
		}
		h(w, r, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// on routes method and path to h, replacing an earlier route.
func (f *fakeJira) on(method, path string, h handler) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[method+" "+path] = h
}

func (f *fakeJira) count(method, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[method+" "+path]
}

// requests lists the requests served, in order.
func (f *fakeJira) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.order...)
}

// reply answers with status and a JSON body.
func reply(status int, body string) handler {
	return func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// testClock is the client's injected clock.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (k *testClock) now() time.Time {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.t
}

func (k *testClock) advance(d time.Duration) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.t = k.t.Add(d)
}

// newTestClient is a Client on f with a static credential and the test
// clock.
func newTestClient(t *testing.T, f *fakeJira) (*Client, *testClock) {
	t.Helper()
	c, err := New(Options{
		BaseURL: f.srv.URL,
		Version: "test",
		Credentials: trackerapi.StaticCredentials(
			trackerapi.Basic("user1@example.com", secret.New([]byte("fake-token")))),
	})
	if err != nil {
		t.Fatal(err)
	}
	clock := &testClock{t: time.Unix(1_800_000_000, 0)}
	c.now = clock.now
	t.Cleanup(c.Close)
	return c, clock
}

// wantJSON fails unless got and want are the same JSON value.
func wantJSON(t *testing.T, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got is not JSON: %v: %s", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want is not JSON: %v: %s", err, want)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("JSON:\n got %s\nwant %s", got, want)
	}
}

// wantQuery fails unless r's query holds key=value.
func wantQuery(t *testing.T, r *http.Request, key, value string) {
	t.Helper()
	if got := r.URL.Query().Get(key); got != value {
		t.Errorf("%s %s: query %s = %q, want %q", r.Method, r.URL.Path, key, got, value)
	}
}

// docJSON is the ADF JSON of doc.
func docJSON(t *testing.T, doc *adf.Node) string {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func para(s string) *adf.Node {
	return &adf.Node{Type: "paragraph", Content: []*adf.Node{{Type: "text", Text: s}}}
}

// layoutDoc is a document with a two-column layout, which Markdown
// writes with comment markers.
func layoutDoc() *adf.Node {
	col := func(s string) *adf.Node {
		return &adf.Node{Type: "layoutColumn", Attrs: map[string]any{"width": 50.0}, Content: []*adf.Node{para(s)}}
	}
	return adf.Doc(para("intro"), &adf.Node{Type: "layoutSection", Content: []*adf.Node{col("left"), col("right")}})
}

const (
	userOneJSON = `{"accountId":"acc-user1","displayName":"User 1","emailAddress":"user1@example.com","active":true,"accountType":"atlassian"}`
	userTwoJSON = `{"accountId":"acc-user2","displayName":"User 2","emailAddress":"user2@example.com","active":true,"accountType":"atlassian"}`
)
