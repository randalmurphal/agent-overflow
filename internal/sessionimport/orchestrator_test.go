package sessionimport

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/provider/codex/rollout"
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
	codex := rowByID(t, result, RowKey(ProviderCodex, codexThreadA))
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

func TestScanIncludesEveryExplicitForkAndItsAncestor(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)

	// Claude: B was forked from A and shares its historical prefix, but both
	// remain independently resumable provider sessions.
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

	// Codex: B declares A as its explicit fork source in its own session_meta.
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
	if len(result.Rows) != 4 {
		t.Fatalf("rows = %s, want both parents and both forks", rowIDs(result))
	}
	for _, tc := range []struct {
		provider, child, parent string
	}{
		{ProviderClaude, claudeSessionB, claudeSessionA},
		{ProviderCodex, codexThreadB, codexThreadA},
	} {
		child := rowByID(t, result, RowKey(tc.provider, tc.child))
		if child.ParentSessionID != tc.parent {
			t.Errorf("%s parent = %q, want %q", child.ID, child.ParentSessionID, tc.parent)
		}
		parent := rowByID(t, result, RowKey(tc.provider, tc.parent))
		if parent.ParentSessionID != "" {
			t.Errorf("%s parent = %q, want root", parent.ID, parent.ParentSessionID)
		}
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

// fixtureRepo writes the on-disk shape of a primary checkout: `.git` is a
// directory.
func fixtureRepo(t *testing.T, root string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir %s/.git: %v", root, err)
	}
	return root
}

// fixtureWorktree writes what `git worktree add` leaves on disk: a private
// gitdir under the repository, a `.git` FILE in the worktree pointing at it,
// and the registration pointing back. live=false registers the worktree
// without creating its directory — a worktree the user has since deleted,
// which is the only case the registration is the ONLY way to place.
func fixtureWorktree(t *testing.T, repoRoot, name, worktreePath string, live bool) string {
	t.Helper()
	private := filepath.Join(repoRoot, ".git", "worktrees", name)
	if err := os.MkdirAll(private, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", private, err)
	}
	writeFixtureFile(t, filepath.Join(private, "gitdir"), filepath.Join(worktreePath, ".git")+"\n")
	writeFixtureFile(t, filepath.Join(private, "commondir"), "../..\n")
	if live {
		if err := os.MkdirAll(worktreePath, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", worktreePath, err)
		}
		writeFixtureFile(t, filepath.Join(worktreePath, ".git"), "gitdir: "+private+"\n")
	}
	return worktreePath
}

func writeFixtureFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// A session that ran in a LINKED WORKTREE belongs to the repository the
// worktree was cut from, not to a project of its own. AO parks its worktrees
// under `<configDir>/worktrees/<project>/<branch>`, nowhere near the
// repository, so path containment alone cannot group them — this is what
// keeps a worktree session from listing (and importing) as a junk project
// named after the branch.
func TestScanGroupsWorktreeSessionsUnderTheirRepository(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	repo := fixtureRepo(t, homes.workspace)
	worktree := fixtureWorktree(t, repo, "feature",
		filepath.Join(filepath.Dir(repo), "worktrees", "fixture-repo", "BLITZ-188"), true)

	homes.claudeShortSession(t, claudeSessionA, map[string]any{"cwd": worktree})
	project := seedProject(t, st, repo)

	result := scanFixture(t, homes.deps(st), Filter{Provider: ProviderClaude})
	row := rowByID(t, result, RowKey(ProviderClaude, claudeSessionA))
	if !row.KnownProject || row.ProjectID != project.ID || row.ProjectLabel != project.Name {
		t.Fatalf("row = %+v, want the repository's project (%s / %s)", row, project.ID, project.Name)
	}
	// The workspace itself is untouched: the thread still runs in the
	// worktree, only the PROJECT resolves to the repository.
	if row.ProjectPath != worktree {
		t.Fatalf("ProjectPath = %q, want the worktree cwd %q", row.ProjectPath, worktree)
	}
}

