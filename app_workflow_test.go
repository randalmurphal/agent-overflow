package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/profile"
)

func TestWorkflowBindingRunsGatesQuestionsAndEnvelopeRetry(t *testing.T) {
	app, bus := setupE2EApp(t)
	app.testEmitHook = bus.emit
	configRoot := t.TempDir()
	writeWorkflowFixture(t, configRoot)

	phaseOneBinary := writeTwoPhaseWorkflowClaude(t)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": phaseOneBinary}); err != nil {
		t.Fatal(err)
	}
	if err := app.initWorkflowEngine(configRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if app.workflowEngine != nil {
			_ = app.workflowEngine.Close()
		}
	})

	projectRow := testutil.EnsureProject(t, app.store, t.TempDir())
	budgetTokens := int64(1_000_000)
	item, err := app.WorkflowStartRun(
		projectRow.ID, "packet-flow", "shared", "exercise workflow",
		json.RawMessage(`{"goal":"exercise workflow"}`), &profile.Budget{Tokens: &budgetTokens}, "", false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(item.Budget) == 0 {
		t.Fatal("start did not persist the optional item budget")
	}
	waitForWorkflowItem(t, app, item.ID, engine.StateNeedsHuman, engine.ReasonQuestion)

	detail, err := app.WorkflowGetItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Phases) != 2 || detail.Phases[0].PhaseID != "prepare" || detail.Phases[0].Status != "completed" ||
		detail.Phases[1].PhaseID != "finish" || detail.Phases[1].Status != "parked" {
		t.Fatalf("gate phase detail = %+v", detail)
	}
	persistedGatePhases, err := app.store.ListWorkItemPhases(item.ID)
	if err != nil || len(persistedGatePhases) != 2 || len(persistedGatePhases[0].GateTrace) == 0 {
		t.Fatalf("persisted gate phase trace = %+v, %v", persistedGatePhases, err)
	}
	questionDetail, err := app.WorkflowGetItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(questionDetail.Phases) != 2 || questionDetail.Phases[1].PhaseID != "finish" || questionDetail.Phases[1].ThreadID == "" {
		t.Fatalf("question detail = %+v", questionDetail)
	}
	firstThread, err := app.store.GetThread(questionDetail.Phases[0].ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if firstThread.WorkspacePath != projectRow.Path || firstThread.WorktreePath != "" {
		t.Fatalf("read-only workflow thread workspace = %+v, want project root %q", firstThread, projectRow.Path)
	}
	if questionDetail.Phases[0].ThreadID == questionDetail.Phases[1].ThreadID {
		t.Fatalf("ordinary phase transition reused thread %q", questionDetail.Phases[0].ThreadID)
	}
	questionThreadID := questionDetail.Phases[1].ThreadID

	if err := app.WorkflowAnswerQuestion(item.ID, "Use option A"); err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")
	completed, err := app.WorkflowGetItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(completed.Phases) != 3 || completed.Phases[2].PhaseID != "finish" || completed.Phases[2].Attempt != 2 ||
		completed.Phases[2].ThreadID != questionThreadID || completed.Phases[2].Status != "completed" {
		t.Fatalf("completed phase detail = %+v", completed)
	}
	persistedItem, err := app.store.GetWorkItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persistedItem.Snapshot), "prepare.md") || !strings.Contains(string(persistedItem.Snapshot), "Prepare the goal") {
		t.Fatalf("snapshot did not freeze inlined prompt bodies: %s", persistedItem.Snapshot)
	}
	persistedPhases, err := app.store.ListWorkItemPhases(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range persistedPhases {
		if !filepath.IsAbs(phase.NarrativePath) || !strings.Contains(phase.NarrativePath, filepath.Join("workflow-runs", item.ID)) {
			t.Fatalf("phase narrative path = %q", phase.NarrativePath)
		}
	}

	invalidBinary := testutil.WriteMockClaudeScript(t, t.TempDir(), [][]string{
		{
			`{"type":"system","subtype":"init","session_id":"invalid","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}`,
			`{"type":"result","subtype":"success","is_error":false}`,
		},
		{
			`{"type":"system","subtype":"init","session_id":"invalid","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}`,
			`{"type":"result","subtype":"success","is_error":false,"structured_output":{"status":"done"}}`,
		},
	})
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": invalidBinary}); err != nil {
		t.Fatal(err)
	}
	invalidItem, err := app.WorkflowStartRun(
		projectRow.ID, "packet-flow", "shared", "invalid envelope",
		json.RawMessage(`{"goal":"invalid envelope"}`), nil, "", false,
	)
	if err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, app, invalidItem.ID, engine.StateNeedsHuman, engine.ReasonAgentError)
	invalidDetail, err := app.WorkflowGetItem(invalidItem.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(invalidDetail.Phases) != 1 || invalidDetail.Phases[0].Status != "parked" {
		t.Fatalf("invalid envelope detail = %+v", invalidDetail)
	}
	threadItems, err := app.store.ListItems(invalidDetail.Phases[0].ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	userTurns := 0
	for _, timelineItem := range threadItems {
		if timelineItem.Role == "user" && timelineItem.Kind == "user_text" {
			userTurns++
		}
	}
	if userTurns != 2 {
		t.Fatalf("invalid envelope user turns = %d, want initial + one retry; items=%+v", userTurns, threadItems)
	}
	if err := app.WorkflowSetGlobalPause(true); err != nil {
		t.Fatal(err)
	}
	if !app.currentSettings().WorkflowPaused {
		t.Fatal("global pause was not persisted to settings")
	}
	if state, err := app.WorkflowGetEngineState(); err != nil || !state.Paused {
		t.Fatalf("engine state = %+v, %v, want paused", state, err)
	}
	if err := app.WorkflowSetGlobalPause(false); err != nil {
		t.Fatal(err)
	}
	if app.currentSettings().WorkflowPaused {
		t.Fatal("unpause was not persisted to settings")
	}
	items, err := app.WorkflowListItems(projectRow.ID)
	if err != nil || len(items) != 2 {
		t.Fatalf("WorkflowListItems = %+v, %v", items, err)
	}

	listedThreads, err := app.ListThreads()
	if err != nil {
		t.Fatal(err)
	}
	for _, thread := range listedThreads {
		if thread.Mode == "workflow" {
			t.Fatalf("workflow thread leaked into normal listing: %+v", thread)
		}
	}
	assertWorkflowEmissions(t, bus, item.ID)
}

func TestWorkflowCodexQuestionAnswerCarriesSchemaEveryTurn(t *testing.T) {
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	workflowDir := filepath.Join(configRoot, "workflows")
	if err := os.MkdirAll(workflowDir, 0o700); err != nil {
		t.Fatal(err)
	}
	definition := `id: codex-flow
name: Codex flow
inputs:
  goal:
    schema:
      type: string
phases:
  - id: execute
    driver: agent
    provider: codex
    model: gpt-5
    prompt: execute.md
    inputs:
      goal:
        schema:
          type: string
    outputs:
      complete:
        schema:
          type: boolean
    gate:
      routes:
        - to: done
cleanup: manual
`
	if err := os.WriteFile(filepath.Join(workflowDir, "codex-flow.yaml"), []byte(definition), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workflowDir, "execute.md"), []byte("Execute {{goal}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	capturePath := filepath.Join(t.TempDir(), "turn-starts.ndjson")
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": writeWorkflowCodex(t, capturePath)}); err != nil {
		t.Fatal(err)
	}
	if err := app.initWorkflowEngine(configRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if app.workflowEngine != nil {
			_ = app.workflowEngine.Close()
		}
	})

	projectRow := testutil.EnsureProject(t, app.store, t.TempDir())
	item, err := app.WorkflowStartRun(
		projectRow.ID, "codex-flow", "shared", "check codex schema",
		json.RawMessage(`{"goal":"check codex schema"}`), nil, "", false,
	)
	if err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, app, item.ID, engine.StateNeedsHuman, engine.ReasonQuestion)
	question, err := app.WorkflowGetItem(item.ID)
	if err != nil || len(question.Phases) != 1 {
		t.Fatalf("question detail = %+v, %v", question, err)
	}
	if err := app.WorkflowAnswerQuestion(item.ID, "continue"); err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")
	done, err := app.WorkflowGetItem(item.ID)
	if err != nil || len(done.Phases) != 2 || done.Phases[1].ThreadID != question.Phases[0].ThreadID {
		t.Fatalf("answered detail = %+v, %v", done, err)
	}

	captured, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatal(err)
	}
	var turns []struct {
		Params struct {
			ThreadID     string          `json:"threadId"`
			OutputSchema json.RawMessage `json:"outputSchema"`
		} `json:"params"`
	}
	for _, line := range strings.Split(strings.TrimSpace(string(captured)), "\n") {
		var turn struct {
			Params struct {
				ThreadID     string          `json:"threadId"`
				OutputSchema json.RawMessage `json:"outputSchema"`
			} `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &turn); err != nil {
			t.Fatalf("decode turn/start: %v\n%s", err, line)
		}
		turns = append(turns, turn)
	}
	if len(turns) != 3 {
		t.Fatalf("turn/start count = %d, want invalid + retry question + answer; capture=%s", len(turns), captured)
	}
	for i, turn := range turns {
		if turn.Params.ThreadID != "workflow-provider-thread" {
			t.Fatalf("provider thread id on turn %d = %q", i+1, turn.Params.ThreadID)
		}
		if len(turn.Params.OutputSchema) == 0 || string(turn.Params.OutputSchema) != string(turns[0].Params.OutputSchema) {
			t.Fatalf("per-turn schema %d = %s, first = %s", i+1, turn.Params.OutputSchema, turns[0].Params.OutputSchema)
		}
	}
}

func TestWorkflowEnvelopeRetryCanRecover(t *testing.T) {
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	workflowDir := filepath.Join(configRoot, "workflows")
	if err := os.MkdirAll(workflowDir, 0o700); err != nil {
		t.Fatal(err)
	}
	definition := `id: retry-flow
name: Retry flow
inputs:
  goal:
    schema:
      type: string
phases:
  - id: retry
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: retry.md
    inputs:
      goal:
        schema:
          type: string
    outputs:
      recovered:
        schema:
          type: boolean
    gate:
      routes:
        - to: done
cleanup: manual
`
	if err := os.WriteFile(filepath.Join(workflowDir, "retry-flow.yaml"), []byte(definition), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workflowDir, "retry.md"), []byte("Retry {{goal}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), [][]string{
		{
			`{"type":"system","subtype":"init","session_id":"retry","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}`,
			`{"type":"result","subtype":"success","is_error":false}`,
		},
		{
			`{"type":"system","subtype":"init","session_id":"retry","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}`,
			`{"type":"result","subtype":"success","is_error":false,"structured_output":{"status":"done","outputs":{"recovered":true},"question":null,"reason":null}}`,
		},
	})
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	if err := app.initWorkflowEngine(configRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if app.workflowEngine != nil {
			_ = app.workflowEngine.Close()
		}
	})
	projectRow := testutil.EnsureProject(t, app.store, t.TempDir())
	item, err := app.WorkflowStartRun(
		projectRow.ID, "retry-flow", "shared", "recover envelope",
		json.RawMessage(`{"goal":"recover envelope"}`), nil, "", false,
	)
	if err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")
	detail, err := app.WorkflowGetItem(item.ID)
	if err != nil || len(detail.Phases) != 1 || detail.Phases[0].Status != "completed" {
		t.Fatalf("recovered detail = %+v, %v", detail, err)
	}
	items, err := app.store.ListItems(detail.Phases[0].ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	userTurns := 0
	for _, item := range items {
		if item.Role == "user" && item.Kind == "user_text" {
			userTurns++
		}
	}
	if userTurns != 2 {
		t.Fatalf("retry recovery user turns = %d, want 2; items=%+v", userTurns, items)
	}
}

