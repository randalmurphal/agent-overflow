//go:build providersmoke

// Real-provider gate for a worktree removed OUTSIDE the app while a thread's
// session is alive in it.
//
// WHAT IS UNPROVEN WITHOUT THIS. The registry watcher and the reattach sweep
// (internal/worktreewatch, internal/app/app_worktree_watch.go) are proven
// against mock providers: the row returns to the project root, the session
// stops and is not restarted, the thread gets a notice, the Claude
// transcript moves under the root slug. Only the real CLIs can prove:
//
//   - the next send starts a process from the root that continues THE SAME
//     conversation. Claude resolves `--resume` against the cwd's project
//     slug, so this is the leg the relocation exists for; Codex resumes by
//     thread id with the new `cwd` on `thread/resume`.
//   - a turn running a command when the worktree goes ends interrupted and
//     runs nothing afterwards. Neither CLI exits when its cwd is deleted, and
//     Claude's shell falls back to the home directory, so a session that kept
//     going would run the rest of its turn there.
//
// COST per provider: two trivial turns for the idle leg; for the mid-turn leg
// two trivial turns plus one that runs a 30-second command and is cut.
package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/kerneltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/sessionfork"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
)

const (
	providerSmokeWorktreeRemovalBudget = 6 * time.Minute
	// providerSmokeRemovalSleep is how long the mid-turn leg's command runs.
	// Long enough that the removal lands while it runs; the leg waits past
	// its end before looking for anything the turn would have run after it.
	providerSmokeRemovalSleep = 30 * time.Second
	providerSmokeRecallPrompt = "What codeword did I ask you to remember? Reply with the codeword and nothing else. Do not use tools."
)

func TestProviderSmokeExternalWorktreeRemoval(t *testing.T) {
	for _, smoke := range []providerSmokeCase{
		providerSmokeClaudeCase(),
		{providerName: string(provider.Codex), model: "gpt-5.6-luna",
			installHint: "install Codex CLI on PATH", loginHint: "run `codex login`", probeAccount: (*App).ProbeCodexAccount},
	} {
		t.Run(smoke.providerName, func(t *testing.T) {
			t.Run("idle", func(t *testing.T) {
				runProviderSmokeExternalRemovalIdle(t, smoke)
			})
			t.Run("mid-turn", func(t *testing.T) {
				runProviderSmokeExternalRemovalMidTurn(t, smoke)
			})
		})
	}
}

// providerSmokeRemovalFixture is one thread on a linked worktree the app
// could have cut, with the production registry watcher running.
type providerSmokeRemovalFixture struct {
	app       *App
	smoke     providerSmokeCase
	ctx       context.Context
	workspace string
	worktree  string
	thread    store.Thread
}

