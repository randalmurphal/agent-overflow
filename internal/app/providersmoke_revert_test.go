//go:build providersmoke

package app

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/kerneltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
)

// TestProviderSmokeRevertFlows verifies native conversation continuity, which
// scripted providers cannot attest. Each leg uses a temporary database/repo,
// four short answered turns and one immediately interrupted send. Authentication
// and native transcript storage use the installed CLI's normal provider home.
func TestProviderSmokeRevertFlows(t *testing.T) {
	for _, smoke := range []providerSmokeCase{
		providerSmokeClaudeCase(),
		{providerName: string(provider.Codex), model: "gpt-5.6-luna",
			installHint: "install Codex CLI on PATH", loginHint: "run `codex login`", probeAccount: (*App).ProbeCodexAccount},
	} {
		t.Run(smoke.providerName, func(t *testing.T) {
			app, _ := setupE2EApp(t)
			// Titles are unrelated to this manual gate and should cost no extra turns.
			app.textGenerationExecutor = kerneltest.StubTextGenerationExecutor()
			binary := preflightProviderBinary(t, app, smoke)
			preflightProviderAuth(t, app, smoke, binary)
			workspace := testutil.InitGitRepo(t)
			thread := e2eThread(uuid.NewString(), smoke.providerName, workspace)
			thread.Title = "Agent Overflow revert smoke"
			thread.Model = smoke.model
			if smoke.providerName == string(provider.Claude) {
				thread.ContextWindow = 200000 // This tiny smoke must not require the 1M usage-credit tier.
			}
			thread.RuntimeMode = string(provider.RuntimeReadOnly)
			if err := app.store.CreateThread(thread); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
			defer cancel()
			var claudeCleanup *providerSmokeClaudeDriver
			if smoke.providerName == string(provider.Claude) {
				claudeCleanup = newProviderSmokeClaudeDriver(t, ctx, binary, workspace, smoke.model)
			}
			track := func() {
				row, err := app.store.GetThread(thread.ID)
				if err != nil {
					t.Error(err)
					return
				}
				if row.SessionRef != "" {
					if claudeCleanup != nil {
						claudeCleanup.trackSession(row.SessionRef)
					}
					t.Logf("native %s smoke session: %s", smoke.providerName, row.SessionRef)
				}
			}
			t.Cleanup(func() {
				track()
				if err := app.StopSession(thread.ID); err != nil {
					t.Errorf("stop smoke session: %v", err)
				}
			})
			send := func(prompt string) store.Item {
				t.Helper()
				item, err := app.sendMessageWithOptions(ctx, thread.ID, prompt, sendMessageOptions{SendID: uuid.NewString()})
				if err != nil {
					t.Fatalf("send: %v", err)
				}
				return item
			}
			wait := func(item store.Item) string {
				t.Helper()
				text := waitProviderSmokeMessage(t, ctx, app, thread.ID, item.ID)
				track()
				return text
			}
			kept, discarded := providerSmokeCodeword(), providerSmokeCodeword()
			wait(send(providerSmokeCodewordPrompt(kept)))
			old := send(providerSmokeCodewordPrompt(discarded))
			wait(old)
			const wip = "Unsent composer work"
			if err := app.SaveDraft(ctx, thread.ID, wip, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			const recall = "What codeword did I ask you to remember? Reply with the codeword and nothing else. Do not use tools."
			resendID := uuid.NewString()
			result, err := app.RevertConversationAndResendMessage(ctx, thread.ID, old.ID, RevertAndResendOptions{SendID: resendID, Content: recall})
			if err != nil || result.Failure != "" || result.Warning != "" {
				t.Fatalf("replacement: %+v err=%v", result, err)
			}
			if result.Cut == nil || result.Cut.Replacement == nil {
				t.Fatalf("replacement lacks atomic cut: %+v", result)
			}
			replacement := *result.Cut.Replacement
			answer := wait(replacement)
			assertProviderSmokeRecall(t, answer, kept, discarded)
			if _, exists, err := app.store.GetThreadItem(thread.ID, old.ID); err != nil || exists {
				t.Fatalf("old message survived cut: exists=%v err=%v", exists, err)
			}
			draft, _, err := app.store.GetThreadDraft(thread.ID)
			if err != nil || draft.Content != wip {
				t.Fatalf("replacement overwrote composer: %+v err=%v", draft, err)
			}
			// An accepted SendID must return without another native turn or cut.
			if retry, err := app.RevertConversationAndResendMessage(ctx, thread.ID, old.ID, RevertAndResendOptions{SendID: resendID, Content: recall}); err != nil || retry.Cut != nil {
				t.Fatalf("duplicate replacement: %+v err=%v", retry, err)
			}
			t.Log("older-message replacement preserved the native prefix, composer WIP and send identity")

			undoID := uuid.NewString()
			undoneCodeword := providerSmokeCodeword()
			raw := "  " + providerSmokeCodewordPrompt(undoneCodeword) + "\n"
			live, hasLiveCodex := app.activeCodexSession(thread.ID)
			expectLiveRevert := hasLiveCodex && live.SupportsThreadRevert()
			sent, err := app.sendMessageWithOptions(ctx, thread.ID, strings.TrimSpace(raw), sendMessageOptions{SendID: undoID})
			if err != nil {
				t.Fatal(err)
			}
			if expectLiveRevert {
				// Wait only for the native start anchor so this leg exercises the
				// live cut, rather than the pre-start fork fallback.
				waitProviderSmokeRevertAnchor(t, ctx, app, thread.ID, sent.TurnIndex)
			}
			undone, err := app.InterruptAndRevertIfClean(thread.ID, InterruptRevertOptions{ExpectedSendID: undoID, Draft: &DraftSnapshot{Content: raw}})
			if err != nil || !undone.Reverted {
				t.Fatalf("early Stop: %+v err=%v", undone, err)
			}
			if expectLiveRevert {
				if active, ok := app.activeCodexSession(thread.ID); !ok || active != live {
					t.Fatal("early Stop replaced the live Codex session")
				}
			}
			track()
			if _, accepted, err := app.findRecordedSend(thread.ID, undoID); err != nil || accepted {
				t.Fatalf("un-sent row remains: accepted=%v err=%v", accepted, err)
			}
			draft, _, err = app.store.GetThreadDraft(thread.ID)
			if err != nil || draft.Content != raw {
				t.Fatalf("early Stop lost raw draft: %+v err=%v", draft, err)
			}
			assertProviderSmokeRecall(t, wait(send(recall)), kept, undoneCodeword)
			t.Log("early Stop restored the raw draft; a subsequent native turn retained the correct history")
		})
	}
}

func waitProviderSmokeRevertAnchor(t *testing.T, ctx context.Context, app *App, threadID string, turnIndex int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		anchor, found, err := app.resolveCodexRevertAnchor(threadID, turnIndex)
		if err != nil {
			t.Fatal(err)
		}
		if found && anchor != "" {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("waiting for native turn %d start anchor: %v", turnIndex, ctx.Err())
		}
	}
}

