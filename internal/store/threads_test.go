package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
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
	if _, err := s.CreateProject(p); err != nil {
		t.Fatalf("CreateProject(%s): %v", id, err)
	}
	return p
}

func TestCreateThreadWithNewFields(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-new-fields", "/home/user/project")

	// First create a parent thread so the parent_thread_id FK is valid.
	parent := makeThread("parent-1", "claude")
	parent.ProjectID = proj.ID
	parent.Mode = "chat"
	if err := s.CreateThread(parent); err != nil {
		t.Fatalf("create parent: %v", err)
	}

	// And a channel so the discussion_id FK is valid.
	now := time.Now().UnixMilli()
	if err := s.CreateChannel(Channel{
		ID: "disc-abc", ThreadID: "parent-1",
		Type: "deliberation", Status: "open",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	thr := Thread{
		ID:                  "thread-new-fields",
		ProjectID:           proj.ID,
		Title:               "Full Thread",
		Provider:            "claude",
		SessionRef:          "sess-123",
		PendingForkRef:      "sess-pending",
		PendingForkResumeAt: "leaf-uuid-1",
		WorkspacePath:       "/home/user/project",
		Model:               "opus-4",
		WorktreePath:        "/home/user/.worktrees/feat-x",
		Branch:              "feat-x",
		Mode:                "plan",
		ReasoningEffort:     "xhigh",
		FastMode:            true,
		ContextWindow:       200000,
		DiscussionID:        "disc-abc",
		ParentThreadID:      "parent-1",
		ForkedFromThreadID:  "parent-1",
		CreatedAt:           now,
		UpdatedAt:           now,
		Archived:            false,
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
	if got.PendingForkResumeAt != thr.PendingForkResumeAt {
		t.Errorf("PendingForkResumeAt: got %q, want %q", got.PendingForkResumeAt, thr.PendingForkResumeAt)
	}
	if got.Provider != thr.Provider {
		t.Errorf("Provider: got %q, want %q", got.Provider, thr.Provider)
	}
	if got.ForkedFromThreadID != thr.ForkedFromThreadID {
		t.Errorf("ForkedFromThreadID: got %q, want %q", got.ForkedFromThreadID, thr.ForkedFromThreadID)
	}
}

func TestListBlockedThreadWorkspaceRefs(t *testing.T) {
	s := newTestStore(t)
	project := newTestProject(t, s, "project-blocked-refs", "/repo")
	otherProject := newTestProject(t, s, "project-blocked-refs-other", "/other")

	createThread := func(id, projectID, workspace string) {
		t.Helper()
		thread := makeThread(id, "claude")
		thread.ProjectID = projectID
		thread.WorkspacePath = workspace
		thread.WorktreePath = workspace
		if err := s.CreateThread(thread); err != nil {
			t.Fatalf("CreateThread(%s): %v", id, err)
		}
	}
	createThread("idle", project.ID, "/repo-idle")
	createThread("active", project.ID, "/repo-active")
	createThread("background", project.ID, "/repo-background")
	createThread("completed-background", project.ID, "/repo-completed-background")
	createThread("other-active", otherProject.ID, "/other-active")
	codexSubagent := makeThread("codex-subagent", "codex")
	codexSubagent.ProjectID = project.ID
	codexSubagent.WorkspacePath = "/repo-codex-subagent"
	codexSubagent.WorktreePath = codexSubagent.WorkspacePath
	if err := s.CreateThread(codexSubagent); err != nil {
		t.Fatalf("CreateThread(codex-subagent): %v", err)
	}

	for _, threadID := range []string{"active", "other-active"} {
		if err := s.InsertTurn(Turn{
			TurnID:    "turn-" + threadID,
			ThreadID:  threadID,
			TurnIndex: 0,
			StartedAt: 1,
		}); err != nil {
			t.Fatalf("InsertTurn(%s): %v", threadID, err)
		}
	}
	now := time.Now().UnixMilli()
	if _, err := s.AppendItem(Item{
		ID:           "bg-running",
		ThreadID:     "background",
		TurnIndex:    0,
		ItemIndex:    0,
		Kind:         "tool_call",
		Role:         "assistant",
		Status:       "running",
		IsBackground: true,
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("AppendItem(background): %v", err)
	}
	if _, err := s.AppendItem(Item{
		ID:           "bg-completed-launch",
		ThreadID:     "completed-background",
		TurnIndex:    0,
		ItemIndex:    0,
		Kind:         "tool_call",
		Role:         "assistant",
		Status:       "running",
		IsBackground: true,
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("AppendItem(completed background launch): %v", err)
	}
	if _, err := s.AppendItem(Item{
		ID:           "bg-completed-result",
		ThreadID:     "completed-background",
		TurnIndex:    0,
		ItemIndex:    1,
		Kind:         "tool_completion",
		Role:         "assistant",
		Status:       "completed",
		CompletionOf: "bg-completed-launch",
		CreatedAt:    now + 1,
		UpdatedAt:    now + 1,
	}); err != nil {
		t.Fatalf("AppendItem(completed background result): %v", err)
	}
	if _, err := s.AppendItem(Item{
		ID:           "spawn-codex-subagent",
		ThreadID:     "codex-subagent",
		TurnIndex:    0,
		ItemIndex:    0,
		Kind:         "tool_call",
		Role:         "assistant",
		Status:       "completed",
		ToolName:     "collab_agent",
		IsBackground: true,
		Meta:         `{"live_background_active":true,"input":{"tool":"spawn_agent"}}`,
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("AppendItem(codex subagent): %v", err)
	}

	refs, err := s.ListBlockedThreadWorkspaceRefs()
	if err != nil {
		t.Fatalf("ListBlockedThreadWorkspaceRefs(): %v", err)
	}
	got := map[string]bool{}
	for _, ref := range refs {
		got[ref.ID] = true
	}
	if !got["active"] || !got["background"] || !got["codex-subagent"] {
		t.Fatalf("blocked ref ids = %v, want active, background, and Codex subagent", got)
	}
	// Unscoped by project on purpose: a directory does not stop being in use
	// because a second project row also names it, and the removal gate this
	// feeds matches paths across every project.
	if !got["other-active"] {
		t.Fatalf("blocked ref ids dropped a busy thread in another project: %v", got)
	}
	if got["idle"] || got["completed-background"] {
		t.Fatalf("blocked ref ids leaked an idle thread: %v", got)
	}
	if len(refs) != 4 {
		t.Fatalf("blocked refs = %+v, want active, background, codex-subagent, other-active", refs)
	}

	planRows, err := s.db.Query("EXPLAIN QUERY PLAN " + blockedThreadWorkspaceRefsSQL)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer planRows.Close()
	var usedBackgroundIndex, usedSubagentIndex, usedCompletionIndex bool
	for planRows.Next() {
		var id, parent, unused int
		var detail string
		if err := planRows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("scan query plan: %v", err)
		}
		usedBackgroundIndex = usedBackgroundIndex || strings.Contains(detail, "idx_items_live_background")
		usedSubagentIndex = usedSubagentIndex || strings.Contains(detail, "idx_items_live_codex_subagent")
		usedCompletionIndex = usedCompletionIndex || strings.Contains(detail, "idx_items_completion_of")
	}
	if err := planRows.Err(); err != nil {
		t.Fatalf("iterate query plan: %v", err)
	}
	if !usedBackgroundIndex || !usedSubagentIndex || !usedCompletionIndex {
		t.Fatalf(
			"query plan missing background/subagent/completion indexes: background=%v subagent=%v completion=%v",
			usedBackgroundIndex, usedSubagentIndex, usedCompletionIndex,
		)
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

	// Channel for the discussion_id FK on the upcoming update.
	if err := s.CreateChannel(Channel{
		ID: "disc-xyz", ThreadID: "thread-upd-parent",
		Type: "deliberation", Status: "open",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	// Update with new field values.
	thr.Title = "After Update"
	thr.WorktreePath = "/home/user/.worktrees/fix-123"
	thr.Branch = "fix-123"
	thr.Mode = "plan"
	thr.DiscussionID = "disc-xyz"
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
	// PendingForkRef / PendingForkResumeAt are deliberately NOT reachable
	// from here — see TestUpdateThreadPreservesPendingForkPin.
	if got.ForkedFromThreadID != "thread-upd-parent" {
		t.Errorf("ForkedFromThreadID: got %q, want %q", got.ForkedFromThreadID, "thread-upd-parent")
	}
	if got.UpdatedAt != now {
		t.Errorf("UpdatedAt after metadata update: got %d, want original %d", got.UpdatedAt, now)
	}

	activityAt := now + 7000
	if err := s.MarkThreadActivity(thr.ID, activityAt); err != nil {
		t.Fatalf("MarkThreadActivity() error = %v", err)
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
	if got2.UpdatedAt != activityAt {
		t.Errorf("UpdatedAt after stale metadata update: got %d, want activity timestamp %d", got2.UpdatedAt, activityAt)
	}
}

func TestProviderSwitchClearsPendingForkPin(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("thread-switch-pin-clear", "claude")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if err := s.SetThreadForkResume(thr.ID, "", "source-session", "leaf-uuid-3"); err != nil {
		t.Fatalf("SetThreadForkResume(pin): %v", err)
	}

	// The switch's UPDATE clears the pin itself (the columns are absent from
	// updateThreadSetSQL, so without the inline clear a committed switch
	// could strand a pin into the old provider's session files).
	switched := thr
	switched.Provider = "codex"
	switched.SessionRef = ""
	if err := s.UpdateThreadIfProviderSwitchAllowed(switched, "claude"); err != nil {
		t.Fatalf("UpdateThreadIfProviderSwitchAllowed: %v", err)
	}
	got, err := s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if got.Provider != "codex" {
		t.Fatalf("Provider = %q, want codex", got.Provider)
	}
	if got.PendingForkRef != "" || got.PendingForkResumeAt != "" {
		t.Fatalf("pin survived the provider switch: %q@%q, want empty",
			got.PendingForkRef, got.PendingForkResumeAt)
	}
}

func TestUpdateSessionRefClearsPendingForkRef(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-pending", "/tmp/p")

	thread := makeThread("thread-clear-pending", "claude")
	thread.ProjectID = proj.ID
	thread.PendingForkRef = "pending-123"
	thread.PendingForkResumeAt = "leaf-abc"
	thread.UpdatedAt = 1000
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	if _, err := s.UpdateSessionRef(thread.ID, "session-456"); err != nil {
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
	if got.PendingForkResumeAt != "" {
		t.Fatalf("PendingForkResumeAt = %q, want empty (one-shot pin, consumed with the ref)", got.PendingForkResumeAt)
	}
	if got.UpdatedAt != thread.UpdatedAt {
		t.Fatalf("UpdatedAt = %d, want %d", got.UpdatedAt, thread.UpdatedAt)
	}

	// A restated (unchanged) ref must still clear a pending fork ref: the
	// changed flag gates the frontend push, never the write itself. The
	// re-pin goes through the pin's own writer — UpdateThread cannot set it.
	if err := s.SetThreadForkResume(thread.ID, got.SessionRef, "pending-again", "leaf-again"); err != nil {
		t.Fatalf("SetThreadForkResume(re-pin) error = %v", err)
	}
	changed, err := s.UpdateSessionRef(thread.ID, "session-456")
	if err != nil {
		t.Fatalf("UpdateSessionRef() (restate) error = %v", err)
	}
	if changed {
		t.Fatal("changed = true when restating the same session ref, want false")
	}
	got, err = s.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if got.PendingForkRef != "" {
		t.Fatalf("PendingForkRef = %q after restate, want empty (clear must not be gated on changed)", got.PendingForkRef)
	}
	if got.PendingForkResumeAt != "" {
		t.Fatalf("PendingForkResumeAt = %q after restate, want empty", got.PendingForkResumeAt)
	}
}

// TestUpdateSessionRefAndRemapClearsPendingForkPin covers the OTHER
// session-ref writer: the remap variant a lazy fork's first session
// init goes through must consume the pin exactly like UpdateSessionRef
// — a survivor would re-pin the NEXT session start.
func TestUpdateSessionRefAndRemapClearsPendingForkPin(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-pending-remap", "/tmp/p")

	thread := makeThread("thread-clear-pending-remap", "claude")
	thread.ProjectID = proj.ID
	thread.PendingForkRef = "pending-123"
	thread.PendingForkResumeAt = "leaf-abc"
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	if _, err := s.UpdateSessionRefAndRemapProviderIDs(thread.ID, "session-789", nil, nil); err != nil {
		t.Fatalf("UpdateSessionRefAndRemapProviderIDs() error = %v", err)
	}

	got, err := s.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if got.SessionRef != "session-789" {
		t.Fatalf("SessionRef = %q, want %q", got.SessionRef, "session-789")
	}
	if got.PendingForkRef != "" || got.PendingForkResumeAt != "" {
		t.Fatalf("pending fork state = %q@%q after remap writer, want both empty", got.PendingForkRef, got.PendingForkResumeAt)
	}
}

// TestUpdateThreadPreservesPendingForkPin is the sixth fossil of the
// clobber class: the lazy-fork pin is ONE-SHOT state the session-ref
// writers consume, so a caller renaming a thread from a snapshot it read
// before the fork's first send must not be able to resurrect a cleared pin
// (or blank a fresh one). SetThreadForkResume is the only writer; the two
// columns are absent from updateThreadSetSQL.
//
// Same assertion shape as the LastReadAt / PinnedAt guards: the pin is set
// out of band, then DELIBERATELY given different values on the in-memory
// struct handed to UpdateThread.
func TestUpdateThreadPreservesPendingForkPin(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("thread-fork-pin-preserve", "claude")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	// The pinned lazy fork: no session of its own, the SOURCE ref plus the
	// leaf its timeline was cloned at.
	if err := s.SetThreadForkResume(thr.ID, "", "source-session", "leaf-uuid-9"); err != nil {
		t.Fatalf("SetThreadForkResume(pin): %v", err)
	}
	pinned, err := s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if pinned.SessionRef != "" || pinned.PendingForkRef != "source-session" || pinned.PendingForkResumeAt != "leaf-uuid-9" {
		t.Fatalf("pin round-trip = %q/%q@%q, want /source-session@leaf-uuid-9",
			pinned.SessionRef, pinned.PendingForkRef, pinned.PendingForkResumeAt)
	}

	// A stale whole-row write: a rename carrying a pre-fork snapshot of the
	// pin columns (empty) and a post-consumption one (a different pin).
	stale := pinned
	stale.Title = "Renamed"
	stale.PendingForkRef = ""
	stale.PendingForkResumeAt = ""
	if err := s.UpdateThread(stale); err != nil {
		t.Fatalf("UpdateThread(cleared pin): %v", err)
	}
	after, err := s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("GetThread after UpdateThread: %v", err)
	}
	if after.PendingForkRef != "source-session" || after.PendingForkResumeAt != "leaf-uuid-9" {
		t.Fatalf("UpdateThread cleared the pin: got %q@%q, want source-session@leaf-uuid-9",
			after.PendingForkRef, after.PendingForkResumeAt)
	}
	if after.Title != "Renamed" {
		t.Fatalf("UpdateThread failed to write title: got %q", after.Title)
	}

	resurrect := after
	resurrect.PendingForkRef = "some-other-session"
	resurrect.PendingForkResumeAt = "leaf-uuid-stale"
	if err := s.UpdateThread(resurrect); err != nil {
		t.Fatalf("UpdateThread(foreign pin): %v", err)
	}
	after, err = s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("GetThread after second UpdateThread: %v", err)
	}
	if after.PendingForkRef != "source-session" || after.PendingForkResumeAt != "leaf-uuid-9" {
		t.Fatalf("UpdateThread overwrote the pin: got %q@%q, want source-session@leaf-uuid-9",
			after.PendingForkRef, after.PendingForkResumeAt)
	}

	// The anchored / Codex fork shape: a session ref of the fork's own, no
	// pin. The three columns are written as a set, so this also clears.
	if err := s.SetThreadForkResume(thr.ID, "fork-session", "", ""); err != nil {
		t.Fatalf("SetThreadForkResume(own session): %v", err)
	}
	after, err = s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("GetThread after own-session write: %v", err)
	}
	if after.SessionRef != "fork-session" || after.PendingForkRef != "" || after.PendingForkResumeAt != "" {
		t.Fatalf("fork resume state = %q/%q@%q, want fork-session with no pin",
			after.SessionRef, after.PendingForkRef, after.PendingForkResumeAt)
	}

	// A row deleted underneath the saga must be named, never silently
	// counted as a wired fork.
	if err := s.SetThreadForkResume("thread-does-not-exist", "", "src", ""); err == nil ||
		!strings.Contains(err.Error(), "thread-does-not-exist") {
		t.Fatalf("SetThreadForkResume(missing thread) = %v, want an error naming the thread", err)
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
		Mode:          "plan",
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
	if got.Mode != "plan" {
		t.Errorf("Mode: got %q, want %q", got.Mode, "plan")
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

// TestListChildThreadsBreaksCreatedAtTiesByInsertionOrder guards the
// rowid tiebreak in ListChildThreads' ORDER BY: discussion participant
// threads are all stamped with one shared created_at millisecond
// (BuildParticipantPlans), so without `rowid ASC` SQL semantics would
// let ties come back in any order and destabilize the deliberation
// roster's round-robin sequence across reads.
func TestListChildThreadsBreaksCreatedAtTiesByInsertionOrder(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-tiebreak", "/tmp/tiebreak")

	parent := makeThread("tiebreak-parent", "claude")
	parent.ProjectID = proj.ID
	if err := s.CreateThread(parent); err != nil {
		t.Fatalf("CreateThread(parent): %v", err)
	}

	sharedCreatedAt := parent.CreatedAt + 1
	ids := []string{"tiebreak-child-1", "tiebreak-child-2", "tiebreak-child-3"}
	for _, id := range ids {
		child := makeThread(id, "claude")
		child.ProjectID = proj.ID
		child.ParentThreadID = parent.ID
		child.CreatedAt = sharedCreatedAt
		child.UpdatedAt = sharedCreatedAt
		if err := s.CreateThread(child); err != nil {
			t.Fatalf("CreateThread(%s): %v", id, err)
		}
	}

	children, err := s.ListChildThreads(parent.ID)
	if err != nil {
		t.Fatalf("ListChildThreads(): %v", err)
	}
	if len(children) != len(ids) {
		t.Fatalf("len(children) = %d, want %d", len(children), len(ids))
	}
	for i, id := range ids {
		if children[i].ID != id {
			t.Fatalf("children[%d].ID = %q, want %q (insertion order under identical created_at)", i, children[i].ID, id)
		}
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

// TestUpdateModeDoesNotBumpUpdatedAt — mode is an in-thread edit and
// must not advance the sidebar timestamp. The interaction-point bump
// helper (Store.MarkThreadActivity) is the only writer to updated_at on
// a live thread.
func TestUpdateModeDoesNotBumpUpdatedAt(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-mode-ua", "/tmp/ua")

	thr := makeThread("thread-mode-ua", "claude")
	thr.ProjectID = proj.ID
	thr.UpdatedAt = time.Now().UnixMilli() - 10_000
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
	if got.UpdatedAt != thr.UpdatedAt {
		t.Fatalf("UpdatedAt = %d, want %d (mode change must not bump activity)", got.UpdatedAt, thr.UpdatedAt)
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

	if _, _, err := s.UpdateReasoningEffort(thr.ID, "xhigh"); err != nil {
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

func TestUpdateReasoningEffortPersistsCodexUltra(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-effort-codex", "/tmp/effort-codex")

	thr := makeThread("thread-effort-codex", "codex")
	thr.ProjectID = proj.ID
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("CreateThread(): %v", err)
	}

	if _, _, err := s.UpdateReasoningEffort(thr.ID, "ultra"); err != nil {
		t.Fatalf("UpdateReasoningEffort(ultra): %v", err)
	}
	got, err := s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("GetThread(): %v", err)
	}
	if got.ReasoningEffort != "ultra" {
		t.Fatalf("ReasoningEffort = %q, want ultra", got.ReasoningEffort)
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

	if _, _, err := s.UpdateReasoningEffort(thr.ID, "ultranope"); !errors.Is(err, ErrInvalidEffort) {
		t.Fatalf("UpdateReasoningEffort(ultranope) error = %v, want ErrInvalidEffort", err)
	}
}

func TestCompareAndSwapModelProfileHonorsTheWholeExpectedProfile(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-model-profile-cas", "/tmp/model-profile-cas")

	thread := makeThread("thread-model-profile-cas", "codex")
	thread.ProjectID = proj.ID
	thread.Model = ""
	thread.ReasoningEffort = "medium"
	thread.ContextWindow = 200_000
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	before, err := s.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread before CAS: %v", err)
	}
	after := before
	after.Model = "gpt-5.6-sol"
	after.ReasoningEffort = "high"
	after.ContextWindow = 258_400

	applied, err := s.CompareAndSwapModelProfile(before, after)
	if err != nil || !applied {
		t.Fatalf("first CompareAndSwapModelProfile = applied:%v err:%v", applied, err)
	}
	got, err := s.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread after CAS: %v", err)
	}
	if got.Model != after.Model || got.ReasoningEffort != after.ReasoningEffort || got.ContextWindow != after.ContextWindow {
		t.Fatalf("profile after CAS = %q/%q/%d, want %q/%q/%d",
			got.Model, got.ReasoningEffort, got.ContextWindow,
			after.Model, after.ReasoningEffort, after.ContextWindow)
	}

	// Replaying a stale plan must lose cleanly after any one profile field
	// moves. This is the import-refresh race: a user choice made after check
	// owns the row when apply arrives.
	newer := got
	newer.ReasoningEffort = "xhigh"
	if err := s.UpdateThread(newer); err != nil {
		t.Fatalf("write newer user profile: %v", err)
	}
	staleTarget := after
	staleTarget.Model = "gpt-5.4"
	applied, err = s.CompareAndSwapModelProfile(got, staleTarget)
	if err != nil {
		t.Fatalf("stale CompareAndSwapModelProfile: %v", err)
	}
	if applied {
		t.Fatal("stale CompareAndSwapModelProfile applied after effort changed")
	}
	got, err = s.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread after stale CAS: %v", err)
	}
	if got.ReasoningEffort != "xhigh" || got.Model != after.Model {
		t.Fatalf("stale CAS overwrote newer profile: %q/%q", got.Model, got.ReasoningEffort)
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

	if _, _, err := s.UpdateFastMode(thr.ID, true); err != nil {
		t.Fatalf("UpdateFastMode(true): %v", err)
	}
	got, _ := s.GetThread(thr.ID)
	if !got.FastMode {
		t.Fatalf("FastMode = false, want true")
	}

	if _, _, err := s.UpdateFastMode(thr.ID, false); err != nil {
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

	if _, _, err := s.UpdateContextSettings(thr.ID, 200000, 0, 0); err != nil {
		t.Fatalf("UpdateContextSettings(200000): %v", err)
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

	if _, _, err := s.UpdateContextSettings(thr.ID, -1, 0, 0); !errors.Is(err, ErrInvalidContextWindow) {
		t.Fatalf("UpdateContextSettings(-1) = %v, want ErrInvalidContextWindow", err)
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

func TestUpdateBranchForWorkspacePersists(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-branch", "/tmp/branch")

	thr := makeThread("thread-branch", "claude")
	thr.ProjectID = proj.ID
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("CreateThread(): %v", err)
	}

	rows, err := s.UpdateBranchForWorkspace(thr.WorkspacePath, thr.WorkspacePath, "feat/abc")
	if err != nil {
		t.Fatalf("UpdateBranchForWorkspace(): %v", err)
	}
	if len(rows) != 1 || rows[0].ID != thr.ID || rows[0].Branch != "feat/abc" {
		t.Fatalf("returned rows = %+v, want the one thread on feat/abc", rows)
	}
	got, _ := s.GetThread(thr.ID)
	if got.Branch != "feat/abc" {
		t.Fatalf("Branch = %q, want feat/abc", got.Branch)
	}

	// Empty string clears the column.
	rows, err = s.UpdateBranchForWorkspace(thr.WorkspacePath, thr.WorkspacePath, "")
	if err != nil {
		t.Fatalf("UpdateBranchForWorkspace(empty): %v", err)
	}
	if len(rows) != 1 || rows[0].Branch != "" {
		t.Fatalf("returned rows = %+v, want the one thread with a cleared branch", rows)
	}
	got, _ = s.GetThread(thr.ID)
	if got.Branch != "" {
		t.Fatalf("Branch = %q, want empty", got.Branch)
	}
}

// TestUpdateBranchForWorkspaceUpdatesEveryThreadThere is the whole reason
// the write is keyed on the workspace: two threads sharing a worktree see
// the same checkout, so a branch observed once must land on both. A
// per-thread write left the second one advertising a branch the working
// tree had left behind.
func TestUpdateBranchForWorkspaceUpdatesEveryThreadThere(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-branch-shared", "/tmp/branch-shared")

	shared := "/tmp/branch-shared"
	for _, id := range []string{"shared-a", "shared-b"} {
		thr := makeThread(id, "claude")
		thr.ProjectID = proj.ID
		thr.WorkspacePath = shared
		thr.Branch = "main"
		if err := s.CreateThread(thr); err != nil {
			t.Fatalf("CreateThread(%s): %v", id, err)
		}
	}
	// A third thread in a different workspace must NOT be touched.
	other := makeThread("other-workspace", "claude")
	other.ProjectID = proj.ID
	other.WorkspacePath = "/tmp/branch-shared-worktree"
	other.Branch = "main"
	if err := s.CreateThread(other); err != nil {
		t.Fatalf("CreateThread(other): %v", err)
	}

	rows, err := s.UpdateBranchForWorkspace(shared, shared, "feature/live")
	if err != nil {
		t.Fatalf("UpdateBranchForWorkspace(): %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("returned %d rows, want 2 (both threads in the workspace)", len(rows))
	}
	for _, id := range []string{"shared-a", "shared-b"} {
		got, err := s.GetThread(id)
		if err != nil {
			t.Fatalf("GetThread(%s): %v", id, err)
		}
		if got.Branch != "feature/live" {
			t.Fatalf("thread %s Branch = %q, want feature/live", id, got.Branch)
		}
	}
	got, _ := s.GetThread(other.ID)
	if got.Branch != "main" {
		t.Fatalf("thread in another workspace Branch = %q, want main (untouched)", got.Branch)
	}
}

// TestUpdateBranchForWorkspaceSkipsThreadsThatMoved reproduces the race the
// workspace keying exists for: a branch observed against the old workspace
// is written back after a worktree switch has already re-pointed the
// thread. Keying on the workspace means the moved row is simply not
// matched, so the stale value cannot follow it.
func TestUpdateBranchForWorkspaceSkipsThreadsThatMoved(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-branch-cas", "/tmp/branch-cas")

	thr := makeThread("thread-branch-cas", "claude")
	thr.ProjectID = proj.ID
	thr.WorkspacePath = "/tmp/branch-cas"
	thr.Branch = "main"
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("CreateThread(): %v", err)
	}
	observedWorkspace := thr.WorkspacePath

	// The worktree switch lands first, rewriting workspace + branch.
	moved := thr
	moved.WorkspacePath = "/tmp/branch-cas-worktree"
	moved.WorktreePath = "/tmp/branch-cas-worktree"
	moved.Branch = "feature/new"
	if err := s.UpdateThread(moved); err != nil {
		t.Fatalf("UpdateThread(): %v", err)
	}

	// The queued observation from the OLD workspace arrives afterwards.
	rows, err := s.UpdateBranchForWorkspace(observedWorkspace, observedWorkspace, "stale/branch")
	if err != nil {
		t.Fatalf("UpdateBranchForWorkspace(): %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("returned %+v, want no rows: nothing occupies the old workspace", rows)
	}
	got, _ := s.GetThread(thr.ID)
	if got.Branch != "feature/new" {
		t.Fatalf("Branch = %q, want feature/new (the switch's value)", got.Branch)
	}

	// The same-workspace write still applies, so the keying is not simply
	// rejecting everything and the thread converges once it re-observes.
	rows, err = s.UpdateBranchForWorkspace(moved.WorkspacePath, moved.WorkspacePath, "feature/renamed")
	if err != nil {
		t.Fatalf("UpdateBranchForWorkspace(current workspace): %v", err)
	}
	if len(rows) != 1 || rows[0].Branch != "feature/renamed" {
		t.Fatalf("returned rows = %+v, want the moved thread on feature/renamed", rows)
	}
	got, _ = s.GetThread(thr.ID)
	if got.Branch != "feature/renamed" {
		t.Fatalf("Branch = %q, want feature/renamed", got.Branch)
	}
}

// TestUpdateBranchForWorkspaceMatchesEitherSpelling: thread rows carry
// whichever spelling of a directory was current when they were created (a
// worktree cut through a symlinked path keeps that path), while the
// observing client knows only its own. Matching one spelling left the rows
// stored under the other claiming a branch the working tree had left behind.
func TestUpdateBranchForWorkspaceMatchesEitherSpelling(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-branch-alias", "/tmp/branch-alias")

	const linked = "/tmp/link/branch-alias"
	const canonical = "/tmp/real/branch-alias"
	for id, path := range map[string]string{"alias-linked": linked, "alias-canonical": canonical} {
		thr := makeThread(id, "claude")
		thr.ProjectID = proj.ID
		thr.WorkspacePath = path
		thr.Branch = "main"
		if err := s.CreateThread(thr); err != nil {
			t.Fatalf("CreateThread(%s): %v", id, err)
		}
	}

	rows, err := s.UpdateBranchForWorkspace(linked, canonical, "feature/live")
	if err != nil {
		t.Fatalf("UpdateBranchForWorkspace(): %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("returned %d rows, want 2 (both spellings of one directory)", len(rows))
	}
	for _, id := range []string{"alias-linked", "alias-canonical"} {
		got, err := s.GetThread(id)
		if err != nil {
			t.Fatalf("GetThread(%s): %v", id, err)
		}
		if got.Branch != "feature/live" {
			t.Fatalf("thread %s Branch = %q, want feature/live", id, got.Branch)
		}
	}
}

// TestUpdateBranchForWorkspaceEmptyWorkspaceIsNotAnError covers the
// no-rows case: an unoccupied workspace must report an empty row set
// rather than a SQL error the caller has to string-match.
func TestUpdateBranchForWorkspaceEmptyWorkspaceIsNotAnError(t *testing.T) {
	s := newTestStore(t)

	rows, err := s.UpdateBranchForWorkspace("/tmp/nobody-here", "/tmp/nobody-here", "main")
	if err != nil {
		t.Fatalf("UpdateBranchForWorkspace(empty workspace): %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("returned %+v, want no rows", rows)
	}
}

// TestUpdateBranchForWorkspaceReturnsOnlyChangedRows pins the perf contract
// the caller depends on: it writes on EVERY attach, so the overwhelmingly
// common case — the observed branch already equals the cached one — must
// read nothing back and hand back nothing to sync.
func TestUpdateBranchForWorkspaceReturnsOnlyChangedRows(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-branch-nochange", "/tmp/branch-nochange")

	shared := "/tmp/branch-nochange"
	for _, id := range []string{"nc-a", "nc-b"} {
		thr := makeThread(id, "claude")
		thr.ProjectID = proj.ID
		thr.WorkspacePath = shared
		thr.Branch = "main"
		if err := s.CreateThread(thr); err != nil {
			t.Fatalf("CreateThread(%s): %v", id, err)
		}
	}

	// Nothing moved: both rows already say main.
	rows, err := s.UpdateBranchForWorkspace(shared, shared, "main")
	if err != nil {
		t.Fatalf("UpdateBranchForWorkspace(no change): %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("returned %+v, want no rows: no thread's branch changed", rows)
	}

	// One row moves out of band; only that one comes back.
	moved, err := s.GetThread("nc-a")
	if err != nil {
		t.Fatalf("GetThread(nc-a): %v", err)
	}
	moved.Branch = "feature/x"
	if err := s.UpdateThread(moved); err != nil {
		t.Fatalf("UpdateThread(nc-a): %v", err)
	}
	rows, err = s.UpdateBranchForWorkspace(shared, shared, "main")
	if err != nil {
		t.Fatalf("UpdateBranchForWorkspace(partial): %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "nc-a" || rows[0].Branch != "main" {
		t.Fatalf("returned rows = %+v, want only nc-a back on main", rows)
	}
	// …and the row that never moved is untouched, not merely unreported.
	got, _ := s.GetThread("nc-b")
	if got.Branch != "main" {
		t.Fatalf("nc-b Branch = %q, want main", got.Branch)
	}
}

// TestUpdateBranchForWorkspaceClearIsNullSafe covers the empty-string
// spelling: "" is stored as NULL, so the no-change predicate has to be
// null-safe or clearing an already-cleared column would report a write.
func TestUpdateBranchForWorkspaceClearIsNullSafe(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-branch-null", "/tmp/branch-null")

	thr := makeThread("thread-branch-null", "claude")
	thr.ProjectID = proj.ID
	thr.WorkspacePath = "/tmp/branch-null"
	thr.Branch = ""
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("CreateThread(): %v", err)
	}

	rows, err := s.UpdateBranchForWorkspace(thr.WorkspacePath, thr.WorkspacePath, "")
	if err != nil {
		t.Fatalf("UpdateBranchForWorkspace(clear an empty branch): %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("returned %+v, want no rows: the branch was already unset", rows)
	}

	// A real clear still reports the row it cleared.
	set, err := s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("GetThread(): %v", err)
	}
	set.Branch = "feature/y"
	if err := s.UpdateThread(set); err != nil {
		t.Fatalf("UpdateThread(): %v", err)
	}
	rows, err = s.UpdateBranchForWorkspace(thr.WorkspacePath, thr.WorkspacePath, "")
	if err != nil {
		t.Fatalf("UpdateBranchForWorkspace(clear): %v", err)
	}
	if len(rows) != 1 || rows[0].Branch != "" {
		t.Fatalf("returned rows = %+v, want the one cleared row", rows)
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

// --- v5 terminal-mode store behavior ---

// TestCreateStandaloneTerminalPersistsNullProject covers a project-less
// "home" terminal: CreateThread must write SQL NULL rather than an empty
// string to satisfy the projects FK. GetThread must read it back as ""
// without a scan error, and the raw column must be NULL.
func TestCreateStandaloneTerminalPersistsNullProject(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	term := Thread{
		ID:            "term-home",
		ProjectID:     "", // standalone: no project
		Title:         "Home Terminal",
		Provider:      "claude",
		WorkspacePath: "/home/u",
		Model:         "claude-sonnet-4-6",
		Mode:          "terminal",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.CreateThread(term); err != nil {
		t.Fatalf("CreateThread standalone terminal: %v", err)
	}

	got, err := s.GetThread("term-home")
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if got.ProjectID != "" {
		t.Errorf("ProjectID = %q, want \"\"", got.ProjectID)
	}
	if got.Mode != "terminal" {
		t.Errorf("Mode = %q, want terminal", got.Mode)
	}
	if got.ProjectPath != "" {
		t.Errorf("ProjectPath = %q, want \"\" (no project)", got.ProjectPath)
	}

	var rawProject sql.NullString
	if err := s.db.QueryRow(`SELECT project_id FROM threads WHERE id = 'term-home'`).Scan(&rawProject); err != nil {
		t.Fatalf("scan raw project_id: %v", err)
	}
	if rawProject.Valid {
		t.Errorf("raw project_id = %q, want NULL (else the projects FK breaks)", rawProject.String)
	}
}

// TestUpdateStandaloneTerminalPreservesNullProject ensures the UPDATE path
// also writes NULL for an empty ProjectID. Writing an empty string would
// violate the projects FK; we assert both that the update succeeds and that
// the column stays NULL.
func TestUpdateStandaloneTerminalPreservesNullProject(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	term := Thread{
		ID: "term-rename", ProjectID: "", Title: "Before",
		Provider: "claude", WorkspacePath: "/home/u", Model: "claude-sonnet-4-6",
		Mode: "terminal", CreatedAt: now, UpdatedAt: now,
	}
	if err := s.CreateThread(term); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	term.Title = "After"
	if err := s.UpdateThread(term); err != nil {
		t.Fatalf("UpdateThread standalone terminal: %v", err)
	}

	got, err := s.GetThread("term-rename")
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if got.Title != "After" {
		t.Errorf("Title = %q, want After", got.Title)
	}
	var rawProject sql.NullString
	if err := s.db.QueryRow(`SELECT project_id FROM threads WHERE id = 'term-rename'`).Scan(&rawProject); err != nil {
		t.Fatalf("scan raw project_id: %v", err)
	}
	if rawProject.Valid {
		t.Errorf("raw project_id = %q after update, want NULL", rawProject.String)
	}
}

// TestListThreadsWithItemsIncludesItemlessTerminal proves terminal threads
// bypass the item/draft visibility gate (they never carry either) while a
// plain chat thread with no items stays hidden. Both a project-scoped and a
// standalone terminal must surface.
func TestListThreadsWithItemsIncludesItemlessTerminal(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-term", "/tmp/term")
	now := time.Now().UnixMilli()

	emptyChat := Thread{
		ID: "chat-empty", ProjectID: proj.ID, Title: "Empty Chat",
		Provider: "claude", WorkspacePath: "/tmp/term", Model: "claude-sonnet-4-6",
		Mode: "chat", CreatedAt: now, UpdatedAt: now,
	}
	projTerminal := Thread{
		ID: "term-proj", ProjectID: proj.ID, Title: "Project Terminal",
		Provider: "claude", WorkspacePath: "/tmp/term", Model: "claude-sonnet-4-6",
		Mode: "terminal", CreatedAt: now, UpdatedAt: now + 1,
	}
	homeTerminal := Thread{
		ID: "term-home2", ProjectID: "", Title: "Home Terminal",
		Provider: "claude", WorkspacePath: "/home/u", Model: "claude-sonnet-4-6",
		Mode: "terminal", CreatedAt: now, UpdatedAt: now + 2,
	}
	for _, th := range []Thread{emptyChat, projTerminal, homeTerminal} {
		if err := s.CreateThread(th); err != nil {
			t.Fatalf("CreateThread(%s): %v", th.ID, err)
		}
	}

	got, err := s.ListThreadsWithItems()
	if err != nil {
		t.Fatalf("ListThreadsWithItems: %v", err)
	}
	ids := map[string]bool{}
	for _, th := range got {
		ids[th.ID] = true
	}
	if ids["chat-empty"] {
		t.Error("item-less chat thread must stay hidden")
	}
	if !ids["term-proj"] {
		t.Error("item-less project terminal must be visible")
	}
	if !ids["term-home2"] {
		t.Error("item-less standalone terminal must be visible")
	}
}

// TestItemlessTerminalReportsAsDraft pins the backend half of the
// per-project terminal sidebar-placement decision. IsDraft is computed
// purely from item-lessness (store.go: "no items have been persisted for
// the thread"), independent of mode — so an item-less terminal (terminals
// carry no items by design) reads back as IsDraft=true, while a chat thread
// reads false once it has an item.
//
// The sidebar depends on this coupling: a draft-flagged thread renders in
// the always-visible / top tier (frontend sidebarTree previewSidebarThreads),
// which is how a per-project terminal stays pinned-visible instead of sinking
// into the truncated "Show more" tail. Terminals have no meaningful activity
// to sort by (no turns, no items), so pinned-visible is the deliberate chosen
// behavior, not an accident.
//
// If a future change makes IsDraft mode-aware (reporting false for terminals
// for cleaner "draft" semantics), it MUST also add deliberate
// terminal-always-visible handling to sidebarTree, or per-project terminals
// silently fall into the truncated tail. This test fails first to force that
// decision into the open.
func TestItemlessTerminalReportsAsDraft(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-draftflag", "/tmp/df")
	now := time.Now().UnixMilli()

	terminal := Thread{
		ID: "term-draftflag", ProjectID: proj.ID, Title: "Terminal",
		Provider: "claude", WorkspacePath: "/tmp/df", Model: "claude-sonnet-4-6",
		Mode: "terminal", CreatedAt: now, UpdatedAt: now,
	}
	chatWithItem := Thread{
		ID: "chat-witem", ProjectID: proj.ID, Title: "Chat",
		Provider: "claude", WorkspacePath: "/tmp/df", Model: "claude-sonnet-4-6",
		Mode: "chat", CreatedAt: now, UpdatedAt: now,
	}
	for _, th := range []Thread{terminal, chatWithItem} {
		if err := s.CreateThread(th); err != nil {
			t.Fatalf("CreateThread(%s): %v", th.ID, err)
		}
	}
	// One item flips the chat thread out of draft state; the terminal keeps none.
	if err := s.InsertItem(Item{
		ID: "i-witem", ThreadID: chatWithItem.ID, TurnIndex: 0, ItemIndex: 0,
		Kind: "user_text", Role: "user", Summary: "hi", CreatedAt: now,
	}); err != nil {
		t.Fatalf("InsertItem: %v", err)
	}

	gotTerm, err := s.GetThread("term-draftflag")
	if err != nil {
		t.Fatalf("GetThread terminal: %v", err)
	}
	if !gotTerm.IsDraft {
		t.Error("item-less terminal must report IsDraft=true so the sidebar keeps it in the always-visible tier; if you intentionally changed this, add deliberate terminal-always-visible handling to sidebarTree")
	}

	gotChat, err := s.GetThread("chat-witem")
	if err != nil {
		t.Fatalf("GetThread chat: %v", err)
	}
	if gotChat.IsDraft {
		t.Error("a chat thread with an item must report IsDraft=false (proves IsDraft tracks item-lessness, the mechanism per-project terminals rely on)")
	}
}

// TestPerProjectTerminalListedUnderProject confirms a terminal that DOES
// carry a project still resolves its project path and appears in the
// project's thread list (it is badge-distinguished in the UI, not
// separated in the store).
func TestPerProjectTerminalListedUnderProject(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-pt", "/tmp/pt")
	now := time.Now().UnixMilli()
	term := Thread{
		ID: "term-pt", ProjectID: proj.ID, Title: "PT",
		Provider: "claude", WorkspacePath: "/tmp/pt", Model: "claude-sonnet-4-6",
		Mode: "terminal", CreatedAt: now, UpdatedAt: now,
	}
	if err := s.CreateThread(term); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	got, err := s.GetThread("term-pt")
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if got.ProjectID != proj.ID {
		t.Errorf("ProjectID = %q, want %q", got.ProjectID, proj.ID)
	}
	if got.ProjectPath != "/tmp/pt" {
		t.Errorf("ProjectPath = %q, want /tmp/pt", got.ProjectPath)
	}

	listed, err := s.ListThreadsByProject(proj.ID)
	if err != nil {
		t.Fatalf("ListThreadsByProject: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != "term-pt" {
		t.Fatalf("ListThreadsByProject = %v, want [term-pt]", listed)
	}
}

func TestListArchivedThreads(t *testing.T) {
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-archive-list", "/tmp/archive")

	now := time.Now().UnixMilli()
	active := Thread{
		ID: "thread-active", ProjectID: proj.ID, Title: "Active",
		Provider: "claude", WorkspacePath: "/tmp/a", Model: "claude-sonnet-4-6",
		Mode: "chat", CreatedAt: now, UpdatedAt: now,
	}
	archived1 := Thread{
		ID: "thread-arch-1", ProjectID: proj.ID, Title: "Archived 1",
		Provider: "claude", WorkspacePath: "/tmp/a", Model: "claude-sonnet-4-6",
		Mode: "chat", CreatedAt: now, UpdatedAt: now, Archived: true,
	}
	archived2 := Thread{
		ID: "thread-arch-2", ProjectID: proj.ID, Title: "Archived 2",
		Provider: "claude", WorkspacePath: "/tmp/a", Model: "claude-sonnet-4-6",
		Mode: "chat", CreatedAt: now, UpdatedAt: now + 1, Archived: true,
	}
	for _, thr := range []Thread{active, archived1, archived2} {
		if err := s.CreateThread(thr); err != nil {
			t.Fatalf("CreateThread(%s): %v", thr.ID, err)
		}
	}

	got, err := s.ListArchivedThreads()
	if err != nil {
		t.Fatalf("ListArchivedThreads: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (only archived threads)", len(got))
	}
	// Newest-touched first.
	if got[0].ID != archived2.ID {
		t.Errorf("got[0] = %q, want %q (newest first)", got[0].ID, archived2.ID)
	}
	if got[1].ID != archived1.ID {
		t.Errorf("got[1] = %q, want %q", got[1].ID, archived1.ID)
	}

	// Active threads must not appear.
	for _, thr := range got {
		if thr.ID == active.ID {
			t.Errorf("active thread %q should not appear in archived list", active.ID)
		}
	}

	// Unarchiving removes from the archived list.
	if _, _, err := s.UnarchiveThread(archived1.ID); err != nil {
		t.Fatalf("UnarchiveThread: %v", err)
	}
	got2, err := s.ListArchivedThreads()
	if err != nil {
		t.Fatalf("ListArchivedThreads after unarchive: %v", err)
	}
	if len(got2) != 1 {
		t.Fatalf("len = %d, want 1 after unarchive", len(got2))
	}
	if got2[0].ID != archived2.ID {
		t.Errorf("remaining = %q, want %q", got2[0].ID, archived2.ID)
	}
}

func TestListThreadsWithItemsSurfacesNonEmptyDrafts(t *testing.T) {
	// Drafts can carry user-authored text or source-plan context before
	// the first item exists. ListThreadsWithItems must surface them so the
	// user can find their seeded composer in the sidebar.
	s := newTestStore(t)
	proj := newTestProject(t, s, "proj-impl-draft", "/tmp/i")

	now := time.Now().UnixMilli()
	implDraft := Thread{
		ID:            "thread-impl-draft",
		ProjectID:     proj.ID,
		Title:         "Implement Foo",
		Provider:      "claude",
		WorkspacePath: "/tmp/i",
		Model:         "claude-sonnet-4-6",
		Mode:          "chat",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	emptyDraft := Thread{
		ID:            "thread-empty-draft",
		ProjectID:     proj.ID,
		Title:         "Empty Draft",
		Provider:      "claude",
		WorkspacePath: "/tmp/i",
		Model:         "claude-sonnet-4-6",
		Mode:          "chat",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	for _, t2 := range []Thread{implDraft, emptyDraft} {
		if err := s.CreateThread(t2); err != nil {
			t.Fatalf("CreateThread(%s): %v", t2.ID, err)
		}
	}

	if _, err := s.UpsertThreadDraft(ThreadDraft{
		ThreadID:                  implDraft.ID,
		Content:                   "PLEASE IMPLEMENT THIS PLAN:\n# Foo",
		Attachments:               "[]",
		TerminalChips:             "[]",
		PendingPlanImplementation: `{"threadId":"src","itemId":"plan-1","payloadId":"pl-1"}`,
		UpdatedAt:                 now,
	}); err != nil {
		t.Fatalf("UpsertThreadDraft(implDraft): %v", err)
	}
	// emptyDraft gets a content-only draft with no source-plan link. That is
	// still user-authored state, so it should now surface in the sidebar.
	if _, err := s.UpsertThreadDraft(ThreadDraft{
		ThreadID:      emptyDraft.ID,
		Content:       "typed but never sent",
		Attachments:   "[]",
		TerminalChips: "[]",
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("UpsertThreadDraft(emptyDraft): %v", err)
	}

	got, err := s.ListThreadsWithItems()
	if err != nil {
		t.Fatalf("ListThreadsWithItems: %v", err)
	}
	if len(got) != 2 {
		ids := make([]string, len(got))
		for i, th := range got {
			ids[i] = th.ID
		}
		t.Fatalf("got %v, want impl and content drafts visible", ids)
	}
	ids := map[string]bool{}
	for _, th := range got {
		ids[th.ID] = true
	}
	if !ids[implDraft.ID] || !ids[emptyDraft.ID] {
		t.Fatalf("got %#v, want %s and %s", ids, implDraft.ID, emptyDraft.ID)
	}
}

func TestListThreadsWithItemsDerivesActionableProposedPlan(t *testing.T) {
	s := newTestStore(t)
	thread := makeThread("thread-plan-ready", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread(): %v", err)
	}
	seedPlanItemForThreadList(t, s, thread.ID, "plan-1", 0, "completed", "assistant")
	if _, err := s.EnsureProposedPlanState(thread.ID, "plan-1", 100); err != nil {
		t.Fatalf("EnsureProposedPlanState(): %v", err)
	}

	got := mustListSingleThreadWithItems(t, s)
	if !got.HasActionableProposedPlan {
		t.Fatal("HasActionableProposedPlan = false, want true")
	}
}

func TestListThreadsWithItemsDerivesActionableProposedPlanFromLatestPlanOnly(t *testing.T) {
	s := newTestStore(t)
	thread := makeThread("thread-plan-implemented", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread(): %v", err)
	}
	seedPlanItemForThreadList(t, s, thread.ID, "plan-1", 0, "completed", "assistant")
	seedPlanItemForThreadList(t, s, thread.ID, "plan-2", 1, "completed", "assistant")
	if _, err := s.EnsureProposedPlanState(thread.ID, "plan-1", 100); err != nil {
		t.Fatalf("EnsureProposedPlanState(plan-1): %v", err)
	}
	if _, err := s.EnsureProposedPlanState(thread.ID, "plan-2", 200); err != nil {
		t.Fatalf("EnsureProposedPlanState(plan-2): %v", err)
	}
	if err := s.MarkProposedPlanImplemented(thread.ID, "plan-2", "impl-thread", "impl-item", 300); err != nil {
		t.Fatalf("MarkProposedPlanImplemented(): %v", err)
	}

	got := mustListSingleThreadWithItems(t, s)
	if got.HasActionableProposedPlan {
		t.Fatal("HasActionableProposedPlan = true, want false for implemented latest plan")
	}
}

func TestListThreadsWithItemsDoesNotDeriveActionablePlanForNonActionableRows(t *testing.T) {
	tests := []struct {
		name   string
		status string
		role   string
	}{
		{name: "streaming", status: "streaming", role: "assistant"},
		{name: "errored", status: "errored", role: "assistant"},
		{name: "user-authored", status: "completed", role: "user"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStore(t)
			thread := makeThread("thread-"+tt.name, "claude")
			if err := s.CreateThread(thread); err != nil {
				t.Fatalf("CreateThread(): %v", err)
			}
			seedPlanItemForThreadList(t, s, thread.ID, "plan-1", 0, tt.status, tt.role)
			if _, err := s.EnsureProposedPlanState(thread.ID, "plan-1", 100); err != nil {
				t.Fatalf("EnsureProposedPlanState(): %v", err)
			}

			got := mustListSingleThreadWithItems(t, s)
			if got.HasActionableProposedPlan {
				t.Fatal("HasActionableProposedPlan = true, want false")
			}
		})
	}
}

func TestListThreadsWithItemsDerivesIncompleteNewestTurn(t *testing.T) {
	s := newTestStore(t)
	thread := makeThread("thread-interrupted", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread(): %v", err)
	}
	seedItem(t, s, thread.ID, "item-1", 0, 0, "")
	startedAt := thread.CreatedAt + 100
	if err := s.InsertTurn(Turn{
		TurnID:    "turn-1",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		StartedAt: startedAt,
	}); err != nil {
		t.Fatalf("InsertTurn(): %v", err)
	}

	got := mustListSingleThreadWithItems(t, s)
	if !got.HasIncompleteTurn {
		t.Fatal("HasIncompleteTurn = false, want true")
	}

	if err := s.UpdateTurnCompleted("turn-1", startedAt+100, "end_turn", "", "", ""); err != nil {
		t.Fatalf("UpdateTurnCompleted(): %v", err)
	}
	got = mustListSingleThreadWithItems(t, s)
	if got.HasIncompleteTurn {
		t.Fatal("HasIncompleteTurn = true after completion, want false")
	}
}

func TestListThreadsWithItemsClearsIncompleteNewestTurnAfterRead(t *testing.T) {
	s := newTestStore(t)
	thread := makeThread("thread-interrupted-read", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread(): %v", err)
	}
	seedItem(t, s, thread.ID, "item-1", 0, 0, "")
	startedAt := thread.CreatedAt + 100
	if err := s.InsertTurn(Turn{
		TurnID:    "turn-1",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		StartedAt: startedAt,
	}); err != nil {
		t.Fatalf("InsertTurn(): %v", err)
	}

	lastReadAt := startedAt - 1
	if _, _, err := s.setThreadLastRead(thread.ID, &lastReadAt); err != nil {
		t.Fatalf("setThreadLastRead(before): %v", err)
	}
	got := mustListSingleThreadWithItems(t, s)
	if !got.HasIncompleteTurn {
		t.Fatal("HasIncompleteTurn = false before read, want true")
	}

	lastReadAt = startedAt
	if _, _, err := s.setThreadLastRead(thread.ID, &lastReadAt); err != nil {
		t.Fatalf("setThreadLastRead(equal): %v", err)
	}
	got = mustListSingleThreadWithItems(t, s)
	if got.HasIncompleteTurn {
		t.Fatal("HasIncompleteTurn = true after read at turn start, want false")
	}
}

func TestListThreadsWithItemsDerivesIncompleteOnlyForNewestTurn(t *testing.T) {
	s := newTestStore(t)
	thread := makeThread("thread-old-interrupted", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread(): %v", err)
	}
	seedItem(t, s, thread.ID, "item-1", 0, 0, "")
	oldStartedAt := thread.CreatedAt + 100
	if err := s.InsertTurn(Turn{
		TurnID:    "turn-old",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		StartedAt: oldStartedAt,
	}); err != nil {
		t.Fatalf("InsertTurn(old): %v", err)
	}
	newStartedAt := oldStartedAt + 200
	if err := s.InsertTurn(Turn{
		TurnID:    "turn-new",
		ThreadID:  thread.ID,
		TurnIndex: 1,
		StartedAt: newStartedAt,
	}); err != nil {
		t.Fatalf("InsertTurn(new): %v", err)
	}
	if err := s.UpdateTurnCompleted("turn-new", newStartedAt+100, "end_turn", "", "", ""); err != nil {
		t.Fatalf("UpdateTurnCompleted(new): %v", err)
	}

	got := mustListSingleThreadWithItems(t, s)
	if got.HasIncompleteTurn {
		t.Fatal("HasIncompleteTurn = true for old incomplete turn, want false")
	}
}

func seedPlanItemForThreadList(
	t *testing.T,
	s *Store,
	threadID, id string,
	turnIndex int,
	status, role string,
) {
	t.Helper()
	payloadID := "payload-" + id
	item := Item{
		ID:        id,
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		ItemIndex: 0,
		Kind:      "assistant_text",
		Role:      role,
		Status:    status,
		Summary:   id,
		PayloadID: payloadID,
		CreatedAt: int64(100 + turnIndex),
		UpdatedAt: int64(100 + turnIndex),
	}
	payload := Payload{
		ID:        payloadID,
		Kind:      "proposed_plan",
		Meta:      "{}",
		Data:      []byte("# Plan"),
		CreatedAt: int64(100 + turnIndex),
	}
	if err := s.InsertItemWithPayload(item, payload); err != nil {
		t.Fatalf("InsertItemWithPayload(%s): %v", id, err)
	}
}

func mustListSingleThreadWithItems(t *testing.T, s *Store) Thread {
	t.Helper()
	got, err := s.ListThreadsWithItems()
	if err != nil {
		t.Fatalf("ListThreadsWithItems(): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListThreadsWithItems() returned %d rows, want 1", len(got))
	}
	return got[0]
}

func TestDeleteThreadItemsChunkAggregatesHistoryAndRestoresOrdinaryTriggers(t *testing.T) {
	s := newTestStore(t)
	thread := makeThread("thread-delete-chunk", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	for i := 0; i < deleteThreadItemChunk+1; i++ {
		if _, err := s.db.Exec(`INSERT INTO items (
			id, thread_id, turn_index, item_index, kind, role, status, summary, created_at, updated_at
		) VALUES (?, ?, 0, ?, 'notification', 'system', 'completed', '', 1, 1)`,
			fmt.Sprintf("delete-chunk-%d", i), thread.ID, i); err != nil {
			t.Fatalf("seed item %d: %v", i, err)
		}
	}

	var beforeRev, beforeEpoch int64
	if err := s.db.QueryRow(
		`SELECT history_rev, history_epoch FROM threads WHERE id = ?`, thread.ID,
	).Scan(&beforeRev, &beforeEpoch); err != nil {
		t.Fatalf("read stamps before chunk: %v", err)
	}
	deleted, err := s.deleteThreadItemsChunk(thread.ID)
	if err != nil {
		t.Fatalf("deleteThreadItemsChunk: %v", err)
	}
	if deleted != deleteThreadItemChunk {
		t.Fatalf("deleted %d rows, want %d", deleted, deleteThreadItemChunk)
	}

	var remaining int
	var afterRev, afterEpoch int64
	var bulk int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM items WHERE thread_id = ?`, thread.ID).Scan(&remaining); err != nil {
		t.Fatalf("count remaining items: %v", err)
	}
	if err := s.db.QueryRow(
		`SELECT history_rev, history_epoch, history_bulk_load FROM threads WHERE id = ?`, thread.ID,
	).Scan(&afterRev, &afterEpoch, &bulk); err != nil {
		t.Fatalf("read stamps after chunk: %v", err)
	}
	if remaining != 1 {
		t.Fatalf("remaining items = %d, want 1", remaining)
	}
	if afterRev != beforeRev+deleteThreadItemChunk || afterEpoch != beforeEpoch+deleteThreadItemChunk || bulk != 0 {
		t.Fatalf("post-chunk state = rev %d epoch %d bulk %d, want %d/%d/0",
			afterRev, afterEpoch, bulk,
			beforeRev+deleteThreadItemChunk, beforeEpoch+deleteThreadItemChunk)
	}

	if _, err := s.db.Exec(`INSERT INTO items (
		id, thread_id, turn_index, item_index, kind, role, status, summary, created_at, updated_at
	) VALUES ('delete-chunk-after', ?, 0, 1000, 'notification', 'system', 'completed', '', 1, 1)`, thread.ID); err != nil {
		t.Fatalf("ordinary insert after chunk: %v", err)
	}
	var finalRev, finalEpoch int64
	if err := s.db.QueryRow(
		`SELECT history_rev, history_epoch FROM threads WHERE id = ?`, thread.ID,
	).Scan(&finalRev, &finalEpoch); err != nil {
		t.Fatalf("read stamps after ordinary insert: %v", err)
	}
	if finalRev != afterRev+1 || finalEpoch != afterEpoch {
		t.Fatalf("ordinary insert stamps = %d/%d, want %d/%d",
			finalRev, finalEpoch, afterRev+1, afterEpoch)
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
	if _, _, err := s.ArchiveThread(thread.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("ArchiveThread() error = %v, want sql.ErrNoRows", err)
	}
	if _, err := s.UpdateSessionRef(thread.ID, "session-ref"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("UpdateSessionRef() error = %v, want sql.ErrNoRows", err)
	}
	if err := s.UpdateTitle(thread.ID, "Renamed"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("UpdateTitle() error = %v, want sql.ErrNoRows", err)
	}
	if err := s.UpdateModel(thread.ID, "claude-opus-4-6"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("UpdateModel() error = %v, want sql.ErrNoRows", err)
	}
}

func TestSetThreadLastReadRoundTrip(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("thread-last-read", "claude")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	// New threads start with a concrete read baseline. NULL is reserved
	// for legacy rows and explicit clears.
	got, err := s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if got.LastReadAt == nil || *got.LastReadAt != thr.CreatedAt {
		t.Fatalf("fresh thread LastReadAt = %v, want CreatedAt %d", got.LastReadAt, thr.CreatedAt)
	}

	// Set to a concrete timestamp.
	ts := int64(1_700_000_000_000)
	if _, _, err := s.setThreadLastRead(thr.ID, &ts); err != nil {
		t.Fatalf("setThreadLastRead(set): %v", err)
	}
	got, err = s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("GetThread after set: %v", err)
	}
	if got.LastReadAt == nil || *got.LastReadAt != ts {
		t.Fatalf("after set, LastReadAt = %v, want %d", got.LastReadAt, ts)
	}

	// Clear back to NULL via nil pointer.
	if _, _, err := s.setThreadLastRead(thr.ID, nil); err != nil {
		t.Fatalf("setThreadLastRead(clear): %v", err)
	}
	got, err = s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("GetThread after clear: %v", err)
	}
	if got.LastReadAt != nil {
		t.Fatalf("after clear, LastReadAt = %v, want nil", *got.LastReadAt)
	}

	// Missing thread should surface sql.ErrNoRows, mirroring the other
	// mutation helpers above.
	if _, _, err := s.setThreadLastRead("missing", &ts); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("setThreadLastRead(missing) error = %v, want sql.ErrNoRows", err)
	}
}

func TestCreateThreadPersistsInitialLastReadAt(t *testing.T) {
	s := newTestStore(t)

	lastReadAt := int64(1_700_000_000_000)
	thr := makeThread("thread-create-last-read", "claude")
	thr.LastReadAt = &lastReadAt
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	got, err := s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if got.LastReadAt == nil || *got.LastReadAt != lastReadAt {
		t.Fatalf("LastReadAt = %v, want %d", got.LastReadAt, lastReadAt)
	}
}

func TestMarkThreadReadNowClampsToLatestCompletedTurn(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("thread-read-clamp", "claude")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	completedAt := time.Now().UnixMilli() + 60_000
	insertCompletedTurn(t, s, thr.ID, "turn-read-clamp", completedAt-1000, completedAt)

	if _, _, err := s.MarkThreadReadNow(context.Background(), thr.ID); err != nil {
		t.Fatalf("MarkThreadReadNow: %v", err)
	}
	got, err := s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if got.LastReadAt == nil {
		t.Fatalf("LastReadAt = nil, want %d", completedAt)
	}
	if got.LatestTurnCompletedAt == nil || *got.LatestTurnCompletedAt != completedAt {
		t.Fatalf("LatestTurnCompletedAt = %v, want %d", got.LatestTurnCompletedAt, completedAt)
	}
	if *got.LastReadAt != completedAt {
		t.Fatalf("LastReadAt = %d, want latest completed turn %d", *got.LastReadAt, completedAt)
	}
}

func TestMarkThreadReadNowClearsIncompleteNewestTurn(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("thread-read-incomplete", "claude")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	startedAt := time.Now().UnixMilli() + 60_000
	if err := s.InsertTurn(Turn{
		TurnID:    "turn-read-incomplete",
		ThreadID:  thr.ID,
		TurnIndex: 0,
		StartedAt: startedAt,
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}

	if _, _, err := s.MarkThreadReadNow(context.Background(), thr.ID); err != nil {
		t.Fatalf("MarkThreadReadNow: %v", err)
	}
	got, err := s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if got.LastReadAt == nil {
		t.Fatalf("LastReadAt = nil, want %d", startedAt)
	}
	if *got.LastReadAt != startedAt {
		t.Fatalf("LastReadAt = %d, want latest incomplete turn start %d", *got.LastReadAt, startedAt)
	}
	if got.HasIncompleteTurn {
		t.Fatal("HasIncompleteTurn = true after MarkThreadReadNow, want false")
	}
}

func TestMarkThreadReadNowAlreadyReadDoesNotRewrite(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("thread-read-noop", "claude")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	insertCompletedTurn(t, s, thr.ID, "turn-read-noop", 500, 1000)

	ts := int64(2000)
	if _, _, err := s.setThreadLastRead(thr.ID, &ts); err != nil {
		t.Fatalf("setThreadLastRead: %v", err)
	}
	if _, _, err := s.MarkThreadReadNow(context.Background(), thr.ID); err != nil {
		t.Fatalf("MarkThreadReadNow: %v", err)
	}

	got, err := s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if got.LastReadAt == nil || *got.LastReadAt != ts {
		t.Fatalf("LastReadAt = %v, want %d", got.LastReadAt, ts)
	}
}

func TestMarkThreadReadNowRefreshesEmptyThreadBaseline(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("thread-read-empty-refresh", "claude")
	initialReadAt := int64(1)
	thr.LastReadAt = &initialReadAt
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	before := nowMillis()
	if _, _, err := s.MarkThreadReadNow(context.Background(), thr.ID); err != nil {
		t.Fatalf("MarkThreadReadNow: %v", err)
	}

	got, err := s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if got.LastReadAt == nil {
		t.Fatal("LastReadAt = nil, want refreshed timestamp")
	}
	if *got.LastReadAt < before {
		t.Fatalf("LastReadAt = %d, want >= %d", *got.LastReadAt, before)
	}
}

func TestMarkThreadReadNowIgnoresMetadataOnlyUpdatedAt(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("thread-read-metadata", "claude")
	thr.UpdatedAt = 5000
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	insertCompletedTurn(t, s, thr.ID, "turn-read-metadata", 500, 1000)

	ts := int64(1000)
	if _, _, err := s.setThreadLastRead(thr.ID, &ts); err != nil {
		t.Fatalf("setThreadLastRead: %v", err)
	}
	if _, _, err := s.MarkThreadReadNow(context.Background(), thr.ID); err != nil {
		t.Fatalf("MarkThreadReadNow: %v", err)
	}

	got, err := s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if got.LastReadAt == nil || *got.LastReadAt != ts {
		t.Fatalf("LastReadAt = %v, want %d", got.LastReadAt, ts)
	}
	if got.UpdatedAt != thr.UpdatedAt {
		t.Fatalf("UpdatedAt = %d, want %d", got.UpdatedAt, thr.UpdatedAt)
	}
}

func insertCompletedTurn(t *testing.T, s *Store, threadID, turnID string, startedAt, completedAt int64) {
	t.Helper()
	if err := s.InsertTurn(Turn{
		TurnID:    turnID,
		ThreadID:  threadID,
		TurnIndex: 0,
		StartedAt: startedAt,
	}); err != nil {
		t.Fatalf("InsertTurn(%s): %v", turnID, err)
	}
	if err := s.UpdateTurnCompleted(turnID, completedAt, "end_turn", "", "", ""); err != nil {
		t.Fatalf("UpdateTurnCompleted(%s): %v", turnID, err)
	}
}

// TestUpdateThreadPreservesLastReadAt guards against a future refactor
// accidentally dropping last_read_at into UpdateThread's write list —
// which would nuke the user's read-state on every mode/title change.
//
// The assertion shape is important: we set last_read_at to `ts`, then
// DELIBERATELY clear the field on the in-memory Thread struct before
// calling UpdateThread. If UpdateThread wrote every field from the
// struct we'd see `ts` get clobbered back to NULL. Only a write list
// that deliberately skips last_read_at preserves it.
func TestUpdateThreadPreservesLastReadAt(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("thread-preserve", "claude")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	ts := int64(1_700_000_000_000)
	if _, _, err := s.setThreadLastRead(thr.ID, &ts); err != nil {
		t.Fatalf("setThreadLastRead: %v", err)
	}

	got, err := s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	// Mutate an unrelated field AND nuke the LastReadAt field on the
	// struct — a future UpdateThread refactor that tries to write the
	// struct's LastReadAt verbatim would silently clear the DB value.
	got.Title = "Renamed"
	got.LastReadAt = nil
	if err := s.UpdateThread(got); err != nil {
		t.Fatalf("UpdateThread: %v", err)
	}

	after, err := s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("GetThread after UpdateThread: %v", err)
	}
	if after.LastReadAt == nil || *after.LastReadAt != ts {
		t.Fatalf("UpdateThread clobbered last_read_at: got %v, want %d", after.LastReadAt, ts)
	}
	if after.Title != "Renamed" {
		t.Fatalf("UpdateThread failed to write title: got %q", after.Title)
	}
}

// TestSetThreadLastReadDoesNotBumpUpdatedAt pins the invariant that
// read-state is UI bookkeeping, not a thread mutation. Bumping updated_at
// would reshuffle the sidebar on every thread open — which is precisely
// what the sidebar's "most-recently-active-at-top" sort is trying to
// represent.
func TestSetThreadLastReadDoesNotBumpUpdatedAt(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("thread-no-bump", "claude")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	before, err := s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}

	ts := int64(1_700_000_000_000)
	if _, _, err := s.setThreadLastRead(thr.ID, &ts); err != nil {
		t.Fatalf("setThreadLastRead(set): %v", err)
	}
	afterSet, err := s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("GetThread after set: %v", err)
	}
	if afterSet.UpdatedAt != before.UpdatedAt {
		t.Fatalf("setThreadLastRead(set) bumped updated_at: %d -> %d",
			before.UpdatedAt, afterSet.UpdatedAt)
	}

	if _, _, err := s.setThreadLastRead(thr.ID, nil); err != nil {
		t.Fatalf("setThreadLastRead(clear): %v", err)
	}
	afterClear, err := s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("GetThread after clear: %v", err)
	}
	if afterClear.UpdatedAt != before.UpdatedAt {
		t.Fatalf("setThreadLastRead(clear) bumped updated_at: %d -> %d",
			before.UpdatedAt, afterClear.UpdatedAt)
	}
}

// TestPinUnpinLifecycle covers front pin → back group → re-pin → unpin.
// Re-pinning deliberately returns the row to the front burner; unpin clears
// every piece of pin state.
func TestPinUnpinLifecycle(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("thread-pin", "claude")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	if _, _, err := s.PinThread(thr.ID); err != nil {
		t.Fatalf("PinThread: %v", err)
	}
	pinned, err := s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("GetThread after pin: %v", err)
	}
	if pinned.PinnedAt == nil {
		t.Fatalf("PinnedAt = nil after PinThread")
	}
	if pinned.PinGroup == nil || *pinned.PinGroup != PinGroupFront {
		t.Fatalf("PinGroup = %v after PinThread, want front", pinned.PinGroup)
	}
	if _, _, err := s.SetThreadPinGroup(thr.ID, PinGroupBack); err != nil {
		t.Fatalf("SetThreadPinGroup(back): %v", err)
	}
	back, err := s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("GetThread after back-burner move: %v", err)
	}
	if back.PinGroup == nil || *back.PinGroup != PinGroupBack {
		t.Fatalf("PinGroup = %v after back-burner move, want back", back.PinGroup)
	}

	if _, _, err := s.PinThread(thr.ID); err != nil {
		t.Fatalf("PinThread (repeat): %v", err)
	}
	repinned, err := s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("GetThread after re-pin: %v", err)
	}
	if repinned.PinnedAt == nil {
		t.Fatal("PinnedAt = nil after repeat PinThread")
	}
	if repinned.PinGroup == nil || *repinned.PinGroup != PinGroupFront {
		t.Fatalf("re-pin left PinGroup = %v, want front", repinned.PinGroup)
	}

	if _, _, err := s.UnpinThread(thr.ID); err != nil {
		t.Fatalf("UnpinThread: %v", err)
	}
	unpinned, err := s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("GetThread after unpin: %v", err)
	}
	if unpinned.PinnedAt != nil {
		t.Fatalf("PinnedAt = %v after UnpinThread, want nil", *unpinned.PinnedAt)
	}
	if unpinned.PinGroup != nil {
		t.Fatalf("PinGroup = %v after UnpinThread, want nil", *unpinned.PinGroup)
	}
	if _, _, err := s.SetThreadPinGroup(thr.ID, PinGroupBack); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("SetThreadPinGroup on unpinned row error = %v, want sql.ErrNoRows", err)
	}
	if _, _, err := s.SetThreadPinGroup(thr.ID, 2); !errors.Is(err, ErrInvalidPinGroup) {
		t.Fatalf("SetThreadPinGroup(invalid) error = %v, want ErrInvalidPinGroup", err)
	}
}

// TestPinDoesNotBumpUpdatedAt mirrors the read-state invariant: pinning
// is presentation, not activity. Bumping updated_at would shuffle the
// project's lastActivity sort and obscure real thread work.
func TestPinDoesNotBumpUpdatedAt(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("thread-pin-quiet", "claude")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	before, err := s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}

	if _, _, err := s.PinThread(thr.ID); err != nil {
		t.Fatalf("PinThread: %v", err)
	}
	afterPin, err := s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("GetThread after pin: %v", err)
	}
	if afterPin.UpdatedAt != before.UpdatedAt {
		t.Fatalf("PinThread bumped updated_at: %d -> %d", before.UpdatedAt, afterPin.UpdatedAt)
	}

	if _, _, err := s.UnpinThread(thr.ID); err != nil {
		t.Fatalf("UnpinThread: %v", err)
	}
	afterUnpin, err := s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("GetThread after unpin: %v", err)
	}
	if afterUnpin.UpdatedAt != before.UpdatedAt {
		t.Fatalf("UnpinThread bumped updated_at: %d -> %d", before.UpdatedAt, afterUnpin.UpdatedAt)
	}
}

// TestUpdateThreadPreservesPinState mirrors the LastReadAt guard above:
// a future UpdateThread refactor that writes every struct field would
// silently nuke the user's pin state on every rename / mode toggle.
func TestUpdateThreadPreservesPinState(t *testing.T) {
	s := newTestStore(t)

	thr := makeThread("thread-pin-preserve", "claude")
	if err := s.CreateThread(thr); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if _, _, err := s.PinThread(thr.ID); err != nil {
		t.Fatalf("PinThread: %v", err)
	}

	got, err := s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if got.PinnedAt == nil {
		t.Fatalf("expected pinnedAt set after PinThread")
	}
	if _, _, err := s.SetThreadPinGroup(thr.ID, PinGroupBack); err != nil {
		t.Fatalf("SetThreadPinGroup: %v", err)
	}
	got, err = s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("GetThread after SetThreadPinGroup: %v", err)
	}
	pinnedTs := *got.PinnedAt

	// Mutate an unrelated field AND nuke PinnedAt on the struct so a
	// regression that adds pinned_at to UpdateThread's write list would
	// clear the DB value.
	got.Title = "Renamed"
	got.PinnedAt = nil
	got.PinGroup = nil
	if err := s.UpdateThread(got); err != nil {
		t.Fatalf("UpdateThread: %v", err)
	}

	after, err := s.GetThread(thr.ID)
	if err != nil {
		t.Fatalf("GetThread after UpdateThread: %v", err)
	}
	if after.PinnedAt == nil || *after.PinnedAt != pinnedTs {
		t.Fatalf("UpdateThread clobbered pinned_at: got %v, want %d", after.PinnedAt, pinnedTs)
	}
	if after.PinGroup == nil || *after.PinGroup != PinGroupBack {
		t.Fatalf("UpdateThread clobbered pin_group: got %v, want back", after.PinGroup)
	}
	if after.Title != "Renamed" {
		t.Fatalf("UpdateThread failed to write title: got %q", after.Title)
	}
}

// A boot-swept crashed turn (settled with stop_reason='interrupted' by
// RecoverCrashedTurns) must keep lighting the sidebar's durable
// Interrupted pill until the user views the thread — same UX as the
// completed_at=NULL row it replaced.
func TestListThreadsWithItemsDerivesInterruptedSettledNewestTurn(t *testing.T) {
	s := newTestStore(t)
	thread := makeThread("thread-swept-interrupted", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread(): %v", err)
	}
	seedItem(t, s, thread.ID, "item-1", 0, 0, "")
	startedAt := thread.CreatedAt + 100
	completedAt := startedAt + 50
	if err := s.InsertTurn(Turn{
		TurnID:    "turn-1",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		StartedAt: startedAt,
	}); err != nil {
		t.Fatalf("InsertTurn(): %v", err)
	}
	if err := s.UpdateTurnCompleted("turn-1", completedAt, "interrupted", "", "", ""); err != nil {
		t.Fatalf("UpdateTurnCompleted(): %v", err)
	}

	got := mustListSingleThreadWithItems(t, s)
	if !got.HasIncompleteTurn {
		t.Fatal("HasIncompleteTurn = false for unseen interrupted turn, want true")
	}

	// A read mid-turn (before the interrupt landed) does not clear it.
	lastReadAt := completedAt - 1
	if _, _, err := s.setThreadLastRead(thread.ID, &lastReadAt); err != nil {
		t.Fatalf("setThreadLastRead(mid-turn): %v", err)
	}
	got = mustListSingleThreadWithItems(t, s)
	if !got.HasIncompleteTurn {
		t.Fatal("HasIncompleteTurn = false for read before interrupt, want true")
	}

	// Reading at/after the settle clears it.
	lastReadAt = completedAt
	if _, _, err := s.setThreadLastRead(thread.ID, &lastReadAt); err != nil {
		t.Fatalf("setThreadLastRead(after): %v", err)
	}
	got = mustListSingleThreadWithItems(t, s)
	if got.HasIncompleteTurn {
		t.Fatal("HasIncompleteTurn = true after reading the interrupted settle, want false")
	}
}

// MarkThreadReadNow must clear the settled-interrupted pill: its read
// target includes MAX(completed_at), which covers the swept turn.
func TestMarkThreadReadNowClearsInterruptedSettledTurn(t *testing.T) {
	s := newTestStore(t)
	thread := makeThread("thread-swept-read-now", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread(): %v", err)
	}
	seedItem(t, s, thread.ID, "item-1", 0, 0, "")
	startedAt := thread.CreatedAt + 100
	if err := s.InsertTurn(Turn{
		TurnID:    "turn-1",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		StartedAt: startedAt,
	}); err != nil {
		t.Fatalf("InsertTurn(): %v", err)
	}
	if err := s.UpdateTurnCompleted("turn-1", startedAt+50, "interrupted", "", "", ""); err != nil {
		t.Fatalf("UpdateTurnCompleted(): %v", err)
	}

	if _, _, err := s.MarkThreadReadNow(context.Background(), thread.ID); err != nil {
		t.Fatalf("MarkThreadReadNow(): %v", err)
	}
	got := mustListSingleThreadWithItems(t, s)
	if got.HasIncompleteTurn {
		t.Fatal("HasIncompleteTurn = true after MarkThreadReadNow, want false")
	}
}

// updateThreadWrittenColumns extracts the column names updateThreadSetSQL
// actually assigns. The SQL is the behavior; parsing it is what keeps the gate
// below from checking a hand-kept list against another hand-kept list.
func updateThreadWrittenColumns(t *testing.T) map[string]bool {
	t.Helper()
	setClause, ok := strings.CutPrefix(updateThreadSetSQL, "UPDATE threads SET ")
	if !ok {
		t.Fatalf("updateThreadSetSQL no longer starts with `UPDATE threads SET `: %q", updateThreadSetSQL)
	}
	written := make(map[string]bool)
	for _, assignment := range strings.Split(setClause, ",") {
		assignment = strings.TrimSpace(assignment)
		column, value, found := strings.Cut(assignment, "=")
		if !found {
			t.Fatalf("updateThreadSetSQL fragment %q is not a `column=?` assignment", assignment)
		}
		column = strings.TrimSpace(column)
		if strings.TrimSpace(value) != "?" {
			t.Fatalf("updateThreadSetSQL assigns %s a literal (%q); the gate only understands bound parameters",
				column, strings.TrimSpace(value))
		}
		if written[column] {
			t.Errorf("updateThreadSetSQL assigns %s twice", column)
		}
		written[column] = true
	}
	if len(written) == 0 {
		t.Fatal("parsed no columns out of updateThreadSetSQL")
	}
	return written
}

// threadColumnsNotWrittenByUpdateThread names every `threads` column that
// updateThreadSetSQL deliberately does NOT write, with the reason it is
// excluded. It is the positive partner to that SQL: together the two MUST
// cover every column the table has, and no column may appear in both.
// TestUpdateThreadColumnGate enforces both halves against
// `PRAGMA table_info('threads')`, so a new column cannot land unclassified —
// the same forcing-function shape transport's //ao:scope annotation gate
// uses for App methods: a new one cannot land unclassified.
//
// A new column belongs in updateThreadSetSQL only if a whole-row write from a
// stale `Thread` struct is the CORRECT outcome for it. Anything owned by a
// narrow writer, a trigger, or the row's own lifecycle belongs here instead;
// the six TestUpdateThreadPreserves* tests are the incident record of what
// happens when that judgement goes the other way.
//
// This map is consulted by nothing at runtime — which is why it lives in the
// test file rather than the production binary: the SQL is the behavior, and a
// second list the writer derived from would only be able to agree with itself.
var threadColumnsNotWrittenByUpdateThread = map[string]string{
	"id":                       "the WHERE key — UpdateThread matches on it and must never rewrite it",
	"created_at":               "the row's birth stamp; immutable after CreateThread",
	"updated_at":               "the sidebar's activity clock, advanced only by writes that mean the user did something (TouchThread, archive/unarchive)",
	"last_read_at":             "per-thread read state, owned by MarkThreadRead / MarkThreadUnread",
	"pinned_at":                "sidebar pin state, owned by PinThread / UnpinThread",
	"pin_group":                "manual front/back pin group, owned by PinThread / SetThreadPinGroup / UnpinThread",
	"worktree_setup_state":     "owned by SetThreadWorktreeSetupState (v47); a workspace switch must not clobber a setup run that is still in flight",
	"pending_fork_session_ref": "half of the one-shot lazy-fork pin, owned by SetThreadForkResume and consumed by the two session-ref writers; a stale snapshot must not resurrect a pin a session start already cleared",
	"pending_fork_resume_at":   "the other half of that pin (v69); it clears with the ref it belongs to and is written by the same narrow writer",
	"import_source":            "write-once provenance (v50); CreateThread is the only writer and nothing may rewrite where a thread came from",
	"history_rev":              "the replica-invalidation counter (v55), maintained by the items triggers and bumpHistoryRevTx — a Go-side whole-row write would rewind it",
	"history_epoch":            "the replica-invalidation epoch (v55), maintained by the same triggers",
	"history_bulk_load":        "a transaction-private flag ApplyImportBatch and DeleteThread raise to suppress the per-row triggers; it is never visible outside their transaction",
	"live_todo":                "the provider's live todo list (v65), owned by SetThreadLiveTodo / ClearThreadLiveTodo; a rename must not drop a list the user is still working through",
	"created_by_device":        "write-once provenance (v73); the screen that started the thread is a fact about its creation, and a later mutation from anywhere must not restate it",
	"created_branch":           "write-once git origin (v74); it records where the workspace STOOD at creation, which is exactly what a whole-row write from a thread that has since moved would destroy",
	"created_remote_url":       "write-once git origin (v74), same reason",
	"created_head_commit":      "write-once git origin (v74), same reason",
}

// TestUpdateThreadColumnGate is the standing version of the six
// TestUpdateThreadPreserves* fossils below and above it. Each of those pins one
// column that a whole-row UpdateThread must not clobber, and each was written
// after the clobber happened. This test makes the NEXT one impossible to
// introduce by silence: every `threads` column must be either written by
// updateThreadSetSQL or named in threadColumnsNotWrittenByUpdateThread with a
// reason, never both, and the omission map may not name a column that does not
// exist.
//
// The column set comes from PRAGMA table_info on a migrated database rather
// than from a list in this file, so a migration that adds a column fails here
// until somebody decides which side it belongs on. Mirrors the //ao:scope
// completeness gate in internal/transport.
func TestUpdateThreadColumnGate(t *testing.T) {
	s := newTestStore(t)

	rows, err := s.db.Query(`SELECT name FROM pragma_table_info('threads') ORDER BY cid`)
	if err != nil {
		t.Fatalf("read threads columns: %v", err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan threads column: %v", err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate threads columns: %v", err)
	}
	if len(columns) == 0 {
		t.Fatal("PRAGMA table_info('threads') returned no columns")
	}

	written := updateThreadWrittenColumns(t)
	existing := make(map[string]bool, len(columns))
	for _, column := range columns {
		existing[column] = true
	}

	// Forward: every column is classified, and classified exactly once.
	for _, column := range columns {
		_, omitted := threadColumnsNotWrittenByUpdateThread[column]
		switch {
		case written[column] && omitted:
			t.Errorf("threads.%s is BOTH written by updateThreadSetSQL and listed in "+
				"threadColumnsNotWrittenByUpdateThread; the two lists disagree about what UpdateThread does",
				column)
		case !written[column] && !omitted:
			t.Errorf("threads.%s is unclassified: add it to updateThreadSetSQL if a whole-row "+
				"UpdateThread from a possibly-stale Thread struct should rewrite it, or to "+
				"threadColumnsNotWrittenByUpdateThread with a one-line reason if it must not",
				column)
		}
	}

	// Backward: neither list may name a column that does not exist. A renamed
	// or dropped column would otherwise leave a stale entry that silently
	// keeps the forward half passing.
	for column := range written {
		if !existing[column] {
			t.Errorf("updateThreadSetSQL writes %q, which is not a threads column", column)
		}
	}
	for column, reason := range threadColumnsNotWrittenByUpdateThread {
		if !existing[column] {
			t.Errorf("threadColumnsNotWrittenByUpdateThread names %q, which is not a threads column", column)
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("threadColumnsNotWrittenByUpdateThread[%q] carries no reason; the reason IS the record "+
				"of why a whole-row write must skip it", column)
		}
	}
}
