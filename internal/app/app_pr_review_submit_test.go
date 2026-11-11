package app

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/errorsx"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/harness/forgefake"
	"agent-overflow/internal/store"
)

// reviewSubmitCase is a PR on the fake forge with a thread whose drafts,
// and the PR's own drafts, the submits post.
type reviewSubmitCase struct {
	app    *App
	path   string
	engine *forgefake.Engine
	gate   *forgeGate
	thread store.Thread
	pr     gitops.PRReference
}

// publishRoute is the route that makes a review visible on each forge.
var publishRoute = map[string]string{"gitlab": "glab api publish draft notes", "github": "gh api pull reviews"}

func newReviewSubmitCase(t *testing.T, forge string) *reviewSubmitCase {
	t.Helper()
	app, path := newTestAppWithStorePath(t)
	engine := forgefake.New(forgefake.Options{APIToken: mergeForgeToken})
	if _, err := engine.Seed(forgefake.Fixture{Viewer: "alice", Repos: []forgefake.Repo{{Forge: forge, Project: "acme/widgets", Pulls: []forgefake.Pull{
		{Number: 9, Title: "Widgets", Author: "octo", HeadRef: "feature", State: "open"},
	}}}}); err != nil {
		t.Fatal(err)
	}
	gate := &forgeGate{next: engine, publish: map[string]string{"gitlab": "/draft_notes/bulk_publish", "github": "/reviews"}[forge]}
	useForgefake(t, app, gate)
	thread := testThread("review-submit-thread")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	pr := gitops.PRReference{Forge: forge, Host: map[string]string{"gitlab": "gitlab.com", "github": "github.com"}[forge], Namespace: "acme", Repo: "widgets", Number: 9}
	return &reviewSubmitCase{app: app, path: path, engine: engine, gate: gate, thread: thread, pr: pr}
}

// forgeGate holds the first review publish until released, so a test can
// act while a submit is at the forge.
type forgeGate struct {
	next    http.Handler
	publish string
	mu      sync.Mutex
	armed   bool
	entered chan struct{}
	release chan struct{}
}

// hold makes the next publish wait for the returned release.
func (g *forgeGate) hold() (entered <-chan struct{}, release func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.armed, g.entered, g.release = true, make(chan struct{}), make(chan struct{})
	return g.entered, sync.OnceFunc(func() { close(g.release) })
}

func (g *forgeGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	held := g.armed && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, g.publish)
	var release chan struct{}
	if held {
		g.armed = false
		close(g.entered)
		release = g.release
	}
	g.mu.Unlock()
	if held {
		<-release
	}
	g.next.ServeHTTP(w, r)
}

func (c *reviewSubmitCase) owner(threadOwned bool) store.DiffReviewOwner {
	if threadOwned {
		return store.ThreadReviewOwner(c.thread.ID)
	}
	return store.PRReviewOwner(c.pr.Key())
}

// draft writes one pr-scope draft, the thread's or the PR's.
func (c *reviewSubmitCase) draft(t *testing.T, threadOwned bool, line int, body string) store.DiffReviewComment {
	t.Helper()
	input := store.DiffReviewCommentInput{Scope: string(store.DiffReviewScopePR), SourceKey: prDraftSource(c.pr), FilePath: "main.go", NewLine: line, Side: "new", Body: body}
	var (
		created store.DiffReviewComment
		err     error
	)
	if threadOwned {
		created, err = c.app.CreateDiffReviewComment(c.thread.ID, input)
	} else {
		created, err = c.app.CreatePRDiffReviewComment(c.pr, input)
	}
	if err != nil {
		t.Fatal(err)
	}
	return created
}

