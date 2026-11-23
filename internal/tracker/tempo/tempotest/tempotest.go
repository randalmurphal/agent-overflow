// Package tempotest is a loopback Tempo for tests of the tempo client and
// of the packages built on it. A Server answers only the exchanges a test
// names, matched by method, path, query and JSON body, and fails the test
// on any other request and on an exchange never requested. It holds no
// Tempo knowledge beyond HTTP and imports nothing of the tempo package,
// which tests inside that package can therefore use.
package tempotest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"sync"
	"testing"
)

// Exchange is one request the Server expects and its answer. Set its
// options before the request it answers is sent.
type Exchange struct {
	method string
	path   string
	query  url.Values // exact; nil expects none
	body   string     // JSON compared by value; "" expects no body
	status int
	resp   string

	// Header is sent with the answer.
	Header http.Header
	// Repeat answers every matching request instead of only the first.
	Repeat bool
	// During runs while the request is being answered, before the answer
	// is written.
	During func()

	used int
}

// Server is a loopback Tempo that answers only the exchanges a test
// names. It is closed when the test ends.
type Server struct {
	t     testing.TB
	token string
	srv   *httptest.Server

	mu        sync.Mutex
	exchanges []*Exchange
	log       []string
}

// NewServer starts a Server for t that expects every request to carry
// token as its bearer credential.
func NewServer(t testing.TB, token string) *Server {
	t.Helper()
	s := &Server{t: t, token: token}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(func() {
		s.srv.Close()
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, e := range s.exchanges {
			if e.used == 0 {
				t.Errorf("expected %s %s?%s was never requested", e.method, e.path, e.query.Encode())
			}
		}
	})
	return s
}

// URL is the server's base URL, http on a loopback address.
func (s *Server) URL() string { return s.srv.URL }

// On adds an exchange: a request with method, path (escaped), exactly
// query (nil for none) and the JSON body ("" for none), answered with
// status and resp.
func (s *Server) On(method, path string, query url.Values, body string, status int, resp string) *Exchange {
	e := &Exchange{method: method, path: path, query: query, body: body, status: status, resp: resp}
	s.mu.Lock()
	s.exchanges = append(s.exchanges, e)
	s.mu.Unlock()
	return e
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	if got := r.Header.Get("Authorization"); got != "Bearer "+s.token {
		s.t.Errorf("%s %s: Authorization %q", r.Method, r.URL.Path, got)
	}
	s.mu.Lock()
	s.log = append(s.log, r.Method+" "+r.URL.EscapedPath())
	var match *Exchange
	for _, e := range s.exchanges {
		if (e.used == 0 || e.Repeat) && e.method == r.Method && e.path == r.URL.EscapedPath() && sameQuery(e.query, r.URL.Query()) && sameJSON(e.body, raw) {
			match = e
			e.used++
			break
		}
	}
	s.mu.Unlock()
	if match == nil {
		s.t.Errorf("unexpected request %s %s?%s body %s", r.Method, r.URL.Path, r.URL.RawQuery, raw)
		w.WriteHeader(http.StatusTeapot)
		return
	}
	if match.During != nil {
		match.During()
	}
	for k, v := range match.Header {
		w.Header()[k] = v
	}
	if match.resp != "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(match.status)
	_, _ = io.WriteString(w, match.resp)
}

// Requests lists the requests served, "METHOD /escaped/path" in order.
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.log)
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
