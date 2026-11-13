package main

import (
	"errors"
	"strings"
	"testing"

	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/harness/forgefake"
)

// commentDiff is the diff commentFixture's pull changes: a.go adds lines
// 3 to 8, b.go removes its old line 2, c.go changes its only line, and
// old.go is renamed new.go with its line 2 replaced, and d.go replaces
// old lines 11 and 12 with new 11 and 12 between unchanged lines.
const commentDiff = "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1,2 +1,8 @@\n o1\n o2\n+n3\n+n4\n+n5\n+n6\n+n7\n+n8\n" +
	"diff --git a/b.go b/b.go\n--- a/b.go\n+++ b/b.go\n@@ -1,3 +1,2 @@\n x\n-y\n z\n" +
	"diff --git a/c.go b/c.go\n--- a/c.go\n+++ b/c.go\n@@ -1 +1 @@\n-a\n+b\n" +
	"diff --git a/old.go b/new.go\nsimilarity index 80%\nrename from old.go\nrename to new.go\n--- a/old.go\n+++ b/new.go\n@@ -1,3 +1,3 @@\n k\n-gone\n+came\n k2\n" +
	"diff --git a/d.go b/d.go\n--- a/d.go\n+++ b/d.go\n@@ -10,5 +10,5 @@\n c10\n-r11\n-r12\n+a11\n+a12\n c13\n c14\n"

// commentFixture adds to a merge fixture's first pull commentDiff, a
// review thread and a conversation comment of another author's, which the
// viewer may neither edit nor delete: on GitHub, where write access may,
// the viewer is TRIAGE.
func commentFixture(fixture forgefake.Fixture) forgefake.Fixture {
	if fixture.Repos[0].Forge == "github" {
		fixture.Repos[0].Merge.Permission = "TRIAGE"
	}
	p := &fixture.Repos[0].Pulls[0]
	p.Diff = commentDiff
	p.Threads = []forgefake.Thread{{Path: "a.go", Line: intPtr(4), Side: "right", Comments: []forgefake.Comment{{Author: "octo", Body: "theirs"}}}}
	p.Comments = []forgefake.Comment{{Author: "octo", Body: "their note"}}
	return fixture
}

// threadByBody is the thread whose first comment has body, nil if none.
func threadByBody(read gitops.PRRead, body string) *gitops.ReviewThread {
	for i := range read.Threads {
		if len(read.Threads[i].Comments) > 0 && read.Threads[i].Comments[0].Body == body {
			return &read.Threads[i]
		}
	}
	return nil
}

func targetOf(thread *gitops.ReviewThread, at int) gitops.CommentTarget {
	return gitops.CommentTarget{ThreadID: thread.ID, CommentID: thread.Comments[at].ID}
}

func expectAllHandled(t *testing.T, r *rig) {
	t.Helper()
	for _, inv := range r.engine.Invocations(0).Invocations {
		if inv.Unhandled {
			t.Errorf("unhandled %s %s: %s", inv.Method, inv.Path, inv.Detail)
		}
	}
}

