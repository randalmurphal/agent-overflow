package store

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// The rebuild keeps every thread-owned comment as it was and lets a row
// belong to a PR instead, never to both or neither; pr_views and the
// thread_pull_requests lookup index arrive empty.
func TestPullRequestsPageMigrationKeepsCommentsAndAddsPROwnership(t *testing.T) {
	db := migrateThrough(t, pullRequestsPageMigrationVersion-1)
	mustExec(t, db, `INSERT INTO projects(id,path,name,slug,sort_position,created_at,updated_at) VALUES ('p','/p','p','p',0,1,1)`)
	mustExec(t, db, `INSERT INTO threads(id,project_id,title,provider,model,workspace_path,created_at,updated_at) VALUES ('t','p','T','claude','m','/p',1,1)`)
	mustExec(t, db, `INSERT INTO diff_review_comments(id,thread_id,scope,source_key,commit_sha,file_path,status,old_line,new_line,side,selected_text,body,sent_at,sent_turn_id,created_at,updated_at)
		VALUES ('c','t','pr','pr:github:o/r:1','abc','a.go','sent',0,3,'new','+x','fix',7,'user:1',5,6)`)
	if err := applyRebuildMigration(db, migrationByVersion(t, pullRequestsPageMigrationVersion)); err != nil {
		t.Fatal(err)
	}
	var threadID, scope, sourceKey, commit, path, status, side, selected, body, sentTurn string
	var prKey *string
	var oldLine, newLine int
	var sentAt, created, updated int64
	if err := db.QueryRow(`SELECT thread_id,pr_key,scope,source_key,commit_sha,file_path,status,old_line,new_line,side,selected_text,body,sent_at,sent_turn_id,created_at,updated_at FROM diff_review_comments WHERE id='c'`).
		Scan(&threadID, &prKey, &scope, &sourceKey, &commit, &path, &status, &oldLine, &newLine, &side, &selected, &body, &sentAt, &sentTurn, &created, &updated); err != nil {
		t.Fatal(err)
	}
	if threadID != "t" || prKey != nil || scope != "pr" || sourceKey != "pr:github:o/r:1" || commit != "abc" || path != "a.go" || status != "sent" ||
		oldLine != 0 || newLine != 3 || side != "new" || selected != "+x" || body != "fix" || sentAt != 7 || sentTurn != "user:1" || created != 5 || updated != 6 {
		t.Fatal("the rebuild changed a thread's comment")
	}
	mustExec(t, db, `INSERT INTO diff_review_comments(id,pr_key,scope,source_key,file_path,side,body,created_at,updated_at) VALUES ('d','github:o/r:1','pr','k','a.go','file','b',1,1)`)
	for _, bad := range []string{
		`INSERT INTO diff_review_comments(id,thread_id,pr_key,scope,source_key,file_path,side,body,created_at,updated_at) VALUES ('both','t','github:o/r:1','pr','k','a.go','file','b',1,1)`,
		`INSERT INTO diff_review_comments(id,scope,source_key,file_path,side,body,created_at,updated_at) VALUES ('neither','pr','k','a.go','file','b',1,1)`,
		`INSERT INTO diff_review_comments(id,pr_key,scope,source_key,file_path,side,body,created_at,updated_at) VALUES ('scope','github:o/r:1','workspace','k','a.go','file','b',1,1)`,
		`INSERT INTO pr_views(pr_key,opened_at,head_sha,comments) VALUES ('k',1,'h',-1)`,
		`INSERT INTO pr_views(pr_key,opened_at,head_sha) VALUES ('',1,'h')`,
	} {
		if _, err := db.Exec(bad); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
	mustExec(t, db, `INSERT INTO pr_views(pr_key,opened_at,head_sha) VALUES ('github:o/r:1',1,'h')`)
	for _, index := range []string{"idx_diff_review_comments_scope", "idx_diff_review_comments_pr", "idx_thread_pull_requests_pr"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, index).Scan(&n); err != nil || n != 1 {
			t.Errorf("index %s: %d, %v", index, n, err)
		}
	}
	mustExec(t, db, `PRAGMA foreign_keys = ON`)
	mustExec(t, db, `DELETE FROM threads WHERE id = 't'`)
	var left []string
	rows, err := db.Query(`SELECT id FROM diff_review_comments ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		left = append(left, id)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(left, ",") != "d" {
		t.Fatalf("rows after the thread's delete = %v, want only the PR's", left)
	}
}

func prDraft(id, prKey string) DiffReviewComment {
	return DiffReviewComment{
		ID: id, PRKey: prKey, Scope: string(DiffReviewScopePR), SourceKey: "pr:" + prKey,
		FilePath: "main.go", NewLine: 4, Side: "new", Body: "PR-owned note", CreatedAt: 10, UpdatedAt: 10,
	}
}

// A PR's drafts go through the same accessors under their own owner, and
// a thread's paths never see them.
func TestPROwnedDiffReviewCommentsStayWithTheirPR(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "th")
	const key = "github:o/r:7"
	pr := PRReviewOwner(key)
	created, err := s.CreateDiffReviewComment(prDraft("p1", key))
	if err != nil || created.PRKey != key || created.ThreadID != "" || created.Status != "draft" {
		t.Fatalf("create = %+v, %v", created, err)
	}
	if _, err := s.CreateDiffReviewComment(DiffReviewComment{ID: "x", ThreadID: "th", PRKey: key, Scope: "pr", SourceKey: "k", FilePath: "a", Side: "file", Body: "b"}); err == nil {
		t.Fatal("a comment owned by both a thread and a PR was accepted")
	}
	bad := prDraft("p2", key)
	bad.Scope = string(DiffReviewScopeWorkspace)
	if _, err := s.CreateDiffReviewComment(bad); err == nil {
		t.Fatal("a PR draft outside pr scope was accepted")
	}
	thread := prDraft("t1", "")
	thread.PRKey, thread.ThreadID = "", "th"
	if _, err := s.CreateDiffReviewComment(thread); err != nil {
		t.Fatal(err)
	}
	if listed, err := s.ListDiffReviewComments(pr, "pr", "pr:"+key); err != nil || len(listed) != 1 || listed[0].ID != "p1" {
		t.Fatalf("PR list = %+v, %v", listed, err)
	}
	if _, err := s.GetDiffReviewComment(ThreadReviewOwner("th"), "p1"); err == nil {
		t.Fatal("a thread read a PR's draft")
	}
	if _, err := s.UpdateDiffReviewComment(PRReviewOwner("github:o/r:8"), "p1", DiffReviewCommentUpdate{Body: "x"}, 20); err == nil {
		t.Fatal("another PR updated the draft")
	}
	if updated, err := s.UpdateDiffReviewComment(pr, "p1", DiffReviewCommentUpdate{Body: "edited"}, 20); err != nil || updated.Body != "edited" {
		t.Fatalf("update = %+v, %v", updated, err)
	}
	if _, err := s.ListDraftDiffReviewCommentsByID(pr, "pr", "pr:"+key, []string{"p1", "t1"}); err == nil {
		t.Fatal("a PR's selection took a thread's draft")
	}
	if err := s.MarkDiffReviewCommentsSent(pr, "pr", "pr:"+key, []string{"p1"}, 30, "user:2"); err != nil {
		t.Fatal(err)
	}
	if sent, err := s.GetDiffReviewComment(pr, "p1"); err != nil || sent.Status != "sent" || sent.SentTurnID != "user:2" {
		t.Fatalf("sent = %+v, %v", sent, err)
	}

	// A thread's delete and export never touch a PR's rows.
	var exported bytes.Buffer
	if err := s.exportTransferAnnotations(context.Background(), "th", &exported); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(exported.String(), "p1") || !strings.Contains(exported.String(), "t1") {
		t.Fatalf("export = %s, want the thread's comment and not the PR's", exported.String())
	}
	if err := s.DeleteThread("th"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetDiffReviewComment(pr, "p1"); err != nil {
		t.Fatalf("the PR's draft went with the thread: %v", err)
	}
	if err := s.DeleteOrResolveDiffReviewComment(pr, "p1", 40); err != nil {
		t.Fatal(err)
	}
	if resolved, err := s.GetDiffReviewComment(pr, "p1"); err != nil || resolved.Status != "resolved" {
		t.Fatalf("resolved = %+v, %v", resolved, err)
	}
}

func TestPRViewsRecordTheLatestOpening(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	comments, commits := 3, 2
	first, err := s.PutPRView(PRView{PRKey: "github:o/r:1", OpenedAt: 10, HeadSHA: "a", Comments: &comments, Commits: &commits})
	if err != nil || first.Comments == nil || *first.Comments != 3 || *first.Commits != 2 {
		t.Fatalf("first = %+v, %v", first, err)
	}
	second, err := s.PutPRView(PRView{PRKey: "github:o/r:1", OpenedAt: 20, HeadSHA: "b"})
	if err != nil || second.OpenedAt != 20 || second.HeadSHA != "b" || second.Comments != nil || second.Commits != nil {
		t.Fatalf("second = %+v, %v", second, err)
	}
	if _, err := s.PutPRView(PRView{PRKey: "github:o/r:2", HeadSHA: "c"}); err == nil {
		t.Fatal("a view with no time was accepted")
	}
	views, err := s.ListPRViews([]string{"github:o/r:1", "github:o/r:9", "github:o/r:1"})
	if err != nil || len(views) != 1 || views["github:o/r:1"].HeadSHA != "b" {
		t.Fatalf("views = %+v, %v", views, err)
	}
}

func TestListThreadPullRequestsForPRFindsEveryTrackingThread(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"a", "b", "c"} {
		mustCreateThread(t, s, id)
	}
	for _, row := range []ThreadPullRequest{trackedPR("a", 1), trackedPR("b", 1), trackedPR("c", 2)} {
		if _, err := s.RecordOpenThreadPullRequest(row); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.ListThreadPullRequestsForPR("github", "github.com", "o/r", 1)
	if err != nil || len(rows) != 2 || rows[0].ThreadID != "a" || rows[1].ThreadID != "b" {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
}
