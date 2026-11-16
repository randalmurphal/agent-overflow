package trackerapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// testDialect reads a Jira-like shape: a login marker header, a message
// with per-field errors, and a 403 whose body names a rate limit.
type testDialect struct{}

func (testDialect) RejectedLogin(h http.Header) bool {
	reason := h.Get("X-Login-Reason")
	return strings.Contains(reason, "FAILED") || strings.Contains(reason, "DENIED")
}

func (testDialect) Problem(_ int, body []byte) Problem {
	var v struct {
		Message string            `json:"message"`
		Errors  map[string]string `json:"errors"`
	}
	if json.Unmarshal(body, &v) != nil {
		return Problem{}
	}
	return Problem{Message: v.Message, Fields: v.Errors}
}

func (testDialect) RateLimited(status int, h http.Header, body []byte) (time.Time, bool) {
	if at := h.Get("X-Limit-Until"); at != "" {
		unix, err := strconv.ParseInt(at, 10, 64)
		return time.Unix(unix, 0), err == nil
	}
	return time.Time{}, status == http.StatusForbidden && bytes.Contains(body, []byte("rate limit"))
}

func TestStatusErrors(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", 10<<10)
	tr := newTracker(t, route{
		"POST /issue": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			writeString(w, `{"message":"Fix the fields","errors":{"summary":"required","customfield_1":"not a number"}}`)
		},
		"GET /issue/WEB-404": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			writeString(w, `{"message":"Issue does not exist"}`)
		},
		"GET /big": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusConflict)
			writeString(w, big)
		},
		"GET /plain": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			writeString(w, "upstream broke")
		},
	})
	opts := testOptions(tr.url())
	opts.Dialect = testDialect{}
	c, _ := loopbackClient(t, opts)

	_, err := c.Do(t.Context(), Request{Method: http.MethodPost, Path: "issue", Query: map[string][]string{"token": {"secret-q"}}, Body: map[string]string{}})
	status, ok := errors.AsType[*StatusError](err)
	if !ok || status.Status != http.StatusBadRequest || status.Message != "Fix the fields" ||
		status.Fields["summary"] != "required" || status.Method != http.MethodPost || status.Tracker != "jira" {
		t.Fatalf("400: %#v", err)
	}
	if status.URL != tr.srv.URL+"/issue" || strings.Contains(err.Error(), "secret-q") {
		t.Fatalf("400: URL %q and %q must be redacted", status.URL, err)
	}
	if !strings.Contains(err.Error(), "customfield_1: not a number; summary: required") || errors.Is(err, ErrNotFound) {
		t.Fatalf("400: %v", err)
	}

	_, err = c.Do(t.Context(), Request{Path: "issue/WEB-404"})
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "Issue does not exist") {
		t.Fatalf("404: %v", err)
	}

	_, err = c.Do(t.Context(), Request{Path: "big"})
	if status, ok := errors.AsType[*StatusError](err); !ok || len(status.Body) != maxErrorBody {
		t.Fatalf("409: body of %d bytes, want %d", len(status.Body), maxErrorBody)
	}

	_, err = c.Do(t.Context(), Request{Path: "plain"})
	if !strings.Contains(err.Error(), "HTTP 500 Internal Server Error: upstream broke") {
		t.Fatalf("500 without a dialect message: %v", err)
	}
}

func TestNotSentErrors(t *testing.T) {
	t.Parallel()
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	c, _ := loopbackClient(t, testOptions(closed.URL))
	_, err := c.Do(t.Context(), Request{Method: http.MethodPost, Path: "issue", Body: map[string]string{}})
	if _, ok := errors.AsType[*NotSentError](err); !ok || !WriteNotApplied(err) {
		t.Fatalf("refused dial: %v, want a *NotSentError", err)
	}
	if _, ok := errors.AsType[*TransientError](err); !ok {
		t.Fatalf("refused dial: %v, want it also a *TransientError", err)
	}

	c, _ = loopbackClient(t, testOptions("https://tracker.invalid/"))
	_, err = c.Do(t.Context(), Request{Method: http.MethodPost, Path: "issue", Body: map[string]string{}})
	if _, ok := errors.AsType[*NotSentError](err); !ok || !WriteNotApplied(err) {
		t.Fatalf("name lookup failure: %v, want a *NotSentError", err)
	}
}