// Every comment action of both forges through the app's own code: a
// comment now on a line, a range, a file and the PR, a reply now, an edit
// and a delete each change what the next read answers, the read carries
// the viewer's permissions, and the forge's refusals change nothing.
func TestCommentActionsThroughTheAppsForgeCode(t *testing.T) {
	for _, tc := range []struct {
		forge   string
		fixture forgefake.Fixture
		ref     gitops.PRReference
		head    string
	}{
		{"github", githubMergeFixture("success"), gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "merge", Number: 9}, strings.Repeat("9", 40)},
		{"gitlab", gitlabMergeFixture("success"), gitops.PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp", Repo: "merge", Number: 4}, strings.Repeat("4", 40)},
	} {
		t.Run(tc.forge, func(t *testing.T) {
			r := newRig(t, commentFixture(tc.fixture))
			ctx := forgeapi.WithInteractive(t.Context())
			ref := tc.ref

			read := mergeRead(t, r, ref)
			theirs, note := threadByBody(read, "theirs"), threadByBody(read, "their note")
			if theirs == nil || note == nil {
				t.Fatalf("seeded threads = %+v", read.Threads)
			}
			if c := theirs.Comments[0]; c.ID == "" || c.ViewerCanEdit || c.ViewerCanDelete || c.URL == "" || c.EditedAt != "" {
				t.Fatalf("another author's comment = %+v", c)
			}

			line, start, oldLine := 6, 3, 2
			for _, comment := range []gitops.ReviewLineComment{
				{Path: "a.go", Body: "on a line", Line: &line, Side: "right"},
				{Path: "a.go", Body: "on a range", Line: &line, Side: "right", Start: &gitops.ReviewLineStart{Line: start, Side: "right"}},
				{Path: "b.go", Body: "on a removed line", Line: &oldLine, Side: "left"},
				{Path: "c.go", Body: "on a file", Side: "file"},
			} {
				if err := r.core.PostPRLineComment(ctx, ref, tc.head, comment); err != nil {
					t.Fatalf("PostPRLineComment %q: %v", comment.Body, err)
				}
			}
			postedID, err := r.core.PostPRComment(ctx, ref, "on the PR")
			if err != nil {
				t.Fatalf("PostPRComment: %v", err)
			}
			if err := r.core.ReplyToThread(ctx, ref, theirs.ID, theirs.Comments[0].DatabaseID, "a reply"); err != nil {
				t.Fatalf("ReplyToThread: %v", err)
			}
			if err := r.core.ReplyToThread(ctx, ref, "PRRT_gone0000000000000000000000000000000", 424242, "lost"); !errors.As(err, new(*gitops.ReplyThreadGoneError)) {
				t.Fatalf("a reply to a missing thread = %v", err)
			}
			// A comment against a head the PR moved from posts nothing.
			stale := tc.head[:39] + "0"
			if err := r.core.PostPRLineComment(ctx, ref, stale, gitops.ReviewLineComment{Path: "a.go", Body: "stale", Line: &line, Side: "right"}); err == nil {
				t.Fatal("a comment on a moved head posted")
			}

			read = mergeRead(t, r, ref)
			onLine, onRange, removed, onFile, onPR := threadByBody(read, "on a line"), threadByBody(read, "on a range"), threadByBody(read, "on a removed line"), threadByBody(read, "on a file"), threadByBody(read, "on the PR")
			switch {
			case onLine == nil || *onLine.Line != 6 || onLine.Side != "right" || onLine.StartLine != nil:
				t.Fatalf("line comment = %+v", onLine)
			case onRange == nil || *onRange.Line != 6 || onRange.StartLine == nil || *onRange.StartLine != 3 || onRange.Side != "right":
				t.Fatalf("range comment = %+v", onRange)
			case removed == nil || *removed.Line != 2 || removed.Side != "left":
				t.Fatalf("removed line comment = %+v", removed)
			case onFile == nil || onFile.Path != "c.go" || onFile.Line != nil:
				t.Fatalf("file comment = %+v", onFile)
			// The post names the comment as the read does.
			case onPR == nil || postedID == "" || onPR.Comments[0].ID != postedID:
				t.Fatalf("PR comment = %+v, answered id %q", onPR, postedID)
			case onPR == nil || onPR.Path != "":
				t.Fatalf("PR comment = %+v", onPR)
			case threadByBody(read, "stale") != nil:
				t.Fatal("the stale comment landed")
			}
			if c := onLine.Comments[0]; c.AuthorLogin != "alice" || !c.ViewerCanEdit || !c.ViewerCanDelete || c.ID == "" {
				t.Fatalf("the viewer's comment = %+v", c)
			}
			if got := threadByBody(read, "theirs"); len(got.Comments) != 2 || got.Comments[1].Body != "a reply" || !got.Comments[1].ViewerCanEdit {
				t.Fatalf("thread after the reply = %+v", got.Comments)
			}

			// Edit: the viewer's own comment changes and reads as edited on
			// GitHub; another author's is refused and stays.
			if err := r.core.EditPRComment(ctx, ref, targetOf(onLine, 0), "edited"); err != nil {
				t.Fatalf("EditPRComment: %v", err)
			}
			if err := r.core.EditPRComment(ctx, ref, targetOf(onPR, 0), "edited note"); err != nil {
				t.Fatalf("EditPRComment on the PR comment: %v", err)
			}
			if err := r.core.EditPRComment(ctx, ref, targetOf(theirs, 0), "mine now"); err == nil {
				t.Fatal("an edit of another author's comment succeeded")
			}
			if err := r.core.DeletePRComment(ctx, ref, targetOf(theirs, 0)); err == nil {
				t.Fatal("a delete of another author's comment succeeded")
			}
			read = mergeRead(t, r, ref)
			edited := threadByBody(read, "edited")
			if edited == nil || threadByBody(read, "edited note") == nil || threadByBody(read, "theirs") == nil {
				t.Fatalf("threads after the edits = %+v", read.Threads)
			}
			if (tc.forge == "github") != (edited.Comments[0].EditedAt != "") {
				t.Fatalf("edited mark = %q", edited.Comments[0].EditedAt)
			}

			// Delete: a thread's only comment takes the thread; the PR
			// comment goes; a reply goes and leaves its thread.
			if err := r.core.DeletePRComment(ctx, ref, targetOf(edited, 0)); err != nil {
				t.Fatalf("DeletePRComment: %v", err)
			}
			if err := r.core.DeletePRComment(ctx, ref, targetOf(threadByBody(read, "edited note"), 0)); err != nil {
				t.Fatalf("DeletePRComment on the PR comment: %v", err)
			}
			if err := r.core.DeletePRComment(ctx, ref, targetOf(threadByBody(read, "theirs"), 1)); err != nil {
				t.Fatalf("DeletePRComment on the reply: %v", err)
			}
			read = mergeRead(t, r, ref)
			if threadByBody(read, "edited") != nil || threadByBody(read, "edited note") != nil || len(threadByBody(read, "theirs").Comments) != 1 {
				t.Fatalf("threads after the deletes = %+v", read.Threads)
			}
			// A deleted comment is the forge's not-found on a second delete.
			if err := r.core.DeletePRComment(ctx, ref, targetOf(edited, 0)); err == nil {
				t.Fatal("a second delete succeeded")
			}
			expectAllHandled(t, r)
		})
	}
}

