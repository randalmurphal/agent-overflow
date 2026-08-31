package app

import (
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflowhost"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A tool phase's command is a real subprocess: these tests bind the project
// profile to scripts on disk and run them through the production start path.

func TestWorkflowToolPhaseGreenCheckAdvancesWithSynthesizedEnvelope(t *testing.T) {
	fixture := newToolWorkflowFixture(t, `
  - id: check
    driver: tool
    check: verify
    gate:
      routes:
        - when:
            eq:
              ref: check.passed
              value: true
          to: done
        - to: failed`)
	fixture.writeProfile(t, map[string][]string{
		"verify": {writeExecutable(t, "green.sh", "#!/bin/sh\necho all good\nexit 0\n")},
	}, nil, "")
	item := fixture.start(t, "green check")

	waitForWorkflowItem(t, fixture.app, item.ID, engine.StateDone, "")
	phases := listWorkflowPhases(t, fixture.app, item.ID)
	if len(phases) != 1 || phases[0].Status != "completed" {
		t.Fatalf("phases = %+v", phases)
	}
	// A tool phase has no provider session, so it has no AO thread: the
	// attempt row carries only the system-written narrative.
	if phases[0].ThreadID != "" {
		t.Fatalf("tool phase attached thread %q", phases[0].ThreadID)
	}
	outputs := decodeEnvelopeOutputs(t, phases[0].OutputEnvelope)
	if outputs["passed"] != true || outputs["exit-code"].(float64) != 0 {
		t.Fatalf("synthesized outputs = %v", outputs)
	}
	narrative := readFileForTest(t, phases[0].NarrativePath)
	for _, want := range []string{"green.sh", "- Exit code: 0", "synthesized from the process exit status", "all good"} {
		if !strings.Contains(narrative, want) {
			t.Fatalf("narrative missing %q:\n%s", want, narrative)
		}
	}
}

func TestWorkflowToolPhaseRedCheckRoutesThroughItsGate(t *testing.T) {
	fixture := newToolWorkflowFixture(t, `
  - id: check
    driver: tool
    check: verify
    gate:
      routes:
        - when:
            eq:
              ref: check.passed
              value: true
          to: done
        - to: failed`)
	fixture.writeProfile(t, map[string][]string{
		"verify": {writeExecutable(t, "red.sh", "#!/bin/sh\necho broken >&2\nexit 3\n")},
	}, nil, "")
	item := fixture.start(t, "red check")

	// A non-zero exit is the check's answer, not a phase failure: the gate is
	// what turns it into a terminal state.
	waitForWorkflowItem(t, fixture.app, item.ID, engine.StateFailed, engine.ReasonCheckFailedGenuine)
	phases := listWorkflowPhases(t, fixture.app, item.ID)
	if len(phases) != 1 || phases[0].Status != "failed" {
		t.Fatalf("phases = %+v", phases)
	}
	outputs := decodeEnvelopeOutputs(t, phases[0].OutputEnvelope)
	if outputs["passed"] != false || outputs["exit-code"].(float64) != 3 {
		t.Fatalf("synthesized outputs = %v", outputs)
	}
	if narrative := readFileForTest(t, phases[0].NarrativePath); !strings.Contains(narrative, "broken") {
		t.Fatalf("stderr missing from narrative:\n%s", narrative)
	}
}

func TestWorkflowToolPhaseWrittenEnvelopeFeedsTheNextPhaseCommand(t *testing.T) {
	fixture := newToolWorkflowFixture(t, `
  - id: probe
    driver: tool
    check: probe
    outputs:
      report:
        schema:
          type: string
    gate:
      routes:
        - to: record
  - id: record
    driver: tool
    command: record
    inputs:
      probe.report:
        schema:
          type: string
    gate:
      routes:
        - to: done`)
	recorded := filepath.Join(t.TempDir(), "recorded.txt")
	fixture.writeProfile(t, map[string][]string{
		"probe": {writeExecutable(t, "probe.sh", "#!/bin/sh\n"+
			`printf '%s' '{"status":"done","outputs":{"report":"green-42"},"question":null,"reason":null}' > "$AO_ENVELOPE"`+"\n")},
	}, map[string][]string{
		"record": {writeExecutable(t, "record.sh", "#!/bin/sh\nprintf '%s' \"$1\" > "+recorded+"\n"), "{{probe.report}}"},
	}, "")
	item := fixture.start(t, "written envelope")

	waitForWorkflowItem(t, fixture.app, item.ID, engine.StateDone, "")
	phases := listWorkflowPhases(t, fixture.app, item.ID)
	if len(phases) != 2 {
		t.Fatalf("phases = %+v", phases)
	}
	outputs := decodeEnvelopeOutputs(t, phases[0].OutputEnvelope)
	if outputs["report"] != "green-42" {
		t.Fatalf("written outputs = %v", outputs)
	}
	// The command cannot know its own exit status while writing the envelope,
	// so the system always owns these two.
	if outputs["passed"] != true || outputs["exit-code"].(float64) != 0 {
		t.Fatalf("system outputs missing from written envelope = %v", outputs)
	}
	if got := readFileForTest(t, recorded); got != "green-42" {
		t.Fatalf("second phase argv interpolation = %q", got)
	}
	if narrative := readFileForTest(t, phases[0].NarrativePath); !strings.Contains(narrative, "written by the command") {
		t.Fatalf("narrative did not record the envelope source:\n%s", narrative)
	}
}

// The envelope schema permits `narrative` on every status, and post-validation
// is written once against the contract for both drivers — so a command may write
// one, and it is folded into the same narrative file the process output goes to
// rather than being refused by a second rule set. The engine still sees no prose.
func TestWorkflowToolPhaseFoldsAWrittenNarrativeIntoTheAttemptFile(t *testing.T) {
	fixture := newToolWorkflowFixture(t, `
  - id: probe
    driver: tool
    check: probe
    outputs:
      report:
        schema:
          type: string
    gate:
      routes:
        - to: done`)
	fixture.writeProfile(t, map[string][]string{
		"probe": {writeExecutable(t, "probe.sh", "#!/bin/sh\n"+
			`echo "scanning three modules"`+"\n"+
			`printf '%s' '{"status":"done","outputs":{"report":"green-42"},"question":null,"reason":null,`+
			`"narrative":"I scanned three modules and all of them resolved."}' > "$AO_ENVELOPE"`+"\n")},
	}, nil, "")
	item := fixture.start(t, "tool narrative")

	waitForWorkflowItem(t, fixture.app, item.ID, engine.StateDone, "")
	phases := listWorkflowPhases(t, fixture.app, item.ID)
	if len(phases) != 1 {
		t.Fatalf("phases = %+v", phases)
	}
	persisted := string(phases[0].OutputEnvelope)
	if strings.Contains(persisted, "narrative") || strings.Contains(persisted, "I scanned three modules") {
		t.Fatalf("the persisted envelope carried prose: %s", persisted)
	}
	if outputs := decodeEnvelopeOutputs(t, phases[0].OutputEnvelope); outputs["report"] != "green-42" {
		t.Fatalf("stripping damaged the outputs = %v", outputs)
	}
	narrative := readFileForTest(t, phases[0].NarrativePath)
	account := strings.Index(narrative, "I scanned three modules and all of them resolved.")
	output := strings.Index(narrative, "scanning three modules")
	if account < 0 || output < 0 || account > output {
		t.Fatalf("the command's account must lead its output tail:\n%s", narrative)
	}
}

func TestWorkflowToolPhaseInvalidWrittenEnvelopeParksWithoutRetrying(t *testing.T) {
	for _, testCase := range []struct {
		name            string
		body            string
		wantFinding     string
		wantPersistence bool
	}{
		{
			name:        "unparseable",
			body:        "not json at all",
			wantFinding: "invalid JSON",
		},
		{
			name:            "branch rules",
			body:            `{"status":"done","outputs":{},"question":"why?","reason":null}`,
			wantFinding:     "$.question",
			wantPersistence: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newToolWorkflowFixture(t, `
  - id: check
    driver: tool
    check: verify
    gate:
      routes:
        - to: done`)
			fixture.writeProfile(t, map[string][]string{
				"verify": {writeExecutable(t, "invalid.sh", "#!/bin/sh\nprintf '%s' '"+testCase.body+"' > \"$AO_ENVELOPE\"\n")},
			}, nil, "")
			item := fixture.start(t, "invalid envelope")

			// A deterministic command gets no feedback turn, so there is
			// exactly one attempt and it parks.
			waitForWorkflowItem(t, fixture.app, item.ID, engine.StateNeedsHuman, engine.ReasonAgentError)
			phases := listWorkflowPhases(t, fixture.app, item.ID)
			if len(phases) != 1 || phases[0].Attempt != 1 || phases[0].Status != "parked" {
				t.Fatalf("phases = %+v", phases)
			}
			if persisted := len(phases[0].OutputEnvelope) > 0; persisted != testCase.wantPersistence {
				t.Fatalf("persisted partial envelope = %v (%s), want %v", persisted, phases[0].OutputEnvelope, testCase.wantPersistence)
			}
			narrative := readFileForTest(t, phases[0].NarrativePath)
			if !strings.Contains(narrative, "Envelope validation failed") || !strings.Contains(narrative, testCase.wantFinding) {
				t.Fatalf("narrative did not record the findings:\n%s", narrative)
			}
		})
	}
}

