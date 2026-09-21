//go:build providersmoke

package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/kerneltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/usermessage"
)

// TestProviderSmokeQueueVisibility exercises native mid-turn consumption with
// cheap models. Reload recovery reads the same live snapshot as a new client;
// browser rendering is covered by the mocked send-queue-handover suite.
func TestProviderSmokeQueueVisibility(t *testing.T) {
	claudeCase := providerSmokeClaudeCase()
	claudeCase.model = "claude-haiku-4-5"
	for _, smoke := range []providerSmokeCase{
		{providerName: string(provider.Codex), model: "gpt-5.6-luna", installHint: "install Codex CLI on PATH", loginHint: "run `codex login`", probeAccount: (*App).ProbeCodexAccount},
		claudeCase,
	} {
		t.Run(smoke.providerName, func(t *testing.T) {
			app, _ := setupE2EApp(t)
			app.textGenerationExecutor = kerneltest.StubTextGenerationExecutor()
			app.configureTriageQueueCallbacks()
			collector := &providerSmokeCollector{}
			app.triage.SetEventHook(collector.observeProviderEvent)
			binary := preflightProviderBinary(t, app, smoke)
			preflightProviderAuth(t, app, smoke, binary)
			workspace := testutil.InitGitRepo(t)
			thread := e2eThread(uuid.NewString(), smoke.providerName, workspace)
			thread.Title = "Live queue visibility smoke"
			thread.Model = smoke.model
			if smoke.providerName == string(provider.Codex) {
				thread.ReasoningEffort = "low"
			}
			thread.ContextWindow = 200000
			thread.RuntimeMode = string(provider.RuntimeReadOnly)
			if err := app.store.CreateThread(thread); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			var claudeCleanup *providerSmokeClaudeDriver
			if smoke.providerName == string(provider.Claude) {
				claudeCleanup = newProviderSmokeClaudeDriver(t, ctx, binary, workspace, smoke.model)
			}
			t.Cleanup(func() {
				if t.Failed() {
					dumpProviderSmokeMergeState(t, app, collector, thread.ID)
				}
				current, err := app.store.GetThread(thread.ID)
				if err != nil {
					t.Error(err)
				} else if claudeCleanup != nil && current.SessionRef != "" {
					claudeCleanup.trackSession(current.SessionRef)
				}
				if err := app.StopSession(thread.ID); err != nil {
					t.Errorf("stop smoke session: %v", err)
				}
			})
			first, err := app.sendMessageWithOptions(ctx, thread.ID,
				"This is a scripted queue test. Do not use tools. Count from 1 to 600, one number per line, without omissions. Write nothing else.",
				sendMessageOptions{SendID: uuid.NewString()})
			if err != nil {
				t.Fatal(err)
			}
			waitProviderSmokeAssistantStreaming(t, ctx, app, thread.ID, first.TurnIndex)
			sendIDs := []string{uuid.NewString(), uuid.NewString()}
			messages := []string{
				"QUEUE_ALPHA: When you receive this message, reply ACK_ALPHA. Do not use tools.",
				"QUEUE_BRAVO: When you receive this message, reply ACK_BRAVO. Do not use tools.",
			}
			pendingObserved := false
			for i, message := range messages {
				live, err := app.GetThreadLiveState(thread.ID)
				if err != nil || live.ActiveTurn == nil {
					t.Fatalf("send %d missed the active turn: %+v, %v", i, live, err)
				}
				if _, err := app.RegisterQueueItem(ctx, thread.ID, message, SendMessageOptions{SendID: sendIDs[i], ReconcileBySendID: true}); err != nil {
					t.Fatal(err)
				}
				live, err = app.GetThreadLiveState(thread.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, queued := range live.QueueItems {
					if strings.Contains(queued.Message, message) {
						pendingObserved = true
					}
				}
				for _, flushed := range live.FlushedItems {
					if strings.Contains(flushed.Message, message) {
						pendingObserved = true
					}
				}
			}
			if !pendingObserved {
				t.Fatal("provider consumed both sends before a pending snapshot was observed")
			}
			// Sample independently, as reload does. Every accepted message must be in
			// the live queue or represented by its confirmed native echo, never absent.
			ticks := time.NewTicker(20 * time.Millisecond)
			defer ticks.Stop()
			samples := 0
			for {
				live, err := app.GetThreadLiveState(thread.ID)
				if err != nil {
					t.Fatal(err)
				}
				rows, err := app.store.ListItems(thread.ID)
				if err != nil {
					t.Fatal(err)
				}
				allConsumed := true
				for i, message := range messages {
					pending := false
					for _, queued := range live.QueueItems {
						pending = pending || strings.Contains(queued.Message, message)
					}
					for _, flushed := range live.FlushedItems {
						pending = pending || strings.Contains(flushed.Message, message)
					}
					count := 0
					for _, row := range rows {
						if row.Kind == "user_text" && strings.Contains(row.Summary, message) && usermessage.ReadProviderItemID(row.Meta) != "" {
							count++
						}
					}
					if count > 1 || (!pending && count == 0) {
						t.Fatalf("send %d lost or duplicated: pending=%v confirmed rows=%d", i, pending, count)
					}
					allConsumed = allConsumed && count == 1
				}
				samples++
				if allConsumed && live.ActiveTurn == nil && len(live.QueueItems) == 0 && len(live.FlushedItems) == 0 {
					for _, sendID := range sendIDs {
						record, accepted, err := app.findRecordedSend(thread.ID, sendID)
						if err != nil || !accepted || record.item.Kind != "user_text" {
							t.Fatalf("send identity missing: accepted=%v err=%v", accepted, err)
						}
					}
					var answer strings.Builder
					for _, row := range rows {
						if row.Kind == "assistant_text" {
							answer.WriteString(row.Summary)
						}
					}
					if !strings.Contains(answer.String(), "ACK_BRAVO") {
						t.Fatalf("provider never acknowledged the final follow-up: %s", providerSmokeTruncate(answer.String()))
					}
					t.Logf("%s: model=%s; pending observed; %d reconnect snapshots; both native echoes exactly once; final acknowledgement received; queue empty", smoke.providerName, smoke.model, samples)
					break
				}
				select {
				case <-ticks.C:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			current, err := app.store.GetThread(thread.ID)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("native smoke session: %s", current.SessionRef)
		})
	}
}
