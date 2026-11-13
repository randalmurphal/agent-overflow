package git

import (
	"crypto/sha1"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/forgeapi"
)

// A comment now on GitHub is one REST review comment pinned to the head
// the caller saw: a line, a range with its start, or a file.
func TestGitHubPostLineCommentShapes(t *testing.T) {
	t.Parallel()
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if call.Method == http.MethodPost && call.Path == "repos/o/r/pulls/5/comments" {
			return forgeAPIAnswer{Status: http.StatusCreated, Body: `{"id":9}`}
		}
		return forgeUnexpected(t, call)
	})
	line, old := 18, 17
	comments := []ReviewLineComment{
		{Path: "a.go", Body: "one", Line: &line, Side: "right"},
		{Path: "a.go", Body: "range", Line: &line, OldLine: &old, Side: "right", Start: &ReviewLineStart{Line: 12, Side: "right"}},
		{Path: "a.go", Body: "left range", Line: &line, Side: "left", Start: &ReviewLineStart{Line: 15, Side: "left"}},
		{Path: "b.go", Body: "file", Side: "file"},
	}
	for _, comment := range comments {
		if err := core.PostPRLineComment(t.Context(), githubTestRef(5), "abc1234", comment); err != nil {
			t.Fatalf("%s: %v", comment.Body, err)
		}
	}
	want := []string{
		`{"body":"one","commit_id":"abc1234","path":"a.go","line":18,"side":"RIGHT"}`,
		`{"body":"range","commit_id":"abc1234","path":"a.go","line":18,"side":"RIGHT","start_line":12,"start_side":"RIGHT"}`,
		`{"body":"left range","commit_id":"abc1234","path":"a.go","line":18,"side":"LEFT","start_line":15,"start_side":"LEFT"}`,
		`{"body":"file","commit_id":"abc1234","path":"b.go","subject_type":"file"}`,
	}
	sent := calls.all()
	if len(sent) != len(want) {
		t.Fatalf("requests = %v", calls.ops())
	}
	for i := range want {
		if string(sent[i].Body) != want[i] {
			t.Fatalf("comment %d = %s, want %s", i, sent[i].Body, want[i])
		}
	}
	for _, bad := range []struct {
		head    string
		comment ReviewLineComment
	}{
		{"", comments[0]},
		{"abc1234", ReviewLineComment{Path: "a.go", Body: " ", Line: &line, Side: "right"}},
		{"abc1234", ReviewLineComment{Path: "a.go", Body: "x", InReplyTo: &ReviewThreadReply{ThreadID: "t", RootDatabaseID: 1}}},
	} {
		if err := core.PostPRLineComment(t.Context(), githubTestRef(5), bad.head, bad.comment); err == nil {
			t.Fatalf("PostPRLineComment(%q, %+v) was sent", bad.head, bad.comment)
		}
	}
	if n := len(calls.all()); n != len(want) {
		t.Fatalf("a refused comment reached the forge: %v", calls.ops())
	}
}

func TestGitHubPostPRComment(t *testing.T) {
	t.Parallel()
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if call.Method == http.MethodPost && call.Path == "repos/o/r/issues/5/comments" {
			return forgeAPIAnswer{Status: http.StatusCreated, Body: `{"id":9,"node_id":"IC_9"}`}
		}
		return forgeUnexpected(t, call)
	})
	// The created comment is named as the read names it: its node id.
	if id, err := core.PostPRComment(t.Context(), githubTestRef(5), "looks good"); err != nil || id != "IC_9" {
		t.Fatalf("PostPRComment = %q, %v; want IC_9", id, err)
	}
	if sent := calls.all(); len(sent) != 1 || string(sent[0].Body) != `{"body":"looks good"}` {
		t.Fatalf("requests = %v", calls.ops())
	}
	if _, err := core.PostPRComment(t.Context(), githubTestRef(5), "  "); err == nil {
		t.Fatal("an empty PR comment was sent")
	}
}

