package app

import (
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflowhost"
	"agent-overflow/internal/worktreesetup"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// The merge-join contract end to end (`accounts_for_units: true`).
//
// The live failure it exists for: a merge join that stopped at its first
// conflict, reported the lanes it had already taken, and said nothing at all
// about the one it dropped. Nothing downstream could tell — the join's envelope
// IS the phase's envelope, so a unit it does not mention simply does not exist.

func writeAccountingFanOutWorkflow(t *testing.T, configRoot string) {
	t.Helper()
	dir := filepath.Join(configRoot, "workflows")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	definition := `id: accounting-flow
name: Accounting flow
phases:
  - id: port
    name: Port in parallel
    shape: fan-out
    outputs:
      merged:
        schema:
          type: array
          items:
            type: string
      blocked:
        schema:
          type: array
          items:
            type: object
            properties:
              unit:
                type: string
              reason:
                type: string
            required: [unit, reason]
    fan_out:
      - id: alpha
        provider: claude
        model: claude-opus-4-7
        prompt: lane.md
        access: read-only
      - id: beta
        provider: claude
        model: claude-opus-4-7
        prompt: lane.md
        access: read-only
    join:
      id: merge
      provider: claude
      model: claude-opus-4-7
      prompt: merge.md
      access: read-only
      accounts_for_units: true
    gate:
      routes:
        - to: done
cleanup: manual
`
	if err := os.WriteFile(filepath.Join(dir, "accounting-flow.yaml"), []byte(definition), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"lane.md":  "Port the LANE-UNIT slice",
		"merge.md": "MERGE-UNIT: consolidate {{units}}",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// writeAccountingClaude answers a lane turn, then the join's first turn with an
// accounting that drops `beta` entirely — the exact live failure — and its retry
// turn with a complete one. The three cases are told apart by what the prompt
// says, so the script needs no state: only a retry carries the validation
// feedback header.
func writeAccountingClaude(t *testing.T, promptLog string) string {
	t.Helper()
	envelope := func(payload string) string {
		return `printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":` + payload + `}'`
	}
	script := `#!/bin/bash
while IFS= read -r line; do
  printf '%s\n' "$line" >> ` + workflowShellQuote(promptLog) + `
  printf '%s\n' '{"type":"system","subtype":"init","session_id":"accounting","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}'
  if [[ "$line" == *"did not produce a valid workflow control envelope"* ]]; then
    ` + envelope(`{"status":"done","outputs":{"merged":["alpha"],"blocked":[{"unit":"beta","reason":"conflicts in a.go"}]},"question":null,"reason":null,"narrative":"accounted for both"}`) + `
    continue
  fi
  if [[ "$line" == *MERGE-UNIT* ]]; then
    ` + envelope(`{"status":"done","outputs":{"merged":["alpha"],"blocked":[]},"question":null,"reason":null,"narrative":"took what merged cleanly"}`) + `
    continue
  fi
  ` + envelope(`{"status":"done","outputs":null,"question":null,"reason":null,"narrative":"ported the slice"}`) + `
done
`
	return writeExecutable(t, "accounting-claude.sh", script)
}

// A join that leaves a unit out of both lists is refused, told which unit, and
// RETRIED — the ordinary envelope-validation feedback path (D44), never a park
// and never a silent pass. The run then completes on the corrected accounting.
func TestJoinAccountingRefusalRetriesWithFeedbackInsteadOfParking(t *testing.T) {
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	projectRow := testutil.EnsureProject(t, app.store, testutil.InitGitRepo(t))
	projectRow = mustReloadProject(t, app.store, projectRow.ID)
	writeAccountingFanOutWorkflow(t, configRoot)
	writeWorkspaceProfile(t, configRoot, projectRow.Slug, `
base_branch: main
reliability:
  watchdog: 1h
  backoff: [5ms]
`)
	promptLog := filepath.Join(t.TempDir(), "prompts.txt")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": writeAccountingClaude(t, promptLog)}); err != nil {
		t.Fatal(err)
	}
	startWorkflowEngineForTest(t, app, configRoot)

	item, err := app.WorkflowStartRun(
		projectRow.ID, "accounting-flow", "shared", "port it", json.RawMessage(`{}`), nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	// Done, not parked: a refused accounting is feedback the join can act on.
	waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")

	units := unitsByID(t, app, item.ID)
	if units["merge"].Status != store.WorkItemUnitDone {
		t.Fatalf("join did not complete: %+v", units["merge"])
	}

	prompts := strings.Join(readLines(t, promptLog), "\n")
	// The obligation is stated to the join BEFORE it answers, and names the exact
	// set it will be judged against.
	if !strings.Contains(prompts, "must account for every unit") {
		t.Fatalf("the join was never told the rule it is held to:\n%s", prompts)
	}
	for _, id := range []string{`\"alpha\"`, `\"beta\"`} {
		if !strings.Contains(prompts, id) {
			t.Fatalf("the join was not shown unit %s it is judged against:\n%s", id, prompts)
		}
	}
	// The refusal names the unit that went missing, so the retry has something to
	// act on rather than a restatement of the schema.
	if !strings.Contains(prompts, `unit \"beta\" is neither merged nor blocked`) {
		t.Fatalf("the retry feedback does not name the unaccounted unit:\n%s", prompts)
	}

	// The accepted envelope is the corrected one: the phase's outputs are what
	// the gate and everything downstream read.
	detail, err := app.WorkflowGetItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Phases) != 1 || detail.Phases[0].Status != "completed" {
		t.Fatalf("phase = %+v", detail.Phases)
	}
	var envelope struct {
		Outputs struct {
			Merged  []string `json:"merged"`
			Blocked []struct {
				Unit   string `json:"unit"`
				Reason string `json:"reason"`
			} `json:"blocked"`
		} `json:"outputs"`
	}
	if err := json.Unmarshal(detail.Phases[0].OutputEnvelope, &envelope); err != nil {
		t.Fatalf("phase envelope %s: %v", detail.Phases[0].OutputEnvelope, err)
	}
	if len(envelope.Outputs.Merged) != 1 || envelope.Outputs.Merged[0] != "alpha" {
		t.Fatalf("accepted merged list = %+v", envelope.Outputs.Merged)
	}
	if len(envelope.Outputs.Blocked) != 1 || envelope.Outputs.Blocked[0].Unit != "beta" ||
		envelope.Outputs.Blocked[0].Reason == "" {
		t.Fatalf("accepted blocked list = %+v", envelope.Outputs.Blocked)
	}
}

func workflowWorktreeBranch(prefix, workflowID, itemID string) string {
	return gitops.BuildTemporaryWorktreeBranchNameWithPrefix(workflowhost.ItemBranchPrefix(prefix, workflowID, itemID))
}

func TestWorkflowWritingItemProvisionsHooksAndCapturesArtifact(t *testing.T) {
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	repo := testutil.InitGitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("TOKEN=test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	projectRow := testutil.EnsureProject(t, app.store, repo)
	projectRow = mustReloadProject(t, app.store, projectRow.ID)
	writeWorkspaceWorkflow(t, configRoot, "done")
	writeWorkspaceProfile(t, configRoot, projectRow.Slug, `
base_branch: main
reliability:
  watchdog: 1h
  backoff: [5ms]
`)
	seedWorktreeSetup(t, app, projectRow.ID, worktreesetup.Config{
		Copy:    []string{".env"},
		Run:     [][]string{{"/bin/sh", "-c", "printf hook-ran > setup.txt"}},
		Timeout: "2s",
	})
	cwdCapture := filepath.Join(t.TempDir(), "cwd")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": writeWorkspaceClaude(t, cwdCapture, true, "done")}); err != nil {
		t.Fatal(err)
	}
	startWorkflowEngineForTest(t, app, configRoot)

	item, err := app.WorkflowStartRun(projectRow.ID, "workspace-flow", "shared", "write", json.RawMessage(`{}`), nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	item = waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")
	if item.WorktreePath == "" || item.Branch == "" || item.BaseBranch != "main" || !strings.Contains(item.Branch, "workflow-workspace-flow") {
		t.Fatalf("provisioned workspace = %+v", item)
	}
	t.Cleanup(func() { _ = app.gitCore().RemoveWorktreeForce(repo, item.WorktreePath, true) })
	for path, want := range map[string]string{
		".env":           "TOKEN=test\n",
		"setup.txt":      "hook-ran",
		"deliverable.md": "artifact body\n",
	} {
		data, readErr := os.ReadFile(filepath.Join(item.WorktreePath, path))
		if readErr != nil || string(data) != want {
			t.Fatalf("worktree file %s = %q, %v; want %q", path, data, readErr, want)
		}
	}
	cwd, err := os.ReadFile(cwdCapture)
	if err != nil || testutil.CanonicalPath(t, strings.TrimSpace(string(cwd))) != testutil.CanonicalPath(t, item.WorktreePath) {
		t.Fatalf("provider cwd = %q, %v; want %q", cwd, err, item.WorktreePath)
	}
	detail, err := app.WorkflowGetItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Artifacts) != 1 || detail.Artifacts[0].Name != "report" || filepath.Ext(detail.Artifacts[0].Path) != ".md" || detail.Artifacts[0].Size != int64(len("artifact body\n")) {
		t.Fatalf("artifacts = %+v", detail.Artifacts)
	}
	artifact, err := os.ReadFile(detail.Artifacts[0].Path)
	if err != nil || string(artifact) != "artifact body\n" {
		t.Fatalf("captured artifact = %q, %v", artifact, err)
	}
	if len(detail.Phases) != 1 {
		t.Fatalf("phases = %+v", detail.Phases)
	}
	thread, err := app.store.GetThread(detail.Phases[0].ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if thread.WorkspacePath != item.WorktreePath || thread.WorktreePath != item.WorktreePath || thread.Branch != item.Branch {
		t.Fatalf("phase thread workspace = %+v, item=%+v", thread, item)
	}
}

func TestWorkflowHookFailureAndTimeoutParkSetupFailed(t *testing.T) {
	for _, test := range []struct {
		name    string
		command []string
		timeout string
	}{
		{name: "exit", command: []string{"/bin/sh", "-c", "printf failed-output; exit 7"}, timeout: "2s"},
		{name: "timeout", command: []string{"/bin/sleep", "1"}, timeout: "20ms"},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, bus := setupE2EApp(t)
			app.testEmitHook = bus.emit
			configRoot := t.TempDir()
			repo := testutil.InitGitRepo(t)
			projectRow := testutil.EnsureProject(t, app.store, repo)
			projectRow = mustReloadProject(t, app.store, projectRow.ID)
			writeWorkspaceWorkflow(t, configRoot, "done")
			writeWorkspaceProfile(t, configRoot, projectRow.Slug, workspaceProfileYAML)
			seedWorktreeSetup(t, app, projectRow.ID, worktreesetup.Config{
				Run: [][]string{test.command}, Timeout: test.timeout,
			})
			if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": writeWorkspaceClaude(t, filepath.Join(t.TempDir(), "cwd"), false, "done")}); err != nil {
				t.Fatal(err)
			}
			startWorkflowEngineForTest(t, app, configRoot)
			if err := app.WorkflowSetGlobalPause(true); err != nil {
				t.Fatal(err)
			}
			item, err := app.WorkflowStartRun(projectRow.ID, "workspace-flow", "shared", test.name, json.RawMessage(`{}`), nil, "", false)
			if err != nil {
				t.Fatal(err)
			}
			if err := app.WorkflowSetGlobalPause(false); err != nil {
				t.Fatalf("fire-and-forget unpause returned provisioning failure inline: %v", err)
			}
			item = waitForWorkflowItem(t, app, item.ID, engine.StateNeedsHuman, engine.ReasonSetupFailed)
			assertWorkflowSetupFailureEvents(t, bus, item.ID)
			if item.WorktreePath != "" || item.Branch != "" {
				t.Fatalf("failed setup persisted workspace = %+v", item)
			}
			if item.BaseBranch != "main" {
				t.Fatalf("setup rollback lost intake base branch = %q, want main", item.BaseBranch)
			}
			worktrees, err := app.gitCore().ListWorktrees(repo)
			if err != nil || len(worktrees) != 1 {
				t.Fatalf("worktrees after rollback = %+v, %v", worktrees, err)
			}
			if test.name == "exit" {
				// Clearing the recipe is the fix a human applies before resuming.
				seedWorktreeSetup(t, app, projectRow.ID, worktreesetup.Config{})
				if err := app.WorkflowResumeItem(context.Background(), item.ID, "", false); err != nil {
					t.Fatal(err)
				}
				item = waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")
				if item.WorktreePath == "" {
					t.Fatal("setup retry did not provision a new worktree")
				}
				t.Cleanup(func() { _ = app.gitCore().RemoveWorktreeForce(repo, item.WorktreePath, true) })
			}
		})
	}
}