func newProviderSmokeRemovalFixture(t *testing.T, smoke providerSmokeCase, branch string, runtime provider.RuntimeMode) providerSmokeRemovalFixture {
	t.Helper()
	app, _ := setupE2EApp(t)
	app.textGenerationExecutor = kerneltest.StubTextGenerationExecutor()
	binary := preflightProviderBinary(t, app, smoke)
	preflightProviderAuth(t, app, smoke, binary)

	workspace := testutil.InitGitRepo(t)
	project, err := app.ensureProjectForWorkspace(workspace)
	if err != nil {
		t.Fatal(err)
	}
	// The production watcher, on test cadences: the removal is seen by the
	// registry watch, not by a direct sweep call.
	startWorktreeWatchForTest(t, app)

	ctx, cancel := context.WithTimeout(context.Background(), providerSmokeWorktreeRemovalBudget)
	t.Cleanup(cancel)
	var claudeCleanup *providerSmokeClaudeDriver
	if smoke.providerName == string(provider.Claude) {
		claudeCleanup = newProviderSmokeClaudeDriver(t, ctx, binary, workspace, smoke.model)
	}

	// Cut the worktree where the app cuts its own, so both the registry and
	// the app's worktrees base dir are the observed places.
	worktreePath := filepath.Join(app.worktreesBaseDir(workspace), branch)
	if err := os.MkdirAll(filepath.Dir(worktreePath), 0o755); err != nil {
		t.Fatal(err)
	}
	providerSmokeGitOutput(t, workspace, "worktree", "add", "-b", branch, worktreePath)
	var worktreeSlugDir string
	if claudeCleanup != nil {
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
	thread.RuntimeMode = string(runtime)
	if claudeCleanup != nil {
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
	return providerSmokeRemovalFixture{app: app, smoke: smoke, ctx: ctx, workspace: workspace, worktree: worktreePath, thread: thread}
}

// send dispatches prompt as a user message and returns the stored user item.
func (f providerSmokeRemovalFixture) send(t *testing.T, prompt string) store.Item {
	t.Helper()
	item, err := f.app.sendMessageWithOptions(f.ctx, f.thread.ID, prompt, sendMessageOptions{SendID: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	return item
}

// answer sends prompt and returns the reply of a turn that must end cleanly.
func (f providerSmokeRemovalFixture) answer(t *testing.T, prompt string) string {
	t.Helper()
	return waitProviderSmokeMessage(t, f.ctx, f.app, f.thread.ID, f.send(t, prompt).ID)
}

func (f providerSmokeRemovalFixture) row(t *testing.T) store.Thread {
	t.Helper()
	row, err := f.app.store.GetThread(f.thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// establish runs the codeword turn in the worktree, leaves its session alive
// there, and returns the codeword and the session ref it recorded.
func (f providerSmokeRemovalFixture) establish(t *testing.T) (codeword, sessionRef string) {
	t.Helper()
	codeword = providerSmokeCodeword()
	f.answer(t, providerSmokeCodewordPrompt(codeword))
	inWorktree := f.row(t)
	if inWorktree.SessionRef == "" {
		t.Fatal("EXTERNAL REMOVAL FAILED: no session ref recorded after the first turn")
	}
	if _, ok := f.app.sessionManager().get(f.thread.ID); !ok {
		t.Fatal("EXTERNAL REMOVAL FAILED: no live session after the first turn")
	}
	return codeword, inWorktree.SessionRef
}

// awaitStoppedAtRoot waits for the watcher to move the row to the project
// root with no session, then holds a moment to prove nothing restarts it.
func (f providerSmokeRemovalFixture) awaitStoppedAtRoot(t *testing.T, removedAt time.Time) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		row := f.row(t)
		atRoot := samePath(row.WorkspacePath, f.workspace) && row.WorktreePath == "" && row.Branch == "main"
		_, present := f.app.sessionManager().get(f.thread.ID)
		_, starting := f.app.sessionManager().startState(f.thread.ID)
		if atRoot && !present && !starting {
			t.Logf("external removal: row at root and session stopped %s after the removal", time.Since(removedAt).Round(time.Millisecond))
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("EXTERNAL REMOVAL FAILED: row workspace=%q worktree=%q branch=%q; session present=%v starting=%v",
				row.WorkspacePath, row.WorktreePath, row.Branch, present, starting)
		}
		time.Sleep(50 * time.Millisecond)
	}
	for hold := time.Now().Add(3 * time.Second); time.Now().Before(hold); time.Sleep(100 * time.Millisecond) {
		_, present := f.app.sessionManager().get(f.thread.ID)
		_, starting := f.app.sessionManager().startState(f.thread.ID)
		if present || starting {
			t.Fatalf("EXTERNAL REMOVAL FAILED: the stopped session was restarted without a send (present=%v starting=%v)", present, starting)
		}
	}
	notices := warningNotices(t, f.app.store, f.thread.ID)
	if len(notices) != 1 || !strings.Contains(notices[0], "was removed outside Agent Overflow") || !strings.Contains(notices[0], "session was stopped") {
		t.Fatalf("EXTERNAL REMOVAL FAILED: notices = %q, want one saying the worktree was removed outside the app and the session stopped", notices)
	}
	t.Logf("external removal notice: %s", notices[0])
}

// assertTranscriptAtRoot checks the Claude transcript moved under the root's
// project slug once the session stopped.
func (f providerSmokeRemovalFixture) assertTranscriptAtRoot(t *testing.T, sessionRef string) {
	t.Helper()
	if f.smoke.providerName != string(provider.Claude) {
		return
	}
	located, err := sessionfork.LocateSessionFile(testProviderProjectsDir(t), sessionRef, f.workspace)
	if err != nil {
		t.Fatalf("EXTERNAL REMOVAL FAILED: transcript for %s not locatable from %s: %v", sessionRef, f.workspace, err)
	}
	if rootSlug, err := sessionfork.WorkspaceProjectDir(testProviderProjectsDir(t), f.workspace); err != nil || !samePath(filepath.Dir(located), rootSlug) {
		t.Fatalf("EXTERNAL REMOVAL FAILED: transcript sits at %s, want under the root slug %s (err=%v)", located, rootSlug, err)
	}
}

// assertContinued sends the recall turn from the root and checks it
// continued the conversation on the same native session.
func (f providerSmokeRemovalFixture) assertContinued(t *testing.T, codeword, sessionRef string) {
	t.Helper()
	answer := f.answer(t, providerSmokeRecallPrompt)
	if !strings.Contains(strings.ToUpper(answer), codeword) {
		t.Fatalf("EXTERNAL REMOVAL FAILED: the session started from the root did not continue the conversation: reply=%q want=%s", answer, codeword)
	}
	resumed := f.row(t)
	if resumed.SessionRef != sessionRef {
		t.Fatalf("EXTERNAL REMOVAL FAILED: the start from the root minted session %q instead of continuing %q", resumed.SessionRef, sessionRef)
	}
	if !samePath(resumed.WorkspacePath, f.workspace) {
		t.Fatalf("EXTERNAL REMOVAL FAILED: row workspace after the resumed turn = %q, want %q", resumed.WorkspacePath, f.workspace)
	}
	t.Logf("external removal: %s resumed %s from the project root and recalled %s (reply %q)", f.smoke.providerName, resumed.SessionRef, codeword, answer)
}

// An idle session in the removed worktree is stopped, not restarted; the
// next send starts it from the root on the same conversation.
func runProviderSmokeExternalRemovalIdle(t *testing.T, smoke providerSmokeCase) {
	f := newProviderSmokeRemovalFixture(t, smoke, "smoke-external-removal", provider.RuntimeReadOnly)
	codeword, sessionRef := f.establish(t)

	// The removal happens in a terminal. Nothing tells the app.
	removedAt := time.Now()
	providerSmokeGitOutput(t, f.workspace, "worktree", "remove", "--force", f.worktree)
	f.awaitStoppedAtRoot(t, removedAt)
	f.assertTranscriptAtRoot(t, sessionRef)
	f.assertContinued(t, codeword, sessionRef)
}

// A turn running a command when the worktree is removed ends interrupted,
// its session stops, and nothing the turn asked for after the command runs,
// in the home directory or anywhere else. The next send continues the
// conversation from the root.
func runProviderSmokeExternalRemovalMidTurn(t *testing.T, smoke providerSmokeCase) {
	// Full access: the command must run without an approval nobody answers.
	f := newProviderSmokeRemovalFixture(t, smoke, "smoke-external-removal-midturn", provider.RuntimeFullAccess)
	codeword, sessionRef := f.establish(t)

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	marker := "ao-smoke-after-removal-" + strings.ToLower(uuid.NewString()[:8]) + ".txt"
	homeMarker := filepath.Join(home, marker)
	t.Cleanup(func() {
		if err := os.Remove(homeMarker); err == nil {
			t.Errorf("EXTERNAL REMOVAL DEFECT: removed %s, written by a turn that should have been stopped", homeMarker)
		}
	})
	// Read from the wire: a Codex unified-exec command is tray state until
	// it completes, not a timeline row.
	commands := &providerSmokeCommandWatch{needle: "time.sleep"}
	f.app.triage.SetEventHook(commands.observe)
	sleepCommand := fmt.Sprintf(`python3 -c "import time; time.sleep(%d)"`, int(providerSmokeRemovalSleep/time.Second))
	prompt := "This is a scripted client test. Do every step in this single turn, in order, without asking questions and without running anything in the background.\n" +
		"Step 1: run this exact shell command in the foreground and wait for it to finish: " + sleepCommand + "\n" +
		"Step 2: after it finishes, run this exact shell command in the current working directory: touch " + marker + "\n" +
		"Step 3: reply with exactly: DONE"
	user := f.send(t, prompt)
	stored, exists, err := f.app.store.GetThreadItem(f.thread.ID, user.ID)
	if err != nil || !exists {
		t.Fatalf("read turn 2 user item: exists=%v err=%v", exists, err)
	}
	turnIndex := stored.TurnIndex

	sleepStarted := commands.awaitStart(t, f, turnIndex)
	// The item appears when the call starts; give the process a moment to be
	// running in the worktree before it disappears under it.
	time.Sleep(3 * time.Second)
	removedAt := time.Now()
	providerSmokeGitOutput(t, f.workspace, "worktree", "remove", "--force", f.worktree)
	f.awaitStoppedAtRoot(t, removedAt)

	if done := commands.completedAt(); !done.IsZero() && done.Before(removedAt) {
		t.Fatalf("EXTERNAL REMOVAL SMOKE: the command finished before the removal landed")
	}
	cut := f.awaitTurnComplete(t, turnIndex)
	t.Logf("external removal mid-turn: turn %d stop_reason=%q error=%q", turnIndex, cut.StopReason, cut.ErrorMessage)
	if cut.StopReason != "interrupted" {
		t.Errorf("EXTERNAL REMOVAL FAILED: the running turn settled with stop_reason %q, want interrupted", cut.StopReason)
	}
	f.assertTranscriptAtRoot(t, sessionRef)

	// Wait past the command's own end: a session that kept running would run
	// step 2 then.
	if wait := time.Until(sleepStarted.Add(providerSmokeRemovalSleep + 10*time.Second)); wait > 0 {
		time.Sleep(wait)
	}
	for _, path := range []string{homeMarker, filepath.Join(f.worktree, marker), filepath.Join(f.workspace, marker)} {
		if _, err := os.Stat(path); err == nil {
			t.Errorf("EXTERNAL REMOVAL DEFECT: %s exists: the turn ran a command after its worktree was removed", path)
		} else if !os.IsNotExist(err) {
			t.Errorf("stat %s: %v", path, err)
		}
	}
	if _, err := os.Stat(f.worktree); !os.IsNotExist(err) {
		t.Errorf("EXTERNAL REMOVAL DEFECT: the removed worktree %s is back on disk (stat err=%v)", f.worktree, err)
	}

	f.assertContinued(t, codeword, sessionRef)
	if _, err := os.Stat(filepath.Join(f.workspace, marker)); err == nil {
		// Not the removal's defect: the model chose to finish the cut turn's
		// work in a turn that asked it not to use tools.
		t.Logf("note: the recall turn wrote %s at the project root", marker)
	}
	if _, err := os.Stat(homeMarker); err == nil {
		t.Errorf("EXTERNAL REMOVAL DEFECT: %s appeared during the recall turn", homeMarker)
	}
}

// providerSmokeCommandWatch records when a tool call whose input contains
// needle starts and completes, from the router hook.
type providerSmokeCommandWatch struct {
	needle string

	mu        sync.Mutex
	itemID    string
	started   time.Time
	completed time.Time
}

func (w *providerSmokeCommandWatch) observe(evt provider.ProviderEvent) {
	now := time.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	switch evt.Kind {
	case provider.EventToolStart:
		if w.itemID == "" && strings.Contains(evt.Content+" "+string(evt.Meta), w.needle) {
			w.itemID, w.started = evt.ItemID, now
		}
	case provider.EventToolComplete:
		if w.itemID != "" && evt.ItemID == w.itemID && w.completed.IsZero() {
			w.completed = now
		}
	}
}

// awaitStart waits for the command to start and returns when it did. It
// fails when the command already finished or the turn ended first.
func (w *providerSmokeCommandWatch) awaitStart(t *testing.T, f providerSmokeRemovalFixture, turnIndex int) time.Time {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		w.mu.Lock()
		itemID, started, completed := w.itemID, w.started, w.completed
		w.mu.Unlock()
		if itemID != "" {
			t.Logf("external removal mid-turn: command %s started", itemID)
			if !completed.IsZero() {
				t.Fatalf("EXTERNAL REMOVAL SMOKE: the command finished before the removal could land")
			}
			return started
		}
		for _, turn := range mustRecentTurns(t, f.app, f.thread.ID) {
			if turn.TurnIndex == turnIndex && turn.CompletedAt != nil {
				t.Fatalf("EXTERNAL REMOVAL SMOKE: turn %d ended before running the command: %+v; items=%s", turnIndex, turn, providerSmokeTurnItems(t, f, turnIndex))
			}
		}
		if time.Now().After(deadline) || f.ctx.Err() != nil {
			t.Fatalf("EXTERNAL REMOVAL SMOKE: no %q command in turn %d; items=%s", w.needle, turnIndex, providerSmokeTurnItems(t, f, turnIndex))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// completedAt reports when the command's completion reached the app.
func (w *providerSmokeCommandWatch) completedAt() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.completed
}

func (f providerSmokeRemovalFixture) awaitTurnComplete(t *testing.T, turnIndex int) store.Turn {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		for _, turn := range mustRecentTurns(t, f.app, f.thread.ID) {
			if turn.TurnIndex == turnIndex && turn.CompletedAt != nil {
				return turn
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("EXTERNAL REMOVAL FAILED: turn %d still open after the session stopped: %+v", turnIndex, mustRecentTurns(t, f.app, f.thread.ID))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func mustRecentTurns(t *testing.T, app *App, threadID string) []store.Turn {
	t.Helper()
	turns, err := app.store.ListRecentTurns(threadID, 8)
	if err != nil {
		t.Fatal(err)
	}
	return turns
}

func providerSmokeTurnItems(t *testing.T, f providerSmokeRemovalFixture, turnIndex int) string {
	t.Helper()
	items, err := f.app.store.ListItems(f.thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, item := range items {
		if item.TurnIndex == turnIndex {
			out = append(out, fmt.Sprintf("{%s tool=%s status=%s %q}", item.Kind, item.ToolName, item.Status, providerSmokeTruncate(item.Summary)))
		}
	}
	return strings.Join(out, " ")
}
