//go:build providersmoke

package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/kerneltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/triage"
)

// TestProviderSmokeClaudeForkedSkill drives forked skills through the real
// CLI (claude-wire.md §E9): a fork the model launches settles in place as
// an awaited agent with its own task, and a fork stopped by that task, as
// its agent pane's Stop does, settles as errored, whether the model
// launched it or the user typed it as a command. The scripted suites
// replay captures of this wire; only this gate proves the CLI still
// sends it.
//
// Haiku is deliberate: the fork lifecycle is the CLI's, not the model's.
// Full access lets the slow skill run its shell sleeps unattended.
func TestProviderSmokeClaudeForkedSkill(t *testing.T) {
	smoke := providerSmokeClaudeCase()
	smoke.model = "claude-haiku-4-5"
	app, _ := setupE2EApp(t)
	app.textGenerationExecutor = kerneltest.StubTextGenerationExecutor()
	app.configureTriageQueueCallbacks()
	collector := &providerSmokeCollector{}
	app.triage.SetEventHook(collector.observeProviderEvent)
	binary := preflightProviderBinary(t, app, smoke)
	preflightProviderAuth(t, app, smoke, binary)
	workspace := testutil.InitGitRepo(t)
	writeProviderSmokeSkill(t, workspace, "smoke-fork",
		"Do exactly these steps:\n1. Run `echo fork-ok` with the Bash tool.\n2. Reply with one line: fork-ok.\n")
	writeProviderSmokeSkill(t, workspace, "smoke-slow-fork",
		"Do exactly these steps, one tool call at a time:\n"+
			"1. Run `python3 -c \"import time; time.sleep(4)\"; echo step1` with the Bash tool.\n"+
			"2. Run `python3 -c \"import time; time.sleep(60)\"; echo step2` with the Bash tool.\n"+
			"3. Reply with one line: slow-done.\n")
	thread := e2eThread(uuid.NewString(), smoke.providerName, workspace)
	thread.Title = "Agent Overflow forked skill smoke"
	thread.Model = smoke.model
	thread.ContextWindow = 200000
	thread.RuntimeMode = string(provider.RuntimeFullAccess)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	driver := newProviderSmokeClaudeDriver(t, ctx, binary, workspace, smoke.model)
	t.Cleanup(func() {
		if row, err := app.store.GetThread(thread.ID); err == nil && row.SessionRef != "" {
			driver.trackSession(row.SessionRef)
		}
		if err := app.StopSession(thread.ID); err != nil {
			t.Errorf("stop smoke session: %v", err)
		}
	})
	send := func(content string) store.Item {
		t.Helper()
		row, err := app.sendMessageWithOptions(ctx, thread.ID, content, sendMessageOptions{
			SendID: uuid.NewString(), ExpandComposerCommands: true,
		})
		if err != nil {
			t.Fatalf("send %q: %v", content, err)
		}
		return row
	}

	finished := send("Use the Skill tool to run the smoke-fork skill. Do nothing else.")
	waitProviderSmokeTurnCompleted(t, ctx, app, thread.ID, finished.TurnIndex)
	fork := providerSmokeForkRow(t, app, thread.ID, finished.TurnIndex)
	if fork.Status != "completed" || fork.IsBackground || triage.TaskIDFromItemMeta(fork.Meta) == "" {
		t.Fatalf("finished fork = status %q background %v meta %s, want completed in place with its task", fork.Status, fork.IsBackground, fork.Meta)
	}
	if _, exists, err := app.store.GetThreadItem(thread.ID, triage.ToolCompletionID(fork.ID)); err != nil || exists {
		t.Fatalf("finished fork has a background completion sibling: exists=%v err=%v", exists, err)
	}
	t.Logf("a fork the model launched settled in place with task %s", triage.TaskIDFromItemMeta(fork.Meta))

	for _, tc := range []struct{ name, prompt string }{
		{"launched", "Use the Skill tool to run the smoke-slow-fork skill. Do nothing else."},
		{"typed", "/smoke-slow-fork"},
	} {
		row := send(tc.prompt)
		running := waitProviderSmokeForkWorking(t, ctx, app, thread.ID, row.TurnIndex)
		taskID := triage.TaskIDFromItemMeta(running.Meta)
		if sent, err := app.StopClaudeTask(thread.ID, taskID); err != nil || !sent {
			t.Fatalf("%s fork: StopClaudeTask(%s) = %v, %v", tc.name, taskID, sent, err)
		}
		waitProviderSmokeTurnCompleted(t, ctx, app, thread.ID, row.TurnIndex)
		stopped := mustProviderSmokeItem(t, app, thread.ID, running.ID)
		if stopped.Status != "errored" || stopped.IsBackground {
			t.Fatalf("%s fork after Stop = status %q background %v, want errored in place", tc.name, stopped.Status, stopped.IsBackground)
		}
		t.Logf("a %s fork stopped by its task settled as errored", tc.name)
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Status == "running" && item.CompletionOf == "" && !item.IsBackground {
			t.Fatalf("row %s (%s %s) still running after every turn ended", item.ID, item.Kind, item.ToolName)
		}
	}
}

func writeProviderSmokeSkill(t *testing.T, workspace, name, body string) {
	t.Helper()
	dir := filepath.Join(workspace, ".claude", "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	skill := "---\nname: " + name + "\ndescription: Provider smoke skill. Use only when explicitly asked to run " + name +
		".\ncontext: fork\nmodel: haiku\n---\n" + body
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(skill), 0o644); err != nil {
		t.Fatal(err)
	}
}

// providerSmokeForkRow is the turn's Skill launch row.
func providerSmokeForkRow(t *testing.T, app *App, threadID string, turnIndex int) store.Item {
	t.Helper()
	items, err := app.store.ListItems(threadID)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.TurnIndex == turnIndex && item.Kind == "tool_call" && item.ToolName == "Skill" {
			return item
		}
	}
	t.Fatalf("turn %d has no Skill row", turnIndex)
	return store.Item{}
}

// waitProviderSmokeForkWorking returns the turn's running fork once it
// carries its task and has run a tool call of its own.
func waitProviderSmokeForkWorking(t *testing.T, ctx context.Context, app *App, threadID string, turnIndex int) store.Item {
	t.Helper()
	ticker := time.NewTicker(providerSmokePollInterval)
	defer ticker.Stop()
	for {
		items, err := app.store.ListItems(threadID)
		if err != nil {
			t.Fatal(err)
		}
		for _, fork := range items {
			if fork.TurnIndex != turnIndex || fork.Kind != "tool_call" || fork.ToolName != "Skill" ||
				fork.Status != "running" || triage.TaskIDFromItemMeta(fork.Meta) == "" {
				continue
			}
			for _, child := range items {
				if child.ParentID == fork.ID && child.Kind == "tool_call" {
					return fork
				}
			}
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("waiting for turn %d's fork to run: %v", turnIndex, ctx.Err())
		}
	}
}

func waitProviderSmokeTurnCompleted(t *testing.T, ctx context.Context, app *App, threadID string, turnIndex int) {
	t.Helper()
	ticker := time.NewTicker(providerSmokePollInterval)
	defer ticker.Stop()
	for {
		turns, err := app.store.ListRecentTurns(threadID, 8)
		if err != nil {
			t.Fatal(err)
		}
		for _, turn := range turns {
			if turn.TurnIndex == turnIndex && turn.CompletedAt != nil {
				return
			}
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("waiting for turn %d to complete: %v", turnIndex, ctx.Err())
		}
	}
}