// An edit or delete runs the mutation of the comment's kind: a review
// thread comment's, or a conversation comment's, whose thread is itself.
// An answer that names no comment is not an edit.
func TestGitHubEditAndDeleteComment(t *testing.T) {
	t.Parallel()
	var answer, owner atomic.Value
	answer.Store("")
	owner.Store("o/r#5")
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if call.Op == "CommentOwner" {
			repo, number, _ := strings.Cut(owner.Load().(string), "#")
			typename := "PullRequestReviewComment"
			if strings.HasPrefix(fmt.Sprint(call.Vars["id"]), "IC_") {
				typename = "IssueComment"
			}
			return githubData(fmt.Sprintf(`{"node":{"__typename":%q,"pullRequest":{"number":%s,"repository":{"nameWithOwner":%q}}}}`, typename, number, repo))
		}
		if body := answer.Load().(string); body != "" {
			return forgeAPIAnswer{Body: body}
		}
		switch call.Op {
		case "UpdateReviewComment":
			return githubData(fmt.Sprintf(`{"updatePullRequestReviewComment":{"pullRequestReviewComment":{"id":%q}}}`, call.Vars["id"]))
		case "UpdateIssueComment":
			return githubData(fmt.Sprintf(`{"updateIssueComment":{"issueComment":{"id":%q}}}`, call.Vars["id"]))
		case "DeleteReviewComment":
			return githubData(`{"deletePullRequestReviewComment":{"clientMutationId":null}}`)
		case "DeleteIssueComment":
			return githubData(`{"deleteIssueComment":{"clientMutationId":null}}`)
		}
		return forgeUnexpected(t, call)
	})
	review := CommentTarget{ThreadID: "PRRT_1", CommentID: "PRRC_2"}
	conversation := CommentTarget{ThreadID: "IC_3", CommentID: "IC_3"}
	ref := githubTestRef(5)
	for _, step := range []func() error{
		func() error { return core.EditPRComment(t.Context(), ref, review, "new") },
		func() error { return core.EditPRComment(t.Context(), ref, conversation, "new") },
		func() error { return core.DeletePRComment(t.Context(), ref, review) },
		func() error { return core.DeletePRComment(t.Context(), ref, conversation) },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(calls.ops(), ","); got != "CommentOwner,UpdateReviewComment,CommentOwner,UpdateIssueComment,CommentOwner,DeleteReviewComment,CommentOwner,DeleteIssueComment" {
		t.Fatalf("operations = %s", got)
	}
	if sent := calls.all(); sent[1].Vars["id"] != "PRRC_2" || sent[1].Vars["body"] != "new" || sent[7].Vars["id"] != "IC_3" {
		t.Fatalf("variables = %v / %v", sent[1].Vars, sent[7].Vars)
	}
	// A comment of another PR, of another repository, or of the other kind
	// than the target names is refused before any mutation.
	for _, elsewhere := range []struct {
		owner  string
		target CommentTarget
	}{{"o/r#6", review}, {"o/other#5", conversation}, {"o/r#5", CommentTarget{ThreadID: "PRRT_9", CommentID: "IC_9"}}} {
		owner.Store(elsewhere.owner)
		before := len(calls.all())
		for _, write := range []func() error{
			func() error { return core.EditPRComment(t.Context(), ref, elsewhere.target, "new") },
			func() error { return core.DeletePRComment(t.Context(), ref, elsewhere.target) },
		} {
			err := write()
			if _, ok := errors.AsType[*CommentNotOnPRError](err); !ok {
				t.Fatalf("%s: a comment elsewhere = %v", elsewhere.owner, err)
			}
			if explained, _ := errors.AsType[*ForgeActionError](ExplainForgeError(ref, ActionEdit, err)); explained == nil || explained.Kind != ForgeErrorNotFound {
				t.Fatalf("%s: explained = %v", elsewhere.owner, explained)
			}
		}
		if got := calls.ops()[before:]; strings.Join(got, ",") != "CommentOwner,CommentOwner" {
			t.Fatalf("%s: a comment elsewhere was written: %v", elsewhere.owner, got)
		}
	}
	owner.Store("o/r#5")
	answer.Store(`{"data":{"updatePullRequestReviewComment":null}}`)
	if err := core.EditPRComment(t.Context(), ref, review, "new"); err == nil {
		t.Fatal("an edit GitHub answered with no comment read as done")
	}
	answer.Store(`{"data":{"deletePullRequestReviewComment":null},"errors":[{"type":"FORBIDDEN","message":"v does not have the correct permissions to execute ` + "`DeletePullRequestReviewComment`" + `"}]}`)
	err := ExplainForgeError(ref, ActionDelete, core.DeletePRComment(t.Context(), ref, review))
	if explained, ok := errors.AsType[*ForgeActionError](err); !ok || explained.Kind != ForgeErrorRefused || !strings.Contains(explained.Message, "did not delete a comment on o/r#5") {
		t.Fatalf("refused delete = %v", err)
	}
	before := len(calls.all())
	if err := core.EditPRComment(t.Context(), ref, CommentTarget{ThreadID: "PRRT_1"}, "new"); err == nil {
		t.Fatal("an edit naming no comment was sent")
	}
	if err := core.EditPRComment(t.Context(), ref, review, " "); err == nil {
		t.Fatal("an empty edit was sent")
	}
	if len(calls.all()) != before {
		t.Fatalf("a refused edit reached the forge: %v", calls.ops())
	}
}

