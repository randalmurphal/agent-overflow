package app

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"testing"

	"agent-overflow/internal/store"
)

func draftMoveApp(t *testing.T) *App {
	t.Helper()
	a := newAttachmentTestApp(t)
	source, err := a.store.GetThread("thr-a")
	if err != nil {
		t.Fatal(err)
	}
	source.ID = "thr-b"
	if err := a.store.CreateThread(source); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestMoveDraftCarriesAttachmentsAndSnippets(t *testing.T) {
	a := draftMoveApp(t)
	image := realPNGBytes(t)
	file := []byte("draft file content\n")
	i := uploadTestAttachment(t, a, "thr-a", "picture.png", "image/png", image)
	f := uploadTestAttachment(t, a, "thr-a", "notes.txt", "text/plain", file)
	chips := []TerminalChip{{ID: "capture", Label: "shell", Content: "captured output", Preview: "output", CreatedAt: 1}}
	plan := &SourceProposedPlan{ThreadID: "plan-thread", ItemID: "plan", Title: "Plan"}
	expected := DraftSnapshot{Content: "prompt [Image #1]", AttachmentIDs: []string{i.ID, f.ID}, TerminalChips: chips, SourceProposedPlan: plan}
	if err := a.SaveDraft(t.Context(), "thr-a", expected.Content, expected.AttachmentIDs, chips, plan); err != nil {
		t.Fatal(err)
	}
	moved, err := a.MoveDraftToThread(t.Context(), "thr-a", "thr-b", expected)
	if err != nil {
		t.Fatal(err)
	}
	if moved.Content != expected.Content || !reflect.DeepEqual(moved.TerminalChips, chips) || !reflect.DeepEqual(moved.SourceProposedPlan, plan) {
		t.Fatalf("moved draft: %+v", moved)
	}
	if len(moved.AttachmentIDs) != 2 {
		t.Fatalf("attachments: %v", moved.AttachmentIDs)
	}
	for index, id := range moved.AttachmentIDs {
		if id == expected.AttachmentIDs[index] {
			t.Fatal("attachment still belongs to source")
		}
		_, path, err := a.attachments.PathForThread("thr-b", id)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, [][]byte{image, file}[index]) {
			t.Fatal("attachment bytes changed")
		}
	}
	source, err := a.GetDraft("thr-a")
	if err != nil || source.Content != "" || len(source.AttachmentIDs) != 0 || len(source.TerminalChips) != 0 || source.SourceProposedPlan != nil {
		t.Fatalf("source: %+v, %v", source, err)
	}
	if removed, err := a.DeleteEmptyDraftThread("thr-a"); err != nil || !removed {
		t.Fatalf("ordinary cleanup: %v %v", removed, err)
	}
	for _, id := range moved.AttachmentIDs {
		_, path, err := a.attachments.PathForThread("thr-b", id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatal("source cleanup removed destination bytes:", err)
		}
	}
}

