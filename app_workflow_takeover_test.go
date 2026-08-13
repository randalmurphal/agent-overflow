package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
)

func TestFailedFinalizeStartClearsTakeoverTransition(t *testing.T) {
	runner := newWorkflowAppRunner(&App{}, t.TempDir(), nil)
	runner.takeovers["thread"] = workflowTakeover{itemID: "item", transitioning: true}
	request := engine.RunRequest{
		Key:              engine.RunKey{ItemID: "item", PhaseID: "phase", Attempt: 2},
		Phase:            def.Phase{ID: "phase", Driver: def.DriverAgent, Shape: "fan-out"},
		PriorThreadID:    "thread",
		PromptMode:       engine.PromptContinue,
		FinalizeTakeover: true,
	}
	if err := runner.Start(t.Context(), request, func() {}, func(engine.Outcome) {}); err == nil {
		t.Fatal("finalize start with unsupported shape succeeded")
	}
	runner.mu.Lock()
	takeover := runner.takeovers["thread"]
	runner.mu.Unlock()
	if takeover.itemID != "item" || takeover.transitioning {
		t.Fatalf("takeover after failed finalize start = %+v, want transition cleared", takeover)
	}
	if err := runner.registerTakeover("item", "thread"); err != nil {
		t.Fatalf("steering re-registration after failed finalize start: %v", err)
	}
}

func TestWorkflowTakeoverRejectsHistoricalPhaseThread(t *testing.T) {
	app := newTestAppWithStore(t)
	for _, id := range []string{"old-phase-thread", "current-phase-thread"} {
		thread := testThread(id)
		thread.Mode = threadmode.ModeWorkflow
		if err := app.store.CreateThread(thread); err != nil {
			t.Fatal(err)
		}
	}
	workflow := def.Workflow{ID: "history", Phases: []def.Phase{
		{ID: "old", Driver: def.DriverAgent, Outputs: map[string]def.Variable{"ok": {Schema: def.JSONSchema{Type: "boolean"}}}},
		{ID: "current", Driver: def.DriverAgent, Outputs: map[string]def.Variable{"ok": {Schema: def.JSONSchema{Type: "boolean"}}}},
	}}
	snapshot, err := json.Marshal(engine.Snapshot{Workflow: workflow})
	if err != nil {
		t.Fatal(err)
	}
	item := store.WorkItem{
		ID: "history-item", ProjectID: defaultTestProjectID, Goal: "history",
		WorkflowID: workflow.ID, WorkflowScope: "project", Snapshot: snapshot,
		State: string(engine.StateNeedsHuman), Reason: string(engine.ReasonTakenOver),
		Source: "manual", CreatedAt: 1,
	}
	if err := app.store.CreateWorkItem(item); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []store.WorkItemPhase{
		{ItemID: item.ID, PhaseID: "old", Attempt: 1, ThreadID: "old-phase-thread", Status: "completed", StartedAt: 2, EndedAt: 3},
		{ItemID: item.ID, PhaseID: "current", Attempt: 1, ThreadID: "current-phase-thread", Status: "parked", StartedAt: 4, EndedAt: 5},
	} {
		if err := app.store.CreateWorkItemPhase(phase); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.initWorkflowEngine(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.workflowEngine.Close() })
	old, err := app.store.GetThread("old-phase-thread")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.prepareWorkflowTakeoverSend(old); err == nil || !strings.Contains(err.Error(), "not the current attempt") {
		t.Fatalf("historical takeover error = %v", err)
	}
}

func TestWorkflowTakeoverSteersSchemaLessThenCompletesThroughGate(t *testing.T) {
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	writeTakeoverWorkflowFixture(t, configRoot)
	capturePath := filepath.Join(t.TempDir(), "takeover-turns.ndjson")
	counterPath := filepath.Join(t.TempDir(), "takeover-counter")
	if _, err := app.settings.Update(map[string]any{
		"codexBinaryPath": writeWorkflowTakeoverCodex(t, capturePath, counterPath),
	}); err != nil {
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
		projectRow.ID, "takeover-flow", "shared", "exercise takeover",
		json.RawMessage(`{"goal":"exercise takeover"}`), nil, "", false,
	)
	if err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, app, item.ID, engine.StateNeedsHuman, engine.ReasonQuestion)
	detail, err := app.WorkflowGetItem(item.ID)
	if err != nil || len(detail.Phases) != 1 {
		t.Fatalf("question detail = %+v, %v", detail, err)
	}
	threadID := detail.Phases[0].ThreadID
	steerComplete := make(chan struct{}, 1)
	unsubscribe := app.subscribeThreadTurnObserver(threadID, func(_ string, event provider.ProviderEvent) {
		if event.Kind == provider.EventTurnComplete {
			select {
			case steerComplete <- struct{}{}:
			default:
			}
		}
	})
	if err := app.SendMessage(threadID, "I fixed the workspace; prepare to finalize.", nil); err != nil {
		unsubscribe()
		t.Fatal(err)
	}
	select {
	case <-steerComplete:
	case <-time.After(8 * time.Second):
		unsubscribe()
		t.Fatal("schema-less steering turn did not complete")
	}
	unsubscribe()
	requireWorkflowItemState(t, app.store, item.ID, engine.StateNeedsHuman, engine.ReasonTakenOver)
	if err := app.WorkflowCompleteTakeover(item.ID); err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")

	completed, err := app.WorkflowGetItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(completed.Phases) != 2 || completed.Phases[1].ThreadID != threadID || completed.Phases[1].Status != "completed" {
		t.Fatalf("completed takeover detail = %+v", completed)
	}
	turns := readCapturedWorkflowTurns(t, capturePath)
	if len(turns) != 3 {
		t.Fatalf("captured turns = %d, want initial + steer + finalize", len(turns))
	}
	if len(turns[0].Params.OutputSchema) == 0 || len(turns[1].Params.OutputSchema) != 0 || len(turns[2].Params.OutputSchema) == 0 {
		t.Fatalf("schema sequence = [%s, %s, %s], want attached/schema-less/attached",
			turns[0].Params.OutputSchema, turns[1].Params.OutputSchema, turns[2].Params.OutputSchema)
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
	if len(userMessages) != 3 || !strings.Contains(userMessages[1], "I fixed the workspace") || !strings.Contains(userMessages[2], "Do not redo the original phase") {
		t.Fatalf("takeover user turns = %#v", userMessages)
	}
}