// Held replies post after the review, one by one; a request with nothing
// for the review itself posts no review, and a reply whose thread is gone
// is a partial submit that names it.
func TestGitHubSubmitReviewPostsHeldReplies(t *testing.T) {
	t.Parallel()
	var goneRoot atomic.Int64
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch {
		case call.Method == http.MethodPost && call.Path == "repos/o/r/pulls/5/reviews":
			return forgeAPIAnswer{Body: `{"id":1}`}
		case call.Method == http.MethodPost && strings.HasPrefix(call.Path, "repos/o/r/pulls/5/comments/"):
			if call.Path == fmt.Sprintf("repos/o/r/pulls/5/comments/%d/replies", goneRoot.Load()) {
				return forgeAPIAnswer{Status: http.StatusNotFound, Body: `{"message":"Not Found"}`}
			}
			return forgeAPIAnswer{Status: http.StatusCreated, Body: `{"id":2}`}
		}
		return forgeUnexpected(t, call)
	})
	line := 3
	replies := []ReviewLineComment{
		{Body: "first", InReplyTo: &ReviewThreadReply{ThreadID: "PRRT_a", RootDatabaseID: 11}},
		{Body: "second", InReplyTo: &ReviewThreadReply{ThreadID: "PRRT_b", RootDatabaseID: 12}},
	}
	result, err := core.SubmitReview(t.Context(), githubTestRef(5), SubmitReviewRequest{Verdict: ReviewVerdictComment, Body: "summary", Comments: append([]ReviewLineComment{{Path: "a.go", Body: "x", Line: &line, Side: "right"}}, replies...)})
	if err != nil || !result.PostedReview || result.PostedReplies != 2 {
		t.Fatalf("SubmitReview = %+v, %v", result, err)
	}
	if got := strings.Join(calls.ops(), ","); got != "POST repos/o/r/pulls/5/reviews,POST repos/o/r/pulls/5/comments/11/replies,POST repos/o/r/pulls/5/comments/12/replies" {
		t.Fatalf("requests = %s", got)
	}
	if body := string(calls.all()[1].Body); body != `{"body":"first"}` {
		t.Fatalf("reply body = %s", body)
	}

	seen := len(calls.all())
	result, err = core.SubmitReview(t.Context(), githubTestRef(5), SubmitReviewRequest{Verdict: ReviewVerdictComment, Comments: replies})
	if err != nil || result.PostedReview || result.PostedReplies != 2 {
		t.Fatalf("replies alone = %+v, %v", result, err)
	}
	if got := calls.ops()[seen:]; len(got) != 2 || strings.Contains(strings.Join(got, ","), "reviews") {
		t.Fatalf("replies alone sent %v, want the two replies and no review", got)
	}

	goneRoot.Store(12)
	_, err = core.SubmitReview(t.Context(), githubTestRef(5), SubmitReviewRequest{Verdict: ReviewVerdictComment, Comments: replies})
	partial, ok := errors.AsType[*PartialSubmitError](err)
	if !ok || partial.PostedReview || partial.PostedReplies != 1 || partial.FailedThreadID != "PRRT_b" || partial.Unconfirmed {
		t.Fatalf("gone thread = %#v", err)
	}
	if _, ok := errors.AsType[*ReplyThreadGoneError](err); !ok {
		t.Fatalf("gone thread lost its kind: %v", err)
	}

	goneRoot.Store(11)
	_, err = core.SubmitReview(t.Context(), githubTestRef(5), SubmitReviewRequest{Verdict: ReviewVerdictComment, Comments: replies})
	if _, partial := errors.AsType[*PartialSubmitError](err); partial || !errors.Is(err, forgeapi.ErrNotFound) {
		t.Fatalf("a first reply refused with nothing posted = %v, want the refusal itself", err)
	}
	explained, _ := errors.AsType[*ForgeActionError](ExplainForgeError(githubTestRef(5), ActionReview, err))
	if explained == nil || explained.Kind != ForgeErrorNotFound || !strings.Contains(explained.Message, "thread this reply answers") {
		t.Fatalf("gone thread sentence = %+v", explained)
	}
}

