package sessionimport

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/store"
)

// refresh_test.go — the rules a re-read of a source file has to obey that
// the check RPC has already promised the user.
//
// PlanUpdate is BOTH the check and the plan, which is what makes "refused
// before anything lands" meaningful: every failure mode below has to be
// discovered while the batch is still store-pure. A turn-id collision that
// only surfaced on ApplyImportBatch's INSERT would fail AFTER
// CheckThreadImportUpdates reported "updates available".

func importOneCodexFixture(t *testing.T, d Deps) string {
	t.Helper()
	outcome := importFixtureRow(t, d, scanOne(t, d, ProviderCodex))
	if len(outcome.Threads) != 1 {
		t.Fatalf("threads = %d, want 1", len(outcome.Threads))
	}
	return outcome.Threads[0].ID
}

func assertTurnCount(t *testing.T, d Deps, threadID string, want int) {
	t.Helper()
	ids, err := d.Store.TurnIDsForThread(threadID)
	if err != nil {
		t.Fatalf("turn ids: %v", err)
	}
	if len(ids) != want {
		t.Fatalf("thread holds %d turns, want %d: %v", len(ids), want, ids)
	}
}

// A rollout imported mid-turn stops before its `task_complete`. The record
// that eventually arrives names the SAME wire turn id, and the Codex reader
// re-opens a turn for it because it has no idea what was imported. Refusing
// is the honest answer; dying on the primary key after the check said
// "2 new messages" is not.
func TestRefreshRefusesACodexTailThatReopensAnImportedTurn(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.writeCodexIndex(t, codexThreadA)
	path := homes.writeCodexRollout(t, codexThreadA,
		homes.codexMetaLine(codexThreadA, ""),
		codexLine(0, "turn_context", map[string]any{
			"turn_id": "turn-1", "cwd": homes.workspace,
			"model": "gpt-5.6-sol", "effort": "high",
		}),
		codexLine(100, "event_msg", map[string]any{"type": "task_started", "turn_id": "turn-1"}),
		codexLine(200, "event_msg", map[string]any{"type": "user_message", "message": "add a test"}),
	)
	d := homes.deps(st)
	threadID := importOneCodexFixture(t, d)
	assertTurnCount(t, d, threadID, 1)

	appendRolloutLines(t, path, codexLine(300, "event_msg", map[string]any{
		"type": "task_complete", "turn_id": "turn-1", "last_agent_message": "done",
	}))

	_, err := PlanUpdate(context.Background(), d, threadID)
	if err == nil {
		t.Fatal("PlanUpdate over a tail that re-opens turn-1: want a refusal, got nil")
	}
	if !strings.Contains(err.Error(), "turn-1") {
		t.Errorf("refusal = %q, want it to name the turn that collided", err)
	}
	assertTurnCount(t, d, threadID, 1)
}

// The same collision through the other door: a rollout with content before
// any `task_started` gets a SYNTHETIC turn, whose id is minted from a
// deterministic session-local coordinate — so a tail refresh re-mints the
// same `import-turn:<sessionID>:1` identity.
func TestRefreshRefusesACodexTailThatReopensASyntheticTurn(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.writeCodexIndex(t, codexThreadA)
	path := homes.writeCodexRollout(t, codexThreadA,
		homes.codexMetaLine(codexThreadA, ""),
		codexLine(100, "event_msg", map[string]any{"type": "user_message", "message": "inherited prefix"}),
	)
	d := homes.deps(st)
	threadID := importOneCodexFixture(t, d)
	assertTurnCount(t, d, threadID, 1)

	appendRolloutLines(t, path, codexLine(200, "event_msg", map[string]any{
		"type": "agent_message", "message": "still outside a turn", "phase": "final_answer",
	}))

	_, err := PlanUpdate(context.Background(), d, threadID)
	if err == nil {
		t.Fatal("PlanUpdate over a tail that re-mints its synthetic turn: want a refusal, got nil")
	}
	if !strings.Contains(err.Error(), "import-turn:"+codexThreadA+":1") {
		t.Errorf("refusal = %q, want it to name the turn that collided", err)
	}
	assertTurnCount(t, d, threadID, 1)
}

