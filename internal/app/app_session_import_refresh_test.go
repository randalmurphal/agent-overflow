//go:build !providersmoke

package app

import (
	"os"
	"strings"
	"testing"

	"agent-overflow/internal/sessionimport"
	"agent-overflow/internal/store"
)

func itemCount(t *testing.T, app *App, threadID string) int {
	t.Helper()
	items, err := app.store.ListItems(threadID)
	if err != nil {
		t.Fatalf("ListItems(%s): %v", threadID, err)
	}
	return len(items)
}

func TestThreadImportUpdatesAppendsAClaudeTail(t *testing.T) {
	app := newTestAppWithStore(t)
	home := newImportHome(t)
	home.attach(app)
	home.claudeLinearSession(t, importFixtureClaudeSession)

	threads := importOneClaudeSession(t, app, home, importFixtureClaudeSession)
	if len(threads) != 1 {
		t.Fatalf("threads = %d, want 1", len(threads))
	}
	thread := threads[0]
	before := itemCount(t, app, thread.ID)

	if status, err := app.CheckThreadImportUpdates(thread.ID); err != nil {
		t.Fatalf("CheckThreadImportUpdates: %v", err)
	} else if status.Status != sessionimport.UpdateUpToDate {
		t.Fatalf("status right after import = %+v, want up-to-date", status)
	}

	// The user kept talking to Claude outside AO.
	home.appendClaudeRows(t, importFixtureClaudeSession,
		home.claudeUserRow("u2", "a1", "now document it", 10_000),
		home.claudeAssistantRow("a2", "u2", "msg-2", "Documented.", 11_000),
	)

	status, err := app.CheckThreadImportUpdates(thread.ID)
	if err != nil {
		t.Fatalf("CheckThreadImportUpdates after growth: %v", err)
	}
	if status.Status != sessionimport.UpdateAvailable {
		t.Fatalf("status = %+v, want updates-available", status)
	}
	if status.NewItems <= 0 || status.NewTurns <= 0 || status.Detail == "" {
		t.Fatalf("status = %+v, want exact counts and user-facing prose", status)
	}
	if status.ThreadID != thread.ID {
		t.Fatalf("status threadId = %q, want %q", status.ThreadID, thread.ID)
	}

	result, err := app.ImportThreadUpdates(thread.ID)
	if err != nil {
		t.Fatalf("ImportThreadUpdates: %v", err)
	}
	if result.AppliedItems != status.NewItems || result.AppliedTurns != status.NewTurns {
		t.Fatalf("applied %+v, want the counts the check reported (%+v)", result, status)
	}
	if after := itemCount(t, app, thread.ID); after != before+result.AppliedItems {
		t.Fatalf("items = %d, want %d after appending %d", after, before+result.AppliedItems, result.AppliedItems)
	}

	// The cursor advanced, so a second check has nothing to add.
	if status, err := app.CheckThreadImportUpdates(thread.ID); err != nil {
		t.Fatalf("CheckThreadImportUpdates after apply: %v", err)
	} else if status.Status != sessionimport.UpdateUpToDate {
		t.Fatalf("status after apply = %+v, want up-to-date", status)
	}
	state, found, err := app.store.GetThreadImportState(thread.ID)
	if err != nil || !found {
		t.Fatalf("GetThreadImportState = %v, %v", found, err)
	}
	if state.RefreshedAt <= 0 {
		t.Error("refreshedAt was not stamped by the apply")
	}
	if state.LastSourceUUID != "a2" {
		t.Errorf("cursor uuid = %q, want the newest imported row a2", state.LastSourceUUID)
	}
}