func writeWorkflowCodex(t *testing.T, capturePath string) string {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/bash
turn=0
while IFS= read -r line; do
  id=$(/bin/echo "$line" | /usr/bin/grep -o '"id":[0-9]*' | /usr/bin/head -1 | /usr/bin/grep -o '[0-9]*')
  if [ -z "$id" ]; then continue; fi
  if /bin/echo "$line" | /usr/bin/grep -q '"method":"initialize"'; then
    printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
    continue
  fi
  if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/start"'; then
    printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"workflow-provider-thread"}}}\n' "$id"
    continue
  fi
  if /bin/echo "$line" | /usr/bin/grep -q '"method":"turn/start"'; then
    printf '%%s\n' "$line" >> %q
    turn=$((turn+1))
    printf '{"jsonrpc":"2.0","id":%%s,"result":{"turn":{"id":"turn-%%s"}}}\n' "$id" "$turn"
	if [ "$turn" -eq 1 ]; then
	  printf '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"workflow-provider-thread","turn":{"id":"turn-%%s","status":"completed"}}}\n' "$turn"
	  continue
	fi
	if [ "$turn" -eq 2 ]; then
      text='{"status":"question","outputs":null,"question":"Continue?","reason":null}'
    else
      text='{"status":"done","outputs":{"complete":true},"question":null,"reason":null}'
    fi
    escaped=$(/bin/printf '%%s' "$text" | /usr/bin/sed 's/\\/\\\\/g; s/"/\\"/g')
    printf '{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"workflow-provider-thread","turnId":"turn-%%s","item":{"id":"message-%%s","type":"agentMessage","text":"%%s"}}}\n' "$turn" "$turn" "$escaped"
    printf '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"workflow-provider-thread","turn":{"id":"turn-%%s","status":"completed"}}}\n' "$turn"
    continue
  fi
  printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
