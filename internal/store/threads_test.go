package store

import (
	"database/sql"
	"errors"
	"testing"
	"time"
)

// newTestProject inserts a fresh project row and returns it. Threads in
// v13+ require a project_id FK; keeping a per-test helper here avoids
// reaching into internal/testutil from the store package.
func newTestProject(t *testing.T, s *Store, id, path string) Project {
	t.Helper()
	now := time.Now().UnixMilli()
	p := Project{
		ID:        id,
		Path:      path,
		Name:      path,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.CreateProject(p); err != nil {
		t.Fatalf("CreateProject(%s): %v", id, err)
	}
	return p
}

func TestCreateThreadWithNewFields(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-new-fields", "/home/user/project")

	// First create a parent thread so the FK is valid.
	parent := makeThread("parent-1", "claude")
	parent.ProjectID = proj.ID
	parent.Mode = "chat"
	if err := s.CreateThread(parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}

	now := time.Now().UnixMilli()
	thr := Thread{
		ID:                 "thread-new-fields",
		ProjectID:          proj.ID,
		Title:              "Full Thread",
		Provider:           "claude",
		SessionRef:         "sess-123",
		PendingForkRef:     "sess-pending",
		WorkspacePath:      "/home/user/project",
		Model:              "opus-4",
		WorktreePath:       "/home/user/.worktrees/feat-x",
		Branch:             "feat-x",
		Mode:               "plan",
		ReasoningEffort:    "xhigh",
		FastMode:           true,
		ContextWindow:      200000,
		DiscussionID:       "disc-abc",
		ParentThreadID:     "parent-1",
		ForkedFromThreadID: "parent-1",
		CreatedAt:          now,
		UpdatedAt:          now,
		Archived:           false,
	}

	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("create thread: %v", err)
	}

	got, err := s.GetThread("thread-new-fields")
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}

	if got.ProjectID != proj.ID {
		t.Errorf("ProjectID: got %q, want %q", got.ProjectID, proj.ID)
	}
	if got.WorktreePath != thr.WorktreePath {
		t.Errorf("WorktreePath: got %q, want %q", got.WorktreePath, thr.WorktreePath)
	}
	if got.Branch != thr.Branch {
		t.Errorf("Branch: got %q, want %q", got.Branch, thr.Branch)
	}
	if got.Mode != thr.Mode {
		t.Errorf("Mode: got %q, want %q", got.Mode, thr.Mode)
	}
	if got.ReasoningEffort != thr.ReasoningEffort {
		t.Errorf("ReasoningEffort: got %q, want %q", got.ReasoningEffort, thr.ReasoningEffort)
	}
	if got.FastMode != thr.FastMode {
		t.Errorf("FastMode: got %v, want %v", got.FastMode, thr.FastMode)
	}
	if got.ContextWindow != thr.ContextWindow {
		t.Errorf("ContextWindow: got %d, want %d", got.ContextWindow, thr.ContextWindow)
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
	if got.PendingForkRef != thr.PendingForkRef {
		t.Errorf("PendingForkRef: got %q, want %q", got.PendingForkRef, thr.PendingForkRef)
	}
	if got.Provider != thr.Provider {
		t.Errorf("Provider: got %q, want %q", got.Provider, thr.Provider)
	}
	if got.ForkedFromThreadID != thr.ForkedFromThreadID {
		t.Errorf("ForkedFromThreadID: got %q, want %q", got.ForkedFromThreadID, thr.ForkedFromThreadID)
	}
}