func assertWorkflowSetupFailureEvents(t *testing.T, bus *capturedEventBus, itemID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	foundState, foundError := false, false
	for !foundState || !foundError {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("setup failure events incomplete: item-state=%v workflow:error=%v", foundState, foundError)
		}
		event := bus.next(t, remaining)
		switch event.Name {
		case "workflow:item-state":
			state, ok := event.Data.(engine.StateEvent)
			if ok && state.ItemID == itemID && state.From == engine.StateRunning &&
				state.To == engine.StateNeedsHuman && state.Reason == engine.ReasonSetupFailed {
				foundState = true
			}
		case "workflow:error":
			workflowErr, ok := event.Data.(engine.ErrorEvent)
			if ok && workflowErr.ItemID == itemID {
				if !errors.Is(workflowErr.Cause(), engine.ErrSetupFailed) {
					t.Fatalf("workflow:error cause = %v, want setup failure", workflowErr.Cause())
				}
				if workflowErr.Error == "" {
					t.Fatal("workflow:error omitted user-facing message")
				}
				foundError = true
			}
		}
	}
}

func TestWorkflowResumeWithMissingWorktreeParksSetupFailed(t *testing.T) {
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	repo := testutil.InitGitRepo(t)
	projectRow := mustReloadProject(t, app.store, testutil.EnsureProject(t, app.store, repo).ID)
	writeWorkspaceWorkflow(t, configRoot, "stuck")
	writeWorkspaceProfile(t, configRoot, projectRow.Slug, workspaceProfileYAML)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": writeWorkspaceClaude(t, filepath.Join(t.TempDir(), "cwd"), false, "stuck")}); err != nil {
		t.Fatal(err)
	}
	startWorkflowEngineForTest(t, app, configRoot)
	item, err := app.WorkflowStartRun(projectRow.ID, "workspace-flow", "shared", "missing", json.RawMessage(`{}`), nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	item = waitForWorkflowItem(t, app, item.ID, engine.StateNeedsHuman, engine.ReasonStuck)
	if err := app.gitCore().RemoveWorktreeForce(repo, item.WorktreePath, true); err != nil {
		t.Fatal(err)
	}
	if err := app.WorkflowResumeItem(context.Background(), item.ID, "", false); err == nil {
		t.Fatal("resume with missing worktree succeeded")
	}
	got := waitForWorkflowItem(t, app, item.ID, engine.StateNeedsHuman, engine.ReasonSetupFailed)
	if got.WorktreePath != item.WorktreePath || got.Branch != item.Branch {
		t.Fatalf("missing-worktree resume rewrote workspace: before=%+v after=%+v", item, got)
	}
}