func TestWorkflowToolPhaseWithoutWrittenEnvelopeParksWhenOutputsAreDeclared(t *testing.T) {
	fixture := newToolWorkflowFixture(t, `
  - id: check
    driver: tool
    check: verify
    outputs:
      report:
        schema:
          type: string
    gate:
      routes:
        - to: done`)
	fixture.writeProfile(t, map[string][]string{
		"verify": {writeExecutable(t, "silent.sh", "#!/bin/sh\nexit 0\n")},
	}, nil, "")
	item := fixture.start(t, "missing outputs")

	waitForWorkflowItem(t, fixture.app, item.ID, engine.StateNeedsHuman, engine.ReasonAgentError)
	phases := listWorkflowPhases(t, fixture.app, item.ID)
	narrative := readFileForTest(t, phases[0].NarrativePath)
	for _, want := range []string{"$.outputs.report", "AO_ENVELOPE"} {
		if !strings.Contains(narrative, want) {
			t.Fatalf("narrative missing %q:\n%s", want, narrative)
		}
	}
}

func TestWorkflowToolPhaseMissingBinaryFailsAsSetup(t *testing.T) {
	fixture := newToolWorkflowFixture(t, `
  - id: check
    driver: tool
    check: verify
    gate:
      routes:
        - to: done`)
	fixture.writeProfile(t, map[string][]string{
		"verify": {filepath.Join(t.TempDir(), "does-not-exist")},
	}, nil, "")
	item := fixture.start(t, "missing binary")

	waitForWorkflowItem(t, fixture.app, item.ID, engine.StateNeedsHuman, engine.ReasonSetupFailed)
	phases := listWorkflowPhases(t, fixture.app, item.ID)
	if narrative := readFileForTest(t, phases[0].NarrativePath); !strings.Contains(narrative, "could not be started") {
		t.Fatalf("narrative did not explain the start failure:\n%s", narrative)
	}
}

