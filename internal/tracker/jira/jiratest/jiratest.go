// Package jiratest is a loopback Jira for tests of the jira client and of
// the packages built on it. A Server answers only the routes a test names
// and fails the test on any other request, so a caller cannot pass by
// sending something else. It holds no Jira knowledge beyond HTTP and
// imports nothing of the jira package, which tests inside that package
// can therefore use.
package jiratest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sync"
	"testing"
)

// Handler answers one request; body is the request body read whole.
type Handler func(w http.ResponseWriter, r *http.Request, body []byte)

// Server is a loopback test server that answers only the routes a test
// names ("METHOD /path") and fails the test on any other request. It is
// closed when the test ends.
type Server struct {
	t      testing.TB
	srv    *httptest.Server
	mu     sync.Mutex
	routes map[string]Handler
	hits   map[string]int
	order  []string
}

// NewServer starts a Server for t.
func NewServer(t testing.TB) *Server {
	t.Helper()
	s := &Server{t: t, routes: map[string]Handler{}, hits: map[string]int{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	key := r.Method + " " + r.URL.Path
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.t.Errorf("%s: read the body: %v", key, err)
	}
	s.mu.Lock()
	s.hits[key]++
	s.order = append(s.order, key)
	h, ok := s.routes[key]
	s.mu.Unlock()
	if !ok {
		s.t.Errorf("unexpected request %s?%s", key, r.URL.RawQuery)
		http.Error(w, "unexpected", http.StatusTeapot)
		return
	}
	h(w, r, body)
}

// URL is the server's base URL, http on a loopback address.
func (s *Server) URL() string { return s.srv.URL }

// On routes method and path to h, replacing an earlier route.
func (s *Server) On(method, path string, h Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes[method+" "+path] = h
}

// Count is how many requests method and path have had.
func (s *Server) Count(method, path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[method+" "+path]
}

// Requests lists the requests served, "METHOD /path" in order.
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.order)
}

// Reply answers with status and a JSON body.
func Reply(status int, body string) Handler {
	return func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// WantJSON fails t unless got and want are the same JSON value.
func WantJSON(t testing.TB, got []byte, want string) {
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

// WantQuery fails t unless r's query holds key=value.
func WantQuery(t testing.TB, r *http.Request, key, value string) {
	t.Helper()
	if got := r.URL.Query().Get(key); got != value {
		t.Errorf("%s %s: query %s = %q, want %q", r.Method, r.URL.Path, key, got, value)
	}
}
