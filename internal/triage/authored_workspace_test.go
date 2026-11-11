package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/pathlinks"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// authoredWorkspaceThread creates a claude thread whose row works in
// workspacePath.
func authoredWorkspaceThread(t *testing.T, st *store.Store, threadID, workspacePath string) {
	t.Helper()
	ensureTriageProject(t, st)
	now := time.Now().UnixMilli()
	if err := st.CreateThread(store.Thread{
		ID:            threadID,
		ProjectID:     triageTestProjectID,
		Title:         "authored workspace",
		Provider:      "claude",
		WorkspacePath: workspacePath,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
}

// settleProse streams text as one assistant text block, settles it, and
// returns the settled row.
func settleProse(t *testing.T, router *Router, st *store.Store, threadID, text string) store.Item {
	t.Helper()
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: threadID, Content: text, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: threadID,
		Meta: json.RawMessage(`{"blockType":"text"}`), Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("content block stop: %v", err)
	}
	router.WaitForPendingSettles()
	items, err := st.ListItems(threadID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	for _, item := range items {
		if item.Kind == itemKindAssistantText && item.Summary == text {
			return item
		}
	}
	t.Fatalf("no settled assistant text %q among %d items", text, len(items))
	return store.Item{}
}

func authoredWorkspaceFromMeta(t *testing.T, meta string) (string, bool) {
	t.Helper()
	if meta == "" {
		return "", false
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal([]byte(meta), &decoded); err != nil {
		t.Fatalf("decode meta %q: %v", meta, err)
	}
	raw, ok := decoded[pathlinks.WorkspaceMetaKey]
	if !ok {
		return "", false
	}
	var workspace string
	if err := json.Unmarshal(raw, &workspace); err != nil {
		t.Fatalf("decode %s %q: %v", pathlinks.WorkspaceMetaKey, raw, err)
	}
	return workspace, true
}

func workspaceChanged(t *testing.T, router *Router, threadID string, meta json.RawMessage) {
	t.Helper()
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventWorkspaceChanged, ThreadID: threadID, Meta: meta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("workspace changed: %v", err)
	}
}

func workspaceChangeMeta(t *testing.T, cwd string) json.RawMessage {
	t.Helper()
	meta, err := json.Marshal(provider.WorkspaceChangeMeta{Tool: "EnterWorktree", Cwd: cwd, WorktreePath: cwd})
	if err != nil {
		t.Fatalf("marshal workspace change: %v", err)
	}
	return meta
}

// A row whose prose holds a markdown image records the workspace it was
// written in, beside the path refs validated against that workspace. A row
// with no image carries no stamp.
func TestSettledProseRecordsAuthoredWorkspaceForImages(t *testing.T) {
	root := seedPathlinksWorkspace(t, "src/foo.ts")
	router, st, _ := newTestRouter(t)
	authoredWorkspaceThread(t, st, "t1", root)

	withImage := settleProse(t, router, st, "t1", "Shot of src/foo.ts:\n\n![overview](shots/overview.png)")
	if got, ok := authoredWorkspaceFromMeta(t, withImage.Meta); !ok || got != root {
		t.Fatalf("authored workspace = %q (present %v), want %q; meta %s", got, ok, root, withImage.Meta)
	}
	if refs := pathRefsFromMeta(t, withImage.Meta); len(refs) != 1 || refs[0].Path != "src/foo.ts" {
		t.Fatalf("path refs = %+v, want src/foo.ts beside the stamp; meta %s", refs, withImage.Meta)
	}

	plain := settleProse(t, router, st, "t1", "Edited src/foo.ts and nothing else.")
	if got, ok := authoredWorkspaceFromMeta(t, plain.Meta); ok {
		t.Fatalf("prose with no image recorded workspace %q; meta %s", got, plain.Meta)
	}
}

// The provider process moves its own directory mid-session. Rows that
// settle after the move are validated against, and record, the directory
// the process moved to; the row written before keeps what it recorded.
func TestAuthoredWorkspaceFollowsProviderMove(t *testing.T) {
	root := seedPathlinksWorkspace(t, "src/root_only.ts")
	worktree := seedPathlinksWorkspace(t, "src/worktree_only.ts")
	router, st, _ := newTestRouter(t)
	authoredWorkspaceThread(t, st, "t1", root)

	before := settleProse(t, router, st, "t1", "Before: ![a](shots/a.png) beside src/root_only.ts")
	if got, _ := authoredWorkspaceFromMeta(t, before.Meta); got != root {
		t.Fatalf("before the move: authored workspace = %q, want %q", got, root)
	}

	workspaceChanged(t, router, "t1", workspaceChangeMeta(t, worktree))

	after := settleProse(t, router, st, "t1", "After: ![b](shots/b.png) beside src/worktree_only.ts and src/root_only.ts")
	if got, _ := authoredWorkspaceFromMeta(t, after.Meta); got != worktree {
		t.Fatalf("after the move: authored workspace = %q, want %q", got, worktree)
	}
	if refs := pathRefsFromMeta(t, after.Meta); len(refs) != 1 || refs[0].Path != "src/worktree_only.ts" {
		t.Fatalf("after the move: path refs = %+v, want only the worktree's file", refs)
	}

	stillBefore, found, err := st.GetThreadItem("t1", before.ID)
	if err != nil || !found {
		t.Fatalf("reread first row: found=%v err=%v", found, err)
	}
	if got, _ := authoredWorkspaceFromMeta(t, stillBefore.Meta); got != root {
		t.Fatalf("the row written before the move now records %q, want %q", got, root)
	}
}

// A replacement process launches from the thread row's workspace, so the
// directory the previous process moved to does not carry over.
func TestAuthoredWorkspaceReturnsToThreadRowForReplacementProcess(t *testing.T) {
	root := seedPathlinksWorkspace(t, "src/root_only.ts")
	worktree := seedPathlinksWorkspace(t, "src/worktree_only.ts")
	router, st, _ := newTestRouter(t)
	authoredWorkspaceThread(t, st, "t1", root)

	settleProse(t, router, st, "t1", "Warm the cache: ![a](shots/a.png)")
	workspaceChanged(t, router, "t1", workspaceChangeMeta(t, worktree))
	moved := settleProse(t, router, st, "t1", "Moved: ![b](shots/b.png)")
	if got, _ := authoredWorkspaceFromMeta(t, moved.Meta); got != worktree {
		t.Fatalf("after the move: authored workspace = %q, want %q", got, worktree)
	}

	router.MarkThreadActive("t1")

	replaced := settleProse(t, router, st, "t1", "Replacement: ![c](shots/c.png)")
	if got, _ := authoredWorkspaceFromMeta(t, replaced.Meta); got != root {
		t.Fatalf("replacement process: authored workspace = %q, want the row's %q", got, root)
	}
}

// A move the router cannot read drops what it cached rather than keeping
// the directory the process left.
func TestUnreadableWorkspaceChangeFallsBackToThreadRow(t *testing.T) {
	root := seedPathlinksWorkspace(t, "src/root_only.ts")
	worktree := seedPathlinksWorkspace(t, "src/worktree_only.ts")
	for name, meta := range map[string]json.RawMessage{
		"malformed": json.RawMessage(`{"cwd":`),
		"blank cwd": json.RawMessage(`{"tool":"ExitWorktree","cwd":"  "}`),
	} {
		t.Run(name, func(t *testing.T) {
			router, st, _ := newTestRouter(t)
			authoredWorkspaceThread(t, st, "t1", root)
			settleProse(t, router, st, "t1", "Warm the cache: ![a](shots/a.png)")
			workspaceChanged(t, router, "t1", workspaceChangeMeta(t, worktree))
			workspaceChanged(t, router, "t1", meta)

			item := settleProse(t, router, st, "t1", "After: ![b](shots/b.png)")
			if got, _ := authoredWorkspaceFromMeta(t, item.Meta); got != root {
				t.Fatalf("authored workspace = %q, want the row's %q", got, root)
			}
		})
	}
}
