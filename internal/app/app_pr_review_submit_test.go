package app

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
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
// act while a submit is at the forge, or loses the next publish's answer.
type forgeGate struct {
	next    http.Handler
	publish string
	mu      sync.Mutex
	armed   bool
	entered chan struct{}
	release chan struct{}
	// losing is how the next publish's answer is lost: "drop" runs it and
	// closes the connection with no answer, "truncate" runs it and cuts
	// its answer short, "unsent" closes the connection without running it.
	losing string
	// deletesGone answers every draft note delete 404 without running it,
	// as if another publish had taken the notes.
	deletesGone bool
	// userFails answers the login probe (GET user) 503.
	userFails bool
}

// lose makes the next publish lose its answer (see losing).
func (g *forgeGate) lose(how string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.losing = how
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
	publish := r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, g.publish)
	held := g.armed && publish
	var release chan struct{}
	if held {
		g.armed = false
		close(g.entered)
		release = g.release
	}
	losing := ""
	if publish {
		losing, g.losing = g.losing, ""
	}
	deletesGone := g.deletesGone && r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/draft_notes/")
	userFails := g.userFails && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/user")
	g.mu.Unlock()
	if held {
		<-release
	}
	if userFails {
		http.Error(w, `{"message":"503 Service Unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	if deletesGone {
		http.Error(w, `{"message":"404 Draft Note Not Found"}`, http.StatusNotFound)
		return
	}
	if losing == "" {
		g.next.ServeHTTP(w, r)
		return
	}
	answer := httptest.NewRecorder()
	if losing != "unsent" {
		g.next.ServeHTTP(answer, r)
	}
	conn, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		panic(err)
	}
	defer func() { _ = conn.Close() }()
	if losing == "truncate" {
		body := answer.Body.Bytes()
		_, _ = fmt.Fprintf(buf, "HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", answer.Code, http.StatusText(answer.Code), len(body)+64)
		_, _ = buf.Write(body[:len(body)/2])
		_ = buf.Flush()
	}
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
		result, err := c.submit(c.request(threadOwned, draft))
		if err != nil || !result.Posted || result.PartialFailureKind != reviewUnmarkedKind || len(result.SentDraftIDs) != 0 {
			t.Fatalf("thread=%v: submit = %+v, %v, want posted with its drafts unmarked", threadOwned, result, err)
		}
		// Another tab's submit of the same drafts posts nothing, and says so.
		if result, err := c.submit(c.request(threadOwned, draft)); reviewSubmitCode(err) != ReviewDraftsUnmarkedCode || result.Posted {
			t.Fatalf("thread=%v: a submit while marking fails = %+v, %v, want %s", threadOwned, result, err, ReviewDraftsUnmarkedCode)
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

// An attempt's outcome answers a lost answer: posted with its drafts,
// failed with its code, or unknown for an id this process never received.
// A repeated attempt id answers the recorded outcome and posts nothing.
func TestSubmitPRReviewAttemptOutcome(t *testing.T) {
	t.Parallel()
	c := newReviewSubmitCase(t, "gitlab")
	draft := c.draft(t, false, 3, "Guard this.")
	review := c.request(false, draft)
	review.AttemptID = "attempt-1"
	first, err := c.submit(review)
	if err != nil || !first.Posted {
		t.Fatalf("submit = %+v, %v", first, err)
	}
	outcome, err := c.app.GetPRReviewSubmitOutcome(context.Background(), c.pr, "attempt-1")
	if err != nil || outcome.State != "posted" || outcome.Result == nil || !slices.Equal(outcome.Result.SentDraftIDs, []string{draft.ID}) {
		t.Fatalf("outcome = %+v, %v", outcome, err)
	}
	again, err := c.submit(review)
	if err != nil || !reflect.DeepEqual(again, first) {
		t.Fatalf("repeated attempt = %+v, %v, want the first answer %+v", again, err, first)
	}
	if n := c.published(); n != 1 {
		t.Fatalf("published %d reviews, want 1", n)
	}

	refused := c.request(false, draft)
	refused.AttemptID = "attempt-2"
	if _, err := c.submit(refused); reviewSubmitCode(err) != ReviewDraftsNotDraftsCode {
		t.Fatalf("resubmit of sent drafts = %v", err)
	}
	outcome, err = c.app.GetPRReviewSubmitOutcome(context.Background(), c.pr, "attempt-2")
	if err != nil || outcome.State != "failed" || outcome.Code != ReviewDraftsNotDraftsCode || outcome.Message == "" {
		t.Fatalf("refused outcome = %+v, %v", outcome, err)
	}

	for _, id := range []string{"attempt-never-sent", ""} {
		if outcome, err := c.app.GetPRReviewSubmitOutcome(context.Background(), c.pr, id); err != nil || outcome.State != "unknown" {
			t.Fatalf("outcome of %q = %+v, %v, want unknown", id, outcome, err)
		}
	}
	other := c.pr
	other.Number++
	if outcome, err := c.app.GetPRReviewSubmitOutcome(context.Background(), other, "attempt-1"); err != nil || outcome.State != "unknown" {
		t.Fatalf("outcome on another PR = %+v, %v, want unknown", outcome, err)
	}
}

// The outcome of an attempt still at the forge waits for it.
func TestSubmitPRReviewAttemptOutcomeWaitsForARunningAttempt(t *testing.T) {
	t.Parallel()
	c := newReviewSubmitCase(t, "github")
	draft := c.draft(t, true, 3, "Guard this.")
	review := c.request(true, draft)
	review.AttemptID = "attempt-running"
	entered, release := c.gate.hold()
	defer release()
	go func() { _, _ = c.submit(review) }()
	<-entered
	answered := make(chan PRReviewSubmitOutcome, 1)
	go func() {
		outcome, _ := c.app.GetPRReviewSubmitOutcome(context.Background(), c.pr, "attempt-running")
		answered <- outcome
	}()
	select {
	case outcome := <-answered:
		t.Fatalf("outcome %+v before the forge answered", outcome)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	if outcome := <-answered; outcome.State != "posted" || !outcome.Result.Posted {
		t.Fatalf("outcome = %+v", outcome)
	}
}

// The attempts a process remembers are bounded, and it never says an
// attempt it may have received never arrived: an attempt whose outcome
// was dropped reads forgotten, and so does every id it does not hold once
// ids have left its window too. A repeat of a forgotten id posts nothing.
func TestReviewAttemptsForgetTheOldestFinished(t *testing.T) {
	t.Parallel()
	var attempts reviewAttempts
	finish := func(i int) {
		attempt, _ := attempts.begin(fmt.Sprintf("a%d", i), "pr")
		attempts.finish(attempt, SubmitPRReviewResult{Posted: true}, nil)
	}
	for i := range reviewAttemptsKept + 1 {
		finish(i)
	}
	if kept, _ := attempts.find(fmt.Sprintf("a%d", reviewAttemptsKept), "pr"); kept == nil {
		t.Fatal("forgot the newest attempt")
	}
	if kept, forgotten := attempts.find("a0", "pr"); kept != nil || !forgotten {
		t.Fatalf("the evicted attempt = %v, forgotten %v; want forgotten", kept, forgotten)
	}
	if _, forgotten := attempts.find("never-sent", "pr"); forgotten {
		t.Fatal("an id never received reads forgotten while every id is kept")
	}
	if len(attempts.byID) != reviewAttemptsKept {
		t.Fatalf("%d outcomes kept, want %d", len(attempts.byID), reviewAttemptsKept)
	}
	repeat, repeated := attempts.begin("a0", "pr")
	if !repeated {
		t.Fatal("a forgotten id began again")
	}
	if _, err := repeat.wait(context.Background()); err == nil {
		t.Fatal("a repeat of a forgotten id answered without an error")
	}

	for i := reviewAttemptsKept + 1; i <= reviewAttemptsKept+reviewAttemptIDsKept+1; i++ {
		finish(i)
	}
	if len(attempts.forgotten) != reviewAttemptIDsKept {
		t.Fatalf("%d ids kept, want %d", len(attempts.forgotten), reviewAttemptIDsKept)
	}
	if _, forgotten := attempts.find("never-sent", "pr"); !forgotten {
		t.Fatal("an id not held reads never received after ids overflowed")
	}
}

// The outcome read of an attempt whose outcome was dropped says so, and
// one of an id the process never received says that.
func TestGetPRReviewSubmitOutcomeForgottenAndNeverReceived(t *testing.T) {
	t.Parallel()
	c := newReviewSubmitCase(t, "github")
	for i := range reviewAttemptsKept + 1 {
		attempt, _ := c.app.reviewAttempts.begin(fmt.Sprintf("a%d", i), c.pr.Key())
		c.app.reviewAttempts.finish(attempt, SubmitPRReviewResult{Posted: true}, nil)
	}
	if outcome, err := c.app.GetPRReviewSubmitOutcome(context.Background(), c.pr, "a0"); err != nil || outcome.State != "forgotten" {
		t.Fatalf("an evicted attempt = %+v, %v; want forgotten", outcome, err)
	}
	if outcome, err := c.app.GetPRReviewSubmitOutcome(context.Background(), c.pr, "never-sent"); err != nil || outcome.State != "unknown" {
		t.Fatalf("a never received id = %+v, %v; want unknown", outcome, err)
	}
}

// A GitHub review the forge took whose answer was lost or cut short is
// unconfirmed: the drafts stay drafts and are not held, the outcome says
// unconfirmed, and nothing posts again by itself.
func TestSubmitPRReviewGitHubAnswerLostIsUnconfirmed(t *testing.T) {
	t.Parallel()
	for _, how := range []string{"drop", "truncate"} {
		t.Run(how, func(t *testing.T) {
			t.Parallel()
			c := newReviewSubmitCase(t, "github")
			draft := c.draft(t, false, 3, "Guard this.")
			review := c.request(false, draft)
			review.AttemptID = "attempt-lost"
			c.gate.lose(how)
			result, err := c.submit(review)
			if err != nil || !result.Unconfirmed || result.Posted {
				t.Fatalf("submit = %+v, %v; want unconfirmed", result, err)
			}
			if got := c.comment(t, false, draft.ID).Status; got != "draft" {
				t.Fatalf("draft is %q, want draft", got)
			}
			if held := c.app.postedReviewDrafts.held(c.owner(false)); len(held) != 0 {
				t.Fatalf("held %d batches for an unconfirmed review", len(held))
			}
			listed, err := c.app.ListPRDiffReviewComments(c.pr, draft.Scope, draft.SourceKey)
			if err != nil || len(listed) != 1 || listed[0].Status != "draft" {
				t.Fatalf("a read after = %+v, %v; want the draft unmarked", listed, err)
			}
			outcome, err := c.app.GetPRReviewSubmitOutcome(context.Background(), c.pr, "attempt-lost")
			if err != nil || outcome.State != "unconfirmed" {
				t.Fatalf("outcome = %+v, %v; want unconfirmed", outcome, err)
			}
			if n := c.published(); n != 1 {
				t.Fatalf("published %d reviews, want 1", n)
			}
		})
	}
}

// A GitHub 422 is a refusal: nothing posted, the drafts stay drafts.
func TestSubmitPRReviewGitHubUnprocessableIsNotPosted(t *testing.T) {
	t.Parallel()
	c := newReviewSubmitCase(t, "github")
	draft := c.draft(t, false, 3, "Guard this.")
	if err := c.engine.SetRefusal(forgefake.Refusal{Route: publishRoute["github"], Status: http.StatusUnprocessableEntity, Times: 1}); err != nil {
		t.Fatal(err)
	}
	review := c.request(false, draft)
	review.AttemptID = "attempt-422"
	result, err := c.submit(review)
	if err == nil || result.Unconfirmed || result.Posted {
		t.Fatalf("submit = %+v, %v; want a not-posted error", result, err)
	}
	if outcome, _ := c.app.GetPRReviewSubmitOutcome(context.Background(), c.pr, "attempt-422"); outcome.State != "failed" {
		t.Fatalf("outcome = %+v, want failed", outcome)
	}
	if got := c.comment(t, false, draft.ID).Status; got != "draft" {
		t.Fatalf("draft is %q, want draft", got)
	}
}

// A review the forge refused with 401, again after the token refresh, is
// not posted when the login probe then fails: the drafts stay drafts and
// GitLab's notes are removed.
func TestSubmitPRReviewRefused401IsNotPostedWhenTheLoginCheckFails(t *testing.T) {
	t.Parallel()
	for _, forge := range []string{"github", "gitlab"} {
		t.Run(forge, func(t *testing.T) {
			t.Parallel()
			c := newReviewSubmitCase(t, forge)
			draft := c.draft(t, forge == "gitlab", 3, "Guard this.")
			if err := c.engine.SetRefusal(forgefake.Refusal{Route: publishRoute[forge], Status: http.StatusUnauthorized}); err != nil {
				t.Fatal(err)
			}
			c.gate.userFails = true
			review := c.request(forge == "gitlab", draft)
			review.AttemptID = "attempt-401"
			result, err := c.submit(review)
			if err == nil || result.Unconfirmed || result.Posted {
				t.Fatalf("submit = %+v, %v; want a not-posted error", result, err)
			}
			if forge == "gitlab" && !strings.Contains(err.Error(), "nothing was changed") {
				t.Fatalf("err = %v; want it to say the publish was refused and nothing changed", err)
			}
			if outcome, _ := c.app.GetPRReviewSubmitOutcome(context.Background(), c.pr, "attempt-401"); outcome.State != "failed" {
				t.Fatalf("outcome = %+v, want failed", outcome)
			}
			if got := c.comment(t, forge == "gitlab", draft.ID).Status; got != "draft" {
				t.Fatalf("draft is %q, want draft", got)
			}
			left := 0
			for _, inv := range c.engine.Invocations(0).Invocations {
				switch {
				case inv.Route == "glab api create draft note" && inv.Status < 300:
					left++
				case inv.Route == "glab api delete draft note" && inv.Status < 300:
					left--
				}
			}
			if left != 0 {
				t.Fatalf("%d draft notes left pending, want them removed", left)
			}
		})
	}
}

// A GitLab publish whose answer was lost is settled by the pending draft
// notes: gone means it published (posted, drafts marked, one publish);
// still pending means it never ran (not posted, the notes removed); a
// failed read leaves it unconfirmed.
func TestSubmitPRReviewGitLabLostPublishIsSettledByTheDraftNotes(t *testing.T) {
	t.Parallel()
	// The draft notes created and not removed.
	pending := func(c *reviewSubmitCase) int {
		n := 0
		for _, inv := range c.engine.Invocations(0).Invocations {
			switch {
			case inv.Route == "glab api create draft note" && inv.Status < 300:
				n++
			case inv.Route == "glab api delete draft note" && inv.Status < 300:
				n--
			}
		}
		return n
	}
	t.Run("published", func(t *testing.T) {
		t.Parallel()
		c := newReviewSubmitCase(t, "gitlab")
		draft := c.draft(t, true, 3, "Guard this.")
		c.gate.lose("drop")
		result, err := c.submit(c.request(true, draft))
		if err != nil || !result.Posted || result.Unconfirmed || !slices.Equal(result.SentDraftIDs, []string{draft.ID}) {
			t.Fatalf("submit = %+v, %v; want posted with the draft marked", result, err)
		}
		if got := c.comment(t, true, draft.ID).Status; got != "sent" {
			t.Fatalf("draft is %q, want sent", got)
		}
		if n := c.published(); n != 1 {
			t.Fatalf("published %d reviews, want 1", n)
		}
	})
	t.Run("never ran", func(t *testing.T) {
		t.Parallel()
		c := newReviewSubmitCase(t, "gitlab")
		draft := c.draft(t, true, 3, "Guard this.")
		c.gate.lose("unsent")
		result, err := c.submit(c.request(true, draft))
		if err == nil || result.Posted || result.Unconfirmed {
			t.Fatalf("submit = %+v, %v; want a not-posted error", result, err)
		}
		if got := c.comment(t, true, draft.ID).Status; got != "draft" {
			t.Fatalf("draft is %q, want draft", got)
		}
		if n := pending(c); n != 0 {
			t.Fatalf("%d draft notes left pending, want them removed", n)
		}
		if n := c.published(); n != 0 {
			t.Fatalf("published %d reviews, want 0", n)
		}
	})
	for _, how := range []string{"drop", "unsent"} {
		t.Run("read fails, "+how, func(t *testing.T) {
			t.Parallel()
			c := newReviewSubmitCase(t, "gitlab")
			draft := c.draft(t, true, 3, "Guard this.")
			if err := c.engine.SetRefusal(forgefake.Refusal{Route: "glab api list draft notes", Status: http.StatusForbidden, Times: 1}); err != nil {
				t.Fatal(err)
			}
			c.gate.lose(how)
			result, err := c.submit(c.request(true, draft))
			if err != nil || !result.Unconfirmed || result.Posted {
				t.Fatalf("submit = %+v, %v; want unconfirmed", result, err)
			}
			if got := c.comment(t, true, draft.ID).Status; got != "draft" {
				t.Fatalf("draft is %q, want draft", got)
			}
		})
	}
}

// A GitLab submit that failed before publishing and finds its draft notes
// already gone cannot say nothing posted: another publish may have taken
// them.
func TestSubmitPRReviewGitLabDraftNotesGoneBeforeRemovalIsUnconfirmed(t *testing.T) {
	t.Parallel()
	c := newReviewSubmitCase(t, "gitlab")
	draft := c.draft(t, true, 3, "Guard this.")
	if err := c.engine.SetRefusal(forgefake.Refusal{Route: publishRoute["gitlab"], Status: http.StatusForbidden, Times: 1}); err != nil {
		t.Fatal(err)
	}
	c.gate.deletesGone = true
	result, err := c.submit(c.request(true, draft))
	if err != nil || !result.Unconfirmed {
		t.Fatalf("submit = %+v, %v; want unconfirmed", result, err)
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
