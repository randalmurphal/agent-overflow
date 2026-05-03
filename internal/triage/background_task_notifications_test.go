package triage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func decodeItemMetaMap(t *testing.T, raw string) map[string]any {
	t.Helper()
	if raw == "" {
		return map[string]any{}
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		t.Fatalf("decode item meta %q: %v", raw, err)
	}
	return meta
}

func TestBackgroundTaskNotification_PersistsNotificationWithoutMutatingLifecycle(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	startMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Bash",
		"is_background": true,
		"input":         map[string]any{"command": "sleep 30"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "bg-notify",
		ItemType:  "Bash",
		Meta:      startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	notificationMeta, _ := json.Marshal(map[string]any{
		"task_id":     "task-notify",
		"tool_use_id": "bg-notify",
		"status":      "completed",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventBackgroundTaskNotification,
		ThreadID:  "t1",
		ItemID:    "bg-notify",
		Meta:      notificationMeta,
		Content:   `Background command "sleep 30" completed`,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("notification: %v", err)
	}

	notifications := findItemsByKind(t, st, "t1", itemKindNotification)
	if len(notifications) != 1 {
		t.Fatalf("expected 1 notification row, got %d", len(notifications))
	}
	notification := notifications[0]
	if notification.PayloadID != "" {
		t.Fatalf("notification payload should be empty without output_file, got %q", notification.PayloadID)
	}
	if notification.Summary != `Background command "sleep 30" completed` {
		t.Fatalf("notification summary = %q", notification.Summary)
	}
	meta := decodeItemMetaMap(t, notification.Meta)
	if got := meta["task_id"]; got != "task-notify" {
		t.Fatalf("notification task_id = %v, want task-notify", got)
	}
	if got := meta["output_file_state"]; got != "ready" {
		t.Fatalf("notification output_file_state = %v, want ready", got)
	}

	launch, ok, err := st.GetThreadItem("t1", "bg-notify")
	if err != nil || !ok {
		t.Fatalf("lookup launch: ok=%v err=%v", ok, err)
	}
	if launch.Status != statusRunning {
		t.Fatalf("launch status = %q, want running", launch.Status)
	}
	if !launch.IsBackground {
		t.Fatal("launch lost background flag")
	}

	dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(dones) != 0 {
		t.Fatalf("notification must not synthesize completion rows, got %d", len(dones))
	}
}

func TestBackgroundTaskNotification_DrainedStashWithoutLaunchDoesNotCreateRows(t *testing.T) {
	cases := []struct {
		name    string
		status  string
		isError bool
	}{
		{name: "completed", status: "completed"},
		{name: "failed", status: "failed", isError: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, st, _ := newTestRouter(t)
			createTestThread(t, st, "t1")

			stashFields := map[string]any{
				"task_id": "hidden-task",
				"status":  tc.status,
				"source":  "task_updated",
			}
			if tc.isError {
				stashFields["is_error"] = true
			}
			stashMeta, _ := json.Marshal(stashFields)
			if err := router.Handle(provider.ProviderEvent{
				Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1",
				Meta: stashMeta, Timestamp: time.Now(),
			}); err != nil {
				t.Fatalf("task_updated stash: %v", err)
			}
			if _, found, err := st.GetPendingBackgroundTerminal("t1", "hidden-task"); err != nil {
				t.Fatalf("read stash: %v", err)
			} else if !found {
				t.Fatal("expected hidden-task stash before notification")
			}

			notificationMeta, _ := json.Marshal(map[string]any{
				"task_id": "hidden-task",
				"status":  tc.status,
			})
			if err := router.Handle(provider.ProviderEvent{
				Kind: provider.EventBackgroundTaskNotification, ThreadID: "t1",
				Meta: notificationMeta, Content: "hidden task " + tc.status, Timestamp: time.Now(),
			}); err != nil {
				t.Fatalf("task_notification drain: %v", err)
			}

			if _, found, err := st.GetPendingBackgroundTerminal("t1", "hidden-task"); err != nil {
				t.Fatalf("read drained stash: %v", err)
			} else if found {
				t.Fatal("expected hidden-task stash to be drained")
			}
			items, err := st.ListItems("t1")
			if err != nil {
				t.Fatalf("list items: %v", err)
			}
			if len(items) != 0 {
				t.Fatalf("hidden task notification must not synthesize parent rows, got %+v", items)
			}
		})
	}
}

func TestBackgroundTaskNotification_WithoutLaunchOrStashDoesNotCreateRow(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	notificationMeta, _ := json.Marshal(map[string]any{
		"task_id":     "hidden-stopped-task",
		"tool_use_id": "hidden-child-tool",
		"status":      "stopped",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskNotification, ThreadID: "t1",
		ItemID: "hidden-child-tool", Meta: notificationMeta, Content: "hidden task stopped", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("task_notification: %v", err)
	}

	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("hidden task notification must not create parent rows, got %+v", items)
	}
}

func TestBackgroundTaskNotification_OutputFileFeedsLaterTerminal(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	startMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Bash",
		"is_background": true,
		"input":         map[string]any{"command": "sleep 5; echo done"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "bg-output",
		ItemType:  "Bash",
		Meta:      startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	outputPath := filepath.Join(t.TempDir(), "bg-output.txt")
	if err := os.WriteFile(outputPath, []byte("line 1\nline 2\n"), 0o644); err != nil {
		t.Fatalf("write output file: %v", err)
	}

	notificationMeta, _ := json.Marshal(map[string]any{
		"task_id":     "task-output",
		"tool_use_id": "bg-output",
		"status":      "completed",
		"output_file": outputPath,
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventBackgroundTaskNotification,
		ThreadID:  "t1",
		ItemID:    "bg-output",
		Meta:      notificationMeta,
		Content:   `Background command "sleep 5; echo done" completed`,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("notification: %v", err)
	}

	notifications := findItemsByKind(t, st, "t1", itemKindNotification)
	if len(notifications) != 1 {
		t.Fatalf("expected 1 notification row, got %d", len(notifications))
	}
	notification := notifications[0]
	if notification.PayloadID == "" {
		t.Fatal("notification should store output_file payload")
	}
	notificationData, err := st.GetPayloadData(notification.PayloadID)
	if err != nil {
		t.Fatalf("read notification payload: %v", err)
	}
	if string(notificationData) != "line 1\nline 2\n" {
		t.Fatalf("notification payload = %q", string(notificationData))
	}

	terminalMeta, _ := json.Marshal(map[string]any{
		"task_id":     "task-output",
		"tool_use_id": "bg-output",
		"status":      "completed",
		"source":      "task_output",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventBackgroundTaskTerminal,
		ThreadID:  "t1",
		ItemID:    "bg-output",
		Meta:      terminalMeta,
		Content:   "",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("terminal: %v", err)
	}

	dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(dones) != 1 {
		t.Fatalf("expected 1 completion row, got %d", len(dones))
	}
	done := dones[0]
	if done.PayloadID != notification.PayloadID {
		t.Fatalf("completion payload = %q, want %q", done.PayloadID, notification.PayloadID)
	}
	if done.PayloadKind != "command_output" {
		t.Fatalf("completion payload kind = %q, want command_output", done.PayloadKind)
	}
	if notification.PayloadKind != "command_output" {
		t.Fatalf("notification payload kind = %q, want command_output", notification.PayloadKind)
	}
	doneData, err := st.GetPayloadData(done.PayloadID)
	if err != nil {
		t.Fatalf("read completion payload: %v", err)
	}
	if string(doneData) != "line 1\nline 2\n" {
		t.Fatalf("completion payload = %q", string(doneData))
	}
	var commandMeta CommandOutputMeta
	if err := json.Unmarshal([]byte(done.PayloadMeta), &commandMeta); err != nil {
		t.Fatalf("decode completion payload meta: %v", err)
	}
	if commandMeta.Command != "sleep 5; echo done" {
		t.Fatalf("completion command = %q, want sleep 5; echo done", commandMeta.Command)
	}
	if commandMeta.Preview != "line 1\nline 2\n" {
		t.Fatalf("completion preview = %q, want output preview", commandMeta.Preview)
	}
	doneMeta := decodeItemMetaMap(t, done.Meta)
	if got := doneMeta["output_file"]; got != outputPath {
		t.Fatalf("completion output_file = %v, want %s", got, outputPath)
	}
}

func TestBackgroundTaskNotification_EnrichesExistingCompletionWithLoadingAndLoadedStates(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Bash",
		"is_background": true,
		"input":         map[string]any{"command": "sleep 5; echo done"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "bg-enrich-notify",
		ItemType:  "Bash",
		Meta:      startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	terminalMeta, _ := json.Marshal(map[string]any{
		"task_id":     "task-enrich-notify",
		"tool_use_id": "bg-enrich-notify",
		"status":      "completed",
		"source":      "task_output",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventBackgroundTaskTerminal,
		ThreadID:  "t1",
		ItemID:    "bg-enrich-notify",
		Meta:      terminalMeta,
		Content:   "",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("terminal: %v", err)
	}

	initial, ok, err := st.GetThreadItem("t1", nextToolCompletionID("bg-enrich-notify"))
	if err != nil || !ok {
		t.Fatalf("lookup initial completion: ok=%v err=%v", ok, err)
	}
	if initial.PayloadID != "" {
		t.Fatalf("initial completion should not have payload yet, got %q", initial.PayloadID)
	}

	outputPath := filepath.Join(t.TempDir(), "later-output.txt")
	if err := os.WriteFile(outputPath, []byte("later output\n"), 0o644); err != nil {
		t.Fatalf("write output file: %v", err)
	}

	before := len(*emissions)
	notificationMeta, _ := json.Marshal(map[string]any{
		"task_id":     "task-enrich-notify",
		"tool_use_id": "bg-enrich-notify",
		"status":      "completed",
		"output_file": outputPath,
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventBackgroundTaskNotification,
		ThreadID:  "t1",
		ItemID:    "bg-enrich-notify",
		Meta:      notificationMeta,
		Content:   `Background command "sleep 5; echo done" completed`,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("notification: %v", err)
	}

	after, ok, err := st.GetThreadItem("t1", nextToolCompletionID("bg-enrich-notify"))
	if err != nil || !ok {
		t.Fatalf("lookup enriched completion: ok=%v err=%v", ok, err)
	}
	if after.TurnIndex != initial.TurnIndex {
		t.Fatalf("completion turn_index moved from %d to %d", initial.TurnIndex, after.TurnIndex)
	}
	if after.ItemIndex != initial.ItemIndex {
		t.Fatalf("completion item_index moved from %d to %d", initial.ItemIndex, after.ItemIndex)
	}
	if after.PayloadID == "" {
		t.Fatal("completion should gain a payload from notification output_file")
	}
	meta := decodeItemMetaMap(t, after.Meta)
	if got := meta["notification_output_state"]; got != "loaded" {
		t.Fatalf("final notification_output_state = %v, want loaded", got)
	}

	upserts := findUpsertedItems((*emissions)[before:])
	var completionStates []string
	for _, item := range upserts {
		if item.ID != nextToolCompletionID("bg-enrich-notify") {
			continue
		}
		meta := decodeItemMetaMap(t, item.Meta)
		if state, ok := meta["notification_output_state"].(string); ok && state != "" {
			completionStates = append(completionStates, state)
		}
	}
	if len(completionStates) < 2 {
		t.Fatalf("expected completion upserts for loading + loaded states, got %v", completionStates)
	}
	if completionStates[0] != "loading" {
		t.Fatalf("first completion state = %q, want loading", completionStates[0])
	}
	if completionStates[len(completionStates)-1] != "loaded" {
		t.Fatalf("last completion state = %q, want loaded", completionStates[len(completionStates)-1])
	}
}

// TestBackgroundTaskNotification_KilledThenNotificationPreservesStopped
// pins the user-Stop UX. Upstream's stop_task path
// (`claude-code-source-code/src/tasks/stopTask.ts:38-95`) suppresses
// the queued XML notification (`task.notified=true` short-circuits
// `enqueueShellNotification`) and instead calls
// `emitTaskTerminatedSdk(taskId, 'stopped', ...)` directly. The CLI
// also fires `task_updated{patch.status:"killed"}` for the lifecycle
// transition. Both events arrive; order on the wire is not pinned.
//
// Either order must produce a `tool_completion` sibling whose
// `Status` renders as "killed" (Stopped badge) — never "errored"
// (Failed badge). This is the regression guard for the
// killed-→-Failed mis-render the architectural review flagged.
func TestBackgroundTaskNotification_KilledThenNotificationPreservesStopped(t *testing.T) {
	t.Run("task_updated_first_then_notification", func(t *testing.T) {
		router, st, _ := newTestRouter(t)
		createTestThread(t, st, "t1")

		startMeta, _ := json.Marshal(map[string]any{
			"toolName":      "Bash",
			"is_background": true,
			"input":         map[string]any{"command": "sleep 3600"},
		})
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-stop",
			ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("seed launch: %v", err)
		}

		// task_updated{killed} — the carve-out at tool_lifecycle.go:421
		// routes this through observeBackgroundTaskTerminal directly
		// (no stash) so chat shows the Stopped badge without waiting
		// for the next turn's task_notification.
		killMeta, _ := json.Marshal(map[string]any{
			"task_id":     "tsk-stop",
			"tool_use_id": "bg-stop",
			"status":      "killed",
			"is_error":    true,
			"source":      "task_updated",
		})
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "bg-stop",
			Meta: killMeta, Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("task_updated{killed}: %v", err)
		}

		// task_notification{stopped} — emitTaskTerminatedSdk emits the
		// SDK-normalized form. Must not downgrade the existing row's
		// Status from "killed" to "errored".
		notifMeta, _ := json.Marshal(map[string]any{
			"task_id":     "tsk-stop",
			"tool_use_id": "bg-stop",
			"status":      "stopped",
		})
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventBackgroundTaskNotification, ThreadID: "t1", ItemID: "bg-stop",
			Meta: notifMeta, Content: `Background command "sleep 3600" was stopped`, Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("task_notification{stopped}: %v", err)
		}

		dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
		if len(dones) != 1 {
			t.Fatalf("expected 1 completion sibling, got %d", len(dones))
		}
		if dones[0].Status != statusKilled {
			t.Errorf("Status = %q, want %q (Stopped badge, not Failed)", dones[0].Status, statusKilled)
		}
	})

	t.Run("notification_first_then_task_updated", func(t *testing.T) {
		router, st, _ := newTestRouter(t)
		createTestThread(t, st, "t2")

		startMeta, _ := json.Marshal(map[string]any{
			"toolName":      "Bash",
			"is_background": true,
			"input":         map[string]any{"command": "sleep 3600"},
		})
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventToolStart, ThreadID: "t2", ItemID: "bg-stop2",
			ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("seed launch: %v", err)
		}

		// task_notification{stopped} arrives BEFORE task_updated.
		// No stash, no existing sibling — notification persists as a
		// notification row only, no sibling write yet.
		notifMeta, _ := json.Marshal(map[string]any{
			"task_id":     "tsk-stop2",
			"tool_use_id": "bg-stop2",
			"status":      "stopped",
		})
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventBackgroundTaskNotification, ThreadID: "t2", ItemID: "bg-stop2",
			Meta: notifMeta, Content: `Background command "sleep 3600" was stopped`, Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("task_notification{stopped}: %v", err)
		}

		// task_updated{killed} arrives second — observe path writes
		// the sibling now. Status must render as killed.
		killMeta, _ := json.Marshal(map[string]any{
			"task_id":     "tsk-stop2",
			"tool_use_id": "bg-stop2",
			"status":      "killed",
			"is_error":    true,
			"source":      "task_updated",
		})
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t2", ItemID: "bg-stop2",
			Meta: killMeta, Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("task_updated{killed}: %v", err)
		}

		dones := findItemsByKind(t, st, "t2", itemKindBackgroundDone)
		if len(dones) != 1 {
			t.Fatalf("expected 1 completion sibling, got %d", len(dones))
		}
		if dones[0].Status != statusKilled {
			t.Errorf("Status = %q, want %q (Stopped badge, not Failed)", dones[0].Status, statusKilled)
		}
	})
}

func TestBackgroundTaskNotification_ReplayPreservesOriginalTimelinePosition(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Bash",
		"is_background": true,
		"input":         map[string]any{"command": "sleep 1"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "bg-replay",
		ItemType:  "Bash",
		Meta:      startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	notificationMeta, _ := json.Marshal(map[string]any{
		"task_id":     "task-replay",
		"tool_use_id": "bg-replay",
		"status":      "completed",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventBackgroundTaskNotification,
		ThreadID:  "t1",
		ItemID:    "bg-replay",
		Meta:      notificationMeta,
		Content:   `Background command "sleep 1" completed`,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("first notification: %v", err)
	}

	initial, ok, err := st.GetThreadItem("t1", nextTaskNotificationID("task-replay"))
	if err != nil || !ok {
		t.Fatalf("lookup initial notification: ok=%v err=%v", ok, err)
	}

	seedOpenTurn(t, router, st, "t1", 3)
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventBackgroundTaskNotification,
		ThreadID:  "t1",
		ItemID:    "bg-replay",
		Meta:      notificationMeta,
		Content:   `Background command "sleep 1" completed`,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("replayed notification: %v", err)
	}

	after, ok, err := st.GetThreadItem("t1", nextTaskNotificationID("task-replay"))
	if err != nil || !ok {
		t.Fatalf("lookup replayed notification: ok=%v err=%v", ok, err)
	}
	if after.TurnIndex != initial.TurnIndex {
		t.Fatalf("notification turn_index moved from %d to %d", initial.TurnIndex, after.TurnIndex)
	}
	if after.ItemIndex != initial.ItemIndex {
		t.Fatalf("notification item_index moved from %d to %d", initial.ItemIndex, after.ItemIndex)
	}
}

func TestReadClaudeTaskOutputFileRejectsPathsOutsideAllowedTempRoots(t *testing.T) {
	repoFile, err := filepath.Abs("../../go.mod")
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}
	if _, _, err := readClaudeTaskOutputFile(repoFile, 1024); err == nil {
		t.Fatal("expected non-temp repo path to be rejected")
	}
}

// TestReadClaudeTaskOutputFile_AcceptsSymlinkInsideAllowedRoot pins the
// fix for the silently-dropped Task subagent payloads bug. Claude wraps
// each task output as a symlink (`<task_id>.output → <subagent>.jsonl`)
// inside the allowed roots; an unconditional symlink rejection
// (the previous behaviour) lost every Task subagent's structured
// output — confirmed in production via launcher.log spam of
// "output_file path is a symlink".
//
// This test creates a regular file inside a temp dir, points a symlink
// at it from the same temp dir, and asserts the read succeeds — the
// resolved target lives in an allowed root, so the containment guard
// is satisfied even when the path entered through a symlink.
func TestReadClaudeTaskOutputFile_AcceptsSymlinkInsideAllowedRoot(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "task.jsonl")
	contents := []byte(`{"hello":"world"}`)
	if err := os.WriteFile(target, contents, 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(dir, "task-id.output")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	data, _, err := readClaudeTaskOutputFile(link, int64(len(contents)*4))
	if err != nil {
		t.Fatalf("read symlink: %v", err)
	}
	if string(data) != string(contents) {
		t.Errorf("read data = %q, want %q", string(data), string(contents))
	}
}

// TestReadClaudeTaskOutputFile_RejectsSymlinkOutsideAllowedRoots pins
// the containment guard: a symlink that resolves to a path OUTSIDE
// the allowed roots (temp dirs + ~/.claude/projects/) is still
// rejected. Without this assertion, dropping the unconditional
// symlink rejection would let an attacker who plants a malicious
// `output_file` entry escape the allow-list.
func TestReadClaudeTaskOutputFile_RejectsSymlinkOutsideAllowedRoots(t *testing.T) {
	// The target lives outside any allowed root. The repo's go.mod is
	// a stable file we know we control inside this checkout.
	target, err := filepath.Abs("../../go.mod")
	if err != nil {
		t.Fatalf("abs target: %v", err)
	}
	dir := t.TempDir()
	link := filepath.Join(dir, "escape.output")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, _, err := readClaudeTaskOutputFile(link, 1024); err == nil {
		t.Fatal("expected symlink resolving outside allowed roots to be rejected")
	}
}

// TestAllowedClaudeOutputRoots_IncludesClaudeProjectsDir asserts that
// the resolved-roots list includes ~/.claude/projects/ when the
// home directory is available. This is the canonical location Claude
// puts subagent jsonl files; without it, the symlink-resolved target
// in TestReadClaudeTaskOutputFile_AcceptsSymlinkInsideAllowedRoot
// passes only because we control both the link and the target inside
// the same temp dir, but production data points at a path under
// ~/.claude/projects/ that the temp-only allow-list would reject.
func TestAllowedClaudeOutputRoots_IncludesClaudeProjectsDir(t *testing.T) {
	resetAllowedClaudeOutputRootsForTest()
	t.Cleanup(resetAllowedClaudeOutputRootsForTest)

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir resolvable: %v", err)
	}
	want := filepath.Join(home, ".claude", "projects")
	resolved, err := filepath.EvalSymlinks(want)
	if err != nil {
		// Directory may not exist on a clean machine. Cleaned candidate
		// is still added to the list for stability — verify against
		// that form instead.
		resolved = filepath.Clean(want)
	}

	roots := allowedClaudeOutputRoots()
	for _, r := range roots {
		if r == resolved {
			return
		}
	}
	t.Errorf("allowedClaudeOutputRoots() did not include %q; got %v", resolved, roots)
}
