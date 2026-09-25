package triage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// runStates reads the live list as App.ListLiveBackgroundTasks does and
// returns each launch's decorated meta by id.
func runStates(t *testing.T, router *Router, st *store.Store, threadID string) map[string]map[string]any {
	t.Helper()
	items, err := st.ListLiveBackgroundTasks(threadID, 0)
	if err != nil {
		t.Fatalf("list live background tasks: %v", err)
	}
	decorated, err := router.DecorateAgentRunStates(threadID, items)
	if err != nil {
		t.Fatalf("decorate run states: %v", err)
	}
	out := map[string]map[string]any{}
	for _, item := range decorated {
		if item.CompletionOf == "" {
			out[item.ID] = decodeItemMetaMap(t, item.Meta)
		}
	}
	return out
}

func seedAgentReport(t *testing.T, st *store.Store, threadID, parentID, id, text string) {
	t.Helper()
	now := time.Now().UnixMilli()
	if _, err := appendSeed(st, store.Item{
		ID: id, ThreadID: threadID, Kind: itemKindAssistantText, Role: "assistant", Status: statusCompleted,
		ParentID: parentID, Summary: text, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed report %s: %v", id, err)
	}
}

func assertRunState(t *testing.T, got map[string]any, want map[string]any) {
	t.Helper()
	for _, key := range []string{metaKeySubagentRunState, metaKeySubagentParkedCommands, metaKeySubagentParkedReportID, metaKeySubagentParkedReportPreview} {
		value, has := got[key]
		expected, wanted := want[key]
		if has != wanted {
			t.Errorf("%s present=%v (%v), want present=%v (%v)", key, has, value, wanted, expected)
			continue
		}
		if wanted {
			if encoded, _ := json.Marshal(value); string(encoded) != mustJSON(t, expected) {
				t.Errorf("%s = %s, want %s", key, encoded, mustJSON(t, expected))
			}
		}
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(encoded)
}

// A two-run parked agent through the park model: running, parked on its
// live shell with its report, running again after the wake, parked with
// the newer report, and done at the final stop. The state is read from
// the stops: the launch row stores none of it, and a park does not move
// its revision. The preview is the head of the report the stop carried.
func TestAgentRunStateFollowsTheParkModel(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "agent", "task-agent", "")
	parkLaunchShell(t, router, "t1", "shell", "task-shell", "agent")

	states := runStates(t, router, st, "t1")
	assertRunState(t, states["agent"], map[string]any{metaKeySubagentRunState: subagentRunRunning})
	if _, has := states["shell"][metaKeySubagentRunState]; has {
		t.Errorf("a background shell has no run state, got %v", states["shell"])
	}

	seedAgentReport(t, st, "t1", "agent", "report-1", "Round one: waiting on the gate.")
	router.DrainWireItemRefresh()
	before := mustGetItem(t, st, "t1", "agent")
	parkStop(t, router, "t1", "agent", "task-agent", "  Round one: waiting on the gate.  ", "u1")
	router.DrainWireItemRefresh()
	if after := mustGetItem(t, st, "t1", "agent"); after.Rev != before.Rev {
		t.Errorf("the park moved the launch's rev from %d to %d", before.Rev, after.Rev)
	}
	assertRunState(t, runStates(t, router, st, "t1")["agent"], map[string]any{
		metaKeySubagentRunState:            subagentRunParked,
		metaKeySubagentParkedCommands:      1,
		metaKeySubagentParkedReportID:      "report-1",
		metaKeySubagentParkedReportPreview: "Round one: waiting on the gate.",
	})

	nextMillisecond()
	parkShellDone(t, router, "t1", "shell", "task-shell", "agent")
	parkWake(t, router, "t1", "agent", "task-agent", "shell", "task-shell", nil)
	assertRunState(t, runStates(t, router, st, "t1")["agent"], map[string]any{metaKeySubagentRunState: subagentRunRunning})

	parkLaunchShell(t, router, "t1", "shell-2", "task-shell-2", "agent")
	seedAgentReport(t, st, "t1", "agent", "report-2", "Round two: waiting again.")
	parkStop(t, router, "t1", "agent", "task-agent", "Round two: waiting again.", "u2")
	assertRunState(t, runStates(t, router, st, "t1")["agent"], map[string]any{
		metaKeySubagentRunState:            subagentRunParked,
		metaKeySubagentParkedCommands:      1,
		metaKeySubagentParkedReportID:      "report-2",
		metaKeySubagentParkedReportPreview: "Round two: waiting again.",
	})

	parkShellDone(t, router, "t1", "shell-2", "task-shell-2", "agent")
	parkWake(t, router, "t1", "agent", "task-agent", "shell-2", "task-shell-2", nil)
	// The final stop's task_updated stashes before its notification: a
	// read between the two says running, and the sibling the
	// notification writes settles it.
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "agent",
		Meta: parkMeta(t, map[string]any{"task_id": "task-agent", "tool_use_id": "agent", "status": "completed", "source": "task_updated"}),
	})
	assertRunState(t, runStates(t, router, st, "t1")["agent"], map[string]any{metaKeySubagentRunState: subagentRunRunning})
	parkHandle(t, router, provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskNotification, ThreadID: "t1", ItemID: "agent", Content: "DONE",
		Meta: parkMeta(t, map[string]any{"task_id": "task-agent", "tool_use_id": "agent", "status": "completed", "uuid": "u3"}),
	})
	assertRunState(t, runStates(t, router, st, "t1")["agent"], map[string]any{metaKeySubagentRunState: subagentRunDone})

	stored, found, err := st.GetThreadItemForWrite("t1", "agent")
	if err != nil || !found {
		t.Fatalf("launch: found=%v err=%v", found, err)
	}
	if strings.Contains(stored.Meta, "subagentRunState") || strings.Contains(stored.Meta, "subagentParked") {
		t.Errorf("the launch row stores its run state: %s", stored.Meta)
	}
}