done
`, capturePath)
	path := filepath.Join(t.TempDir(), "workflow-codex.sh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeWorkflowFixture(t *testing.T, configRoot string) {
	t.Helper()
	workflowDir := filepath.Join(configRoot, "workflows")
	if err := os.MkdirAll(workflowDir, 0o700); err != nil {
		t.Fatal(err)
	}
	workflow := `id: packet-flow
name: Packet flow
inputs:
  goal:
    schema:
      type: string
phases:
  - id: prepare
    name: Prepare
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: prepare.md
    access: read-only
    inputs:
      goal:
        schema:
          type: string
    outputs:
      ready:
        schema:
          type: boolean
    gate:
      routes:
        - to: finish
  - id: finish
    name: Finish
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: finish.md
    access: read-only
    inputs:
      prepare.ready:
        schema:
          type: boolean
    outputs:
      complete:
        schema:
          type: boolean
    gate:
      routes:
        - to: done
cleanup: manual
`
	for name, body := range map[string]string{
		"workflow.yaml": workflow,
		"prepare.md":    "Prepare the goal: {{goal}}",
		"finish.md":     "Finish after preparation: {{prepare.ready}}",
	} {
		if err := os.WriteFile(filepath.Join(workflowDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func writeTwoPhaseWorkflowClaude(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "workflow-claude.sh")
	script := `#!/bin/bash
