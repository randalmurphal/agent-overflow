package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflowhost"
)

// TestWorkflowStaticFanOutRunsUnitsAndJoin is the end-to-end proof that a
// fan-out phase executes as units rather than as one turn: two writing units run
// on their own threads in their own sub-worktrees, the join runs on the item's
// primary workspace, and the join's envelope is what drives the phase's gate.
func TestWorkflowStaticFanOutRunsUnitsAndJoin(t *testing.T) {
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	repo := testutil.InitGitRepo(t)
	projectRow := testutil.EnsureProject(t, app.store, repo)
	projectRow = mustReloadProject(t, app.store, projectRow.ID)
	writeFanOutWorkflow(t, configRoot)
	writeWorkspaceProfile(t, configRoot, projectRow.Slug, `
base_branch: main
reliability:
  watchdog: 1h
  backoff: [5ms]
`)
	cwdLog := filepath.Join(t.TempDir(), "cwds.txt")
	if _, err := app.settings.Update(map[string]any{
		"claudeBinaryPath": writeFanOutClaude(t, cwdLog, ""),
	}); err != nil {
		t.Fatal(err)
	}
	startWorkflowEngineForTest(t, app, configRoot)

	item, err := app.WorkflowStartRun(projectRow.ID, "fanout-flow", "shared", "port it", json.RawMessage(`{}`), nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	item = waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")
	t.Cleanup(func() { _ = app.gitCore().RemoveWorktreeForce(repo, item.WorktreePath, true) })
	if item.WorktreePath == "" || item.Branch == "" {
		t.Fatalf("writing fan-out did not provision the item workspace: %+v", item)
	}

	units := unitsByID(t, app, item.ID)
	if len(units) != 3 {
		t.Fatalf("units = %+v, want alpha, beta, and the join", units)
	}
	for _, id := range []string{"alpha", "beta", "merge"} {
		if units[id].Status != store.WorkItemUnitDone {
			t.Fatalf("unit %q status = %q, want done", id, units[id].Status)
		}
		if units[id].ThreadID == "" {
			t.Fatalf("unit %q ran without its own thread: %+v", id, units[id])
		}
	}
	if units["merge"].Kind != store.WorkItemUnitKindJoin || units["alpha"].Kind != store.WorkItemUnitKindUnit {
		t.Fatalf("unit kinds = %q / %q", units["merge"].Kind, units["alpha"].Kind)
	}

	// Each writing unit got its own branch, deterministically derived from the
	// item's branch, and every one of them still exists after the run: the join
	// consumed the checkouts, not the history.
	branches := map[string]bool{}
	for _, branch := range mustListBranches(t, app, repo) {
		branches[branch.Name] = true
	}
	for _, id := range []string{"alpha", "beta"} {
		want := workflowhost.UnitBranch(item.Branch, workflowhost.UnitWorkspaceRef{
			ItemID: item.ID, PhaseID: "port", Attempt: 1, UnitID: id, UnitAttempt: 1,
		})
		if units[id].Branch != want {
			t.Fatalf("unit %q branch = %q, want %q", id, units[id].Branch, want)
		}
		if !branches[want] {
			t.Fatalf("unit %q branch %q was removed with its worktree", id, want)
		}
	}
	if units["alpha"].Branch == units["beta"].Branch {
		t.Fatal("both writing units landed on one branch")
	}
	if units["merge"].Branch != "" || units["merge"].WorktreePath != "" {
		t.Fatalf("join was isolated: %+v", units["merge"])
	}

	// A consumed sub-worktree is removed once the join is done, and the row stops
	// claiming a path that is no longer on disk.
	for _, id := range []string{"alpha", "beta"} {
		if units[id].WorktreePath != "" {
			t.Fatalf("unit %q still records worktree %q after the join finished", id, units[id].WorktreePath)
		}
	}
	worktrees, err := app.gitCore().ListWorktrees(repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, worktree := range worktrees {
		if strings.HasPrefix(worktree.Branch, item.Branch+"-") {
			t.Fatalf("unit worktree %q survived a done join", worktree.Path)
		}
	}

	// Where each turn actually ran: two distinct sub-worktrees for the units, the
	// item's own worktree for the join.
	cwds := readLines(t, cwdLog)
	if len(cwds) != 3 {
		t.Fatalf("provider ran %d times: %v", len(cwds), cwds)
	}
	unitCwds := map[string]bool{}
	joinRuns := 0
	// The provider records its real cwd, which on macOS is the /private/var
	// resolution of the /var temp path the run record stores.
	itemWorktree := testutil.CanonicalPath(t, item.WorktreePath)
	for _, cwd := range cwds {
		if testutil.CanonicalPath(t, cwd) == itemWorktree {
			joinRuns++
			continue
		}
		unitCwds[testutil.CanonicalPath(t, cwd)] = true
	}
	if joinRuns != 1 || len(unitCwds) != 2 {
		t.Fatalf("cwds = %v; want two distinct unit worktrees and one run in %q", cwds, item.WorktreePath)
	}

	// Unit threads are ordinary inspectable AO threads, titled for the unit and
	// carrying the access their definition declared (D22, per unit).
	for _, tc := range []struct {
		unitID      string
		wantTitle   string
		wantRuntime provider.RuntimeMode
		wantSpace   string
	}{
		{"alpha", "Workflow: Port in parallel / alpha", provider.RuntimeFullAccess, ""},
		{"beta", "Workflow: Port in parallel / beta", provider.RuntimeFullAccess, ""},
		{"merge", "Workflow: Port in parallel / merge", provider.RuntimeReadOnly, item.WorktreePath},
	} {
		thread, threadErr := app.store.GetThread(units[tc.unitID].ThreadID)
		if threadErr != nil {
			t.Fatal(threadErr)
		}
		if thread.Title != tc.wantTitle {
			t.Fatalf("unit %q thread title = %q, want %q", tc.unitID, thread.Title, tc.wantTitle)
		}
		if thread.Mode != "workflow" {
			t.Fatalf("unit %q thread mode = %q, want workflow", tc.unitID, thread.Mode)
		}
		if thread.RuntimeMode != string(tc.wantRuntime) {
			t.Fatalf("unit %q thread runtime_mode = %q, want %q", tc.unitID, thread.RuntimeMode, tc.wantRuntime)
		}
		if tc.wantSpace != "" && thread.WorkspacePath != tc.wantSpace {
			t.Fatalf("unit %q workspace = %q, want %q", tc.unitID, thread.WorkspacePath, tc.wantSpace)
		}
	}

	// The join's thread is the phase attempt's thread: its envelope IS the
	// phase's, so every phase-level continuation resolves through it.
	detail, err := app.WorkflowGetItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Phases) != 1 {
		t.Fatalf("phases = %+v", detail.Phases)
	}
	if detail.Phases[0].ThreadID != units["merge"].ThreadID {
		t.Fatalf("phase thread = %q, join thread = %q", detail.Phases[0].ThreadID, units["merge"].ThreadID)
	}
	if len(detail.Units) != 3 {
		t.Fatalf("run detail units = %+v", detail.Units)
	}
	// Each unit's narrative is its own file under its own try directory, so one
	// unit's account of its work can never overwrite another's.
	narratives := map[string]bool{}
	for id, unit := range units {
		if unit.NarrativePath == "" {
			t.Fatalf("unit %q has no narrative path", id)
		}
		narratives[unit.NarrativePath] = true
	}
	if len(narratives) != 3 {
		t.Fatalf("units share narrative files: %+v", units)
	}
}

// TestWorkflowUnitBranchNamesAreDerivedAndDeterministic pins the naming scheme
// the recovery paths depend on: re-entering one try finds its own worktree, and
// nothing that provisions a DIFFERENT lane can derive the name it already
// holds.
func TestWorkflowUnitBranchNamesAreDerivedAndDeterministic(t *testing.T) {
	const itemBranch = "ao-workflow-port-1234abcd-5f3a"
	base := workflowhost.UnitWorkspaceRef{
		ItemID: "9f1c3a4e-2b77-4d51-9c8a-3e6f0b2d1a55", PhaseID: "implement",
		Attempt: 1, UnitID: "port-0", UnitAttempt: 1,
	}
	first := workflowhost.UnitBranch(itemBranch, base)
	if first != itemBranch+"-9f1c3a4e-implement-a1-port-0-1" {
		t.Fatalf("unit branch = %q", first)
	}
	if again := workflowhost.UnitBranch(itemBranch, base); again != first {
		t.Fatalf("unit branch is not deterministic: %q vs %q", again, first)
	}

	// Every coordinate that can differ between two live lanes has to move the
	// name. The item id is what separates the waves of a self-calling campaign,
	// which all fan out from ONE item branch; the phase attempt is what
	// separates a re-expanded phase from the attempt it replaces, whose tries
	// restart at 1; the phase id is what separates two fan-outs of one workflow
	// that name their units alike.
	for _, tc := range []struct {
		name string
		ref  workflowhost.UnitWorkspaceRef
	}{
		{"another wave's fan-out owner", withUnitRef(base, func(r *workflowhost.UnitWorkspaceRef) {
			r.ItemID = "0d55e21b-88fa-4c19-b7d0-1a92c4e73f60"
		})},
		{"another fan-out phase", withUnitRef(base, func(r *workflowhost.UnitWorkspaceRef) { r.PhaseID = "review" })},
		{"a re-expanded phase attempt", withUnitRef(base, func(r *workflowhost.UnitWorkspaceRef) { r.Attempt = 2 })},
		{"a sibling unit", withUnitRef(base, func(r *workflowhost.UnitWorkspaceRef) { r.UnitID = "port-1" })},
		{"a retry of the same unit", withUnitRef(base, func(r *workflowhost.UnitWorkspaceRef) { r.UnitAttempt = 2 })},
	} {
		branch := workflowhost.UnitBranch(itemBranch, tc.ref)
		if branch == first {
			t.Fatalf("%s derived the same branch %q", tc.name, branch)
		}
		// Every branch a run creates extends the item's branch, which is what
		// makes the whole set findable from the item alone.
		if !strings.HasPrefix(branch, itemBranch+"-") {
			t.Fatalf("unit branch %q does not extend the item branch", branch)
		}
		if err := gitops.ValidateBranchName(branch); err != nil {
			t.Fatalf("unit branch %q is not a legal ref: %v", branch, err)
		}
	}

	// Author-controlled ids are sanitized into ref fragments, and an absent
	// attempt or try can never produce a branch two of them would share.
	sanitized := workflowhost.UnitBranch(itemBranch, workflowhost.UnitWorkspaceRef{
		ItemID: "9f1c3a4e-2b77-4d51-9c8a-3e6f0b2d1a55", PhaseID: "Port Phase",
		Attempt: 0, UnitID: "Port Section", UnitAttempt: 0,
	})
	if want := itemBranch + "-9f1c3a4e-port-phase-a1-port-section-1"; sanitized != want {
		t.Fatalf("sanitized unit branch = %q, want %q", sanitized, want)
	}
}

func withUnitRef(base workflowhost.UnitWorkspaceRef, edit func(*workflowhost.UnitWorkspaceRef)) workflowhost.UnitWorkspaceRef {
	edit(&base)
	return base
}

// TestWorkflowJoinReceivesUnitGitStateAndKeepsDirtyWorktrees covers the two
// halves of the commit contract at the join boundary: the join is TOLD what each
// lane actually committed and what it left behind, and a lane that left work
// uncommitted keeps its checkout instead of having it destroyed by the
// retirement that follows a done join.
//
// The join here is a tool, which is the shipped campaign shape
// (`merge-unit-branches`): it receives `{{units}}` as one argv element, so this
// also pins that the enrichment reaches argv interpolation and not only prompts.
func TestWorkflowJoinReceivesUnitGitStateAndKeepsDirtyWorktrees(t *testing.T) {
	app, bus := setupE2EApp(t)
	configRoot := t.TempDir()
	repo := testutil.InitGitRepo(t)
	projectRow := mustReloadProject(t, app.store, testutil.EnsureProject(t, app.store, repo).ID)
	writeCommitFanOutWorkflow(t, configRoot)
	unitsJSON := filepath.Join(t.TempDir(), "units.json")
	writeWorkspaceProfile(t, configRoot, projectRow.Slug, `
base_branch: main
reliability:
  watchdog: 1h
  backoff: [5ms]
commands:
  merge-units: `+mustJSONArgv(t, []string{
		writeExecutable(t, "merge-units.sh", "#!/bin/sh\nprintf '%s' \"$1\" > "+unitsJSON+"\n"),
		"{{units}}",
	}))
	if _, err := app.settings.Update(map[string]any{
		"claudeBinaryPath": writeCommittingFanOutClaude(t),
	}); err != nil {
		t.Fatal(err)
	}
	app.testEmitHook = bus.emit
	startWorkflowEngineForTest(t, app, configRoot)

	item, err := app.WorkflowStartRun(projectRow.ID, "commit-fanout", "shared", "port it", json.RawMessage(`{}`), nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	item = waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")
	t.Cleanup(func() { _ = app.gitCore().RemoveWorktreeForce(repo, item.WorktreePath, true) })

	units := unitsByID(t, app, item.ID)
	// What the join was handed. Both lanes committed one commit onto their own
	// branch; only the untidy one left anything in its working tree.
	state := map[string]map[string]any{}
	for _, entry := range decodeUnitsArgv(t, unitsJSON) {
		id, _ := entry["id"].(string)
		state[id] = entry
	}
	if len(state) != 2 {
		t.Fatalf("join received %d unit entries: %+v", len(state), state)
	}
	for _, id := range []string{"tidy", "untidy"} {
		if ahead, ok := state[id]["commitsAhead"].(float64); !ok || ahead != 1 {
			t.Fatalf("unit %q commitsAhead = %v, want 1", id, state[id]["commitsAhead"])
		}
		if state[id]["branch"] != units[id].Branch {
			t.Fatalf("unit %q branch in join input = %v, row = %q", id, state[id]["branch"], units[id].Branch)
		}
	}
	if state["tidy"]["dirty"] != false {
		t.Fatalf("committed lane reported dirty = %v", state["tidy"]["dirty"])
	}
	if state["untidy"]["dirty"] != true {
		t.Fatalf("lane with uncommitted work reported dirty = %v", state["untidy"]["dirty"])
	}

	// Retirement is non-force, so it takes the clean checkout and refuses the
	// one still holding work no branch carries. The refused row KEEPS its path:
	// it is the only pointer left to that work.
	if units["tidy"].WorktreePath != "" {
		t.Fatalf("clean unit worktree survived a done join: %q", units["tidy"].WorktreePath)
	}
	if units["untidy"].WorktreePath == "" {
		t.Fatal("retirement cleared the path of a worktree it did not remove")
	}
	if _, err := os.Stat(units["untidy"].WorktreePath); err != nil {
		t.Fatalf("uncommitted unit work was destroyed: %v", err)
	}
	t.Cleanup(func() { _ = app.gitCore().RemoveWorktreeForce(repo, units["untidy"].WorktreePath, true) })

	// And it is loud: a slipped commit contract must not be a silent retention.
	retained := ""
	for _, event := range bus.allEvents() {
		payload, ok := event.Data.(map[string]any)
		if event.Name != "workflow:error" || !ok || payload["itemId"] != item.ID {
			continue
		}
		if message, _ := payload["error"].(string); strings.Contains(message, "untidy") {
			retained = message
		}
	}
	if retained == "" {
		t.Fatalf("retained worktree emitted no workflow:error; events = %+v", bus.allEvents())
	}
	for _, want := range []string{units["untidy"].WorktreePath, units["untidy"].Branch, "uncommitted"} {
		if !strings.Contains(retained, want) {
			t.Fatalf("retention message %q does not name %q", retained, want)
		}
	}
}

func mustJSONArgv(t *testing.T, argv []string) string {
	t.Helper()
	encoded, err := json.Marshal(argv)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func decodeUnitsArgv(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var entries []map[string]any
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("join received %q, which is not the units array: %v", data, err)
	}
	return entries
}

// writeCommitFanOutWorkflow declares two writing units and a tool join, which is
// the campaign shape: the join consolidates BRANCHES, so what each unit
// committed is the only thing it can consume.
func writeCommitFanOutWorkflow(t *testing.T, configRoot string) {
	t.Helper()
	dir := filepath.Join(configRoot, "workflows")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	unit := func(id string) string {
		return fmt.Sprintf(`      - id: %s
        provider: claude
        model: claude-opus-4-7
        prompt: %s.md
        access: write
        outputs:
          report:
            schema:
              type: string
`, id, id)
	}
	definition := `id: commit-fanout
name: Commit fan-out
phases:
  - id: port
    name: Port in parallel
    shape: fan-out
    fan_out:
` + unit("tidy") + unit("untidy") + `    join:
      id: merge
      command: merge-units
    gate:
      routes:
        - to: done
cleanup: manual
`
	if err := os.WriteFile(filepath.Join(dir, "commit-fanout.yaml"), []byte(definition), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"tidy.md":   "Port the TIDY-LANE slice",
		"untidy.md": "Port the UNTIDY-LANE slice",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// writeCommittingFanOutClaude commits its work like a writing element is told
// to. The untidy lane also leaves an untracked file behind, which is the slip
// the retention path exists for.
func writeCommittingFanOutClaude(t *testing.T) string {
	t.Helper()
	done := `{"status":"done","outputs":{"report":"ok"},"question":null,"reason":null}`
	script := `#!/bin/bash
while IFS= read -r line; do
  if [[ "$line" == *TIDY-LANE* || "$line" == *UNTIDY-LANE* ]]; then
    printf 'lane\n' > lane.txt
    git add lane.txt
    git commit -q -m 'lane work'
  fi
  if [[ "$line" == *UNTIDY-LANE* ]]; then
    printf 'never committed\n' > scratch.txt
  fi
  printf '%s\n' '{"type":"system","subtype":"init","session_id":"commit-fanout","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}'
  printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":` + done + `}'
done
`
	return writeExecutable(t, "committing-claude.sh", script)
}

func unitsByID(t *testing.T, app *App, itemID string) map[string]store.WorkItemUnit {
	t.Helper()
	rows, err := app.store.ListWorkItemUnits(itemID)
	if err != nil {
		t.Fatal(err)
	}
	units := make(map[string]store.WorkItemUnit, len(rows))
	for _, unit := range rows {
		units[unit.UnitID] = unit
	}
	return units
}

func mustListBranches(t *testing.T, app *App, repo string) []gitops.GitBranch {
	t.Helper()
	branches, err := app.gitCore().ListBranches(repo)
	if err != nil {
		t.Fatal(err)
	}
	return branches
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	return lines
}

// writeFanOutWorkflow declares two writing units and a read-only join. The units'
// output contract matches the phase's on purpose: one mock provider script then
// satisfies both a unit envelope and the join's phase envelope, so the test
// exercises real per-element contracts instead of a bespoke stub per role.
func writeFanOutWorkflow(t *testing.T, configRoot string) {
	t.Helper()
	dir := filepath.Join(configRoot, "workflows")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	unit := func(id string) string {
		return fmt.Sprintf(`      - id: %s
        provider: claude
        model: claude-opus-4-7
        prompt: %s.md
        access: write
        outputs:
          report:
            schema:
              type: string
`, id, id)
	}
	definition := `id: fanout-flow
name: Fan-out flow
phases:
  - id: port
    name: Port in parallel
    shape: fan-out
    outputs:
      report:
        schema:
          type: string
    fan_out:
` + unit("alpha") + unit("beta") + `    join:
      id: merge
      provider: claude
      model: claude-opus-4-7
      prompt: merge.md
      access: read-only
    gate:
      routes:
        - to: done
cleanup: manual
`
	if err := os.WriteFile(filepath.Join(dir, "fanout-flow.yaml"), []byte(definition), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"alpha.md": "Port the ALPHA-UNIT slice",
		"beta.md":  "Port the BETA-UNIT slice",
		"merge.md": "Consolidate {{units}}",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// writeFanOutClaude answers every turn with a done envelope and appends the
// working directory it ran in, so the test can see where each unit executed.
//
// Each token in failFirstTurn names a unit whose FIRST turn is stuck instead —
// one deterministic failure per named unit, recorded by a marker file under
// markerDir so the retry succeeds. Naming one unit is the single-failure park;
// naming several is the shape a provider usage limit produces, where one cause
// takes down most of a fan-out at once.
func writeFanOutClaude(t *testing.T, cwdLog, markerDir string, failFirstTurn ...string) string {
	t.Helper()
	done := `{"status":"done","outputs":{"report":"ok"},"question":null,"reason":null}`
	stuck := `{"status":"stuck","outputs":null,"question":null,"reason":"slice does not build"}`
	failure := ""
	for _, token := range failFirstTurn {
		if markerDir == "" {
			t.Fatalf("writeFanOutClaude: failing %q needs a marker directory", token)
		}
		marker := workflowShellQuote(filepath.Join(markerDir, token+".failed"))
		failure += `
  if [[ "$line" == *` + token + `* && ! -f ` + marker + ` ]]; then
    : > ` + marker + `
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":` + stuck + `}'
    continue
  fi`
	}
	script := `#!/bin/bash
while IFS= read -r line; do
  pwd >> ` + workflowShellQuote(cwdLog) + `
  printf '%s\n' '{"type":"system","subtype":"init","session_id":"fanout","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}'` + failure + `
  printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":` + done + `}'
done
`
	return writeExecutable(t, "fanout-claude.sh", script)
}
