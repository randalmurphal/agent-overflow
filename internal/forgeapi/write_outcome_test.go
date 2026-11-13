package forgeapi

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A write's error proves the forge did not apply it only when the request
// never left the process or the forge refused it in full with a 4xx.
func TestWriteNotAppliedClassifiesWhatAWritesErrorProves(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"no error", nil, false},
		{"never sent", &NotSentError{Err: errors.New("dial refused")}, true},
		{"never sent, wrapped", fmt.Errorf("submit: %w", &NotSentError{Err: errors.New("dial refused")}), true},
		{"422", &StatusError{Status: http.StatusUnprocessableEntity}, true},
		{"403 forbidden", &ForbiddenError{Status: &StatusError{Status: http.StatusForbidden}}, true},
		{"401 after a refresh", &TransientError{Err: &StatusError{Status: http.StatusUnauthorized}}, true},
		{"rate limited", &RateLimitedError{Host: "github.com"}, true},
		{"login refused", &SetupError{Kind: SetupUnauthenticated}, true},
		{"GraphQL refusal", &GraphQLError{Problems: []GraphQLProblem{{Type: "FORBIDDEN", Message: "no"}}}, true},
		{"502", &StatusError{Status: http.StatusBadGateway}, false},
		{"reset after sending", &TransientError{Err: errors.New("connection reset")}, false},
		{"other", errors.New("decode response"), false},
	} {
		if got := WriteNotApplied(tc.err); got != tc.want {
			t.Errorf("%s: WriteNotApplied = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// On the wire: a forge that cannot be reached was never sent the request;
// one that cut its answer short, or closed with none, may have applied it.
func TestWriteNotAppliedOnTheWire(t *testing.T) {
	t.Parallel()
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	_, err := isolatedService(t, closed).GitHub("github.com").Do(t.Context(), Request{Method: http.MethodPost, Path: "repos/o/r/pulls/1/reviews", Body: map[string]string{}})
	if _, ok := errors.AsType[*NotSentError](err); !ok || !WriteNotApplied(err) {
		t.Fatalf("a refused dial = %v; want NotSentError", err)
	}
	if _, ok := errors.AsType[*TransientError](err); !ok {
		t.Fatalf("a refused dial = %v; want it still a TransientError", err)
	}

	for _, how := range []string{"truncate", "drop"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, buf, err := http.NewResponseController(w).Hijack()
			if err != nil {
				panic(err)
			}
			defer func() { _ = conn.Close() }()
			if how == "truncate" {
				_, _ = buf.WriteString("HTTP/1.1 201 Created\r\nContent-Type: application/json\r\nContent-Length: 64\r\n\r\n{\"id\":")
				_ = buf.Flush()
			}
		}))
		_, err := isolatedService(t, srv).GitHub("github.com").Do(t.Context(), Request{Method: http.MethodPost, Path: "repos/o/r/pulls/1/reviews", Body: map[string]string{}})
		srv.Close()
		if err == nil || WriteNotApplied(err) {
			t.Fatalf("%s: err = %v; want an error that proves nothing", how, err)
		}
	}
}