// A session death settles a parked agent from its stash; the sibling it
// writes says so, and the state is ended.
func TestAgentRunStateEndsAtSessionDeath(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "agent", "task-agent", "")
	parkLaunchShell(t, router, "t1", "shell", "task-shell", "agent")
	parkStop(t, router, "t1", "agent", "task-agent", "WAITING", "u1")

	if _, err := router.SettleBackgroundLaunchesForSessionEnd("t1"); err != nil {
		t.Fatalf("session-end settle: %v", err)
	}
	assertRunState(t, runStates(t, router, st, "t1")["agent"], map[string]any{metaKeySubagentRunState: subagentRunEnded})
}

// A resume carrier parks at its own sibling on the commands and serves
// the report at its transcript root, where every run's rows are; the root
// it resumed ended before it.
func TestAgentRunStateOfAParkedCarrierReadsItsRoot(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "root", "task-agent", "")
	parkStop(t, router, "t1", "root", "task-agent", "ROUND 1", "u1")

	resumeAgent(t, router, "t1", "carrier", map[string]any{
		"task_id": "task-agent", "task_type": "local_agent", "resumes_tool_use_id": "root",
		"description": "Spike root", "subagent_type": "general-purpose", provider.MetaTranscriptRootIDKey: "root",
	})
	parkLaunchShell(t, router, "t1", "shell", "task-shell", "root")
	parkLaunchShell(t, router, "t1", "shell-2", "task-shell-2", "root")
	seedAgentReport(t, st, "t1", "root", "report-2", "Round two report")
	parkStop(t, router, "t1", "carrier", "task-agent", "Round two report", "u2")

	states := runStates(t, router, st, "t1")
	assertRunState(t, states["root"], map[string]any{metaKeySubagentRunState: subagentRunDone})
	assertRunState(t, states["carrier"], map[string]any{
		metaKeySubagentRunState:            subagentRunParked,
		metaKeySubagentParkedCommands:      2,
		metaKeySubagentParkedReportID:      "report-2",
		metaKeySubagentParkedReportPreview: "Round two report",
	})
}

// A SendMessage rebind onto a parked agent retires the parked launch, so
// the tray serves one row for the agent: the carrier, running.
func TestAgentRunStateAfterARebindOntoAParkedAgent(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	parkLaunchAgent(t, router, "t1", "root", "task-agent", "")
	parkLaunchShell(t, router, "t1", "shell", "task-shell", "root")
	parkStop(t, router, "t1", "root", "task-agent", "ROUND 1", "u1")
	assertRunState(t, runStates(t, router, st, "t1")["root"], map[string]any{
		metaKeySubagentRunState:       subagentRunParked,
		metaKeySubagentParkedCommands: 1,
	})

	resumeAgent(t, router, "t1", "carrier", map[string]any{
		"task_id": "task-agent", "task_type": "local_agent", "resumes_tool_use_id": "root",
		"description": "Spike root", "subagent_type": "general-purpose", provider.MetaTranscriptRootIDKey: "root",
	})
	states := runStates(t, router, st, "t1")
	if _, served := states["root"]; served {
		t.Errorf("the tray still serves the retired root: %v", states["root"])
	}
	assertRunState(t, states["carrier"], map[string]any{metaKeySubagentRunState: subagentRunRunning})
}
