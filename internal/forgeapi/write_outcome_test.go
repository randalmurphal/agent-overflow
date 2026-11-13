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
		{"GraphQL refusal beside a null field", &GraphQLError{Problems: []GraphQLProblem{{Type: "FORBIDDEN", Message: "no"}}, Data: []byte(`{"m":null}`)}, true},
		{"GraphQL refusal beside data that did not decode", &GraphQLError{Problems: []GraphQLProblem{{Type: "FORBIDDEN", Message: "no"}}, Data: []byte(`{"m":null}`), Undecoded: true}, false},
		{"GraphQL refusal of an unknown type", &GraphQLError{Problems: []GraphQLProblem{{Type: "SOMETHING", Message: "no"}}, Data: []byte(`{"m":null}`)}, false},
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

// A GraphQL answer that is also a rate-limit refusal proves a write not
// applied only while it carried no data: a mutation that produced data
// ran, whatever the quota headers say.
func TestWriteNotAppliedRateLimitedGraphQLOnTheWire(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"data beside an execution error, quota spent", `{"data":{"m":{"id":"1"}},"errors":[{"message":"Something went wrong while executing your query."}]}`, false},
		{"rate limited, no data", `{"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`, true},
		{"rate limited, null data", `{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`, true},
		{"rate limited, null field", `{"data":{"m":null},"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-RateLimit-Limit", "5000")
				w.Header().Set("X-RateLimit-Remaining", "0")
				w.Header().Set("X-RateLimit-Reset", "4102444800")
				w.Header().Set("X-RateLimit-Resource", "graphql")
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)
			var out struct {
				M *struct {
					ID string `json:"id"`
				} `json:"m"`
			}
			_, err := isolatedService(t, srv).GitHub("github.com").GraphQL(t.Context(), "M", "mutation M { m { id } }", nil, &out)
			if _, limited := errors.AsType[*RateLimitedError](err); !limited {
				t.Fatalf("err = %v, want the rate limit", err)
			}
			if got := WriteNotApplied(err); got != tc.want {
				t.Fatalf("WriteNotApplied(%v) = %v, want %v", err, got, tc.want)
			}
		})
	}
}

// On the wire, a GraphQL answer with errors proves a write not applied
// only when it carried no data or a null field, every error is a known
// refusal, and its data decoded.
func TestWriteNotAppliedGraphQLOnTheWire(t *testing.T) {
	t.Parallel()
	const gitlabDenied = "The resource that you are attempting to access does not exist or you don't have permission to perform this action"
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"GitHub refusal, null field", `{"data":{"m":null},"errors":[{"type":"FORBIDDEN","message":"no","path":["m"]}]}`, true},
		{"GitHub refusal, null data", `{"data":null,"errors":[{"type":"UNPROCESSABLE","message":"no"}]}`, true},
		{"validation, no data", `{"errors":[{"message":"Field 'x' doesn't exist on type 'Mutation'","extensions":{"code":"undefinedField"}}]}`, true},
		{"GitLab permission answer", `{"data":{"m":null},"errors":[{"message":"` + gitlabDenied + `","path":["m"]}]}`, true},
		{"partial data with errors", `{"data":{"m":{"id":"1"}},"errors":[{"type":"FORBIDDEN","message":"no"}]}`, false},
		{"internal error, null payload", `{"data":{"m":null},"errors":[{"message":"Something went wrong while executing your query."}]}`, false},
		{"timeout, null data", `{"data":null,"errors":[{"message":"Timeout on validation of query"}]}`, false},
		{"refusal beside data that does not decode", `{"data":{"m":5},"errors":[{"type":"FORBIDDEN","message":"no"}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)
			var out struct {
				M *struct {
					ID string `json:"id"`
				} `json:"m"`
			}
			_, err := isolatedService(t, srv).GitHub("github.com").GraphQL(t.Context(), "M", "mutation M { m { id } }", nil, &out)
			if err == nil {
				t.Fatal("no error")
			}
			if got := WriteNotApplied(err); got != tc.want {
				t.Fatalf("WriteNotApplied(%v) = %v, want %v", err, got, tc.want)
			}
		})
	}
}
