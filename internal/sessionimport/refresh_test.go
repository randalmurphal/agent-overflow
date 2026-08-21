package sessionimport

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex/rollout"
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

// --------------------------------------------- codex source-identity guard
//
// Codex 0.147 can rewrite a rollout IN PLACE when it migrates a thread from
// `legacy` to `paginated` history: the whole file is canonicalised and
// atomically published over the same path. The rewritten file is usually the
// same size or larger, so the append-only size test passes while every byte
// offset in it addresses a different record — which is why the import records
// a fingerprint of the file's header and the refresh compares it first.

// paginatedMigratedRollout is the shape codex-rs's canonicalizer leaves
// behind: the same thread id and path, `history_mode: paginated` in the
// header, and the legacy per-family events replaced by `item_completed`
// records. Hand-written against tag rust-v0.149.0.
func (h providerHomes) paginatedMigratedRollout(t *testing.T, threadID string, padding string) string {
	t.Helper()
	meta := codexLine(0, "session_meta", map[string]any{
		"id": threadID, "cwd": h.workspace, "originator": "codex_cli",
		"cli_version": "0.149.0", "history_mode": "paginated",
		"git": map[string]any{"branch": "main"},
	})
	return h.writeCodexRollout(t, threadID,
		meta,
		codexLine(0, "event_msg", map[string]any{
			"type": "task_started", "turn_id": "turn-1", "model_context_window": 258400,
		}),
		codexLine(100, "turn_context", map[string]any{
			"turn_id": "turn-1", "cwd": h.workspace,
			"model": "gpt-5.6-sol", "effort": "high",
		}),
		codexLine(200, "response_item", map[string]any{
			"type": "message", "id": "m1", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": "add a test" + padding}},
		}),
		codexLine(600, "response_item", map[string]any{
			"type": "message", "id": "msg-1", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": "All tests pass."}},
		}),
		codexLine(800, "event_msg", map[string]any{
			"type": "task_complete", "turn_id": "turn-1", "last_agent_message": "All tests pass.",
		}),
	)
}

func planCodexRefresh(t *testing.T, d Deps, threadID string) Update {
	t.Helper()
	update, err := PlanUpdate(context.Background(), d, threadID)
	if err != nil {
		t.Fatalf("PlanUpdate: %v", err)
	}
	return update
}

// The case the size test cannot see: a migrated file that happens to be the
// SAME SIZE as the one that was imported.
func TestRefreshRefusesACodexRolloutMigratedToTheSameSize(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.writeCodexIndex(t, codexThreadA)
	path := homes.codexLinearSession(t, codexThreadA)
	d := homes.deps(st)
	threadID := importOneCodexFixture(t, d)

	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat original rollout: %v", err)
	}
	// Grow the replacement's user text until the two files match byte for
	// byte in length, which is exactly the blind spot of the size check.
	var after os.FileInfo
	for pad := 0; pad < 4096; pad++ {
		homes.paginatedMigratedRollout(t, codexThreadA, strings.Repeat("x", pad))
		after, err = os.Stat(path)
		if err != nil {
			t.Fatalf("stat migrated rollout: %v", err)
		}
		if after.Size() == before.Size() {
			break
		}
		if after.Size() > before.Size() {
			t.Fatalf("could not land on the original size (%d vs %d)", after.Size(), before.Size())
		}
	}
	if after.Size() != before.Size() {
		t.Fatalf("fixture never reached the original size: %d vs %d", after.Size(), before.Size())
	}

	update := planCodexRefresh(t, d, threadID)
	if update.Status != UpdateSourceDiverged {
		t.Fatalf("status = %q (%s), want source-diverged", update.Status, update.Detail)
	}
	if !strings.Contains(update.Detail, "migrated") {
		t.Errorf("detail = %q, want it to name the history migration as the cause", update.Detail)
	}
}

