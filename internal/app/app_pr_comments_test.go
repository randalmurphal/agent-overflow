package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/harness/forgefake"
	"agent-overflow/internal/store"
)

// seedTheirThread reseeds the case's pull with a review thread of another
// author's on main.go line 2, and, unless gone, nothing else.
func (c *reviewSubmitCase) seedTheirThread(t *testing.T, gone bool) {
	t.Helper()
	pull := forgefake.Pull{Number: 9, Title: "Widgets", Author: "octo", HeadRef: "feature", State: "open", Diff: reviewTestDiff}
	if !gone {
		line := 2
		pull.Threads = []forgefake.Thread{{ID: map[string]string{"github": "PRRT_theirs", "gitlab": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}[c.pr.Forge], Path: "main.go", Line: &line, Side: "right",
			Comments: []forgefake.Comment{{ID: 501, Author: "octo", Body: "theirs"}}}}
	}
	repo := forgefake.Repo{Forge: c.pr.Forge, Project: "acme/widgets", Pulls: []forgefake.Pull{pull}}
	if c.pr.Forge == "github" {
		// Below write access, which may edit and delete others' comments.
		repo.Merge = &forgefake.RepoMerge{Permission: "TRIAGE"}
	}
	if _, err := c.engine.Seed(forgefake.Fixture{Viewer: "alice", Repos: []forgefake.Repo{repo}}); err != nil {
		t.Fatal(err)
	}
}

func (c *reviewSubmitCase) threads(t *testing.T) []gitops.ReviewThread {
	t.Helper()
	threads, err := c.app.gitCore().ListReviewThreads(context.Background(), c.pr)
	if err != nil {
		t.Fatal(err)
	}
	return threads
}

func threadWithBody(threads []gitops.ReviewThread, body string) *gitops.ReviewThread {
	for i := range threads {
		for _, comment := range threads[i].Comments {
			if comment.Body == body {
				return &threads[i]
			}
		}
	}
	return nil
}

func (c *reviewSubmitCase) head(t *testing.T) string {
	t.Helper()
	read, err := c.app.gitCore().ReadPR(context.Background(), c.pr, gitops.PRReadParts{Detail: true}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return read.Detail.HeadSHA
}

// A range draft goes to the forge from its first line to its last on the
// half they share; a held reply as a reply to its thread.
func TestReviewLineCommentForDraftOfRangesAndReplies(t *testing.T) {
	t.Parallel()
	n := func(v int) *int { return &v }
	cases := []struct {
		name  string
		draft store.DiffReviewComment
		want  gitops.ReviewLineComment
	}{
		{"added range", store.DiffReviewComment{Side: "new", NewLine: 8, StartNewLine: 5},
			gitops.ReviewLineComment{Line: n(8), Side: "right", Start: &gitops.ReviewLineStart{Line: 5, Side: "right"}}},
		{"removed range", store.DiffReviewComment{Side: "old", OldLine: 9, StartOldLine: 4},
			gitops.ReviewLineComment{Line: n(9), Side: "left", Start: &gitops.ReviewLineStart{Line: 4, Side: "left"}}},
		{"from an unchanged line", store.DiffReviewComment{Side: "new", NewLine: 8, StartOldLine: 3, StartNewLine: 4},
			gitops.ReviewLineComment{Line: n(8), Side: "right", Start: &gitops.ReviewLineStart{Line: 4, OldLine: 3, Side: "right"}}},
		{"reply", store.DiffReviewComment{Side: "reply", ReplyThreadID: "PRRT_1", ReplyRootID: 7},
			gitops.ReviewLineComment{InReplyTo: &gitops.ReviewThreadReply{ThreadID: "PRRT_1", RootDatabaseID: 7}}},
	}
	for _, tc := range cases {
		tc.draft.FilePath, tc.draft.Body = "main.go", tc.name
		got, ok := reviewLineCommentForDraft(tc.draft)
		want := tc.want
		want.Path, want.Body = "main.go", tc.name
		if !ok || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: = %+v (%v), want %+v", tc.name, got, ok, want)
		}
	}
}