func TestCreateThreadDefaultValues(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-minimal", "/tmp/ws")

	now := time.Now().UnixMilli()
	thr := Thread{
		ID:            "thread-minimal",
		ProjectID:     proj.ID,
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

	if got.Mode != "chat" {
		t.Errorf("Mode: got %q, want chat", got.Mode)
	}
	if got.ReasoningEffort != "high" {
		t.Errorf("ReasoningEffort: got %q, want high (default)", got.ReasoningEffort)
	}
	if got.ContextWindow != 1000000 {
		t.Errorf("ContextWindow: got %d, want 1000000 (default)", got.ContextWindow)
	}
	if got.FastMode {
		t.Errorf("FastMode: got true, want false (default)")
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
	if got.PendingForkRef != "" {
		t.Errorf("PendingForkRef: got %q, want empty string", got.PendingForkRef)
	}
	if got.ForkedFromThreadID != "" {
		t.Errorf("ForkedFromThreadID: got %q, want empty string", got.ForkedFromThreadID)
	}
}

func TestUpdateThreadNewFields(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-upd", "/tmp/ws")

	now := time.Now().UnixMilli()
	thr := Thread{
		ID:            "thread-upd",
		ProjectID:     proj.ID,
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

	parent := makeThread("thread-upd-parent", "claude")
	parent.ProjectID = proj.ID
	if err := s.CreateThread(parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}

	// Update with new field values.
	thr.Title = "After Update"
	thr.WorktreePath = "/home/user/.worktrees/fix-123"
	thr.Branch = "fix-123"
	thr.Mode = "plan"
	thr.DiscussionID = "disc-xyz"
	thr.PendingForkRef = "fork-pending"
	thr.ForkedFromThreadID = "thread-upd-parent"
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
	if got.WorktreePath != "/home/user/.worktrees/fix-123" {
		t.Errorf("WorktreePath: got %q, want %q", got.WorktreePath, "/home/user/.worktrees/fix-123")
	}
	if got.Branch != "fix-123" {
		t.Errorf("Branch: got %q, want %q", got.Branch, "fix-123")
	}
	if got.Mode != "plan" {
		t.Errorf("Mode: got %q, want %q", got.Mode, "plan")
	}
	if got.DiscussionID != "disc-xyz" {
		t.Errorf("DiscussionID: got %q, want %q", got.DiscussionID, "disc-xyz")
	}
	if got.PendingForkRef != "fork-pending" {
		t.Errorf("PendingForkRef: got %q, want %q", got.PendingForkRef, "fork-pending")
	}
	if got.ForkedFromThreadID != "thread-upd-parent" {
		t.Errorf("ForkedFromThreadID: got %q, want %q", got.ForkedFromThreadID, "thread-upd-parent")
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

func TestUpdateSessionRefClearsPendingForkRef(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-pending", "/tmp/p")

	thread := makeThread("thread-clear-pending", "claude")
	thread.ProjectID = proj.ID
	thread.PendingForkRef = "pending-123"
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	if err := s.UpdateSessionRef(thread.ID, "session-456"); err != nil {
		t.Fatalf("UpdateSessionRef() error = %v", err)
	}

	got, err := s.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if got.SessionRef != "session-456" {
		t.Fatalf("SessionRef = %q, want %q", got.SessionRef, "session-456")
	}
	if got.PendingForkRef != "" {
		t.Fatalf("PendingForkRef = %q, want empty", got.PendingForkRef)
	}
}

func TestListThreadsIncludesNewFields(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-list", "/home/user/proj")

	now := time.Now().UnixMilli()
	thr := Thread{
		ID:            "thread-list",
		ProjectID:     proj.ID,
		Title:         "Listed Thread",
		Provider:      "claude",
		WorkspacePath: "/home/user/proj",
		Model:         "opus-4",
		Branch:        "main",
		Mode:          "design",
		CreatedAt:     now,
		UpdatedAt:     now,
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
	if got.ProjectID != proj.ID {
		t.Errorf("ProjectID: got %q, want %q", got.ProjectID, proj.ID)
	}
	if got.Branch != "main" {
		t.Errorf("Branch: got %q, want %q", got.Branch, "main")
	}
	if got.Mode != "design" {
		t.Errorf("Mode: got %q, want %q", got.Mode, "design")
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
	proj := newTestProject(t, s, "proj-children", "/tmp/children")

	parent := makeThread("parent-thread", "claude")
	parent.ProjectID = proj.ID
	if err := s.CreateThread(parent); err != nil {
		t.Fatalf("CreateThread(parent): %v", err)
	}

	childA := makeThread("child-a", "claude")
	childA.ProjectID = proj.ID
	childA.ParentThreadID = parent.ID
	childA.CreatedAt = parent.CreatedAt + 1
	childA.UpdatedAt = childA.CreatedAt
	if err := s.CreateThread(childA); err != nil {
		t.Fatalf("CreateThread(childA): %v", err)
	}

	childB := makeThread("child-b", "codex")
	childB.ProjectID = proj.ID
	childB.ParentThreadID = parent.ID
	childB.CreatedAt = parent.CreatedAt + 2
	childB.UpdatedAt = childB.CreatedAt
	if err := s.CreateThread(childB); err != nil {
		t.Fatalf("CreateThread(childB): %v", err)
	}

	other := makeThread("other-thread", "claude")
	other.ProjectID = proj.ID
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

func TestUpdateModePersists(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-mode", "/tmp/mode")

	thr := makeThread("thread-mode", "claude")
	thr.ProjectID = proj.ID
	thr.Mode = "chat"
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	if err := s.UpdateMode(thr.ID, "plan"); err != nil {
		t.Fatalf("UpdateMode() error = %v", err)
	}

	got, err := s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if got.Mode != "plan" {
		t.Fatalf("Mode = %q, want plan", got.Mode)
	}
}

func TestUpdateModeNormalizesEmptyToChat(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-mode-empty", "/tmp/mode-empty")

	thr := makeThread("thread-mode-empty", "claude")
	thr.ProjectID = proj.ID
	thr.Mode = "plan"
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	if err := s.UpdateMode(thr.ID, ""); err != nil {
		t.Fatalf("UpdateMode() error = %v", err)
	}

	got, err := s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if got.Mode != "chat" {
		t.Fatalf("Mode = %q, want chat (normalized)", got.Mode)
	}
}

func TestUpdateModeRejectsInvalidValue(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-mode-invalid", "/tmp/mi")

	thr := makeThread("thread-mode-invalid", "claude")
	thr.ProjectID = proj.ID
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("CreateThread(): %v", err)
	}

	if err := s.UpdateMode(thr.ID, "bogus"); !errors.Is(err, ErrInvalidMode) {
		t.Fatalf("UpdateMode(bogus) error = %v, want ErrInvalidMode", err)
	}
}

func TestUpdateModeReturnsNotFoundForMissing(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpdateMode("nonexistent", "plan"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("UpdateMode() error = %v, want sql.ErrNoRows", err)
	}
}

func TestUpdateModeBumpsUpdatedAt(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-mode-ua", "/tmp/ua")

	thr := makeThread("thread-mode-ua", "claude")
	thr.ProjectID = proj.ID
	thr.UpdatedAt = time.Now().UnixMilli() - 10_000
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	if err := s.UpdateMode(thr.ID, "design"); err != nil {
		t.Fatalf("UpdateMode() error = %v", err)
	}

	got, err := s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if got.UpdatedAt <= thr.UpdatedAt {
		t.Fatalf("UpdatedAt = %d, want > %d (should have been bumped)", got.UpdatedAt, thr.UpdatedAt)
	}
}

func TestUpdateReasoningEffortPersists(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-effort", "/tmp/effort")

	thr := makeThread("thread-effort", "claude")
	thr.ProjectID = proj.ID
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("CreateThread(): %v", err)
	}

	if err := s.UpdateReasoningEffort(thr.ID, "xhigh"); err != nil {
		t.Fatalf("UpdateReasoningEffort(): %v", err)
	}
	got, err := s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("GetThread(): %v", err)
	}
	if got.ReasoningEffort != "xhigh" {
		t.Fatalf("ReasoningEffort = %q, want xhigh", got.ReasoningEffort)
	}
}

func TestUpdateReasoningEffortRejectsUnknown(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-effort-bad", "/tmp/eb")

	thr := makeThread("thread-effort-bad", "claude")
	thr.ProjectID = proj.ID
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("CreateThread(): %v", err)
	}

	if err := s.UpdateReasoningEffort(thr.ID, "ultranope"); !errors.Is(err, ErrInvalidEffort) {
		t.Fatalf("UpdateReasoningEffort(ultranope) error = %v, want ErrInvalidEffort", err)
	}
}

func TestUpdateFastModePersists(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-fast", "/tmp/fast")

	thr := makeThread("thread-fast", "claude")
	thr.ProjectID = proj.ID
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("CreateThread(): %v", err)
	}

	if err := s.UpdateFastMode(thr.ID, true); err != nil {
		t.Fatalf("UpdateFastMode(true): %v", err)
	}
	got, _ := s.GetThread(thr.ID)
	if !got.FastMode {
		t.Fatalf("FastMode = false, want true")
	}

	if err := s.UpdateFastMode(thr.ID, false); err != nil {
		t.Fatalf("UpdateFastMode(false): %v", err)
	}
	got, _ = s.GetThread(thr.ID)
	if got.FastMode {
		t.Fatalf("FastMode = true, want false")
	}
}

func TestUpdateContextWindowValid(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-cw", "/tmp/cw")

	thr := makeThread("thread-cw", "claude")
	thr.ProjectID = proj.ID
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("CreateThread(): %v", err)
	}

	if err := s.UpdateContextWindow(thr.ID, 200000); err != nil {
		t.Fatalf("UpdateContextWindow(200000): %v", err)
	}
	got, _ := s.GetThread(thr.ID)
	if got.ContextWindow != 200000 {
		t.Fatalf("ContextWindow = %d, want 200000", got.ContextWindow)
	}
}

func TestUpdateContextWindowInvalid(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-cw-bad", "/tmp/cwb")

	thr := makeThread("thread-cw-bad", "claude")
	thr.ProjectID = proj.ID
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("CreateThread(): %v", err)
	}

	if err := s.UpdateContextWindow(thr.ID, 500000); !errors.Is(err, ErrInvalidContextWindow) {
		t.Fatalf("UpdateContextWindow(500000) = %v, want ErrInvalidContextWindow", err)
	}
}

func TestUpdateProviderValid(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-prov", "/tmp/prov")

	thr := makeThread("thread-prov", "claude")
	thr.ProjectID = proj.ID
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("CreateThread(): %v", err)
	}

	if err := s.UpdateProvider(thr.ID, "codex"); err != nil {
		t.Fatalf("UpdateProvider(codex): %v", err)
	}
	got, _ := s.GetThread(thr.ID)
	if got.Provider != "codex" {
		t.Fatalf("Provider = %q, want codex", got.Provider)
	}
}

