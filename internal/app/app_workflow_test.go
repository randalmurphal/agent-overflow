package app

import (
	projectpkg "agent-overflow/internal/project"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/profile"
	"agent-overflow/internal/workflowhost"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
		if app.workflowApplication().Engine() != nil {
			_ = app.workflowApplication().Engine().Close()
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

	if err := app.WorkflowAnswerQuestion(context.Background(), item.ID, "Use option A"); err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")
	requireWorkspaceLockForgotten(t, app, item.ID)
	completed, err := app.WorkflowGetItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(completed.Phases) != 3 || completed.Phases[2].PhaseID != "finish" || completed.Phases[2].Attempt != 2 ||
		completed.Phases[2].ThreadID != questionThreadID || completed.Phases[2].Status != "completed" {
		t.Fatalf("completed phase detail = %+v", completed)
	}
	questionThreadItems, err := app.store.ListItems(questionThreadID)
	if err != nil {
		t.Fatal(err)
	}
	var userPrompts []string
	for _, timelineItem := range questionThreadItems {
		if timelineItem.Role == "user" && timelineItem.Kind == "user_text" {
			userPrompts = append(userPrompts, timelineItem.Summary)
		}
	}
	if len(userPrompts) != 2 || !strings.Contains(userPrompts[1], "Resume the current workflow phase") ||
		strings.Contains(userPrompts[1], "Finish after preparation") {
		t.Fatalf("question continuation prompts = %#v, want a short second prompt", userPrompts)
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
		if app.workflowApplication().Engine() != nil {
			_ = app.workflowApplication().Engine().Close()
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
	if err := app.WorkflowAnswerQuestion(context.Background(), item.ID, "continue"); err != nil {
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
		if app.workflowApplication().Engine() != nil {
			_ = app.workflowApplication().Engine().Close()
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
    printf '{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"workflow-provider-thread","turn":{"id":"turn-%%s"}}}\n' "$turn"
	if [ "$turn" -eq 1 ]; then
	  printf '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"workflow-provider-thread","turn":{"id":"turn-%%s","status":"completed"}}}\n' "$turn"
	  continue
	fi
	if [ "$turn" -eq 2 ]; then
      text='{"status":"question","outputs":null,"question":"Continue?","reason":null}'
    else
      text='{"status":"done","outputs":{"complete":true},"question":null,"reason":null}'
    fi
    escaped=$(/usr/bin/printf '%%s' "$text" | /usr/bin/sed 's/\\/\\\\/g; s/"/\\"/g')
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
  if [[ "$line" == *"Finish after preparation"* || "$line" == *"Resume the current workflow phase"* ]]; then
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

// requireWorkspaceLockForgotten asserts a finished run left no workspace-lock
// entry behind. The registry self-cleans when the last holder or waiter
// releases; before that, it grew one entry per run for the life of the
// process. The wait is for provisioning's final release, which runs on the
// start worker rather than strictly before the store write the state poller
// reads.
func requireWorkspaceLockForgotten(t *testing.T, app *App, itemID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if app.workflowApplication().WorkspaceLockRefs(itemID) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("workspace lock entry for %s still registered after the run ended", itemID)
		}
		time.Sleep(10 * time.Millisecond)
	}
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
	app.workflowApplication().SetRunnerForTesting(runner)
	if err := runner.RegisterTakeover(context.Background(), item.ID, thread.ID); err != nil {
		t.Fatal(err)
	}
	schema, err := app.workflowSchemaForSession(thread)
	if err != nil {
		t.Fatalf("schema-less takeover session registration failed: %v", err)
	}
	if len(schema) != 0 || runner.WorkItemForThread(thread.ID) != item.ID {
		t.Fatalf("schema/item registration = %s/%q, want schema-less/%q", schema, runner.WorkItemForThread(thread.ID), item.ID)
	}
	app.sessionManager().put(thread.ID, session{})
	if err := runner.RegisterTakeover(context.Background(), item.ID, thread.ID); err != nil {
		t.Fatal(err)
	}
	// Read through the boundary the session start itself uses: a live
	// schema-less takeover session mistaken for a schema-attached Claude one
	// would hand the restart a schema here.
	if schema, err := app.workflowSchemaForSession(thread); err != nil || len(schema) != 0 {
		t.Fatalf("later registration schema = %s, %v; want the session still schema-less", schema, err)
	}
}

// TestWorkflowPhaseAccessMapsToThreadRuntimeMode is the end-to-end proof for
// decision D22: a phase's `access` declaration is enforced at the provider
// session, not merely used to decide whether a worktree is cut.
//
// The fixture deliberately mixes access levels in ONE run so the two axes are
// visibly independent: because the write phase exists, the whole item gets a
// worktree and both phases execute in it — yet the read-only phase's session
// must still be restricted. A test with a read-only-only workflow would pass
// even if access were still wired to workspace derivation alone.
//
// Assertions run against the persisted thread row and the SessionOptions
// derived from it, because the row is the source of truth every later session
// start (restart, resume, Answer-continuation) re-derives from.
func TestWorkflowPhaseAccessMapsToThreadRuntimeMode(t *testing.T) {
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	repo := testutil.InitGitRepo(t)
	projectRow := testutil.EnsureProject(t, app.store, repo)
	projectRow = mustReloadProject(t, app.store, projectRow.ID)
	writeMixedAccessWorkflow(t, configRoot)
	writeWorkspaceProfile(t, configRoot, projectRow.Slug, `
base_branch: main
reliability:
  watchdog: 1h
  backoff: [5ms]
`)
	if _, err := app.settings.Update(map[string]any{
		"claudeBinaryPath": writeWorkspaceClaude(t, filepath.Join(t.TempDir(), "cwd"), false, "done"),
	}); err != nil {
		t.Fatal(err)
	}
	startWorkflowEngineForTest(t, app, configRoot)

	item, err := app.WorkflowStartRun(
		projectRow.ID, "mixed-access", "shared", "exercise access",
		json.RawMessage(`{}`), nil, "", false,
	)
	if err != nil {
		t.Fatal(err)
	}
	item = waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")
	t.Cleanup(func() {
		if item.WorktreePath != "" {
			_ = app.gitCore().RemoveWorktreeForce(repo, item.WorktreePath, true)
		}
	})

	// A writing phase in the graph means the run is provisioned a worktree —
	// which both phases share. Access is therefore NOT derivable from the
	// workspace here; it has to come from the phase declaration.
	if item.WorktreePath == "" {
		t.Fatal("mixed-access workflow did not provision a worktree for its write phase")
	}

	detail, err := app.WorkflowGetItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	threadsByPhase := map[string]string{}
	for _, phase := range detail.Phases {
		threadsByPhase[phase.PhaseID] = phase.ThreadID
	}
	for _, phaseID := range []string{"survey", "apply"} {
		if threadsByPhase[phaseID] == "" {
			t.Fatalf("phase %q produced no thread: %+v", phaseID, detail.Phases)
		}
	}

	cases := []struct {
		phaseID            string
		wantRuntimeMode    provider.RuntimeMode
		wantPermissionMode string
		wantToolsRemoved   bool
	}{
		{"survey", provider.RuntimeReadOnly, "dontAsk", true},
		{"apply", provider.RuntimeFullAccess, "bypassPermissions", false},
	}
	for _, tc := range cases {
		t.Run(tc.phaseID, func(t *testing.T) {
			thread, err := app.store.GetThread(threadsByPhase[tc.phaseID])
			if err != nil {
				t.Fatal(err)
			}
			if thread.RuntimeMode != string(tc.wantRuntimeMode) {
				t.Fatalf("thread row runtime_mode = %q, want %q", thread.RuntimeMode, tc.wantRuntimeMode)
			}
			// Both phases run in the same worktree — proving the runtime mode
			// came from `access`, not from which workspace was provisioned.
			if thread.WorkspacePath != item.WorktreePath {
				t.Fatalf("thread workspace = %q, want the item worktree %q", thread.WorkspacePath, item.WorktreePath)
			}

			opts, err := app.buildSessionOptions(thread)
			if err != nil {
				t.Fatal(err)
			}
			if opts.RuntimeMode != tc.wantRuntimeMode {
				t.Fatalf("SessionOptions.RuntimeMode = %q, want %q", opts.RuntimeMode, tc.wantRuntimeMode)
			}
			cfg := claude.ConfigFromOptions(opts)
			if cfg.BasePermissionMode != tc.wantPermissionMode {
				t.Fatalf("claude BasePermissionMode = %q, want %q", cfg.BasePermissionMode, tc.wantPermissionMode)
			}
			if got := len(cfg.DisallowedTools) > 0; got != tc.wantToolsRemoved {
				t.Fatalf("claude DisallowedTools present = %v (%v), want %v", got, cfg.DisallowedTools, tc.wantToolsRemoved)
			}
		})
	}
}

// TestWorkflowUndeclaredAccessRunsReadOnly pins the default direction at the
// level that matters. A phase that says nothing about access gets no worktree
// AND a restricted session — the two halves of "unset means read-only".
func TestWorkflowUndeclaredAccessRunsReadOnly(t *testing.T) {
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	repo := testutil.InitGitRepo(t)
	projectRow := testutil.EnsureProject(t, app.store, repo)
	projectRow = mustReloadProject(t, app.store, projectRow.ID)
	writeUndeclaredAccessWorkflow(t, configRoot)
	writeWorkspaceProfile(t, configRoot, projectRow.Slug, `
base_branch: main
reliability:
  watchdog: 1h
  backoff: [5ms]
`)
	if _, err := app.settings.Update(map[string]any{
		"claudeBinaryPath": writeWorkspaceClaude(t, filepath.Join(t.TempDir(), "cwd"), false, "done"),
	}); err != nil {
		t.Fatal(err)
	}
	startWorkflowEngineForTest(t, app, configRoot)

	item, err := app.WorkflowStartRun(
		projectRow.ID, "undeclared-access", "shared", "no access field",
		json.RawMessage(`{}`), nil, "", false,
	)
	if err != nil {
		t.Fatal(err)
	}
	item = waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")

	if item.WorktreePath != "" {
		t.Fatalf("undeclared access provisioned a worktree %q — unset must mean read-only", item.WorktreePath)
	}
	detail, err := app.WorkflowGetItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Phases) == 0 {
		t.Fatal("no phases recorded")
	}
	thread, err := app.store.GetThread(detail.Phases[0].ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if thread.RuntimeMode != string(provider.RuntimeReadOnly) {
		t.Fatalf("undeclared-access thread runtime_mode = %q, want read-only", thread.RuntimeMode)
	}
	if thread.WorkspacePath != projectRow.Path {
		t.Fatalf("read-only phase workspace = %q, want the project root %q", thread.WorkspacePath, projectRow.Path)
	}
}

// TestWorkflowPhaseRuntimeModeMapping covers the pure mapping directly,
// including the unset case the YAML fixtures cannot express twice.
func TestWorkflowPhaseRuntimeModeMapping(t *testing.T) {
	cases := map[def.Access]provider.RuntimeMode{
		def.AccessWrite:    provider.RuntimeFullAccess,
		def.AccessReadOnly: provider.RuntimeReadOnly,
		"":                 provider.RuntimeReadOnly,
	}
	for access, want := range cases {
		if got := workflowPhaseRuntimeMode(access); got != want {
			t.Errorf("workflowPhaseRuntimeMode(%q) = %q, want %q", access, got, want)
		}
	}
}

// TestWorkflowPhasesNeverRunUnderTheAutoRuntimeMode records a deliberate
// decision rather than an accident of the current mapping.
//
// `auto` is an interactive tier. Two of its properties are wrong for
// unattended work: a Claude auto session that accumulates classifier denials
// falls back to PROMPTING (there is nobody to prompt — the whole reason D22
// gave workflow phases `read-only`, the one tier that denies instead of
// asking), and every reviewed tool call bills a classifier turn against a run
// the user is not watching. `read-only` and `full-access` stay the only two
// modes a phase can reach, which is already structural — `def.Access` is a
// closed two-value set and `normalizeAccess` collapses anything else onto
// read-only — so this test is the guard that keeps it structural.
//
// Changing this is a scope conversation, not a bug fix: it would need a new
// `access:` value in the workflow schema, which is a definition-format change.
func TestWorkflowPhasesNeverRunUnderTheAutoRuntimeMode(t *testing.T) {
	for _, access := range []def.Access{def.AccessWrite, def.AccessReadOnly, "", "auto", "nonsense"} {
		if got := workflowPhaseRuntimeMode(access); got == provider.RuntimeAuto {
			t.Errorf("workflowPhaseRuntimeMode(%q) = auto; unattended phases must never route approvals to a billed reviewer", access)
		}
	}
}

// TestCreateWorkflowThreadRejectsProviderThatCannotEnforceAccess proves the
// phase refuses to start rather than running with an inert access
// declaration. claude-tui hands permissions to the real TUI, so its threads'
// runtime mode is never applied — starting an unattended read-only phase on
// it would silently grant full access.
func TestCreateWorkflowThreadRejectsProviderThatCannotEnforceAccess(t *testing.T) {
	app, _ := setupE2EApp(t)
	repo := testutil.InitGitRepo(t)
	projectRow := testutil.EnsureProject(t, app.store, repo)

	phase := def.Phase{
		ID:       "survey",
		Driver:   def.DriverAgent,
		Provider: string(provider.ClaudeTUI),
		Model:    "claude-opus-4-7",
		Access:   def.AccessReadOnly,
	}
	_, err := app.createWorkflowThread(workflowhost.ThreadSpec{
		ItemID: "item-access", Label: `phase "survey"`,
		Title:        workflowhost.ThreadTitle(phase.Name, phase.ID),
		ProviderName: phase.Provider, Model: phase.Model,
		Access:    phase.EffectiveAccess(),
		Workspace: workflowhost.PreparedWorkspace{Path: repo, Project: projectRow},
	})
	if err == nil {
		t.Fatal("createWorkflowThread accepted a provider that cannot enforce runtime modes")
	}
	if !strings.Contains(err.Error(), "does not enforce runtime modes") {
		t.Fatalf("error = %v, want a runtime-mode enforcement message", err)
	}
	// Typed so the engine parks it as a wiring error rather than an
	// agent error — the definition is unrunnable, not the agent misbehaving.
	if !errors.Is(err, engine.ErrWiringFailed) {
		t.Fatalf("error %v is not tagged engine.ErrWiringFailed", err)
	}

	threads, err := app.store.ListThreadsByProject(projectRow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 0 {
		t.Fatalf("refused phase still created %d thread(s)", len(threads))
	}
}

func writeMixedAccessWorkflow(t *testing.T, configRoot string) {
	t.Helper()
	writeAccessWorkflowFixture(t, configRoot, "mixed-access", `id: mixed-access
name: Mixed access
phases:
  - id: survey
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: step.md
    access: read-only
    outputs:
      report:
        schema:
          type: string
    gate:
      routes:
        - to: apply
  - id: apply
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: step.md
    access: write
    outputs:
      report:
        schema:
          type: string
    gate:
      routes:
        - to: done
cleanup: manual
`)
}

func writeUndeclaredAccessWorkflow(t *testing.T, configRoot string) {
	t.Helper()
	writeAccessWorkflowFixture(t, configRoot, "undeclared-access", `id: undeclared-access
name: Undeclared access
phases:
  - id: survey
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: step.md
    outputs:
      report:
        schema:
          type: string
    gate:
      routes:
        - to: done
cleanup: manual
`)
}

func writeAccessWorkflowFixture(t *testing.T, configRoot, id, definition string) {
	t.Helper()
	dir := filepath.Join(configRoot, "workflows")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".yaml"), []byte(definition), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "step.md"), []byte("Do the step"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A campaign edits its own prompts between waves: the human reads what the last
// wave produced, sharpens the instruction, and the next wave is supposed to run
// the sharpened one. That only works because a call resolves its target from
// disk per invocation — the caller's frozen snapshot is the caller's, and a
// child is a new run resolved fresh (§8).
//
// The test asserts the property at the definition source, which is where the
// re-resolution lives: `startCall` / `startUnitCall` call `ResolveCall` per
// invocation, and `ResolveCall` reads the workflow and inlines its prompt
// bodies off disk every time.
func TestCallResolutionReadsEditedPromptsPerInvocation(t *testing.T) {
	configRoot := t.TempDir()
	database := storetest.Clone(t)
	projectRow := testutil.EnsureProject(t, database, t.TempDir())
	writeSelfCallingCampaign(t, configRoot, "first wave instructions")

	source := workflowDefinitionSource{
		store:      database,
		configRoot: configRoot,
		profiles:   workflowProfileSource{store: database, configRoot: configRoot},
	}
	first, err := source.ResolveCall(context.Background(), projectRow.ID, "campaign")
	if err != nil {
		t.Fatal(err)
	}
	if body := first.Workflow.Phases[0].Prompt; !strings.Contains(body, "first wave instructions") {
		t.Fatalf("first resolution inlined %q", body)
	}

	// The human edits the prompt between waves. Nothing restarts, nothing is
	// re-registered — the file on disk is the whole mechanism.
	writePromptFile(t, configRoot, "wave.md", "second wave instructions {{goal}}")

	second, err := source.ResolveCall(context.Background(), projectRow.ID, "campaign")
	if err != nil {
		t.Fatal(err)
	}
	body := second.Workflow.Phases[0].Prompt
	if !strings.Contains(body, "second wave instructions") {
		t.Fatalf("the next wave did not pick up the edited prompt: %q", body)
	}
	if strings.Contains(body, "first wave instructions") {
		t.Fatalf("the next wave reused a cached prompt body: %q", body)
	}
	// The already-resolved definition is a value, not a view: the first wave's
	// snapshot is untouched by the edit, which is what freezing means for the run
	// that is already going.
	if got := first.Workflow.Phases[0].Prompt; !strings.Contains(got, "first wave instructions") {
		t.Fatalf("an earlier resolution changed under the edit: %q", got)
	}
}

// writeSelfCallingCampaign writes the campaign shape the re-resolution matters
// for: a fan-out whose units call the campaign back, bounded by max_depth.
func writeSelfCallingCampaign(t *testing.T, configRoot, promptBody string) {
	t.Helper()
	dir := filepath.Join(configRoot, "workflows")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	workflow := `id: campaign
name: Campaign
inputs:
  goal:
    schema:
      type: string
phases:
  - id: plan
    name: Plan
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: wave.md
    access: read-only
    inputs:
      goal:
        schema:
          type: string
    outputs:
      sections:
        schema:
          type: array
          items:
            type: string
    gate:
      routes:
        - to: wave
  - id: wave
    shape: fan-out
    over: plan.sections
    as: section
    unit:
      id: wave-unit
      call: campaign
      max_depth: 120
      args:
        goal: section
    join:
      id: merge
      provider: claude
      model: claude-opus-4-7
      prompt: merge.md
    inputs:
      plan.sections:
        schema:
          type: array
          items:
            type: string
    outputs:
      merged:
        schema:
          type: boolean
    gate:
      routes:
        - to: done
`
	if err := os.WriteFile(filepath.Join(dir, "campaign.yaml"), []byte(workflow), 0o600); err != nil {
		t.Fatal(err)
	}
	writePromptFile(t, configRoot, "wave.md", promptBody+" {{goal}}")
	writePromptFile(t, configRoot, "merge.md", "merge {{units}}")
}

func writePromptFile(t *testing.T, configRoot, name, body string) {
	t.Helper()
	path := filepath.Join(configRoot, "workflows", name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(fmt.Errorf("write prompt %q: %w", path, err))
	}
}

func TestWorkflowReliabilityStallTripsWatchdog(t *testing.T) {
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	writeReliabilityWorkflow(t, configRoot, `
  - id: run
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: run.md
    watchdog: 20ms
    gate:
      routes:
        - to: done`)
	binary := writeStallingWorkflowClaude(t)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	projectRow := testutil.EnsureProject(t, app.store, t.TempDir())
	projectRow = mustReloadProject(t, app.store, projectRow.ID)
	writeReliabilityProfile(t, configRoot, projectRow.Slug, "watchdog: 1h\n  backoff: [5ms]\n")
	startWorkflowEngineForTest(t, app, configRoot)

	item, err := app.WorkflowStartRun(projectRow.ID, "reliability-flow", "shared", "stall", json.RawMessage(`{}`), nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, app, item.ID, engine.StateNeedsHuman, engine.ReasonStalled)
}

func TestWorkflowReliabilityTransientDeathRetriesThenExhausts(t *testing.T) {
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	writeReliabilityWorkflow(t, configRoot, `
  - id: run
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: run.md
    gate:
      routes:
        - to: done`)
	counterPath := filepath.Join(t.TempDir(), "starts")
	binary := writeDyingWorkflowClaude(t, counterPath)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	projectRow := testutil.EnsureProject(t, app.store, t.TempDir())
	projectRow = mustReloadProject(t, app.store, projectRow.ID)
	writeReliabilityProfile(t, configRoot, projectRow.Slug, "watchdog: 1h\n  backoff: [50ms, 50ms]\n")
	startWorkflowEngineForTest(t, app, configRoot)

	item, err := app.WorkflowStartRun(projectRow.ID, "reliability-flow", "shared", "retry deaths", json.RawMessage(`{}`), nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, app, item.ID, engine.StateNeedsHuman, engine.ReasonProviderRetriesExhausted)
	data, err := os.ReadFile(counterPath)
	if err != nil {
		t.Fatal(err)
	}
	if starts := strings.Count(string(data), "start\n"); starts != 3 {
		t.Fatalf("provider starts = %d, want initial + two scheduled retries; log=%q", starts, data)
	}
}

func TestWorkflowReliabilityAttributedTokenBudgetTripsAtBoundary(t *testing.T) {
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	writeReliabilityWorkflow(t, configRoot, `
  - id: first
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: first.md
    outputs:
      ready:
        schema:
          type: boolean
    gate:
      routes:
        - to: second
  - id: second
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: second.md
    gate:
      routes:
        - to: done`)
	binary := writeBudgetWorkflowClaude(t)
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}
	projectRow := testutil.EnsureProject(t, app.store, t.TempDir())
	projectRow = mustReloadProject(t, app.store, projectRow.ID)
	writeReliabilityProfile(t, configRoot, projectRow.Slug, "watchdog: 1h\n  backoff: [5ms]\n")
	startWorkflowEngineForTest(t, app, configRoot)

	tokenLimit := int64(10)
	item, err := app.WorkflowStartRun(
		projectRow.ID, "reliability-flow", "shared", "budget", json.RawMessage(`{}`),
		&profile.Budget{Tokens: &tokenLimit}, "", false,
	)
	if err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, app, item.ID, engine.StateNeedsHuman, engine.ReasonBudgetExhausted)
	detail, err := app.WorkflowGetItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The budget tripped at the boundary, so `second` never ran a turn — but it
	// still rests on an attempt row carrying the breach, because a park with no
	// row is a run that stopped with no record of why.
	if len(detail.Phases) != 2 ||
		detail.Phases[0].PhaseID != "first" || detail.Phases[0].Status != "completed" ||
		detail.Phases[1].PhaseID != "second" || detail.Phases[1].Status != "parked" ||
		len(detail.Phases[1].ThreadID) != 0 || len(detail.Phases[1].OutputEnvelope) != 0 {
		t.Fatalf("phase boundary detail = %+v", detail)
	}
	usage, err := app.store.QueryWorkItemUsage(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if usage.TotalTokens != 11 || usage.CostUSD != 0.1 {
		t.Fatalf("attributed usage = %+v", usage)
	}
}

func TestWorkflowSpendSourceAddsEstimatedRowsToWireCost(t *testing.T) {
	app := newTestAppWithStore(t)
	if err := app.store.AppendUsage([]store.UsageLedgerRow{
		{WorkItemID: "item", Model: "claude-opus-4-7", CostUSD: 0.5, CostSource: "wire"},
		{WorkItemID: "item", Model: "gpt-5.2-codex", InputTokens: 1_000_000, OutputTokens: 1_000_000, CacheReadInputTokens: 1_000_000, CacheCreationInputTokens: 1_000_000, CostSource: "none"},
	}); err != nil {
		t.Fatal(err)
	}
	spend, err := (workflowSpendSource{store: app.store}).TreeSpend(t.Context(), "item")
	if err != nil {
		t.Fatal(err)
	}
	if spend.Tokens != 4_000_000 || math.Abs(spend.USD-16.425) > 1e-12 {
		t.Fatalf("composed spend = %+v", spend)
	}
	// Most of that total came off a rate table rather than a provider, and the
	// spend says so — the caveat is what a budget surface has to be able to state.
	if !spend.Estimated || spend.Unpriced != 0 {
		t.Fatalf("composed spend = %+v, want estimated with nothing unpriced", spend)
	}

	// A model the rate table cannot price is REPORTED, not fatal: the refusal
	// belongs where the ceiling's kind is known, because tokens stay exact
	// whatever the rate table knows. See TestUSDBudgetRefusesUnpricedRowsItIsStillInside
	// (engine) and TestWorkflowTreeSpendReportsUnpricedRowsRatherThanFailing.
	if err := app.store.AppendUsage([]store.UsageLedgerRow{{WorkItemID: "unknown", Model: "future-model", InputTokens: 1, CostSource: "none"}}); err != nil {
		t.Fatal(err)
	}
	unpriced, err := (workflowSpendSource{store: app.store}).TreeSpend(t.Context(), "unknown")
	if err != nil {
		t.Fatalf("an unpriceable model must not fail the spend read: %v", err)
	}
	if unpriced.Tokens != 1 || unpriced.USD != 0 || unpriced.Unpriced != 1 {
		t.Fatalf("unpriced spend = %+v", unpriced)
	}
}

func startWorkflowEngineForTest(t *testing.T, app *App, configRoot string) {
	t.Helper()
	if err := app.initWorkflowEngine(configRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// The scheduler stops first for the same reason it does in production: a
		// trigger must not be able to fire into an engine that is closing.
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := app.stopWorkflowScheduler(stopCtx); err != nil {
			t.Errorf("stop workflow scheduler: %v", err)
		}
		if app.workflowApplication().Engine() != nil {
			_ = app.workflowApplication().Engine().Close()
		}
	})
}

func mustReloadProject(t *testing.T, database *store.Store, projectID string) store.Project {
	t.Helper()
	projectRow, err := database.GetProject(projectID)
	if err != nil {
		t.Fatal(err)
	}
	return projectRow
}

func writeReliabilityWorkflow(t *testing.T, configRoot, phases string) {
	t.Helper()
	dir := filepath.Join(configRoot, "workflows")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	definition := "id: reliability-flow\nname: Reliability flow\nphases:" + phases + "\ncleanup: manual\n"
	if err := os.WriteFile(filepath.Join(dir, "reliability-flow.yaml"), []byte(definition), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"run.md", "first.md", "second.md"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("Execute reliability test"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func writeReliabilityProfile(t *testing.T, configRoot, slug, reliability string) {
	t.Helper()
	dir := filepath.Join(configRoot, "projects", slug)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "profile.yaml"), []byte("reliability:\n  "+reliability), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeStallingWorkflowClaude(t *testing.T) string {
	t.Helper()
	script := `#!/bin/bash
while IFS= read -r line; do
  if [[ "$line" == *'"subtype":"interrupt"'* ]]; then
    reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
    printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"
    continue
  fi
  printf '%s\n' '{"type":"system","subtype":"init","session_id":"stall","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}'
done
`
	return writeExecutable(t, "stall-claude.sh", script)
}

func writeDyingWorkflowClaude(t *testing.T, counterPath string) string {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/bash
while IFS= read -r line; do
  printf 'start\n' >> %q
  printf '%%s\n' '{"type":"system","subtype":"init","session_id":"death","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}'
  exit 1
done
`, counterPath)
	return writeExecutable(t, "death-claude.sh", script)
}

func writeBudgetWorkflowClaude(t *testing.T) string {
	t.Helper()
	script := `#!/bin/bash
while IFS= read -r line; do
  printf '%s\n' '{"type":"system","subtype":"init","session_id":"budget","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}'
  printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"stop_reason":"end_turn","modelUsage":{"claude-opus-4-7":{"inputTokens":6,"outputTokens":5,"cacheReadInputTokens":0,"cacheCreationInputTokens":0,"costUSD":0.1}},"structured_output":{"status":"done","outputs":{"ready":true},"question":null,"reason":null}}'
done
`
	return writeExecutable(t, "budget-claude.sh", script)
}

func writeExecutable(t *testing.T, name, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// The run map is a pure store read, so these tests build the campaign the
// engine would have written and assert the projection over it. Nothing here
// starts a session; the fixture's poisoned provider binaries stay poisoned.

// campaignSnapshot is the shape the map exists for: a definition whose LAST
// phase calls itself, so the chain root → wave → wave flattens into waves,
// alongside one of every other phase shape the skeleton has to describe.
func campaignSnapshot(t *testing.T) json.RawMessage {
	t.Helper()
	return marshalSnapshot(t, def.Workflow{ID: "campaign", Phases: []def.Phase{
		{ID: "plan", Name: "Plan the wave", Driver: def.DriverAgent, Provider: "claude", Model: "m"},
		{ID: "verify", Driver: def.DriverTool, Check: "lint"},
		{ID: "port", Driver: def.DriverAgent, Shape: def.ShapeFanOut, Provider: "claude", Model: "m"},
		{ID: "audit", Shape: def.ShapeCall, Call: "reviewer"},
		{ID: "next", Shape: def.ShapeCall, Call: "campaign", MaxDepth: 8},
	}})
}

func laneSnapshot(t *testing.T) json.RawMessage {
	t.Helper()
	return marshalSnapshot(t, def.Workflow{ID: "port-lane", Phases: []def.Phase{
		{ID: "work", Driver: def.DriverAgent, Provider: "claude", Model: "m"},
	}})
}

func marshalSnapshot(t *testing.T, workflow def.Workflow) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(engine.Snapshot{Workflow: workflow})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// seedRunMapCampaign writes root → wave 1 → wave 2, wave 2's fan-out with one
// unit of every reachable status, the child that fan-out unit called, and the
// non-self call child of the same wave. It returns the run ids by name.
func seedRunMapCampaign(t *testing.T, app *App) map[string]string {
	t.Helper()
	campaign := campaignSnapshot(t)
	items := []store.WorkItem{
		{
			ID: "root", ProjectID: defaultTestProjectID, Goal: "port everything",
			WorkflowID: "campaign", WorkflowScope: "project", Snapshot: campaign,
			State: string(engine.StateRunning), Source: "manual", SoftStop: true,
			Budget: json.RawMessage(`{"usd":25}`), CreatedAt: 10, StartedAt: 10,
		},
		{
			ID: "wave-1", ProjectID: defaultTestProjectID, Goal: "wave 1",
			WorkflowID: "campaign", WorkflowScope: "project", Snapshot: campaign,
			State: string(engine.StateDone), Source: "call",
			ParentItemID: "root", ParentPhaseID: "next", ParentAttempt: 1, CallDepth: 1,
			CreatedAt: 20, StartedAt: 20, EndedAt: 30,
		},
		{
			ID: "wave-2", ProjectID: defaultTestProjectID, Goal: "wave 2",
			WorkflowID: "campaign", WorkflowScope: "project", Snapshot: campaign,
			State: string(engine.StateNeedsHuman), Reason: string(engine.ReasonProviderRetriesExhausted),
			Source: "call", ParentItemID: "wave-1", ParentPhaseID: "next", ParentAttempt: 1,
			CallDepth: 2, CreatedAt: 40, StartedAt: 40,
		},
		{
			ID: "lane-child", ProjectID: defaultTestProjectID, Goal: "port one lane",
			WorkflowID: "port-lane", WorkflowScope: "project", Snapshot: laneSnapshot(t),
			State: string(engine.StateRunning), Source: "call",
			ParentItemID: "wave-2", ParentPhaseID: "port", ParentUnitID: "port-0",
			ParentAttempt: 1, CallDepth: 3, CreatedAt: 50, StartedAt: 50,
		},
		{
			ID: "audit-child", ProjectID: defaultTestProjectID, Goal: "review the wave",
			WorkflowID: "reviewer", WorkflowScope: "project",
			Snapshot: marshalSnapshot(t, def.Workflow{ID: "reviewer", Phases: []def.Phase{
				{ID: "review", Driver: def.DriverAgent, Provider: "codex", Model: "m"},
			}}),
			State: string(engine.StateRunning), Source: "call",
			ParentItemID: "wave-2", ParentPhaseID: "audit", ParentAttempt: 1, CallDepth: 3,
			CreatedAt: 60, StartedAt: 60,
		},
	}
	for _, item := range items {
		if err := app.store.CreateWorkItem(item); err != nil {
			t.Fatalf("create run %s: %v", item.ID, err)
		}
	}

	takeover, err := json.Marshal(engine.TakeoverIntervention{Kind: engine.TakeoverInterventionKind, At: 45})
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []store.WorkItemPhase{
		{ItemID: "wave-1", PhaseID: "plan", Attempt: 1, Status: "completed", StartedAt: 20, EndedAt: 22},
		{ItemID: "wave-1", PhaseID: "next", Attempt: 1, Status: "completed", StartedAt: 24, EndedAt: 30},
		{ItemID: "wave-2", PhaseID: "plan", Attempt: 1, Status: "completed", StartedAt: 41, EndedAt: 42,
			ThreadID: "thread-plan", Intervention: takeover},
		{ItemID: "wave-2", PhaseID: "port", Attempt: 1, Status: "parked", StartedAt: 43,
			ParkCause: "unit port-1 failed"},
	} {
		if err := app.store.CreateWorkItemPhase(phase); err != nil {
			t.Fatalf("create phase %s/%s: %v", phase.ItemID, phase.PhaseID, err)
		}
	}
	if err := app.store.CreateWorkItemUnits([]store.WorkItemUnit{
		{ItemID: "wave-2", PhaseID: "port", Attempt: 1, UnitID: "port-0", UnitIndex: 0,
			Kind: store.WorkItemUnitKindUnit, Provider: "claude", Status: store.WorkItemUnitDone,
			ThreadID: "thread-port-0", UnitAttempt: 1, StartedAt: 44, EndedAt: 46},
		{ItemID: "wave-2", PhaseID: "port", Attempt: 1, UnitID: "port-1", UnitIndex: 1,
			Kind: store.WorkItemUnitKindUnit, Provider: "codex", Status: store.WorkItemUnitRunning,
			UnitAttempt: 2, StartedAt: 45},
		{ItemID: "wave-2", PhaseID: "port", Attempt: 1, UnitID: "port-2", UnitIndex: 2,
			Kind: store.WorkItemUnitKindUnit, Provider: "codex", Status: store.WorkItemUnitPending, UnitAttempt: 1},
		{ItemID: "wave-2", PhaseID: "port", Attempt: 1, UnitID: "port-join", UnitIndex: 3,
			Kind: store.WorkItemUnitKindJoin, Provider: "claude", Status: store.WorkItemUnitPending, UnitAttempt: 1},
	}); err != nil {
		t.Fatalf("create units: %v", err)
	}
	if err := app.store.SetWorkItemAutoResumeAt("wave-2", 9_999); err != nil {
		t.Fatalf("arm auto resume: %v", err)
	}
	// Spend lands on a CHILD, which is the whole reason the root's number is the
	// tree's: a campaign's dollars are almost never on the run that started it.
	if err := app.store.AppendUsage([]store.UsageLedgerRow{{
		CreatedAt: 47, ThreadID: "thread-port-0", ProjectID: defaultTestProjectID,
		WorkItemID: "lane-child", TurnID: "turn-1", Provider: "claude", Model: "claude-opus-4-7",
		InputTokens: 100, OutputTokens: 200, CostUSD: 1.5, CostSource: "wire",
	}}); err != nil {
		t.Fatalf("append usage: %v", err)
	}
	return map[string]string{"root": "root", "wave-1": "wave-1", "wave-2": "wave-2",
		"lane-child": "lane-child", "audit-child": "audit-child"}
}

func runMapByID(t *testing.T, view WorkflowRunMapView) map[string]WorkflowRunMapRun {
	t.Helper()
	runs := make(map[string]WorkflowRunMapRun, len(view.Runs))
	for _, run := range view.Runs {
		runs[run.ItemID] = run
	}
	return runs
}

func TestWorkflowGetRunMapResolvesTheRootFromAnyRunInTheTree(t *testing.T) {
	app := newTestAppWithStore(t)
	seedRunMapCampaign(t, app)

	for _, from := range []string{"root", "wave-2", "lane-child", "audit-child"} {
		view, err := app.WorkflowGetRunMap(t.Context(), from)
		if err != nil {
			t.Fatalf("run map from %s: %v", from, err)
		}
		if view.RootItemID != "root" {
			t.Fatalf("run map from %s resolved root %q", from, view.RootItemID)
		}
		if len(view.Runs) != 5 || view.Runs[0].ItemID != "root" {
			t.Fatalf("run map from %s = %d runs, first %q", from, len(view.Runs), view.Runs[0].ItemID)
		}
		// Parent before child: the consumer builds the tree in one pass.
		seen := map[string]bool{}
		for _, run := range view.Runs {
			if run.ParentItemID != "" && !seen[run.ParentItemID] {
				t.Fatalf("run %s arrived before its parent %s", run.ItemID, run.ParentItemID)
			}
			seen[run.ItemID] = true
		}
	}

	if _, err := app.WorkflowGetRunMap(t.Context(), "  "); err == nil {
		t.Fatal("blank item id was accepted")
	}
}

// A run that is simply GONE is the commonest thing this method is asked for —
// a stale nav entry, a discarded campaign — so it is an answer with a code the
// client can stop retrying on, not an error string.
func TestWorkflowGetRunMapRefusesAnUnknownRunPermanently(t *testing.T) {
	app := newTestAppWithStore(t)
	seedRunMapCampaign(t, app)

	view, err := app.WorkflowGetRunMap(t.Context(), "no-such-run")
	if err != nil {
		t.Fatalf("an absent run must be an answer, not an error: %v", err)
	}
	if view.Refusal == nil || view.Refusal.Code != WorkflowRunMapRefusalNotFound {
		t.Fatalf("absent run = %#v", view.Refusal)
	}
	if len(view.Runs) != 0 || view.RootItemID != "" {
		t.Fatalf("a refusal carried a tree: %#v", view)
	}
	if !strings.Contains(view.Refusal.Message, "no-such-run") {
		t.Fatalf("refusal message does not name the run: %q", view.Refusal.Message)
	}
}

// The classification itself, over the store's typed refusals. Seeding a
// 4097-run campaign to reach the member cap would test SQLite rather than this
// contract; what matters is that each typed refusal maps to its code, and that
// everything else stays an error so the client keeps retrying it.
func TestWorkflowRunMapRefusalClassification(t *testing.T) {
	for _, testCase := range []struct {
		name string
		err  error
		code string
	}{
		{"absent", fmt.Errorf("workflow run map: %w", sql.ErrNoRows), WorkflowRunMapRefusalNotFound},
		{"too large", fmt.Errorf("wrapped: %w", store.ErrWorkItemTreeTooLarge), WorkflowRunMapRefusalTooLarge},
		{"too deep", fmt.Errorf("wrapped: %w", store.ErrWorkItemTreeTooDeep), WorkflowRunMapRefusalCorruptLinkage},
		{"cyclic", fmt.Errorf("wrapped: %w", store.ErrWorkItemTreeCyclicLinkage), WorkflowRunMapRefusalCorruptLinkage},
	} {
		view, err := workflowRunMapRefusalFor("run", testCase.err)
		if err != nil {
			t.Fatalf("%s: %v", testCase.name, err)
		}
		if view.Refusal == nil || view.Refusal.Code != testCase.code {
			t.Fatalf("%s = %#v, want code %q", testCase.name, view.Refusal, testCase.code)
		}
		if view.Refusal.Message == "" {
			t.Fatalf("%s carried no sentence to render", testCase.name)
		}
	}
	// A failure retrying COULD fix keeps the retry: it stays an error and never
	// becomes a permanent-looking refusal.
	transient := errors.New("database is locked")
	if _, err := workflowRunMapRefusalFor("run", transient); !errors.Is(err, transient) {
		t.Fatalf("a transient failure was classified as permanent: %v", err)
	}
}

func TestWorkflowGetRunMapProjectsSkeletonRecordsAndMoney(t *testing.T) {
	app := newTestAppWithStore(t)
	seedRunMapCampaign(t, app)

	view, err := app.WorkflowGetRunMap(t.Context(), "root")
	if err != nil {
		t.Fatal(err)
	}
	runs := runMapByID(t, view)

	root := runs["root"]
	if len(root.Skeleton) != 5 {
		t.Fatalf("root skeleton = %#v", root.Skeleton)
	}
	for index, want := range []WorkflowRunMapSkeletonPhase{
		{ID: "plan", Name: "Plan the wave", Shape: string(def.ShapeSingle)},
		{ID: "verify", Shape: string(def.ShapeSingle), IsCheck: true},
		{ID: "port", Shape: string(def.ShapeFanOut)},
		{ID: "audit", Shape: string(def.ShapeCall), CallTarget: "reviewer"},
		{ID: "next", Shape: string(def.ShapeCall), CallTarget: "campaign", MaxDepth: 8},
	} {
		if root.Skeleton[index] != want {
			t.Fatalf("skeleton[%d] = %#v, want %#v", index, root.Skeleton[index], want)
		}
	}
	if root.SkeletonMissing || !root.TailSelfCall || !root.SoftStop {
		t.Fatalf("root = %#v", root)
	}
	// The chain flattens; the runs a chain member CALLED do not.
	for _, id := range []string{"wave-1", "wave-2"} {
		if !runs[id].TailSelfCall {
			t.Fatalf("chain run %s is not tail-self-calling", id)
		}
	}
	for _, id := range []string{"lane-child", "audit-child"} {
		if runs[id].TailSelfCall {
			t.Fatalf("non-self callee %s reported a tail self call", id)
		}
	}
	if lane := runs["lane-child"]; lane.ParentUnitID != "port-0" || lane.ParentPhaseID != "port" || lane.CallDepth != 3 {
		t.Fatalf("unit-bound child linkage = %#v", lane)
	}
	if audit := runs["audit-child"]; audit.ParentUnitID != "" || audit.ParentPhaseID != "audit" {
		t.Fatalf("phase-call child linkage = %#v", audit)
	}

	wave := runs["wave-2"]
	if wave.State != string(engine.StateNeedsHuman) || wave.Reason != string(engine.ReasonProviderRetriesExhausted) {
		t.Fatalf("wave 2 state = %s/%s", wave.State, wave.Reason)
	}
	if wave.AutoResumeAt != 9_999 {
		t.Fatalf("wave 2 auto resume = %d", wave.AutoResumeAt)
	}
	if len(wave.Phases) != 2 || wave.Phases[0].PhaseID != "plan" ||
		wave.Phases[0].InterventionKind != engine.TakeoverInterventionKind ||
		wave.Phases[0].ThreadID != "thread-plan" {
		t.Fatalf("wave 2 phases = %#v", wave.Phases)
	}
	if wave.Phases[1].Status != "parked" || wave.Phases[1].Cause != "unit port-1 failed" {
		t.Fatalf("parked attempt = %#v", wave.Phases[1])
	}
	if len(wave.Units) != 4 {
		t.Fatalf("wave 2 units = %#v", wave.Units)
	}
	for index, want := range []struct {
		id     string
		status string
	}{
		{"port-0", store.WorkItemUnitDone},
		{"port-1", store.WorkItemUnitRunning},
		{"port-2", store.WorkItemUnitPending},
		{"port-join", store.WorkItemUnitPending},
	} {
		unit := wave.Units[index]
		if unit.UnitID != want.id || unit.Status != want.status {
			t.Fatalf("unit[%d] = %#v, want %s/%s", index, unit, want.id, want.status)
		}
	}
	if wave.Units[3].Kind != store.WorkItemUnitKindJoin || wave.Units[1].UnitAttempt != 2 {
		t.Fatalf("join/retry projection = %#v", wave.Units)
	}
	if runs["wave-1"].Units == nil || runs["wave-1"].Phases == nil {
		t.Fatal("a run with no units must carry empty lists, never null")
	}

	// Money is the tree's and the root's: the ledger row sits on a grandchild.
	if root.Spend == nil || root.Spend.CostUSD != 1.5 || root.Spend.WireCostUSD != 1.5 {
		t.Fatalf("root spend = %#v", root.Spend)
	}
	if root.Spend.EstimatedCostUSD != 0 || root.Spend.UnpricedRows != 0 {
		t.Fatalf("a fully wire-priced tree reported an estimate: %#v", root.Spend)
	}
	if root.Budget == nil || root.Budget.Kind != engine.BudgetKindUSD ||
		root.Budget.CeilingUSD != 25 || root.Budget.SpentUSD != 1.5 || root.Budget.Percent != 6 {
		t.Fatalf("root budget = %#v", root.Budget)
	}
	if root.Budget.Exhausted || root.Budget.RootItemID != "" {
		t.Fatalf("the root's own ceiling was reported as an ancestor's: %#v", root.Budget)
	}
	for _, id := range []string{"wave-1", "wave-2", "lane-child", "audit-child"} {
		if runs[id].Spend != nil || runs[id].Budget != nil {
			t.Fatalf("called run %s carries money: %#v", id, runs[id])
		}
	}
}

// The ceiling a run is under is not always the one it declared: a project
// profile's `reliability.per_item_budget` applies to every run that declared
// none, and the engine enforces it. A map that read the run's own column alone
// drew a profile-defaulted campaign as unbounded right up to the park.
func TestWorkflowGetRunMapBudgetResolvesTheProjectProfileDefault(t *testing.T) {
	app := newTestAppWithStore(t)
	// A run that declared NO budget, which is what makes the project profile the
	// only possible source of a ceiling.
	if err := app.store.CreateWorkItem(store.WorkItem{
		ID: "solo", ProjectID: defaultTestProjectID, Goal: "port everything",
		WorkflowID: "campaign", WorkflowScope: "project", Snapshot: campaignSnapshot(t),
		State: string(engine.StateRunning), Source: "manual", CreatedAt: 10, StartedAt: 10,
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.store.AppendUsage([]store.UsageLedgerRow{{
		CreatedAt: 47, ThreadID: "thread-plan", ProjectID: defaultTestProjectID,
		WorkItemID: "solo", TurnID: "turn-1", Provider: "claude", Model: "claude-opus-4-7",
		InputTokens: 100, OutputTokens: 200, CostUSD: 1.5, CostSource: "wire",
	}}); err != nil {
		t.Fatal(err)
	}

	view, err := app.WorkflowGetRunMap(t.Context(), "solo")
	if err != nil {
		t.Fatal(err)
	}
	if budget := runMapByID(t, view)["solo"].Budget; budget != nil {
		t.Fatalf("no run budget and no profile budget must be genuinely unbounded, got %#v", budget)
	}

	app.configDir = t.TempDir()
	project, err := app.store.GetProject(defaultTestProjectID)
	if err != nil {
		t.Fatal(err)
	}
	configDir := projectpkg.ConfigDir(app.configDir, project.Slug)
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(configDir, "profile.yaml"),
		[]byte("reliability:\n  per_item_budget:\n    usd: 4\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}

	view, err = app.WorkflowGetRunMap(t.Context(), "solo")
	if err != nil {
		t.Fatal(err)
	}
	budget := runMapByID(t, view)["solo"].Budget
	if budget == nil || budget.Kind != engine.BudgetKindUSD || budget.CeilingUSD != 4 {
		t.Fatalf("profile-defaulted ceiling = %#v", budget)
	}
	// $1.50 of a $4 ceiling, through the same numbers the enforcement compares.
	if budget.SpentUSD != 1.5 || budget.Percent != 38 || budget.Exhausted {
		t.Fatalf("profile-defaulted budget stand = %#v", budget)
	}
}

// A dollar total is a LOWER BOUND whenever the rate table could not price a
// row, and every reader of it has to say so. The map carries the halves apart
// for exactly that, and the ceiling it shows carries the same caveat the
// enforcement refuses to judge on.
func TestWorkflowGetRunMapSpendSaysWhatItCouldNotPrice(t *testing.T) {
	app := newTestAppWithStore(t)
	seedRunMapCampaign(t, app)
	if err := app.store.AppendUsage([]store.UsageLedgerRow{{
		CreatedAt: 48, ThreadID: "thread-port-1", ProjectID: defaultTestProjectID,
		WorkItemID: "wave-2", TurnID: "turn-2", Provider: "codex", Model: "not-a-model-anybody-prices",
		InputTokens: 1_000, OutputTokens: 2_000, CostSource: "none",
	}}); err != nil {
		t.Fatal(err)
	}

	view, err := app.WorkflowGetRunMap(t.Context(), "root")
	if err != nil {
		t.Fatal(err)
	}
	root := runMapByID(t, view)["root"]
	if root.Spend == nil || root.Spend.UnpricedRows != 1 {
		t.Fatalf("unpriced rows were dropped from the map's spend: %#v", root.Spend)
	}
	if root.Spend.CostUSD != 1.5 || root.Spend.WireCostUSD != 1.5 {
		t.Fatalf("an unpriceable row moved the total: %#v", root.Spend)
	}
	if root.Budget == nil || root.Budget.UnpricedRows != 1 || !root.Budget.Estimated {
		t.Fatalf("the ceiling did not carry the caveat its spend has: %#v", root.Budget)
	}
}

func TestWorkflowGetRunMapDegradesToRecordsOnlyWithoutASnapshot(t *testing.T) {
	app := newTestAppWithStore(t)
	seedRunMapCampaign(t, app)

	// A run that never froze a definition (it failed before its first entry) and
	// one whose column does not decode as a snapshot are the same records-only
	// answer. The column is CHECK-constrained to valid JSON, so the reachable
	// corruption is valid JSON of the wrong shape.
	if err := app.store.UpdateWorkItemRunStart("wave-1", nil, "", "", "", 20); err != nil {
		t.Fatal(err)
	}
	if err := app.store.UpdateWorkItemRunStart("wave-2", json.RawMessage(`["not a snapshot"]`), "", "", "", 40); err != nil {
		t.Fatal(err)
	}

	view, err := app.WorkflowGetRunMap(t.Context(), "root")
	if err != nil {
		t.Fatalf("an unreadable snapshot took the whole map away: %v", err)
	}
	runs := runMapByID(t, view)
	for _, id := range []string{"wave-1", "wave-2"} {
		run := runs[id]
		if !run.SkeletonMissing || len(run.Skeleton) != 0 || run.TailSelfCall {
			t.Fatalf("records-only run %s = %#v", id, run)
		}
	}
	// The two are NOT the same answer, which is the whole point of the second
	// field: a run that never froze a definition is ordinary history, and one
	// whose column will not decode is corruption somebody has to see.
	if runs["wave-1"].SkeletonError != "" {
		t.Fatalf("an absent snapshot was reported as corruption: %q", runs["wave-1"].SkeletonError)
	}
	if runs["wave-2"].SkeletonError == "" {
		t.Fatal("an undecodable snapshot rendered as a run that simply never froze one")
	}
	// The records themselves are untouched — that is what records-only means.
	if len(runs["wave-2"].Phases) != 2 || len(runs["wave-2"].Units) != 4 {
		t.Fatalf("records-only run lost its records: %#v", runs["wave-2"])
	}
	if runs["root"].SkeletonMissing || runs["root"].SkeletonError != "" || !runs["root"].TailSelfCall {
		t.Fatalf("a sibling's bad snapshot changed the root: %#v", runs["root"])
	}
}
