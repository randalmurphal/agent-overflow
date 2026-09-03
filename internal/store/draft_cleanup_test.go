package store

import (
	"testing"
	"time"
)

func createDraftCleanupThread(t *testing.T, s *Store, id, mode string) Thread {
	t.Helper()
	thread := makeThread(id, "claude")
	thread.Mode = mode
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread(%s): %v", id, err)
	}
	return thread
}

func threadRowExists(t *testing.T, s *Store, threadID string) bool {
	t.Helper()
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM threads WHERE id = ?`, threadID).Scan(&count); err != nil {
		t.Fatalf("count thread %s: %v", threadID, err)
	}
	return count > 0
}

func TestDeleteEmptyDraftThreadDeletesEmptyChatDraft(t *testing.T) {
	s := newTestStore(t)
	thread := createDraftCleanupThread(t, s, "empty-chat-draft", "chat")
	if _, err := s.UpsertThreadDraft(ThreadDraft{
		ThreadID:      thread.ID,
		Content:       "   ",
		Attachments:   "[]",
		TerminalChips: "[]",
		UpdatedAt:     time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("UpsertThreadDraft: %v", err)
	}

	deleted, err := s.DeleteEmptyDraftThread(thread.ID)
	if err != nil {
		t.Fatalf("DeleteEmptyDraftThread: %v", err)
	}
	if !deleted {
		t.Fatal("empty chat draft should be deleted")
	}
	if threadRowExists(t, s, thread.ID) {
		t.Fatal("thread row still exists after delete")
	}
}

func TestDeleteEmptyDraftThreadDeletesEmptyPlanDraft(t *testing.T) {
	s := newTestStore(t)
	thread := createDraftCleanupThread(t, s, "empty-plan-draft", "plan")

	empty, err := s.IsEmptyDraftThread(thread.ID)
	if err != nil {
		t.Fatalf("IsEmptyDraftThread: %v", err)
	}
	if !empty {
		t.Fatal("plan draft should be classified as empty before delete")
	}

	deleted, err := s.DeleteEmptyDraftThread(thread.ID)
	if err != nil {
		t.Fatalf("DeleteEmptyDraftThread: %v", err)
	}
	if !deleted {
		t.Fatal("empty plan draft should be deleted")
	}
}

func TestDeleteEmptyDraftThreadKeepsDraftWithContent(t *testing.T) {
	s := newTestStore(t)
	thread := createDraftCleanupThread(t, s, "draft-with-content", "chat")
	if _, err := s.UpsertThreadDraft(ThreadDraft{
		ThreadID:      thread.ID,
		Content:       "do not delete",
		Attachments:   "[]",
		TerminalChips: "[]",
		UpdatedAt:     time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("UpsertThreadDraft: %v", err)
	}

	deleted, err := s.DeleteEmptyDraftThread(thread.ID)
	if err != nil {
		t.Fatalf("DeleteEmptyDraftThread: %v", err)
	}
	if deleted {
		t.Fatal("draft with content should not be deleted")
	}
	if !threadRowExists(t, s, thread.ID) {
		t.Fatal("thread row was deleted")
	}
}

func TestDeleteEmptyDraftThreadKeepsThreadWithChildren(t *testing.T) {
	s := newTestStore(t)
	parent := createDraftCleanupThread(t, s, "draft-with-child", "chat")
	child := makeThread("child-thread", "claude")
	child.ProjectID = parent.ProjectID
	child.ParentThreadID = parent.ID
	if err := s.CreateThread(child); err != nil {
		t.Fatalf("CreateThread(child): %v", err)
	}

	empty, err := s.IsEmptyDraftThread(parent.ID)
	if err != nil {
		t.Fatalf("IsEmptyDraftThread: %v", err)
	}
	if empty {
		t.Fatal("thread with child should not be classified as empty")
	}
	deleted, err := s.DeleteEmptyDraftThread(parent.ID)
	if err != nil {
		t.Fatalf("DeleteEmptyDraftThread: %v", err)
	}
	if deleted {
		t.Fatal("thread with child should not be deleted")
	}
	if !threadRowExists(t, s, parent.ID) {
		t.Fatal("parent thread row was deleted")
	}
}

func TestDeleteEmptyDraftThreadKeepsThreadWithItems(t *testing.T) {
	s := newTestStore(t)
	thread := createDraftCleanupThread(t, s, "draft-with-items", "chat")
	if _, err := s.AppendItem(Item{
		ID:        "item-1",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   "sent",
		CreatedAt: time.Now().UnixMilli(),
		UpdatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("AppendItem: %v", err)
	}

	deleted, err := s.DeleteEmptyDraftThread(thread.ID)
	if err != nil {
		t.Fatalf("DeleteEmptyDraftThread: %v", err)
	}
	if deleted {
		t.Fatal("thread with items should not be deleted")
	}
	if !threadRowExists(t, s, thread.ID) {
		t.Fatal("thread row was deleted")
	}
}

func TestDeleteEmptyDraftThreadKeepsThreadWithTurns(t *testing.T) {
	s := newTestStore(t)
	thread := createDraftCleanupThread(t, s, "draft-with-turn", "chat")
	if err := s.InsertTurn(Turn{
		TurnID:    "turn-1",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		StartedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}

	deleted, err := s.DeleteEmptyDraftThread(thread.ID)
	if err != nil {
		t.Fatalf("DeleteEmptyDraftThread: %v", err)
	}
	if deleted {
		t.Fatal("thread with turns should not be deleted")
	}
	if !threadRowExists(t, s, thread.ID) {
		t.Fatal("thread row was deleted")
	}
}

func TestDeleteEmptyDraftThreadKeepsThreadWithWorktree(t *testing.T) {
	s := newTestStore(t)
	thread := createDraftCleanupThread(t, s, "draft-with-worktree", "chat")
	thread.WorktreePath = "/tmp/worktrees/draft-with-worktree"
	thread.WorkspacePath = thread.WorktreePath
	if err := s.UpdateThread(thread); err != nil {
		t.Fatalf("UpdateThread: %v", err)
	}

	empty, err := s.IsEmptyDraftThread(thread.ID)
	if err != nil {
		t.Fatalf("IsEmptyDraftThread: %v", err)
	}
	if empty {
		t.Fatal("thread with a worktree should not be classified as empty — deleting it would orphan the checkout")
	}
	deleted, err := s.DeleteEmptyDraftThread(thread.ID)
	if err != nil {
		t.Fatalf("DeleteEmptyDraftThread: %v", err)
	}
	if deleted {
		t.Fatal("thread with a worktree should not be deleted")
	}
	if !threadRowExists(t, s, thread.ID) {
		t.Fatal("thread row was deleted")
	}
}

func TestDeleteEmptyDraftThreadKeepsDiscussionMode(t *testing.T) {
	s := newTestStore(t)
	thread := createDraftCleanupThread(t, s, "discussion-draft", "discussion")

	deleted, err := s.DeleteEmptyDraftThread(thread.ID)
	if err != nil {
		t.Fatalf("DeleteEmptyDraftThread: %v", err)
	}
	if deleted {
		t.Fatal("discussion draft should not be deleted")
	}
	if !threadRowExists(t, s, thread.ID) {
		t.Fatal("thread row was deleted")
	}
}