func TestUpdateProviderInvalid(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-prov-bad", "/tmp/pb")

	thr := makeThread("thread-prov-bad", "claude")
	thr.ProjectID = proj.ID
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("CreateThread(): %v", err)
	}

	if err := s.UpdateProvider(thr.ID, "bing"); !errors.Is(err, ErrInvalidProvider) {
		t.Fatalf("UpdateProvider(bing) = %v, want ErrInvalidProvider", err)
	}
}

func TestUpdateBranchPersists(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-branch", "/tmp/branch")

	thr := makeThread("thread-branch", "claude")
	thr.ProjectID = proj.ID
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("CreateThread(): %v", err)
	}

	if err := s.UpdateBranch(thr.ID, "feat/abc"); err != nil {
		t.Fatalf("UpdateBranch(): %v", err)
	}
	got, _ := s.GetThread(thr.ID)
	if got.Branch != "feat/abc" {
		t.Fatalf("Branch = %q, want feat/abc", got.Branch)
	}

	// Empty string clears the column.
	if err := s.UpdateBranch(thr.ID, ""); err != nil {
		t.Fatalf("UpdateBranch(empty): %v", err)
	}
	got, _ = s.GetThread(thr.ID)
	if got.Branch != "" {
		t.Fatalf("Branch = %q, want empty", got.Branch)
	}
}