func TestThreadImportUpdatesAppendsACodexTail(t *testing.T) {
	app := newTestAppWithStore(t)
	home := newImportHome(t)
	home.attach(app)
	home.writeCodexIndex(t, importFixtureCodexThread)
	home.codexLinearSession(t, importFixtureCodexThread)

	if _, err := app.ListImportableSessions(ImportScanRequest{}); err != nil {
		t.Fatalf("list: %v", err)
	}
	frames := runImport(t, app, "codex:"+importFixtureCodexThread)
	if frames[0].Status != sessionImportStatusImported {
		t.Fatalf("import frame = %+v, want imported", frames[0])
	}
	threadID := frames[0].ThreadIDs[0]
	before := itemCount(t, app, threadID)

	home.appendCodexRollout(t, importFixtureCodexThread,
		codexFixtureTurn(t, "turn-2", "now document it", "Documented.", 1_000)...)

	status, err := app.CheckThreadImportUpdates(threadID)
	if err != nil {
		t.Fatalf("CheckThreadImportUpdates: %v", err)
	}
	if status.Status != sessionimport.UpdateAvailable || status.NewTurns != 1 {
		t.Fatalf("status = %+v, want updates-available with one new turn", status)
	}
	if _, err := app.ImportThreadUpdates(threadID); err != nil {
		t.Fatalf("ImportThreadUpdates: %v", err)
	}
	if after := itemCount(t, app, threadID); after <= before {
		t.Fatalf("items = %d, want more than the %d the import wrote", after, before)
	}
	if status, err := app.CheckThreadImportUpdates(threadID); err != nil {
		t.Fatalf("CheckThreadImportUpdates after apply: %v", err)
	} else if status.Status != sessionimport.UpdateUpToDate {
		t.Fatalf("status after apply = %+v, want up-to-date", status)
	}
}