func TestTransientErrors(t *testing.T) {
	t.Parallel()
	tr := newTracker(t, route{
		"POST /reset": func(w http.ResponseWriter, r *http.Request) {
			hijackClose(t, w, "")
		},
		"POST /cut": func(w http.ResponseWriter, r *http.Request) {
			hijackClose(t, w, "HTTP/1.1 201 Created\r\nContent-Type: application/json\r\nContent-Length: 64\r\n\r\n{\"id\":")
		},
	})
	c, _ := loopbackClient(t, testOptions(tr.url()))
	for _, path := range []string{"reset", "cut"} {
		_, err := c.Do(t.Context(), Request{Method: http.MethodPost, Path: path, Body: map[string]string{}})
		if _, ok := errors.AsType[*TransientError](err); !ok || WriteNotApplied(err) {
			t.Fatalf("%s: %v, want an unconfirmed *TransientError", path, err)
		}
		if _, ok := errors.AsType[*NotSentError](err); ok {
			t.Fatalf("%s: %v is not a request that was never sent", path, err)
		}
	}

	untrusted := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a request reached a server whose certificate is not trusted")
	}))
	t.Cleanup(untrusted.Close)
	c, _ = loopbackClient(t, testOptions(untrusted.URL))
	_, err := c.Do(t.Context(), Request{Path: "me"})
	if _, ok := errors.AsType[*TransientError](err); !ok || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("TLS failure: %v, want a *TransientError", err)
	}
}

// hijackClose answers with raw, possibly nothing, and closes the
// connection.
func hijackClose(t *testing.T, w http.ResponseWriter, raw string) {
	conn, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		t.Error(err)
		return
	}
	defer func() { _ = conn.Close() }()
	if raw != "" {
		_, _ = buf.WriteString(raw)
		_ = buf.Flush()
	}
}

// A call out of time is transient, and a write that timed out is
// unconfirmed.
func TestTimeoutIsTransient(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		c := memoryClient(t, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			hang(r)
		}, nil)
		_, err := c.Do(t.Context(), Request{Method: http.MethodPut, Path: "issue/WEB-1", Body: map[string]string{}, Timeout: 5 * time.Second})
		if _, ok := errors.AsType[*TransientError](err); !ok || !errors.Is(err, context.DeadlineExceeded) || WriteNotApplied(err) {
			t.Fatalf("Do = %v, want an unconfirmed *TransientError", err)
		}
		if calls.Load() != 1 {
			t.Fatalf("the write was sent %d times", calls.Load())
		}
	})
}

func TestBodyBound(t *testing.T) {
	t.Parallel()
	payload := strings.Repeat("a", 100)
	tr := newTracker(t, route{
		"GET /sized": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "100")
			writeString(w, payload)
		},
		"GET /chunked": func(w http.ResponseWriter, r *http.Request) {
			writeString(w, payload[:50])
			_ = http.NewResponseController(w).Flush()
			writeString(w, payload[50:])
		},
	})
	opts := testOptions(tr.url())
	opts.MaxBodyBytes = 99
	c, _ := loopbackClient(t, opts)
	for _, path := range []string{"sized", "chunked"} {
		_, err := c.Do(t.Context(), Request{Path: path})
		if tooLarge, ok := errors.AsType[*BodyTooLargeError](err); !ok || tooLarge.Limit != 99 {
			t.Fatalf("%s over the client bound: %v", path, err)
		}
		var out map[string]any
		if _, err := c.JSON(t.Context(), Request{Path: path, MaxBodyBytes: 50}, &out); err == nil {
			t.Fatalf("%s over the request bound: no error", path)
		}
		resp, err := c.Do(t.Context(), Request{Path: path, MaxBodyBytes: 100})
		if err != nil || string(resp.Body) != payload {
			t.Fatalf("%s at the bound: %v", path, err)
		}
	}
}

// An answer whose length is over the bound is refused at its headers,
// without waiting for a body that may be slow to come.
func TestBodyOverTheBoundIsNotRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := memoryClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "1000")
			writeString(w, "start")
			_ = http.NewResponseController(w).Flush()
			hang(r)
		}, nil)
		start := time.Now()
		_, err := c.Do(t.Context(), Request{Path: "a", MaxBodyBytes: 100})
		if _, ok := errors.AsType[*BodyTooLargeError](err); !ok || time.Since(start) != 0 {
			t.Fatalf("Do = %v after %s, want a refusal at once", err, time.Since(start))
		}
	})
}