func TestWorkflowTakeoverInterruptsLiveTurnBeforeSteering(t *testing.T) {
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	writeLiveTakeoverWorkflowFixture(t, configRoot)
	claudeBinary, argsPath := writeLiveTakeoverClaude(t)
	if _, err := app.settings.Update(map[string]any{
		"claudeBinaryPath": claudeBinary,
	}); err != nil {
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
		projectRow.ID, "live-takeover", "shared", "interrupt me",
		json.RawMessage(`{"goal":"interrupt me"}`), nil, "", false,
	)
	if err != nil {
		t.Fatal(err)
	}
	threadID := waitForRunningWorkflowThread(t, app, item.ID)

	completions := make(chan struct{}, 2)
	unsubscribe := app.subscribeThreadTurnObserver(threadID, func(_ string, event provider.ProviderEvent) {
		if event.Kind == provider.EventTurnComplete {
			select {
			case completions <- struct{}{}:
			default:
			}
		}
	})
	if err := app.SendMessage(threadID, "Steer after the interrupt.", nil); err != nil {
		unsubscribe()
		t.Fatal(err)
	}
	for count := 0; count < 2; count++ {
		select {
		case <-completions:
		case <-time.After(8 * time.Second):
			unsubscribe()
			t.Fatalf("received %d of 2 expected interrupt/steer completions", count)
		}
	}
	unsubscribe()
	requireWorkflowItemState(t, app.store, item.ID, engine.StateNeedsHuman, engine.ReasonTakenOver)
	if err := app.WorkflowCompleteTakeover(item.ID); err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")

	items, err := app.store.ListItems(threadID)
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	for _, timelineItem := range items {
		if timelineItem.Kind == "error" && timelineItem.Summary == "Stopped by user" {
			stopped = true
		}
	}
	if !stopped {
		t.Fatalf("live takeover did not persist the interrupt marker: %+v", items)
	}
	argsPayload, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	invocations := strings.Split(strings.TrimSpace(string(argsPayload)), "\n")
	if len(invocations) != 3 || !strings.Contains(invocations[0], "--json-schema") || strings.Contains(invocations[1], "--json-schema") || !strings.Contains(invocations[2], "--json-schema") {
		t.Fatalf("Claude takeover schema process sequence = %#v, want attached/schema-less/attached", invocations)
	}
}

type capturedWorkflowTurn struct {
	Params struct {
		OutputSchema json.RawMessage `json:"outputSchema"`
	} `json:"params"`
}

func readCapturedWorkflowTurns(t *testing.T, path string) []capturedWorkflowTurn {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(payload)), "\n")
	turns := make([]capturedWorkflowTurn, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		var turn capturedWorkflowTurn
		if err := json.Unmarshal([]byte(line), &turn); err != nil {
			t.Fatalf("decode captured turn: %v\n%s", err, line)
		}
		turns = append(turns, turn)
	}
	return turns
}

func requireWorkflowItemState(t *testing.T, st *store.Store, itemID string, state engine.State, reason engine.Reason) {
	t.Helper()
	item, err := st.GetWorkItem(itemID)
	if err != nil {
		t.Fatal(err)
	}
	if item.State != string(state) || item.Reason != string(reason) {
		t.Fatalf("item state = %s(%s), want %s(%s)", item.State, item.Reason, state, reason)
	}
}