// Each comment action, on both forges, changes the forge, and its answer's
// sequence is that of the pump's frame that shows the change; a refusal
// is the one sentence under its kind and changes nothing; a request that
// names no line of the diff reaches no forge.
func TestPRCommentActionsSettleOnTheFrameThatShowsThem(t *testing.T) {
	t.Parallel()
	for _, forge := range []string{"github", "gitlab"} {
		t.Run(forge, func(t *testing.T) {
			t.Parallel()
			c := newReviewSubmitCase(t, forge)
			c.seedTheirThread(t, false)
			stubPRCIFetch(c.app)
			events := capturePRUpdates(t, c.app)
			ctx := context.Background()
			sub, err := c.app.SubscribePRUpdates(ctx, c.pr)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = c.app.UnsubscribePRUpdates(context.Background(), sub.ID)
				c.app.prUpdates.wg.Wait()
			})
			// frameAt is the frame with seq, the change the answer awaits.
			frameAt := func(seq uint64, why string) PRUpdatedEvent {
				t.Helper()
				for {
					frame := awaitPRUpdate(t, events, why)
					if frame.Seq == seq {
						return frame
					}
					if frame.Seq > seq {
						t.Fatalf("%s: frame %d passed the answer's %d", why, frame.Seq, seq)
					}
				}
			}
			settled := func(why string, act func() (PRActionResult, error)) []gitops.ReviewThread {
				t.Helper()
				result, err := act()
				if err != nil || result.Seq == 0 {
					t.Fatalf("%s = %+v, %v", why, result, err)
				}
				return frameAt(result.Seq, why).Threads
			}
			head := c.head(t)

			threads := settled("range comment", func() (PRActionResult, error) {
				return c.app.PostPRLineComment(ctx, c.pr, PRLineCommentRequest{HeadSHA: head, FilePath: "main.go", Side: "new", NewLine: 8, StartNewLine: 5, Body: "a range"})
			})
			if got := threadWithBody(threads, "a range"); got == nil || *got.Line != 8 || got.StartLine == nil || *got.StartLine != 5 || got.Side != "right" {
				t.Fatalf("range thread = %+v", got)
			}
			threads = settled("PR comment", func() (PRActionResult, error) { return c.app.PostPRComment(ctx, c.pr, "on the PR") })
			if got := threadWithBody(threads, "on the PR"); got == nil || got.Path != "" {
				t.Fatalf("PR comment = %+v", got)
			}
			theirs := threadWithBody(threads, "theirs")
			threads = settled("reply", func() (PRActionResult, error) {
				return c.app.ReplyToPRThread(ctx, c.pr, theirs.ID, theirs.Comments[0].DatabaseID, "a reply")
			})
			if got := threadWithBody(threads, "theirs"); len(got.Comments) != 2 || got.Comments[1].Body != "a reply" {
				t.Fatalf("thread after the reply = %+v", got)
			}
			mine := threadWithBody(threads, "a range")
			target := gitops.CommentTarget{ThreadID: mine.ID, CommentID: mine.Comments[0].ID}
			threads = settled("edit", func() (PRActionResult, error) { return c.app.EditPRComment(ctx, c.pr, target, "an edited range") })
			if threadWithBody(threads, "an edited range") == nil {
				t.Fatalf("threads after the edit = %+v", threads)
			}
			threads = settled("delete", func() (PRActionResult, error) { return c.app.DeletePRComment(ctx, c.pr, target) })
			if threadWithBody(threads, "an edited range") != nil {
				t.Fatalf("the deleted thread is in the frame: %+v", threads)
			}

			// Refusals: another author's comment, a moved head.
			theirTarget := gitops.CommentTarget{ThreadID: theirs.ID, CommentID: theirs.Comments[0].ID}
			result, err := c.app.EditPRComment(ctx, c.pr, theirTarget, "mine now")
			var explained *gitops.ForgeActionError
			if !errors.As(err, &explained) || explained.Message == "" || result.Seq != 0 {
				t.Fatalf("edit of another author's comment = %+v, %v", result, err)
			}
			_, err = c.app.PostPRLineComment(ctx, c.pr, PRLineCommentRequest{HeadSHA: "0000000000000000000000000000000000000000", FilePath: "main.go", Side: "new", NewLine: 3, Body: "stale"})
			if !errors.As(err, &explained) {
				t.Fatalf("a comment on a moved head = %v", err)
			}
			if forge == "gitlab" && explained.Kind != gitops.ForgeErrorStaleHead {
				t.Fatalf("GitLab's moved head = %+v", explained)
			}
			before := len(c.engine.Invocations(0).Invocations)
			for _, bad := range []PRLineCommentRequest{
				{HeadSHA: head, FilePath: "main.go", Side: "new", Body: "no line"},
				{HeadSHA: head, FilePath: "main.go", Side: "new", NewLine: 3, StartNewLine: 3, Body: "empty range"},
				{HeadSHA: head, FilePath: "main.go", Side: "reply", Body: "a reply is not a line"},
			} {
				if _, err := c.app.PostPRLineComment(ctx, c.pr, bad); err == nil {
					t.Fatalf("%q was posted", bad.Body)
				}
			}
			if after := len(c.engine.Invocations(0).Invocations); after != before {
				t.Fatalf("invalid anchors made %d forge calls", after-before)
			}
			if got := threadWithBody(c.threads(t), "theirs"); got.Comments[0].Body != "theirs" || threadWithBody(c.threads(t), "stale") != nil {
				t.Fatalf("a refusal changed the forge: %+v", got)
			}
		})
	}
}

