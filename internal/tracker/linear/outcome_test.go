package linear

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agent-overflow/internal/tracker/trackerapi"
)

// outcome is what an answer proves about a write.
type outcome int

const (
	applied outcome = iota // no error
	refused                // an error with WriteNotApplied
	unknown                // an error without it
)

func (o outcome) String() string { return [...]string{"applied", "refused", "unknown"}[o] }

// What an answer to a mutation proves, for each payload by each kind of
// errors beside it:
//
//	payload           no errors  user errors only  other errors
//	missing           unknown    refused           unknown
//	success missing   unknown    unknown           unknown
//	success null      unknown    unknown           unknown
//	success malformed unknown    unknown           unknown
//	success false     refused    refused           refused
//	success true      applied    unknown           unknown
//
// A present success: false proves the refusal whatever comes with it,
// and the errors stay on the returned error.
func TestMutationOutcome(t *testing.T) {
	t.Parallel()
	issue := issueJSON("issue-1", "WEB-1", "2026-11-02T10:00:00.000Z", 1)
	payloads := []struct{ name, data string }{
		{"missing", `null`},
		{"null result", `{"result":null}`},
		// Each payload carries the entity, so only success decides.
		{"success missing", `{"result":{"value":` + issue + `}}`},
		{"success null", `{"result":{"success":null,"value":` + issue + `}}`},
		{"success malformed", `{"result":{"success":"true","value":` + issue + `}}`},
		{"success false", `{"result":{"success":false,"value":` + issue + `}}`},
		{"success true", `{"result":{"success":true,"value":` + issue + `}}`},
	}
	errorKinds := []struct{ name, errors string }{
		{"no errors", ``},
		{"user errors only", `,"errors":[{"message":"bad","extensions":{"code":"INPUT_ERROR","userError":true}}]`},
		{"other errors", `,"errors":[{"message":"bad","extensions":{"code":"INPUT_ERROR","userError":true}},{"message":"boom","extensions":{"code":"INTERNAL_ERROR"}}]`},
	}
	want := map[string][3]outcome{
		"missing":           {unknown, refused, unknown},
		"null result":       {unknown, refused, unknown},
		"success missing":   {unknown, unknown, unknown},
		"success null":      {unknown, unknown, unknown},
		"success malformed": {unknown, unknown, unknown},
		"success false":     {refused, refused, refused},
		"success true":      {applied, unknown, unknown},
	}
	for _, p := range payloads {
		for k, kind := range errorKinds {
			f := newFake(t, exchange{op: "ArchiveIssue", vars: `{"id":"WEB-1"}`, body: `{"data":` + p.data + kind.errors + `}`})
			_, err := f.client(t).ArchiveIssue(context.Background(), "WEB-1")
			if got := outcomeOf(err); got != want[p.name][k] {
				t.Errorf("%s, %s: %s, want %s (%v)", p.name, kind.name, got, want[p.name][k], err)
			}
			if e, ok := errors.AsType[*Error](err); ok && kind.errors != "" && len(e.Errors) == 0 {
				t.Errorf("%s, %s: the errors were dropped", p.name, kind.name)
			}
		}
	}
}

// The same rule holds for an answer the transport refused with a body,
// and for answers that are not a GraphQL payload at all.
func TestMutationOutcomeOtherAnswers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   outcome
	}{
		{"400 with errors", 400, inputBody, refused},
		{"500 with errors", 500, `{"errors":[{"message":"boom"}]}`, unknown},
		{"500 with success false", 500, `{"data":{"result":{"success":false,"value":null}},"errors":[{"message":"boom"}]}`, refused},
		{"500 with success true", 500, `{"data":{"result":{"success":true,"value":null}},"errors":[{"message":"boom"}]}`, unknown},
		{"auth error with success false", 200, `{"data":{"result":{"success":false,"value":null}},"errors":[{"message":"no","extensions":{"code":"AUTHENTICATION_ERROR"}}]}`, refused},
		{"auth error without payload", 200, `{"data":null,"errors":[{"message":"no","extensions":{"code":"AUTHENTICATION_ERROR"}}]}`, unknown},
		{"not JSON", 200, `{"data":`, unknown},
		{"success without entity", 200, `{"data":{"result":{"success":true,"value":null}}}`, unknown},
	} {
		f := newFake(t, exchange{op: "ArchiveIssue", vars: `{"id":"WEB-1"}`, status: tc.status, body: tc.body})
		_, err := f.client(t).ArchiveIssue(context.Background(), "WEB-1")
		if got := outcomeOf(err); got != tc.want {
			t.Errorf("%s: %s, want %s (%v)", tc.name, got, tc.want, err)
		}
		if _, ok := errors.AsType[*trackerapi.AuthError](err); strings.HasPrefix(tc.name, "auth error") && !ok {
			t.Errorf("%s: %v is not an AuthError", tc.name, err)
		}
	}
}

func outcomeOf(err error) outcome {
	switch {
	case err == nil:
		return applied
	case WriteNotApplied(err):
		return refused
	}
	return unknown
}
