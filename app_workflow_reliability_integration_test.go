package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflow/profile"
)

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

	item, err := app.WorkflowEnqueueItem(projectRow.ID, "reliability-flow", "shared", "stall", json.RawMessage(`{}`), nil, false)
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

	item, err := app.WorkflowEnqueueItem(projectRow.ID, "reliability-flow", "shared", "retry deaths", json.RawMessage(`{}`), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, app, item.ID, engine.StateNeedsHuman, engine.ReasonRetriesExhausted)
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
	item, err := app.WorkflowEnqueueItem(
		projectRow.ID, "reliability-flow", "shared", "budget", json.RawMessage(`{}`),
		&profile.Budget{Tokens: &tokenLimit}, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, app, item.ID, engine.StateNeedsHuman, engine.ReasonBudgetExhausted)
	detail, err := app.WorkflowGetItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Phases) != 1 || detail.Phases[0].PhaseID != "first" || detail.Phases[0].Status != "completed" {
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
	spend, err := (workflowSpendSource{store: app.store}).ItemSpend(t.Context(), "item")
	if err != nil {
		t.Fatal(err)
	}
	if spend.Tokens != 4_000_000 || math.Abs(spend.USD-16.425) > 1e-12 {
		t.Fatalf("composed spend = %+v", spend)
	}

	if err := app.store.AppendUsage([]store.UsageLedgerRow{{WorkItemID: "unknown", Model: "future-model", InputTokens: 1, CostSource: "none"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := (workflowSpendSource{store: app.store}).ItemSpend(t.Context(), "unknown"); err == nil || !strings.Contains(err.Error(), "no USD rate") {
		t.Fatalf("unknown model error = %v", err)
	}
}

func startWorkflowEngineForTest(t *testing.T, app *App, configRoot string) {
	t.Helper()
	if err := app.initWorkflowEngine(configRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if app.workflowEngine != nil {
			_ = app.workflowEngine.Close()
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
