//go:build providersmoke

// Real-provider gate for a worktree removed OUTSIDE the app while a thread's
// session is alive in it.
//
// WHAT IS UNPROVEN WITHOUT THIS. The registry watcher and the reattach sweep
// (internal/worktreewatch, internal/app/app_worktree_watch.go) are proven
// against mock providers: the row returns to the project root, the Claude
// transcript is moved under the root slug, the idle session is restarted.
// Only the real CLIs can prove the restarted process resumes THE SAME
// conversation from the root cwd: Claude resolves `--resume` against the
// cwd's project slug, so this is the leg the relocation exists for; Codex
// resumes by thread id with the new `cwd` on `thread/resume`.
//
// COST: two real turns per provider, each trivial.
package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/kerneltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/sessionfork"
	"agent-overflow/internal/testutil"
)

const providerSmokeWorktreeRemovalBudget = 5 * time.Minute

func TestProviderSmokeExternalWorktreeRemoval(t *testing.T) {
	for _, smoke := range []providerSmokeCase{
		providerSmokeClaudeCase(),
		{providerName: string(provider.Codex), model: "gpt-5.6-luna",
			installHint: "install Codex CLI on PATH", loginHint: "run `codex login`", probeAccount: (*App).ProbeCodexAccount},
	} {
		t.Run(smoke.providerName, func(t *testing.T) {
			runProviderSmokeExternalWorktreeRemoval(t, smoke)
		})
	}
}

