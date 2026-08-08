package sessionimport

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/store"
)

func scanFixture(t *testing.T, d Deps, filter Filter) ScanResult {
	t.Helper()
	result, err := Scan(context.Background(), d, filter)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return result
}

func rowByID(t *testing.T, result ScanResult, id string) Row {
	t.Helper()
	for _, row := range result.Rows {
		if row.ID == id {
			return row
		}
	}
	t.Fatalf("no row %s in scan (%d rows: %s)", id, len(result.Rows), rowIDs(result))
	return Row{}
}

func rowIDs(result ScanResult) string {
	ids := make([]string, 0, len(result.Rows))
	for _, row := range result.Rows {
		ids = append(ids, row.ID)
	}
	return strings.Join(ids, ", ")
}

func statusFor(t *testing.T, result ScanResult, providerName string) ProviderStatus {
	t.Helper()
	for _, status := range result.Providers {
		if status.Provider == providerName {
			return status
		}
	}
	t.Fatalf("no %s provider status in scan", providerName)
	return ProviderStatus{}
}

func TestScanListsBothProviders(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.claudeLinearSession(t, claudeSessionA)
	homes.writeCodexIndex(t, codexThreadA)
	homes.codexLinearSession(t, codexThreadA)

	result := scanFixture(t, homes.deps(st), Filter{})

	if len(result.Providers) != 2 {
		t.Fatalf("providers = %+v, want one entry per provider", result.Providers)
	}
	for _, status := range result.Providers {
		if !status.Available || status.Error != "" {
			t.Errorf("provider %s = %+v, want available", status.Provider, status)
		}
	}
	if len(result.Rows) != 2 {
		t.Fatalf("rows = %s, want one per provider", rowIDs(result))
	}

	claude := rowByID(t, result, RowKey(ProviderClaude, claudeSessionA))
	if claude.Title != "add a test" || claude.ProjectPath != homes.workspace {
		t.Errorf("claude row = %+v", claude)
	}
	if claude.SourcePath == "" || claude.SizeBytes == 0 {
		t.Errorf("claude row source = %q/%d", claude.SourcePath, claude.SizeBytes)
	}
	// Claude branch enumeration needs a full transcript read, so listing
	// reports 0 = "not determined" rather than a number it did not count.
	if claude.BranchCount != 0 {
		t.Errorf("claude BranchCount = %d, want 0 (not determined at list time)", claude.BranchCount)
	}

	codex := rowByID(t, result, RowKey(ProviderCodex, codexThreadA))
	if codex.BranchCount != 1 {
		t.Errorf("codex BranchCount = %d, want 1 (a rollout is one conversation)", codex.BranchCount)
	}
	if codex.ProjectPath != homes.workspace {
		t.Errorf("codex ProjectPath = %q, want %q", codex.ProjectPath, homes.workspace)
	}
}

// TestScanDedupsAgainstEveryKnownRef is the property that makes "Import
// All" safe to press twice, and the middle case is the one a session_ref
// check alone would miss.
func TestScanDedupsAgainstEveryKnownRef(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.claudeLinearSession(t, claudeSessionA)
	homes.claudeLinearSession(t, claudeSessionB)
	homes.writeCodexIndex(t, codexThreadA, codexThreadB)
	homes.codexLinearSession(t, codexThreadA)
	homes.codexLinearSession(t, codexThreadB)

	seedProject(t, st, homes.workspace)

	// A live session claims Claude A.
	live := seedThread(t, st, "thread-live", "claude", homes.workspace)
	if _, err := st.UpdateSessionRef(live.ID, claudeSessionA); err != nil {
		t.Fatalf("update session ref: %v", err)
	}
	// A fork that has never been resumed claims Codex A: a session file on
	// disk and no session_ref at all.
	if err := st.CreateThread(store.Thread{
		ID:             "thread-fork",
		ProjectID:      testProjectID,
		Title:          "Unresumed fork",
		Provider:       "codex",
		WorkspacePath:  homes.workspace,
		PendingForkRef: codexThreadA,
		CreatedAt:      baseMillis,
		UpdatedAt:      baseMillis,
	}); err != nil {
		t.Fatalf("create fork thread: %v", err)
	}
	// An earlier import claims Claude B.
	imported := seedThread(t, st, "thread-imported", "claude", homes.workspace)
	if err := st.SetThreadImportState(store.ThreadImportState{
		ThreadID:        imported.ID,
		Provider:        "claude",
		SourcePath:      "/gone",
		SourceSessionID: claudeSessionB,
		LastTurnIndex:   -1,
		LastItemIndex:   -1,
		ImportedAt:      baseMillis,
	}); err != nil {
		t.Fatalf("set import state: %v", err)
	}

	result := scanFixture(t, homes.deps(st), Filter{})
	if len(result.Rows) != 1 || result.Rows[0].SessionID != codexThreadB {
		t.Fatalf("rows = %s, want only codex B", rowIDs(result))
	}
}