// The same rewrite, but LARGER than the file that was imported — which reads
// to the size test as an ordinary append.
func TestRefreshRefusesACodexRolloutMigratedToALargerFile(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.writeCodexIndex(t, codexThreadA)
	path := homes.codexLinearSession(t, codexThreadA)
	d := homes.deps(st)
	threadID := importOneCodexFixture(t, d)

	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat original rollout: %v", err)
	}
	homes.paginatedMigratedRollout(t, codexThreadA, strings.Repeat("y", 8192))
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat migrated rollout: %v", err)
	}
	if after.Size() <= before.Size() {
		t.Fatalf("fixture is not larger: %d vs %d", after.Size(), before.Size())
	}

	update := planCodexRefresh(t, d, threadID)
	if update.Status != UpdateSourceDiverged {
		t.Fatalf("status = %q (%s), want source-diverged", update.Status, update.Detail)
	}
	// It must be the IDENTITY guard that refused it, not the record-boundary
	// check downstream — a larger rewritten file can pass that one by luck.
	if !strings.Contains(update.Detail, "migrated") {
		t.Errorf("detail = %q, want the identity guard's migration prose", update.Detail)
	}
}

// A history_mode FLIP with the same header bytes otherwise is still a
// divergence, and it is named as one.
func TestRefreshRefusesACodexRolloutWhoseHistoryModeChanged(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.writeCodexIndex(t, codexThreadA)
	homes.codexLinearSession(t, codexThreadA)
	d := homes.deps(st)
	threadID := importOneCodexFixture(t, d)

	state, found, err := st.GetThreadImportState(threadID)
	if err != nil || !found {
		t.Fatalf("import state: found=%v err=%v", found, err)
	}
	if state.SourceHistoryMode != "" {
		t.Fatalf("legacy fixture recorded history mode %q, want empty", state.SourceHistoryMode)
	}
	if state.SourceMetaHash == "" {
		t.Fatal("import recorded no source fingerprint")
	}
	// Pretend the file now declares paginated while everything the import
	// hashed is otherwise unchanged: only the MODE differs.
	state.SourceHistoryMode = "paginated"
	if err := st.SetThreadImportState(state); err != nil {
		t.Fatalf("SetThreadImportState: %v", err)
	}

	update := planCodexRefresh(t, d, threadID)
	if update.Status != UpdateSourceDiverged {
		t.Fatalf("status = %q (%s), want source-diverged", update.Status, update.Detail)
	}
	if !strings.Contains(update.Detail, "paginated") || !strings.Contains(update.Detail, "legacy") {
		t.Errorf("detail = %q, want both modes named", update.Detail)
	}
}

// The guard must not fire on the ordinary case it sits in front of: a
// genuine append leaves the header alone.
func TestRefreshAcceptsAGenuineCodexAppendWithTheIdentityGuardArmed(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.writeCodexIndex(t, codexThreadA)
	path := homes.codexLinearSession(t, codexThreadA)
	d := homes.deps(st)
	threadID := importOneCodexFixture(t, d)

	state, found, err := st.GetThreadImportState(threadID)
	if err != nil || !found {
		t.Fatalf("import state: found=%v err=%v", found, err)
	}
	if state.SourceMetaHash == "" {
		t.Fatal("import recorded no source fingerprint, so the guard is not armed")
	}

	appendRolloutLines(t, path,
		codexLine(900, "event_msg", map[string]any{"type": "task_started", "turn_id": "turn-2"}),
		codexLine(1_000, "event_msg", map[string]any{"type": "user_message", "message": "and one more"}),
		codexLine(1_100, "event_msg", map[string]any{
			"type": "task_complete", "turn_id": "turn-2", "last_agent_message": "done",
		}),
	)
	update := planCodexRefresh(t, d, threadID)
	if !update.Appliable() {
		t.Fatalf("status = %q (%s), want updates-available", update.Status, update.Detail)
	}
	if _, err := ApplyUpdate(d, update); err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}
	assertTurnCount(t, d, threadID, 2)
}