func TestWorkflowRecoversInterruptedProvisioningWithoutSecondWorktree(t *testing.T) {
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	repo := testutil.InitGitRepo(t)
	projectRow := mustReloadProject(t, app.store, testutil.EnsureProject(t, app.store, repo).ID)
	writeWorkspaceWorkflow(t, configRoot, "done")
	writeWorkspaceProfile(t, configRoot, projectRow.Slug, workspaceProfileYAML)
	seedWorktreeSetup(t, app, projectRow.ID, worktreesetup.Config{
		Run: [][]string{{"/bin/sh", "-c", "printf recovered > recovered.txt"}}, Timeout: "2s",
	})
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": writeWorkspaceClaude(t, filepath.Join(t.TempDir(), "cwd"), true, "done")}); err != nil {
		t.Fatal(err)
	}
	startWorkflowEngineForTest(t, app, configRoot)
	if err := app.WorkflowSetGlobalPause(true); err != nil {
		t.Fatal(err)
	}
	item, err := app.WorkflowStartRun(projectRow.ID, "workspace-flow", "shared", "recover provision", json.RawMessage(`{}`), nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	branch := workflowWorktreeBranch(app.worktreeBranchPrefix(), "workspace-flow", item.ID)
	interruptedPath, err := app.defaultWorktreePath(repo, branch)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.gitCore().CreateWorktreeFromBranch(repo, interruptedPath, "main", branch); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.gitCore().RemoveWorktreeForce(repo, interruptedPath, true) })
	if err := app.WorkflowSetGlobalPause(false); err != nil {
		t.Fatal(err)
	}
	item = waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")
	// Recovery adopts the path `git worktree list` reports, which on macOS is
	// the /private/var resolution of the /var temp path this test computed.
	if testutil.CanonicalPath(t, item.WorktreePath) != testutil.CanonicalPath(t, interruptedPath) || item.Branch != branch {
		t.Fatalf("recovered workspace = %+v, want %q on %q", item, interruptedPath, branch)
	}
	if _, err := os.Stat(filepath.Join(interruptedPath, "recovered.txt")); err != nil {
		t.Fatalf("recovered setup hook did not run: %v", err)
	}
	worktrees, err := app.gitCore().ListWorktrees(repo)
	if err != nil || len(worktrees) != 2 {
		t.Fatalf("worktrees after recovery = %+v, %v", worktrees, err)
	}
}