// request is a draft submit of drafts, the thread's or the PR's.
func (c *reviewSubmitCase) request(threadOwned bool, drafts ...store.DiffReviewComment) SubmitPRReviewRequest {
	selection := &SubmitPRReviewDrafts{HeadSHA: "head"}
	if threadOwned {
		selection.ThreadID = c.thread.ID
	}
	for _, draft := range drafts {
		selection.IDs = append(selection.IDs, draft.ID)
	}
	return SubmitPRReviewRequest{Verdict: gitops.ReviewVerdictComment, Body: "Summary", Drafts: selection}
}

func (c *reviewSubmitCase) submit(review SubmitPRReviewRequest) (SubmitPRReviewResult, error) {
	return c.app.SubmitPRReview(context.Background(), c.pr, review)
}

// published counts the reviews the forge accepted.
func (c *reviewSubmitCase) published() int {
	n := 0
	for _, inv := range c.engine.Invocations(0).Invocations {
		if inv.Route == publishRoute[c.pr.Forge] && inv.Status < 300 {
			n++
		}
	}
	return n
}

// postedBodies are the comment bodies of the PR's review threads.
func (c *reviewSubmitCase) postedBodies(t *testing.T) []string {
	t.Helper()
	threads, err := c.app.gitCore().ListReviewThreads(context.Background(), c.pr)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, thread := range threads {
		for _, comment := range thread.Comments {
			out = append(out, comment.Body)
		}
	}
	return out
}