// The listing and the import must agree: a worktree session imports into the
// REPOSITORY's project, with its own worktree cwd preserved as the thread's
// workspace. Importing it into a project of its own is what produced junk
// projects named after a branch.
func TestImportOneLandsAWorktreeSessionInTheRepositoryProject(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	repo := fixtureRepo(t, homes.workspace)
	worktree := fixtureWorktree(t, repo, "feature",
		filepath.Join(filepath.Dir(repo), "worktrees", "fixture-repo", "BLITZ-188"), true)
	homes.claudeShortSession(t, claudeSessionA, map[string]any{"cwd": worktree})

	deps := homes.deps(st)
	result := scanFixture(t, deps, Filter{Provider: ProviderClaude})
	outcome := importFixtureRow(t, deps, rowByID(t, result, RowKey(ProviderClaude, claudeSessionA)))
	if len(outcome.Threads) != 1 {
		t.Fatalf("threads = %d, want one", len(outcome.Threads))
	}
	thread := outcome.Threads[0]

	projects, err := st.ListProjects()
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("projects = %+v, want exactly one (the repository)", projects)
	}
	if filepath.Base(projects[0].Path) != filepath.Base(repo) || projects[0].ID != thread.ProjectID {
		t.Fatalf("project = %+v, want the repository %q holding thread %s", projects[0], repo, thread.ID)
	}
	if thread.WorkspacePath != worktree {
		t.Fatalf("thread WorkspacePath = %q, want the worktree cwd %q", thread.WorkspacePath, worktree)
	}
}

// A worktree the user deleted leaves nothing on disk to walk up from, so the
// grouping has to come from the repository side: the registration git still
// holds. The row keeps its missing-workspace warning either way.
func TestScanGroupsDeletedWorktreeSessionsThroughTheirRegistration(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	repo := fixtureRepo(t, homes.workspace)
	gone := fixtureWorktree(t, repo, "gone",
		filepath.Join(filepath.Dir(repo), "worktrees", "fixture-repo", "removed"), false)

	// A cwd INSIDE the deleted worktree, which is what a session started
	// from a subdirectory records.
	cwd := filepath.Join(gone, "internal", "store")
	homes.claudeShortSession(t, claudeSessionA, map[string]any{"cwd": cwd})
	project := seedProject(t, st, repo)

	result := scanFixture(t, homes.deps(st), Filter{Provider: ProviderClaude})
	row := rowByID(t, result, RowKey(ProviderClaude, claudeSessionA))
	if !row.KnownProject || row.ProjectID != project.ID {
		t.Fatalf("row = %+v, want the registering project %s", row, project.ID)
	}
	if len(row.Warnings) == 0 || !strings.Contains(row.Warnings[0], "no longer exists") {
		t.Fatalf("warnings = %v, want the missing-workspace warning kept", row.Warnings)
	}
}

// A project row sitting ON a worktree path is one the user has been working
// in, so it wins over the repository the worktree was cut from — the most
// specific answer, not the resolved one. Probing the repository root first
// makes the worktree row unreachable in exactly the case it exists for, and
// every session in that worktree silently moves to the other project.
//
// It also pins the registration fold: the repository REGISTERS this worktree,
// and that registration must not displace the real project row on it.
func TestScanPrefersAProjectRowOnTheWorktreeItself(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	repo := fixtureRepo(t, homes.workspace)
	worktree := fixtureWorktree(t, repo, "feature",
		filepath.Join(filepath.Dir(repo), "worktrees", "fixture-repo", "BLITZ-188"), true)

	homes.claudeShortSession(t, claudeSessionA, map[string]any{"cwd": worktree})
	repository := seedProject(t, st, repo)
	branch := seedProjectAt(t, st, "project-worktree", "BLITZ-188", worktree)

	result := scanFixture(t, homes.deps(st), Filter{Provider: ProviderClaude})
	row := rowByID(t, result, RowKey(ProviderClaude, claudeSessionA))
	if !row.KnownProject || row.ProjectID != branch.ID || row.ProjectLabel != branch.Name {
		t.Fatalf("row = %+v, want the worktree's own project (%s / %s), not the repository's %s",
			row, branch.ID, branch.Name, repository.ID)
	}
}

// With no project row yet, the label is what the import will name the project
// it creates — so it has to be the REPOSITORY's name, not the worktree
// directory's (which is a branch name).
func TestScanLabelsAnUnknownWorktreeSessionByItsRepository(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	repo := fixtureRepo(t, homes.workspace)
	worktree := fixtureWorktree(t, repo, "feature",
		filepath.Join(filepath.Dir(repo), "worktrees", "fixture-repo", "BLITZ-188"), true)

	homes.claudeShortSession(t, claudeSessionA, map[string]any{"cwd": worktree})

	result := scanFixture(t, homes.deps(st), Filter{Provider: ProviderClaude})
	row := rowByID(t, result, RowKey(ProviderClaude, claudeSessionA))
	if row.KnownProject {
		t.Fatalf("row = %+v, want no project yet", row)
	}
	if row.ProjectLabel != filepath.Base(repo) {
		t.Fatalf("ProjectLabel = %q, want the repository name %q", row.ProjectLabel, filepath.Base(repo))
	}
}