// A phase resolves its command from the profile as it is at phase start, so a
// binding removed while the run is held parks with the wiring reason rather
// than reporting an agent failure.
func TestWorkflowToolPhaseUnboundCheckParksAsWiringError(t *testing.T) {
	fixture := newToolWorkflowFixture(t, `
  - id: check
    driver: tool
    check: verify
    gate:
      routes:
        - to: done`)
	fixture.writeProfile(t, map[string][]string{
		"verify": {writeExecutable(t, "green.sh", "#!/bin/sh\nexit 0\n")},
	}, nil, "")
	if err := fixture.app.WorkflowSetGlobalPause(true); err != nil {
		t.Fatal(err)
	}
	item := fixture.start(t, "unbound check")
	fixture.writeProfile(t, map[string][]string{"other": {"/usr/bin/true"}}, nil, "")
	if err := fixture.app.WorkflowSetGlobalPause(false); err != nil {
		t.Fatal(err)
	}

	waitForWorkflowItem(t, fixture.app, item.ID, engine.StateNeedsHuman, engine.ReasonWiringError)
}

// Secrets reach the command as environment variables and never reach the run
// record: the narrative is untrusted command output.
func TestWorkflowToolPhaseInjectsAndMasksProfileSecrets(t *testing.T) {
	t.Setenv("AO_TEST_TOOL_SECRET", "s3cr3t-value")
	fixture := newToolWorkflowFixture(t, `
  - id: check
    driver: tool
    check: verify
    gate:
      routes:
        - to: done`)
	fixture.writeProfile(t, map[string][]string{
		"verify": {writeExecutable(t, "echo-secret.sh", "#!/bin/sh\necho \"token=$DEPLOY_TOKEN\"\nexit 0\n")},
	}, nil, "secrets:\n  deploy-token:\n    source: env\n    env: AO_TEST_TOOL_SECRET\n")
	item := fixture.start(t, "secrets")

	waitForWorkflowItem(t, fixture.app, item.ID, engine.StateDone, "")
	phases := listWorkflowPhases(t, fixture.app, item.ID)
	narrative := readFileForTest(t, phases[0].NarrativePath)
	if strings.Contains(narrative, "s3cr3t-value") {
		t.Fatalf("resolved secret landed in the narrative:\n%s", narrative)
	}
	if !strings.Contains(narrative, "token=[redacted]") {
		t.Fatalf("secret was not injected into the command environment:\n%s", narrative)
	}
}

