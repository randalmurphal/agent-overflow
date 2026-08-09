package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/workflow/engine"
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
	if f.app.workflowEngine == nil {
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