// A held reply is one per thread for an owner and source, in both draft
// families; a submit posts it into its thread with the review's line
// comments and marks it sent.
func TestSubmitPRReviewPostsHeldRepliesAndRanges(t *testing.T) {
	t.Parallel()
	for _, forge := range []string{"github", "gitlab"} {
		for _, threadOwned := range []bool{true, false} {
			c := newReviewSubmitCase(t, forge)
			c.seedTheirThread(t, false)
			theirs := threadWithBody(c.threads(t), "theirs")
			reply := c.replyDraft(t, threadOwned, theirs, "held reply")
			if _, err := c.createDraft(threadOwned, store.DiffReviewCommentInput{Side: "reply", FilePath: "main.go", ReplyThreadID: theirs.ID, Body: "a second"}); reviewSubmitCode(err) != ReviewReplyExistsCode {
				t.Fatalf("%s thread=%v: a second held reply = %v", forge, threadOwned, err)
			}
			ranged, err := c.createDraft(threadOwned, store.DiffReviewCommentInput{FilePath: "main.go", Side: "new", NewLine: 8, StartNewLine: 5, Body: "a range"})
			if err != nil {
				t.Fatal(err)
			}
			result, err := c.submit(c.request(threadOwned, reply, ranged))
			if err != nil || !result.Posted || !slices.Contains(result.SentDraftIDs, reply.ID) || !slices.Contains(result.SentDraftIDs, ranged.ID) {
				t.Fatalf("%s thread=%v: submit = %+v, %v", forge, threadOwned, result, err)
			}
			if forge == "github" && result.PostedReplies != 1 {
				t.Fatalf("github: posted replies = %d", result.PostedReplies)
			}
			threads := c.threads(t)
			if got := threadWithBody(threads, "theirs"); len(got.Comments) != 2 || got.Comments[1].Body != "held reply" {
				t.Fatalf("%s thread=%v: thread = %+v", forge, threadOwned, got)
			}
			if got := threadWithBody(threads, "a range"); got == nil || got.StartLine == nil || *got.StartLine != 5 || *got.Line != 8 {
				t.Fatalf("%s thread=%v: range = %+v", forge, threadOwned, got)
			}
			if got := c.comment(t, threadOwned, reply.ID).Status; got != "sent" {
				t.Fatalf("%s thread=%v: reply draft is %q", forge, threadOwned, got)
			}
		}
	}
}

// A held reply whose thread was deleted is reported, not dropped: on
// GitHub the review lands and the reply stays a draft with its thread
// named; on GitLab nothing posts.
func TestSubmitPRReviewReportsAHeldReplyWhoseThreadIsGone(t *testing.T) {
	t.Parallel()
	for _, forge := range []string{"github", "gitlab"} {
		c := newReviewSubmitCase(t, forge)
		c.seedTheirThread(t, false)
		theirs := threadWithBody(c.threads(t), "theirs")
		reply := c.replyDraft(t, false, theirs, "held reply")
		line := c.draft(t, false, 3, "a line")
		c.seedTheirThread(t, true)
		review := c.request(false, line, reply)
		review.AttemptID = "attempt-gone-" + forge
		result, err := c.submit(review)
		switch forge {
		case "github":
			if err != nil || !result.PostedReview || result.PartialFailureThreadID != theirs.ID || result.PartialFailureKind != gitops.ForgeErrorNotFound || !strings.Contains(result.PartialFailure, "reply answers is no longer on") {
				t.Fatalf("github: submit = %+v, %v", result, err)
			}
			if !slices.Equal(result.SentDraftIDs, []string{line.ID}) {
				t.Fatalf("github: sent %v, want the line alone", result.SentDraftIDs)
			}
		case "gitlab":
			var explained *gitops.ForgeActionError
			if !errors.As(err, &explained) || explained.Kind != gitops.ForgeErrorNotFound || !strings.Contains(explained.Message, "reply answers is no longer on") || result.Posted {
				t.Fatalf("gitlab: submit = %+v, %v", result, err)
			}
			if c.published() != 0 {
				t.Fatal("gitlab: a review published")
			}
		}
		if got := c.comment(t, false, reply.ID).Status; got != "draft" {
			t.Fatalf("%s: the reply draft is %q, want draft", forge, got)
		}
	}
}