// A thread imported before migration v67 has no fingerprint at all. Empty
// means UNKNOWN, not mismatched: it must keep refreshing under the size test
// it has always had, and the apply must BACKFILL the fingerprint so the guard
// is armed from then on.
func TestRefreshBackfillsAMissingSourceFingerprintInsteadOfDiverging(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.writeCodexIndex(t, codexThreadA)
	path := homes.codexLinearSession(t, codexThreadA)
	d := homes.deps(st)
	threadID := importOneCodexFixture(t, d)

	state, found, err := st.GetThreadImportState(threadID)
	if err != nil || !found {
		t.Fatalf("import state: found=%v err=%v", found, err)
	}
	state.SourceMetaHash = ""
	state.SourceHistoryMode = ""
	if err := st.SetThreadImportState(state); err != nil {
		t.Fatalf("SetThreadImportState: %v", err)
	}

	appendRolloutLines(t, path,
		codexLine(900, "event_msg", map[string]any{"type": "task_started", "turn_id": "turn-2"}),
		codexLine(1_000, "event_msg", map[string]any{"type": "user_message", "message": "and one more"}),
		codexLine(1_100, "event_msg", map[string]any{
			"type": "task_complete", "turn_id": "turn-2", "last_agent_message": "done",
		}),
	)
	update := planCodexRefresh(t, d, threadID)
	if !update.Appliable() {
		t.Fatalf("status = %q (%s), want a pre-v67 row to still refresh", update.Status, update.Detail)
	}
	if _, err := ApplyUpdate(d, update); err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}
	after, found, err := st.GetThreadImportState(threadID)
	if err != nil || !found {
		t.Fatalf("import state after apply: found=%v err=%v", found, err)
	}
	if after.SourceMetaHash == "" {
		t.Fatal("apply did not backfill the source fingerprint")
	}
}

// The whole point of the guard: a migrated rollout is refused rather than
// resumed, so nothing from the rewritten file lands in the thread.
func TestRefreshWritesNothingWhenTheSourceIdentityChanged(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.writeCodexIndex(t, codexThreadA)
	homes.codexLinearSession(t, codexThreadA)
	d := homes.deps(st)
	threadID := importOneCodexFixture(t, d)
	assertTurnCount(t, d, threadID, 1)

	homes.paginatedMigratedRollout(t, codexThreadA, strings.Repeat("z", 4096))
	update := planCodexRefresh(t, d, threadID)
	if update.Status != UpdateSourceDiverged {
		t.Fatalf("status = %q (%s)", update.Status, update.Detail)
	}
	if !strings.Contains(update.Detail, "migrated") {
		t.Errorf("detail = %q, want the identity guard's migration prose", update.Detail)
	}
	if _, err := ApplyUpdate(d, update); err == nil {
		t.Fatal("ApplyUpdate accepted a refused plan")
	}
	assertTurnCount(t, d, threadID, 1)
}

// The hash branch on its own: the header changed while the declared history
// mode did not. That is a rollout replaced by something else, and it is worth
// its own refusal because the mode comparison cannot see it.
func TestRefreshRefusesACodexRolloutWhoseHeaderChangedWithoutTheMode(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.writeCodexIndex(t, codexThreadA)
	homes.codexLinearSession(t, codexThreadA)
	d := homes.deps(st)
	threadID := importOneCodexFixture(t, d)

	state, found, err := st.GetThreadImportState(threadID)
	if err != nil || !found {
		t.Fatalf("import state: found=%v err=%v", found, err)
	}
	state.SourceMetaHash = strings.Repeat("0", 64)
	if err := st.SetThreadImportState(state); err != nil {
		t.Fatalf("SetThreadImportState: %v", err)
	}

	update := planCodexRefresh(t, d, threadID)
	if update.Status != UpdateSourceDiverged {
		t.Fatalf("status = %q (%s), want source-diverged", update.Status, update.Detail)
	}
	if !strings.Contains(update.Detail, "header of") {
		t.Errorf("detail = %q, want the header-mismatch prose", update.Detail)
	}
}