// The read carries what an edit, a delete and the "edited" mark need, for
// review thread comments and conversation comments alike.
func TestGitHubReadCarriesCommentPermissions(t *testing.T) {
	t.Parallel()
	core, _ := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if call.Op != "PRTick" {
			return forgeUnexpected(t, call)
		}
		if !strings.Contains(call.Query, "lastEditedAt url viewerCanUpdate viewerCanDelete") {
			t.Errorf("PRTick does not select the comment permissions:\n%s", call.Query)
		}
		return githubData(`{"repository":{"pullRequest":{"number":5,
			"reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PRRT_1","isResolved":false,"isOutdated":false,"path":"a.go","line":3,"startLine":null,"diffSide":"RIGHT","subjectType":"LINE",
				"comments":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PRRC_2","databaseId":2,"author":{"login":"me"},"body":"b","createdAt":"t","lastEditedAt":"t2","url":"https://github.com/o/r/pull/5#discussion_r2","viewerCanUpdate":true,"viewerCanDelete":true,"replyTo":null}]}}]},
			"comments":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"IC_3","databaseId":3,"author":{"login":"other"},"body":"c","createdAt":"t","lastEditedAt":null,"url":"https://github.com/o/r/pull/5#issuecomment-3","viewerCanUpdate":false,"viewerCanDelete":true,"isMinimized":false}]}}}}`)
	})
	threads, err := core.ListReviewThreads(t.Context(), githubTestRef(5))
	if err != nil || len(threads) != 2 {
		t.Fatalf("threads = %+v, %v", threads, err)
	}
	review, conversation := threads[0].Comments[0], threads[1].Comments[0]
	if review.ID != "PRRC_2" || !review.ViewerCanEdit || !review.ViewerCanDelete || review.EditedAt != "t2" || review.URL != "https://github.com/o/r/pull/5#discussion_r2" {
		t.Fatalf("review comment = %+v", review)
	}
	if conversation.ID != "IC_3" || threads[1].ID != "IC_3" || conversation.ViewerCanEdit || !conversation.ViewerCanDelete || conversation.EditedAt != "" {
		t.Fatalf("conversation comment = %+v in %s", conversation, threads[1].ID)
	}
}

