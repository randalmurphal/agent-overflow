package app

import (
	"context"
	"testing"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/sessionimport"
	"agent-overflow/internal/store"
	"agent-overflow/internal/terminal"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/triage"
)

// The convergence wave (2026-09-03): every one of these write paths persisted
// its change and answered its own caller, and told no other connected client.
// Each test below asserts the one thing that was missing — that the write
// EMITS — because the second client's screen is otherwise stale until reload.
//
// The frontend halves live beside their stores as vitest suites; these pin the
// Go end of each channel, which is the end a second client cannot compensate
// for.

// emissionNames flattens a capture to its channel names, for the tests that
// only care that a frame was produced.
func emissionNames(captured []capturedEmission) []string {
	out := make([]string, len(captured))
	for i, emission := range captured {
		out[i] = emission.name
	}
	return out
}

// firstEmission returns the first captured frame on a channel.
func firstEmission(t *testing.T, captured []capturedEmission, name string) any {
	t.Helper()
	for _, emission := range captured {
		if emission.name == name {
			return emission.data
		}
	}
	t.Fatalf("no %s frame in %v", name, emissionNames(captured))
	return nil
}

// Cutting a worktree moves the thread's workspace, worktree and branch. Before
// commitThreadCheckout existed only switchThreadWorkspace broadcast, so a
// worktree cut on one client left every other pane pointing at the old
// checkout.
func TestPrepareThreadWorktreeBroadcastsTheMovedRow(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-prepare-broadcast")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	snapshot := captureOrderedEmissions(app, string(eventchan.ThreadUpdated))
	updated, err := app.PrepareThreadWorktree(thread.ID, "", "feature/broadcast-prepare", false)
	if err != nil {
		t.Fatalf("PrepareThreadWorktree() error = %v", err)
	}
	t.Cleanup(func() {
		_ = app.gitCore().RemoveWorktreeForce(repo, updated.WorktreePath, true)
	})

	assertThreadRowBroadcast(t, snapshot(), updated)
}

// The attach path is the same move against a branch that already exists, and
// carried the same hole.
func TestAttachThreadWorktreeBroadcastsTheMovedRow(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)
	testutil.RunGit(t, repo, "branch", "feature/broadcast-attach")
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-attach-broadcast")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	snapshot := captureOrderedEmissions(app, string(eventchan.ThreadUpdated))
	updated, err := app.AttachThreadWorktree(thread.ID, "feature/broadcast-attach")
	if err != nil {
		t.Fatalf("AttachThreadWorktree() error = %v", err)
	}
	t.Cleanup(func() {
		_ = app.gitCore().RemoveWorktreeForce(repo, updated.WorktreePath, true)
	})

	assertThreadRowBroadcast(t, snapshot(), updated)
}

// The chokepoint's other rule, inherited from switchThreadWorkspace: a move
// that moves nothing says nothing. Re-selecting the workspace a thread already
// sits in is not a change, and a frame for it would wake every sidebar row.
func TestSwitchThreadWorkspaceToTheSamePathIsSilent(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("thread-switch-noop")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	// The first switch settles the row's branch (the seeded thread carries
	// none), which IS a change and is broadcast. The second moves nothing.
	if _, err := app.switchThreadWorkspace(thread.ID, repo); err != nil {
		t.Fatalf("switchThreadWorkspace() error = %v", err)
	}
	snapshot := captureOrderedEmissions(app, string(eventchan.ThreadUpdated))
	if _, err := app.switchThreadWorkspace(thread.ID, repo); err != nil {
		t.Fatalf("switchThreadWorkspace() error = %v", err)
	}
	if got := snapshot(); len(got) != 0 {
		t.Fatalf("a no-op workspace switch broadcast %v", emissionNames(got))
	}
}

// assertThreadRowBroadcast: exactly one `full` frame, carrying the row the RPC
// answered with. Echo-equals-optimistic-apply is the property — the initiator
// applies its own return value and then receives these same bytes.
func assertThreadRowBroadcast(t *testing.T, captured []capturedEmission, want store.Thread) {
	t.Helper()
	if len(captured) != 1 {
		t.Fatalf("thread:updated frames = %d, want 1 (%v)", len(captured), emissionNames(captured))
	}
	event, ok := captured[0].data.(triage.ThreadUpdateEvent)
	if !ok {
		t.Fatalf("thread:updated payload type = %T, want triage.ThreadUpdateEvent", captured[0].data)
	}
	if event.Action != triage.ThreadActionFull {
		t.Errorf("action = %q, want %q", event.Action, triage.ThreadActionFull)
	}
	if event.Thread == nil {
		t.Fatal("thread:updated carried no row")
	}
	if event.Thread.WorkspacePath != want.WorkspacePath ||
		event.Thread.WorktreePath != want.WorktreePath ||
		event.Thread.Branch != want.Branch {
		t.Errorf("broadcast checkout = %q/%q/%q, want %q/%q/%q",
			event.Thread.WorkspacePath, event.Thread.WorktreePath, event.Thread.Branch,
			want.WorkspacePath, want.WorktreePath, want.Branch)
	}
}