// An unreadable header is UNKNOWN, not diverged. `source-diverged` is a
// permanent verdict about the FILE's contents — it tells the user this
// rollout no longer continues their thread — and a header AO could not read
// says nothing about the contents at all. It is also the posture the first
// import already takes (import_one.go), so a refresh must not fail where the
// import that created the row succeeded.
func TestRefreshDoesNotCallAnUnreadableCodexHeaderDiverged(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000-mode file, so the fixture cannot be made unreadable")
	}
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.writeCodexIndex(t, codexThreadA)
	path := homes.codexLinearSession(t, codexThreadA)
	d := homes.deps(st)
	threadID := importOneCodexFixture(t, d)

	// Armed guard: without a recorded fingerprint there is nothing to
	// compare and the test would pass on a technicality.
	state, found, err := st.GetThreadImportState(threadID)
	if err != nil || !found {
		t.Fatalf("import state: found=%v err=%v", found, err)
	}
	if state.SourceMetaHash == "" {
		t.Fatal("import recorded no source fingerprint, so the guard is not armed")
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	update, err := PlanUpdate(context.Background(), d, threadID)
	if err == nil && update.Status == UpdateSourceDiverged {
		t.Fatalf("unreadable header reported as divergence: %s", update.Detail)
	}
	// And the fingerprint the next readable refresh compares against must
	// survive: nothing was learned, so nothing may be overwritten.
	after, found, err := st.GetThreadImportState(threadID)
	if err != nil || !found {
		t.Fatalf("import state after: found=%v err=%v", found, err)
	}
	if after.SourceMetaHash != state.SourceMetaHash {
		t.Fatalf("fingerprint changed: %q -> %q", state.SourceMetaHash, after.SourceMetaHash)
	}
}

// The FINGERPRINTED half of the unreadable-header split, and the half that
// fails closed. A row with a recorded fingerprint AND a position inside the
// file has an identity precisely so a rewritten rollout cannot be resumed
// from; skipping the comparison because the header could not be read skips
// it exactly when the file is least trustworthy. The remedy is the same one
// divergence asks for — re-import — so it is worded as divergence.
//
// The fixture is a DIRECTORY inside the Codex home, which is the one thing
// that opens and stats like a file and then fails every read of the handle
// (EISDIR): the fd is good, the header is not readable through it. A
// 0000-mode file cannot produce this state — os.Open itself fails, which
// lands on source-missing (TestRefreshDoesNotCallAnUnreadableCodexHeaderDiverged
// is that path, and asserts only that it is not called divergence).
func TestRefreshFailsClosedWhenAFingerprintedCodexHeaderCannotBeRead(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.writeCodexIndex(t, codexThreadA)
	homes.codexLinearSession(t, codexThreadA)
	d := homes.deps(st)
	threadID := importOneCodexFixture(t, d)
	assertTurnCount(t, d, threadID, 1)
	itemsBefore := importedItemCount(t, st, threadID)

	before, found, err := st.GetThreadImportState(threadID)
	if err != nil || !found {
		t.Fatalf("import state: found=%v err=%v", found, err)
	}
	if before.SourceMetaHash == "" {
		t.Fatal("import recorded no source fingerprint, so the fail-closed branch is not armed")
	}
	if before.LastSourceOffset <= 0 {
		t.Fatalf("import recorded offset %d, want a real resume position", before.LastSourceOffset)
	}

	unreadable := filepath.Join(homes.codexHome, "sessions")
	repointImportSource(t, st, threadID, unreadable)

	update, err := PlanUpdate(context.Background(), d, threadID)
	if err != nil {
		t.Fatalf("PlanUpdate: %v", err)
	}
	if update.Status != UpdateSourceDiverged {
		t.Fatalf("status = %q (%s), want source-diverged", update.Status, update.Detail)
	}
	if !strings.Contains(update.Detail, "could not be read") ||
		!strings.Contains(update.Detail, "recorded fingerprint cannot be checked") {
		t.Errorf("detail = %q, want the fail-closed prose naming the unreadable header "+
			"and the fingerprint it could not check", update.Detail)
	}
	if !strings.Contains(update.Detail, unreadable) {
		t.Errorf("detail = %q, want it to name the recorded source path", update.Detail)
	}
	if update.NewItems != 0 || update.NewTurns != 0 {
		t.Errorf("refused plan carries rows: items=%d turns=%d", update.NewItems, update.NewTurns)
	}
	if _, err := ApplyUpdate(d, update); err == nil {
		t.Fatal("ApplyUpdate accepted a fail-closed plan")
	}

	assertTurnCount(t, d, threadID, 1)
	if got := importedItemCount(t, st, threadID); got != itemsBefore {
		t.Fatalf("items = %d, want the imported %d — a refused refresh wrote rows", got, itemsBefore)
	}
	// Nothing was learned about the file, so nothing about it may be
	// rewritten: the fingerprint the next readable refresh compares against
	// and the cursor it resumes from both have to survive verbatim.
	after, found, err := st.GetThreadImportState(threadID)
	if err != nil || !found {
		t.Fatalf("import state after: found=%v err=%v", found, err)
	}
	want := before
	want.SourcePath = unreadable
	if after != want {
		t.Fatalf("state row changed under a fail-closed refresh:\n before %+v\n  after %+v", want, after)
	}
}