// A comment now on GitLab is a discussion positioned on the merge
// request's current diff, refused before any write once the head moved
// past the one the caller saw.
func TestGitLabPostLineComment(t *testing.T) {
	t.Parallel()
	mr := gitlabTestProject + "/merge_requests/7"
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch {
		case call.Method == http.MethodGet && call.Path == mr:
			return forgeAPIAnswer{Body: strings.ReplaceAll(gitlabTestMR(7, 0), "head2", "abc1234")}
		case call.Method == http.MethodGet && call.Path == mr+"/diffs?per_page=100":
			return gitlabTestDiffsAnswer()
		case call.Method == http.MethodPost && call.Path == mr+"/discussions":
			return forgeAPIAnswer{Status: http.StatusCreated, Body: `{"id":"d1"}`}
		case call.Method == http.MethodPost && call.Path == mr+"/notes":
			return forgeAPIAnswer{Status: http.StatusCreated, Body: `{"id":5}`}
		}
		return forgeUnexpected(t, call)
	})
	end := 6
	err := core.PostPRLineComment(t.Context(), gitlabTestRef(7), "abc1234", ReviewLineComment{Path: "a.go", Body: "range", Line: &end, Side: "right", Start: &ReviewLineStart{Line: 4, OldLine: 3, Side: "right"}})
	if err != nil {
		t.Fatal(err)
	}
	code := fmt.Sprintf("%x", sha1.Sum([]byte("a.go")))
	want := `{"body":"range","position":{"base_sha":"base","start_sha":"start","head_sha":"abc1234","old_path":"a.go","new_path":"a.go","position_type":"text","new_line":6,` +
		`"line_range":{"start":{"line_code":"` + code + `_3_4","old_line":3,"new_line":4},"end":{"line_code":"` + code + `_4_6","type":"new","new_line":6}}}}`
	if sent := calls.all(); len(sent) != 3 || string(sent[2].Body) != want {
		t.Fatalf("requests = %v, body = %s\nwant %s", calls.ops(), sent[len(sent)-1].Body, want)
	}
	err = core.PostPRLineComment(t.Context(), gitlabTestRef(7), "abc0000", ReviewLineComment{Path: "a.go", Body: "x", Line: &end, Side: "right"})
	if _, ok := errors.AsType[*HeadMovedError](err); !ok {
		t.Fatalf("moved head = %v", err)
	}
	if explained, _ := errors.AsType[*ForgeActionError](ExplainForgeError(gitlabTestRef(7), ActionComment, err)); explained == nil || explained.Kind != ForgeErrorStaleHead {
		t.Fatalf("moved head sentence = %+v", explained)
	}
	if got := calls.ops(); len(got) != 4 || got[3] != "GET "+mr {
		t.Fatalf("a comment on a moved head was posted: %v", got)
	}
	// The created note is named as the read names it: its id.
	if id, err := core.PostPRComment(t.Context(), gitlabTestRef(7), "overall"); err != nil || id != "5" {
		t.Fatalf("PostPRComment = %q, %v; want 5", id, err)
	}
	if sent := calls.all(); string(sent[len(sent)-1].Body) != `{"body":"overall"}` {
		t.Fatalf("note body = %s", sent[len(sent)-1].Body)
	}
}

func TestGitLabEditAndDeleteComment(t *testing.T) {
	t.Parallel()
	note := gitlabTestProject + "/merge_requests/7/discussions/d1/notes/42"
	var answered atomic.Int64
	answered.Store(42)
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch {
		case call.Method == http.MethodPut && call.Path == note:
			return forgeAPIAnswer{Body: fmt.Sprintf(`{"id":%d,"body":"new"}`, answered.Load())}
		case call.Method == http.MethodDelete && call.Path == note:
			return forgeAPIAnswer{Status: http.StatusNoContent}
		}
		return forgeUnexpected(t, call)
	})
	target := CommentTarget{ThreadID: "d1", CommentID: "42"}
	if err := core.EditPRComment(t.Context(), gitlabTestRef(7), target, "new"); err != nil {
		t.Fatal(err)
	}
	if err := core.DeletePRComment(t.Context(), gitlabTestRef(7), target); err != nil {
		t.Fatal(err)
	}
	if sent := calls.all(); len(sent) != 2 || string(sent[0].Body) != `{"body":"new"}` {
		t.Fatalf("requests = %v", calls.ops())
	}
	answered.Store(41)
	if err := core.EditPRComment(t.Context(), gitlabTestRef(7), target, "new"); err == nil {
		t.Fatal("an edit answered with another note read as done")
	}
	if err := core.EditPRComment(t.Context(), gitlabTestRef(7), CommentTarget{ThreadID: "d1", CommentID: "x/../1"}, "new"); err == nil {
		t.Fatal("a note id that is not a number was sent")
	}
}

