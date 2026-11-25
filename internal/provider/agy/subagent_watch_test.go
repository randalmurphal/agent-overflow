package agy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/acp"
)

func TestParseAgyTranscriptModel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw        string
		wantModel  string
		wantEffort string
	}{
		{"gemini-4-hydrogen-tiered-high", "gemini-4-hydrogen", "high"},
		{"gemini-4-neon-xhigh", "gemini-4-neon", "xhigh"},
		{"claude-sonnet-4-6-medium", "claude-sonnet-4-6", "medium"},
		{"gpt-oss-120b-medium", "gpt-oss-120b-medium", ""},
		{"gemini-pro-orchestrator", "gemini-pro-orchestrator", ""},
		{"", "", ""},
	}
	for _, tc := range cases {
		gotModel, gotEffort := ParseAgyTranscriptModel(tc.raw)
		if gotModel != tc.wantModel || gotEffort != tc.wantEffort {
			t.Errorf("ParseAgyTranscriptModel(%q) = (%q, %q), want (%q, %q)",
				tc.raw, gotModel, gotEffort, tc.wantModel, tc.wantEffort)
		}
	}
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestWatchSubagentCorrelatesByInboxToolCallIDAndSettlesFromParentInbox(t *testing.T) {
	t.Parallel()
	appDataDir := t.TempDir()
	parentID := "4d5f2b49-97b2-49e7-a214-7aff00575443"
	child1 := "8d1d87ec-d2f0-472e-8626-2c83eefc2bc1"
	child2 := "7e836975-0d1f-4944-a41f-9940e16715d1"

	// Two parallel subagents launched simultaneously — exact toolCall.id in
	// each child's inbox uniquely identifies them without touching transcript.jsonl.
	writeJSONFile(t, filepath.Join(appDataDir, "brain", parentID, ".system_generated", "subagents", child1+".json"), map[string]any{
		"conversationId": child1,
		"state":          "SUBAGENT_STATE_ALIVE",
	})
	writeJSONFile(t, filepath.Join(appDataDir, "brain", parentID, ".system_generated", "subagents", child2+".json"), map[string]any{
		"conversationId": child2,
		"state":          "SUBAGENT_STATE_ALIVE",
	})
	writeJSONFile(t, filepath.Join(appDataDir, "brain", child1, ".system_generated", "messages", "launch-1.json"), map[string]any{
		"sender": parentID,
		"sourceMetadata": map[string]any{
			"tool": map[string]any{
				"conversationId": parentID,
				"toolCall":       map[string]any{"id": "call_sub_1", "name": "invoke_subagent"},
			},
		},
	})
	writeJSONFile(t, filepath.Join(appDataDir, "brain", child2, ".system_generated", "messages", "launch-2.json"), map[string]any{
		"sender": parentID,
		"sourceMetadata": map[string]any{
			"tool": map[string]any{
				"conversationId": parentID,
				"toolCall":       map[string]any{"id": "call_sub_2", "name": "invoke_subagent"},
			},
		},
	})

	flavor := NewFlavor(appDataDir)
	flavor.subagentPollInterval = 10 * time.Millisecond
	flavor.subagentDiscoveryTimeout = 2 * time.Second

	// Live ResolveSubagentLaunch maps child conversation UUID -> parent launch ID.
	if gotLaunch, ok := flavor.ResolveSubagentLaunch(context.Background(), parentID, child2); !ok || gotLaunch != "call_sub_2" {
		t.Fatalf("ResolveSubagentLaunch(child2) = (%q, %v), want (call_sub_2, true)", gotLaunch, ok)
	}

	var mu sync.Mutex
	var events []provider.ProviderEvent
	var discovered string
	done := make(chan struct{})
	go func() {
		defer close(done)
		flavor.WatchSubagent(context.Background(), acp.SubagentLaunchContext{
			SessionID:    parentID,
			ThreadID:     "t1",
			TurnID:       "turn-1",
			ToolCallID:   "call_sub_1",
			RawInput:     json.RawMessage(`{"TypeName":"research","Role":"Queue Auditor","Model":"inherit"}`),
			Started:      acp.NormalizedToolStart{SubagentType: "research", Description: "Queue Auditor"},
			ParentModel:  "gemini-4-neon",
			ParentEffort: "high",
			Emit: func(evt provider.ProviderEvent) {
				mu.Lock()
				events = append(events, evt)
				mu.Unlock()
			},
			OnChildDiscovered: func(id string) {
				mu.Lock()
				discovered = id
				mu.Unlock()
			},
		})
	}()

	// Wait for discovery callback, then deliver child1's report into parent inbox.
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		got := discovered
		mu.Unlock()
		if got == child1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for child discovery, got %q", got)
		}
		time.Sleep(10 * time.Millisecond)
	}

	writeJSONFile(t, filepath.Join(appDataDir, "brain", parentID, ".system_generated", "messages", "report-1.json"), map[string]any{
		"sender":   child1,
		"priority": "MESSAGE_PRIORITY_HIGH",
		"content":  "Found 7 queue bugs.",
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WatchSubagent did not settle after parent inbox report arrived")
	}

	mu.Lock()
	defer mu.Unlock()
	var sawIdentity, sawCompleted bool
	for _, ev := range events {
		if ev.Kind == provider.EventToolStart && ev.ItemID == "call_sub_1" {
			var m map[string]any
			_ = json.Unmarshal(ev.Meta, &m)
			if m["agent_id"] == child1 && m["subagent_model"] == "gemini-4-neon" && m["run_effort"] == "high" {
				sawIdentity = true
			}
		}
		if ev.Kind == provider.EventBackgroundTaskNotification && ev.ItemID == "call_sub_1" {
			var m map[string]any
			_ = json.Unmarshal(ev.Meta, &m)
			if m["status"] == "completed" && ev.Content == "Found 7 queue bugs." {
				sawCompleted = true
			}
		}
	}
	if !sawIdentity {
		t.Fatalf("missing identity meta stamp (agent_id + inherited parent model/effort); events=%+v", events)
	}
	if !sawCompleted {
		t.Fatalf("missing completed background task notification with inbox report; events=%+v", events)
	}
}

