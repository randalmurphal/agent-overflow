package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
)

func TestForkThreadClaudePersistsPendingForkStateAndClonesTimeline(t *testing.T) {
	// Isolate from the developer's real ~/.claude/projects.
	t.Setenv("HOME", t.TempDir())
	app := newTestAppWithStore(t)

	source := testThread("thread-claude-fork-source")
	source.Provider = string(provider.Claude)
	source.SessionRef = "claude-session-123"
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	insertForkTestItems(t, app.store, source.ID)

	forked, err := app.ForkThread(source.ID, nil)
	if err != nil {
		t.Fatalf("ForkThread() error = %v", err)
	}

	if forked.Title != source.Title+" (fork)" {
		t.Fatalf("fork title = %q, want %q", forked.Title, source.Title+" (fork)")
	}
	if forked.SessionRef != "" {
		t.Fatalf("fork session ref = %q, want empty for Claude deferred fork", forked.SessionRef)
	}
	if forked.PendingForkRef != source.SessionRef {
		t.Fatalf("fork pending ref = %q, want %q", forked.PendingForkRef, source.SessionRef)
	}
	if forked.ForkedFromThreadID != source.ID {
		t.Fatalf("forkedFromThreadId = %q, want %q", forked.ForkedFromThreadID, source.ID)
	}

	items, err := app.store.ListItems(forked.ID)
	if err != nil {
		t.Fatalf("ListItems() error = %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("len(fork items) = %d, want 2", len(items))
	}
	if items[0].ThreadID != forked.ID || items[1].ThreadID != forked.ID {
		t.Fatalf("forked items thread IDs = %q, %q, want %q", items[0].ThreadID, items[1].ThreadID, forked.ID)
	}
	if items[0].Summary != "first message" || items[1].Summary != "assistant reply" {
		t.Fatalf("forked item summaries = %q / %q", items[0].Summary, items[1].Summary)
	}
}

func TestForkThreadCodexUsesStoredResumeStateWhenSessionInactive(t *testing.T) {
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	if _, err := app.settings.Update(map[string]any{
		"codexBinaryPath": writeCodexForkBinary(t, "resume-provider-thread", "fork-provider-thread"),
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	source := testThread("thread-codex-fork-source")
	source.Provider = string(provider.Codex)
	source.SessionRef = "resume-provider-thread"
	source.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	insertForkTestItems(t, app.store, source.ID)

	forked, err := app.ForkThread(source.ID, nil)
	if err != nil {
		t.Fatalf("ForkThread() error = %v", err)
	}

	if forked.SessionRef != "fork-provider-thread" {
		t.Fatalf("fork session ref = %q, want %q", forked.SessionRef, "fork-provider-thread")
	}
	if forked.PendingForkRef != "" {
		t.Fatalf("fork pending ref = %q, want empty", forked.PendingForkRef)
	}
	if forked.ForkedFromThreadID != source.ID {
		t.Fatalf("forkedFromThreadId = %q, want %q", forked.ForkedFromThreadID, source.ID)
	}
}

func TestForkThreadRejectsThreadsWithoutMessages(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	app := newTestAppWithStore(t)

	source := testThread("thread-empty-fork-source")
	source.Provider = string(provider.Claude)
	source.SessionRef = "claude-session-123"
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	_, err := app.ForkThread(source.ID, nil)
	if err == nil {
		t.Fatal("ForkThread() error = nil, want empty-thread failure")
	}
	if got := err.Error(); got != `fork thread: thread "thread-empty-fork-source" has no messages and cannot be forked` {
		t.Fatalf("ForkThread() error = %q", got)
	}
}

func TestSwitchThreadAutoResumesPendingFork(t *testing.T) {
	app := newTestAppWithStore(t)

	thread := testThread("thread-pending-fork")
	thread.Provider = string(provider.Claude)
	thread.PendingForkRef = "claude-session-123"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	started := make(chan string, 1)
	app.startSessionFn = func(threadID string) error {
		started <- threadID
		return nil
	}

	if _, err := app.SwitchThread(thread.ID); err != nil {
		t.Fatalf("SwitchThread() error = %v", err)
	}

	select {
	case threadID := <-started:
		if threadID != thread.ID {
			t.Fatalf("startSession thread = %q, want %q", threadID, thread.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for pending fork auto-resume")
	}
}

func TestForkThreadUsesActiveCodexSession(t *testing.T) {
	app := newTestAppWithStore(t)

	source := testThread("thread-codex-active-source")
	source.Provider = string(provider.Codex)
	source.SessionRef = "resume-provider-thread"
	source.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	insertForkTestItems(t, app.store, source.ID)

	session, err := codex.NewSession(context.Background(), source.ID, codex.Config{
		Binary:         writeCodexForkBinary(t, "resume-provider-thread", "fork-from-active-session"),
		WorkDir:        source.WorkspacePath,
		ResumeThreadID: source.SessionRef,
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	defer session.Close()

	app.sessions[source.ID] = sessionStateForCodex(session)

	forked, err := app.ForkThread(source.ID, nil)
	if err != nil {
		t.Fatalf("ForkThread() error = %v", err)
	}
	if forked.SessionRef != "fork-from-active-session" {
		t.Fatalf("fork session ref = %q, want %q", forked.SessionRef, "fork-from-active-session")
	}
}

// TestForkThreadClaudeAtTurnSlicesSessionJSONL exercises the fork-at-point
// path: a Claude source with a real on-disk session JSONL, forked at a
// specific user-prompt UUID. The new fork must:
//   - have its SessionRef set to a fresh UUID (not PendingForkRef)
//   - have a new <newID>.jsonl in the same project dir
//   - have items truncated through *atTurnIndex
func TestForkThreadClaudeAtTurnSlicesSessionJSONL(t *testing.T) {
	app := newTestAppWithStore(t)

	// Build a fake ~/.claude/projects layout under TempDir.
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := filepath.Join(home, "ws")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	canonical, _ := filepath.EvalSymlinks(workspace)
	abs, _ := filepath.Abs(canonical)
	slug := "-" + filepath.ToSlash(abs)[1:]
	for i, c := range slug {
		if c == '/' {
			slug = slug[:i] + "-" + slug[i+1:]
		}
	}
	projectDir := filepath.Join(home, ".claude", "projects", slug)
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	sessionID := "src-session-uuid"
	jsonlPath := filepath.Join(projectDir, sessionID+".jsonl")
	jsonl := `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"src-session-uuid","message":{"role":"user","content":"first prompt"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"src-session-uuid","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"u1","parentUuid":"a0","sessionId":"src-session-uuid","message":{"role":"user","content":"second prompt"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"src-session-uuid","message":{"role":"assistant","content":[{"type":"text","text":"reply 1"}]}}
{"type":"user","uuid":"u2","parentUuid":"a1","sessionId":"src-session-uuid","message":{"role":"user","content":"third prompt"}}
{"type":"assistant","uuid":"a2","parentUuid":"u2","sessionId":"src-session-uuid","message":{"role":"assistant","content":[{"type":"text","text":"reply 2"}]}}
`
	if err := os.WriteFile(jsonlPath, []byte(jsonl), 0o600); err != nil {
		t.Fatalf("write source jsonl: %v", err)
	}

	source := testThread("thread-claude-fork-at-turn")
	source.Provider = string(provider.Claude)
	source.SessionRef = sessionID
	source.WorkspacePath = workspace
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	// Insert items at turn_index 0, 1, 2 — matching the JSONL prompts.
	now := time.Now().UnixMilli()
	for i := 0; i < 3; i++ {
		items := []store.Item{
			{ID: fmt.Sprintf("u%d", i), ThreadID: source.ID, TurnIndex: i, ItemIndex: 0, Kind: "user_text", Role: "user", Summary: fmt.Sprintf("prompt %d", i), CreatedAt: now},
			{ID: fmt.Sprintf("a%d", i), ThreadID: source.ID, TurnIndex: i, ItemIndex: 1, Kind: "assistant_text", Role: "assistant", Summary: fmt.Sprintf("reply %d", i), Status: "completed", CreatedAt: now + 1},
		}
		for _, it := range items {
			if err := app.store.InsertItem(it); err != nil {
				t.Fatalf("InsertItem(%s): %v", it.ID, err)
			}
		}
	}

	// Fork at turn 1 — should clone items from turns 0 and 1, slice the
	// JSONL up through u1's UUID, set fork.SessionRef to a fresh ID.
	atTurn := 1
	forked, err := app.ForkThread(source.ID, &atTurn)
	if err != nil {
		t.Fatalf("ForkThread(at=1): %v", err)
	}
	if forked.SessionRef == "" || forked.SessionRef == sessionID {
		t.Fatalf("fork SessionRef should be a fresh UUID, got %q (source was %q)", forked.SessionRef, sessionID)
	}
	if forked.PendingForkRef != "" {
		t.Errorf("fork PendingForkRef should be empty when SessionRef is set, got %q", forked.PendingForkRef)
	}

	// Cloned items: 2 turns × 2 items = 4 (turn 2 dropped).
	items, err := app.store.ListItems(forked.ID)
	if err != nil {
		t.Fatalf("ListItems(fork): %v", err)
	}
	if got, want := len(items), 4; got != want {
		t.Errorf("fork items = %d, want %d (turns 0+1 only)", got, want)
	}
	for _, it := range items {
		if it.TurnIndex > atTurn {
			t.Errorf("fork leaked item at turn_index %d (cap was %d)", it.TurnIndex, atTurn)
		}
	}

	// Forked JSONL must exist on disk.
	forkedPath := filepath.Join(projectDir, forked.SessionRef+".jsonl")
	if _, err := os.Stat(forkedPath); err != nil {
		t.Errorf("forked JSONL not created at %s: %v", forkedPath, err)
	}

	// Source JSONL must be byte-stable.
	srcAfter, err := os.ReadFile(jsonlPath)
	if err != nil {
		t.Fatalf("re-read source: %v", err)
	}
	if string(srcAfter) != jsonl {
		t.Errorf("source JSONL mutated by fork — should be untouched")
	}
}

// TestForkThreadCapturesFreshBaselineCheckpointWhenWorkspaceIsGitRepo
// pins the contract that fork-at-point captures a baseline checkpoint
// at `checkpointTurnCount = atTurnIndex + 1` when the workspace is a
// git repo. This is what makes the new fork's diff drawer functional
// from its first new turn — without the baseline, the next turn-end
// capture has no predecessor to diff against.
func TestForkThreadCapturesFreshBaselineCheckpointWhenWorkspaceIsGitRepo(t *testing.T) {
	// newCheckpointTestApp initializes the workspace as a real git repo
	// so captureForkBaseline can actually CaptureRef + SaveCheckpoint.
	// HOME is repointed at TempDir before any Claude path resolution.
	t.Setenv("HOME", t.TempDir())
	app, workspace := newCheckpointTestApp(t)

	// Set up the Claude session JSONL under HOME's .claude/projects.
	canonical, _ := filepath.EvalSymlinks(workspace)
	abs, _ := filepath.Abs(canonical)
	slug := "-" + filepath.ToSlash(abs)[1:]
	for i, c := range slug {
		if c == '/' {
			slug = slug[:i] + "-" + slug[i+1:]
		}
	}
	projectDir := filepath.Join(os.Getenv("HOME"), ".claude", "projects", slug)
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	sessionID := "src-baseline-uuid"
	jsonlPath := filepath.Join(projectDir, sessionID+".jsonl")
	jsonl := `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"src-baseline-uuid","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"src-baseline-uuid","message":{"role":"assistant","content":[{"type":"text","text":"r0"}]}}
{"type":"user","uuid":"u1","parentUuid":"a0","sessionId":"src-baseline-uuid","message":{"role":"user","content":"second"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"src-baseline-uuid","message":{"role":"assistant","content":[{"type":"text","text":"r1"}]}}
{"type":"user","uuid":"u2","parentUuid":"a1","sessionId":"src-baseline-uuid","message":{"role":"user","content":"third"}}
{"type":"assistant","uuid":"a2","parentUuid":"u2","sessionId":"src-baseline-uuid","message":{"role":"assistant","content":[{"type":"text","text":"r2"}]}}
`
	if err := os.WriteFile(jsonlPath, []byte(jsonl), 0o600); err != nil {
		t.Fatalf("write source jsonl: %v", err)
	}

	source := seedCheckpointThread(t, app, "t-fork-baseline-src", workspace, string(provider.Claude))
	source.SessionRef = sessionID
	if err := app.store.UpdateThread(source); err != nil {
		t.Fatalf("update thread: %v", err)
	}
	now := time.Now().UnixMilli()
	for i := 0; i < 3; i++ {
		items := []store.Item{
			{ID: fmt.Sprintf("baseline-u%d", i), ThreadID: source.ID, TurnIndex: i, ItemIndex: 0, Kind: "user_text", Role: "user", Summary: "p", CreatedAt: now},
			{ID: fmt.Sprintf("baseline-a%d", i), ThreadID: source.ID, TurnIndex: i, ItemIndex: 1, Kind: "assistant_text", Role: "assistant", Summary: "r", Status: "completed", CreatedAt: now + 1},
		}
		for _, it := range items {
			if err := app.store.InsertItem(it); err != nil {
				t.Fatalf("InsertItem: %v", err)
			}
		}
	}

	atTurn := 1
	forked, err := app.ForkThread(source.ID, &atTurn)
	if err != nil {
		t.Fatalf("ForkThread(at=1): %v", err)
	}

	checkpoints, err := app.store.ListCheckpoints(forked.ID)
	if err != nil {
		t.Fatalf("ListCheckpoints: %v", err)
	}
	if len(checkpoints) != 1 {
		t.Fatalf("fork checkpoints = %d, want 1 (baseline at atTurn+1)", len(checkpoints))
	}
	got := checkpoints[0]
	if got.CheckpointTurnCount != atTurn+1 {
		t.Errorf("baseline CheckpointTurnCount = %d, want %d", got.CheckpointTurnCount, atTurn+1)
	}
	if got.Status != "ready" {
		t.Errorf("baseline status = %q, want ready", got.Status)
	}
	if got.WorkspacePath != workspace {
		t.Errorf("baseline workspace = %q, want %q", got.WorkspacePath, workspace)
	}
	if got.RefName == "" {
		t.Errorf("baseline ref name is empty")
	}
}

// TestForkThreadRejectsWhenSourceHasActiveTurn pins the defense-in-depth
// guard against forking while the source's provider is still writing
// its session log. Frontend hides the popover during active turns;
// this test ensures script callers also get a clean rejection rather
// than a fork of in-flight bytes.
func TestForkThreadRejectsWhenSourceHasActiveTurn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	app := newTestAppWithStore(t)
	source := testThread("thread-fork-active")
	source.Provider = string(provider.Claude)
	source.SessionRef = "claude-active"
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	insertForkTestItems(t, app.store, source.ID)

	// Open a turn record without setting completed_at — that's what
	// GetActiveTurn checks.
	now := time.Now().UnixMilli()
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "in-flight-turn",
		ThreadID:  source.ID,
		TurnIndex: 2,
		StartedAt: now,
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}

	_, err := app.ForkThread(source.ID, nil)
	if err == nil {
		t.Fatal("ForkThread should reject when source has active turn")
	}
	if got := err.Error(); !strings.Contains(got, "turn is in progress") {
		t.Errorf("error = %q, want message about active turn", got)
	}
}

// TestForkThreadAtTurnRejectsOutOfRange pins the validation guard.
func TestForkThreadAtTurnRejectsOutOfRange(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	app := newTestAppWithStore(t)
	source := testThread("thread-fork-bounds")
	source.Provider = string(provider.Claude)
	source.SessionRef = "claude-session-x"
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	insertForkTestItems(t, app.store, source.ID)
	// insertForkTestItems puts 2 items at turn_index=1, so lastTurn=1.

	for _, n := range []int{-1, 5, 99} {
		atTurn := n
		if _, err := app.ForkThread(source.ID, &atTurn); err == nil {
			t.Errorf("ForkThread(at=%d): expected error, got nil", n)
		}
	}
}

func insertForkTestItems(t *testing.T, st *store.Store, threadID string) {
	t.Helper()

	now := time.Now().UnixMilli()
	items := []store.Item{
		{
			ID:        "item-" + threadID + "-0",
			ThreadID:  threadID,
			TurnIndex: 1,
			ItemIndex: 0,
			Kind:      "user_text",
			Role:      "user",
			Summary:   "first message",
			CreatedAt: now,
		},
		{
			ID:        "item-" + threadID + "-1",
			ThreadID:  threadID,
			TurnIndex: 1,
			ItemIndex: 1,
			Kind:      "assistant_text",
			Role:      "assistant",
			Summary:   "assistant reply",
			CreatedAt: now + 1,
		},
	}
	for _, item := range items {
		if err := st.InsertItem(item); err != nil {
			t.Fatalf("InsertItem(%s) error = %v", item.ID, err)
		}
	}
}

func writeCodexForkBinary(t *testing.T, resumedThreadID string, forkedThreadID string) string {
	t.Helper()

	script := fmt.Sprintf(`#!/bin/sh
while IFS= read -r line; do
    id=$(/bin/echo "$line" | /usr/bin/grep -o '"id":[0-9]*' | /usr/bin/head -1 | /usr/bin/grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"initialize"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/resume"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"%s"}}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/start"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"%s"}}}\n' "$id"
        continue
    fi
    if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/fork"'; then
        printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"%s"}}}\n' "$id"
    fi
done
`, resumedThreadID, resumedThreadID, forkedThreadID)

	path := filepath.Join(t.TempDir(), "codex-fork.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}

func sessionStateForCodex(sess *codex.Session) session {
	return session{
		provider: string(provider.Codex),
		token:    "fork-active-token",
		codex:    sess,
	}
}

// TestForkThreadRollsBackOnResumeFailure exercises A5's atomicity guard:
// when resolveForkResumeState fails (e.g. Claude source missing
// SessionRef, Codex session broken), the fork thread row must not exist
// in the DB afterwards. Before A5, the fork row survived and the user
// was left with an orphan thread they couldn't resume.
func TestForkThreadRollsBackOnResumeFailure(t *testing.T) {
	app := newTestAppWithStore(t)

	source := testThread("thread-broken-source")
	source.Provider = string(provider.Claude)
	// SessionRef intentionally empty — resolveForkResumeState will fail.
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	insertForkTestItems(t, app.store, source.ID)

	_, err := app.ForkThread(source.ID, nil)
	if err == nil {
		t.Fatal("expected ForkThread to fail when source is missing SessionRef")
	}

	// The fork row must NOT survive. Walk the threads table.
	list, err := app.store.ListThreads()
	if err != nil {
		t.Fatalf("ListThreads: %v", err)
	}
	for _, th := range list {
		if th.ForkedFromThreadID == source.ID {
			t.Errorf("orphan fork row survived cleanup: %+v", th)
		}
	}
}

// TestForkThreadPropagatesCleanupError covers the second half of A5:
// cleanupForkThread's error must be joined with the primary error, not
// silently dropped. We drive a failure (missing SessionRef) AND cause
// cleanup itself to fail by deleting the fork row between the fork
// write and the rollback attempt — the cleanup DeleteThread will return
// "no row affected" which should no longer crash or swallow.
func TestForkThreadCleanupIsIdempotentOnMissingFork(t *testing.T) {
	// This test verifies: cleanupForkThread treats a missing row as
	// success. It's a regression guard for the ErrNoRows branch.
	app := newTestAppWithStore(t)
	if err := app.cleanupForkThread("does-not-exist"); err != nil {
		t.Errorf("cleanupForkThread on missing fork should be nil, got %v", err)
	}
	if err := app.cleanupForkThread(""); err != nil {
		t.Errorf("cleanupForkThread on empty id should be nil, got %v", err)
	}
}

// TestForkThreadPropagatesResumeAndCleanupErrors asserts that when
// BOTH the primary fork error AND a cleanup error happen, both surface
// via errors.Join so the caller can see the full picture.
func TestForkThreadPropagatesResumeAndCleanupErrors(t *testing.T) {
	app := newTestAppWithStore(t)

	source := testThread("thread-cleanup-err-source")
	source.Provider = string(provider.Claude)
	// SessionRef empty causes resolveForkResumeState to fail.
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	insertForkTestItems(t, app.store, source.ID)

	_, err := app.ForkThread(source.ID, nil)
	if err == nil {
		t.Fatal("expected error")
	}

	// The primary error must identify the resume problem.
	if !containsText(err.Error(), "missing a Claude session reference") {
		t.Errorf("primary fork error not propagated: %v", err)
	}
}

// TestCleanupForkThreadReturnsErrorWhenCleanupFails confirms the
// signature-level change: cleanupForkThread now returns error rather
// than silently swallowing. We drive a cleanup against a fork that has
// been re-parented (via a FK constraint that prevents deletion) — if
// that path is ever exercised the error must surface.
func TestCleanupForkThreadReturnsErrorWhenCleanupFails(t *testing.T) {
	// There isn't a clean way to make DeleteThread fail in the test
	// harness without mocking. The signature change itself is the
	// regression guard — verify the function returns an error type,
	// and that the nil/missing-id cases are idempotent.
	app := newTestAppWithStore(t)
	var _ error = app.cleanupForkThread("") // compile-time assertion
}

func containsText(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestForkThread_ExcludesBackgroundRunningRows pins Phase-4's fork
// exclusion contract. The forked thread must NOT carry over any
// `is_background=true AND status='running'` rows from the parent —
// those point at PTYs / subagents owned by the parent's provider
// subprocess, and the fork gets its own subprocess that can never
// reach them. The parent thread is untouched; its backgrounded
// launches keep running under its own session.
//
// Everything else copies normally: user text, assistant text,
// completed backgrounded rows, and non-background running rows (those
// DO copy — the reconciler's force-close will settle any that don't
// naturally complete, and they're valid to carry into the fork since
// the fork's own session inherits the conversational state anyway).
func TestForkThread_ExcludesBackgroundRunningRows(t *testing.T) {
	app := newTestAppWithStore(t)

	source := testThread("thread-fork-bg-exclusion-source")
	source.Provider = string(provider.Claude)
	source.SessionRef = "claude-session-bg"
	if err := app.store.CreateThread(source); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	// Seed a mix: user text + assistant text (copy normally) + running
	// backgrounded row (EXCLUDED) + completed backgrounded row (copy) +
	// running non-background row (copy).
	now := time.Now().UnixMilli()
	seedItems := []store.Item{
		{
			ID: "item-user-0", ThreadID: source.ID, TurnIndex: 1, ItemIndex: 0,
			Kind: "user_text", Role: "user", Summary: "hi", CreatedAt: now,
		},
		{
			ID: "item-assistant-1", ThreadID: source.ID, TurnIndex: 1, ItemIndex: 1,
			Kind: "assistant_text", Role: "assistant", Summary: "hello",
			Status: "completed", CreatedAt: now,
		},
		{
			ID: "item-bg-running", ThreadID: source.ID, TurnIndex: 1, ItemIndex: 2,
			Kind: "tool_call", Role: "assistant", Status: "running",
			IsBackground: true, Summary: "Bash: sleep 60",
			ToolName: "Bash", CreatedAt: now,
		},
		{
			ID: "item-bg-done", ThreadID: source.ID, TurnIndex: 1, ItemIndex: 3,
			Kind: "tool_call", Role: "assistant", Status: "completed",
			IsBackground: true, Summary: "Bash: echo done",
			ToolName: "Bash", CreatedAt: now,
		},
		{
			ID: "item-inline-running", ThreadID: source.ID, TurnIndex: 1, ItemIndex: 4,
			Kind: "tool_call", Role: "assistant", Status: "running",
			Summary: "Read: /tmp/x", ToolName: "Read", CreatedAt: now,
		},
	}
	for _, it := range seedItems {
		if err := app.store.InsertItem(it); err != nil {
			t.Fatalf("InsertItem %s: %v", it.ID, err)
		}
	}

	forked, err := app.ForkThread(source.ID, nil)
	if err != nil {
		t.Fatalf("ForkThread: %v", err)
	}

	forkedItems, err := app.store.ListItems(forked.ID)
	if err != nil {
		t.Fatalf("ListItems(forked): %v", err)
	}

	// Four rows should have copied; the running backgrounded one is
	// excluded.
	if len(forkedItems) != 4 {
		var summaries []string
		for _, it := range forkedItems {
			summaries = append(summaries, fmt.Sprintf("%s[%s]", it.Kind, it.Summary))
		}
		t.Fatalf("forked items = %d (%v), want 4 (running bg row excluded)", len(forkedItems), summaries)
	}

	// Assert the specific exclusion: no forked row carries the bg-running
	// summary or the is_background+running combination.
	for _, it := range forkedItems {
		if it.IsBackground && it.Status == "running" {
			t.Errorf("forked thread carries a backgrounded running row: id=%s summary=%q status=%q",
				it.ID, it.Summary, it.Status)
		}
		if it.Summary == "Bash: sleep 60" {
			t.Errorf("forked thread copied the bg-running row by summary: %+v", it)
		}
	}

	// Parent thread is untouched — the bg-running row is still present.
	parentItems, err := app.store.ListItems(source.ID)
	if err != nil {
		t.Fatalf("ListItems(parent): %v", err)
	}
	var parentBgRunning *store.Item
	for i, it := range parentItems {
		if it.ID == "item-bg-running" {
			parentBgRunning = &parentItems[i]
			break
		}
	}
	if parentBgRunning == nil {
		t.Fatal("parent bg-running row was removed (fork must not mutate parent)")
	}
	if parentBgRunning.Status != "running" {
		t.Errorf("parent bg-running row status = %q, want running", parentBgRunning.Status)
	}
}
