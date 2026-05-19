package store

import "testing"

func TestDiffReviewCommentsDraftLifecycle(t *testing.T) {
	s := newTestStore(t)
	thread := makeThread("diff-review-thread", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	created, err := s.CreateDiffReviewComment(DiffReviewComment{
		ID:           "comment-1",
		ThreadID:     thread.ID,
		Scope:        string(DiffReviewScopeWorkspace),
		SourceKey:    "diff-1",
		FilePath:     "app.ts",
		NewLine:      12,
		Side:         "new",
		SelectedText: "+next",
		Body:         "Use a clearer name.",
		CreatedAt:    100,
		UpdatedAt:    100,
	})
	if err != nil {
		t.Fatalf("CreateDiffReviewComment: %v", err)
	}
	if created.Status != "draft" || created.NewLine != 12 || created.Side != "new" {
		t.Fatalf("created = %+v", created)
	}

	updated, err := s.UpdateDiffReviewComment(thread.ID, created.ID, DiffReviewCommentUpdate{
		Body: "Use the domain term.",
	}, 200)
	if err != nil {
		t.Fatalf("UpdateDiffReviewComment: %v", err)
	}
	if updated.Body != "Use the domain term." || updated.UpdatedAt != 200 {
		t.Fatalf("updated = %+v", updated)
	}

	selected, err := s.ListDraftDiffReviewCommentsByID(thread.ID, string(DiffReviewScopeWorkspace), "diff-1", []string{created.ID})
	if err != nil {
		t.Fatalf("ListDraftDiffReviewCommentsByID: %v", err)
	}
	if len(selected) != 1 || selected[0].ID != created.ID {
		t.Fatalf("selected = %+v", selected)
	}

	if err := s.MarkDiffReviewCommentsSent(thread.ID, string(DiffReviewScopeWorkspace), "diff-1", []string{created.ID}, 300, "user:1"); err != nil {
		t.Fatalf("MarkDiffReviewCommentsSent: %v", err)
	}
	if err := s.MarkDiffReviewCommentsSent(thread.ID, string(DiffReviewScopeWorkspace), "diff-1", []string{created.ID}, 300, "user:1"); err != nil {
		t.Fatalf("MarkDiffReviewCommentsSent idempotent retry: %v", err)
	}
	sent, err := s.GetDiffReviewComment(thread.ID, created.ID)
	if err != nil {
		t.Fatalf("GetDiffReviewComment: %v", err)
	}
	if sent.Status != "sent" || sent.SentAt != 300 || sent.SentTurnID != "user:1" {
		t.Fatalf("sent = %+v", sent)
	}

	if err := s.DeleteOrResolveDiffReviewComment(thread.ID, created.ID, 400); err != nil {
		t.Fatalf("DeleteOrResolveDiffReviewComment: %v", err)
	}
	resolved, err := s.GetDiffReviewComment(thread.ID, created.ID)
	if err != nil {
		t.Fatalf("GetDiffReviewComment resolved: %v", err)
	}
	if resolved.Status != "resolved" || resolved.UpdatedAt != 400 {
		t.Fatalf("resolved = %+v", resolved)
	}
}

func TestDiffReviewCommentsRejectInvalidScopeAndAnchors(t *testing.T) {
	s := newTestStore(t)
	thread := makeThread("diff-review-invalid-thread", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	if _, err := s.CreateDiffReviewComment(DiffReviewComment{
		ID:        "bad-scope",
		ThreadID:  thread.ID,
		Scope:     "turn",
		SourceKey: "diff-1",
		FilePath:  "app.ts",
		Side:      "file",
		Body:      "Nope.",
	}); err == nil {
		t.Fatal("CreateDiffReviewComment accepted invalid scope")
	}

	if _, err := s.CreateDiffReviewComment(DiffReviewComment{
		ID:        "bad-file-anchor",
		ThreadID:  thread.ID,
		Scope:     string(DiffReviewScopeWorkspace),
		SourceKey: "diff-1",
		FilePath:  "app.ts",
		NewLine:   1,
		Side:      "file",
		Body:      "Nope.",
	}); err == nil {
		t.Fatal("CreateDiffReviewComment accepted file comment with line number")
	}
}

func TestListDraftDiffReviewCommentsByIDRejectsStaleIDs(t *testing.T) {
	s := newTestStore(t)
	thread := makeThread("diff-review-stale-thread", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	created, err := s.CreateDiffReviewComment(DiffReviewComment{
		ID:        "comment-1",
		ThreadID:  thread.ID,
		Scope:     string(DiffReviewScopeWorkspace),
		SourceKey: "diff-1",
		FilePath:  "app.ts",
		NewLine:   12,
		Side:      "new",
		Body:      "Use a clearer name.",
		CreatedAt: 100,
		UpdatedAt: 100,
	})
	if err != nil {
		t.Fatalf("CreateDiffReviewComment: %v", err)
	}

	if _, err := s.ListDraftDiffReviewCommentsByID(thread.ID, string(DiffReviewScopeWorkspace), "diff-1", []string{created.ID, "missing"}); err == nil {
		t.Fatal("ListDraftDiffReviewCommentsByID accepted a stale id")
	}
}
