package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"github.com/google/uuid"
)

func TestDraftProjectTransferCompletesWithoutFrontendOrProvider(t *testing.T) {
	source := transferTestBackend(t)
	destination := transferTestBackend(t, func(context.Context, string) error { return errors.New("a draft must not require a provider account") })
	project, err := source.store.CreateProject(store.Project{ID: uuid.NewString(), Name: "Source", Path: testutil.InitGitRepo(t)})
	if err != nil {
		t.Fatal(err)
	}
	targetProject, err := destination.store.CreateProject(store.Project{ID: uuid.NewString(), Name: "Destination", Path: testutil.InitGitRepo(t)})
	if err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(t.TempDir(), "draft-worktree")
	testutil.RunGit(t, project.Path, "worktree", "add", "-b", "draft-work", worktree)
	thread := store.Thread{ID: uuid.NewString(), ProjectID: project.ID, Title: "Draft", Mode: "plan", Provider: "claude", Model: "claude-sonnet-4-6", ReasoningEffort: "high", RuntimeMode: "read-only", WorkspacePath: worktree, WorktreePath: worktree}
	if err := source.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	file := uploadTestAttachment(t, source, thread.ID, "draft.txt", "text/plain", []byte("portable file"))
	chips := []TerminalChip{{ID: "capture", Label: "shell", Content: "portable terminal output", Preview: "output", CreatedAt: 1}}
	plan := &SourceProposedPlan{ThreadID: "original-plan", ItemID: "plan-item", Title: "Original plan"}
	snapshot := DraftSnapshot{Content: "portable prompt", AttachmentIDs: []string{file.ID}, TerminalChips: chips, SourceProposedPlan: plan}
	if err := source.SaveDraft(t.Context(), thread.ID, snapshot.Content, snapshot.AttachmentIDs, chips, plan); err != nil {
		t.Fatal(err)
	}
	operation := uuid.NewString()
	destinationID, _ := destination.backendIdentity()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	intent, err := source.BeginDraftProjectTransfer(ctx, thread.ID, operation, destinationID, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	before, err := source.GetDraft(thread.ID)
	if err != nil || !reflect.DeepEqual(before.SourceProposedPlan, plan) {
		t.Fatalf("source plan link changed before transfer: %+v %v", before, err)
	}
	if err := source.SaveDraft(ctx, thread.ID, "new text", nil, nil, nil); err == nil {
		t.Fatal("accepted an edit while transferring")
	}
	if retry, err := source.BeginDraftProjectTransfer(ctx, thread.ID, operation, destinationID, snapshot); err != nil || retry != intent {
		t.Fatalf("retry: %+v %v", retry, err)
	}
	if _, err := source.BeginDraftProjectTransfer(ctx, thread.ID, operation, destinationID, DraftSnapshot{Content: "different request"}); err == nil {
		t.Fatal("reused operation ID with different contents")
	}
	offer, err := destination.CreateThreadTransferOffer(ctx, intent, targetProject.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.BindThreadTransferDestination(ctx, thread.ID, offer); err != nil {
		t.Fatal(err)
	}
	cancel()
	awaitTransferPhase(t, source, operation, "complete")
	awaitTransferPhase(t, destination, operation, "complete")
	got, err := destination.GetDraft(intent.TargetThreadID)
	if err != nil || got.Content != snapshot.Content || !reflect.DeepEqual(got.TerminalChips, chips) || len(got.AttachmentIDs) != 1 || got.SourceProposedPlan != nil {
		t.Fatalf("destination: %+v %v", got, err)
	}
	_, path, err := destination.attachments.PathForThread(intent.TargetThreadID, got.AttachmentIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "portable file" {
		t.Fatalf("file: %q %v", data, err)
	}
	completed, err := source.store.GetThreadTransfer(operation)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.announceTransferredDraft(completed); err != nil {
		t.Fatal(err)
	}
	leftover, err := source.ListAttachments(thread.ID)
	if err != nil || len(leftover) != 0 {
		t.Fatalf("source attachments retained: %+v %v", leftover, err)
	}
	remaining, err := source.GetDraft(thread.ID)
	if err != nil || remaining.Content != "" || len(remaining.AttachmentIDs) != 0 || len(remaining.TerminalChips) != 0 {
		t.Fatalf("source: %+v %v", remaining, err)
	}
	if deleted, err := source.DeleteEmptyDraftThread(thread.ID); err != nil || deleted {
		t.Fatalf("worktree draft removed: %v %v", deleted, err)
	}
	if _, err := os.Stat(worktree); err != nil {
		t.Fatal("worktree removed:", err)
	}
	target, err := destination.store.GetThread(intent.TargetThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if target.Mode != thread.Mode || target.Model != thread.Model || target.RuntimeMode != thread.RuntimeMode || target.ReasoningEffort != thread.ReasoningEffort || target.WorkspacePath != targetProject.Path || target.WorktreePath != "" || target.Branch != destination.gitCore().CurrentBranch(targetProject.Path) {
		t.Fatalf("settings or workspace changed: %+v", target)
	}
}
