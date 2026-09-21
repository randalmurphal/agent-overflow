//go:build providersmoke

package app

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/kerneltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
)

// TestProviderSmokeForkContinuity checks native context, not just the fork RPC
// arguments. Like the other manual provider gates, it uses a temporary AO
// database and workspace, with the installed CLI's authentication and history.
func TestProviderSmokeForkContinuity(t *testing.T) {
	claude := providerSmokeClaudeCase()
	claude.model = "claude-haiku-4-5"
	for _, smoke := range []providerSmokeCase{
		claude,
		{providerName: string(provider.Codex), model: "gpt-5.6-luna",
			installHint: "install Codex CLI on PATH", loginHint: "run `codex login`", probeAccount: (*App).ProbeCodexAccount},
	} {
		t.Run(smoke.providerName, func(t *testing.T) {
			app, _ := setupE2EApp(t)
			app.textGenerationExecutor = kerneltest.StubTextGenerationExecutor()
			binary := preflightProviderBinary(t, app, smoke)
			preflightProviderAuth(t, app, smoke, binary)
			workspace := testutil.InitGitRepo(t)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
			defer cancel()
			var cleanup *providerSmokeClaudeDriver
			if smoke.providerName == string(provider.Claude) {
				cleanup = newProviderSmokeClaudeDriver(t, ctx, binary, workspace, smoke.model)
			}
			track := func(thread store.Thread) {
				t.Helper()
				t.Cleanup(func() {
					row, err := app.store.GetThread(thread.ID)
					if err != nil {
						t.Error(err)
					} else if row.SessionRef != "" {
						t.Logf("native %s fork smoke session: %s", smoke.providerName, row.SessionRef)
						if cleanup != nil {
							cleanup.trackSession(row.SessionRef)
						}
					}
					if err := app.StopSession(thread.ID); err != nil {
						t.Errorf("stop fork smoke session: %v", err)
					}
				})
			}
			source := e2eThread(uuid.NewString(), smoke.providerName, workspace)
			source.Title = "Agent Overflow fork continuity smoke"
			source.Model = smoke.model
			source.RuntimeMode = string(provider.RuntimeReadOnly)
			if smoke.providerName == string(provider.Claude) {
				source.ContextWindow = 200000
			}
			if err := app.store.CreateThread(source); err != nil {
				t.Fatal(err)
			}
			track(source)
			send := func(t *testing.T, thread store.Thread, prompt string) (store.Item, string) {
				t.Helper()
				item, err := app.sendMessageWithOptions(ctx, thread.ID, prompt, sendMessageOptions{SendID: uuid.NewString()})
				if err != nil {
					t.Fatal(err)
				}
				return item, waitProviderSmokeMessage(t, ctx, app, thread.ID, item.ID)
			}
			kept, discarded := providerSmokeCodeword(), providerSmokeCodeword()
			first, _ := send(t, source, providerSmokeCodewordPrompt(kept))
			prepared := 0
			for {
				n, err := app.store.PrepareThreadHistory(ctx, source.ID)
				if err != nil {
					t.Fatal(err)
				}
				if n == 0 {
					break
				}
				prepared += n
			}
			if prepared == 0 {
				t.Fatal("smoke must exercise shared history")
			}
			tail, err := app.ForkThread(ctx, source.ID, nil)
			if err != nil {
				t.Fatal(err)
			}
			track(tail)
			second, _ := send(t, source, providerSmokeCodewordPrompt(discarded))
			anchored, err := app.ForkThread(ctx, source.ID, &first.TurnIndex)
			if err != nil {
				t.Fatal(err)
			}
			track(anchored)
			before, err := app.ForkThreadFromMessage(ctx, source.ID, second.ID)
			if err != nil {
				t.Fatal(err)
			}
			track(before)
			grandchild, err := app.ForkThread(ctx, tail.ID, nil)
			if err != nil {
				t.Fatal(err)
			}
			track(grandchild)
			const recall = "What is the MOST RECENT codeword I asked you to remember? Reply with only that single codeword. Do not use tools."
			for i, fork := range []store.Thread{tail, anchored, before, grandchild} {
				t.Run(fmt.Sprintf("branch-%d", i), func(t *testing.T) {
					if fork.ForkPreparing {
						t.Fatal("fork returned before preparation completed")
					}
					if _, exists, err := app.store.GetThreadItem(fork.ID, second.ID); err != nil || exists {
						t.Fatalf("displayed fork contains excluded message: exists=%v err=%v", exists, err)
					}
					_, answer := send(t, fork, recall)
					assertProviderSmokeRecall(t, answer, kept, discarded)
				})
			}
			_, answer := send(t, source, recall)
			assertProviderSmokeRecall(t, answer, discarded, kept)

			// The source must actually be streaming when the fork starts, rather
			// than merely have an open process after an already completed turn.
			activeWord, laterWord := providerSmokeCodeword(), providerSmokeCodeword()
			prompt := fmt.Sprintf("Remember the new codeword %s. Do not use tools. Start your response with that codeword, then write every integer from 1 through 400 on its own line, without skipping any.", activeWord)
			streaming, err := app.sendMessageWithOptions(ctx, source.ID, prompt, sendMessageOptions{SendID: uuid.NewString()})
			if err != nil {
				t.Fatal(err)
			}
			waitProviderSmokeAssistantStreaming(t, ctx, app, source.ID, streaming.TurnIndex)
			if turn, active, err := app.store.GetActiveTurn(source.ID); err != nil || !active || turn.TurnIndex != streaming.TurnIndex {
				t.Fatalf("source must still be running at fork: active=%v turn=%+v err=%v", active, turn, err)
			}
			liveFork, err := app.ForkThread(ctx, source.ID, nil)
			if err != nil {
				t.Fatal(err)
			}
			track(liveFork)
			waitProviderSmokeMessage(t, ctx, app, source.ID, streaming.ID)
			later, _ := send(t, source, providerSmokeCodewordPrompt(laterWord))
			if _, exists, err := app.store.GetThreadItem(liveFork.ID, later.ID); err != nil || exists {
				t.Fatalf("live fork contains a later source turn: exists=%v err=%v", exists, err)
			}
			_, answer = send(t, liveFork, recall)
			assertProviderSmokeRecall(t, answer, activeWord, laterWord)
			t.Log("live fork resumed the active prompt and excluded subsequent source turns; source completed uninterrupted")
		})
	}
}