// TestScanReportsTheProviderOriginMarker pins both halves of the "already ran
// in Agent Overflow" signal: the raw marker passes through verbatim, and the
// per-provider equality that turns it into a boolean stays in the backend —
// including the cross-spelling cases, which are what a single shared constant
// would get wrong.
func TestScanReportsTheProviderOriginMarker(t *testing.T) {
	// The same marker string is stamped on BOTH providers' fixtures, so
	// every case also asserts the provider that must NOT claim it.
	cases := []struct {
		name         string
		marker       string
		claudeIsOurs bool
		codexIsOurs  bool
	}{
		{name: "no marker at all"},
		{name: "claude spelling", marker: "agent-overflow", claudeIsOurs: true},
		{name: "codex spelling", marker: "agent_overflow", codexIsOurs: true},
		{name: "another client", marker: "cli"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t)
			homes := newProviderHomes(t)

			claudeFields := map[string]any{}
			if tc.marker != "" {
				claudeFields["entrypoint"] = tc.marker
			}
			homes.claudeShortSession(t, claudeSessionA, claudeFields)

			homes.writeCodexIndex(t, codexThreadA)
			homes.writeCodexRollout(t, codexThreadA,
				homes.codexMetaLineFrom(codexThreadA, "", tc.marker),
				codexLine(100, "event_msg", map[string]any{"type": "task_started", "turn_id": "turn-1"}),
				codexLine(200, "event_msg", map[string]any{"type": "user_message", "message": "add a test"}),
				codexLine(300, "event_msg", map[string]any{"type": "task_complete", "turn_id": "turn-1"}),
			)

			result := scanFixture(t, homes.deps(st), Filter{})
			claude := rowByID(t, result, RowKey(ProviderClaude, claudeSessionA))
			codex := rowByID(t, result, RowKey(ProviderCodex, codexThreadA))

			for _, row := range []Row{claude, codex} {
				if row.Origin != tc.marker {
					t.Errorf("%s Origin = %q, want the marker verbatim %q", row.Provider, row.Origin, tc.marker)
				}
			}
			if claude.RanInAgentOverflow != tc.claudeIsOurs {
				t.Errorf("claude RanInAgentOverflow = %v for %q, want %v",
					claude.RanInAgentOverflow, tc.marker, tc.claudeIsOurs)
			}
			if codex.RanInAgentOverflow != tc.codexIsOurs {
				t.Errorf("codex RanInAgentOverflow = %v for %q, want %v",
					codex.RanInAgentOverflow, tc.marker, tc.codexIsOurs)
			}
		})
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
	return seedProjectAt(t, st, testProjectID, "Fixture Repo", path)
}

// seedProjectAt creates one project row, or returns the existing row under
// that id. Callers naming their own id are the ones that need TWO projects at
// once — a repository and a row sitting on one of its worktrees.
func seedProjectAt(t *testing.T, st *store.Store, id, name, path string) store.Project {
	t.Helper()
	project := store.Project{
		ID:        id,
		Path:      path,
		Name:      name,
		CreatedAt: baseMillis,
		UpdatedAt: baseMillis,
	}
	if _, err := st.GetProject(id); err != nil {
		if err := st.CreateProject(project); err != nil {
			t.Fatalf("create project: %v", err)
		}
	}
	return project
}

// ------------------------------------------- Codex's external import ledger

// writeCodexImportLedger writes the file Codex appends to when IT imports a
// session from another coding agent, hand-written against
// `ImportedExternalAgentSessionRecord` at codex tag rust-v0.149.0.
func (h providerHomes) writeCodexImportLedger(t *testing.T, records ...map[string]any) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"records": records})
	if err != nil {
		t.Fatalf("marshal ledger: %v", err)
	}
	path := filepath.Join(h.codexHome, "external_agent_session_imports.json")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write ledger: %v", err)
	}
}

func (h providerHomes) claudeLedgerRecord(threadID, claudeSessionID string) map[string]any {
	return map[string]any{
		"source_path": filepath.Join(
			h.claudeProjects, "-fixture-repo", claudeSessionID+".jsonl"),
		"content_sha256":     "0f0f0f",
		"imported_thread_id": threadID,
		"imported_at":        baseMillis / 1000,
		"connector_names":    []string{},
		"title":              "Imported conversation",
	}
}