// --- fixture -----------------------------------------------------------------

type toolWorkflowFixture struct {
	app        *App
	configRoot string
	project    store.Project
}

// newToolWorkflowFixture installs a shared workflow definition and boots the
// engine against an isolated config root. Phases are authored YAML so the tests
// exercise the same parse/validate/freeze path production uses.
func newToolWorkflowFixture(t *testing.T, phases string) *toolWorkflowFixture {
	t.Helper()
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	dir := filepath.Join(configRoot, "workflows")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	definition := "id: tool-flow\nname: Tool flow\nphases:" + phases + "\ncleanup: manual\n"
	if err := os.WriteFile(filepath.Join(dir, "tool-flow.yaml"), []byte(definition), 0o600); err != nil {
		t.Fatal(err)
	}
	projectRow := mustReloadProject(t, app.store, testutil.EnsureProject(t, app.store, t.TempDir()).ID)
	return &toolWorkflowFixture{app: app, configRoot: configRoot, project: projectRow}
}

// writeProfile writes the project profile and (on first call) starts the
// engine. Later calls rewrite it in place, which is how a test exercises the
// live-profile read every phase start performs.
func (f *toolWorkflowFixture) writeProfile(t *testing.T, checks, commands map[string][]string, extra string) {
	t.Helper()
	body := "checks:\n" + renderProfileArgvMap(t, checks)
	if len(commands) > 0 {
		body += "commands:\n" + renderProfileArgvMap(t, commands)
	}
	body += extra
	dir := filepath.Join(f.configRoot, "projects", f.project.Slug)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "profile.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if f.app.workflowApplication().Engine() == nil {
		startWorkflowEngineForTest(t, f.app, f.configRoot)
	}
}

func renderProfileArgvMap(t *testing.T, values map[string][]string) string {
	t.Helper()
	var body strings.Builder
	for name, argv := range values {
		encoded, err := json.Marshal(argv)
		if err != nil {
			t.Fatal(err)
		}
		body.WriteString("  " + name + ": " + string(encoded) + "\n")
	}
	return body.String()
}