func TestThreadImportUpdatesReportsAndAppliesAProfileOnlyRepair(t *testing.T) {
	app := newTestAppWithStore(t)
	home := newImportHome(t)
	home.attach(app)
	home.writeCodexIndex(t, importFixtureCodexThread)
	home.codexLinearSession(t, importFixtureCodexThread)

	if _, err := app.ListImportableSessions(ImportScanRequest{}); err != nil {
		t.Fatalf("list: %v", err)
	}
	threadID := runImport(t, app, "codex:"+importFixtureCodexThread)[0].ThreadIDs[0]
	if err := app.store.UpdateModel(threadID, ""); err != nil {
		t.Fatalf("clear imported model: %v", err)
	}

	status, err := app.CheckThreadImportUpdates(threadID)
	if err != nil {
		t.Fatalf("CheckThreadImportUpdates: %v", err)
	}
	if status.Status != sessionimport.UpdateAvailable || status.NewItems != 0 || status.NewTurns != 0 ||
		!status.RestoresModelProfile || status.Detail == "" {
		t.Fatalf("profile repair status = %+v", status)
	}
	result, err := app.ImportThreadUpdates(threadID)
	if err != nil {
		t.Fatalf("ImportThreadUpdates: %v", err)
	}
	if result.AppliedItems != 0 || result.AppliedTurns != 0 || !result.RestoredModelProfile {
		t.Fatalf("profile repair result = %+v", result)
	}
	thread, err := app.store.GetThread(threadID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if thread.Model != "gpt-5.6-sol" || thread.ReasoningEffort != "high" {
		t.Fatalf("restored profile = %q/%q", thread.Model, thread.ReasoningEffort)
	}
}

// A thread the user continued inside AO has a future of its own. Appending
// the file's tail would interleave two conversations, so the refresh refuses
// instead of repairing.
func TestThreadImportUpdatesRefusesAThreadContinuedInAO(t *testing.T) {
	app := newTestAppWithStore(t)
	home := newImportHome(t)
	home.attach(app)
	home.claudeLinearSession(t, importFixtureClaudeSession)
	thread := importOneClaudeSession(t, app, home, importFixtureClaudeSession)[0]

	state, _, err := app.store.GetThreadImportState(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadImportState: %v", err)
	}
	if err := app.store.InsertItem(store.Item{
		ID: "local-1", ThreadID: thread.ID,
		TurnIndex: state.LastTurnIndex + 1, ItemIndex: 0,
		Kind: "user_text", Role: "user", Summary: "continued in AO",
		CreatedAt: importFixtureMillis + 100_000,
	}); err != nil {
		t.Fatalf("InsertItem: %v", err)
	}
	home.appendClaudeRows(t, importFixtureClaudeSession,
		home.claudeUserRow("u2", "a1", "now document it", 10_000),
		home.claudeAssistantRow("a2", "u2", "msg-2", "Documented.", 11_000),
	)

	status, err := app.CheckThreadImportUpdates(thread.ID)
	if err != nil {
		t.Fatalf("CheckThreadImportUpdates: %v", err)
	}
	if status.Status != sessionimport.UpdateDivergedLocal || status.Detail == "" {
		t.Fatalf("status = %+v, want diverged-local with an explanation", status)
	}
	if _, err := app.ImportThreadUpdates(thread.ID); err == nil {
		t.Fatal("ImportThreadUpdates on a diverged thread = nil error, want the refusal surfaced")
	}
}

func TestThreadImportUpdatesReportsAMissingSource(t *testing.T) {
	app := newTestAppWithStore(t)
	home := newImportHome(t)
	home.attach(app)
	home.claudeLinearSession(t, importFixtureClaudeSession)
	thread := importOneClaudeSession(t, app, home, importFixtureClaudeSession)[0]

	if err := os.Remove(home.claudeSessionPath(importFixtureClaudeSession)); err != nil {
		t.Fatalf("remove transcript: %v", err)
	}
	status, err := app.CheckThreadImportUpdates(thread.ID)
	if err != nil {
		t.Fatalf("CheckThreadImportUpdates: %v", err)
	}
	if status.Status != sessionimport.UpdateSourceMissing {
		t.Fatalf("status = %+v, want source-missing", status)
	}
	if !strings.Contains(status.Detail, importFixtureClaudeSession) {
		t.Errorf("detail = %q, want it to name the file that is gone", status.Detail)
	}
}

// A rollout that shrank was replaced, not extended: its history no longer
// continues the thread, and a tail read from the old offset would be garbage.
func TestThreadImportUpdatesReportsADivergedCodexSource(t *testing.T) {
	app := newTestAppWithStore(t)
	home := newImportHome(t)
	home.attach(app)
	home.writeCodexIndex(t, importFixtureCodexThread)
	home.codexLinearSession(t, importFixtureCodexThread)

	if _, err := app.ListImportableSessions(ImportScanRequest{}); err != nil {
		t.Fatalf("list: %v", err)
	}
	threadID := runImport(t, app, "codex:"+importFixtureCodexThread)[0].ThreadIDs[0]

	if err := os.WriteFile(home.codexRolloutPath(importFixtureCodexThread), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("truncate rollout: %v", err)
	}
	status, err := app.CheckThreadImportUpdates(threadID)
	if err != nil {
		t.Fatalf("CheckThreadImportUpdates: %v", err)
	}
	if status.Status != sessionimport.UpdateSourceDiverged || status.Detail == "" {
		t.Fatalf("status = %+v, want source-diverged with an explanation", status)
	}
}

func TestCheckThreadImportUpdatesReportsANonImportedThread(t *testing.T) {
	app := newTestAppWithStore(t)
	// Even the answer "this thread was never imported" resolves the provider
	// homes first, so the fixture home is required — sessionImportDeps refuses
	// to fall through to the developer's real ~/.claude inside a test binary.
	newImportHome(t).attach(app)
	thread := testThread("thread-not-imported")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	status, err := app.CheckThreadImportUpdates(thread.ID)
	if err != nil {
		t.Fatalf("CheckThreadImportUpdates: %v", err)
	}
	if status.Status != sessionimport.UpdateNotImported || status.Detail == "" {
		t.Fatalf("status = %+v, want not-imported with an explanation", status)
	}
}