// A terminal opened on one client was invisible on every other one: the
// surface reads the set once at mount, so its output frames were dropped as
// belonging to an id it had never seen.
func TestOpenTerminalAnnouncesTheNewSession(t *testing.T) {
	app := newTestAppWithStore(t)
	app.terminals = terminal.NewManager(app.terminalOutputCallback, app.terminalExitCallback)
	t.Cleanup(func() { _ = app.terminals.Shutdown() })

	snapshot := captureOrderedEmissions(app, string(eventchan.TerminalOpened))
	handle, err := app.OpenTerminal("thread-terminal-open", TerminalOpenOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("OpenTerminal() error = %v", err)
	}
	t.Cleanup(func() { _ = app.terminals.Close(handle.TerminalID) })

	frame, ok := firstEmission(t, snapshot(), string(eventchan.TerminalOpened)).(TerminalHandle)
	if !ok {
		t.Fatalf("terminal:opened payload type = %T, want TerminalHandle", snapshot()[0].data)
	}
	if frame != handle {
		t.Errorf("terminal:opened frame = %+v, want the handle the RPC returned %+v", frame, handle)
	}
}

// Rewriting the keybindings file told nobody, so a rebind made in one window
// left every other one dispatching the old chord.
func TestUpdateKeybindingsAnnouncesTheRewrite(t *testing.T) {
	app := newTestAppWithStore(t)
	snapshot := captureOrderedEmissions(app, string(eventchan.KeybindingsUpdated))
	if err := app.UpdateKeybindings(nil); err != nil {
		t.Fatalf("UpdateKeybindings() error = %v", err)
	}
	if err := app.ResetKeybindings(); err != nil {
		t.Fatalf("ResetKeybindings() error = %v", err)
	}
	if got := len(snapshot()); got != 2 {
		t.Fatalf("keybindings:updated frames = %d, want 2 (update + reset)", got)
	}
}

// Starring a model answered the clicking client with the whole new list and
// told nobody, so every other open model menu kept the old stars.
func TestSetChatBarFavoriteAnnouncesTheWholeList(t *testing.T) {
	app := newTestAppWithStore(t)
	snapshot := captureOrderedEmissions(app, string(eventchan.ChatBarFavorites))

	favorite := store.ChatBarFavorite{Kind: "model", Provider: "codex", Value: "gpt-5.4"}
	returned, err := app.SetChatBarFavorite(favorite, true)
	if err != nil {
		t.Fatalf("SetChatBarFavorite() error = %v", err)
	}

	frame, ok := firstEmission(t, snapshot(), string(eventchan.ChatBarFavorites)).([]store.ChatBarFavorite)
	if !ok {
		t.Fatalf("chatbar:favorites payload type = %T, want []store.ChatBarFavorite", snapshot()[0].data)
	}
	if len(frame) != len(returned) {
		t.Fatalf("frame length = %d, want the RPC's %d", len(frame), len(returned))
	}
	for i := range frame {
		if frame[i] != returned[i] {
			t.Fatalf("frame[%d] = %+v, want the RPC's %+v", i, frame[i], returned[i])
		}
	}
}

// The new-thread defaults reached only the client that picked them, so every
// other "+ New" composer on the project was about to create a thread with the
// superseded model, effort and runtime mode.
func TestUpdateNewThreadDefaultsAnnouncesTheSeed(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}

	snapshot := captureOrderedEmissions(app, string(eventchan.ChatBarNewThreadDefaults))
	defaults, err := app.UpdateNewThreadDefaults(context.Background(), NewThreadDefaultsUpdate{
		ProjectID: project.ID,
		Provider:  "codex",
		Model:     "gpt-5.4",
	})
	if err != nil {
		t.Fatalf("UpdateNewThreadDefaults() error = %v", err)
	}

	frame, ok := firstEmission(t, snapshot(), string(eventchan.ChatBarNewThreadDefaults)).(NewThreadDefaultsChangedEvent)
	if !ok {
		t.Fatalf("payload type = %T, want NewThreadDefaultsChangedEvent", snapshot()[0].data)
	}
	if frame.ProjectID != project.ID {
		t.Errorf("frame project = %q, want %q", frame.ProjectID, project.ID)
	}
	if frame.Defaults != defaults {
		t.Errorf("frame defaults = %+v, want the RPC's %+v", frame.Defaults, defaults)
	}
}