func TestJSONDecodes(t *testing.T) {
	t.Parallel()
	tr := newTracker(t, route{
		"GET /ok":        func(w http.ResponseWriter, r *http.Request) { writeString(w, `{"key":"WEB-1"}`) },
		"PUT /none":      func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) },
		"POST /html":     func(w http.ResponseWriter, r *http.Request) { writeString(w, "<html>maintenance</html>") },
		"POST /empty200": func(w http.ResponseWriter, r *http.Request) {},
	})
	c, _ := loopbackClient(t, testOptions(tr.url()))
	var issue struct{ Key string }
	if _, err := c.JSON(t.Context(), Request{Path: "ok"}, &issue); err != nil || issue.Key != "WEB-1" {
		t.Fatalf("JSON = %v, %+v", err, issue)
	}
	if resp, err := c.JSON(t.Context(), Request{Method: http.MethodPut, Path: "none", Body: map[string]string{}}, &issue); err != nil || resp.Status != http.StatusNoContent {
		t.Fatalf("204: %v", err)
	}
	for _, path := range []string{"html", "empty200"} {
		resp, err := c.JSON(t.Context(), Request{Method: http.MethodPost, Path: path, Body: map[string]string{}}, &issue)
		if err == nil || resp == nil || WriteNotApplied(err) {
			t.Fatalf("%s: %v, want an unconfirmed decode failure with its Response", path, err)
		}
	}
}

func TestWriteNotAppliedTruthTable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"no error", nil, false},
		{"never sent", &NotSentError{Err: errors.New("dial refused")}, true},
		{"never sent, wrapped", fmt.Errorf("create: %w", &NotSentError{Err: errors.New("dial refused")}), true},
		{"gate closed", &NotSentError{Err: &RateLimitedError{Tracker: "jira"}}, true},
		{"400", &StatusError{Status: http.StatusBadRequest}, true},
		{"404", &StatusError{Status: http.StatusNotFound}, true},
		{"409 wrapped", fmt.Errorf("edit: %w", &StatusError{Status: http.StatusConflict}), true},
		{"429", &RateLimitedError{Status: http.StatusTooManyRequests}, true},
		{"dialect limit on a 403", &RateLimitedError{Status: http.StatusForbidden}, true},
		{"dialect limit on a 200", &RateLimitedError{Status: http.StatusOK}, false},
		{"dialect limit on a 503", &RateLimitedError{Status: http.StatusServiceUnavailable}, false},
		{"401", &AuthError{Status: http.StatusUnauthorized}, true},
		{"401 with a failed re-read", &AuthError{Status: http.StatusUnauthorized, Err: errors.New("unreadable")}, true},
		{"refused login on a 200", &AuthError{Status: http.StatusOK}, false},
		{"refused login on a 200 holding a not-sent error", &AuthError{Status: http.StatusOK, Err: &NotSentError{Err: errors.New("x")}}, false},
		{"refused login on a 200 holding a not-sent error, wrapped", fmt.Errorf("edit: %w", &AuthError{Status: http.StatusOK, Err: &NotSentError{Err: errors.New("x")}}), false},
		{"not sent after a check read's 500", &NotSentError{Err: fmt.Errorf("check: %w", &StatusError{Status: http.StatusInternalServerError})}, true},
		{"not sent after a check read's 500, wrapped", fmt.Errorf("log work: %w", &NotSentError{Err: &StatusError{Status: http.StatusBadGateway}}), true},
		{"not sent after a check read's refused login on a 200", &NotSentError{Err: &AuthError{Status: http.StatusOK}}, true},
		{"not sent after a check read's limit on a 503", &NotSentError{Err: &RateLimitedError{Status: http.StatusServiceUnavailable}}, true},
		{"gate refusal on a redirect hop", &RateLimitedError{Tracker: "jira"}, false},
		{"500", &StatusError{Status: http.StatusInternalServerError}, false},
		{"502", &StatusError{Status: http.StatusBadGateway}, false},
		{"reset", &TransientError{Err: errors.New("connection reset")}, false},
		{"timeout", &TransientError{Err: context.DeadlineExceeded}, false},
		{"over the bound", &BodyTooLargeError{Limit: 1}, false},
		{"decode", errors.New("decode the answer"), false},
		{"cancelled", context.Canceled, false},
	} {
		if got := WriteNotApplied(tc.err); got != tc.want {
			t.Errorf("%s: WriteNotApplied = %v, want %v", tc.name, got, tc.want)
		}
	}
}
