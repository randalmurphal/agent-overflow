package store

import (
	"strings"
	"testing"
)

// recordReviewSent marks owner's drafts ids sent at at in turn as a posted
// review does, under a submit id of its own.
func recordReviewSent(t *testing.T, s *Store, owner DiffReviewOwner, scope, sourceKey string, ids []string, at int64, turn string) {
	t.Helper()
	submit := PRReviewSubmit{SubmitID: "submit-" + owner.String() + "-" + strings.Join(ids, ","), PRKey: "github:o/r:1", Owner: owner, Result: []byte(`{}`), CreatedAt: at}
	if err := s.RecordPRReviewSubmit(submit, PRReviewSubmitSent{Scope: scope, SourceKey: sourceKey, CommentIDs: ids, SentTurnID: turn}); err != nil {
		t.Fatal(err)
	}
}

// v148 creates pr_review_submits, whose rows name one owner and a JSON
// result.
func TestPRReviewSubmitsMigrationAddsTheTable(t *testing.T) {
	db := migrateThrough(t, prReviewSubmitsMigrationVersion-1)
	if err := applyMigration(db, migrationByVersion(t, prReviewSubmitsMigrationVersion)); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO pr_review_submits(submit_id,pr_key,owner_thread_id,result,created_at) VALUES ('s1','github:o/r:1','t1','{}',1)`)
	mustExec(t, db, `INSERT INTO pr_review_submits(submit_id,pr_key,owner_pr_key,result,created_at) VALUES ('s2','github:o/r:1','github:o/r:1','{}',1)`)
	for name, insert := range map[string]string{
		"repeated id": `INSERT INTO pr_review_submits(submit_id,pr_key,owner_thread_id,result,created_at) VALUES ('s1','github:o/r:1','t1','{}',2)`,
		"no owner":    `INSERT INTO pr_review_submits(submit_id,pr_key,result,created_at) VALUES ('s3','github:o/r:1','{}',1)`,
		"two owners":  `INSERT INTO pr_review_submits(submit_id,pr_key,owner_thread_id,owner_pr_key,result,created_at) VALUES ('s4','k','t1','k','{}',1)`,
		"no JSON":     `INSERT INTO pr_review_submits(submit_id,pr_key,owner_thread_id,result,created_at) VALUES ('s5','k','t1','{',1)`,
		"empty id":    `INSERT INTO pr_review_submits(submit_id,pr_key,owner_thread_id,result,created_at) VALUES ('','k','t1','{}',1)`,
	} {
		if _, err := db.Exec(insert); err == nil {
			t.Errorf("%s: stored", name)
		}
	}
}

// A recorded submit marks exactly its owner's drafts it names sent, with
// its record, in one transaction: a repeated id stores nothing and marks
// nothing, and outcomes past retention go.
func TestRecordPRReviewSubmitMarksDraftsWithItsOutcome(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	owner := PRReviewOwner("github:o/r:5")
	draft := func(path string) DiffReviewComment {
		created, err := s.CreateDiffReviewComment(DiffReviewComment{
			ID: "c-" + path, PRKey: owner.PRKey, Scope: "pr", SourceKey: "pr:base..head", FilePath: path, Side: "new", NewLine: 1, Body: "b", Status: "draft",
			CreatedAt: 1, UpdatedAt: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	a, b, c := draft("a.go"), draft("b.go"), draft("c.go")
	sent := PRReviewSubmitSent{Scope: "pr", SourceKey: "pr:base..head", CommentIDs: []string{a.ID, b.ID}}
	submit := PRReviewSubmit{SubmitID: "submit-1", PRKey: owner.PRKey, Owner: owner, Result: []byte(`{"postedReview":true}`), CreatedAt: 1_000}
	if err := s.RecordPRReviewSubmit(submit, sent); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetPRReviewSubmit("submit-1")
	if err != nil || !ok || string(got.Result) != `{"postedReview":true}` || got.Owner != owner || got.PRKey != owner.PRKey {
		t.Fatalf("recorded = %+v, %v, %v", got, ok, err)
	}
	status := func(id string) string {
		comment, err := s.GetDiffReviewComment(owner, id)
		if err != nil {
			t.Fatal(err)
		}
		return comment.Status
	}
	if status(a.ID) != "sent" || status(b.ID) != "sent" || status(c.ID) != "draft" {
		t.Fatalf("statuses = %s %s %s", status(a.ID), status(b.ID), status(c.ID))
	}
	// The same id again: refused, and c stays a draft.
	if err := s.RecordPRReviewSubmit(submit, PRReviewSubmitSent{Scope: "pr", SourceKey: "pr:base..head", CommentIDs: []string{c.ID}}); err == nil {
		t.Fatal("a repeated submit id was recorded")
	}
	if status(c.ID) != "draft" {
		t.Fatal("a refused record marked a draft sent")
	}
	if _, ok, _ := s.GetPRReviewSubmit("other"); ok {
		t.Fatal("an unknown id answered")
	}
	// A later record drops outcomes past retention.
	late := PRReviewSubmit{SubmitID: "submit-2", PRKey: owner.PRKey, Owner: owner, Result: []byte(`{}`), CreatedAt: 1_000 + prReviewSubmitRetention.Milliseconds() + 1}
	if err := s.RecordPRReviewSubmit(late, PRReviewSubmitSent{}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.GetPRReviewSubmit("submit-1"); ok {
		t.Fatal("an outcome past retention stayed")
	}
	if err := s.RecordPRReviewSubmit(PRReviewSubmit{SubmitID: "", PRKey: "k", Owner: owner, Result: []byte(`{}`), CreatedAt: 1}, PRReviewSubmitSent{}); err == nil {
		t.Fatal("an empty submit id was recorded")
	}
}
