package store

import (
	"testing"
	"time"
)

func TestCreateThreadWithNewFields(t *testing.T) {
	s := newTestStore(t)

	// First create a parent thread so the FK is valid.
	parent := makeThread("parent-1", "claude")
	parent.InteractionMode = "default"
	if err := s.CreateThread(parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}

	now := time.Now().UnixMilli()
	thr := Thread{
		ID:              "thread-new-fields",
		Title:           "Full Thread",
		Provider:        "claude",
		SessionRef:      "sess-123",
		WorkspacePath:   "/home/user/project",
		Model:           "opus-4",
		ProjectPath:     "/home/user/project/sub",
		WorktreePath:    "/home/user/.worktrees/feat-x",
		Branch:          "feat-x",
		InteractionMode: "plan",
		DiscussionID:    "disc-abc",
		ParentThreadID:  "parent-1",
		CreatedAt:       now,
		UpdatedAt:       now,
		Archived:        false,
	}

	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	got, err := s.GetThread("thread-new-fields")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}

	if got.ProjectPath != thr.ProjectPath {
		t.Errorf("ProjectPath: got %q, want %q", got.ProjectPath, thr.ProjectPath)
	}
	if got.WorktreePath != thr.WorktreePath {
		t.Errorf("WorktreePath: got %q, want %q", got.WorktreePath, thr.WorktreePath)
	}
	if got.Branch != thr.Branch {
		t.Errorf("Branch: got %q, want %q", got.Branch, thr.Branch)
	}
	if got.InteractionMode != thr.InteractionMode {
		t.Errorf("InteractionMode: got %q, want %q", got.InteractionMode, thr.InteractionMode)
	}
	if got.DiscussionID != thr.DiscussionID {
		t.Errorf("DiscussionID: got %q, want %q", got.DiscussionID, thr.DiscussionID)
	}
	if got.ParentThreadID != thr.ParentThreadID {
		t.Errorf("ParentThreadID: got %q, want %q", got.ParentThreadID, thr.ParentThreadID)
	}

	// Verify existing fields still round-trip correctly.
	if got.ID != thr.ID {
		t.Errorf("ID: got %q, want %q", got.ID, thr.ID)
	}
	if got.SessionRef != thr.SessionRef {
		t.Errorf("SessionRef: got %q, want %q", got.SessionRef, thr.SessionRef)
	}
	if got.Provider != thr.Provider {
		t.Errorf("Provider: got %q, want %q", got.Provider, thr.Provider)
	}
}