idx=0
while IFS= read -r line; do
  case "$line" in
    *'"type":"control_request"'*'"subtype":"interrupt"'* | *'"subtype":"interrupt"'*'"type":"control_request"'*)
      reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
      printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"
      continue
      ;;
  esac
  if [[ "$line" == *"Finish after preparation"* ]]; then
    if [[ $idx -eq 0 ]]; then
      printf '%s\n' '{"type":"system","subtype":"init","session_id":"phase-two","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}'
      printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":{"status":"question","outputs":null,"question":"Which option?","reason":null}}'
    else
      printf '%s\n' '{"type":"system","subtype":"init","session_id":"phase-two","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}'
      printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":{"status":"done","outputs":{"complete":true},"question":null,"reason":null}}'
    fi
  else
    printf '%s\n' '{"type":"system","subtype":"init","session_id":"phase-one","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}'
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":{"status":"done","outputs":{"ready":true},"question":null,"reason":null}}'
  fi
  idx=$((idx+1))
done
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitForWorkflowItem(t *testing.T, app *App, itemID string, state engine.State, reason engine.Reason) store.WorkItem {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		item, err := app.store.GetWorkItem(itemID)
		if err == nil && item.State == string(state) && item.Reason == string(reason) {
			return item
		}
		if err == nil && item.State == string(engine.StateNeedsHuman) && item.Reason != string(reason) {
			detail, _ := app.WorkflowGetItem(itemID)
			var timeline []store.Item
			if len(detail.Phases) > 0 {
				timeline, _ = app.store.ListItems(detail.Phases[len(detail.Phases)-1].ThreadID)
			}
			t.Fatalf("item %s parked as %s instead of %s: %+v timeline=%+v", itemID, item.Reason, reason, detail, timeline)
		}
		time.Sleep(10 * time.Millisecond)
	}
	item, err := app.store.GetWorkItem(itemID)
	t.Fatalf("item %s did not reach %s/%s: item=%+v err=%v", itemID, state, reason, item, err)
	return store.WorkItem{}
}