func TestWatchSubagentSettlesKilledFromSubagentRecord(t *testing.T) {
	t.Parallel()
	appDataDir := t.TempDir()
	parentID := "4d5f2b49-97b2-49e7-a214-7aff00575443"
	childID := "2893f594-c39d-4623-b8af-0d15a65a717e"

	recPath := filepath.Join(appDataDir, "brain", parentID, ".system_generated", "subagents", childID+".json")
	writeJSONFile(t, recPath, map[string]any{
		"conversationId": childID,
		"state":          "SUBAGENT_STATE_ALIVE",
	})
	writeJSONFile(t, filepath.Join(appDataDir, "brain", childID, ".system_generated", "messages", "launch.json"), map[string]any{
		"sender": parentID,
		"sourceMetadata": map[string]any{
			"tool": map[string]any{
				"conversationId": parentID,
				"toolCall":       map[string]any{"id": "call_kill_1", "name": "invoke_subagent"},
			},
		},
	})

	flavor := NewFlavor(appDataDir)
	flavor.subagentPollInterval = 10 * time.Millisecond
	flavor.subagentDiscoveryTimeout = 2 * time.Second

	var mu sync.Mutex
	var events []provider.ProviderEvent
	done := make(chan struct{})
	go func() {
		defer close(done)
		flavor.WatchSubagent(context.Background(), acp.SubagentLaunchContext{
			SessionID:  parentID,
			ThreadID:   "t1",
			TurnID:     "turn-1",
			ToolCallID: "call_kill_1",
			Started:    acp.NormalizedToolStart{SubagentType: "research"},
			Emit: func(evt provider.ProviderEvent) {
				mu.Lock()
				events = append(events, evt)
				mu.Unlock()
			},
		})
	}()

	time.Sleep(30 * time.Millisecond)
	writeJSONFile(t, recPath, map[string]any{
		"conversationId": childID,
		"state":          "SUBAGENT_STATE_KILLED",
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WatchSubagent did not settle on SUBAGENT_STATE_KILLED")
	}

	mu.Lock()
	defer mu.Unlock()
	for _, ev := range events {
		if ev.Kind == provider.EventBackgroundTaskNotification {
			var m map[string]any
			_ = json.Unmarshal(ev.Meta, &m)
			if m["status"] == "killed" {
				return
			}
		}
	}
	t.Fatalf("expected killed notification, got events=%+v", events)
}

func TestWatchSubagentDiscoveryTimeoutDoesNotEmitFakeFailure(t *testing.T) {
	t.Parallel()
	flavor := NewFlavor(t.TempDir())
	flavor.subagentPollInterval = 10 * time.Millisecond
	flavor.subagentDiscoveryTimeout = 40 * time.Millisecond

	var events []provider.ProviderEvent
	flavor.WatchSubagent(context.Background(), acp.SubagentLaunchContext{
		SessionID:  "4d5f2b49-97b2-49e7-a214-7aff00575443",
		ThreadID:   "t1",
		TurnID:     "turn-1",
		ToolCallID: "call_missing",
		Emit: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	})
	if len(events) != 0 {
		t.Fatalf("discovery timeout must not emit fake failed agent notification, got %+v", events)
	}
}

func TestWatchSubagentWorksPast256InboxFiles(t *testing.T) {
	t.Parallel()
	appDataDir := t.TempDir()
	parentID := "4d5f2b49-97b2-49e7-a214-7aff00575443"
	childID := "9d5f2b49-97b2-49e7-a214-7aff00575499"

	writeJSONFile(t, filepath.Join(appDataDir, "brain", parentID, ".system_generated", "subagents", childID+".json"), map[string]any{
		"conversationId": childID,
		"state":          "SUBAGENT_STATE_ALIVE",
	})
	writeJSONFile(t, filepath.Join(appDataDir, "brain", childID, ".system_generated", "messages", "0001.json"), map[string]any{
		"sender": parentID,
		"sourceMetadata": map[string]any{
			"tool": map[string]any{
				"conversationId": parentID,
				"toolCall": map[string]any{
					"id":   "call_past_256",
					"name": "invoke_subagent",
				},
			},
		},
	})
	msgDir := filepath.Join(appDataDir, "brain", parentID, ".system_generated", "messages")
	for i := 0; i < 270; i++ {
		name := "0" + string(rune('a'+(i/26)%26)) + string(rune('a'+i%26)) + "_" + time.Now().Format("150405") + "_" + string(rune('0'+(i/100)%10)) + string(rune('0'+(i/10)%10)) + string(rune('0'+i%10)) + ".json"
		writeJSONFile(t, filepath.Join(msgDir, name), map[string]any{
			"sender":  "other-sender",
			"content": "filler",
		})
	}
	writeJSONFile(t, filepath.Join(msgDir, "zzz_report.json"), map[string]any{
		"sender":  childID,
		"content": "Final report past 256 messages",
	})

	flavor := NewFlavor(appDataDir)
	flavor.subagentPollInterval = 10 * time.Millisecond
	var events []provider.ProviderEvent
	flavor.WatchSubagent(context.Background(), acp.SubagentLaunchContext{
		SessionID:  parentID,
		ThreadID:   "t1",
		TurnID:     "turn-1",
		ToolCallID: "call_past_256",
		Started:    acp.NormalizedToolStart{SubagentType: "research"},
		Emit: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	})
	for _, ev := range events {
		if ev.Kind == provider.EventBackgroundTaskNotification && ev.Content == "Final report past 256 messages" {
			return
		}
	}
	t.Fatalf("expected settlement past 256 inbox files, got %+v", events)
}

func TestWatchSubagentSettlesOnParentInboxSystemError(t *testing.T) {
	t.Parallel()
	appDataDir := t.TempDir()
	parentID := "4d5f2b49-97b2-49e7-a214-7aff00575443"
	childID := "33333333-3333-3333-3333-333333333333"

	writeJSONFile(t, filepath.Join(appDataDir, "brain", parentID, ".system_generated", "subagents", childID+".json"), map[string]any{
		"conversationId": childID,
		"state":          "SUBAGENT_STATE_ALIVE",
	})
	writeJSONFile(t, filepath.Join(appDataDir, "brain", childID, ".system_generated", "messages", "0001.json"), map[string]any{
		"sender": parentID,
		"sourceMetadata": map[string]any{
			"tool": map[string]any{
				"conversationId": parentID,
				"toolCall":       map[string]any{"id": "call_crash", "name": "invoke_subagent"},
			},
		},
	})
	errMsg := `The subagent research with id "` + childID + `" encountered an error and has either stopped or failed to start execution: RESOURCE_EXHAUSTED`
	writeJSONFile(t, filepath.Join(appDataDir, "brain", parentID, ".system_generated", "messages", "0002_err.json"), map[string]any{
		"sender":    "system",
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"content":   errMsg,
	})

	flavor := NewFlavor(appDataDir)
	flavor.subagentPollInterval = 10 * time.Millisecond
	var events []provider.ProviderEvent
	flavor.WatchSubagent(context.Background(), acp.SubagentLaunchContext{
		SessionID:  parentID,
		ThreadID:   "t1",
		TurnID:     "turn-1",
		ToolCallID: "call_crash",
		Started:    acp.NormalizedToolStart{SubagentType: "research"},
		Emit: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	})
	for _, ev := range events {
		if ev.Kind == provider.EventBackgroundTaskNotification {
			var m map[string]any
			_ = json.Unmarshal(ev.Meta, &m)
			if m["status"] == "failed" && ev.Content == errMsg {
				return
			}
		}
	}
	t.Fatalf("expected failed settlement from parent inbox system error, got %+v", events)
}

func TestWatchSubagentSettlesOnParentInboxIdleGuard(t *testing.T) {
	t.Parallel()
	appDataDir := t.TempDir()
	parentID := "4d5f2b49-97b2-49e7-a214-7aff00575443"
	childID := "55555555-5555-5555-5555-555555555555"

	writeJSONFile(t, filepath.Join(appDataDir, "brain", parentID, ".system_generated", "subagents", childID+".json"), map[string]any{
		"conversationId": childID,
		"state":          "SUBAGENT_STATE_ALIVE",
	})
	writeJSONFile(t, filepath.Join(appDataDir, "brain", childID, ".system_generated", "messages", "0001.json"), map[string]any{
		"sender": parentID,
		"sourceMetadata": map[string]any{
			"tool": map[string]any{
				"conversationId": parentID,
				"toolCall":       map[string]any{"id": "call_idle", "name": "invoke_subagent"},
			},
		},
	})
	idleMsg := "Subagent " + childID + " has gone idle. This was the subagent's most recent response: done"
	writeJSONFile(t, filepath.Join(appDataDir, "brain", parentID, ".system_generated", "messages", "0002_idle.json"), map[string]any{
		"sender":    "system",
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"content":   idleMsg,
	})

	flavor := NewFlavor(appDataDir)
	flavor.subagentPollInterval = 10 * time.Millisecond
	var events []provider.ProviderEvent
	flavor.WatchSubagent(context.Background(), acp.SubagentLaunchContext{
		SessionID:  parentID,
		ThreadID:   "t1",
		TurnID:     "turn-1",
		ToolCallID: "call_idle",
		Started:    acp.NormalizedToolStart{SubagentType: "research"},
		Emit: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	})
	for _, ev := range events {
		if ev.Kind == provider.EventBackgroundTaskNotification {
			var m map[string]any
			_ = json.Unmarshal(ev.Meta, &m)
			if m["status"] == "completed" && ev.Content == idleMsg {
				return
			}
		}
	}
	t.Fatalf("expected completed settlement from parent inbox idle guard, got %+v", events)
}

func TestLookupBackgroundCommandsCorrelatesConcurrentCommands(t *testing.T) {
	t.Parallel()
	appDataDir := t.TempDir()
	sessionID := "4d5f2b49-97b2-49e7-a214-7aff00575443"

	db := createTestConvDB(t, appDataDir, sessionID)
	defer func() { _ = db.Close() }()
	for _, stmt := range []string{
		`ALTER TABLE steps ADD COLUMN status INTEGER NOT NULL DEFAULT 3`,
		`ALTER TABLE steps ADD COLUMN metadata BLOB`,
		`ALTER TABLE steps ADD COLUMN task_details BLOB`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}

	logPath := filepath.Join(appDataDir, "brain", sessionID, ".system_generated", "tasks", "task-12.log")
	taskBlob := []byte("\x0a." + sessionID + "/task-12\x12|file://" + filepath.ToSlash(logPath) + "\x1a\x04logs")
	// Step 11: inline running command (no task_details).
	if _, err := db.Exec(
		`INSERT INTO steps (idx, step_type, status, metadata, task_details, error_details, step_payload) VALUES (11, 132, ?, ?, NULL, X'', X'')`,
		cortexStepStatusRunning, []byte("\x0a\x09call_1234\x12\x0brun_command"),
	); err != nil {
		t.Fatal(err)
	}
	// Step 12: backgrounded running command (task_details populated).
	if _, err := db.Exec(
		`INSERT INTO steps (idx, step_type, status, metadata, task_details, error_details, step_payload) VALUES (12, 132, ?, ?, ?, X'', X'')`,
		cortexStepStatusRunning, []byte("\x0a\x08call_123\x12\x0brun_command"), taskBlob,
	); err != nil {
		t.Fatal(err)
	}

	flavor := NewFlavor(appDataDir)
	// Querying call_12 (prefix of call_123), call_123 (backgrounded), and call_1234 (inline):
	// ONLY call_123 must match!
	got := flavor.LookupBackgroundCommands(context.Background(), sessionID, []string{"call_12", "call_123", "call_1234"}, false, -1)
	if len(got) != 1 {
		t.Fatalf("LookupBackgroundCommands = %+v, want only call_123", got)
	}
	info, ok := got["call_123"]
	if !ok {
		t.Fatalf("missing call_123 in %+v", got)
	}
	if info.TaskID != sessionID+"/task-12" {
		t.Fatalf("TaskID = %q, want %q", info.TaskID, sessionID+"/task-12")
	}
	if info.OutputFile != filepath.Clean(logPath) {
		t.Fatalf("OutputFile = %q, want %q", info.OutputFile, filepath.Clean(logPath))
	}
}

func TestLookupBackgroundCommandsStatGateSkipsUnchangedDB(t *testing.T) {
	t.Parallel()
	appDataDir := t.TempDir()
	sessionID := "5d5f2b49-97b2-49e7-a214-7aff00575444"

	db := createTestConvDB(t, appDataDir, sessionID)
	defer func() { _ = db.Close() }()
	for _, stmt := range []string{
		`PRAGMA journal_mode=WAL`,
		`ALTER TABLE steps ADD COLUMN status INTEGER NOT NULL DEFAULT 3`,
		`ALTER TABLE steps ADD COLUMN metadata BLOB`,
		`ALTER TABLE steps ADD COLUMN task_details BLOB`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	// Step 1: inline running command (no task_details yet).
	if _, err := db.Exec(
		`INSERT INTO steps (idx, step_type, status, metadata, task_details, error_details, step_payload) VALUES (1, 132, ?, ?, NULL, X'', X'')`,
		cortexStepStatusRunning, []byte("\x0a\x07call_bg\x12\x0brun_command"),
	); err != nil {
		t.Fatal(err)
	}

	flavor := NewFlavor(appDataDir)
	var opens int
	flavor.bgLookupOpenHook = func() { opens++ }

	// 1st live call: opens DB, finds nothing, caches negative result for current DB/WAL stat.
	if got := flavor.LookupBackgroundCommands(context.Background(), sessionID, []string{"call_bg"}, false, -1); len(got) != 0 {
		t.Fatalf("1st lookup = %+v, want empty", got)
	}
	if opens != 1 {
		t.Fatalf("opens after 1st call = %d, want 1", opens)
	}

	// 2nd live call with unchanged DB/WAL and same IDs: skipped without opening SQLite.
	if got := flavor.LookupBackgroundCommands(context.Background(), sessionID, []string{"call_bg"}, false, -1); len(got) != 0 {
		t.Fatalf("2nd lookup = %+v, want empty", got)
	}
	if opens != 1 {
		t.Fatalf("opens after unchanged 2nd call = %d, want 1", opens)
	}

	// 3rd live call with an unchecked ID: must open SQLite.
	if got := flavor.LookupBackgroundCommands(context.Background(), sessionID, []string{"call_bg", "call_new"}, false, -1); len(got) != 0 {
		t.Fatalf("3rd lookup = %+v, want empty", got)
	}
	if opens != 2 {
		t.Fatalf("opens after new-ID 3rd call = %d, want 2", opens)
	}

	// Completion-time check (includeCompleted=true) must always bypass the live cache.
	if got := flavor.LookupBackgroundCommands(context.Background(), sessionID, []string{"call_bg"}, true, -1); len(got) != 0 {
		t.Fatalf("includeCompleted lookup = %+v, want empty", got)
	}
	if opens != 3 {
		t.Fatalf("opens after includeCompleted call = %d, want 3", opens)
	}

	// Writing task_details modifies the WAL/DB and invalidates the stat gate.
	logPath := filepath.Join(appDataDir, "brain", sessionID, ".system_generated", "tasks", "task-1.log")
	taskBlob := []byte("\x0a." + sessionID + "/task-1\x12|file://" + filepath.ToSlash(logPath) + "\x1a\x04logs")
	if _, err := db.Exec(`UPDATE steps SET task_details = ? WHERE idx = 1`, taskBlob); err != nil {
		t.Fatal(err)
	}
	// minStepIdx == 1 excludes idx <= 1 via PRIMARY KEY range scan.
	if got := flavor.LookupBackgroundCommands(context.Background(), sessionID, []string{"call_bg"}, true, 1); len(got) != 0 {
		t.Fatalf("bounded lookup(minStepIdx=1) = %+v, want empty", got)
	}
	got := flavor.LookupBackgroundCommands(context.Background(), sessionID, []string{"call_bg"}, false, 0)
	if opens != 5 {
		t.Fatalf("opens after DB write + bounded check = %d, want 5", opens)
	}
	info, ok := got["call_bg"]
	if !ok || info.TaskID != sessionID+"/task-1" || info.OutputFile != filepath.Clean(logPath) {
		t.Fatalf("lookup after DB write = %+v, want call_bg with task-1", got)
	}
}

func TestDecodeTranscriptToolArgsPreservesNativeStringsInFullTranscript(t *testing.T) {
	t.Parallel()
	args := map[string]json.RawMessage{
		"Path":        json.RawMessage(`"/tmp/cfg.json"`),
		"Content":     json.RawMessage(`"{\"enabled\":true}"`),
		"Target":      json.RawMessage(`"42"`),
		"Replacement": json.RawMessage(`"true"`),
		"StartLine":   json.RawMessage(`10`),
	}
	// Full transcript must keep JSON/numeric/boolean string literals as strings.
	fullRaw := decodeTranscriptToolArgs(args, false)
	var full map[string]any
	if err := json.Unmarshal(fullRaw, &full); err != nil {
		t.Fatalf("unmarshal full args: %v", err)
	}
	if s, ok := full["Content"].(string); !ok || s != `{"enabled":true}` {
		t.Fatalf("full Content = %#v, want string {\"enabled\":true}", full["Content"])
	}
	if s, ok := full["Target"].(string); !ok || s != "42" {
		t.Fatalf("full Target = %#v, want string 42", full["Target"])
	}
	if s, ok := full["Replacement"].(string); !ok || s != "true" {
		t.Fatalf("full Replacement = %#v, want string true", full["Replacement"])
	}
	if n, ok := full["StartLine"].(float64); !ok || n != 10 {
		t.Fatalf("full StartLine = %#v, want number 10", full["StartLine"])
	}

	// Compact transcript unwraps string-encoded JSON values.
	compactRaw := decodeTranscriptToolArgs(map[string]json.RawMessage{
		"Path":      json.RawMessage(`"\"/tmp/cfg.json\""`),
		"StartLine": json.RawMessage(`"10"`),
	}, true)
	var compact map[string]any
	if err := json.Unmarshal(compactRaw, &compact); err != nil {
		t.Fatalf("unmarshal compact args: %v", err)
	}
	if s, ok := compact["Path"].(string); !ok || s != "/tmp/cfg.json" {
		t.Fatalf("compact Path = %#v, want /tmp/cfg.json", compact["Path"])
	}
	if n, ok := compact["StartLine"].(float64); !ok || n != 10 {
		t.Fatalf("compact StartLine = %#v, want 10", compact["StartLine"])
	}
}

func TestDetectCompactionAdvancesCursorWithoutCheckpoint(t *testing.T) {
	t.Parallel()
	appDataDir := t.TempDir()
	sid := "sess-compact-cursor"
	dbPath := seedRichConversationDB(t, appDataDir, sid, []testStepRow{
		{idx: 0, stepType: cortexStepTypeUserInput, payload: encodeTestUserInputStep("u0", "h0", 1000, false, false)},
		{idx: 5, stepType: cortexStepTypePlannerResponse, payload: []byte("r0")},
	})
	f := NewFlavor(appDataDir)
	cursor := f.InitCompactionCursor(context.Background(), sid)
	if cursor != 5 {
		t.Fatalf("InitCompactionCursor = %d, want 5 (O(1) MAX(idx))", cursor)
	}
	// Append non-checkpoint steps: DetectCompaction must advance nextIdx to 12.
	appendTestStepRows(t, dbPath, []testStepRow{
		{idx: 10, stepType: cortexStepTypeUserInput, payload: encodeTestUserInputStep("u1", "h1", 2000, false, false)},
		{idx: 12, stepType: cortexStepTypePlannerResponse, payload: []byte("r1")},
	})
	nextIdx, ev := f.DetectCompaction(context.Background(), sid, cursor)
	if ev != nil || nextIdx != 12 {
		t.Fatalf("DetectCompaction(no checkpoint) = (%d, %+v), want (12, nil)", nextIdx, ev)
	}
	// Append a real checkpoint at idx 15: must detect and advance cursor.
	appendTestStepRows(t, dbPath, []testStepRow{
		{idx: 15, stepType: cortexStepTypeCheckpoint, payload: []byte("cp")},
		{idx: 16, stepType: cortexStepTypePlannerResponse, payload: []byte("r2")},
	})
	nextIdx, ev = f.DetectCompaction(context.Background(), sid, nextIdx)
	if ev == nil || ev.ItemID != "agy-compact-15" || nextIdx != 16 {
		t.Fatalf("DetectCompaction(with checkpoint) = (%d, %+v), want (16, agy-compact-15)", nextIdx, ev)
	}
}

func TestWatchSubagentSettlesOnParentSystemStopNoticeAfterLaunch(t *testing.T) {
	t.Parallel()
	appDataDir := t.TempDir()
	parentID := "4d5f2b49-97b2-49e7-a214-7aff00575443"
	childID := "44444444-4444-4444-4444-444444444444"

	writeJSONFile(t, filepath.Join(appDataDir, "brain", parentID, ".system_generated", "subagents", childID+".json"), map[string]any{
		"conversationId": childID,
		"state":          "SUBAGENT_STATE_ALIVE",
	})
	writeJSONFile(t, filepath.Join(appDataDir, "brain", childID, ".system_generated", "messages", "0001.json"), map[string]any{
		"sender": parentID,
		"sourceMetadata": map[string]any{
			"tool": map[string]any{
				"conversationId": parentID,
				"toolCall":       map[string]any{"id": "call_sys_stop", "name": "invoke_subagent"},
			},
		},
	})
	msgDir := filepath.Join(appDataDir, "brain", parentID, ".system_generated", "messages")
	// An older system stop notice from 10 minutes ago must NOT prematurely kill this subagent.
	writeJSONFile(t, filepath.Join(msgDir, "0001_old_stop.json"), map[string]any{
		"sender":    "system",
		"timestamp": time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339Nano),
		"content":   "[Notice] All your subagents and background tasks have been stopped due to server restart.",
	})

	flavor := NewFlavor(appDataDir)
	flavor.subagentPollInterval = 10 * time.Millisecond
	var mu sync.Mutex
	var events []provider.ProviderEvent
	done := make(chan struct{})
	go func() {
		defer close(done)
		flavor.WatchSubagent(context.Background(), acp.SubagentLaunchContext{
			SessionID:  parentID,
			ThreadID:   "t1",
			TurnID:     "turn-1",
			ToolCallID: "call_sys_stop",
			Started:    acp.NormalizedToolStart{SubagentType: "research"},
			Emit: func(evt provider.ProviderEvent) {
				mu.Lock()
				events = append(events, evt)
				mu.Unlock()
			},
		})
	}()

	time.Sleep(35 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("stale system stop notice from before launch prematurely settled subagent")
	default:
	}

	writeJSONFile(t, filepath.Join(msgDir, "0002_new_stop.json"), map[string]any{
		"sender":    "system",
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"content":   "[Notice] All your subagents and background tasks have been stopped due to user force cancellation.",
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WatchSubagent did not settle on post-launch system stop notice")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, ev := range events {
		if ev.Kind == provider.EventBackgroundTaskNotification {
			var m map[string]any
			_ = json.Unmarshal(ev.Meta, &m)
			if m["status"] == "killed" {
				return
			}
		}
	}
	t.Fatalf("expected killed settlement on post-launch system stop notice, got %+v", events)
}

func appendJSONLLine(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(append(raw, '\n')); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestWatchSubagentTailsFullTranscriptAndStampsResolvedModelEffort(t *testing.T) {
	t.Parallel()
	appDataDir := t.TempDir()
	parentID := "4d5f2b49-97b2-49e7-a214-7aff00575443"
	childID := "66666666-6666-6666-6666-666666666666"

	writeJSONFile(t, filepath.Join(appDataDir, "brain", parentID, ".system_generated", "subagents", childID+".json"), map[string]any{
		"conversationId": childID,
		"state":          "SUBAGENT_STATE_ALIVE",
	})
	writeJSONFile(t, filepath.Join(appDataDir, "brain", childID, ".system_generated", "messages", "0001.json"), map[string]any{
		"sender": parentID,
		"sourceMetadata": map[string]any{
			"tool": map[string]any{
				"conversationId": parentID,
				"toolCall":       map[string]any{"id": "call_full_1", "name": "invoke_subagent"},
			},
		},
	})

	// Populate both compact transcript.jsonl (truncated) and transcript_full.jsonl (full);
	// WatchSubagent must prefer transcript_full.jsonl.
	compactPath := filepath.Join(appDataDir, "brain", childID, ".system_generated", "logs", "transcript.jsonl")
	fullPath := filepath.Join(appDataDir, "brain", childID, ".system_generated", "logs", "transcript_full.jsonl")

	appendJSONLLine(t, compactPath, map[string]any{
		"step_index": 1, "type": "PLANNER_RESPONSE", "status": "DONE",
		"model": "gemini-4-hydrogen-tiered-high", "input_tokens": 1000, "output_tokens": 50,
		"tool_calls": []map[string]any{{"name": "read_file", "args": map[string]string{"Path": `"/repo/a.go"`}}},
	})
	appendJSONLLine(t, compactPath, map[string]any{
		"step_index": 2, "type": "GENERIC", "status": "DONE",
		"content": "COMPACT_TRUNCATED_ONLY",
	})

	appendJSONLLine(t, fullPath, map[string]any{
		"step_index": 1, "type": "PLANNER_RESPONSE", "status": "DONE",
		"created_at": "2026-11-24T19:54:26Z",
		"model":      "gemini-4-hydrogen-tiered-high", "input_tokens": 5781, "output_tokens": 88,
		"thinking": "Checking capabilities.go for MaxMCPWaitSeconds.",
		// Real transcript_full.jsonl writes native typed JSON values (numbers,
		// booleans, plain strings) rather than string-encoded values.
		"tool_calls": []map[string]any{
			{"name": "read_file", "args": map[string]any{"Path": "/repo/capabilities.go", "StartLine": 1, "EndLine": 30}},
			{"name": "run_command", "args": map[string]any{"CommandLine": "echo ok", "WaitMsBeforeAsync": 5000, "IsDaemon": false}},
		},
	})
	appendJSONLLine(t, fullPath, map[string]any{
		"step_index": 2, "type": "GENERIC", "status": "DONE",
		"created_at": "2026-11-24T19:54:28Z",
		"content":    "FULL_UNTRUNCATED_FILE_OUTPUT_LINE_206_MAX_150",
	})
	appendJSONLLine(t, fullPath, map[string]any{
		"step_index": 3, "type": "GENERIC", "status": "DONE",
		"created_at": "2026-11-24T19:54:29Z",
		"content":    "Command exited with code 0.\n\nCommand output:\nok\n",
	})

	flavor := NewFlavor(appDataDir)
	flavor.subagentPollInterval = 10 * time.Millisecond
	var openMu sync.Mutex
	var openCount int
	flavor.subagentTranscriptOpenHook = func() {
		openMu.Lock()
		openCount++
		openMu.Unlock()
	}

	var mu sync.Mutex
	var events []provider.ProviderEvent
	done := make(chan struct{})
	go func() {
		defer close(done)
		flavor.WatchSubagent(context.Background(), acp.SubagentLaunchContext{
			SessionID:    parentID,
			ThreadID:     "t1",
			TurnID:       "turn-1",
			ToolCallID:   "call_full_1",
			RawInput:     json.RawMessage(`{"TypeName":"research","Role":"Capabilities Checker","Model":"flash"}`),
			Started:      acp.NormalizedToolStart{SubagentType: "research", Description: "Capabilities Checker"},
			ParentModel:  "gemini-4-neon",
			ParentEffort: "high",
			Emit: func(evt provider.ProviderEvent) {
				mu.Lock()
				events = append(events, evt)
				mu.Unlock()
			},
		})
	}()

	// Wait until the first two transcript steps have been read and projected.
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		sawReadDone := false
		for _, ev := range events {
			if ev.Kind == provider.EventToolComplete && ev.ParentToolUseID == "call_full_1" {
				sawReadDone = true
				break
			}
		}
		mu.Unlock()
		if sawReadDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for transcript_full.jsonl tool completion")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Across several idle ticks at EOF, the os.Stat size gate must prevent reopening the file.
	openMu.Lock()
	opensAtEOF := openCount
	openMu.Unlock()
	time.Sleep(60 * time.Millisecond)
	openMu.Lock()
	opensAfterIdle := openCount
	openMu.Unlock()
	if opensAfterIdle != opensAtEOF {
		t.Fatalf("expected 0 transcript opens while idle at EOF, went from %d to %d", opensAtEOF, opensAfterIdle)
	}

	// Append final PLANNER_RESPONSE and deliver parent inbox report to settle.
	appendJSONLLine(t, fullPath, map[string]any{
		"step_index": 4, "type": "PLANNER_RESPONSE", "status": "DONE",
		"created_at": "2026-11-24T19:54:30Z",
		"model":      "gemini-4-hydrogen-tiered-high", "input_tokens": 1200, "output_tokens": 30,
		"content": "Reported MaxMCPWaitSeconds = 150.",
	})
	writeJSONFile(t, filepath.Join(appDataDir, "brain", parentID, ".system_generated", "messages", "report.json"), map[string]any{
		"sender":  childID,
		"content": "MaxMCPWaitSeconds is 150.",
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WatchSubagent did not settle after parent inbox report")
	}

	mu.Lock()
	defer mu.Unlock()
	var sawResolvedModel, sawThinking, sawFullRead, sawBashStart, sawChildText, sawProgress, sawSettled bool
	for _, ev := range events {
		if ev.Kind == provider.EventToolStart && ev.ItemID == "call_full_1" {
			var m map[string]any
			_ = json.Unmarshal(ev.Meta, &m)
			if m["subagent_model"] == "flash" {
				t.Fatalf("must not prematurely stamp tier alias 'flash' as subagent_model: %+v", m)
			}
			if m["subagent_model"] == "gemini-4-hydrogen" && m["run_effort"] == "high" && m["agent_id"] == childID {
				sawResolvedModel = true
			}
		}
		if ev.Kind == provider.EventToolStart && ev.ParentToolUseID == "call_full_1" && ev.ItemType == "command_execution" {
			sawBashStart = true
		}
		if ev.Kind == provider.EventThinking && ev.ParentToolUseID == "call_full_1" && strings.Contains(ev.Content, "MaxMCPWaitSeconds") {
			sawThinking = true
		}
		if ev.Kind == provider.EventToolComplete && ev.ParentToolUseID == "call_full_1" && strings.Contains(ev.Content, "FULL_UNTRUNCATED_FILE_OUTPUT_LINE_206_MAX_150") {
			sawFullRead = true
		}
		if ev.Kind == provider.EventTextDelta && ev.ParentToolUseID == "call_full_1" && strings.Contains(ev.Content, "Reported MaxMCPWaitSeconds = 150.") {
			sawChildText = true
		}
		if ev.Kind == provider.EventSubagentProgress && ev.ItemID == "call_full_1" {
			var p provider.SubagentProgressMeta
			if json.Unmarshal(ev.Meta, &p) == nil && p.ToolUses == 2 && p.TotalTokens == 5781+88+1200+30 {
				sawProgress = true
			}
		}
		if ev.Kind == provider.EventBackgroundTaskNotification && ev.ItemID == "call_full_1" && ev.Content == "MaxMCPWaitSeconds is 150." {
			sawSettled = true
		}
	}
	if !sawResolvedModel || !sawThinking || !sawFullRead || !sawBashStart || !sawChildText || !sawProgress || !sawSettled {
		t.Fatalf("missing expected events: resolvedModel=%v thinking=%v fullRead=%v bashStart=%v childText=%v progress=%v settled=%v",
			sawResolvedModel, sawThinking, sawFullRead, sawBashStart, sawChildText, sawProgress, sawSettled)
	}
}

func TestWatchSubagentFallsBackToCompactTranscriptAndEmitsReportInPane(t *testing.T) {
	t.Parallel()
	appDataDir := t.TempDir()
	parentID := "4d5f2b49-97b2-49e7-a214-7aff00575443"
	childID := "77777777-7777-7777-7777-777777777777"

	writeJSONFile(t, filepath.Join(appDataDir, "brain", parentID, ".system_generated", "subagents", childID+".json"), map[string]any{
		"conversationId": childID,
		"state":          "SUBAGENT_STATE_ALIVE",
	})
	writeJSONFile(t, filepath.Join(appDataDir, "brain", childID, ".system_generated", "messages", "0001.json"), map[string]any{
		"sender": parentID,
		"sourceMetadata": map[string]any{
			"tool": map[string]any{
				"conversationId": parentID,
				"toolCall":       map[string]any{"id": "call_compact_1", "name": "invoke_subagent"},
			},
		},
	})
	// Only compact transcript.jsonl exists, with no assistant text block before reporting.
	compactPath := filepath.Join(appDataDir, "brain", childID, ".system_generated", "logs", "transcript.jsonl")
	appendJSONLLine(t, compactPath, map[string]any{
		"step_index": 1, "type": "PLANNER_RESPONSE", "status": "DONE",
		"model": "gemini-4-neon-medium", "input_tokens": 500, "output_tokens": 40,
		"tool_calls": []map[string]any{{"name": "send_message", "args": map[string]string{"Message": `"Done!"`}}},
	})
	appendJSONLLine(t, compactPath, map[string]any{
		"step_index": 2, "type": "GENERIC", "status": "DONE",
		"content": "Message sent.",
	})
	writeJSONFile(t, filepath.Join(appDataDir, "brain", parentID, ".system_generated", "messages", "report.json"), map[string]any{
		"sender":  childID,
		"content": "Done!",
	})

	flavor := NewFlavor(appDataDir)
	flavor.subagentPollInterval = 10 * time.Millisecond
	var events []provider.ProviderEvent
	flavor.WatchSubagent(context.Background(), acp.SubagentLaunchContext{
		SessionID:  parentID,
		ThreadID:   "t1",
		TurnID:     "turn-1",
		ToolCallID: "call_compact_1",
		RawInput:   json.RawMessage(`{"TypeName":"research","Role":"Quick Check","Model":"pro"}`),
		Started:    acp.NormalizedToolStart{SubagentType: "research"},
		Emit: func(evt provider.ProviderEvent) {
			events = append(events, evt)
		},
	})

	var sawModel, sawReportText bool
	for _, ev := range events {
		if ev.Kind == provider.EventToolStart && ev.ItemID == "call_compact_1" {
			var m map[string]any
			_ = json.Unmarshal(ev.Meta, &m)
			if m["subagent_model"] == "gemini-4-neon" && m["run_effort"] == "medium" {
				sawModel = true
			}
		}
		if ev.Kind == provider.EventTextDelta && ev.ParentToolUseID == "call_compact_1" && ev.Content == "Done!" {
			sawReportText = true
		}
	}
	if !sawModel || !sawReportText {
		t.Fatalf("expected compact fallback model=%v and reportText=%v; events=%+v", sawModel, sawReportText, events)
	}
}