func TestUpdateWorkspacePathPersists(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-ws", "/tmp/ws")

	thr := makeThread("thread-ws", "claude")
	thr.ProjectID = proj.ID
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("CreateThread(): %v", err)
	}

	if err := s.UpdateWorkspacePath(thr.ID, "/tmp/alt"); err != nil {
		t.Fatalf("UpdateWorkspacePath(): %v", err)
	}
	got, _ := s.GetThread(thr.ID)
	if got.WorkspacePath != "/tmp/alt" {
		t.Fatalf("WorkspacePath = %q, want /tmp/alt", got.WorkspacePath)
	}
}

func TestListThreadsByProject(t *testing.T) {
	s := newTestStore(t)
	projA := newTestProject(t, s, "proj-a", "/tmp/a")
	projB := newTestProject(t, s, "proj-b", "/tmp/b")

	threadA1 := makeThread("a1", "claude")
	threadA1.ProjectID = projA.ID
	threadA2 := makeThread("a2", "claude")
	threadA2.ProjectID = projA.ID
	threadB1 := makeThread("b1", "codex")
	threadB1.ProjectID = projB.ID
	for _, t2 := range []Thread{threadA1, threadA2, threadB1} {
		if err := s.CreateThread(t2); err != nil {
			t.Fatalf("CreateThread(%s): %v", t2.ID, err)
		}
	}

	got, err := s.ListThreadsByProject(projA.ID)
	if err != nil {
		t.Fatalf("ListThreadsByProject(): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	for _, th := range got {
		if th.ProjectID != projA.ID {
			t.Errorf("thread %s project = %q, want %q", th.ID, th.ProjectID, projA.ID)
		}
	}
}

func TestListThreadsWithItemsHidesEmptyDrafts(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-drafts", "/tmp/d")

	now := time.Now().UnixMilli()
	withItems := Thread{
		ID:            "thread-with-items",
		ProjectID:     proj.ID,
		Title:         "Sent Thread",
		Provider:      "claude",
		WorkspacePath: "/tmp/d",
		Model:         "claude-sonnet-4-6",
		Mode:          "chat",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	draftEmpty := Thread{
		ID:            "thread-draft-empty",
		ProjectID:     proj.ID,
		Title:         "Draft Thread",
		Provider:      "claude",
		WorkspacePath: "/tmp/d",
		Model:         "claude-sonnet-4-6",
		Mode:          "chat",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	archivedWithItems := Thread{
		ID:            "thread-archived",
		ProjectID:     proj.ID,
		Title:         "Archived Thread",
		Provider:      "claude",
		WorkspacePath: "/tmp/d",
		Model:         "claude-sonnet-4-6",
		Mode:          "chat",
		CreatedAt:     now,
		UpdatedAt:     now,
		Archived:      true,
	}
	for _, t2 := range []Thread{withItems, draftEmpty, archivedWithItems} {
		if err := s.CreateThread(t2); err != nil {
			t.Fatalf("CreateThread(%s): %v", t2.ID, err)
		}
	}
	if err := s.InsertItem(Item{
		ID:        "item-1",
		ThreadID:  withItems.ID,
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Summary:   "hi",
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("InsertItem(withItems): %v", err)
	}
	if err := s.InsertItem(Item{
		ID:        "item-2",
		ThreadID:  archivedWithItems.ID,
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Summary:   "old",
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("InsertItem(archivedWithItems): %v", err)
	}

	got, err := s.ListThreadsWithItems()
	if err != nil {
		t.Fatalf("ListThreadsWithItems: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (draft hidden, archived hidden)", len(got))
	}
	if got[0].ID != withItems.ID {
		t.Errorf("got thread %q, want %q", got[0].ID, withItems.ID)
	}
}

func TestThreadMutationsReturnNotFoundForMissingRows(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-missing", "/tmp/m")

	now := time.Now().UnixMilli()
	thread := Thread{
		ID:            "missing-thread",
		ProjectID:     proj.ID,
		Title:         "Missing",
		Provider:      "claude",
		WorkspacePath: "/tmp/workspace",
		Model:         "claude-sonnet-4-6",
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	if err := s.UpdateThread(thread); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("UpdateThread() error = %v, want sql.ErrNoRows", err)
	}
	if err := s.DeleteThread(thread.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("DeleteThread() error = %v, want sql.ErrNoRows", err)
	}
	if err := s.ArchiveThread(thread.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("ArchiveThread() error = %v, want sql.ErrNoRows", err)
	}
	if err := s.UpdateSessionRef(thread.ID, "session-ref"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("UpdateSessionRef() error = %v, want sql.ErrNoRows", err)
	}
	if err := s.UpdateTitle(thread.ID, "Renamed"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("UpdateTitle() error = %v, want sql.ErrNoRows", err)
	}
	if err := s.UpdateModel(thread.ID, "claude-opus-4-6"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("UpdateModel() error = %v, want sql.ErrNoRows", err)
	}
}