func runProviderSmokeExternalWorktreeRemoval(t *testing.T, smoke providerSmokeCase) {
	app, _ := setupE2EApp(t)
	app.textGenerationExecutor = kerneltest.StubTextGenerationExecutor()
	binary := preflightProviderBinary(t, app, smoke)
	preflightProviderAuth(t, app, smoke, binary)

	workspace := testutil.InitGitRepo(t)
	project, err := app.ensureProjectForWorkspace(workspace)
	if err != nil {
		t.Fatal(err)
	}
	// The production watcher, on test cadences: the removal below is seen by
	// the registry watch, not by a direct sweep call.
	startWorktreeWatchForTest(t, app)

	ctx, cancel := context.WithTimeout(context.Background(), providerSmokeWorktreeRemovalBudget)
	defer cancel()
	var claudeCleanup *providerSmokeClaudeDriver
	if smoke.providerName == string(provider.Claude) {
		claudeCleanup = newProviderSmokeClaudeDriver(t, ctx, binary, workspace, smoke.model)
	}

	// Cut the worktree where the app cuts its own, so both the registry and
	// the app's worktrees base dir are the observed places.
	const branch = "smoke-external-removal"
	worktreePath := filepath.Join(app.worktreesBaseDir(workspace), branch)
	if err := os.MkdirAll(filepath.Dir(worktreePath), 0o755); err != nil {
		t.Fatal(err)
	}
	providerSmokeGitOutput(t, workspace, "worktree", "add", "-b", branch, worktreePath)
	var worktreeSlugDir string
	if smoke.providerName == string(provider.Claude) {
		// The exact slug needs the path on disk; capture it while it exists.
		dir, err := sessionfork.WorkspaceProjectDir(testProviderProjectsDir(t), worktreePath)
		if err != nil {
			t.Fatal(err)
		}
		worktreeSlugDir = dir
	}

	thread := e2eThread(uuid.NewString(), smoke.providerName, worktreePath)
	thread.Title = "Agent Overflow external worktree removal smoke"
	thread.ProjectID = project.ID
	thread.Model = smoke.model
	thread.WorktreePath = worktreePath
	thread.Branch = branch
	thread.RuntimeMode = string(provider.RuntimeReadOnly)
	if smoke.providerName == string(provider.Claude) {
		thread.ContextWindow = 200000
	}
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		row, err := app.store.GetThread(thread.ID)
		if err != nil {
			t.Error(err)
		} else if row.SessionRef != "" {
			if claudeCleanup != nil {
				claudeCleanup.trackSession(row.SessionRef)
			}
			t.Logf("native %s smoke session: %s", smoke.providerName, row.SessionRef)
		}
		if err := app.StopSession(thread.ID); err != nil {
			t.Errorf("stop smoke session: %v", err)
		}
		if worktreeSlugDir != "" {
			_ = os.Remove(filepath.Join(worktreeSlugDir, "memory"))
			if err := os.Remove(worktreeSlugDir); err != nil && !os.IsNotExist(err) {
				t.Errorf("remove worktree slug dir %s: %v", worktreeSlugDir, err)
			}
		}
	})

	send := func(prompt string) string {
		t.Helper()
		item, err := app.sendMessageWithOptions(ctx, thread.ID, prompt, sendMessageOptions{SendID: uuid.NewString()})
		if err != nil {
			t.Fatal(err)
		}
		return waitProviderSmokeMessage(t, ctx, app, thread.ID, item.ID)
	}

	// Turn 1 runs in the worktree and leaves the session alive there.
	codeword := providerSmokeCodeword()
	send(providerSmokeCodewordPrompt(codeword))
	inWorktree, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if inWorktree.SessionRef == "" {
		t.Fatal("EXTERNAL REMOVAL FAILED: no session ref recorded after the first turn")
	}
	live, ok := app.sessionManager().get(thread.ID)
	if !ok {
		t.Fatal("EXTERNAL REMOVAL FAILED: no live session after the first turn")
	}

	// The removal happens in a terminal. Nothing tells the app.
	providerSmokeGitOutput(t, workspace, "worktree", "remove", "--force", worktreePath)

	// The watcher moves the row to the root and restarts the idle session
	// from there.
	deadline := time.Now().Add(30 * time.Second)
	for {
		row, err := app.store.GetThread(thread.ID)
		if err != nil {
			t.Fatal(err)
		}
		atRoot := samePath(row.WorkspacePath, workspace) && row.WorktreePath == "" && row.Branch == "main"
		restarted, present := app.sessionManager().get(thread.ID)
		_, starting := app.sessionManager().startState(thread.ID)
		if atRoot && present && !starting && restarted.Token != live.Token {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("EXTERNAL REMOVAL FAILED: row workspace=%q worktree=%q branch=%q; session present=%v starting=%v token=%q (was %q)",
				row.WorkspacePath, row.WorktreePath, row.Branch, present, starting, restarted.Token, live.Token)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if smoke.providerName == string(provider.Claude) {
		located, err := sessionfork.LocateSessionFile(testProviderProjectsDir(t), inWorktree.SessionRef, workspace)
		if err != nil {
			t.Fatalf("EXTERNAL REMOVAL FAILED: transcript for %s not locatable from %s: %v", inWorktree.SessionRef, workspace, err)
		}
		if rootSlug, err := sessionfork.WorkspaceProjectDir(testProviderProjectsDir(t), workspace); err != nil || !samePath(filepath.Dir(located), rootSlug) {
			t.Fatalf("EXTERNAL REMOVAL FAILED: transcript sits at %s, want under the root slug %s (err=%v)", located, rootSlug, err)
		}
	}

	// Turn 2 runs on the restarted process from the root and must continue
	// the same conversation: the codeword only exists in the first turn.
	const recall = "What codeword did I ask you to remember? Reply with the codeword and nothing else. Do not use tools."
	answer := send(recall)
	if !strings.Contains(strings.ToUpper(answer), codeword) {
		t.Fatalf("EXTERNAL REMOVAL FAILED: the restarted session did not continue the conversation: reply=%q want=%s", answer, codeword)
	}
	resumed, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.SessionRef != inWorktree.SessionRef {
		t.Fatalf("EXTERNAL REMOVAL FAILED: restart from the root minted session %q instead of continuing %q", resumed.SessionRef, inWorktree.SessionRef)
	}
	t.Logf("external removal: %s resumed %s from the project root and recalled the codeword", smoke.providerName, resumed.SessionRef)
}