func writeTakeoverWorkflowFixture(t *testing.T, configRoot string) {
	t.Helper()
	dir := filepath.Join(configRoot, "workflows")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	definition := `id: takeover-flow
name: Takeover flow
inputs:
  goal:
    schema:
      type: string
phases:
  - id: execute
    driver: agent
    provider: codex
    model: gpt-5
    access: read-only
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
	if err := os.WriteFile(filepath.Join(dir, "takeover-flow.yaml"), []byte(definition), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "execute.md"), []byte("Execute {{goal}}"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeLiveTakeoverWorkflowFixture(t *testing.T, configRoot string) {
	t.Helper()
	dir := filepath.Join(configRoot, "workflows")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	definition := `id: live-takeover
name: Live takeover
inputs:
  goal:
    schema:
      type: string
phases:
  - id: execute
    driver: agent
    provider: claude
    model: claude-opus-4-7
    access: read-only
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
	if err := os.WriteFile(filepath.Join(dir, "live-takeover.yaml"), []byte(definition), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "execute.md"), []byte("Execute {{goal}}"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeLiveTakeoverClaude(t *testing.T) (string, string) {
	t.Helper()
	turnFile := filepath.Join(t.TempDir(), "turn-counter")
	argsFile := filepath.Join(t.TempDir(), "args")
	script := `#!/bin/bash
turn_file="__TURN_FILE__"
printf '%s\n' "$*" >> "__ARGS_FILE__"
while IFS= read -r line; do
  case "$line" in
    *'"type":"control_request"'*'"subtype":"interrupt"'* | *'"subtype":"interrupt"'*'"type":"control_request"'*)
      reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
      printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"
      printf '%s\n' '{"type":"result","subtype":"error_during_execution","is_error":true,"terminal_reason":"aborted_streaming"}'
      continue
      ;;
  esac
  turn=0
  if [ -f "$turn_file" ]; then turn=$(cat "$turn_file"); fi
  turn=$((turn+1))
  printf '%s' "$turn" > "$turn_file"
  printf '%s\n' '{"type":"system","subtype":"init","session_id":"live-takeover","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}'
  if [ "$turn" -eq 1 ]; then
    continue
  fi
  if [ "$turn" -eq 2 ]; then
    printf '%s\n' '{"type":"result","subtype":"success","is_error":false}'
    continue
  fi
  printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":{"status":"done","outputs":{"complete":true},"question":null,"reason":null}}'
done
`
	script = strings.ReplaceAll(script, "__TURN_FILE__", turnFile)
	script = strings.ReplaceAll(script, "__ARGS_FILE__", argsFile)
	path := filepath.Join(t.TempDir(), "workflow-live-takeover-claude.sh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path, argsFile
}

func waitForRunningWorkflowThread(t *testing.T, app *App, itemID string) string {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		item, err := app.store.GetWorkItem(itemID)
		if err == nil && item.State == string(engine.StateRunning) {
			phases, phaseErr := app.store.ListWorkItemPhases(itemID)
			if phaseErr == nil && len(phases) > 0 && phases[len(phases)-1].ThreadID != "" {
				if _, active, activeErr := app.store.GetActiveTurn(phases[len(phases)-1].ThreadID); activeErr == nil && active {
					return phases[len(phases)-1].ThreadID
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("workflow item %s did not expose a running phase thread", itemID)
	return ""
}

func writeWorkflowTakeoverCodex(t *testing.T, capturePath, counterPath string) string {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/bash
while IFS= read -r line; do
  id=$(/bin/echo "$line" | /usr/bin/grep -o '"id":[0-9]*' | /usr/bin/head -1 | /usr/bin/grep -o '[0-9]*')
  if [ -z "$id" ]; then continue; fi
  if /bin/echo "$line" | /usr/bin/grep -q '"method":"initialize"'; then
    printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
    continue
  fi
  if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/start"'; then
    printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"takeover-provider-thread"}}}\n' "$id"
    continue
  fi
  if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/resume"'; then
    printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"takeover-provider-thread"}}}\n' "$id"
    continue
  fi
  if /bin/echo "$line" | /usr/bin/grep -q '"method":"turn/start"'; then
    printf '%%s\n' "$line" >> %q
    turn=0
    if [ -f %q ]; then turn=$(/bin/cat %q); fi
    turn=$((turn+1))
    printf '%%s' "$turn" > %q
    printf '{"jsonrpc":"2.0","id":%%s,"result":{"turn":{"id":"turn-%%s"}}}\n' "$id" "$turn"
    printf '{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"takeover-provider-thread","turn":{"id":"turn-%%s"}}}\n' "$turn"
    if [ "$turn" -eq 1 ]; then
      text='{"status":"question","outputs":null,"question":"Take over?","reason":null}'
    elif [ "$turn" -eq 2 ]; then
      text='Steering acknowledged.'
    else
      text='{"status":"done","outputs":{"complete":true},"question":null,"reason":null}'
    fi
    escaped=$(/usr/bin/printf '%%s' "$text" | /usr/bin/sed 's/\\/\\\\/g; s/"/\\"/g')
    printf '{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"takeover-provider-thread","turnId":"turn-%%s","item":{"id":"message-%%s","type":"agentMessage","text":"%%s"}}}\n' "$turn" "$turn" "$escaped"
    printf '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"takeover-provider-thread","turn":{"id":"turn-%%s","status":"completed"}}}\n' "$turn"
    continue
  fi
  printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
done
`, capturePath, counterPath, counterPath, counterPath)
	path := filepath.Join(t.TempDir(), "workflow-takeover-codex.sh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