func TestMoveDraftLeavesEmptyWorktreeThread(t *testing.T) {
	a := draftMoveApp(t)
	source, err := a.store.GetThread("thr-a")
	if err != nil {
		t.Fatal(err)
	}
	source.WorktreePath = "/repo-worktrees/feature"
	source.WorkspacePath = source.WorktreePath
	if err := a.store.UpdateThread(source); err != nil {
		t.Fatal(err)
	}
	if err := a.SaveDraft(t.Context(), "thr-a", "move me", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := a.MoveDraftToThread(t.Context(), "thr-a", "thr-b", DraftSnapshot{Content: "move me"}); err != nil {
		t.Fatal(err)
	}
	if removed, err := a.DeleteEmptyDraftThread("thr-a"); err != nil || removed {
		t.Fatalf("worktree cleanup: %v %v", removed, err)
	}
	remaining, err := a.store.GetThread("thr-a")
	if err != nil || remaining.WorktreePath != source.WorktreePath {
		t.Fatalf("worktree changed: %+v %v", remaining, err)
	}
}

func TestMoveDraftRejectsChangedSourceAndOccupiedDestination(t *testing.T) {
	for _, targetContent := range []string{"", "other draft"} {
		t.Run(targetContent, func(t *testing.T) {
			a := draftMoveApp(t)
			if err := a.SaveDraft(t.Context(), "thr-a", "newer text", nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			if err := a.SaveDraft(t.Context(), "thr-b", targetContent, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			expected := "stale text"
			if targetContent != "" {
				expected = "newer text"
			}
			if _, err := a.MoveDraftToThread(t.Context(), "thr-a", "thr-b", DraftSnapshot{Content: expected}); err == nil {
				t.Fatal("unsafe move accepted")
			}
			for id, want := range map[string]string{"thr-a": "newer text", "thr-b": targetContent} {
				got, err := a.GetDraft(id)
				if err != nil || got.Content != want {
					t.Fatalf("%s: %+v %v", id, got, err)
				}
			}
		})
	}
}

func TestMoveDraftRefusesHistoryAndPartialAttachmentCopy(t *testing.T) {
	for _, history := range []bool{false, true} {
		t.Run(map[bool]string{true: "history", false: "copy failure"}[history], func(t *testing.T) {
			a := draftMoveApp(t)
			attachment := uploadTestAttachment(t, a, "thr-a", "notes.txt", "text/plain", []byte("contents"))
			ids := []string{attachment.ID, "missing"}
			if err := a.SaveDraft(t.Context(), "thr-a", "keep", ids, nil, nil); err != nil {
				t.Fatal(err)
			}
			if history {
				if _, err := a.store.UpsertItem(store.Item{ID: "sent", ThreadID: "thr-a", Kind: "user_text", Role: "user", Status: "completed", Summary: "sent", CreatedAt: 1, UpdatedAt: 1}, nil); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := a.MoveDraftToThread(t.Context(), "thr-a", "thr-b", DraftSnapshot{Content: "keep", AttachmentIDs: ids}); err == nil {
				t.Fatal("move should fail")
			}
			attachments, err := a.ListAttachments("thr-b")
			if err != nil || len(attachments) != 0 {
				t.Fatalf("partial copy leaked: %+v %v", attachments, err)
			}
			got, err := a.GetDraft("thr-a")
			if err != nil || got.Content != "keep" {
				t.Fatalf("source lost: %+v %v", got, err)
			}
		})
	}
}

func TestMovedDraftAttachmentCleanupPreservesNewWorkAndRepeats(t *testing.T) {
	a := draftMoveApp(t)
	old := uploadTestAttachment(t, a, "thr-a", "old.txt", "text/plain", []byte("old"))
	reused := uploadTestAttachment(t, a, "thr-a", "reused.txt", "text/plain", []byte("reused"))
	if err := a.SaveDraft(t.Context(), "thr-a", "moving", []string{old.ID, reused.ID}, nil, nil); err != nil {
		t.Fatal(err)
	}
	moved, _, err := a.store.GetThreadDraft("thr-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SaveDraft(t.Context(), "thr-a", "new work", []string{reused.ID}, nil, nil); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := a.cleanupMovedDraftAttachments(t.Context(), moved); err != nil {
			t.Fatal(err)
		}
		files, err := a.ListAttachments("thr-a")
		if err != nil || len(files) != 1 || files[0].ID != reused.ID {
			t.Fatalf("cleanup lost new work or retained old files: %+v %v", files, err)
		}
	}
}

func TestDraftMoveCannotCrossUpdateHandoff(t *testing.T) {
	a := draftMoveApp(t)
	if err := a.SaveDraft(t.Context(), "thr-a", "retained", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if reason, err := a.workAdmission.quiesce(func() (string, error) { return "", nil }); err != nil || reason != "" {
		t.Fatalf("handoff: %q %v", reason, err)
	}
	a.workAdmission.stopWaiting()
	if _, err := a.MoveDraftToThread(t.Context(), "thr-a", "thr-b", DraftSnapshot{Content: "retained"}); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("move crossed handoff: %v", err)
	}
	if got, err := a.GetDraft("thr-a"); err != nil || got.Content != "retained" {
		t.Fatalf("source changed: %+v %v", got, err)
	}
}