func TestCreateThreadDefaultValues(t *testing.T) {
	s := newTestStore(t)

	now := time.Now().UnixMilli()
	thr := Thread{
		ID:            "thread-minimal",
		Title:         "Minimal Thread",
		Provider:      "codex",
		WorkspacePath: "/tmp/ws",
		Model:         "gpt-4.1",
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.GetThread("thread-minimal")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if got.InteractionMode != "default" {
		t.Errorf("InteractionMode: got %q, want default", got.InteractionMode)
	}
	if got.ProjectPath != "" {
		t.Errorf("ProjectPath: got %q, want empty string", got.ProjectPath)
	}

	// Nullable columns should come back as empty string via COALESCE.
	if got.WorktreePath != "" {
		t.Errorf("WorktreePath: got %q, want empty string", got.WorktreePath)
	}
	if got.Branch != "" {
		t.Errorf("Branch: got %q, want empty string", got.Branch)
	}
	if got.DiscussionID != "" {
		t.Errorf("DiscussionID: got %q, want empty string", got.DiscussionID)
	}
	if got.ParentThreadID != "" {
		t.Errorf("ParentThreadID: got %q, want empty string", got.ParentThreadID)
	}
}

func TestUpdateThreadNewFields(t *testing.T) {
	s := newTestStore(t)

	now := time.Now().UnixMilli()
	thr := Thread{
		ID:            "thread-upd",
		Title:         "Before Update",
		Provider:      "claude",
		WorkspacePath: "/tmp/ws",
		Model:         "opus-4",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Update with new field values.
	thr.Title = "After Update"
	thr.ProjectPath = "/home/user/project"
	thr.WorktreePath = "/home/user/.worktrees/fix-123"
	thr.Branch = "fix-123"
	thr.InteractionMode = "plan"
	thr.DiscussionID = "disc-xyz"
	thr.UpdatedAt = now + 5000

	if err := s.UpdateThread(thr); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err := s.GetThread("thread-upd")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if got.Title != "After Update" {
		t.Errorf("Title: got %q, want %q", got.Title, "After Update")
	}
	if got.ProjectPath != "/home/user/project" {
		t.Errorf("ProjectPath: got %q, want %q", got.ProjectPath, "/home/user/project")
	}
	if got.WorktreePath != "/home/user/.worktrees/fix-123" {
		t.Errorf("WorktreePath: got %q, want %q", got.WorktreePath, "/home/user/.worktrees/fix-123")
	}
	if got.Branch != "fix-123" {
		t.Errorf("Branch: got %q, want %q", got.Branch, "fix-123")
	}
	if got.InteractionMode != "plan" {
		t.Errorf("InteractionMode: got %q, want %q", got.InteractionMode, "plan")
	}
	if got.DiscussionID != "disc-xyz" {
		t.Errorf("DiscussionID: got %q, want %q", got.DiscussionID, "disc-xyz")
	}

	// Verify clearing a nullable field back to empty works.
	thr.WorktreePath = ""
	thr.Branch = ""
	thr.UpdatedAt = now + 10000
	if err := s.UpdateThread(thr); err != nil {
		t.Fatalf("update (clear): %v", err)
	}

	got2, err := s.GetThread("thread-upd")
	if err != nil {
		t.Fatalf("get after clear: %v", err)
	}
	if got2.WorktreePath != "" {
		t.Errorf("WorktreePath after clear: got %q, want empty", got2.WorktreePath)
	}
	if got2.Branch != "" {
		t.Errorf("Branch after clear: got %q, want empty", got2.Branch)
	}
}

func TestListThreadsIncludesNewFields(t *testing.T) {
	s := newTestStore(t)

	now := time.Now().UnixMilli()
	thr := Thread{
		ID:              "thread-list",
		Title:           "Listed Thread",
		Provider:        "claude",
		WorkspacePath:   "/tmp/ws",
		Model:           "opus-4",
		ProjectPath:     "/home/user/proj",
		Branch:          "main",
		InteractionMode: "design",
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create: %v", err)
	}

	threads, err := s.ListThreads()
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(threads) != 1 {
		t.Fatalf("expected 1 thread, got %d", len(threads))
	}

	got := threads[0]
	if got.ProjectPath != "/home/user/proj" {
		t.Errorf("ProjectPath: got %q, want %q", got.ProjectPath, "/home/user/proj")
	}
	if got.Branch != "main" {
		t.Errorf("Branch: got %q, want %q", got.Branch, "main")
	}
	if got.InteractionMode != "design" {
		t.Errorf("InteractionMode: got %q, want %q", got.InteractionMode, "design")
	}
	// Unset nullable fields should be empty.
	if got.WorktreePath != "" {
		t.Errorf("WorktreePath: got %q, want empty", got.WorktreePath)
	}
	if got.DiscussionID != "" {
		t.Errorf("DiscussionID: got %q, want empty", got.DiscussionID)
	}
	if got.ParentThreadID != "" {
		t.Errorf("ParentThreadID: got %q, want empty", got.ParentThreadID)
	}
}

func TestListChildThreadsReturnsOnlyDirectChildren(t *testing.T) {
	s := newTestStore(t)

	parent := makeThread("parent-thread", "claude")
	if err := s.CreateThread(parent); err != nil {
		t.Fatalf("CreateThread(parent): %v", err)
	}

	childA := makeThread("child-a", "claude")
	childA.ParentThreadID = parent.ID
	childA.CreatedAt = parent.CreatedAt + 1
	childA.UpdatedAt = childA.CreatedAt
	if err := s.CreateThread(childA); err != nil {
		t.Fatalf("CreateThread(childA): %v", err)
	}

	childB := makeThread("child-b", "codex")
	childB.ParentThreadID = parent.ID
	childB.CreatedAt = parent.CreatedAt + 2
	childB.UpdatedAt = childB.CreatedAt
	if err := s.CreateThread(childB); err != nil {
		t.Fatalf("CreateThread(childB): %v", err)
	}

	other := makeThread("other-thread", "claude")
	if err := s.CreateThread(other); err != nil {
		t.Fatalf("CreateThread(other): %v", err)
	}

	children, err := s.ListChildThreads(parent.ID)
	if err != nil {
		t.Fatalf("ListChildThreads(): %v", err)
	}
	if len(children) != 2 {
		t.Fatalf("len(children) = %d, want 2", len(children))
	}
	if children[0].ID != childA.ID || children[1].ID != childB.ID {
		t.Fatalf("children order/IDs = %q, %q; want %q, %q", children[0].ID, children[1].ID, childA.ID, childB.ID)
	}
}
