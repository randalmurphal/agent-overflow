package linearprovider

import (
	"context"
	"errors"
	"testing"

	"agent-overflow/internal/secret"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear"
	"agent-overflow/internal/tracker/linear/lineartest"
	"agent-overflow/internal/tracker/trackerapi"
)

// A write whose answer arrived as its key was dropped has an unknown
// outcome: it was sent, and the answer that would say whether it applied
// is not returned.
func TestWriteAnsweredAfterTheKeyWasDropped(t *testing.T) {
	t.Parallel()
	var p *Provider
	id := issueID(1)
	srv := lineartest.NewServer(t, lineartest.Exchange{Op: "CreateComment", Vars: `{"input":{"issueId":"` + id + `","body":"x"}}`,
		BeforeBody: func() { p.c.ForgetCredential() }, Body: applied(`{"id":"comment-1"}`)})
	p = providerOf(t, srv)
	r := p.AddComment(context.Background(), tracker.CommentInput{Issue: tracker.IssueID(id), Markdown: "x"})
	expectResult(t, r, "1:comment:unknown")
	if !errors.Is(r.Parts[0].Outcome.Err(), trackerapi.ErrCredentialChanged) {
		t.Errorf("outcome %v", r.Parts[0].Outcome.Err())
	}
}

// A provider over a client bound to a workspace whose key is another
// workspace's sends nothing but the client's check: a read fails and a
// write is not sent, each with the client's error reachable.
func TestProviderOverAnotherWorkspace(t *testing.T) {
	t.Parallel()
	srv := lineartest.NewServer(t, lineartest.Exchange{Op: "Viewer", Body: lineartest.ViewerAnswer("other")})
	c, err := linear.New(linear.Options{
		BaseURL: srv.URL(), Version: "test", Workspace: "example",
		Credentials: trackerapi.StaticCredentials(trackerapi.Key(secret.New([]byte(lineartest.Key)))),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	p, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := p.Projects(ctx); !errors.As(err, new(*linear.WrongWorkspaceError)) {
		t.Errorf("read: %v", err)
	}
	for _, r := range []tracker.Result{
		p.AddComment(ctx, tracker.CommentInput{Issue: tracker.IssueID(issueID(1)), Markdown: "x"}),
		p.SetEstimate(ctx, tracker.EstimateInput{Board: web, Issue: tracker.IssueID(issueID(1))}),
	} {
		expectResult(t, r, "1:"+r.Parts[0].Step.String()+":not sent")
		if ww, ok := errors.AsType[*linear.WrongWorkspaceError](r.Parts[0].Outcome.Err()); !ok || ww.Want != "example" || ww.Got != "other" {
			t.Errorf("write %s: %v", describe(r), r.Parts[0].Outcome.Err())
		}
	}
}