func assertWorkflowEmissions(t *testing.T, bus *capturedEventBus, itemID string) {
	t.Helper()
	bus.mu.Lock()
	events := append([]capturedEvent(nil), bus.all...)
	bus.mu.Unlock()
	seen := map[string]bool{}
	for _, event := range events {
		switch payload := event.Data.(type) {
		case engine.StateEvent:
			if payload.ItemID == itemID {
				seen[event.Name] = true
			}
		case engine.PhaseEvent:
			if payload.ItemID == itemID {
				seen[event.Name] = true
			}
		case engine.EngineState:
			seen[event.Name] = true
		}
	}
	for _, name := range []string{"workflow:item-state", "workflow:phase-state", "workflow:engine-state"} {
		if !seen[name] {
			t.Fatalf("missing workflow emission %q; events=%s", name, summarizeEvents(events))
		}
	}
}

func TestWorkflowSessionRequiresRegisteredSchema(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("workflow-schema")
	thread.Mode = "workflow"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	if _, err := app.workflowSchemaForSession(thread); err == nil {
		t.Fatal("workflow session without runner schema succeeded")
	}
	if _, err := app.UpdateThreadMode(thread.ID, "chat"); err == nil {
		t.Fatal("workflow thread interaction mode was mutable")
	}
	runner := newWorkflowAppRunner(app, t.TempDir(), nil)
	runner.schemas[thread.ID] = json.RawMessage(`{"type":"object"}`)
	app.workflowRunner = runner
	if schema, err := app.workflowSchemaForSession(thread); err != nil || len(schema) == 0 {
		t.Fatalf("registered workflow schema = %s, %v", schema, err)
	}
}