// Discussion definitions are created, renamed, edited and deleted with no
// signal at all, so a definition written on one device never appeared on
// another until that screen was reopened.
func TestDiscussionDefinitionWritesAnnounceTheList(t *testing.T) {
	app := newTestAppWithStore(t)
	snapshot := captureOrderedEmissions(app, string(eventchan.DiscussionDefinitionsChanged))

	def := store.DiscussionDefinition{
		Name:        "Converge",
		Description: "Argue the design",
		Scope:       "global",
		Participants: []store.DiscussionParticipant{
			{Role: "proposer", System: "Drive the proposal"},
			{Role: "reviewer", System: "Critique the proposal"},
		},
		Settings: store.DiscussionSettings{MaxTurns: 4},
	}
	if err := app.CreateDiscussion(def); err != nil {
		t.Fatalf("CreateDiscussion() error = %v", err)
	}
	if err := app.DeleteDiscussion(def.Name, def.Scope); err != nil {
		t.Fatalf("DeleteDiscussion() error = %v", err)
	}
	if got := len(snapshot()); got != 2 {
		t.Fatalf("discussion:definitions-changed frames = %d, want 2 (create + delete)", got)
	}
}

// A removal or a rename of an attached machine told nobody, so a second page
// on this host kept a row for a profile that no longer exists — and a socket
// to it.
func TestBackendSetMutationsAnnounceThemselves(t *testing.T) {
	app := newTestAppWithStore(t)
	snapshot := captureOrderedEmissions(app, string(eventchan.BackendSetChanged))

	// No profile directory on a bare fixture: both methods refuse, and a
	// refusal must say nothing at all.
	if err := app.RemoveBackend("machine-1"); err == nil {
		t.Fatal("RemoveBackend() on an App with no profiles should refuse")
	}
	if err := app.RenameBackend("machine-1", "laptop"); err == nil {
		t.Fatal("RenameBackend() on an App with no profiles should refuse")
	}
	if got := snapshot(); len(got) != 0 {
		t.Fatalf("a refused backend mutation broadcast %v", emissionNames(got))
	}
}

// Inline review comments persisted and answered their own caller only, on both
// sets. A delete is a DELETE-OR-RESOLVE, so the frame is a refetch nudge
// naming the set rather than a row.
func TestDiffReviewCommentWritesAnnounceTheirSet(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-diff-comments")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	snapshot := captureOrderedEmissions(app, string(eventchan.ReviewCommentsChanged))
	created, err := app.CreateDiffReviewComment(thread.ID, store.DiffReviewCommentInput{
		Scope:     string(store.DiffReviewScopeWorkspace),
		SourceKey: "workspace",
		FilePath:  "main.go",
		NewLine:   3,
		Side:      "new",
		Body:      "converge",
	})
	if err != nil {
		t.Fatalf("CreateDiffReviewComment() error = %v", err)
	}
	if err := app.DeleteDiffReviewComment(thread.ID, created.ID); err != nil {
		t.Fatalf("DeleteDiffReviewComment() error = %v", err)
	}

	captured := snapshot()
	if len(captured) != 2 {
		t.Fatalf("review:comments-changed frames = %d, want 2 (create + delete): %v",
			len(captured), emissionNames(captured))
	}
	for i, emission := range captured {
		frame, ok := emission.data.(ReviewCommentsChangedEvent)
		if !ok {
			t.Fatalf("frame %d payload type = %T, want ReviewCommentsChangedEvent", i, emission.data)
		}
		if frame.ThreadID != thread.ID || frame.Scope != created.Scope || frame.SourceKey != created.SourceKey {
			t.Errorf("frame %d = %+v, want the set the read RPC takes (%s/%s/%s)",
				i, frame, thread.ID, created.Scope, created.SourceKey)
		}
		if frame.PlanItemID != "" {
			t.Errorf("frame %d named a plan item on a diff-review set: %+v", i, frame)
		}
	}
}

