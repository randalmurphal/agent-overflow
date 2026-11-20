// Package lineartest is the test support shared by the linear client's
// tests and the tests of packages built on it: a loopback Linear GraphQL
// server that checks every document and its variables against Linear's
// public schema (package linearschema) and answers only the exchanges a
// test scripts, JSON builders for invented answers, and a polled
// workspace.
//
// It imports nothing from package linear, so the client's in-package
// tests can use it; a test builds its own client on Server.URL with Key.
// Only tests import it.
package lineartest

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

	"agent-overflow/internal/tracker/linear/linearschema"
)

// Key is the invented personal key every test client sends; the server
// refuses a request that does not carry it bare.
const Key = "lin_api_fake0000000000"

// Exchange is one request the server expects and its answer.
type Exchange struct {
	// Op is the operation name the request must carry.
	Op string
	// Vars is the exact variables object expected, as JSON; empty means
	// the request carries none.
	Vars string
	// VarsFn, when set, gives the exact expected variables at the time
	// the request arrives, for a script whose cursors depend on earlier
	// answers.
	VarsFn func() string
	// Key is the key the request must carry, bare; empty means Key.
	Key    string
	Status int // zero is 200
	Header map[string]string
	Body   string
	// Respond, when set, answers instead of Body; it gets the request's
	// variables.
	Respond func(vars json.RawMessage) string
	// Wait, when set, holds the answer until the request ends, after
	// telling Arrived that the request is in.
	Wait    bool
	Arrived chan<- struct{}
	// Reset, when set, closes the connection without an answer.
	Reset bool
	// BeforeBody, when set, runs after the status and headers are sent
	// and before the body.
	BeforeBody func()
}

// exercised records the operation names a server answered in this test
// binary.
var exercised sync.Map

// Exercised is every operation name a server of this test binary
// answered, sorted.
func Exercised() []string {
	var out []string
	exercised.Range(func(name, _ any) bool {
		out = append(out, name.(string))
		return true
	})
	slices.Sort(out)
	return out
}

// Server is a loopback Linear. Each request must match an expected
// exchange by operation name and exact variables, and each exchange
// answers once; any other request fails the test, and so does an
// exchange never requested by the end of the test. Every document it
// receives is checked against the schema, and its variables against the
// document's declarations, before it is matched.
type Server struct {
	srv     *httptest.Server
	mu      sync.Mutex
	report  func(format string, args ...any)
	pending []*Exchange
}

// NewServer starts a server expecting script, in any order, and closes
// it when the test ends.
func NewServer(t testing.TB, script ...Exchange) *Server {
	t.Helper()
	s := &Server{report: t.Errorf}
	s.Expect(script...)
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(func() {
		s.srv.Close()
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, e := range s.pending {
			t.Errorf("expected %s %s was never requested", e.Op, e.Vars)
		}
	})
	return s
}

// URL is the server's origin, the client's BaseURL.
func (s *Server) URL() string { return s.srv.URL }

// Expect adds exchanges to the script.
func (s *Server) Expect(script ...Exchange) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range script {
		s.pending = append(s.pending, &script[i])
	}
}

// SetReport replaces what a refused request is reported to, t.Errorf
// by default, for a test of the server itself.
func (s *Server) SetReport(report func(format string, args ...any)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.report = report
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	fail := func(format string, args ...any) {
		s.mu.Lock()
		report := s.report
		s.mu.Unlock()
		report("fake Linear: "+format, args...)
		http.Error(w, "unexpected request", http.StatusTeapot)
	}
	if r.Method != http.MethodPost || r.URL.Path != "/graphql" || r.URL.RawQuery != "" {
		fail("unexpected %s %s", r.Method, r.URL)
		return
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "lin_api_") {
		fail("Authorization %q, want a bare key", auth)
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
	if problems := linearschema.DocumentProblems(req.Query, req.OperationName); len(problems) > 0 {
		fail("document %s: %s", req.OperationName, strings.Join(problems, "; "))
		return
	}
	if problems := linearschema.VariableProblems(req.Query, req.OperationName, req.Variables); len(problems) > 0 {
		fail("variables of %s: %s", req.OperationName, strings.Join(problems, "; "))
		return
	}
	exercised.Store(req.OperationName, true)
	e := s.take(req.OperationName, req.Variables, auth)
	if e == nil {
		fail("unexpected %s with variables %s and key %s", req.OperationName, req.Variables, auth)
		return
	}
	s.answer(w, r, e, req.Variables, fail)
}

func (s *Server) answer(w http.ResponseWriter, r *http.Request, e *Exchange, vars json.RawMessage, fail func(string, ...any)) {
	if e.Reset {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			fail("hijack: %v", err)
			return
		}
		_ = conn.Close()
		return
	}
	if e.Wait {
		if e.Arrived != nil {
			e.Arrived <- struct{}{}
		}
		<-r.Context().Done()
		return
	}
	for k, v := range e.Header {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "application/json")
	status := e.Status
	if status == 0 {
		status = http.StatusOK
	}
	answer := e.Body
	if e.Respond != nil {
		answer = e.Respond(vars)
	}
	w.WriteHeader(status)
	if e.BeforeBody != nil {
		if err := http.NewResponseController(w).Flush(); err != nil {
			fail("flush: %v", err)
			return
		}
		e.BeforeBody()
	}
	_, _ = io.WriteString(w, answer)
}

// take removes and returns the first pending exchange for op whose
// variables equal vars and whose key is key.
func (s *Server) take(op string, vars json.RawMessage, key string) *Exchange {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, e := range s.pending {
		want := e.Vars
		if e.VarsFn != nil {
			want = e.VarsFn()
		}
		wantKey := e.Key
		if wantKey == "" {
			wantKey = Key
		}
		if e.Op == op && key == wantKey && SameJSON(want, vars) {
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
			return e
		}
	}
	return nil
}

// SameJSON reports whether got is the JSON value want spells; empty want
// matches no value or null.
func SameJSON(want string, got json.RawMessage) bool {
	if want == "" {
		return len(got) == 0 || string(got) == "null"
	}
	var a, b any
	if json.Unmarshal([]byte(want), &a) != nil || json.Unmarshal(got, &b) != nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}