func (f *toolWorkflowFixture) start(t *testing.T, goal string) store.WorkItem {
	t.Helper()
	item, err := f.app.WorkflowStartRun(
		f.project.ID, "tool-flow", "shared", goal, json.RawMessage(`{}`), nil, "", false,
	)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func listWorkflowPhases(t *testing.T, app *App, itemID string) []store.WorkItemPhase {
	t.Helper()
	phases, err := app.store.ListWorkItemPhases(itemID)
	if err != nil {
		t.Fatal(err)
	}
	return phases
}

func decodeEnvelopeOutputs(t *testing.T, payload json.RawMessage) map[string]any {
	t.Helper()
	var envelope struct {
		Outputs map[string]any `json:"outputs"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("decode envelope %s: %v", payload, err)
	}
	return envelope.Outputs
}

// Human recovery of one fan-out unit: a failed unit parks the attempt with its
// siblings' results intact, and retry / drop / takeover repair that attempt in
// place rather than replacing it. The fixtures these use live in
// app_workflow_fanout_test.go.

// TestWorkflowUnitFailureParksAndRetryCompletesTheRun covers the recovery loop a
// human drives: one unit fails, the attempt parks with its siblings' results
// intact, WorkflowRetryUnit re-runs only the failed unit on a fresh try, and the
// join then produces the phase's envelope.
func TestWorkflowUnitFailureParksAndRetryCompletesTheRun(t *testing.T) {
	app, item, repo := startFailingFanOutRun(t, "BETA-UNIT")
	item = waitForWorkflowItem(t, app, item.ID, engine.StateNeedsHuman, engine.ReasonUnitFailed)

	units := unitsByID(t, app, item.ID)
	if units["alpha"].Status != store.WorkItemUnitDone || units["beta"].Status != store.WorkItemUnitFailed {
		t.Fatalf("unit statuses = alpha:%q beta:%q", units["alpha"].Status, units["beta"].Status)
	}
	if units["merge"].Status != store.WorkItemUnitPending {
		t.Fatalf("join status = %q, want pending — it must not run over a failed unit", units["merge"].Status)
	}
	// The failed unit keeps its worktree and its branch: the work is evidence a
	// human retries or salvages from.
	if units["beta"].WorktreePath == "" || units["beta"].Branch == "" {
		t.Fatalf("failed unit lost its isolation: %+v", units["beta"])
	}
	if _, err := os.Stat(units["beta"].WorktreePath); err != nil {
		t.Fatalf("failed unit worktree %q: %v", units["beta"].WorktreePath, err)
	}
	failedWorktree := units["beta"].WorktreePath
	t.Cleanup(func() { _ = app.gitCore().RemoveWorktreeForce(repo, failedWorktree, true) })

	// A failed unit's thread stays inspectable but is not steerable: takeover
	// detaches a LIVE unit, and this one has already ended. The recovery actions
	// for a settled unit are retry and drop, and the refusal says so rather than
	// accepting a send nothing would consume.
	if items, itemsErr := app.store.ListItems(units["beta"].ThreadID); itemsErr != nil || len(items) == 0 {
		t.Fatalf("failed unit thread is not inspectable: %d items, %v", len(items), itemsErr)
	}
	if err := app.SendMessage(units["beta"].ThreadID, "let me fix this myself", nil); err == nil ||
		!strings.Contains(err.Error(), "only a running unit can be taken over") {
		t.Fatalf("send into a failed unit thread = %v, want a takeover refusal", err)
	}
	if err := app.WorkflowTakeOverUnit(item.ID, "beta"); err == nil {
		t.Fatal("WorkflowTakeOverUnit accepted a unit whose run already ended")
	}

	if err := app.WorkflowRetryUnit(context.Background(), item.ID, "beta", "the merge script is fixed"); err != nil {
		t.Fatal(err)
	}
	item = waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")
	t.Cleanup(func() { _ = app.gitCore().RemoveWorktreeForce(repo, item.WorktreePath, true) })

	units = unitsByID(t, app, item.ID)
	for _, id := range []string{"alpha", "beta", "merge"} {
		if units[id].Status != store.WorkItemUnitDone {
			t.Fatalf("after retry unit %q status = %q", id, units[id].Status)
		}
	}
	// The retried unit reused its row and counted a second try, and its second try
	// was cut fresh from the item's branch rather than inheriting the failed one.
	if units["beta"].UnitAttempt != 2 {
		t.Fatalf("retried unit try = %d, want 2", units["beta"].UnitAttempt)
	}
	if want := workflowhost.UnitBranch(item.Branch, workflowhost.UnitWorkspaceRef{
		ItemID: item.ID, PhaseID: "port", Attempt: 1, UnitID: "beta", UnitAttempt: 2,
	}); units["beta"].Branch != want {
		t.Fatalf("retried unit branch = %q, want %q", units["beta"].Branch, want)
	}
	if units["alpha"].UnitAttempt != 1 {
		t.Fatalf("a sibling was re-run: alpha try = %d", units["alpha"].UnitAttempt)
	}
	phases, err := app.store.ListWorkItemPhases(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(phases) != 1 || phases[0].Attempt != 1 {
		t.Fatalf("repairing a unit replaced the Attempt: %+v", phases)
	}
}

// TestWorkflowDropUnitLetsTheJoinProceedOverSurvivors is the other half of unit
// recovery: the human accepts the unit's absence and the attempt finishes
// without it.
func TestWorkflowDropUnitLetsTheJoinProceedOverSurvivors(t *testing.T) {
	app, item, repo := startFailingFanOutRun(t, "BETA-UNIT")
	item = waitForWorkflowItem(t, app, item.ID, engine.StateNeedsHuman, engine.ReasonUnitFailed)

	if err := app.WorkflowDropUnit(item.ID, "beta", "not needed for this port"); err != nil {
		t.Fatal(err)
	}
	item = waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")
	t.Cleanup(func() { _ = app.gitCore().RemoveWorktreeForce(repo, item.WorktreePath, true) })

	units := unitsByID(t, app, item.ID)
	if units["beta"].Status != store.WorkItemUnitDropped {
		t.Fatalf("dropped unit status = %q", units["beta"].Status)
	}
	if units["merge"].Status != store.WorkItemUnitDone {
		t.Fatalf("join status = %q, want done over the survivors", units["merge"].Status)
	}
	// The attempt ended on a done join, so every unit checkout is retired —
	// including the dropped one, whose branch is what preserves whatever it
	// committed before the human let it go.
	if units["beta"].WorktreePath != "" {
		t.Fatalf("dropped unit still records worktree %q after a done join", units["beta"].WorktreePath)
	}
	branches := map[string]bool{}
	for _, branch := range mustListBranches(t, app, repo) {
		branches[branch.Name] = true
	}
	if !branches[units["beta"].Branch] {
		t.Fatalf("dropped unit branch %q was removed with its worktree", units["beta"].Branch)
	}
}

// TestWorkflowRetryFailedUnitsRepairsEveryFailedUnitAtOnce is the many-at-once
// half of unit recovery: one cause — a provider usage limit — fails every unit
// of the attempt, and one call puts all of them back on the same attempt. The
// per-unit outcome is the same as calling WorkflowRetryUnit on each: a fresh
// try on a fresh branch cut from the item's, on the attempt row that already
// exists.
func TestWorkflowRetryFailedUnitsRepairsEveryFailedUnitAtOnce(t *testing.T) {
	app, item, repo := startFailingFanOutRun(t, "ALPHA-UNIT", "BETA-UNIT")
	item = waitForWorkflowItem(t, app, item.ID, engine.StateNeedsHuman, engine.ReasonUnitFailed)

	units := unitsByID(t, app, item.ID)
	for _, id := range []string{"alpha", "beta"} {
		if units[id].Status != store.WorkItemUnitFailed {
			t.Fatalf("unit %q status = %q, want every unit failed", id, units[id].Status)
		}
	}
	if units["merge"].Status != store.WorkItemUnitPending {
		t.Fatalf("join status = %q, want pending over a wholly failed attempt", units["merge"].Status)
	}
	for _, id := range []string{"alpha", "beta"} {
		failedWorktree := units[id].WorktreePath
		if failedWorktree == "" {
			t.Fatalf("failed unit %q lost its isolation: %+v", id, units[id])
		}
		t.Cleanup(func() { _ = app.gitCore().RemoveWorktreeForce(repo, failedWorktree, true) })
	}

	if err := app.WorkflowRetryFailedUnits(context.Background(), item.ID, "the usage limit reset"); err != nil {
		t.Fatal(err)
	}
	item = waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")
	t.Cleanup(func() { _ = app.gitCore().RemoveWorktreeForce(repo, item.WorktreePath, true) })

	units = unitsByID(t, app, item.ID)
	for _, id := range []string{"alpha", "beta", "merge"} {
		if units[id].Status != store.WorkItemUnitDone {
			t.Fatalf("after retry-all unit %q status = %q", id, units[id].Status)
		}
	}
	for _, id := range []string{"alpha", "beta"} {
		if units[id].UnitAttempt != 2 {
			t.Fatalf("repaired unit %q try = %d, want 2", id, units[id].UnitAttempt)
		}
		if want := workflowhost.UnitBranch(item.Branch, workflowhost.UnitWorkspaceRef{
			ItemID: item.ID, PhaseID: "port", Attempt: 1, UnitID: id, UnitAttempt: 2,
		}); units[id].Branch != want {
			t.Fatalf("repaired unit %q branch = %q, want %q", id, units[id].Branch, want)
		}
	}
	phases, err := app.store.ListWorkItemPhases(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(phases) != 1 || phases[0].Attempt != 1 {
		t.Fatalf("repairing every unit replaced the Attempt: %+v", phases)
	}
}

// A run with nothing failed is refused rather than quietly resumed: the CLI
// caller that mistimed a usage-reset poll has to see that its repair did not
// happen.
func TestWorkflowRetryFailedUnitsRefusesARunWithNothingFailed(t *testing.T) {
	app, item, repo := startFailingFanOutRun(t)
	item = waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")
	t.Cleanup(func() { _ = app.gitCore().RemoveWorktreeForce(repo, item.WorktreePath, true) })

	if err := app.WorkflowRetryFailedUnits(context.Background(), item.ID, ""); err == nil {
		t.Fatal("WorkflowRetryFailedUnits accepted a run that has nothing to repair")
	}
}

// startFailingFanOutRun boots a fan-out run whose named units fail on their
// first try and succeed on any later one, and returns once the run has started.
// The tokens are the unit markers in the prompt bodies (`ALPHA-UNIT`,
// `BETA-UNIT`), so a caller picks between the one-unit park and the
// many-at-once park a usage limit produces.
func startFailingFanOutRun(t *testing.T, failFirstTurn ...string) (*App, store.WorkItem, string) {
	t.Helper()
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
		"claudeBinaryPath": writeFanOutClaude(t, cwdLog, t.TempDir(), failFirstTurn...),
	}); err != nil {
		t.Fatal(err)
	}
	startWorkflowEngineForTest(t, app, configRoot)

	item, err := app.WorkflowStartRun(projectRow.ID, "fanout-flow", "shared", "port it", json.RawMessage(`{}`), nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	return app, item, repo
}

// TestWorkflowTakeOverLiveUnitSteersThenRetriesUnderEngineControl covers the
// live-unit takeover path end to end: one unit is detached mid-turn for human
// steering while its siblings finish, the human's send runs schema-less on the
// unit's own thread, and the run completes once the unit is handed back through
// a retry.
func TestWorkflowTakeOverLiveUnitSteersThenRetriesUnderEngineControl(t *testing.T) {
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
	if _, err := app.settings.Update(map[string]any{
		"claudeBinaryPath": writeFanOutTakeoverClaude(t, filepath.Join(t.TempDir(), "beta-hung")),
	}); err != nil {
		t.Fatal(err)
	}
	startWorkflowEngineForTest(t, app, configRoot)

	item, err := app.WorkflowStartRun(projectRow.ID, "fanout-flow", "shared", "port it", json.RawMessage(`{}`), nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.gitCore().RemoveWorktreeForce(repo, item.WorktreePath, true) })
	threadID := waitForRunningWorkflowUnitThread(t, app, item.ID, "beta")

	if err := app.WorkflowTakeOverUnit(item.ID, "beta"); err != nil {
		t.Fatal(err)
	}
	item = waitForWorkflowItem(t, app, item.ID, engine.StateNeedsHuman, engine.ReasonTakenOver)
	units := unitsByID(t, app, item.ID)
	if units["beta"].Status != store.WorkItemUnitTakenOver {
		t.Fatalf("taken-over unit status = %q", units["beta"].Status)
	}
	// Its siblings were left alone: taking over one unit is not taking over the
	// attempt, and the join still has not run.
	if units["alpha"].Status != store.WorkItemUnitDone || units["merge"].Status != store.WorkItemUnitPending {
		t.Fatalf("takeover disturbed the Attempt: alpha=%q merge=%q", units["alpha"].Status, units["merge"].Status)
	}
	if units["beta"].ThreadID != threadID {
		t.Fatalf("unit thread changed on takeover: %q vs %q", units["beta"].ThreadID, threadID)
	}

	steered := make(chan struct{}, 1)
	unsubscribe := app.subscribeThreadTurnObserver(threadID, func(_ string, event provider.ProviderEvent) {
		if event.Kind == provider.EventTurnComplete {
			select {
			case steered <- struct{}{}:
			default:
			}
		}
	})
	if err := app.SendMessage(threadID, "I unblocked the beta slice by hand.", nil); err != nil {
		unsubscribe()
		t.Fatal(err)
	}
	select {
	case <-steered:
	case <-time.After(8 * time.Second):
		unsubscribe()
		t.Fatal("schema-less steering turn on the unit thread did not complete")
	}
	unsubscribe()

	if err := app.WorkflowRetryUnit(context.Background(), item.ID, "beta", "steered by hand; run it again"); err != nil {
		t.Fatal(err)
	}
	item = waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")
	units = unitsByID(t, app, item.ID)
	for _, id := range []string{"alpha", "beta", "merge"} {
		if units[id].Status != store.WorkItemUnitDone {
			t.Fatalf("after handing the unit back, %q status = %q", id, units[id].Status)
		}
	}
	// The retry ran on a new thread under engine control; the steered one stays
	// as the record of what the human did.
	if units["beta"].ThreadID == threadID {
		t.Fatal("the retried try reused the steering thread")
	}
	if units["beta"].UnitAttempt != 2 {
		t.Fatalf("retried unit try = %d, want 2", units["beta"].UnitAttempt)
	}
	steeredThread, err := app.store.GetThread(threadID)
	if err != nil {
		t.Fatal(err)
	}
	if steeredThread.Mode != "workflow" {
		t.Fatalf("steered unit thread mode = %q", steeredThread.Mode)
	}
	items, err := app.store.ListItems(threadID)
	if err != nil {
		t.Fatal(err)
	}
	var userMessages []string
	for _, timelineItem := range items {
		if timelineItem.Kind == "user_text" && timelineItem.Role == "user" {
			userMessages = append(userMessages, timelineItem.Summary)
		}
	}
	if len(userMessages) != 2 || !strings.Contains(userMessages[1], "I unblocked the beta slice") {
		t.Fatalf("steering turns on the unit thread = %#v", userMessages)
	}
}

func waitForRunningWorkflowUnitThread(t *testing.T, app *App, itemID, unitID string) string {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		units, err := app.store.ListWorkItemUnits(itemID)
		if err != nil {
			t.Fatal(err)
		}
		for _, unit := range units {
			if unit.UnitID != unitID || unit.Status != store.WorkItemUnitRunning || unit.ThreadID == "" {
				continue
			}
			if _, active, activeErr := app.store.GetActiveTurn(unit.ThreadID); activeErr == nil && active {
				return unit.ThreadID
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("unit %q of item %s never exposed a live thread", unitID, itemID)
	return ""
}

// writeFanOutTakeoverClaude hangs the beta unit's first turn so it can be taken
// over mid-flight, answers the interrupt the takeover sends, runs the human's
// steering turn without a schema, and completes every workflow turn after that.
func writeFanOutTakeoverClaude(t *testing.T, hangMarker string) string {
	t.Helper()
	done := `{"status":"done","outputs":{"report":"ok"},"question":null,"reason":null}`
	script := `#!/bin/bash
while IFS= read -r line; do
  case "$line" in
    *'"subtype":"interrupt"'*)
      reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
      printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"
      printf '%s\n' '{"type":"result","subtype":"error_during_execution","is_error":true,"terminal_reason":"aborted_streaming"}'
      continue
      ;;
  esac
  printf '%s\n' '{"type":"system","subtype":"init","session_id":"fanout-takeover","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}'
  if [[ "$line" == *BETA-UNIT* && ! -f ` + workflowShellQuote(hangMarker) + ` ]]; then
    : > ` + workflowShellQuote(hangMarker) + `
    continue
  fi
  if [[ "$line" == *workflow-system-instructions* ]]; then
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":` + done + `}'
    continue
  fi
  printf '%s\n' '{"type":"result","subtype":"success","is_error":false}'
done
`
	return writeExecutable(t, "fanout-takeover-claude.sh", script)
}