// Removing an account that was NOT the active one took an early return before
// any emit, so the card stayed on every other client's Settings screen until
// reload. The nudge is published before that branch.
func TestRemovingAnInactiveProviderAccountAnnouncesTheListing(t *testing.T) {
	app := newTestAppWithStore(t)
	installRemovalTestAccounts(t, app, string(provider.Claude), "first", "second")

	snapshot := captureOrderedEmissions(app, string(eventchan.ProviderAccountsChanged))
	if err := app.RemoveProviderAccount(string(provider.Claude), "first"); err != nil {
		t.Fatalf("RemoveProviderAccount() error = %v", err)
	}
	if got := len(snapshot()); got != 1 {
		t.Fatalf("provider:accounts_changed frames = %d, want 1", got)
	}
}

// An import persisted threads and auto-created projects with nothing said
// about them: the run reported its own progress and the importing client
// compensated with a whole-sidebar resync, which no other client could be
// given. Now every imported row arrives as the ordinary `listed` frame.
func TestImportedRowsAreAnnouncedPerRow(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	for _, id := range []string{"imported-1", "imported-2", "imported-3"} {
		thread := testThread(id)
		thread.ProjectID = project.ID
		thread.WorkspacePath = repo
		if err := app.store.CreateThread(thread); err != nil {
			t.Fatalf("CreateThread(%s) error = %v", id, err)
		}
	}

	var broadcast importedRowBroadcast
	snapshot := captureOrderedEmissions(app,
		string(eventchan.ThreadUpdated), string(eventchan.ProjectUpdated))

	broadcast.announce(app, sessionimport.ProgressEvent{
		ImportID:  "run-1",
		ThreadIDs: []string{"imported-1", "imported-2"},
	})
	// A second frame in the SAME run: the threads are new, the project is not.
	broadcast.announce(app, sessionimport.ProgressEvent{
		ImportID:  "run-1",
		ThreadIDs: []string{"imported-3"},
	})

	captured := snapshot()
	if got := emissionNames(captured); len(got) != 4 {
		t.Fatalf("frames = %v, want one project frame and three thread frames", got)
	}
	// The project must lead, so a client has it before the threads that name it.
	if captured[0].name != string(eventchan.ProjectUpdated) {
		t.Fatalf("first frame = %s, want the project", captured[0].name)
	}
	projectEvent, ok := captured[0].data.(triage.ProjectUpdateEvent)
	if !ok {
		t.Fatalf("project payload type = %T, want triage.ProjectUpdateEvent", captured[0].data)
	}
	if projectEvent.Action != triage.ProjectActionListed || projectEvent.Project == nil ||
		projectEvent.Project.ID != project.ID {
		t.Fatalf("project frame = %+v, want a listed row for %s", projectEvent, project.ID)
	}
	var threadIDs []string
	for _, emission := range captured[1:] {
		if emission.name != string(eventchan.ThreadUpdated) {
			t.Fatalf("a second project frame was sent for one run: %v", emissionNames(captured))
		}
		event, ok := emission.data.(triage.ThreadUpdateEvent)
		if !ok {
			t.Fatalf("thread payload type = %T, want triage.ThreadUpdateEvent", emission.data)
		}
		if event.Action != triage.ThreadActionListed || event.Thread == nil {
			t.Fatalf("thread frame = %+v, want a listed row", event)
		}
		threadIDs = append(threadIDs, event.Thread.ID)
	}
	want := []string{"imported-1", "imported-2", "imported-3"}
	for i := range want {
		if threadIDs[i] != want[i] {
			t.Fatalf("thread frames = %v, want %v", threadIDs, want)
		}
	}
}

// A new run re-owes its project frame: the dedupe is per run, not for the
// life of the process, or a project imported into twice would be announced
// once and the second run's rows would arrive naming a project a fresh client
// never heard of.
func TestASecondImportRunReannouncesItsProject(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	thread := testThread("imported-again")
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	var broadcast importedRowBroadcast
	broadcast.announce(app, sessionimport.ProgressEvent{ImportID: "run-1", ThreadIDs: []string{thread.ID}})

	snapshot := captureOrderedEmissions(app, string(eventchan.ProjectUpdated))
	broadcast.announce(app, sessionimport.ProgressEvent{ImportID: "run-2", ThreadIDs: []string{thread.ID}})
	if got := len(snapshot()); got != 1 {
		t.Fatalf("project frames on the second run = %d, want 1", got)
	}
}