// The regression this exists for: an imported Claude Code conversation lands
// as an ordinary Codex rollout whose originator says `codex_cli`, so without
// the ledger the picker offers it with nothing to say where it came from.
func TestScanLabelsCodexSessionsCodexImportedFromAnotherAgent(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.writeCodexIndex(t, codexThreadA, codexThreadB)
	homes.codexLinearSession(t, codexThreadA)
	homes.codexLinearSession(t, codexThreadB)
	homes.writeCodexImportLedger(t, homes.claudeLedgerRecord(codexThreadA, claudeSessionA))

	result := scanFixture(t, homes.deps(st), Filter{Provider: ProviderCodex})

	imported := rowByID(t, result, RowKey(ProviderCodex, codexThreadA))
	if imported.ImportedFrom == nil {
		t.Fatalf("codex A should carry its external origin")
	}
	if imported.ImportedFrom.Agent != rollout.ExternalImportAgentClaude {
		t.Errorf("agent = %q", imported.ImportedFrom.Agent)
	}
	if imported.ImportedFrom.SourceSessionID != claudeSessionA {
		t.Errorf("source session id = %q, want %q",
			imported.ImportedFrom.SourceSessionID, claudeSessionA)
	}
	if imported.ImportedFrom.ImportedAt != (baseMillis/1000)*1000 {
		t.Errorf("importedAt = %d", imported.ImportedFrom.ImportedAt)
	}
	// AO does not hold the source conversation, so there is no duplicate to
	// name — the absence has to be explicit, or the badge lies.
	if imported.ImportedFrom.DuplicateOfThreadID != "" {
		t.Errorf("duplicateOfThreadId = %q, want empty",
			imported.ImportedFrom.DuplicateOfThreadID)
	}
	// A session the ledger says nothing about carries nothing.
	if plain := rowByID(t, result, RowKey(ProviderCodex, codexThreadB)); plain.ImportedFrom != nil {
		t.Errorf("codex B should carry no origin: %+v", plain.ImportedFrom)
	}
}

// The second thing the ledger buys: AO already holds this conversation,
// imported straight from ~/.claude. The row is still OFFERED — both provider
// sessions exist and both resume — but the user must be told.
func TestScanMarksACodexImportAsADuplicateOfAnAlreadyImportedClaudeSession(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.writeCodexIndex(t, codexThreadA)
	homes.codexLinearSession(t, codexThreadA)
	homes.writeCodexImportLedger(t, homes.claudeLedgerRecord(codexThreadA, claudeSessionA))

	seedProject(t, st, homes.workspace)
	imported := seedThread(t, st, "thread-claude-a", "claude", homes.workspace)
	if err := st.SetThreadImportState(store.ThreadImportState{
		ThreadID:        imported.ID,
		Provider:        "claude",
		SourcePath:      "/gone",
		SourceSessionID: claudeSessionA,
		LastTurnIndex:   -1,
		LastItemIndex:   -1,
		ImportedAt:      baseMillis,
	}); err != nil {
		t.Fatalf("set import state: %v", err)
	}

	result := scanFixture(t, homes.deps(st), Filter{Provider: ProviderCodex})

	row := rowByID(t, result, RowKey(ProviderCodex, codexThreadA))
	if row.ImportedFrom == nil {
		t.Fatalf("codex A should carry its external origin")
	}
	if row.ImportedFrom.DuplicateOfThreadID != imported.ID {
		t.Errorf("duplicateOfThreadId = %q, want %q",
			row.ImportedFrom.DuplicateOfThreadID, imported.ID)
	}
}

// A ledger that cannot be read costs the labels and nothing else: the badge
// is decoration on a listing that must still list.
func TestScanStillListsWhenTheImportLedgerIsCorrupt(t *testing.T) {
	st := newTestStore(t)
	homes := newProviderHomes(t)
	homes.writeCodexIndex(t, codexThreadA)
	homes.codexLinearSession(t, codexThreadA)
	if err := os.WriteFile(
		filepath.Join(homes.codexHome, "external_agent_session_imports.json"),
		[]byte("{not json"), 0o644,
	); err != nil {
		t.Fatalf("write corrupt ledger: %v", err)
	}

	result := scanFixture(t, homes.deps(st), Filter{Provider: ProviderCodex})

	if status := statusFor(t, result, ProviderCodex); !status.Available {
		t.Fatalf("codex should still be available: %+v", status)
	}
	row := rowByID(t, result, RowKey(ProviderCodex, codexThreadA))
	if row.ImportedFrom != nil {
		t.Errorf("no labels from a corrupt ledger: %+v", row.ImportedFrom)
	}
	// The ledger warning is not a per-file skip and must not be counted as one.
	if status := statusFor(t, result, ProviderCodex); status.SkippedCount != 0 {
		t.Errorf("skippedCount = %d, want 0", status.SkippedCount)
	}
}