func importedItemCount(t *testing.T, st *store.Store, threadID string) int {
	t.Helper()
	window, err := st.ListRecentItems(threadID, 0)
	if err != nil {
		t.Fatalf("ListRecentItems: %v", err)
	}
	return len(window.Items)
}

// ------------------------------------------------- codex one-handle refresh
//
// codexTail opens the rollout ONCE and serves the identity fingerprint and
// the tail parse from that handle, because Codex publishes a migrated rollout
// by renaming over the same path: two independent opens can straddle that
// rename, prove the identity of the file that used to be there, and then
// splice the replacement's bytes onto the thread at offsets that address
// different records.
//
// The swap cannot be interposed BETWEEN codexTail's two reads: there is no
// hook, no injected filesystem, and nothing between them yields (the identity
// read, the fingerprint comparison, and Parse run back to back on one
// goroutine). A rename racing a goroutine would land in an unknown window and
// pass either way, so this pins the property at the API boundary codexTail
// depends on instead — the two calls it makes, given the one handle it holds,
// answer about the fd and never re-resolve the path. Cross-checking that
// contract is the only thing that makes "one handle" mean anything; that
// refresh.go passes `file` to both is a two-line read of codexTail.
const (
	// The original header, byte for byte, and the sha256 of exactly these
	// bytes as a fixture literal — not recomputed here the way
	// ReadSourceIdentityAt computes it.
	oneHandleOriginalHeader = `{"timestamp":"2026-08-07T15:07:44.000Z","type":"session_meta",` +
		`"payload":{"id":"33333333-3333-4333-8333-333333333333","cwd":"/fixture/repo",` +
		`"originator":"codex_cli","cli_version":"0.149.0","history_mode":"legacy"}}`
	oneHandleOriginalHeaderSHA = "b0bd9bc7f9261b8a21b23e1a0cec560b08316ad5979731d837535b8a1ad44b1b"
)

