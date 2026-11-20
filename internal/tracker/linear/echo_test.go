package linear

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker/linear/lineartest"
	"agent-overflow/internal/tracker/trackerapi"
)

// A successful answer that echoes the key, raw or written with a JSON
// escape, in a viewer's name, an issue's title or a GraphQL error inside
// a 200, never puts it in a returned value or error text: each holds the
// redaction mark. The scrub is the transport's (Response.Body arrives
// scrubbed); this checks that no path here reads the answer otherwise.
func TestEchoedKeyNeverReturned(t *testing.T) {
	t.Parallel()
	escaped := strings.Replace(lineartest.Key, "_", `_`, 1)
	viewer := func(name string) string {
		return strings.Replace(lineartest.ViewerAnswer("example"), `"name":"User 1"`, `"name":"User `+name+`"`, 1)
	}
	is := lineartest.NewIssue("6f1c2a3b-0000-4000-8000-000000000001", "WEB-1", "2026-11-02T10:00:00.000Z", 1)
	is.Title = "Rotate " + lineartest.Key
	f := newFake(t,
		lineartest.Exchange{Op: "Viewer", Body: viewer(lineartest.Key)},
		lineartest.Exchange{Op: "Viewer", Body: viewer(escaped)},
		lineartest.Exchange{Op: "IssueSummary", Vars: `{"id":"WEB-1"}`, Body: `{"data":{"issue":` + is.JSON() + `}}`},
		lineartest.Exchange{Op: "Teams", Vars: `{"first":100}`,
			Body: `{"data":null,"errors":[{"message":"key ` + escaped + ` is not valid here","extensions":{"code":"INPUT_ERROR"}}]}`},
	)
	c := f.client(t)
	ctx := context.Background()
	clean := func(what, s string) {
		t.Helper()
		if strings.Contains(s, lineartest.Key) || !strings.Contains(s, secret.Redacted) {
			t.Errorf("%s %q holds the key or no redaction mark", what, s)
		}
	}
	for _, how := range []string{"raw", "escaped"} {
		v, err := c.Viewer(ctx)
		if err != nil {
			t.Fatal(err)
		}
		clean("viewer name ("+how+")", v.User.Name)
	}
	got, err := c.IssueSummary(ctx, "WEB-1")
	if err != nil {
		t.Fatal(err)
	}
	clean("issue title", got.Title)
	_, err = c.Teams(ctx)
	if err == nil {
		t.Fatal("an answer with errors succeeded")
	}
	clean("error text", err.Error())
	if e, ok := errors.AsType[*Error](err); !ok || len(e.Errors) != 1 {
		t.Fatalf("error %v", err)
	} else {
		clean("GraphQL error message", e.Errors[0].Message)
	}
}

// An answer whose key was dropped while it was read is not returned: a
// read fails with trackerapi.ErrCredentialChanged, and a mutation's
// outcome is unknown, neither not sent nor refused.
func TestAnswerAfterTheKeyWasDropped(t *testing.T) {
	t.Parallel()
	var c *Client
	forget := func() { c.ForgetCredential() }
	f := newFake(t,
		lineartest.Exchange{Op: "Teams", Vars: `{"first":100}`, BeforeBody: forget, Body: lineartest.Data(lineartest.PageJSON(nil, false, ""))},
		lineartest.Exchange{Op: "DeleteComment", Vars: `{"id":"comment-1"}`, BeforeBody: forget, Body: `{"data":{"result":{"success":true}}}`},
	)
	c = f.client(t)
	ctx := context.Background()
	if _, err := c.Teams(ctx); !errors.Is(err, trackerapi.ErrCredentialChanged) {
		t.Errorf("read: %v", err)
	}
	err := c.DeleteComment(ctx, "comment-1")
	_, notSent := errors.AsType[*trackerapi.NotSentError](err)
	if !errors.Is(err, trackerapi.ErrCredentialChanged) || notSent || WriteNotApplied(err) {
		t.Errorf("mutation: %v (not sent %t, not applied %t)", err, notSent, WriteNotApplied(err))
	}
}