func (c *reviewSubmitCase) comment(t *testing.T, threadOwned bool, id string) store.DiffReviewComment {
	t.Helper()
	got, err := c.app.store.GetDiffReviewComment(c.owner(threadOwned), id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func reviewSubmitCode(err error) string {
	code, _, _ := errorsx.PublicDetails(err)
	return code
}

// A draft submit posts the stored drafts and marks them sent in the same
// call, on both forges, for a thread's drafts and a PR's. The same submit
// again (a retry after a lost answer) posts nothing and is refused as
// already sent.
func TestSubmitPRReviewWithDraftsPostsAndMarksThemInOneCall(t *testing.T) {
	t.Parallel()
	for _, forge := range []string{"gitlab", "github"} {
		for _, threadOwned := range []bool{true, false} {
			c := newReviewSubmitCase(t, forge)
			draft := c.draft(t, threadOwned, 3, "Guard this.")
			review := c.request(threadOwned, draft)
			review.Comments = []gitops.ReviewLineComment{{Path: "main.go", Body: "the caller's text, ignored", Side: "file"}}
			result, err := c.submit(review)
			if err != nil || !result.PostedReview || !slices.Equal(result.SentDraftIDs, []string{draft.ID}) || len(result.EditedDraftIDs) != 0 {
				t.Fatalf("%s thread=%v: submit = %+v, %v", forge, threadOwned, result, err)
			}
			if bodies := c.postedBodies(t); !slices.Contains(bodies, "Guard this.") || slices.Contains(bodies, "the caller's text, ignored") {
				t.Fatalf("%s thread=%v: the forge holds %q", forge, threadOwned, bodies)
			}
			got := c.comment(t, threadOwned, draft.ID)
			wantTurn := ""
			if threadOwned {
				wantTurn = "pr:head"
			}
			if got.Status != "sent" || got.SentTurnID != wantTurn {
				t.Fatalf("%s thread=%v: draft = %+v", forge, threadOwned, got)
			}
			if _, err := c.submit(review); reviewSubmitCode(err) != ReviewDraftsNotDraftsCode {
				t.Fatalf("%s thread=%v: retry = %v, want %s", forge, threadOwned, err, ReviewDraftsNotDraftsCode)
			}
			if n := c.published(); n != 1 {
				t.Fatalf("%s thread=%v: published %d reviews, want 1", forge, threadOwned, n)
			}
		}
	}
}

// A request without drafts, an older client's, is exactly the comments it
// carries: posted as given, nothing marked; the client marks its drafts
// with the separate call, which still works.
func TestSubmitPRReviewWithoutDraftsPostsTheCallersComments(t *testing.T) {
	t.Parallel()
	c := newReviewSubmitCase(t, "gitlab")
	draft := c.draft(t, false, 3, "Guard this.")
	line := 3
	result, err := c.submit(SubmitPRReviewRequest{Verdict: gitops.ReviewVerdictComment, Comments: []gitops.ReviewLineComment{{Path: "main.go", Body: "Guard this.", Line: &line, Side: "right"}}})
	if err != nil || !result.PostedReview || len(result.SentDraftIDs) != 0 {
		t.Fatalf("submit = %+v, %v", result, err)
	}
	if got := c.comment(t, false, draft.ID); got.Status != "draft" {
		t.Fatalf("a submit without drafts marked %+v", got)
	}
	if err := c.app.MarkPRDiffReviewCommentsSent(c.pr, draft.Scope, draft.SourceKey, []string{draft.ID}); err != nil {
		t.Fatal(err)
	}
	if got := c.comment(t, false, draft.ID); got.Status != "sent" {
		t.Fatalf("after the client's mark the draft is %+v", got)
	}
}

// Two submits of the same drafts at once (two tabs, two devices on one
// backend): the second waits for the first, finds its drafts sent and
// posts nothing.
func TestSubmitPRReviewOverlappingSubmitsPostOnce(t *testing.T) {
	t.Parallel()
	for _, forge := range []string{"gitlab", "github"} {
		c := newReviewSubmitCase(t, forge)
		draft := c.draft(t, true, 3, "Guard this.")
		entered, release := c.gate.hold()
		defer release()
		first := make(chan error, 1)
		go func() {
			_, err := c.submit(c.request(true, draft))
			first <- err
		}()
		<-entered
		second := make(chan error, 1)
		go func() {
			_, err := c.submit(c.request(true, draft))
			second <- err
		}()
		// The second is waiting on the first, or (without the lock) at the
		// forge already.
		deadline := time.Now().Add(10 * time.Second)
		for c.app.reviewSubmitLocks().Refs(c.pr.Key()) < 2 && c.published() == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		release()
		if err := <-first; err != nil {
			t.Fatalf("%s: first = %v", forge, err)
		}
		if err := <-second; reviewSubmitCode(err) != ReviewDraftsNotDraftsCode {
			t.Fatalf("%s: second = %v, want %s", forge, err, ReviewDraftsNotDraftsCode)
		}
		if n := c.published(); n != 1 {
			t.Fatalf("%s: published %d reviews, want 1", forge, n)
		}
	}
}

// On GitLab a publish takes every pending draft note of the viewer, so a
// submit of a thread's drafts and one of the PR's own on the same merge
// request run one after the other: the second creates no draft note while
// the first is publishing.
func TestSubmitPRReviewGitLabSubmitsOfOneMergeRequestDoNotInterleave(t *testing.T) {
	t.Parallel()
	c := newReviewSubmitCase(t, "gitlab")
	threadDraft := c.draft(t, true, 3, "From the thread.")
	prDraft := c.draft(t, false, 4, "From the page.")
	created := func() int {
		n := 0
		for _, inv := range c.engine.Invocations(0).Invocations {
			if inv.Route == "glab api create draft note" {
				n++
			}
		}
		return n
	}
	entered, release := c.gate.hold()
	defer release()
	first := make(chan error, 1)
	go func() {
		_, err := c.submit(c.request(true, threadDraft))
		first <- err
	}()
	<-entered
	second := make(chan error, 1)
	go func() {
		_, err := c.submit(c.request(false, prDraft))
		second <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for c.app.reviewSubmitLocks().Refs(c.pr.Key()) < 2 && created() <= 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// The first's summary and comment; the second's wait for its publish.
	if n := created(); n != 2 {
		t.Fatalf("%d draft notes created while the first published, want 2", n)
	}
	release()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if n := c.published(); n != 2 {
		t.Fatalf("published %d reviews, want 2", n)
	}
}

// A submit without draft ids (an older client) holds the PR's lock too:
// with either one publishing first, the other creates no draft note until
// that publish is done, so neither publishes the other's notes.
func TestSubmitPRReviewGitLabSubmitsOfBothShapesDoNotInterleave(t *testing.T) {
	t.Parallel()
	for _, legacyFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("legacy first %v", legacyFirst), func(t *testing.T) {
			t.Parallel()
			c := newReviewSubmitCase(t, "gitlab")
			stored := c.draft(t, false, 3, "From the page.")
			line := 4
			legacy := SubmitPRReviewRequest{Verdict: gitops.ReviewVerdictComment, Body: "Older client", Comments: []gitops.ReviewLineComment{{Path: "main.go", Body: "From an older client.", Line: &line, Side: "right"}}}
			first, second := legacy, c.request(false, stored)
			if !legacyFirst {
				first, second = second, first
			}
			created := func() int {
				n := 0
				for _, inv := range c.engine.Invocations(0).Invocations {
					if inv.Route == "glab api create draft note" {
						n++
					}
				}
				return n
			}
			entered, release := c.gate.hold()
			defer release()
			firstDone := make(chan error, 1)
			go func() {
				_, err := c.submit(first)
				firstDone <- err
			}()
			<-entered
			secondDone := make(chan error, 1)
			go func() {
				_, err := c.submit(second)
				secondDone <- err
			}()
			deadline := time.Now().Add(10 * time.Second)
			for c.app.reviewSubmitLocks().Refs(c.pr.Key()) < 2 && created() <= 2 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if n := c.app.reviewSubmitLocks().Refs(c.pr.Key()); n != 2 {
				t.Fatalf("%d submits on the lock while the first published, want 2 (the second waiting)", n)
			}
			if n := created(); n != 2 {
				t.Fatalf("%d draft notes created while the first published, want 2", n)
			}
			release()
			if err := <-firstDone; err != nil {
				t.Fatal(err)
			}
			if err := <-secondDone; err != nil {
				t.Fatal(err)
			}
			if n := c.published(); n != 2 {
				t.Fatalf("published %d reviews, want 2", n)
			}
			if got := c.comment(t, false, stored.ID).Status; got != "sent" {
				t.Fatalf("stored draft is %q, want sent", got)
			}
		})
	}
}

// A draft edited while its review is at the forge was posted with its old
// text: it stays a draft with the new text, and the result says so.
func TestSubmitPRReviewLeavesADraftEditedWhileItPosts(t *testing.T) {
	t.Parallel()
	c := newReviewSubmitCase(t, "gitlab")
	edited := c.draft(t, false, 3, "Guard this.")
	kept := c.draft(t, false, 4, "And this.")
	entered, release := c.gate.hold()
	defer release()
	done := make(chan struct{})
	var (
		result SubmitPRReviewResult
		err    error
	)
	go func() {
		defer close(done)
		result, err = c.submit(c.request(false, edited, kept))
	}()
	<-entered
	if _, err := c.app.UpdatePRDiffReviewComment(c.pr, edited.ID, store.DiffReviewCommentUpdate{Body: "Guard this, and log it."}); err != nil {
		t.Fatal(err)
	}
	release()
	<-done
	if err != nil || !slices.Equal(result.SentDraftIDs, []string{kept.ID}) || !slices.Equal(result.EditedDraftIDs, []string{edited.ID}) {
		t.Fatalf("submit = %+v, %v", result, err)
	}
	if got := c.comment(t, false, edited.ID); got.Status != "draft" || got.Body != "Guard this, and log it." {
		t.Fatalf("the edited draft = %+v", got)
	}
	if got := c.comment(t, false, kept.ID); got.Status != "sent" {
		t.Fatalf("the other draft = %+v", got)
	}
}

// The forge accepts a review and its drafts cannot be marked: the submit
// says the review posted, a submit while marking still fails posts
// nothing, and the next read marks them. Posted once throughout.
func TestSubmitPRReviewPostedButUnmarkedReconcilesWithoutASecondPost(t *testing.T) {
	t.Parallel()
	for _, threadOwned := range []bool{true, false} {
		c := newReviewSubmitCase(t, "gitlab")
		draft := c.draft(t, threadOwned, 3, "Guard this.")
		db, err := sql.Open("sqlite", c.path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := db.Close(); err != nil {
				t.Error(err)
			}
		})
		if _, err := db.Exec(`CREATE TRIGGER refuse_review_sent BEFORE UPDATE OF status ON diff_review_comments WHEN NEW.status = 'sent' BEGIN SELECT RAISE(ABORT, 'injected mark failure'); END`); err != nil {
			t.Fatal(err)
		}
		if _, err := c.submit(c.request(threadOwned, draft)); reviewSubmitCode(err) != ReviewPostedUnmarkedCode {
			t.Fatalf("thread=%v: submit = %v, want %s", threadOwned, err, ReviewPostedUnmarkedCode)
		}
		if _, err := c.submit(c.request(threadOwned, draft)); reviewSubmitCode(err) != ReviewPostedUnmarkedCode {
			t.Fatalf("thread=%v: a submit while marking fails = %v, want %s", threadOwned, err, ReviewPostedUnmarkedCode)
		}
		if n := c.published(); n != 1 {
			t.Fatalf("thread=%v: published %d reviews, want 1", threadOwned, n)
		}
		if _, err := db.Exec(`DROP TRIGGER refuse_review_sent`); err != nil {
			t.Fatal(err)
		}
		var listed []store.DiffReviewComment
		if threadOwned {
			listed, err = c.app.ListDiffReviewComments(c.thread.ID, draft.Scope, draft.SourceKey)
		} else {
			listed, err = c.app.ListPRDiffReviewComments(c.pr, draft.Scope, draft.SourceKey)
		}
		if err != nil || len(listed) != 1 || listed[0].Status != "sent" {
			t.Fatalf("thread=%v: the read after = %+v, %v", threadOwned, listed, err)
		}
		if _, err := c.submit(c.request(threadOwned, draft)); reviewSubmitCode(err) != ReviewDraftsNotDraftsCode {
			t.Fatalf("thread=%v: a submit after = %v, want %s", threadOwned, err, ReviewDraftsNotDraftsCode)
		}
		if n := c.published(); n != 1 {
			t.Fatalf("thread=%v: published %d reviews, want 1", threadOwned, n)
		}
	}
}