// A GitLab held reply is a draft note in its discussion that keeps the
// discussion resolved or open; its discussion is read before (gone: no
// draft is created) and after the publish (a reply GitLab dropped is a
// partial submit naming it). A 400 on the draft is the discussion gone
// only when the discussion's own read says so.
func TestGitLabSubmitReviewHeldReply(t *testing.T) {
	t.Parallel()
	mr := gitlabTestProject + "/merge_requests/7"
	var state atomic.Value // "ok", "gone-before", "refused-gone", "refused-other", "dropped"
	state.Store("ok")
	published := atomic.Bool{}
	discussions := func() string {
		replies := ""
		if published.Load() && state.Load() != "dropped" {
			replies = `,{"id":12,"body":"held","author":{"username":"me"}}`
		}
		open := `{"id":"d9","notes":[{"id":11,"body":"root","author":{"username":"rev"},"resolvable":true,"resolved":true}` + replies + `]}`
		other := `{"id":"d8","notes":[{"id":21,"body":"open root","author":{"username":"rev"},"resolvable":true,"resolved":false}]}`
		if state.Load() == "gone-before" {
			return "[" + other + "]"
		}
		return "[" + open + "," + other + "]"
	}
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch {
		case call.Method == http.MethodGet && call.Path == mr:
			return forgeAPIAnswer{Body: gitlabTestMR(7, 0)}
		case call.Method == http.MethodGet && strings.HasPrefix(call.Path, mr+"/discussions?"):
			return forgeAPIAnswer{Body: discussions()}
		case call.Method == http.MethodGet && call.Path == mr+"/discussions/d9":
			if state.Load() == "refused-gone" {
				return forgeAPIAnswer{Status: http.StatusNotFound, Body: `{"message":"404 Not found"}`}
			}
			return forgeAPIAnswer{Body: `{"id":"d9"}`}
		case call.Method == http.MethodPost && call.Path == mr+"/draft_notes":
			if st := state.Load(); (st == "refused-gone" || st == "refused-other") && strings.Contains(string(call.Body), "in_reply_to_discussion_id") {
				return forgeAPIAnswer{Status: http.StatusBadRequest, Body: `{"message":"400 Bad request"}`}
			}
			return forgeAPIAnswer{Status: http.StatusCreated, Body: fmt.Sprintf(`{"id":%d}`, len(call.Body))}
		case call.Method == http.MethodDelete && strings.HasPrefix(call.Path, mr+"/draft_notes/"):
			return forgeAPIAnswer{Status: http.StatusNoContent}
		case call.Method == http.MethodPost && call.Path == mr+"/draft_notes/bulk_publish":
			published.Store(true)
			return forgeAPIAnswer{Status: http.StatusNoContent}
		case call.Path == "user":
			return forgeAPIAnswer{Body: `{"id":1,"username":"me"}`}
		}
		return forgeUnexpected(t, call)
	})
	request := SubmitReviewRequest{Verdict: ReviewVerdictComment, Body: "summary", Comments: []ReviewLineComment{
		{Body: "held", InReplyTo: &ReviewThreadReply{ThreadID: "d9"}},
	}}
	draftBodies := func(from int) []string {
		var notes []string
		for _, call := range calls.all()[from:] {
			if call.Method == http.MethodPost && call.Path == mr+"/draft_notes" {
				notes = append(notes, string(call.Body))
			}
		}
		return notes
	}
	result, err := core.SubmitReview(t.Context(), gitlabTestRef(7), request)
	if err != nil || !result.PostedReview {
		t.Fatalf("SubmitReview = %+v, %v", result, err)
	}
	if got := strings.Join(draftBodies(0), "\n"); got != `{"note":"summary"}`+"\n"+`{"note":"held","in_reply_to_discussion_id":"d9","resolve_discussion":true}` {
		t.Fatalf("draft notes = %v", got)
	}
	// An open discussion's reply keeps it open.
	open := SubmitReviewRequest{Verdict: ReviewVerdictComment, Comments: []ReviewLineComment{{Body: "open root", InReplyTo: &ReviewThreadReply{ThreadID: "d8"}}}}
	seen := len(calls.all())
	published.Store(false)
	_, _ = core.SubmitReview(t.Context(), gitlabTestRef(7), open)
	if got := draftBodies(seen); len(got) != 1 || got[0] != `{"note":"open root","in_reply_to_discussion_id":"d8","resolve_discussion":false}` {
		t.Fatalf("open discussion's draft = %v", got)
	}

	// Gone before the submit: nothing is created.
	state.Store("gone-before")
	published.Store(false)
	seen = len(calls.all())
	_, err = core.SubmitReview(t.Context(), gitlabTestRef(7), request)
	if gone, ok := errors.AsType[*ReplyThreadGoneError](err); !ok || gone.ThreadID != "d9" || len(draftBodies(seen)) != 0 {
		t.Fatalf("gone before = %v, drafts %v", err, draftBodies(seen))
	}

	// A 400 on the reply's draft is gone when the discussion's read is a
	// 404, and the summary's draft is removed; another 400 is not gone.
	for _, tc := range []struct {
		state string
		gone  bool
	}{{"refused-gone", true}, {"refused-other", false}} {
		state.Store(tc.state)
		seen = len(calls.all())
		_, err = core.SubmitReview(t.Context(), gitlabTestRef(7), request)
		if _, ok := errors.AsType[*ReplyThreadGoneError](err); ok != tc.gone || err == nil {
			t.Fatalf("%s = %v", tc.state, err)
		}
		if got := calls.ops()[seen:]; !strings.HasPrefix(got[len(got)-1], "DELETE "+mr+"/draft_notes/") {
			t.Fatalf("%s requests = %v, want the summary's draft removed", tc.state, got)
		}
	}

	// GitLab published the review and dropped the reply: a partial naming
	// its thread.
	state.Store("dropped")
	published.Store(false)
	result, err = core.SubmitReview(t.Context(), gitlabTestRef(7), request)
	partial, ok := errors.AsType[*PartialSubmitError](err)
	if !ok || !partial.PostedReview || partial.FailedThreadID != "d9" || !slices.Equal(partial.UnpostedReplies, []int{0}) || !result.PostedReview {
		t.Fatalf("dropped reply = %+v, %v", result, err)
	}
	if _, ok := errors.AsType[*ReplyThreadGoneError](err); !ok {
		t.Fatalf("dropped reply = %v, want the thread gone", err)
	}
}