func TestWorkflowStepModeBindingParksThenApprovesToDone(t *testing.T) {
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	writeReliabilityWorkflow(t, configRoot, `
  - id: run
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: run.md
    access: read-only
    outputs:
      ready:
        schema:
          type: boolean
    gate:
      routes:
        - to: done`)
	projectRow := mustReloadProject(t, app.store, testutil.EnsureProject(t, app.store, t.TempDir()).ID)
	writeReliabilityProfile(t, configRoot, projectRow.Slug, "watchdog: 1h\n  backoff: [5ms]\n")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": writeBudgetWorkflowClaude(t)}); err != nil {
		t.Fatal(err)
	}
	startWorkflowEngineForTest(t, app, configRoot)
	item, err := app.WorkflowStartRun(projectRow.ID, "reliability-flow", "shared", "step", json.RawMessage(`{}`), nil, "", true)
	if err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, app, item.ID, engine.StateNeedsHuman, engine.ReasonGate)
	if err := app.WorkflowResolveGate(context.Background(), item.ID, "reject", ""); err == nil || !strings.Contains(err.Error(), "step gates support approve") {
		t.Fatalf("reject error = %v", err)
	}
	if err := app.WorkflowResolveGate(context.Background(), item.ID, "approve", ""); err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")
}

func TestWorkflowArtifactFailureDoesNotChangePhaseOutcome(t *testing.T) {
	app, bus := setupE2EApp(t)
	app.testEmitHook = bus.emit
	configRoot := t.TempDir()
	repo := testutil.InitGitRepo(t)
	projectRow := mustReloadProject(t, app.store, testutil.EnsureProject(t, app.store, repo).ID)
	writeWorkspaceWorkflow(t, configRoot, "done")
	writeWorkspaceProfile(t, configRoot, projectRow.Slug, workspaceProfileYAML)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": writeWorkspaceClaudeWithArtifactPath(t, filepath.Join(t.TempDir(), "cwd"), false, "done", "../escape.md")}); err != nil {
		t.Fatal(err)
	}
	startWorkflowEngineForTest(t, app, configRoot)
	item, err := app.WorkflowStartRun(projectRow.ID, "workspace-flow", "shared", "artifact failure", json.RawMessage(`{}`), nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	item = waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")
	t.Cleanup(func() { _ = app.gitCore().RemoveWorktreeForce(repo, item.WorktreePath, true) })
	detail, err := app.WorkflowGetItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Artifacts) != 0 || len(detail.Phases) != 1 || detail.Phases[0].Status != "completed" {
		t.Fatalf("artifact failure changed outcome: %+v", detail)
	}
	bus.mu.Lock()
	events := append([]capturedEvent(nil), bus.all...)
	bus.mu.Unlock()
	found := false
	for _, event := range events {
		payload, ok := event.Data.(map[string]any)
		if event.Name == "workflow:error" && ok && payload["output"] == "report" {
			found = true
		}
	}
	if !found {
		t.Fatalf("artifact error naming report was not emitted: %s", summarizeEvents(events))
	}
}

