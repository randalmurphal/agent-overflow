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
	"strings"
	"sync"
	"testing"
)

// Handler answers one request; body is the request body read whole.
type Handler func(w http.ResponseWriter, r *http.Request, body []byte)

// Server is a loopback test server that answers only the routes a test
// names ("METHOD /path") and fails the test on any other request, unless
// the test names a handler for those (Otherwise). A GET whose expand
// parameter names a routed sub-resource of its path embeds that route's
// answer (see embed); the sub-resource is not counted as a request. As
// Jira Cloud does for a valid token, every answer to a request with a
// credential names the account it was made for in X-AAccountId
// (DefaultAccount unless SetAccount says otherwise). It is closed when
// the test ends.
type Server struct {
	t       testing.TB
	srv     *httptest.Server
	mu      sync.Mutex
	routes  map[string]Handler
	other   Handler
	hits    map[string]int
	order   []string
	account string
	before  func(*http.Request)
}

// DefaultAccount is the account a Server's answers name: User 1's.
const DefaultAccount = "acc-user1"

// NewServer starts a Server for t.
func NewServer(t testing.TB) *Server {
	t.Helper()
	s := &Server{t: t, routes: map[string]Handler{}, hits: map[string]int{}, account: DefaultAccount}
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
	if !ok && s.other != nil {
		h, ok = s.other, true
	}
	account, before := s.account, s.before
	s.mu.Unlock()
	if before != nil {
		before(r)
	}
	if account != "" && r.Header.Get("Authorization") != "" {
		w.Header().Set("X-AAccountId", account)
	}
	if !ok {
		s.t.Errorf("unexpected request %s?%s", key, r.URL.RawQuery)
		http.Error(w, "unexpected", http.StatusTeapot)
		return
	}
	if subs := s.expansions(r); len(subs) > 0 {
		s.embed(w, r, body, h, subs)
		return
	}
	h(w, r, body)
}

// expansions are the routes a GET's expand parameter names as its own
// sub-resources: for each listed name, the route GET <path>/<name>.
func (s *Server) expansions(r *http.Request) map[string]Handler {
	if r.Method != http.MethodGet {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]Handler{}
	for _, v := range r.URL.Query()["expand"] {
		for name := range strings.SplitSeq(v, ",") {
			if h, ok := s.routes["GET "+r.URL.Path+"/"+name]; ok {
				out[name] = h
			}
		}
	}
	return out
}

// embed answers with h's answer, and when that is a JSON object with a
// 200, each sub-resource's 200 answer in it as the member of its name
// (Jira's editmeta), or the sub-resource's own member of that name when
// its answer has one (Jira's transitions). A member h's answer already
// holds is kept, so a test writes an expansion whose shape differs from
// its sub-resource's (Jira's changelog) itself. A sub-resource answering
// another status fails the expansion with that answer.
func (s *Server) embed(w http.ResponseWriter, r *http.Request, body []byte, h Handler, subs map[string]Handler) {
	main := httptest.NewRecorder()
	h(main, r, body)
	var obj map[string]json.RawMessage
	if main.Code != http.StatusOK || json.Unmarshal(main.Body.Bytes(), &obj) != nil {
		copyAnswer(w, main)
		return
	}
	for name, sub := range subs {
		if _, held := obj[name]; held {
			continue
		}
		rec := httptest.NewRecorder()
		sub(rec, r, nil)
		if rec.Code != http.StatusOK {
			copyAnswer(w, rec)
			return
		}
		val := json.RawMessage(rec.Body.Bytes())
		var inner map[string]json.RawMessage
		if json.Unmarshal(val, &inner) == nil && inner[name] != nil {
			val = inner[name]
		}
		obj[name] = val
	}
	out, err := json.Marshal(obj)
	if err != nil {
		s.t.Errorf("%s %s: embed the expansions: %v", r.Method, r.URL.Path, err)
	}
	for k, vs := range main.Header() {
		w.Header()[k] = vs
	}
	w.Header().Del("Content-Length")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

func copyAnswer(w http.ResponseWriter, rec *httptest.ResponseRecorder) {
	for k, vs := range rec.Header() {
		w.Header()[k] = vs
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(rec.Body.Bytes())
}

// SetAccount sets the account later answers name; empty names none, as
// an answer Jira made for no account.
func (s *Server) SetAccount(id string) {
	s.mu.Lock()
	s.account = id
	s.mu.Unlock()
}

// URL is the server's base URL, http on a loopback address.
func (s *Server) URL() string { return s.srv.URL }

// On routes method and path to h, replacing an earlier route.
func (s *Server) On(method, path string, h Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes[method+" "+path] = h
}

// Before calls f with every request the server receives, before it is
// answered.
func (s *Server) Before(f func(*http.Request)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.before = f
}

// Otherwise answers every request no route names with h, instead of
// failing the test; h decides what such a request means.
func (s *Server) Otherwise(h Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.other = h
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