func TestCodexRefreshReadsIdentityAndTailFromTheHeldHandleNotThePath(t *testing.T) {
	homes := newProviderHomes(t)
	path := homes.codexRolloutPath(codexThreadA)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir codex sessions: %v", err)
	}

	turnStart := `{"timestamp":"2026-08-07T15:07:45.000Z","type":"event_msg",` +
		`"payload":{"type":"task_started","turn_id":"turn-1"}}`
	originalTail := `{"timestamp":"2026-08-07T15:07:46.000Z","type":"event_msg",` +
		`"payload":{"type":"user_message","message":"original tail"}}`
	original := oneHandleOriginalHeader + "\n" + turnStart + "\n" + originalTail + "\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatalf("write original rollout: %v", err)
	}
	// The cursor the import would have recorded: one byte past the newline
	// that ends the header line.
	offset := int64(len(oneHandleOriginalHeader) + 1)

	// The handle the refresh holds. Opened exactly as codexTail opens it,
	// and before anything replaces the path.
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open rollout: %v", err)
	}
	t.Cleanup(func() { _ = file.Close() })

	// The rewrite Codex publishes: a paginated header of its own, different
	// content, atomically renamed over the same path. A paginated file
	// persists its conversation as `response_item` records rather than the
	// legacy `user_message` events the original carries — the rewrite the
	// migration performs, and why an offset into it addresses something else
	// entirely.
	replacementTail := `{"timestamp":"2026-08-07T15:09:00.000Z","type":"response_item",` +
		`"payload":{"type":"message","id":"m1","role":"user",` +
		`"content":[{"type":"input_text","text":"replacement tail"}]}}`
	// The replacement header is padded so the recorded offset still lands one
	// byte past a newline in the REPLACEMENT too: otherwise the
	// record-boundary check would refuse it and the test would pass without
	// ever proving which file was read. The filler then makes the file at
	// least as large as the original, which is the blind spot of the
	// append-only size test.
	headerPrefix := `{"timestamp":"2026-08-07T15:08:00.000Z","type":"session_meta",` +
		`"payload":{"id":"33333333-3333-4333-8333-333333333333","history_mode":"paginated","pad":"`
	const headerSuffix = `"}}`
	padding := int(offset) - 1 - len(headerPrefix) - len(headerSuffix)
	if padding < 0 {
		t.Fatalf("replacement header does not fit the recorded offset %d", offset)
	}
	filler := `{"timestamp":"2026-08-07T15:09:01.000Z","type":"world_state",` +
		`"payload":{"pad":"` + strings.Repeat("f", len(original)) + `"}}`
	replacement := headerPrefix + strings.Repeat("p", padding) + headerSuffix + "\n" +
		turnStart + "\n" + replacementTail + "\n" + filler + "\n"
	staged := filepath.Join(filepath.Dir(path), "staged.jsonl")
	if err := os.WriteFile(staged, []byte(replacement), 0o644); err != nil {
		t.Fatalf("write replacement rollout: %v", err)
	}
	if err := os.Rename(staged, path); err != nil {
		t.Fatalf("publish replacement over the rollout path: %v", err)
	}
	if len(replacement) < len(original) {
		t.Fatalf("replacement is smaller (%d < %d), so the size test would have caught it",
			len(replacement), len(original))
	}

	// What the HANDLE says. Both answers must describe the file that was
	// opened, header and tail alike.
	identity, err := rollout.ReadSourceIdentityAt(file, path, codexThreadA)
	if err != nil {
		t.Fatalf("ReadSourceIdentityAt on the held handle: %v", err)
	}
	if identity.MetaHash != oneHandleOriginalHeaderSHA {
		t.Fatalf("MetaHash = %q, want the original header's %q — the identity read followed the path",
			identity.MetaHash, oneHandleOriginalHeaderSHA)
	}
	if identity.HistoryMode != "legacy" {
		t.Fatalf("HistoryMode = %q, want legacy", identity.HistoryMode)
	}
	parsed, err := rollout.Parse(context.Background(), rollout.ParseOptions{
		File:       file,
		Path:       path,
		SessionID:  codexThreadA,
		FromOffset: offset,
	})
	if err != nil {
		t.Fatalf("Parse on the held handle: %v", err)
	}
	if got := onlyUserText(t, parsed.Events); got != "original tail" {
		t.Fatalf("parsed %q, want the original tail — Parse re-opened the path", got)
	}

	// And the discrimination is real: the same two calls against the PATH
	// answer about the replacement, so a refresh that opened twice would
	// have spliced these bytes onto the thread.
	fresh, err := os.Open(path)
	if err != nil {
		t.Fatalf("re-open rollout path: %v", err)
	}
	defer fresh.Close()
	freshIdentity, err := rollout.ReadSourceIdentityAt(fresh, path, codexThreadA)
	if err != nil {
		t.Fatalf("ReadSourceIdentityAt on a fresh handle: %v", err)
	}
	if freshIdentity.MetaHash == oneHandleOriginalHeaderSHA || freshIdentity.HistoryMode != "paginated" {
		t.Fatalf("fixture did not actually replace the file: identity = %+v", freshIdentity)
	}
	freshParse, err := rollout.Parse(context.Background(), rollout.ParseOptions{
		Path:       path,
		SessionID:  codexThreadA,
		FromOffset: offset,
	})
	if err != nil {
		t.Fatalf("Parse over the path: %v", err)
	}
	if got := onlyUserText(t, freshParse.Events); got != "replacement tail" {
		t.Fatalf("path parse read %q, want the replacement tail", got)
	}
}

