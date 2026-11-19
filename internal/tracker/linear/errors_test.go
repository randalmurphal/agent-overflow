package linear

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/tracker/trackerapi"

	"agent-overflow/internal/tracker/linear/lineartest"
)

const (
	viewerData = `{"data":{"viewer":{"id":"user-1","name":"User 1","displayName":"user1","email":"user1@example.com","avatarUrl":null,"active":true,"admin":false,"guest":false,"app":false,"isMe":true},"organization":{"id":"org-1","name":"Example","urlKey":"example"}}}`
	authBody   = `{"errors":[{"message":"Authentication required, not authenticated","extensions":{"type":"authentication error","code":"AUTHENTICATION_ERROR","userError":true,"userPresentableMessage":"You need to authenticate to access this operation."}}]}`
	inputBody  = `{"errors":[{"message":"Argument Validation Error","extensions":{"type":"invalid input","code":"INPUT_ERROR","userError":true}}]}`
	limitBody  = `{"errors":[{"message":"Rate limit exceeded","extensions":{"type":"ratelimited","code":"RATELIMITED","userError":true}}]}`
)

func TestViewer(t *testing.T) {
	t.Parallel()
	f := newFake(t, lineartest.Exchange{Op: "Viewer", Body: viewerData, Header: map[string]string{
		"X-Complexity": "3", "X-Ratelimit-Requests-Limit": "2500", "X-Ratelimit-Requests-Remaining": "2499",
		"X-Ratelimit-Requests-Reset": "1800000000000", "X-Ratelimit-Complexity-Limit": "3000000",
		"X-Ratelimit-Complexity-Remaining": "2999997", "X-Ratelimit-Complexity-Reset": "1800000000500",
	}})
	c := f.client(t)
	v, err := c.Viewer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v.User.ID != "user-1" || v.User.Email != "user1@example.com" || !v.User.IsMe || v.Organization.URLKey != "example" {
		t.Errorf("viewer %+v", v)
	}
	want := Usage{
		Complexity: 3,
		Requests:   Budget{Known: true, Limit: 2500, Remaining: 2499, Reset: time.UnixMilli(1800000000000)},
		Points:     Budget{Known: true, Limit: 3000000, Remaining: 2999997, Reset: time.UnixMilli(1800000000500)},
	}
	if got := c.Usage(); got != want {
		t.Errorf("usage %+v, want %+v", got, want)
	}
	if !want.Points.Allows(2999997) || want.Points.Allows(2999998) || !(Budget{}).Allows(1<<30) {
		t.Error("Budget.Allows misjudges the remaining points")
	}
}

// A refused key is a *trackerapi.AuthError carrying Linear's message,
// under the 401 the transport reads and under any other status.
func TestAuthenticationError(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusUnauthorized, http.StatusBadRequest, http.StatusOK} {
		f := newFake(t, lineartest.Exchange{Op: "Viewer", Status: status, Body: authBody})
		_, err := f.client(t).Viewer(context.Background())
		auth, ok := errors.AsType[*trackerapi.AuthError](err)
		if !ok {
			t.Errorf("HTTP %d: error %v is not an AuthError", status, err)
			continue
		}
		if !strings.Contains(auth.Message, "You need to authenticate") || auth.Status != status {
			t.Errorf("HTTP %d: auth error %+v", status, auth)
		}
	}
}

// A key sent with a Bearer prefix is answered 400 INPUT_ERROR: an
// *Error over the transport's StatusError, not a refused login.
func TestBearerPrefixInputError(t *testing.T) {
	t.Parallel()
	f := newFake(t, lineartest.Exchange{Op: "Viewer", Status: http.StatusBadRequest, Body: inputBody})
	_, err := f.client(t).Viewer(context.Background())
	e, ok := errors.AsType[*Error](err)
	if !ok || e.Status != 400 || len(e.Errors) != 1 || e.Errors[0].Code != CodeInput || !e.Errors[0].UserError {
		t.Fatalf("error %#v", err)
	}
	if _, ok := errors.AsType[*trackerapi.StatusError](err); !ok {
		t.Error("the transport's StatusError is not reachable")
	}
	if _, ok := errors.AsType[*trackerapi.AuthError](err); ok {
		t.Error("an input error became an auth error")
	}
	if !strings.Contains(err.Error(), "Argument Validation Error (INPUT_ERROR)") {
		t.Errorf("message %q", err)
	}
}

// Linear's 429 with Retry-After closes the client's gate for that long;
// a background call made while it is closed fails without a request.
func TestRateLimitedRetryAfter(t *testing.T) {
	t.Parallel()
	f := newFake(t, lineartest.Exchange{Op: "Viewer", Status: http.StatusTooManyRequests, Body: limitBody, Header: map[string]string{
		"Retry-After": "30", "X-Ratelimit-Requests-Remaining": "0", "X-Ratelimit-Requests-Reset": "4000000000000",
	}})
	c := f.client(t)
	before := time.Now()
	_, err := c.Viewer(context.Background())
	after := time.Now()
	limited, ok := errors.AsType[*trackerapi.RateLimitedError](err)
	if !ok || limited.Status != http.StatusTooManyRequests {
		t.Fatalf("error %v", err)
	}
	if limited.Until.Before(before.Add(30*time.Second)) || limited.Until.After(after.Add(30*time.Second)) {
		t.Errorf("until %v, want 30s after the answer", limited.Until)
	}
	_, err = c.Teams(context.Background())
	if _, ok := errors.AsType[*trackerapi.NotSentError](err); !ok {
		t.Errorf("a call while the gate is closed: %v", err)
	}
}