// A maintainer may edit and delete another author's comment on both
// forges (GitHub: write access and above; GitLab: "Edit comments posted
// by other users" is Maintainer and Owner), and the read says so: GitLab
// for every note from the viewer's cached project role, GitHub in its own
// viewerCanUpdate and viewerCanDelete.
func TestMaintainersDeleteOthersCommentsThroughTheAppsForgeCode(t *testing.T) {
	for _, tc := range []struct {
		fixture forgefake.Fixture
		role    string
		ref     gitops.PRReference
		mayEdit bool
	}{
		{githubMergeFixture("success"), "MAINTAIN", gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "merge", Number: 9}, true},
		{gitlabMergeFixture("success"), "maintainer", gitops.PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp", Repo: "merge", Number: 4}, true},
	} {
		t.Run(tc.ref.Forge, func(t *testing.T) {
			fixture := commentFixture(tc.fixture)
			fixture.Repos[0].Merge.Permission = tc.role
			r := newRig(t, fixture)
			ctx := forgeapi.WithInteractive(t.Context())
			theirs := threadByBody(mergeRead(t, r, tc.ref), "theirs")
			if c := theirs.Comments[0]; !c.ViewerCanDelete || c.ViewerCanEdit != tc.mayEdit {
				t.Fatalf("a maintainer's permissions = %+v", c)
			}
			err := r.core.EditPRComment(ctx, tc.ref, targetOf(theirs, 0), "edited by a maintainer")
			if (err == nil) != tc.mayEdit {
				t.Fatalf("a maintainer's edit = %v, want allowed %v", err, tc.mayEdit)
			}
			if err := r.core.DeletePRComment(ctx, tc.ref, targetOf(theirs, 0)); err != nil {
				t.Fatalf("a maintainer's delete: %v", err)
			}
			read := mergeRead(t, r, tc.ref)
			if threadByBody(read, "theirs") != nil || threadByBody(read, "edited by a maintainer") != nil {
				t.Fatal("the deleted thread is still read")
			}
			expectAllHandled(t, r)
		})
	}
	// A GitLab developer may edit and delete its own notes alone.
	r := newRig(t, commentFixture(gitlabMergeFixture("success")))
	ref := gitops.PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp", Repo: "merge", Number: 4}
	if c := threadByBody(mergeRead(t, r, ref), "theirs").Comments[0]; c.ViewerCanEdit || c.ViewerCanDelete {
		t.Fatalf("a developer's permissions on another author's note = %+v", c)
	}
}