func TestDeadWorkflowSessionCanRegisterSchemaLessTakeover(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("workflow-dead-takeover")
	thread.Mode = "workflow"
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	phase := def.Phase{
		ID: "work", Driver: def.DriverAgent, Provider: "codex", Model: "gpt-5",
		Outputs: map[string]def.Variable{"ok": {Schema: def.JSONSchema{Type: "boolean"}}},
	}
	snapshot, err := json.Marshal(engine.Snapshot{Workflow: def.Workflow{ID: "flow", Phases: []def.Phase{phase}}})
	if err != nil {
		t.Fatal(err)
	}
	item := store.WorkItem{
		ID: "taken-over", ProjectID: defaultTestProjectID, Goal: "goal",
		WorkflowID: "flow", WorkflowScope: "shared", Snapshot: snapshot,
		State: string(engine.StateNeedsHuman), Reason: string(engine.ReasonTakenOver),
		Source: "manual", CreatedAt: 1,
	}
	if err := app.store.CreateWorkItem(item); err != nil {
		t.Fatal(err)
	}
	if err := app.store.CreateWorkItemPhase(store.WorkItemPhase{
		ItemID: item.ID, PhaseID: phase.ID, Attempt: 1, ThreadID: thread.ID,
		Status: "parked", StartedAt: 1, EndedAt: 2,
	}); err != nil {
		t.Fatal(err)
	}
	runner := newWorkflowAppRunner(app, t.TempDir(), nil)
	app.workflowRunner = runner
	if err := runner.registerTakeover(item.ID, thread.ID); err != nil {
		t.Fatal(err)
	}
	schema, err := app.workflowSchemaForSession(thread)
	if err != nil {
		t.Fatalf("schema-less takeover session registration failed: %v", err)
	}
	if len(schema) != 0 || runner.workItemForThread(thread.ID) != item.ID {
		t.Fatalf("schema/item registration = %s/%q, want schema-less/%q", schema, runner.workItemForThread(thread.ID), item.ID)
	}
	app.sessionManager().put(thread.ID, session{})
	if err := runner.registerTakeover(item.ID, thread.ID); err != nil {
		t.Fatal(err)
	}
	if runner.takeovers[thread.ID].schemaAttached {
		t.Fatal("a later registration mistook the live schema-less session for a schema-attached Claude session")
	}
}

func TestWorkflowRunnerRejectsUnsupportedPhasesAndStopsUnknownRuns(t *testing.T) {
	runner := newWorkflowAppRunner(&App{}, t.TempDir(), nil)
	if _, err := runner.Stop(context.Background(), engine.RunKey{ItemID: "missing", PhaseID: "phase", Attempt: 1}); err != nil {
		t.Fatalf("Stop unknown run = %v", err)
	}
	for _, phase := range []def.Phase{
		{ID: "tool", Driver: def.DriverTool, Shape: def.ShapeSingle},
		{ID: "fan", Shape: def.ShapeFanOut},
	} {
		err := runner.Start(context.Background(), engine.RunRequest{Phase: phase}, func() {}, func(engine.Outcome) {})
		if err == nil {
			t.Fatalf("unsupported phase %+v succeeded", phase)
		}
	}

	key := engine.RunKey{ItemID: "known", PhaseID: "phase", Attempt: 1}
	runKey := workflowRunKey(key)
	unsubscribed := false
	runner.runs[runKey] = &workflowAttempt{
		workflowCompletion: workflowCompletion{key: key}, threadID: "workflow-thread", unsubscribe: func() { unsubscribed = true },
	}
	runner.schemas["workflow-thread"] = json.RawMessage(`{"type":"object"}`)
	if _, err := runner.Stop(context.Background(), key); err != nil {
		t.Fatalf("Stop known run = %v", err)
	}
	if !unsubscribed || runner.runs[runKey] != nil || len(runner.schemas["workflow-thread"]) != 0 {
		t.Fatalf("known run cleanup: unsubscribed=%v runs=%v schemas=%v", unsubscribed, runner.runs, runner.schemas)
	}
	if sent, err := runner.sendIfActive(runKey, "late retry", json.RawMessage(`{"type":"object"}`)); err != nil || sent {
		t.Fatalf("post-stop retry = sent %v, err %v", sent, err)
	}
}

func TestWorkflowTurnErrorTerminalClassification(t *testing.T) {
	if !workflowTurnErrorIsTerminal(json.RawMessage(`{"fatal":true}`)) {
		t.Fatal("fatal error without a following turn completion was not terminal")
	}
	for _, meta := range []json.RawMessage{
		nil,
		json.RawMessage(`{"fatal":false}`),
		json.RawMessage(`{"fatal":true,"expect_turn_complete":true}`),
	} {
		if workflowTurnErrorIsTerminal(meta) {
			t.Fatalf("non-terminal error meta classified terminal: %s", meta)
		}
	}
}
