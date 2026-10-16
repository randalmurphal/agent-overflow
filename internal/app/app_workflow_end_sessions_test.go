package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/testutil"
	"agent-overflow/internal/workflow/engine"
)

// Pause keeps the paused members' sessions, as it always did; Pause and End
// Sessions also ends them, and Resume starts them again on the same provider
// session (docs/specs/standing-agents.md, "Interrupt and end session").
func TestWorkflowPauseAndEndSessionsEndsThePausedMembersSessions(t *testing.T) {
	t.Parallel()
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
	startLog := filepath.Join(t.TempDir(), "starts")
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": writeRecordingStallClaude(t, startLog)}); err != nil {
		t.Fatal(err)
	}
	projectRow := testutil.EnsureProject(t, app.store, t.TempDir())
	projectRow = mustReloadProject(t, app.store, projectRow.ID)
	writeReliabilityProfile(t, configRoot, projectRow.Slug, "watchdog: 1h\n  backoff: [5ms]\n")
	startWorkflowEngineForTest(t, app, configRoot)
	ctx := context.Background()

	item, err := app.WorkflowStartRun(projectRow.ID, "reliability-flow", "shared", "hold", json.RawMessage(`{}`), nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	threadID := awaitLivePhaseSession(t, app, item.ID)
	// The provider session the resume must continue on.
	waitUntil(t, 8*time.Second, func() bool {
		row, err := app.store.GetThread(threadID)
		return err == nil && row.SessionRef == "stall"
	})
	// A resumable transcript, which a cold Claude resume is checked against.
	row, err := app.store.GetThread(threadID)
	if err != nil {
		t.Fatal(err)
	}
	writeTranscriptContent(t, testProviderHome(t, app), row.WorkspacePath, "stall",
		`{"type":"user","uuid":"u0","parentUuid":null,"sessionId":"stall","message":{"role":"user","content":"hold"}}
{"type":"assistant","uuid":"a0","parentUuid":"u0","sessionId":"stall","message":{"role":"assistant","content":[{"type":"text","text":"holding"}]}}
`)

	if err := app.WorkflowPauseItem(ctx, item.ID); err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, app, item.ID, engine.StateNeedsHuman, engine.ReasonPaused)
	if _, live := app.sessionManager().get(threadID); !live {
		t.Fatal("a plain Pause ended the member's session")
	}

	if err := app.WorkflowResumeItem(ctx, item.ID, "", false); err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, app, item.ID, engine.StateRunning, "")
	if err := app.WorkflowPauseItemAndEndSessions(ctx, item.ID); err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, app, item.ID, engine.StateNeedsHuman, engine.ReasonPaused)
	if _, live := app.sessionManager().get(threadID); live {
		t.Fatal("Pause and End Sessions left the member's session running")
	}
	if starts := readLines(t, startLog); len(starts) != 1 {
		t.Fatalf("provider starts before the second resume = %v, want the one process", starts)
	}

	if err := app.WorkflowResumeItem(ctx, item.ID, "", false); err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, app, item.ID, engine.StateRunning, "")
	deadline := time.Now().Add(8 * time.Second)
	for {
		starts := readLines(t, startLog)
		if len(starts) == 2 {
			if !strings.Contains(starts[1], "--resume stall") {
				t.Fatalf("the resumed member started a new provider session: %q", starts[1])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Resume did not start the member's session again: starts = %v", starts)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := app.WorkflowPauseItem(ctx, item.ID); err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, app, item.ID, engine.StateNeedsHuman, engine.ReasonPaused)
}

// awaitLivePhaseSession waits for the run's current phase thread to hold a
// live session and returns that thread.
func awaitLivePhaseSession(t *testing.T, app *App, itemID string) string {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		phases, err := app.store.ListWorkItemPhases(itemID)
		if err != nil {
			t.Fatal(err)
		}
		for _, phase := range phases {
			if phase.ThreadID == "" {
				continue
			}
			if _, live := app.sessionManager().get(phase.ThreadID); live {
				return phase.ThreadID
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run %s never held a live phase session", itemID)
	return ""
}

// writeRecordingStallClaude is writeStallingWorkflowClaude that records each
// process start with its arguments.
func writeRecordingStallClaude(t *testing.T, startLog string) string {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/bash
printf '%%s\n' "$*" >> %q
while IFS= read -r line; do
  if [[ "$line" == *'"subtype":"interrupt"'* ]]; then
    reqid=$(printf '%%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
    printf '{"type":"control_response","response":{"subtype":"success","request_id":"%%s","response":{}}}\n' "$reqid"
    continue
  fi
  printf '%%s\n' '{"type":"system","subtype":"init","session_id":"stall","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}'
done
`, startLog)
	path := writeExecutable(t, "recording-stall-claude.sh", script)
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	return path
}