// The Codex containment proof runs BEFORE the recorded path is touched at
// all — not merely before it is opened. A `source_path` outside this
// machine's Codex home is therefore answered from the path alone, whether or
// not anything is there: stat'ing first would report the existence (and, on
// the other branch, the size) of an arbitrary file back to the user through
// the refresh status, and would answer "source-missing" about a file this
// app was never allowed to look at.
func TestRefreshProvesCodexContainmentBeforeStattingTheSource(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.writeCodexIndex(t, codexThreadA)
	homes.codexLinearSession(t, codexThreadA)
	d := homes.deps(st)
	threadID := importOneCodexFixture(t, d)

	outside := filepath.Join(t.TempDir(), "not-in-the-codex-home.jsonl")
	repointImportSource(t, st, threadID, outside)

	update, err := PlanUpdate(context.Background(), d, threadID)
	if err != nil {
		t.Fatalf("PlanUpdate: %v", err)
	}
	if update.Status != UpdateSourceDiverged {
		t.Fatalf("status = %q (%s), want source-diverged", update.Status, update.Detail)
	}
	if !strings.Contains(update.Detail, "Codex home") {
		t.Errorf("detail = %q, want it to say the file is not in this machine's Codex home", update.Detail)
	}
}

// A rollout that IS contained and simply gone is the other answer, and it
// still names the path the import recorded.
func TestRefreshReportsADeletedCodexRolloutAsSourceMissing(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.writeCodexIndex(t, codexThreadA)
	path := homes.codexLinearSession(t, codexThreadA)
	d := homes.deps(st)
	threadID := importOneCodexFixture(t, d)

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove rollout: %v", err)
	}
	update, err := PlanUpdate(context.Background(), d, threadID)
	if err != nil {
		t.Fatalf("PlanUpdate: %v", err)
	}
	if update.Status != UpdateSourceMissing {
		t.Fatalf("status = %q (%s), want source-missing", update.Status, update.Detail)
	}
	if !strings.Contains(update.Detail, path) {
		t.Errorf("detail = %q, want it to name the file that is gone", update.Detail)
	}
}

// repointImportSource rewrites where a thread believes it was imported from.
func repointImportSource(t *testing.T, st *store.Store, threadID, sourcePath string) {
	t.Helper()
	state, found, err := st.GetThreadImportState(threadID)
	if err != nil || !found {
		t.Fatalf("import state for %s: found=%v err=%v", threadID, found, err)
	}
	state.SourcePath = sourcePath
	if err := st.SetThreadImportState(state); err != nil {
		t.Fatalf("SetThreadImportState: %v", err)
	}
}

// The ordinary case still works: a tail that opens a turn the thread does
// not hold appends, and the refusal above is not a blanket one.
func TestRefreshAppendsACodexTailThatOpensANewTurn(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.writeCodexIndex(t, codexThreadA)
	path := homes.codexLinearSession(t, codexThreadA)
	d := homes.deps(st)
	threadID := importOneCodexFixture(t, d)
	assertTurnCount(t, d, threadID, 1)

	appendRolloutLines(t, path,
		codexLine(900, "event_msg", map[string]any{"type": "task_started", "turn_id": "turn-2"}),
		codexLine(1_000, "event_msg", map[string]any{"type": "user_message", "message": "and one more"}),
		codexLine(1_100, "event_msg", map[string]any{
			"type": "task_complete", "turn_id": "turn-2", "last_agent_message": "done",
		}),
	)

	update, err := PlanUpdate(context.Background(), d, threadID)
	if err != nil {
		t.Fatalf("PlanUpdate: %v", err)
	}
	if !update.Appliable() {
		t.Fatalf("status = %q (%s), want updates-available", update.Status, update.Detail)
	}
	if _, err := ApplyUpdate(d, update); err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}
	assertTurnCount(t, d, threadID, 2)
}

func TestRefreshRepairsAnOlderCodexImportWithoutReplayingHistory(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.writeCodexIndex(t, codexThreadA)
	homes.codexLinearSession(t, codexThreadA)
	d := homes.deps(st)
	threadID := importOneCodexFixture(t, d)
	before, found, err := st.GetThreadImportState(threadID)
	if err != nil || !found {
		t.Fatalf("GetThreadImportState before repair: found=%v err=%v", found, err)
	}
	if err := st.UpdateModel(threadID, ""); err != nil {
		t.Fatalf("clear model to simulate an older import: %v", err)
	}

	update, err := PlanUpdate(context.Background(), d, threadID)
	if err != nil {
		t.Fatalf("PlanUpdate: %v", err)
	}
	if !update.Appliable() || update.NewItems != 0 || update.NewTurns != 0 {
		t.Fatalf("repair plan = status:%q items:%d turns:%d detail:%q",
			update.Status, update.NewItems, update.NewTurns, update.Detail)
	}
	result, err := ApplyUpdate(d, update)
	if err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}
	if !result.RestoredModelProfile {
		t.Fatal("ApplyUpdate did not report the model-profile repair")
	}
	thread, err := st.GetThread(threadID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if thread.Model != "gpt-5.6-sol" || thread.ReasoningEffort != "high" {
		t.Fatalf("repaired profile = %q/%q", thread.Model, thread.ReasoningEffort)
	}
	after, found, err := st.GetThreadImportState(threadID)
	if err != nil || !found {
		t.Fatalf("GetThreadImportState after repair: found=%v err=%v", found, err)
	}
	if after.LastTurnIndex != before.LastTurnIndex || after.LastItemIndex != before.LastItemIndex ||
		after.LastSourceUUID != before.LastSourceUUID || after.LastSourceOffset != before.LastSourceOffset {
		t.Fatalf("profile-only repair moved the import cursor: before=%+v after=%+v", before, after)
	}
	assertTurnCount(t, d, threadID, 1)
}