// A review submit carrying held replies through the app's own code: each
// reply joins its thread, and a reply whose thread is gone is reported as
// such, on GitHub after the review landed, on GitLab before anything
// published.
func TestSubmitReviewHeldRepliesThroughTheAppsForgeCode(t *testing.T) {
	for _, tc := range []struct {
		fixture forgefake.Fixture
		ref     gitops.PRReference
	}{
		{githubMergeFixture("success"), gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "merge", Number: 9}},
		{gitlabMergeFixture("success"), gitops.PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp", Repo: "merge", Number: 4}},
	} {
		t.Run(tc.ref.Forge, func(t *testing.T) {
			r := newRig(t, commentFixture(tc.fixture))
			ctx := forgeapi.WithInteractive(t.Context())
			read := mergeRead(t, r, tc.ref)
			theirs, note := threadByBody(read, "theirs"), threadByBody(read, "their note")
			reply := func(thread *gitops.ReviewThread, body string) gitops.ReviewLineComment {
				return gitops.ReviewLineComment{Path: thread.Path, Body: body, InReplyTo: &gitops.ReviewThreadReply{ThreadID: thread.ID, RootDatabaseID: thread.Comments[0].DatabaseID}}
			}
			line := 5
			review := gitops.SubmitReviewRequest{Verdict: gitops.ReviewVerdictComment, Body: "Notes", Comments: []gitops.ReviewLineComment{
				{Path: "a.go", Body: "new line note", Line: &line, Side: "right"},
				reply(theirs, "held reply"),
			}}
			if tc.ref.Forge == "gitlab" {
				// A GitLab conversation comment's discussion takes replies too.
				review.Comments = append(review.Comments, reply(note, "held note reply"))
			}
			result, err := r.core.SubmitReview(ctx, tc.ref, review)
			if err != nil {
				t.Fatalf("SubmitReview: %v", err)
			}
			if tc.ref.Forge == "github" && result.PostedReplies != 1 {
				t.Fatalf("result = %+v", result)
			}
			read = mergeRead(t, r, tc.ref)
			got := threadByBody(read, "theirs")
			if len(got.Comments) != 2 || got.Comments[1].Body != "held reply" || threadByBody(read, "new line note") == nil {
				t.Fatalf("threads after the submit = %+v", read.Threads)
			}
			if tc.ref.Forge == "gitlab" {
				if got := threadByBody(read, "their note"); len(got.Comments) != 2 || got.Comments[1].Body != "held note reply" {
					t.Fatalf("conversation after the submit = %+v", got)
				}
			}

			// The thread is deleted before the next submit.
			fixture := commentFixture(tc.fixture)
			fixture.Repos[0].Pulls[0].Threads = nil
			if _, err := r.engine.Seed(fixture); err != nil {
				t.Fatal(err)
			}
			_, err = r.core.SubmitReview(ctx, tc.ref, gitops.SubmitReviewRequest{Verdict: gitops.ReviewVerdictComment, Body: "Again", Comments: []gitops.ReviewLineComment{reply(theirs, "too late")}})
			if !errors.As(err, new(*gitops.ReplyThreadGoneError)) {
				t.Fatalf("a held reply to a deleted thread = %v", err)
			}
			if got := explained(t, tc.ref, gitops.ActionReview, err); got.Kind != gitops.ForgeErrorNotFound {
				t.Fatalf("explained = %+v", got)
			}
			var partial *gitops.PartialSubmitError
			if isPartial := errors.As(err, &partial); isPartial != (tc.ref.Forge == "github") || (isPartial && (!partial.PostedReview || partial.FailedThreadID != theirs.ID)) {
				t.Fatalf("partial = %+v", partial)
			}
			expectAllHandled(t, r)
		})
	}
}

