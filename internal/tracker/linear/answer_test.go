package linear

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker/trackerapi"
)

// Answer text that echoes the key the request carried never reaches an
// error: GraphQL messages, codes, types and paths, Linear's message
// for a refused key, and a decoder's quote of the data.
func TestAnswerTextIsScrubbed(t *testing.T) {
	t.Parallel()
	echoed := `{"data":null,"errors":[{"message":"bad ` + testKey + `","path":["` + testKey + `"],` +
		`"extensions":{"code":"` + testKey + `","type":"` + testKey + `","userError":true}}]}`
	presentable := `{"errors":[{"message":"m","extensions":{"code":"INPUT_ERROR","userPresentableMessage":"key ` + testKey + `"}}]}`
	auth := `{"errors":[{"message":"key ` + testKey + ` refused","extensions":{"code":"AUTHENTICATION_ERROR"}}]}`
	badTime := `{"data":{"issue":` + issueJSON("issue-1", "WEB-1", testKey, 1) + `}}`
	for _, tc := range []struct {
		name   string
		status int
		body   string
		issue  bool // ask for an issue rather than the viewer
	}{
		{"errors on 200", 200, echoed, false},
		{"errors on 400", 400, echoed, false},
		{"presentable message", 200, presentable, false},
		{"refused key on 200", 200, auth, false},
		{"refused key on 401", 401, auth, false},
		{"decoder quote", 200, badTime, true},
	} {
		var err error
		if tc.issue {
			f := newFake(t, exchange{op: "Issue", vars: `{"id":"WEB-1"}`, status: tc.status, body: tc.body})
			_, err = f.client(t).Issue(context.Background(), "WEB-1")
		} else {
			f := newFake(t, exchange{op: "Viewer", status: tc.status, body: tc.body})
			_, err = f.client(t).Viewer(context.Background())
		}
		if err == nil {
			t.Errorf("%s: no error", tc.name)
			continue
		}
		text := err.Error()
		if e, ok := errors.AsType[*Error](err); ok {
			for _, g := range e.Errors {
				text += fmt.Sprint(g.Message, g.Code, g.Type, g.Path, g.rawMessage)
			}
		}
		if auth, ok := errors.AsType[*trackerapi.AuthError](err); ok {
			text += auth.Message
		}
		if strings.Contains(text, testKey) || !strings.Contains(text, secret.Redacted) {
			t.Errorf("%s: %s", tc.name, text)
		}
	}
}

// Not found is the resource the call asked for missing, said by an
// error at the root field of the call. The same error deeper in the
// answer, or without a path, is a failed read.
func TestNotFoundOnlyAtRoot(t *testing.T) {
	t.Parallel()
	missing := func(path string) string {
		return `{"data":null,"errors":[{"message":"Entity not found: Issue",` + path +
			`"extensions":{"code":"INPUT_ERROR","userError":true}}]}`
	}
	for _, tc := range []struct {
		name, op, body string
		want           bool
	}{
		{"read root", "Issue", missing(`"path":["issue"],`), true},
		{"read nested", "Issue", missing(`"path":["issue","project"],`), false},
		{"read no path", "Issue", missing(``), false},
		{"read other code", "Issue", strings.Replace(missing(`"path":["issue"],`), "INPUT_ERROR", "INTERNAL_ERROR", 1), false},
		{"read other message", "Issue", strings.Replace(missing(`"path":["issue"],`), "Entity not found", "Bad", 1), false},
		{"write root", "ArchiveIssue", missing(`"path":["result"],`), true},
		{"write nested", "ArchiveIssue", missing(`"path":["result","value"],`), false},
	} {
		f := newFake(t, exchange{op: tc.op, vars: `{"id":"WEB-1"}`, body: tc.body})
		c := f.client(t)
		var err error
		if tc.op == "Issue" {
			_, err = c.Issue(context.Background(), "WEB-1")
		} else {
			_, err = c.ArchiveIssue(context.Background(), "WEB-1")
		}
		if _, ok := errors.AsType[*Error](err); !ok {
			t.Errorf("%s: %v is not an *Error", tc.name, err)
		}
		if got := errors.Is(err, trackerapi.ErrNotFound); got != tc.want {
			t.Errorf("%s: ErrNotFound %t, want %t (%v)", tc.name, got, tc.want, err)
		}
	}
}

// A mutation is sent once whatever the failure: a 503, a reset
// connection, a rate limit answered on a 200 to a person's own action.
// A query is sent again after each.
func TestRetryOnlyQueries(t *testing.T) {
	t.Parallel()
	limited := `{"errors":[{"message":"Rate limit exceeded","extensions":{"code":"RATELIMITED"}}]}`
	unavailable := exchange{status: http.StatusServiceUnavailable, header: map[string]string{"Retry-After": "0"}, body: `{"errors":[{"message":"busy"}]}`}
	rateLimited := exchange{body: limited, header: map[string]string{"Retry-After": "0"}}
	reset := exchange{reset: true}
	for name, failure := range map[string]exchange{"503": unavailable, "rate limit on 200": rateLimited, "reset": reset} {
		mutation := failure
		mutation.op, mutation.vars = "ArchiveIssue", `{"id":"WEB-1"}`
		f := newFake(t, mutation)
		ctx := trackerapi.WithInteractive(context.Background())
		if _, err := f.client(t).ArchiveIssue(ctx, "WEB-1"); err == nil {
			t.Errorf("%s: the mutation succeeded", name)
		}

		query := failure
		query.op = "Viewer"
		f = newFake(t, query, exchange{op: "Viewer", body: viewerData})
		if _, err := f.client(t).Viewer(ctx); err != nil {
			t.Errorf("%s: the query was not sent again: %v", name, err)
		}
	}
}
