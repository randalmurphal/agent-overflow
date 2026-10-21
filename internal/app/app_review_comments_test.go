package app

// Unit-level coverage for the diff-review prompt builder and helpers
// lives in internal/diffreview.

import (
	"agent-overflow/internal/testutil/mockexec"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/diffreview"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/identity"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

func TestSendPlanRevisionCommentsSendsDraftsAndMarksSent(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	mockexec.Write(t, path, script)
	return path
}

// diffReviewSendThread is a Claude thread with a live passthrough session,
// so a send reaches a provider and lands as a user item.
func diffReviewSendThread(t *testing.T, app *App, id string) store.Thread {
	t.Helper()
	thread := testThread(id)
	thread.Provider = string(provider.Claude)
	thread.Mode = "chat"
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	sess, err := claude.NewSession(context.Background(), thread.ID,
		claude.Config{Binary: writeClaudeControlPassthroughBinary(t), WorkDir: thread.WorkspacePath},
		func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put(thread.ID, session{Provider: string(provider.Claude), Token: "test-token", Claude: sess})
	return thread
}

// sentDiffReviewItem is the user item a diff review send appended.
func sentDiffReviewItem(t *testing.T, app *App, threadID string) store.Item {
	t.Helper()
	items, err := app.store.ListItems(threadID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	for _, item := range items {
		if item.Role == "user" {
			return item
		}
	}
	t.Fatalf("thread %s has no user item", threadID)
	return store.Item{}
}

var diffReviewSendPR = gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "widgets", Number: 5}

func diffReviewPRInput(body string) store.DiffReviewCommentInput {
	return store.DiffReviewCommentInput{
		Scope: string(store.DiffReviewScopePR), SourceKey: "pr:github:acme/widgets#5", FilePath: "main.go",
		NewLine: 3, Side: "new", SelectedText: "x := 1", Body: body,
	}
}

func diffReviewPRContext(commentID string) SendDiffReviewCommentsInput {
	return SendDiffReviewCommentsInput{PR: &store.DiffReviewPRContext{
		Number: 5, URL: "https://github.com/acme/widgets/pull/5",
		Comments: []store.DiffReviewPRContextEntry{{CommentID: commentID, HunkExcerpt: "@@ -1 +1 @@\n+x := 1"}},
	}}
}

// The thread's own drafts: the prompt with the PR context, sent on the
// turn that carried them.
func TestSendDiffReviewCommentsSendsTheThreadsDraftsWithThePRContext(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := diffReviewSendThread(t, app, "thread-diff-review-send")
	draft, err := app.CreateDiffReviewComment(thread.ID, diffReviewPRInput("Name this better."))
	if err != nil {
		t.Fatal(err)
	}
	input := diffReviewPRContext(draft.ID)
	if _, err := app.SendDiffReviewComments(thread.ID, draft.Scope, draft.SourceKey, []string{draft.ID}, input); err != nil {
		t.Fatalf("SendDiffReviewComments: %v", err)
	}
	item := sentDiffReviewItem(t, app, thread.ID)
	if want := diffreview.BuildPromptWithPRContext([]store.DiffReviewComment{draft}, input.PR); item.Summary != want {
		t.Fatalf("prompt = %q, want %q", item.Summary, want)
	}
	sent, err := app.ListDiffReviewComments(thread.ID, draft.Scope, draft.SourceKey)
	if err != nil || len(sent) != 1 || sent[0].Status != "sent" || sent[0].SentTurnID != fmt.Sprintf("%s:%d", thread.ID, item.TurnIndex) {
		t.Fatalf("after send = %+v (%v)", sent, err)
	}
}

// A PR's drafts go to the named thread exactly as a thread's own do, and
// are marked sent on that thread's turn; an id the PR does not own is
// refused before anything is sent.
func TestSendPRDiffReviewCommentsSendsAPRsDraftsToAThread(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	thread := diffReviewSendThread(t, app, "thread-pr-diff-review-send")
	draft, err := app.CreatePRDiffReviewComment(diffReviewSendPR, diffReviewPRInput("Name this better."))
	if err != nil {
		t.Fatal(err)
	}
	if draft.PRKey != diffReviewSendPR.Key() || draft.ThreadID != "" {
		t.Fatalf("draft = %+v", draft)
	}
	threadOwned, err := app.CreateDiffReviewComment(thread.ID, diffReviewPRInput("The thread's own."))
	if err != nil {
		t.Fatal(err)
	}
	otherPR := diffReviewSendPR
	otherPR.Number = 6
	otherOwned, err := app.CreatePRDiffReviewComment(otherPR, diffReviewPRInput("Another PR's."))
	if err != nil {
		t.Fatal(err)
	}
	input := diffReviewPRContext(draft.ID)
	for _, foreign := range []string{threadOwned.ID, otherOwned.ID} {
		if _, err := app.SendPRDiffReviewComments(context.Background(), diffReviewSendPR, thread.ID, draft.Scope, draft.SourceKey, []string{draft.ID, foreign}, input); err == nil || !strings.Contains(err.Error(), "non-draft ids") {
			t.Fatalf("sending %s with the PR's draft = %v, want it refused as not the PR's", foreign, err)
		}
	}
	if items, _ := app.store.ListItems(thread.ID); len(items) != 0 {
		t.Fatalf("a refused send appended %d items", len(items))
	}
	// The thread's send door refuses a PR's drafts: they are git:operate.
	if _, err := app.SendMessageWithOptions(context.Background(), thread.ID, "", SendMessageOptions{
		RevisionSourceDiffReview:     &SourceDiffReview{PRKey: diffReviewSendPR.Key(), Scope: draft.Scope, SourceKey: draft.SourceKey},
		RevisionSourceDiffCommentIDs: []string{draft.ID},
	}); err == nil || !strings.Contains(err.Error(), "SendPRDiffReviewComments") {
		t.Fatalf("SendMessageWithOptions with a PR's drafts = %v", err)
	}

	if _, err := app.SendPRDiffReviewComments(context.Background(), diffReviewSendPR, thread.ID, draft.Scope, draft.SourceKey, []string{draft.ID}, input); err != nil {
		t.Fatalf("SendPRDiffReviewComments: %v", err)
	}
	item := sentDiffReviewItem(t, app, thread.ID)
	if want := diffreview.BuildPromptWithPRContext([]store.DiffReviewComment{draft}, input.PR); item.Summary != want {
		t.Fatalf("prompt = %q, want %q", item.Summary, want)
	}
	sent, err := app.ListPRDiffReviewComments(diffReviewSendPR, draft.Scope, draft.SourceKey)
	if err != nil || len(sent) != 1 || sent[0].Status != "sent" || sent[0].SentTurnID != fmt.Sprintf("%s:%d", thread.ID, item.TurnIndex) {
		t.Fatalf("PR drafts after send = %+v (%v)", sent, err)
	}
	if own, _ := app.ListDiffReviewComments(thread.ID, draft.Scope, draft.SourceKey); len(own) != 1 || own[0].Status != "draft" {
		t.Fatalf("the thread's own draft moved: %+v", own)
	}
}

// The PR-owned CRUD: pr scope only, owned by the PR key, and a sent
// draft deleted is resolved.
func TestPRDiffReviewCommentsAreThePRsOwn(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	if _, err := app.CreatePRDiffReviewComment(gitops.PRReference{Forge: "github", Host: "github.com", Repo: "widgets", Number: 5}, diffReviewPRInput("x")); err == nil {
		t.Fatal("an invalid PR reference was accepted")
	}
	workspace := diffReviewPRInput("x")
	workspace.Scope = string(store.DiffReviewScopeWorkspace)
	if _, err := app.CreatePRDiffReviewComment(diffReviewSendPR, workspace); err == nil {
		t.Fatal("a PR's draft outside pr scope was accepted")
	}
	draft, err := app.CreatePRDiffReviewComment(diffReviewSendPR, diffReviewPRInput("First."))
	if err != nil {
		t.Fatal(err)
	}
	otherPR := diffReviewSendPR
	otherPR.Number = 6
	if _, err := app.UpdatePRDiffReviewComment(otherPR, draft.ID, store.DiffReviewCommentUpdate{Body: "Stolen."}); err == nil {
		t.Fatal("another PR updated the draft")
	}
	updated, err := app.UpdatePRDiffReviewComment(diffReviewSendPR, draft.ID, store.DiffReviewCommentUpdate{Body: "Second."})
	if err != nil || updated.Body != "Second." {
		t.Fatalf("update = %+v, %v", updated, err)
	}
	if err := app.MarkPRDiffReviewCommentsSent(diffReviewSendPR, draft.Scope, draft.SourceKey, []string{draft.ID}); err != nil {
		t.Fatal(err)
	}
	if err := app.DeletePRDiffReviewComment(diffReviewSendPR, draft.ID); err != nil {
		t.Fatal(err)
	}
	// A posted draft deleted is resolved, which the list leaves out.
	if resolved, err := app.store.GetDiffReviewComment(store.PRReviewOwner(diffReviewSendPR.Key()), draft.ID); err != nil || resolved.Status != "resolved" || resolved.SentAt == 0 {
		t.Fatalf("after posting and deleting = %+v (%v), want resolved", resolved, err)
	}
	second, err := app.CreatePRDiffReviewComment(diffReviewSendPR, diffReviewPRInput("Draft."))
	if err != nil {
		t.Fatal(err)
	}
	if err := app.DeletePRDiffReviewComment(diffReviewSendPR, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.store.GetDiffReviewComment(store.PRReviewOwner(diffReviewSendPR.Key()), second.ID); err == nil {
		t.Fatal("a deleted draft is still stored")
	}
}

func TestSendPRDiffReviewCommentsNeedsThreadsOperate(t *testing.T) {
	t.Parallel()
	app := identityApp(t)
	gitOnly := pairSessionWithScopes(t, app, "thumb-git", []identity.Scope{identity.ScopeGitOperate})
	_, err := app.SendPRDiffReviewComments(callFrom(gitOnly.ID, false), diffReviewSendPR, "thread", "pr", "key", []string{"c"}, SendDiffReviewCommentsInput{})
	wantScopeRefusal(t, err, transport.ScopeThreadsOperate)
}

// raceTheClaim makes a send's claim of its drafts lose to raceSQL, run in
// its place against the claimed row (NEW.id), as a write that lands between
// the send's read of the drafts and its claim does.
func raceTheClaim(t *testing.T, dbPath, raceSQL string) {
	t.Helper()
	execOnStore(t, dbPath, `CREATE TRIGGER race_claim BEFORE UPDATE OF status ON diff_review_comments
		WHEN OLD.status = 'draft' AND NEW.status = 'sent' AND NEW.sent_turn_id <> 'elsewhere:9'
		BEGIN `+raceSQL+`; SELECT RAISE(IGNORE); END`)
}

// A PR's draft another thread's message took between this send's read and
// its claim fails this send before anything is delivered: two threads
// never both carry it.
func TestSendPRDiffReviewCommentsRefusesADraftAnotherMessageTook(t *testing.T) {
	t.Parallel()
	app, dbPath := newTestAppWithStorePath(t)
	thread := diffReviewSendThread(t, app, "thread-pr-draft-taken")
	draft, err := app.CreatePRDiffReviewComment(diffReviewSendPR, diffReviewPRInput("Name this better."))
	if err != nil {
		t.Fatal(err)
	}
	raceTheClaim(t, dbPath, `UPDATE diff_review_comments SET status = 'sent', sent_turn_id = 'elsewhere:9' WHERE id = NEW.id`)

	_, err = app.SendPRDiffReviewComments(context.Background(), diffReviewSendPR, thread.ID, draft.Scope, draft.SourceKey, []string{draft.ID}, diffReviewPRContext(draft.ID))
	if !errors.Is(err, store.ErrDiffReviewCommentsSentElsewhere) {
		t.Fatalf("SendPRDiffReviewComments = %v, want ErrDiffReviewCommentsSentElsewhere", err)
	}
	if items, _ := app.store.ListItems(thread.ID); len(items) != 0 {
		t.Fatalf("the refused send appended %d items", len(items))
	}
	// The other message's mark rode the refused claim's transaction here,
	// so it rolled back with it; what matters is that this thread's turn
	// holds nothing.
	if got, err := app.store.GetDiffReviewComment(store.PRReviewOwner(diffReviewSendPR.Key()), draft.ID); err != nil || strings.HasPrefix(got.SentTurnID, thread.ID) {
		t.Fatalf("draft = %+v (%v), want it not marked for this thread", got, err)
	}
}

// diffReviewSendEnvelope reads drafts for a message to threadID as a send
// does, before its claim.
func diffReviewSendEnvelope(t *testing.T, app *App, threadID string, source *SourceDiffReview, ids []string) resolvedUserMessage {
	t.Helper()
	resolved, err := app.resolveUserMessageEnvelope(threadID, "", userMessageInputs{
		revisionSourceDiffReview: source, revisionSourceDiffCommentIDs: ids,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return resolved
}

// An edit saved between a send's read of its drafts and its claim stays a
// draft, on a PR's drafts and a thread's own: the message carries the text
// it read, and the new text was never sent.
func TestDiffReviewClaimLeavesAnEditMadeDuringTheSendADraft(t *testing.T) {
	t.Parallel()
	for _, prOwned := range []bool{true, false} {
		app := newTestAppWithStore(t)
		thread := diffReviewSendThread(t, app, fmt.Sprintf("thread-edit-during-send-%v", prOwned))
		var draft store.DiffReviewComment
		var err error
		if prOwned {
			draft, err = app.CreatePRDiffReviewComment(diffReviewSendPR, diffReviewPRInput("Before."))
		} else {
			draft, err = app.CreateDiffReviewComment(thread.ID, diffReviewPRInput("Before."))
		}
		if err != nil {
			t.Fatal(err)
		}
		source := &SourceDiffReview{ThreadID: thread.ID, Scope: draft.Scope, SourceKey: draft.SourceKey}
		owner := store.ThreadReviewOwner(thread.ID)
		if prOwned {
			source = &SourceDiffReview{PRKey: diffReviewSendPR.Key(), Scope: draft.Scope, SourceKey: draft.SourceKey}
			owner = store.PRReviewOwner(diffReviewSendPR.Key())
		}
		resolved := diffReviewSendEnvelope(t, app, thread.ID, source, []string{draft.ID})
		if !strings.Contains(resolved.content, "Before.") {
			t.Fatalf("pr-owned %v: the message = %q, want the text it read", prOwned, resolved.content)
		}
		if prOwned {
			_, err = app.UpdatePRDiffReviewComment(diffReviewSendPR, draft.ID, store.DiffReviewCommentUpdate{Body: "After."})
		} else {
			_, err = app.UpdateDiffReviewComment(thread.ID, draft.ID, store.DiffReviewCommentUpdate{Body: "After."})
		}
		if err != nil {
			t.Fatal(err)
		}

		claim, err := app.claimDiffReviewDelivery(thread.ID, 1, resolved)
		if err != nil || len(claim.ids) != 0 {
			t.Fatalf("pr-owned %v: claim = %+v, %v; want nothing claimed", prOwned, claim, err)
		}
		if got, err := app.store.GetDiffReviewComment(owner, draft.ID); err != nil || got.Status != "draft" || got.Body != "After." {
			t.Fatalf("pr-owned %v: draft = %+v (%v), want the edit left a draft", prOwned, got, err)
		}
	}
}

// Two threads that both read a PR's draft: the one whose message claims it
// first delivers it, and the other's claim refuses.
func TestDiffReviewClaimRefusesADraftAnotherThreadDelivered(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	first := diffReviewSendThread(t, app, "thread-pr-draft-first")
	second := diffReviewSendThread(t, app, "thread-pr-draft-second")
	draft, err := app.CreatePRDiffReviewComment(diffReviewSendPR, diffReviewPRInput("Name this better."))
	if err != nil {
		t.Fatal(err)
	}
	source := &SourceDiffReview{PRKey: diffReviewSendPR.Key(), Scope: draft.Scope, SourceKey: draft.SourceKey}
	late := diffReviewSendEnvelope(t, app, second.ID, source, []string{draft.ID})

	if _, err := app.SendPRDiffReviewComments(context.Background(), diffReviewSendPR, first.ID, draft.Scope, draft.SourceKey, []string{draft.ID}, diffReviewPRContext(draft.ID)); err != nil {
		t.Fatalf("first send: %v", err)
	}
	if _, err := app.claimDiffReviewDelivery(second.ID, 1, late); !errors.Is(err, store.ErrDiffReviewCommentsSentElsewhere) {
		t.Fatalf("second claim = %v, want ErrDiffReviewCommentsSentElsewhere", err)
	}
	if got, err := app.store.GetDiffReviewComment(store.PRReviewOwner(diffReviewSendPR.Key()), draft.ID); err != nil || !strings.HasPrefix(got.SentTurnID, first.ID+":") {
		t.Fatalf("draft = %+v (%v), want it the first thread's", got, err)
	}
}

// A send that fails before its message lands hands its claimed drafts
// back.
func TestSendPRDiffReviewCommentsReturnsTheDraftsWhenTheMessageFails(t *testing.T) {
	t.Parallel()
	app, dbPath := newTestAppWithStorePath(t)
	thread := diffReviewSendThread(t, app, "thread-pr-draft-send-fails")
	draft, err := app.CreatePRDiffReviewComment(diffReviewSendPR, diffReviewPRInput("Name this better."))
	if err != nil {
		t.Fatal(err)
	}
	execOnStore(t, dbPath, `CREATE TRIGGER refuse_user_row BEFORE INSERT ON items WHEN NEW.kind = 'user_text' BEGIN SELECT RAISE(ABORT, 'injected persist failure'); END`)

	_, err = app.SendPRDiffReviewComments(context.Background(), diffReviewSendPR, thread.ID, draft.Scope, draft.SourceKey, []string{draft.ID}, diffReviewPRContext(draft.ID))
	if err == nil || !strings.Contains(err.Error(), "injected persist failure") {
		t.Fatalf("SendPRDiffReviewComments = %v, want the persist failure", err)
	}
	if got, err := app.store.GetDiffReviewComment(store.PRReviewOwner(diffReviewSendPR.Key()), draft.ID); err != nil || got.Status != "draft" || got.SentTurnID != "" {
		t.Fatalf("draft = %+v (%v), want it a draft again", got, err)
	}
}
