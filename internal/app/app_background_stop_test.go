package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/triage"
)

// Stop All is one call that stops each named background task once, by its
// provider's primitive (app_background_stop.go).

// writeClaudeStopRecorderBinary acks every stop_task control_request and
// appends its task id to logPath, refusing the task named refused the way
// the CLI refuses a stop.
func writeClaudeStopRecorderBinary(t *testing.T, logPath, refused string) string {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
set -u
while IFS= read -r line; do
    case "$line" in
        *'"subtype":"stop_task"'*)
            reqid=$(printf '%%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
            task=$(printf '%%s' "$line" | sed -n 's/.*"task_id":"\([^"]*\)".*/\1/p')
            printf '%%s\n' "$task" >> %s
            if [ "$task" = %s ]; then
                printf '{"type":"control_response","response":{"subtype":"error","request_id":"%%s","error":"No task found"}}\n' "$reqid"
            else
                printf '{"type":"control_response","response":{"subtype":"success","request_id":"%%s","response":{}}}\n' "$reqid"
            fi
            ;;
    esac
done
`, shellQuote(logPath), shellQuote(refused))
	path := filepath.Join(t.TempDir(), "claude-stop-recorder.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write stop recorder: %v", err)
	}
	return path
}

func newClaudeStopFixture(t *testing.T, refused string) (*agentKillFixture, string) {
	t.Helper()
	app, dbPath := newTestAppWithStorePath(t)
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})
	thread, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-sonnet-4-6", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	f := &agentKillFixture{app: app, thread: thread, dbPath: dbPath}
	f.handle(t, provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: thread.ID + ":turn"}, nil)
	stopLog := filepath.Join(t.TempDir(), "stops.log")
	sess, err := claude.NewSession(context.Background(), thread.ID,
		claude.Config{Binary: writeClaudeStopRecorderBinary(t, stopLog, refused), WorkDir: t.TempDir()},
		func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put(thread.ID, session{Provider: string(provider.Claude), Token: "agent-stop", Claude: sess})
	return f, stopLog
}

func stoppedTasks(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read stop log: %v", err)
	}
	tasks := strings.Fields(string(data))
	slices.Sort(tasks)
	return tasks
}

// Each named background agent gets its own stop_task, nested async agents
// included; the shells and foreground agents under a stopped agent get
// none (they die with it, and a parked agent's shell stopped first would
// wake it); a shell whose agent is not stopped, one under an unnamed
// nested agent (which outlives its parent's stop) included, and the main
// thread's own shell, are stopped themselves. A resumed agent is stopped by its carrier
// and covers the shells at its root. A refused stop fails alone, and a
// launch that is not running has ended.
func TestStopBackgroundTasksStopsEachAgentOnceAndItsShellsWithIt(t *testing.T) {
	f, stopLog := newClaudeStopFixture(t, "task-refused")
	f.launchAgent(t, "agent", "task-agent", "")
	f.launchShell(t, "agent-sh1", "task-agent-sh1", "agent")
	f.launchShell(t, "agent-sh2", "task-agent-sh2", "agent")
	f.handle(t, provider.ProviderEvent{Kind: provider.EventToolStart, ItemID: "fg", ItemType: "Agent", ParentToolUseID: "agent"},
		map[string]any{"toolName": "Agent", "input": map[string]any{"description": "Agent fg", "prompt": "help"}})
	f.launchShell(t, "fg-sh", "task-fg-sh", "fg")
	f.launchAgent(t, "nested", "task-nested", "agent")
	f.launchAgent(t, "nested-free", "task-nested-free", "agent")
	f.launchShell(t, "nested-free-sh", "task-nested-free-sh", "nested-free")
	f.launchAgent(t, "parked", "task-parked", "")
	f.launchShell(t, "parked-sh", "task-parked-sh", "parked")
	f.stopAgent(t, "parked", "task-parked")
	f.launchAgent(t, "unnamed", "task-unnamed", "")
	f.launchShell(t, "unnamed-sh", "task-unnamed-sh", "unnamed")
	f.launchShell(t, "main-sh", "task-main-sh", "")
	f.launchAgent(t, "finished", "task-finished", "")
	f.stopAgent(t, "finished", "task-finished")
	f.launchAgent(t, "refused", "task-refused", "")
	f.launchAgent(t, "root", "task-root", "")
	f.stopAgent(t, "root", "task-root")
	f.handle(t, provider.ProviderEvent{Kind: provider.EventToolStart, ItemID: "carrier", ItemType: "SendMessage"},
		map[string]any{"toolName": "SendMessage", "input": map[string]any{"to": "task-root", "message": "continue"}})
	f.handle(t, provider.ProviderEvent{Kind: provider.EventToolStart, ItemID: "carrier"},
		map[string]any{"task_id": "task-root", "task_type": "local_agent", "resumes_tool_use_id": "root",
			"description": "Agent root", "subagent_type": "general-purpose", provider.MetaTranscriptRootIDKey: "root"})
	f.handle(t, provider.ProviderEvent{Kind: provider.EventToolComplete, ItemID: "carrier", Content: "Resuming agent"},
		map[string]any{"is_background": true})
	f.launchShell(t, "root-sh", "task-root-sh", "root")

	named := []string{"agent", "agent-sh1", "agent-sh2", "fg", "fg-sh", "nested", "parked", "parked-sh",
		"unnamed-sh", "main-sh", "finished", "refused", "carrier", "root-sh", "nested-free-sh", "nope", "agent"}
	results, err := f.app.StopBackgroundTasks(f.thread.ID, named)
	if err != nil {
		t.Fatalf("StopBackgroundTasks: %v", err)
	}
	got := map[string]string{}
	var order []string
	for _, result := range results {
		got[result.LaunchItemID] = result.Outcome
		order = append(order, result.LaunchItemID)
		if (result.Outcome == BackgroundStopFailed) != (result.Error != "") {
			t.Errorf("%s: outcome %s with error %q", result.LaunchItemID, result.Outcome, result.Error)
		}
	}
	if want := named[:len(named)-1]; !slices.Equal(order, want) {
		t.Fatalf("results in order %v, want one per distinct launch %v", order, want)
	}
	want := map[string]string{
		"agent": BackgroundStopStopping, "agent-sh1": BackgroundStopWithAgent, "agent-sh2": BackgroundStopWithAgent,
		"fg": BackgroundStopWithAgent, "fg-sh": BackgroundStopWithAgent, "nested": BackgroundStopStopping,
		"parked": BackgroundStopStopping, "parked-sh": BackgroundStopWithAgent, "unnamed-sh": BackgroundStopStopping,
		"main-sh": BackgroundStopStopping, "finished": BackgroundStopEnded, "refused": BackgroundStopFailed,
		"carrier": BackgroundStopStopping, "root-sh": BackgroundStopWithAgent, "nope": BackgroundStopEnded,
		"nested-free-sh": BackgroundStopStopping,
	}
	for id, outcome := range want {
		if got[id] != outcome {
			t.Errorf("%s: outcome %q, want %q", id, got[id], outcome)
		}
	}
	sent := []string{"task-agent", "task-main-sh", "task-nested", "task-nested-free-sh", "task-parked", "task-refused", "task-root",
		"task-unnamed-sh"}
	slices.Sort(sent)
	if tasks := stoppedTasks(t, stopLog); !slices.Equal(tasks, sent) {
		t.Fatalf("stop_task sent for %v, want %v", tasks, sent)
	}
}

// A call that names nothing sends nothing, and a thread with no live
// session fails each stop with the reason rather than failing the call.
func TestStopBackgroundTasksReportsAMissingSessionPerTask(t *testing.T) {
	f, stopLog := newClaudeStopFixture(t, "")
	f.launchAgent(t, "agent", "task-agent", "")
	if results, err := f.app.StopBackgroundTasks(f.thread.ID, []string{" ", ""}); err != nil || len(results) != 0 {
		t.Fatalf("an empty call = %+v, %v; want no results", results, err)
	}
	if _, ok := f.app.sessionManager().take(f.thread.ID); !ok {
		t.Fatal("the fixture has no session to take")
	}
	results, err := f.app.StopBackgroundTasks(f.thread.ID, []string{"agent"})
	if err != nil {
		t.Fatalf("StopBackgroundTasks: %v", err)
	}
	if len(results) != 1 || results[0].Outcome != BackgroundStopFailed || !strings.Contains(results[0].Error, "no active session") {
		t.Fatalf("results = %+v, want the agent failed for the missing session", results)
	}
	if tasks := stoppedTasks(t, stopLog); len(tasks) != 0 {
		t.Fatalf("stop_task sent for %v, want none", tasks)
	}
}

// A Codex thread interrupts each named live subagent and cleans its
// terminals once for the named terminals; with no live session each stop
// fails with the reason. A launch that is not live has ended.
func TestStopBackgroundTasksClassifiesCodexLaunches(t *testing.T) {
	app, _ := setupE2EApp(t)
	thread, err := createTestThread(t, app, string(provider.Codex), t.TempDir(), "gpt-5", "chat")
	if err != nil {
		t.Fatalf("create Codex thread: %v", err)
	}
	now := time.Now().UnixMilli()
	seedCodexSubagentLaunchRow(t, app, thread.ID, "spawn-1", now)
	seedClaudeBackgroundTaskRow(t, app, thread.ID, "terminal-1", "", now)

	results, err := app.StopBackgroundTasks(thread.ID, []string{"spawn-1", "terminal-1", "gone"})
	if err != nil {
		t.Fatalf("StopBackgroundTasks: %v", err)
	}
	outcomes := map[string]BackgroundTaskStop{}
	for _, result := range results {
		outcomes[result.LaunchItemID] = result
	}
	for _, id := range []string{"spawn-1", "terminal-1"} {
		if r := outcomes[id]; r.Outcome != BackgroundStopFailed || !strings.Contains(r.Error, "no active session") {
			t.Errorf("%s: %+v, want failed for the missing session", id, r)
		}
	}
	if r := outcomes["gone"]; r.Outcome != BackgroundStopEnded {
		t.Errorf("gone: %+v, want ended", r)
	}
	if !strings.Contains(outcomes["spawn-1"].Error, "stop Codex subagent") || !strings.Contains(outcomes["terminal-1"].Error, "clean codex background terminals") {
		t.Errorf("spawn-1 %q and terminal-1 %q, want the subagent interrupted and the terminals cleaned", outcomes["spawn-1"].Error, outcomes["terminal-1"].Error)
	}
}

// runBounded runs every call, never more than the bound at once.
func TestRunBoundedKeepsTheBound(t *testing.T) {
	var inFlight, peak atomic.Int32
	var mu sync.Mutex
	ran := map[int]bool{}
	runBounded(40, 4, func(i int) {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		inFlight.Add(-1)
		mu.Lock()
		ran[i] = true
		mu.Unlock()
	})
	if len(ran) != 40 || peak.Load() > 4 || peak.Load() < 2 {
		t.Fatalf("ran %d calls with a peak of %d in flight, want 40 with at most 4", len(ran), peak.Load())
	}
}