func (c *reviewSubmitCase) createDraft(threadOwned bool, input store.DiffReviewCommentInput) (store.DiffReviewComment, error) {
	input.Scope, input.SourceKey = string(store.DiffReviewScopePR), prDraftSource(c.pr)
	if threadOwned {
		return c.app.CreateDiffReviewComment(c.thread.ID, input)
	}
	return c.app.CreatePRDiffReviewComment(c.pr, input)
}

func (c *reviewSubmitCase) replyDraft(t *testing.T, threadOwned bool, thread *gitops.ReviewThread, body string) store.DiffReviewComment {
	t.Helper()
	created, err := c.createDraft(threadOwned, store.DiffReviewCommentInput{Side: "reply", FilePath: thread.Path, ReplyThreadID: thread.ID, ReplyRootID: thread.Comments[0].DatabaseID, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	return created
}

// A comment posted now whose answer was lost may be on the forge: it is
// its own failure (kind unconfirmed, a sentence that says check before
// sending again), never the transient "try again", and the pump polls so
// the pane shows what the forge holds. Nothing reposts it.
func TestPRCommentCreatesWithALostAnswerAreUnconfirmed(t *testing.T) {
	t.Parallel()
	for _, forge := range []string{"github", "gitlab"} {
		t.Run(forge, func(t *testing.T) {
			t.Parallel()
			c := newReviewSubmitCase(t, forge)
			c.seedTheirThread(t, false)
			stubPRCIFetch(c.app)
			events := capturePRUpdates(t, c.app)
			ctx := context.Background()
			sub, err := c.app.SubscribePRUpdates(ctx, c.pr)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = c.app.UnsubscribePRUpdates(context.Background(), sub.ID)
				c.app.prUpdates.wg.Wait()
			})
			head := c.head(t)
			theirs := threadWithBody(c.threads(t), "theirs")
			suffix := map[string][3]string{
				"github": {"/pulls/9/comments", "/issues/9/comments", "/replies"},
				"gitlab": {"/discussions", "/notes", "/notes"},
			}[forge]
			for i, create := range []struct {
				body string
				post func() (PRActionResult, error)
			}{
				{"lost line", func() (PRActionResult, error) {
					return c.app.PostPRLineComment(ctx, c.pr, PRLineCommentRequest{HeadSHA: head, FilePath: "main.go", Side: "new", NewLine: 3, Body: "lost line"})
				}},
				{"lost note", func() (PRActionResult, error) { return c.app.PostPRComment(ctx, c.pr, "lost note") }},
				{"lost reply", func() (PRActionResult, error) {
					return c.app.ReplyToPRThread(ctx, c.pr, theirs.ID, theirs.Comments[0].DatabaseID, "lost reply")
				}},
			} {
				c.gate.mu.Lock()
				c.gate.publish = suffix[i]
				c.gate.mu.Unlock()
				c.gate.lose("drop")
				result, err := create.post()
				explained, ok := errors.AsType[*gitops.ForgeActionError](err)
				if !ok || explained.Kind != gitops.ForgeErrorUnconfirmed || !strings.Contains(explained.Message, "may have posted this comment. Check the thread before sending it again.") || result.Seq != 0 {
					t.Fatalf("%s = %+v, %v", create.body, result, err)
				}
				for deadline := time.Now().Add(5 * time.Second); ; {
					if frame := awaitPRUpdate(t, events, create.body); threadWithBody(frame.Threads, create.body) != nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("%s: no frame shows the comment the forge holds", create.body)
					}
				}
			}
			if n := strings.Count(strings.Join(bodiesOf(c.threads(t)), "\n"), "lost"); n != 3 {
				t.Fatalf("the forge holds %d lost comments, want each once", n)
			}
		})
	}
}