func TestRefreshProfileRepairDoesNotOverwriteANewerUserSelection(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.writeCodexIndex(t, codexThreadA)
	homes.codexLinearSession(t, codexThreadA)
	d := homes.deps(st)
	threadID := importOneCodexFixture(t, d)
	if err := st.UpdateModel(threadID, ""); err != nil {
		t.Fatalf("clear model: %v", err)
	}
	update, err := PlanUpdate(context.Background(), d, threadID)
	if err != nil {
		t.Fatalf("PlanUpdate: %v", err)
	}
	if err := st.UpdateModel(threadID, "gpt-user-choice"); err != nil {
		t.Fatalf("record newer user choice: %v", err)
	}
	result, err := ApplyUpdate(d, update)
	if err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}
	if result.RestoredModelProfile {
		t.Fatal("stale repair reported a profile change after compare-and-swap lost")
	}
	thread, err := st.GetThread(threadID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if thread.Model != "gpt-user-choice" {
		t.Fatalf("model = %q, want the newer user selection", thread.Model)
	}
}

func TestRefreshWithNewHistoryPreservesAnExistingModelSelection(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.writeCodexIndex(t, codexThreadA)
	path := homes.codexLinearSession(t, codexThreadA)
	d := homes.deps(st)
	threadID := importOneCodexFixture(t, d)
	if err := st.UpdateModel(threadID, "gpt-user-choice"); err != nil {
		t.Fatalf("record user model selection: %v", err)
	}

	appendRolloutLines(t, path,
		codexLine(900, "event_msg", map[string]any{
			"type": "task_started", "turn_id": "turn-2", "model_context_window": 200000,
		}),
		codexLine(1_000, "turn_context", map[string]any{
			"turn_id": "turn-2", "model": "gpt-5.4", "effort": "xhigh",
		}),
		codexLine(1_100, "event_msg", map[string]any{
			"type": "user_message", "message": "continued outside AO",
		}),
		codexLine(1_200, "event_msg", map[string]any{
			"type": "task_complete", "turn_id": "turn-2", "last_agent_message": "done",
		}),
	)
	update, err := PlanUpdate(context.Background(), d, threadID)
	if err != nil {
		t.Fatalf("PlanUpdate: %v", err)
	}
	if !update.Appliable() || update.RestoresModelProfile() {
		t.Fatalf("update = status:%q restore-profile:%v", update.Status, update.RestoresModelProfile())
	}
	result, err := ApplyUpdate(d, update)
	if err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}
	if result.RestoredModelProfile {
		t.Fatal("history refresh reported an unrequested profile change")
	}
	thread, err := st.GetThread(threadID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if thread.Model != "gpt-user-choice" {
		t.Fatalf("model = %q, want the existing selection", thread.Model)
	}
	assertTurnCount(t, d, threadID, 2)
}

func TestClaudeRefreshDoesNotMistakeTheHistoricalProfileForARepair(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.claudeLinearSession(t, claudeSessionA)
	d := homes.deps(st)
	outcome := importFixtureRow(t, d, scanOne(t, d, ProviderClaude))
	threadID := outcome.Threads[0].ID
	if err := st.UpdateModel(threadID, "claude-user-choice"); err != nil {
		t.Fatalf("record user model selection: %v", err)
	}

	update, err := PlanUpdate(context.Background(), d, threadID)
	if err != nil {
		t.Fatalf("PlanUpdate: %v", err)
	}
	if update.Status != UpdateUpToDate {
		t.Fatalf("status = %q (%s), want up-to-date", update.Status, update.Detail)
	}
	thread, err := st.GetThread(threadID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if thread.Model != "claude-user-choice" {
		t.Fatalf("model = %q, want the user's selection", thread.Model)
	}
}