// An edit or delete names its comment by a global id; one of another pull
// request or another repository is refused through the app's own code and
// the comment is unchanged, though the viewer could edit it there.
func TestGitHubCommentWritesStayOnTheirPR(t *testing.T) {
	fixture := githubMergeFixture("success")
	mine := func(body string) []forgefake.Thread {
		return []forgefake.Thread{{Path: "a.go", Line: intPtr(2), Side: "right", Comments: []forgefake.Comment{{Author: "alice", Body: body}}}}
	}
	fixture.Repos[0].Pulls[1].Threads = mine("on pull 10")
	fixture.Repos[0].Pulls[1].Comments = []forgefake.Comment{{Author: "alice", Body: "note on pull 10"}}
	fixture.Repos = append(fixture.Repos, forgefake.Repo{Forge: "github", Project: "acme/other", ID: 5151, Pulls: []forgefake.Pull{
		{Number: 9, Title: "Elsewhere", Author: "octo", HeadRef: "x", Threads: mine("in another repository")},
	}})
	r := newRig(t, fixture)
	ctx := forgeapi.WithInteractive(t.Context())
	ref := gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "merge", Number: 9}
	pull10 := mergeRead(t, r, gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "merge", Number: 10})
	other := mergeRead(t, r, gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "other", Number: 9})
	for _, thread := range []*gitops.ReviewThread{threadByBody(pull10, "on pull 10"), threadByBody(pull10, "note on pull 10"), threadByBody(other, "in another repository")} {
		for _, write := range []func() error{
			func() error { return r.core.EditPRComment(ctx, ref, targetOf(thread, 0), "hijacked") },
			func() error { return r.core.DeletePRComment(ctx, ref, targetOf(thread, 0)) },
		} {
			if got := explained(t, ref, gitops.ActionEdit, write()); got.Kind != gitops.ForgeErrorNotFound {
				t.Fatalf("a write on %q = %+v", thread.Comments[0].Body, got)
			}
		}
	}
	pull10 = mergeRead(t, r, gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "merge", Number: 10})
	other = mergeRead(t, r, gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "other", Number: 9})
	if threadByBody(pull10, "on pull 10") == nil || threadByBody(pull10, "note on pull 10") == nil || threadByBody(other, "in another repository") == nil {
		t.Fatal("a write named by another PR changed its comment")
	}
	expectAllHandled(t, r)
}