// A developer may edit and delete its own notes and no other; a
// maintainer or owner every note. Each note links to its anchor on the
// merge request.
func TestGitLabReadMarksTheViewersNotes(t *testing.T) {
	t.Parallel()
	mr := gitlabTestProject + "/merge_requests/7"
	for _, tc := range []struct {
		access       int
		mayEditOther bool
	}{{30, false}, {40, true}, {50, true}} {
		core, _ := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
			switch {
			case call.Path == mr:
				return forgeAPIAnswer{Body: gitlabTestMR(7, 0)}
			case strings.HasPrefix(call.Path, mr+"/discussions?"):
				return forgeAPIAnswer{Body: `[{"id":"d","notes":[{"id":1,"body":"mine","author":{"username":"Me"}},{"id":2,"body":"theirs","author":{"username":"rev"}}]}]`}
			case call.Path == gitlabTestProject:
				return forgeAPIAnswer{Body: fmt.Sprintf(`{"permissions":{"project_access":null,"group_access":{"access_level":%d}}}`, tc.access)}
			case call.Path == "user":
				return forgeAPIAnswer{Body: `{"id":1,"username":"me"}`}
			}
			return forgeUnexpected(t, call)
		})
		threads, err := core.ListReviewThreads(t.Context(), gitlabTestRef(7))
		if err != nil || len(threads) != 1 || len(threads[0].Comments) != 2 {
			t.Fatalf("access %d: threads = %+v, %v", tc.access, threads, err)
		}
		mine, theirs := threads[0].Comments[0], threads[0].Comments[1]
		if mine.ID != "1" || !mine.ViewerCanEdit || !mine.ViewerCanDelete || mine.URL != "https://gitlab.example/grp/sub/app/-/merge_requests/7#note_1" {
			t.Fatalf("access %d: viewer's note = %+v", tc.access, mine)
		}
		if theirs.ViewerCanEdit != tc.mayEditOther || theirs.ViewerCanDelete != tc.mayEditOther || mine.EditedAt != "" {
			t.Fatalf("access %d: another's note = %+v", tc.access, theirs)
		}
	}
}

// A reply now whose thread is gone says so on both forges.
func TestReplyToAGoneThread(t *testing.T) {
	t.Parallel()
	core, _ := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		return forgeAPIAnswer{Status: http.StatusNotFound, Body: `{"message":"Not Found"}`}
	})
	for _, ref := range []PRReference{githubTestRef(5), gitlabTestRef(7)} {
		err := core.ReplyToThread(t.Context(), ref, "t1", 4, "hi")
		if gone, ok := errors.AsType[*ReplyThreadGoneError](err); !ok || gone.ThreadID != "t1" {
			t.Fatalf("%s: reply to a gone thread = %v", ref.Forge, err)
		}
	}
}
