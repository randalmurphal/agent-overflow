package jira

import (
	"errors"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker/trackerapi"
)

func TestDialectProblemShapes(t *testing.T) {
	for name, tc := range map[string]struct {
		body   string
		msg    string
		fields map[string]string
	}{
		"messages and field errors": {
			body:   `{"errorMessages":["Issue does not exist"],"errors":{"summary":"Summary is required","customfield_10001":"Bad number"}}`,
			msg:    "Issue does not exist",
			fields: map[string]string{"summary": "Summary is required", "customfield_10001": "Bad number"},
		},
		"field errors only":       {body: `{"errorMessages":[],"errors":{"priority":"Priority is invalid"}}`, fields: map[string]string{"priority": "Priority is invalid"}},
		"several messages":        {body: `{"errorMessages":["one","two"],"errors":{}}`, msg: "one; two"},
		"errors as a list":        {body: `{"errors":[{"message":"first"},{"title":"second"},"third"]}`, msg: "first; second; third"},
		"message key":             {body: `{"message":"Rate limited"}`, msg: "Rate limited"},
		"errorMessage key":        {body: `{"errorMessage":"Something broke"}`, msg: "Something broke"},
		"a non-string field note": {body: `{"errors":{"labels":{"code":7}}}`, fields: map[string]string{"labels": `{"code":7}`}},
		"not JSON":                {body: `<html>oops</html>`},
		"empty":                   {body: ``},
	} {
		t.Run(name, func(t *testing.T) {
			p := dialect{}.Problem(400, []byte(tc.body))
			if p.Message != tc.msg || !maps.Equal(p.Fields, tc.fields) {
				t.Errorf("got %q %v, want %q %v", p.Message, p.Fields, tc.msg, tc.fields)
			}
		})
	}
}

func TestDialectRejectedLogin(t *testing.T) {
	for value, want := range map[string]bool{
		"AUTHENTICATED_FAILED":      true,
		"AUTHENTICATION_DENIED":     true,
		"OK, AUTHENTICATION_DENIED": true,
		"OK":                        false,
		"AUTHORISATION_FAILED":      false,
		"":                          false,
	} {
		h := http.Header{}
		if value != "" {
			h.Set("X-Seraph-LoginReason", value)
		}
		if got := (dialect{}).RejectedLogin(h); got != want {
			t.Errorf("%q: got %v, want %v", value, got, want)
		}
	}
}

func TestDialectRateLimited(t *testing.T) {
	reset := "2026-11-16T12:00:05Z"
	h := http.Header{"X-Ratelimit-Reset": {reset}}
	until, limited := dialect{}.RateLimited(429, h, nil)
	if want, _ := time.Parse(time.RFC3339, reset); !limited || !until.Equal(want) {
		t.Errorf("reset header: %v %v", until, limited)
	}
	h.Set("Retry-After", "3")
	if until, limited := (dialect{}).RateLimited(429, h, nil); !limited || !until.IsZero() {
		t.Errorf("Retry-After must win, through the transport: %v %v", until, limited)
	}
	if _, limited := (dialect{}).RateLimited(503, h, nil); limited {
		t.Error("a 503 is not a rate limit")
	}
}

// TestDialectThroughTransport proves the dialect reaches callers: field
// errors on a StatusError, and a refused login marked on a 200.
func TestDialectThroughTransport(t *testing.T) {
	f := newFake(t)
	c := newTestClient(t, f)
	f.on("PUT", "/rest/api/3/issue/WEB-1/assignee", reply(400, `{"errorMessages":[],"errors":{"assignee":"User cannot be assigned"}}`))
	err := c.Assign(t.Context(), "WEB-1", "acc-user1")
	se, ok := errors.AsType[*trackerapi.StatusError](err)
	if !ok || se.Fields["assignee"] != "User cannot be assigned" {
		t.Fatalf("got %v", err)
	}
	if !trackerapi.WriteNotApplied(err) {
		t.Error("a 400 is a refusal")
	}
	f.on("GET", "/rest/api/3/myself", func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("X-Seraph-LoginReason", "AUTHENTICATED_FAILED")
		_, _ = w.Write([]byte(`{}`))
	})
	if _, err := c.Myself(t.Context()); !isAuth(err) {
		t.Errorf("a refused login on a 200: %v", err)
	}
}

func isAuth(err error) bool {
	_, ok := errors.AsType[*trackerapi.AuthError](err)
	return ok
}

// TestOnAnswerGetsTheRateLimitHeaders proves the answer hook receives
// exactly the rate limit headers Jira documents, scrubbed of the
// credential, and no other header.
func TestOnAnswerGetsTheRateLimitHeaders(t *testing.T) {
	f := newFake(t)
	var mu sync.Mutex
	var got []http.Header
	c, err := New(Options{
		BaseURL: f.srv.URL, Version: "test",
		Credentials: trackerapi.StaticCredentials(trackerapi.Basic("user1@example.com", secret.New([]byte(testToken)))),
		OnAnswer: func(status int, h http.Header) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, h)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	documented := []string{
		"X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset", "X-RateLimit-NearLimit",
		"RateLimit-Reason", "Retry-After", "RateLimit", "RateLimit-Policy",
		"Beta-RateLimit", "Beta-RateLimit-Policy", "Beta-Retry-After",
		"X-Beta-RateLimit-Limit", "X-Beta-RateLimit-Remaining", "X-Beta-RateLimit-Reset",
		"X-Beta-RateLimit-NearLimit", "X-Beta-RateLimit-Reason",
	}
	f.on("GET", myselfURL, func(w http.ResponseWriter, r *http.Request, b []byte) {
		for i, name := range documented {
			w.Header().Set(name, "v"+strconv.Itoa(i))
		}
		w.Header().Set("RateLimit-Reason", "jira-burst-based "+testToken)
		w.Header().Set("Location", "https://example.com/"+testToken)
		w.Header().Set("Set-Cookie", "session=1")
		w.Header().Set("X-Request-Id", "abc")
		reply(200, userOneJSON)(w, r, b)
	})
	if _, err := c.Myself(t.Context()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("%d reports", len(got))
	}
	h := got[0]
	var keys []string
	for k := range h {
		keys = append(keys, k)
	}
	want := make([]string, len(documented))
	for i, name := range documented {
		want[i] = http.CanonicalHeaderKey(name)
	}
	slices.Sort(keys)
	slices.Sort(want)
	if !slices.Equal(keys, want) {
		t.Errorf("headers %v\nwant %v", keys, want)
	}
	for i, name := range documented {
		if name != "RateLimit-Reason" && h.Get(name) != "v"+strconv.Itoa(i) {
			t.Errorf("%s = %q", name, h.Get(name))
		}
	}
	if reason := h.Get("RateLimit-Reason"); strings.Contains(reason, testToken) || !strings.HasPrefix(reason, "jira-burst-based ") {
		t.Errorf("RateLimit-Reason %q", reason)
	}
}