func waitProviderSmokeMessage(t *testing.T, ctx context.Context, app *App, threadID, itemID string) string {
	t.Helper()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		item, exists, err := app.store.GetThreadItem(threadID, itemID)
		if err != nil {
			t.Fatal(err)
		}
		if exists {
			turns, err := app.store.ListRecentTurns(threadID, 8)
			if err != nil {
				t.Fatal(err)
			}
			for _, turn := range turns {
				if turn.TurnIndex != item.TurnIndex || turn.CompletedAt == nil {
					continue
				}
				if turn.ErrorMessage != "" || turn.StopReason == "interrupted" {
					t.Fatalf("provider turn failed: %+v", turn)
				}
				items, err := app.store.ListItems(threadID)
				if err != nil {
					t.Fatal(err)
				}
				var answer strings.Builder
				for _, row := range items {
					if row.TurnIndex == turn.TurnIndex && row.Kind == "assistant_text" && row.ParentID == "" {
						answer.WriteString(row.Summary)
					}
				}
				if answer.Len() > 0 {
					return answer.String()
				}
			}
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			turns, err := app.store.ListRecentTurns(threadID, 8)
			if err != nil {
				t.Errorf("read turns after timeout: %v", err)
			}
			t.Logf("waiting user item: %+v; recent turns: %+v", item, turns)
			t.Fatal(fmt.Errorf("waiting for native reply to %s: %w", itemID, ctx.Err()))
		}
	}
}

func assertProviderSmokeRecall(t *testing.T, answer, kept, discarded string) {
	t.Helper()
	upper := strings.ToUpper(answer)
	if !strings.Contains(upper, kept) || strings.Contains(upper, discarded) {
		t.Fatalf("native rollback resumed the wrong history: reply=%q want=%s must-not-contain=%s", answer, kept, discarded)
	}
}
