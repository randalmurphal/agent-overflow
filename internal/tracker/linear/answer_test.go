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

// escapedKey is testKey as JSON may spell it inside a string, its first
// underscore written as a \u escape, which a text scrub of the raw body
// cannot match.
var escapedKey = strings.Replace(testKey, "_", `\`+"u005f", 1)

type scrubCase struct {
	name   string
	status int
	body   string
	issue  bool // ask for an issue rather than the viewer
}

// scrubCases puts the key, as written and JSON-escaped, in each place
// of an error answer, under each status that reaches a different path:
// a 200 the client decodes, a 400 the transport refuses with a body,
// and a 401 the transport takes as a refused key.
func scrubCases() []scrubCase {
	var cases []scrubCase
	for _, key := range []string{testKey, escapedKey} {
		bodies := map[string]string{
			"message, code, type and path": `{"data":null,"errors":[{"message":"bad ` + key + `","path":["` + key + `"],` +
				`"extensions":{"code":"` + key + `","type":"` + key + `","userError":true}}]}`,
			"presentable message": `{"errors":[{"message":"m","extensions":{"code":"INPUT_ERROR","userPresentableMessage":"key ` + key + `"}}]}`,
			"raw message":         `{"errors":[{"message":"key ` + key + `","extensions":{"code":"INPUT_ERROR","userPresentableMessage":"m"}}]}`,
			"refused key":         `{"errors":[{"message":"key ` + key + ` refused","extensions":{"code":"AUTHENTICATION_ERROR"}}]}`,
		}
		for what, body := range bodies {
			for _, status := range []int{http.StatusOK, http.StatusBadRequest, http.StatusUnauthorized} {
				cases = append(cases, scrubCase{name: fmt.Sprintf("%s, %s, HTTP %d", key, what, status), status: status, body: body})
			}
		}
		issue := strings.Replace(issueJSON("issue-1", "WEB-1", "KEY", 1), `"updatedAt":"KEY"`, `"updatedAt":"`+key+`"`, 1)
		cases = append(cases, scrubCase{name: key + ", decoder quote", status: http.StatusOK, body: `{"data":{"issue":` + issue + `}}`, issue: true})
	}
	return cases
}

// Answer text that echoes the key the request carried never reaches an
// error, however JSON spells it: GraphQL messages, codes, types and
// paths, Linear's message for a refused key, and a decoder's quote of
// the data. Text is scrubbed after it is decoded.
func TestAnswerTextIsScrubbed(t *testing.T) {
	t.Parallel()
	for _, tc := range scrubCases() {
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
		// The transport's message for a 401 shows the presentable message
		// only, so the raw one leaves no trace to redact.
		shown := tc.status != http.StatusUnauthorized || !strings.Contains(tc.name, "raw message")
		if strings.Contains(text, testKey) || (shown && !strings.Contains(text, secret.Redacted)) {
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