// onlyUserText returns the content of the single user_text event in a parse,
// failing when there is not exactly one.
func onlyUserText(t *testing.T, events []importir.Event) string {
	t.Helper()
	var found []string
	for _, evt := range events {
		if evt.Kind == provider.EventUserText {
			found = append(found, evt.Content)
		}
	}
	if len(found) != 1 {
		t.Fatalf("user_text events = %v, want exactly one", found)
	}
	return found[0]
}

// ------------------------------------------- the empty CURRENT fingerprint
//
// ReadSourceIdentityAt answers a NIL ERROR with an empty MetaHash whenever the
// file has no complete, in-window first line: a first record longer than the
// bounded head read, a truncated head, an empty file. That is a different
// failure from the read error TestRefreshFailsClosedWhenAFingerprintedCodexHeaderCannotBeRead
// pins — nothing errored, so a guard written as "compare only when both sides
// are non-empty" silently skips the comparison and trusts the recorded byte
// offset against a file whose header it never read.

func TestCodexSourceIdentityFailsClosedOnAnEmptyCurrentFingerprint(t *testing.T) {
	state := store.ThreadImportState{
		SourcePath:        "/codex/sessions/rollout.jsonl",
		SourceMetaHash:    strings.Repeat("a", 64),
		SourceHistoryMode: "legacy",
		LastSourceOffset:  4096,
	}
	err := codexSourceIdentityMatches(state, rollout.SourceIdentity{})
	if err == nil {
		t.Fatal("an unfingerprintable current header was accepted as 'nothing to compare'")
	}
	if !strings.Contains(err.Error(), "can no longer be read as this thread's header") {
		t.Fatalf("detail = %q, want prose naming the unreadable first record", err.Error())
	}
	if !strings.Contains(err.Error(), state.SourcePath) {
		t.Fatalf("detail = %q, want it to name the source path", err.Error())
	}
}

// The two directions that must NOT fail closed, so the guard above cannot be
// satisfied by refusing everything.
func TestCodexSourceIdentityTreatsAnUnknownRecordedFingerprintAsUnknown(t *testing.T) {
	// Pre-v67 row: no recorded fingerprint, so there is nothing to compare
	// and the size test is what it has always had.
	pre := store.ThreadImportState{SourcePath: "/x.jsonl", LastSourceOffset: 4096}
	if err := codexSourceIdentityMatches(pre, rollout.SourceIdentity{}); err != nil {
		t.Fatalf("unfingerprinted row refused: %v", err)
	}
	// Never resumed from: no position exists that a rewritten header could
	// invalidate.
	fresh := store.ThreadImportState{SourcePath: "/x.jsonl", SourceMetaHash: strings.Repeat("b", 64)}
	if err := codexSourceIdentityMatches(fresh, rollout.SourceIdentity{}); err != nil {
		t.Fatalf("offset-less row refused: %v", err)
	}
}

// An ABSENT history mode is `legacy` (the field exists only from Codex 0.147
// and its enum defaults to Legacy), so the two spellings are one mode. Reading
// them as a migration would report "migrated from legacy to legacy" — prose
// that describes nothing — over a file whose real problem, if it has one, the
// fingerprint states accurately.
func TestCodexSourceIdentityReadsAnAbsentHistoryModeAsLegacy(t *testing.T) {
	hash := strings.Repeat("c", 64)
	state := store.ThreadImportState{
		SourcePath:       "/x.jsonl",
		SourceMetaHash:   hash,
		LastSourceOffset: 4096,
	}
	identity := rollout.SourceIdentity{MetaHash: hash, HistoryMode: "legacy"}
	if err := codexSourceIdentityMatches(state, identity); err != nil {
		t.Fatalf("absent-vs-legacy reported as a migration: %v", err)
	}
	// And the real migration still is one.
	identity.HistoryMode = "paginated"
	err := codexSourceIdentityMatches(state, identity)
	if err == nil || !strings.Contains(err.Error(), "migrated") {
		t.Fatalf("legacy -> paginated not refused as a migration: %v", err)
	}
}

