package app

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
)

// A lazy Claude fork that has not started a session of its own resumes its
// source at the pinned cut. These tests build one through `/side-chat`, so
// the fork is a pointer fork with a real cut, and check that a rollback or a
// fork-from-message at a message the fork sent itself keeps that resume
// state, while one at an inherited message still slices the source.

const unstartedForkSourceSession = "unstarted-fork-source"

type unstartedForkFixture struct {
	app       *App
	workspace string
	source    store.Thread
	fork      store.Thread
	// projectDir holds the workspace's Claude session files.
	projectDir string
}

// newUnstartedForkFixture forks a two-turn Claude thread and adds a message
// the fork sent itself at turn 2. The CLI wrote that send into a forked
// session ("cli-fork") whose init the stopped process never reported, so
// the fork row still carries only its pending resume state.
func newUnstartedForkFixture(t *testing.T) unstartedForkFixture {
	t.Helper()
	f := newRequestFixture(t)
	home := testProviderHome(t, f.app)
	workspace := t.TempDir()
	history := `{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"SID","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"SID","message":{"role":"assistant","content":[{"type":"text","text":"reply 0"}]}}
{"type":"user","uuid":"u1","parentUuid":"a0","sessionId":"SID","message":{"role":"user","content":"second"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"SID","message":{"role":"assistant","content":[{"type":"text","text":"reply 1"}]}}
`
	sourcePath := writeClaudeProjectSession(t, home, workspace, unstartedForkSourceSession, history)
	writeClaudeProjectSession(t, home, workspace, "cli-fork", history+
		`{"type":"user","uuid":"u-fork","parentUuid":"a1","sessionId":"cli-fork","message":{"role":"user","content":"side question"}}
`)

	source, err := createTestThread(t, f.app, string(provider.Claude), workspace, "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	source.SessionRef = unstartedForkSourceSession
	if err := f.app.store.UpdateThread(source); err != nil {
		t.Fatalf("set source session ref: %v", err)
	}
	insertUserItemWithMeta(t, f.app.store, source.ID, "src:u0", 0, "first", `{"provider_item_id":"u0"}`)
	insertAssistantTextItem(t, f.app.store, source.ID, "src:a0", 0, "reply 0")
	insertUserItemWithMeta(t, f.app.store, source.ID, "src:u1", 1, "second", `{"provider_item_id":"u1"}`)
	insertAssistantTextItem(t, f.app.store, source.ID, "src:a1", 1, "reply 1")

	fork, err := f.app.ForkSideChat(t.Context(), source.ID)
	if err != nil {
		t.Fatalf("ForkSideChat: %v", err)
	}
	if fork.SessionRef != "" || fork.PendingForkRef != unstartedForkSourceSession || fork.PendingForkResumeAt == "" {
		t.Fatalf("fork resume state = ref %q pending %q pin %q, want an unstarted pinned fork of the source",
			fork.SessionRef, fork.PendingForkRef, fork.PendingForkResumeAt)
	}
	insertUserItemWithMeta(t, f.app.store, fork.ID, "fork:u2", 2, "side question", `{"provider_item_id":"u-fork"}`)
	seedMessageAnchor(t, f.app.store, fork.ID, "fork:u2", 2, "u-fork", "")
	return unstartedForkFixture{
		app: f.app, workspace: workspace, source: source, fork: fork,
		projectDir: filepath.Dir(sourcePath),
	}
}

func (x unstartedForkFixture) sessionFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(x.projectDir)
	if err != nil {
		t.Fatalf("read project dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	return names
}

func (x unstartedForkFixture) thread(t *testing.T, id string) store.Thread {
	t.Helper()
	thread, err := x.app.store.GetThread(id)
	if err != nil {
		t.Fatalf("GetThread %s: %v", id, err)
	}
	return thread
}

func TestRollbackOfUnstartedForksOwnMessageKeepsItsPendingResume(t *testing.T) {
	t.Parallel()
	x := newUnstartedForkFixture(t)
	filesBefore := x.sessionFiles(t)

	if err := rollbackToMessage(x.app, x.fork.ID, "fork:u2"); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	got := x.thread(t, x.fork.ID)
	if got.SessionRef != "" || got.PendingForkRef != x.fork.PendingForkRef || got.PendingForkResumeAt != x.fork.PendingForkResumeAt {
		t.Fatalf("fork resume state = ref %q pending %q pin %q, want unchanged pending %q pin %q",
			got.SessionRef, got.PendingForkRef, got.PendingForkResumeAt, x.fork.PendingForkRef, x.fork.PendingForkResumeAt)
	}
	if files := x.sessionFiles(t); !slices.Equal(files, filesBefore) {
		t.Fatalf("session files = %v, want unchanged %v", files, filesBefore)
	}
	if _, found, err := x.app.store.GetThreadItem(x.fork.ID, "fork:u2"); err != nil || found {
		t.Fatalf("rolled-back message still on the fork: found=%v err=%v", found, err)
	}
	if _, found, err := x.app.store.GetThreadItem(x.fork.ID, "src:u1"); err != nil || !found {
		t.Fatalf("inherited message lost: found=%v err=%v", found, err)
	}
}

func TestRollbackOfUnstartedForksInheritedMessageSlicesTheSource(t *testing.T) {
	t.Parallel()
	x := newUnstartedForkFixture(t)

	if err := rollbackToMessage(x.app, x.fork.ID, "src:u1"); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	got := x.thread(t, x.fork.ID)
	if got.SessionRef == "" || got.SessionRef == unstartedForkSourceSession || got.PendingForkRef != "" {
		t.Fatalf("fork resume state = ref %q pending %q, want its own slice of the source", got.SessionRef, got.PendingForkRef)
	}
	assertClaudeSessionText(t, x.app, x.workspace, got.SessionRef, []string{"first"}, []string{"second", "side question"})
}

// The caller read the thread before the stop, and the stopped process's init
// committed the fork's own session during it. The rollback must cut that
// session, not take the stale snapshot's pending state for the truth.
func TestRollbackOfUnstartedForkUsesTheSessionCommittedDuringTheStop(t *testing.T) {
	t.Parallel()
	x := newUnstartedForkFixture(t)
	stale := x.fork
	item, found, err := x.app.store.GetThreadItem(x.fork.ID, "fork:u2")
	if err != nil || !found {
		t.Fatalf("GetThreadItem: found=%v err=%v", found, err)
	}
	if _, err := x.app.store.UpdateSessionRef(x.fork.ID, "cli-fork"); err != nil {
		t.Fatalf("UpdateSessionRef: %v", err)
	}

	unlock := x.app.threadLocks().Lock(x.fork.ID)
	err = x.app.rollbackClaudeThreadToMessage(stale, x.app.resolveMessageAnchor("test", x.fork.ID, item), item)
	unlock()
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}

	got := x.thread(t, x.fork.ID)
	if got.SessionRef == "" || got.SessionRef == "cli-fork" {
		t.Fatalf("fork session ref = %q, want a slice of the fork's own session", got.SessionRef)
	}
	assertClaudeSessionText(t, x.app, x.workspace, got.SessionRef, []string{"first", "second"}, []string{"side question"})
}

func TestForkFromUnstartedForksOwnMessageInheritsItsPendingResume(t *testing.T) {
	t.Parallel()
	x := newUnstartedForkFixture(t)
	filesBefore := x.sessionFiles(t)

	forked, err := x.app.ForkThreadFromMessage(t.Context(), x.fork.ID, "fork:u2")
	if err != nil {
		t.Fatalf("ForkThreadFromMessage: %v", err)
	}

	got := x.thread(t, forked.ID)
	if got.SessionRef != "" || got.PendingForkRef != x.fork.PendingForkRef || got.PendingForkResumeAt != x.fork.PendingForkResumeAt {
		t.Fatalf("fork resume state = ref %q pending %q pin %q, want the source fork's pending %q pin %q",
			got.SessionRef, got.PendingForkRef, got.PendingForkResumeAt, x.fork.PendingForkRef, x.fork.PendingForkResumeAt)
	}
	if files := x.sessionFiles(t); !slices.Equal(files, filesBefore) {
		t.Fatalf("session files = %v, want unchanged %v", files, filesBefore)
	}
}

func TestForkFromUnstartedForksInheritedMessageSlicesTheSource(t *testing.T) {
	t.Parallel()
	x := newUnstartedForkFixture(t)

	forked, err := x.app.ForkThreadFromMessage(t.Context(), x.fork.ID, "src:u1")
	if err != nil {
		t.Fatalf("ForkThreadFromMessage: %v", err)
	}

	got := x.thread(t, forked.ID)
	if got.SessionRef == "" || got.SessionRef == unstartedForkSourceSession || got.PendingForkRef != "" {
		t.Fatalf("fork resume state = ref %q pending %q, want its own slice of the source", got.SessionRef, got.PendingForkRef)
	}
	assertClaudeSessionText(t, x.app, x.workspace, got.SessionRef, []string{"first"}, []string{"second", "side question"})
}
