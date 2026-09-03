package app

// Unit-level coverage for the diff-review prompt builder and helpers
// lives in internal/diffreview. The remaining App-level integration
// for ListDiffReviewComments / Create / Update / Delete / Send is
// covered through app_bindings_test.go and app_send_test.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
)

func TestSendPlanRevisionCommentsSendsDraftsAndMarksSent(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-plan-revision-comments")
	thread.Provider = string(provider.Claude)
	thread.Mode = "chat"
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	now := time.Now().UnixMilli()
	if err := app.store.InsertItemWithPayload(store.Item{
		ID:        "plan-item",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "tool_call",
		Role:      "assistant",
		Status:    "completed",
		Summary:   "Plan",
		PayloadID: "plan-payload",
		ToolName:  "plan",
		CreatedAt: now,
		UpdatedAt: now,
	}, store.Payload{
		ID:        "plan-payload",
		Kind:      "proposed_plan",
		Meta:      `{"title":"Plan","preview":"one","lineCount":1,"charCount":3}`,
		Data:      []byte("# Plan\n\nOne"),
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	if _, err := app.store.EnsureProposedPlanState(thread.ID, "plan-item", now); err != nil {
		t.Fatalf("ensure plan state: %v", err)
	}
	comment, err := app.store.CreateProposedPlanComment(store.ProposedPlanComment{
		ID: "comment-1", ThreadID: thread.ID, PlanItemID: "plan-item", StartLine: 2, EndLine: 3,
		SelectedText: "Old text", Body: "Make this more concrete.", CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("create comment: %v", err)
	}

	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{Binary: writeClaudeControlPassthroughBinary(t), WorkDir: thread.WorkspacePath},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put(thread.ID, session{Provider: string(provider.Claude), Token: "test-token", Claude: sess})

	updated, err := app.SendPlanRevisionComments(thread.ID, "plan-item", []string{comment.ID})
	if err != nil {
		t.Fatalf("SendPlanRevisionComments() error = %v", err)
	}
	if updated.Mode != "plan" {
		t.Fatalf("updated mode = %q, want plan", updated.Mode)
	}

	// applyProposedPlanAcceptance fires synchronously inside the send
	// path, so the comment status flip is durable by the time
	// SendPlanRevisionComments returns.

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var userItem store.Item
	for _, item := range items {
		if item.Role == "user" && strings.Contains(item.Summary, "comment: Make this more concrete.") {
			userItem = item
			break
		}
	}
	if !strings.Contains(userItem.Summary, "Old text\ncomment: Make this more concrete.") {
		t.Fatalf("revision prompt = %q, want selected text and comment", userItem.Summary)
	}
	if strings.Contains(userItem.Summary, "startLine") || strings.Contains(userItem.Summary, "```json") {
		t.Fatalf("revision prompt = %q, should not include line metadata or JSON", userItem.Summary)
	}
	var userMeta userMessageMeta
	if err := json.Unmarshal([]byte(userItem.Meta), &userMeta); err != nil {
		t.Fatalf("unmarshal revision user meta: %v", err)
	}
	if userMeta.RevisionSourceProposedPlan == nil || userMeta.RevisionSourceProposedPlan.ItemID != "plan-item" {
		t.Fatalf("revision source = %+v, want plan-item", userMeta.RevisionSourceProposedPlan)
	}

	comments, err := app.store.ListProposedPlanComments(thread.ID, "plan-item")
	if err != nil {
		t.Fatalf("ListProposedPlanComments: %v", err)
	}
	wantTurnID := fmt.Sprintf("%s:%d", thread.ID, userItem.TurnIndex)
	if len(comments) != 1 || comments[0].Status != "sent" || comments[0].SentAt == 0 || comments[0].SentTurnID != wantTurnID {
		t.Fatalf("comments after send = %+v, want sent marker linked to %s", comments, wantTurnID)
	}
}

func TestSendMessageWithOptionsAppendsDraftPlanComments(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-plan-revision-message-comments")
	thread.Provider = string(provider.Claude)
	thread.Mode = "plan"
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	now := time.Now().UnixMilli()
	if err := app.store.InsertItemWithPayload(store.Item{
		ID:        "plan-item",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "tool_call",
		Role:      "assistant",
		Status:    "completed",
		Summary:   "Plan",
		PayloadID: "plan-payload",
		ToolName:  "plan",
		CreatedAt: now,
		UpdatedAt: now,
	}, store.Payload{
		ID:        "plan-payload",
		Kind:      "proposed_plan",
		Meta:      `{"title":"Plan","preview":"one","lineCount":1,"charCount":3}`,
		Data:      []byte("# Plan\n\nOne"),
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	if _, err := app.store.EnsureProposedPlanState(thread.ID, "plan-item", now); err != nil {
		t.Fatalf("ensure plan state: %v", err)
	}
	comment, err := app.store.CreateProposedPlanComment(store.ProposedPlanComment{
		ID: "comment-1", ThreadID: thread.ID, PlanItemID: "plan-item", StartLine: 3, EndLine: 3,
		SelectedText: "One", Body: "Make this concrete.", CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("create comment: %v", err)
	}
	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{Binary: writeClaudeControlPassthroughBinary(t), WorkDir: thread.WorkspacePath},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put(thread.ID, session{Provider: string(provider.Claude), Token: "test-token", Claude: sess})

	_, err = app.SendMessageWithOptions(context.Background(), thread.ID, "Please revise.", SendMessageOptions{
		RevisionSourceProposedPlan: &SourceProposedPlan{ThreadID: thread.ID, ItemID: "plan-item"},
		RevisionSourceCommentIDs:   []string{comment.ID},
	})
	if err != nil {
		t.Fatalf("SendMessageWithOptions() error = %v", err)
	}

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var userItem store.Item
	for _, item := range items {
		if item.Role == "user" && strings.Contains(item.Summary, "Please revise.") {
			userItem = item
			break
		}
	}
	if !strings.Contains(userItem.Summary, "Please revise.\n\nOne\ncomment: Make this concrete.") {
		t.Fatalf("user summary = %q, want message plus formatted comment", userItem.Summary)
	}
}

func TestCreateProposedPlanCommentRejectsOutOfRangeLines(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-plan-comment-range")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	now := time.Now().UnixMilli()
	if err := app.store.InsertItemWithPayload(store.Item{
		ID:        "plan-item",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "tool_call",
		Role:      "assistant",
		Status:    "completed",
		Summary:   "Plan",
		PayloadID: "plan-payload",
		ToolName:  "plan",
		CreatedAt: now,
		UpdatedAt: now,
	}, store.Payload{
		ID:        "plan-payload",
		Kind:      "proposed_plan",
		Meta:      `{"title":"Plan","preview":"one","lineCount":1,"charCount":3}`,
		Data:      []byte("# Plan\n\nOne"),
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed plan: %v", err)
	}

	_, err := app.CreateProposedPlanComment(thread.ID, store.ProposedPlanCommentInput{
		PlanItemID: "plan-item",
		StartLine:  3,
		EndLine:    4,
		Body:       "Out of range",
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds plan length") {
		t.Fatalf("CreateProposedPlanComment() error = %v, want out-of-range line error", err)
	}
}

func TestCreateProposedPlanCommentDerivesSelectedTextFromPayload(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-plan-comment-selected-text")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	now := time.Now().UnixMilli()
	if err := app.store.InsertItemWithPayload(store.Item{
		ID:        "plan-item",
		ThreadID:  thread.ID,
		TurnIndex: 0,
		ItemIndex: 0,
		Kind:      "tool_call",
		Role:      "assistant",
		Status:    "completed",
		Summary:   "Plan",
		PayloadID: "plan-payload",
		ToolName:  "plan",
		CreatedAt: now,
		UpdatedAt: now,
	}, store.Payload{
		ID:        "plan-payload",
		Kind:      "proposed_plan",
		Meta:      `{"title":"Plan","preview":"one","lineCount":3,"charCount":30}`,
		Data:      []byte("# Plan\n\nActual text"),
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed plan: %v", err)
	}

	comment, err := app.CreateProposedPlanComment(thread.ID, store.ProposedPlanCommentInput{
		PlanItemID: "plan-item",
		StartLine:  3,
		EndLine:    3,
		Body:       "Use the real line",
	})
	if err != nil {
		t.Fatalf("CreateProposedPlanComment() error = %v", err)
	}
	if comment.SelectedText != "Actual text" {
		t.Fatalf("selected text = %q, want payload-derived text", comment.SelectedText)
	}
}

func writeClaudeControlPassthroughBinary(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "claude-control-passthrough.sh")
	script := `#!/bin/sh
set -eu
while IFS= read -r line; do
    case "$line" in
        *'"set_permission_mode"'*)
            reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
            printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"
            ;;
    esac
done
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}