// Without Retry-After the limit lifts at the reset of the exhausted
// budget, under a 429 or a RATELIMITED error on any other status.
func TestRateLimitedReset(t *testing.T) {
	t.Parallel()
	reset := time.UnixMilli(4000000000123)
	for _, status := range []int{http.StatusTooManyRequests, http.StatusBadRequest, http.StatusOK} {
		f := newFake(t, lineartest.Exchange{Op: "Viewer", Status: status, Body: limitBody, Header: map[string]string{
			"X-Ratelimit-Requests-Remaining": "100", "X-Ratelimit-Requests-Reset": "4000000000999",
			"X-Ratelimit-Complexity-Remaining": "0", "X-Ratelimit-Complexity-Reset": "4000000000123",
		}})
		_, err := f.client(t).Viewer(context.Background())
		limited, ok := errors.AsType[*trackerapi.RateLimitedError](err)
		if !ok || !limited.Until.Equal(reset) || limited.Status != status {
			t.Errorf("HTTP %d: error %v, want a limit until %v", status, err, reset)
		}
	}
}

func TestDialect(t *testing.T) {
	t.Parallel()
	d := dialect{}
	if p := d.Problem(400, []byte(inputBody)); p.Message != "Argument Validation Error (INPUT_ERROR)" {
		t.Errorf("Problem %q", p.Message)
	}
	if p := d.Problem(502, []byte("<html>bad gateway</html>")); p.Message != "" {
		t.Errorf("Problem of a non-GraphQL body %q", p.Message)
	}
	h := http.Header{}
	h.Set("X-Ratelimit-Endpoint-Requests-Reset", "4000000000777")
	h.Set("X-Ratelimit-Requests-Remaining", "0")
	h.Set("X-Ratelimit-Requests-Reset", "4000000000999")
	if until, ok := d.RateLimited(400, h, []byte(limitBody)); !ok || !until.Equal(time.UnixMilli(4000000000777)) {
		t.Errorf("endpoint limit: %v %t", until, ok)
	}
	if _, ok := d.RateLimited(200, h, []byte(`{"data":{"issue":{"title":"RATELIMITED in a title"}}}`)); ok {
		t.Error("the word in data is not a limit")
	}
	if until, ok := d.RateLimited(429, http.Header{}, nil); !ok || !until.IsZero() {
		t.Errorf("a bare 429: %v %t, want the transport's backoff", until, ok)
	}
}

// A 200 carrying errors is an *Error with each error's code, message,
// path and user flag; a read fails even when data came with it.
func TestGraphQLErrors(t *testing.T) {
	t.Parallel()
	partial := `{"data":{"viewer":{"id":"user-1"},"organization":null},` +
		`"errors":[{"message":"Internal","path":["organization",0],"extensions":{"type":"internal error","code":"INTERNAL_ERROR","userError":false}}]}`
	f := newFake(t, lineartest.Exchange{Op: "Viewer", Body: partial})
	_, err := f.client(t).Viewer(context.Background())
	e, ok := errors.AsType[*Error](err)
	if !ok {
		t.Fatalf("error %v", err)
	}
	want := GraphQLError{Message: "Internal", Code: "INTERNAL_ERROR", Type: "internal error", Path: []string{"organization", "0"}, rawMessage: "Internal"}
	if len(e.Errors) != 1 || e.Errors[0].String() != want.String() || e.Errors[0].Type != want.Type || e.Errors[0].UserError {
		t.Errorf("errors %+v", e.Errors)
	}
	if e.Op != "Viewer" || e.Refused || errors.Is(err, trackerapi.ErrNotFound) {
		t.Errorf("error %+v", e)
	}
}

func TestNotFound(t *testing.T) {
	t.Parallel()
	body := `{"data":null,"errors":[{"message":"Entity not found: Issue","path":["issue"],"extensions":{"type":"invalid input","code":"INPUT_ERROR","userError":true,"userPresentableMessage":"Could not find referenced Issue."}}]}`
	for _, status := range []int{http.StatusOK, http.StatusBadRequest} {
		f := newFake(t, lineartest.Exchange{Op: "Issue", Vars: `{"id":"WEB-404"}`, Status: status, Body: body})
		_, err := f.client(t).Issue(context.Background(), "WEB-404")
		if !errors.Is(err, trackerapi.ErrNotFound) {
			t.Errorf("HTTP %d: %v does not match ErrNotFound", status, err)
		}
		if !strings.Contains(err.Error(), "Could not find referenced Issue.") {
			t.Errorf("HTTP %d: message %q", status, err)
		}
	}
}