func bodiesOf(threads []gitops.ReviewThread) []string {
	var out []string
	for _, thread := range threads {
		for _, comment := range thread.Comments {
			out = append(out, comment.Body)
		}
	}
	return out
}

// A GitLab held reply leaves its discussion resolved or open as it was,
// and one whose discussion goes while the submit is at the forge (GitLab
// publishes the rest and drops it) is reported, not marked sent.
func TestSubmitPRReviewGitLabHeldRepliesKeepTheirDiscussion(t *testing.T) {
	t.Parallel()
	c := newReviewSubmitCase(t, "gitlab")
	line := 2
	seed := func(threads ...forgefake.Thread) {
		t.Helper()
		pull := forgefake.Pull{Number: 9, Title: "Widgets", Author: "octo", HeadRef: "feature", State: "open", Threads: threads, Diff: reviewTestDiff}
		if _, err := c.engine.Seed(forgefake.Fixture{Viewer: "alice", Repos: []forgefake.Repo{{Forge: "gitlab", Project: "acme/widgets", Pulls: []forgefake.Pull{pull}}}}); err != nil {
			t.Fatal(err)
		}
	}
	resolved := forgefake.Thread{ID: strings.Repeat("a", 40), Path: "main.go", Line: &line, Side: "right", Resolved: true, Comments: []forgefake.Comment{{ID: 501, Author: "octo", Body: "resolved root"}}}
	open := forgefake.Thread{ID: strings.Repeat("b", 40), Path: "main.go", Line: &line, Side: "right", Comments: []forgefake.Comment{{ID: 502, Author: "alice", Body: "open root"}}}
	seed(resolved, open)
	threads := c.threads(t)
	toResolved := c.replyDraft(t, false, threadWithBody(threads, "resolved root"), "into the resolved one")
	toOpen := c.replyDraft(t, false, threadWithBody(threads, "open root"), "into the open one")
	if _, err := c.submit(c.request(false, toResolved, toOpen)); err != nil {
		t.Fatal(err)
	}
	threads = c.threads(t)
	if got := threadWithBody(threads, "resolved root"); !got.IsResolved || len(got.Comments) != 2 {
		t.Fatalf("resolved discussion after a reply = %+v", got)
	}
	if got := threadWithBody(threads, "open root"); got.IsResolved || len(got.Comments) != 2 {
		t.Fatalf("open discussion after a reply = %+v", got)
	}

	// The open discussion is deleted while the publish is held.
	seed(resolved, open)
	threads = c.threads(t)
	lost := c.replyDraft(t, false, threadWithBody(threads, "open root"), "lost reply")
	kept := c.draft(t, false, 3, "a line")
	entered, release := c.gate.hold()
	type answer struct {
		result SubmitPRReviewResult
		err    error
	}
	done := make(chan answer, 1)
	go func() {
		result, err := c.submit(c.request(false, kept, lost))
		done <- answer{result, err}
	}()
	<-entered
	// The viewer deletes the discussion's only note on GitLab meanwhile.
	deleteNote := httptest.NewRequest(http.MethodDelete, "/gitlab/api/v4/projects/acme%2Fwidgets/merge_requests/9/discussions/"+open.ID+"/notes/502", nil)
	deleteNote.Host = "gitlab.com"
	deleteNote.Header.Set("Private-Token", mergeForgeToken)
	recorder := httptest.NewRecorder()
	c.engine.ServeHTTP(recorder, deleteNote)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("delete the discussion = %d %s", recorder.Code, recorder.Body)
	}
	release()
	got := <-done
	if got.err != nil || !got.result.PostedReview || got.result.PartialFailureThreadID != open.ID || got.result.PartialFailureKind != gitops.ForgeErrorNotFound ||
		!slices.Equal(got.result.SentDraftIDs, []string{kept.ID}) {
		t.Fatalf("submit with a dropped reply = %+v, %v", got.result, got.err)
	}
	if status := c.comment(t, false, lost.ID).Status; status != "draft" {
		t.Fatalf("the dropped reply is %q, want draft", status)
	}
}