// End to end, through PlanUpdate, on the shape that produces a nil-error empty
// fingerprint in the wild: a rollout whose FIRST record is larger than the
// bounded head read, standing where the imported file used to be.
//
// The recorded resume offset is placed on a record boundary of the
// REPLACEMENT, because that is the only arrangement in which the hole is
// reachable at all — a cursor that misses a boundary is already refused by
// the append-only test, and one past EOF by the size test. With the offset
// landing cleanly and the file larger than the original, the fingerprint is
// the only thing between the thread and a tail spliced out of an unrelated
// file.
func TestRefreshRefusesACodexRolloutWhoseFirstRecordOutgrewTheHeadRead(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.writeCodexIndex(t, codexThreadA)
	path := homes.codexLinearSession(t, codexThreadA)
	d := homes.deps(st)
	threadID := importOneCodexFixture(t, d)
	assertTurnCount(t, d, threadID, 1)
	itemsBefore := importedItemCount(t, st, threadID)

	before, found, err := st.GetThreadImportState(threadID)
	if err != nil || !found {
		t.Fatalf("import state: found=%v err=%v", found, err)
	}
	if before.SourceMetaHash == "" {
		t.Fatal("import recorded no source fingerprint, so the guard is not armed")
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read original rollout: %v", err)
	}

	// headScanBytes in the rollout reader. A first record past it means the
	// head read reaches the end of its window without a newline, which is an
	// unterminated line: EOF, no error, and no fingerprint.
	const headWindow = 4 << 20
	headPrefix := `{"timestamp":"2026-08-07T15:08:00.000Z","type":"session_meta",` +
		`"payload":{"id":"` + codexThreadA + `","history_mode":"legacy","pad":"`
	const headSuffix = `"}}`
	head := headPrefix + strings.Repeat("p", headWindow+4096-len(headPrefix)-len(headSuffix)) + headSuffix
	tail := `{"timestamp":"2026-08-07T15:09:00.000Z","type":"event_msg",` +
		`"payload":{"type":"user_message","message":"spliced from an unrelated rollout"}}`
	body := head + "\n" + tail + "\n"
	staged := filepath.Join(filepath.Dir(path), "staged.jsonl")
	if err := os.WriteFile(staged, []byte(body), 0o644); err != nil {
		t.Fatalf("write replacement rollout: %v", err)
	}
	if err := os.Rename(staged, path); err != nil {
		t.Fatalf("publish replacement: %v", err)
	}
	if len(body) < len(original) {
		t.Fatalf("replacement is smaller (%d < %d), so the size test would catch it",
			len(body), len(original))
	}

	// The cursor: one byte past the head record's newline, so the tail parse
	// would read a whole, well-formed record if the guard let it through.
	before.LastSourceOffset = int64(len(head) + 1)
	if err := st.SetThreadImportState(before); err != nil {
		t.Fatalf("SetThreadImportState: %v", err)
	}

	// Proof the fixture reproduces THIS hole and not one of the neighbouring
	// refusals: the identity read succeeds and reports nothing.
	fresh, err := os.Open(path)
	if err != nil {
		t.Fatalf("open replacement: %v", err)
	}
	identity, identityErr := rollout.ReadSourceIdentityAt(fresh, path, codexThreadA)
	_ = fresh.Close()
	if identityErr != nil {
		t.Fatalf("ReadSourceIdentityAt errored (%v) — that is the read-error path, not this one", identityErr)
	}
	if identity.MetaHash != "" {
		t.Fatalf("MetaHash = %q, want empty — the fixture's first record fits the head read", identity.MetaHash)
	}

	update := planCodexRefresh(t, d, threadID)
	if update.Status != UpdateSourceDiverged {
		t.Fatalf("status = %q (%s), want source-diverged", update.Status, update.Detail)
	}
	if update.NewItems != 0 || update.NewTurns != 0 {
		t.Errorf("refused plan carries rows: items=%d turns=%d", update.NewItems, update.NewTurns)
	}
	if _, err := ApplyUpdate(d, update); err == nil {
		t.Fatal("ApplyUpdate accepted a refused plan")
	}
	assertTurnCount(t, d, threadID, 1)
	if got := importedItemCount(t, st, threadID); got != itemsBefore {
		t.Fatalf("items = %d, want the imported %d — the replacement's tail was spliced in", got, itemsBefore)
	}
}