func TestWorkflowWorktreeBranchUsesWorkflowIdentity(t *testing.T) {
	if branch := workflowWorktreeBranch("task", "flow", "12345678-abcd"); !strings.HasPrefix(branch, "task-workflow-flow-12345678-abcd-") {
		t.Fatalf("workflow branch = %q", branch)
	}
}

func writeWorkspaceWorkflow(t *testing.T, configRoot, outcome string) {
	t.Helper()
	dir := filepath.Join(configRoot, "workflows")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	definition := `id: workspace-flow
name: Workspace flow
outputs:
  report:
    from: write.report
    artifact: true
phases:
  - id: write
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: write.md
    access: write
    outputs:
      report:
        schema:
          type: string
    gate:
      routes:
        - to: done
cleanup: auto
`
	if err := os.WriteFile(filepath.Join(dir, "workspace-flow.yaml"), []byte(definition), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "write.md"), []byte("Write the deliverable for "+outcome), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A profile.yaml that still carries the retired worktree_setup block must fail
// LOUDLY with the message naming where the recipe moved to, rather than loading
// and silently running no setup on every worktree the project cuts.
func TestUnmigratedProfileWorktreeSetupBlockFailsLoudly(t *testing.T) {
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	app.configDir = configRoot
	repo := testutil.InitGitRepo(t)
	projectRow := mustReloadProject(t, app.store, testutil.EnsureProject(t, app.store, repo).ID)
	writeWorkspaceWorkflow(t, configRoot, "done")
	writeWorkspaceProfile(t, configRoot, projectRow.Slug, workspaceProfileYAML+"worktree_setup:\n  copy: [.env]\n")

	_, err := app.WorkflowListDefinitions(projectRow.ID)
	if err == nil {
		t.Fatal("profile still carrying worktree_setup loaded")
	}
	if !strings.Contains(err.Error(), "Settings") || !strings.Contains(err.Error(), "worktree_setup") {
		t.Fatalf("error does not direct the author to the new home: %v", err)
	}

	// Removing the block is the whole fix — the recipe now lives on the row.
	writeWorkspaceProfile(t, configRoot, projectRow.Slug, workspaceProfileYAML)
	seedWorktreeSetup(t, app, projectRow.ID, worktreesetup.Config{Copy: []string{".env"}})
	if _, err := app.WorkflowListDefinitions(projectRow.ID); err != nil {
		t.Fatalf("migrated profile = %v", err)
	}
}

// workspaceProfileYAML is the profile these workspace tests share now that the
// worktree setup recipe lives on the project row instead of in profile.yaml.
const workspaceProfileYAML = "\nbase_branch: main\nreliability:\n  watchdog: 1h\n  backoff: [5ms]\n"

// seedWorktreeSetup persists a project's setup recipe the way the Settings
// editor does — through the validating binding, so a fixture cannot seed a
// recipe the UI would have refused.
func seedWorktreeSetup(t *testing.T, app *App, projectID string, config worktreesetup.Config) {
	t.Helper()
	if _, err := app.SetProjectWorktreeSetup(projectID, WorktreeSetupConfig{
		Copy: config.Copy, Run: config.Run, Timeout: config.Timeout,
	}); err != nil {
		t.Fatalf("seed worktree setup: %v", err)
	}
}

func writeWorkspaceProfile(t *testing.T, configRoot, slug, contents string) {
	t.Helper()
	dir := filepath.Join(configRoot, "projects", slug)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "profile.yaml"), []byte(strings.TrimSpace(contents)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeWorkspaceClaude(t *testing.T, cwdCapture string, writeArtifact bool, outcome string) string {
	return writeWorkspaceClaudeWithArtifactPath(t, cwdCapture, writeArtifact, outcome, "deliverable.md")
}

func writeWorkspaceClaudeWithArtifactPath(t *testing.T, cwdCapture string, writeArtifact bool, outcome, artifactPath string) string {
	t.Helper()
	artifactCommand := ""
	if writeArtifact {
		artifactCommand = `printf 'artifact body\n' > deliverable.md`
	}
	encodedPath, err := json.Marshal(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	status := `{"status":"done","outputs":{"report":` + string(encodedPath) + `},"question":null,"reason":null}`
	if outcome == "stuck" {
		status = `{"status":"stuck","outputs":null,"question":null,"reason":"blocked"}`
	}
	script := `#!/bin/bash
while IFS= read -r line; do
  pwd > ` + workflowShellQuote(cwdCapture) + `
  ` + artifactCommand + `
  printf '%s\n' '{"type":"system","subtype":"init","session_id":"workspace","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}'
  printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":` + status + `}'
done
`
	return writeExecutable(t, "workspace-claude.sh", script)
}

func workflowShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