// A review the forge refused leaves its drafts unsent (on GitLab the draft
// notes it created are removed), and the next submit posts once.
func TestSubmitPRReviewRefusedLeavesTheDraftsUnsent(t *testing.T) {
	t.Parallel()
	for _, forge := range []string{"gitlab", "github"} {
		c := newReviewSubmitCase(t, forge)
		draft := c.draft(t, true, 3, "Guard this.")
		if err := c.engine.SetRefusal(forgefake.Refusal{Route: publishRoute[forge], Status: http.StatusForbidden, Times: 1}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.submit(c.request(true, draft)); err == nil {
			t.Fatalf("%s: a refused review succeeded", forge)
		}
		if got := c.comment(t, true, draft.ID); got.Status != "draft" {
			t.Fatalf("%s: after the refusal the draft is %+v", forge, got)
		}
		result, err := c.submit(c.request(true, draft))
		if err != nil || !slices.Equal(result.SentDraftIDs, []string{draft.ID}) {
			t.Fatalf("%s: retry = %+v, %v", forge, result, err)
		}
		if bodies := c.postedBodies(t); strings.Count(strings.Join(bodies, "\n"), "Guard this.") != 1 {
			t.Fatalf("%s: the forge holds %q, want the comment once", forge, bodies)
		}
	}
}