// A GitLab comment on a renamed file names both its paths, on either side
// and across them, posted now and held in a review, and reads back on the
// file's new path.
func TestGitLabCommentsOnARenamedFileThroughTheAppsForgeCode(t *testing.T) {
	r := newRig(t, commentFixture(gitlabMergeFixture("success")))
	ctx := forgeapi.WithInteractive(t.Context())
	ref := gitops.PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp", Repo: "merge", Number: 4}
	two, three := 2, 3
	for _, comment := range []gitops.ReviewLineComment{
		{Path: "new.go", Body: "on the removed line", Line: &two, Side: "left"},
		{Path: "new.go", Body: "on the added line", Line: &two, Side: "right"},
		{Path: "new.go", Body: "on the renamed file", Side: "file"},
	} {
		if err := r.core.PostPRLineComment(ctx, ref, strings.Repeat("4", 40), comment); err != nil {
			t.Fatalf("PostPRLineComment %q: %v", comment.Body, err)
		}
	}
	review := gitops.SubmitReviewRequest{Verdict: gitops.ReviewVerdictComment, Comments: []gitops.ReviewLineComment{
		{Path: "new.go", Body: "held across the change", Line: &three, OldLine: &three, Side: "right", Start: &gitops.ReviewLineStart{Line: 2, Side: "right"}},
	}}
	if _, err := r.core.SubmitReview(ctx, ref, review); err != nil {
		t.Fatalf("SubmitReview: %v", err)
	}
	expectAllHandled(t, r)
	read := mergeRead(t, r, ref)
	for _, body := range []string{"on the removed line", "on the added line", "on the renamed file", "held across the change"} {
		if thread := threadByBody(read, body); thread == nil || thread.Path != "new.go" {
			t.Fatalf("%q = %+v", body, thread)
		}
	}
}

// A GitLab range the app posts reads back as the same range on the same
// side: removed lines on the left, added lines on the right, and a range
// from a removed line to an unchanged one on the right from the removed
// line's number, as GitHub reads a range across sides.
func TestGitLabRangesReadBackAsPostedThroughTheAppsForgeCode(t *testing.T) {
	r := newRig(t, commentFixture(gitlabMergeFixture("success")))
	ctx := forgeapi.WithInteractive(t.Context())
	ref := gitops.PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp", Repo: "merge", Number: 4}
	n := func(v int) *int { return &v }
	cases := []struct {
		comment     gitops.ReviewLineComment
		side        string
		start, line int
	}{
		{gitops.ReviewLineComment{Path: "d.go", Body: "old side", Line: n(12), Side: "left", Start: &gitops.ReviewLineStart{Line: 11, Side: "left"}}, "left", 11, 12},
		{gitops.ReviewLineComment{Path: "d.go", Body: "new side", Line: n(12), Side: "right", Start: &gitops.ReviewLineStart{Line: 11, Side: "right"}}, "right", 11, 12},
		{gitops.ReviewLineComment{Path: "d.go", Body: "mixed", Line: n(13), OldLine: n(13), Side: "right", Start: &gitops.ReviewLineStart{Line: 11, Side: "left"}}, "right", 11, 13},
		{gitops.ReviewLineComment{Path: "d.go", Body: "unchanged", Line: n(14), OldLine: n(14), Side: "right", Start: &gitops.ReviewLineStart{Line: 13, OldLine: 13, Side: "right"}}, "right", 13, 14},
	}
	for _, tc := range cases {
		if err := r.core.PostPRLineComment(ctx, ref, strings.Repeat("4", 40), tc.comment); err != nil {
			t.Fatalf("PostPRLineComment %q: %v", tc.comment.Body, err)
		}
	}
	held := cases[2].comment
	held.Body = "mixed, held"
	if _, err := r.core.SubmitReview(ctx, ref, gitops.SubmitReviewRequest{Verdict: gitops.ReviewVerdictComment, Comments: []gitops.ReviewLineComment{held}}); err != nil {
		t.Fatalf("SubmitReview: %v", err)
	}
	expectAllHandled(t, r)
	read := mergeRead(t, r, ref)
	for _, tc := range append(cases, struct {
		comment     gitops.ReviewLineComment
		side        string
		start, line int
	}{held, "right", 11, 13}) {
		got := threadByBody(read, tc.comment.Body)
		if got == nil || got.Side != tc.side || got.Line == nil || *got.Line != tc.line || got.StartLine == nil || *got.StartLine != tc.start {
			t.Fatalf("%q reads back as %+v, want %s %d..%d", tc.comment.Body, got, tc.side, tc.start, tc.line)
		}
	}
}