func TestScanExcludesForkAncestors(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)

	// Claude: B was forked from A, so A's history lives inside B.
	homes.claudeLinearSession(t, claudeSessionA)
	homes.writeClaudeSession(t, claudeSessionB,
		map[string]any{
			"type": "file-history-snapshot", "messageId": "m", "isSnapshotUpdate": false,
			"forkedFrom": map[string]any{"sessionId": claudeSessionA},
		},
		homes.claudeUserRow("bu1", "", "continue from the fork", 0),
		homes.claudeAssistantRow("ba1", "bu1", "msg-b1", []any{claudeTextBlock("Sure.")}, 1_000, nil),
		claudeLastPromptRow("ba1", "continue from the fork"),
	)

	// Codex: B declares A as its source in its own session_meta.
	homes.writeCodexIndex(t, codexThreadA, codexThreadB)
	homes.codexLinearSession(t, codexThreadA)
	homes.writeCodexRollout(t, codexThreadB,
		homes.codexMetaLine(codexThreadA, ""), // the source's meta, embedded by the fork
		homes.codexMetaLine(codexThreadB, codexThreadA),
		codexLine(100, "event_msg", map[string]any{"type": "task_started", "turn_id": "turn-1"}),
		codexLine(200, "event_msg", map[string]any{"type": "user_message", "message": "continue"}),
		codexLine(300, "event_msg", map[string]any{"type": "task_complete", "turn_id": "turn-1"}),
	)

	result := scanFixture(t, homes.deps(st), Filter{})
	for _, row := range result.Rows {
		if row.SessionID == claudeSessionA || row.SessionID == codexThreadA {
			t.Errorf("scan offered fork ancestor %s", row.ID)
		}
	}
	if len(result.Rows) != 2 {
		t.Fatalf("rows = %s, want the two forks only", rowIDs(result))
	}
}

// TestScanSurvivesOneBrokenProvider pins the isolation the contract
// promises: a Codex home that cannot be read is a provider-level error and
// Claude's sessions still list.
func TestScanSurvivesOneBrokenProvider(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.claudeLinearSession(t, claudeSessionA)
	// No state_5.sqlite written at all.

	result := scanFixture(t, homes.deps(st), Filter{})

	codex := statusFor(t, result, ProviderCodex)
	if codex.Available || codex.Error == "" {
		t.Errorf("codex status = %+v, want unavailable with prose", codex)
	}
	claude := statusFor(t, result, ProviderClaude)
	if !claude.Available {
		t.Errorf("claude status = %+v, want available", claude)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("rows = %s, want the claude session", rowIDs(result))
	}
}

func TestScanReportsAnAbsentProviderHomeAsUnavailable(t *testing.T) {
	st := newTestStore(t)
	result := scanFixture(t, Deps{Store: st}, Filter{})
	for _, status := range result.Providers {
		if status.Available || status.Error == "" {
			t.Errorf("provider %s = %+v, want unavailable with prose", status.Provider, status)
		}
	}
}

func TestScanWarnsWhenTheWorkspaceIsGone(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.claudeLinearSession(t, claudeSessionA)
	if err := os.RemoveAll(homes.workspace); err != nil {
		t.Fatalf("remove workspace: %v", err)
	}

	result := scanFixture(t, homes.deps(st), Filter{Provider: ProviderClaude})
	row := rowByID(t, result, RowKey(ProviderClaude, claudeSessionA))
	if len(row.Warnings) == 0 || !strings.Contains(row.Warnings[0], "no longer exists") {
		t.Fatalf("warnings = %v, want a missing-workspace warning", row.Warnings)
	}
}

func TestScanLabelsKnownAndUnknownProjects(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.claudeLinearSession(t, claudeSessionA)

	unknown := scanFixture(t, homes.deps(st), Filter{Provider: ProviderClaude})
	row := rowByID(t, unknown, RowKey(ProviderClaude, claudeSessionA))
	if row.KnownProject || row.ProjectID != "" {
		t.Errorf("row = %+v, want no project yet", row)
	}
	if row.ProjectLabel != filepath.Base(homes.workspace) {
		t.Errorf("ProjectLabel = %q, want the workspace base name", row.ProjectLabel)
	}

	project := seedProject(t, st, homes.workspace)
	known := scanFixture(t, homes.deps(st), Filter{Provider: ProviderClaude})
	row = rowByID(t, known, RowKey(ProviderClaude, claudeSessionA))
	if !row.KnownProject || row.ProjectID != project.ID || row.ProjectLabel != project.Name {
		t.Errorf("row = %+v, want the seeded project", row)
	}
}

func TestScanFiltersByProviderAndWorkspace(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.claudeLinearSession(t, claudeSessionA)
	homes.writeCodexIndex(t, codexThreadA)
	homes.codexLinearSession(t, codexThreadA)

	onlyCodex := scanFixture(t, homes.deps(st), Filter{Provider: ProviderCodex})
	if len(onlyCodex.Rows) != 1 || onlyCodex.Rows[0].Provider != ProviderCodex {
		t.Fatalf("rows = %s, want codex only", rowIDs(onlyCodex))
	}
	if len(onlyCodex.Providers) != 1 {
		t.Errorf("providers = %+v, want only the one scanned", onlyCodex.Providers)
	}

	elsewhere := scanFixture(t, homes.deps(st), Filter{WorkspacePath: "/somewhere/else"})
	if len(elsewhere.Rows) != 0 {
		t.Fatalf("rows = %s, want none for a foreign workspace", rowIDs(elsewhere))
	}
}

func TestScanIsCancellable(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.claudeLinearSession(t, claudeSessionA)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Scan(ctx, homes.deps(st), Filter{}); err == nil {
		t.Fatal("Scan on a cancelled context returned no error")
	}
}

// seedProject creates the project row a scan matches a workspace against.
func seedProject(t *testing.T, st *store.Store, path string) store.Project {
	t.Helper()
	project := store.Project{
		ID:        testProjectID,
		Path:      path,
		Name:      "Fixture Repo",
		CreatedAt: baseMillis,
		UpdatedAt: baseMillis,
	}
	if _, err := st.GetProject(testProjectID); err != nil {
		if err := st.CreateProject(project); err != nil {
			t.Fatalf("create project: %v", err)
		}
	}
	return project
}
