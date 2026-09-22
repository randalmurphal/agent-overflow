//go:build providersmoke

// Real-provider gate for the PROVIDER-DRIVEN checkout move (Claude only).
//
// WHAT IS UNPROVEN WITHOUT THIS. Claude's `EnterWorktree` / `ExitWorktree`
// move the CLI's own working directory mid-session, and the thread row
// follows them from the tool's structured `tool_use_result`
// (internal/app/app_worktree_follow.go). The mocked suites prove the parser
// and the follow against hand-written results; only the real CLI can prove
// the sibling is on the wire under production's flags, that the worktree the
// CLI cuts is one the row can represent, and that a stopped session resumes
// from the row's workspace afterwards (the CLI relocates the transcript
// between project slugs as it moves, verified 2.1.257).
//
// COST: three real turns on the default model, each trivial.
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
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
)

const providerSmokeWorktreeBudget = 5 * time.Minute

func TestProviderSmokeClaudeWorktreeFollow(t *testing.T) {
	smoke := providerSmokeClaudeCase()
	app, _ := setupE2EApp(t)
	app.textGenerationExecutor = kerneltest.StubTextGenerationExecutor()
	binary := preflightProviderBinary(t, app, smoke)
	preflightProviderAuth(t, app, smoke, binary)

	workspace := testutil.InitGitRepo(t)
	project, err := app.ensureProjectForWorkspace(workspace)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), providerSmokeWorktreeBudget)
	defer cancel()
	cleanup := newProviderSmokeClaudeDriver(t, ctx, binary, workspace, smoke.model)

	const worktreeName = "smoke-follow"
	worktreePath := filepath.Join(workspace, ".claude", "worktrees", worktreeName)
	var worktreeSlugDir string

	thread := e2eThread(uuid.NewString(), smoke.providerName, workspace)
	thread.Title = "Agent Overflow worktree follow smoke"
	thread.ProjectID = project.ID
	thread.Model = smoke.model
	thread.Branch = "main"
	// Full access: `EnterWorktree` is not on the read-only tier's auto-allow
	// list, and an unattended turn has nobody to answer an ask.
	thread.RuntimeMode = string(provider.RuntimeFullAccess)
	thread.ContextWindow = 200000
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		row, err := app.store.GetThread(thread.ID)
		if err != nil {
			t.Error(err)
		} else if row.SessionRef != "" {
			cleanup.trackSession(row.SessionRef)
		}
		if err := app.StopSession(thread.ID); err != nil {
			t.Errorf("stop worktree smoke session: %v", err)
		}
		// The CLI leaves the worktree's emptied project slug dir behind
		// after it moves the transcript back; it is not a transcript, so the
		// driver's teardown does not know about it. The slug was captured
		// while the worktree existed: the exact slug needs the path on disk.
		if worktreeSlugDir != "" {
			_ = os.Remove(filepath.Join(worktreeSlugDir, "memory"))
			if err := os.Remove(worktreeSlugDir); err != nil && !os.IsNotExist(err) {
				t.Errorf("remove worktree slug dir %s: %v", worktreeSlugDir, err)
			}
		}
	})

	send := func(t *testing.T, prompt string) string {
		t.Helper()
		item, err := app.sendMessageWithOptions(ctx, thread.ID, prompt, sendMessageOptions{SendID: uuid.NewString()})
		if err != nil {
			t.Fatal(err)
		}
		return waitProviderSmokeMessage(t, ctx, app, thread.ID, item.ID)
	}
	waitRow := func(t *testing.T, what string, want func(store.Thread) bool) store.Thread {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			row, err := app.store.GetThread(thread.ID)
			if err != nil {
				t.Fatal(err)
			}
			if want(row) {
				return row
			}
			if time.Now().After(deadline) {
				t.Fatalf("WORKTREE FOLLOW FAILED: %s; row workspace=%q worktree=%q branch=%q",
					what, row.WorkspacePath, row.WorktreePath, row.Branch)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	// Turn 1: the CLI cuts and enters its worktree. The row must follow while
	// the SAME session keeps running.
	send(t, "This is a scripted client test. Call the EnterWorktree tool with name "+worktreeName+". After it returns, reply with exactly: DONE")
	live, ok := app.sessionManager().get(thread.ID)
	if !ok {
		t.Fatal("WORKTREE FOLLOW FAILED: no live session after the EnterWorktree turn")
	}
	entered := waitRow(t, "the row did not follow EnterWorktree into "+worktreePath, func(r store.Thread) bool {
		return samePath(r.WorkspacePath, worktreePath) && samePath(r.WorktreePath, worktreePath)
	})
	if entered.Branch != "worktree-"+worktreeName {
		t.Errorf("WORKTREE FOLLOW FAILED: branch = %q, want worktree-%s", entered.Branch, worktreeName)
	}
	if dir, err := sessionfork.WorkspaceProjectDir(testProviderProjectsDir(t), worktreePath); err != nil {
		t.Errorf("WORKTREE FOLLOW FAILED: no project slug for the worktree the CLI entered: %v", err)
	} else {
		worktreeSlugDir = dir
	}
	if !strings.Contains(providerSmokeGitOutput(t, workspace, "worktree", "list"), worktreePath) {
		t.Errorf("WORKTREE FOLLOW FAILED: git does not list %s as a worktree of %s", worktreePath, workspace)
	}
	if after, ok := app.sessionManager().get(thread.ID); !ok || after.Token != live.Token {
		t.Errorf("WORKTREE FOLLOW FAILED: the follow replaced the live session (before=%q after=%q present=%v)", live.Token, after.Token, ok)
	}

	// Turn 2: leave and delete it. The row returns to the project root and
	// the checkout is gone.
	send(t, "This is a scripted client test. Call the ExitWorktree tool with action remove. After it returns, reply with exactly: DONE")
	exited := waitRow(t, "the row did not return to the project root after ExitWorktree remove", func(r store.Thread) bool {
		return samePath(r.WorkspacePath, workspace) && r.WorktreePath == ""
	})
	if exited.Branch != "main" {
		t.Errorf("WORKTREE FOLLOW FAILED: branch after exit = %q, want main", exited.Branch)
	}
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Errorf("WORKTREE FOLLOW FAILED: worktree %s still on disk after remove (stat err = %v)", worktreePath, err)
	}

	// Turn 3: a fresh process resumes from the row's workspace. This is the
	// path the start-time transcript settle guards; the CLI must find the
	// transcript from the root cwd and continue the same session.
	if err := app.StopSession(thread.ID); err != nil {
		t.Fatal(err)
	}
	before, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	answer := send(t, "This is a scripted client test. Reply with exactly: READY")
	if !strings.Contains(answer, "READY") {
		t.Errorf("WORKTREE FOLLOW FAILED: resumed turn answered %q, want READY", answer)
	}
	resumed, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.SessionRef != before.SessionRef {
		t.Errorf("WORKTREE FOLLOW FAILED: resume from the root minted session %q instead of continuing %q", resumed.SessionRef, before.SessionRef)
	}
	if located, err := sessionfork.LocateSessionFile(testProviderProjectsDir(t), resumed.SessionRef, workspace); err != nil {
		t.Errorf("WORKTREE FOLLOW FAILED: transcript for %s not locatable from %s: %v", resumed.SessionRef, workspace, err)
	} else if dir, err := sessionfork.WorkspaceProjectDir(testProviderProjectsDir(t), workspace); err != nil || !samePath(filepath.Dir(located), dir) {
		t.Errorf("WORKTREE FOLLOW FAILED: transcript sits at %s, want under the root slug %s (err=%v)", located, dir, err)
	}
}
