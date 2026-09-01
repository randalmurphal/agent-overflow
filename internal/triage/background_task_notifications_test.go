package triage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
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

// A task_notification whose launch cannot be resolved writes no rows
// AND leaves the stashed terminal alone: the launch row may still land
// later (subagent transcript projection lags the main wire), and
// draining the stash here destroyed the only evidence the late row
// could settle against. Rowless stashes are pruned by the session-end
// settle and the boot sweep instead.
func TestBackgroundTaskNotification_PreservesStashWithoutLaunchAndCreatesNoRows(t *testing.T) {
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
				t.Fatalf("read stash: %v", err)
			} else if !found {
				t.Fatal("expected hidden-task stash to be preserved for a late-arriving launch row")
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

// TestBackgroundTaskNotification_StashedTerminalThenNotificationWritesSibling
// pins the case-1 regression from the screenshot scenario: a
// background subagent (local_agent) completes WHILE a concurrent
// foreground tool_result is in flight. Claude's CLI delivers the
// completion through `system/task_updated{completed}` (which the
// router stashes) and then the synthetic `<task-notification>` XML
// echo on a `user{isReplay:true}` envelope (parser-side fix at
// parse_user_replay.go promotes this to EventBackgroundTaskNotification).
// The structured `system/task_notification` envelope is NEVER emitted
// for this scenario. The handler MUST drain the stash and write the
// `tool_completion` sibling so the launch row gets its "-> done"
// companion in the UI. Without this drain the launch stays running
// forever (the original bug).
func TestBackgroundTaskNotification_StashedTerminalThenNotificationWritesSibling(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	// Background subagent launch (run_in_background:true, like the
	// `Agent` tool with run_in_background).
	startMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Agent",
		"is_background": true,
		"input": map[string]any{
			"description":       "Background python sleep test",
			"prompt":            "sleep then report done",
			"run_in_background": true,
		},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "bg-subagent",
		ItemType:  "Agent",
		Meta:      startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	// task_started meta-update stamps task_id onto the launch (the
	// real parser emits this; we feed it directly to mirror).
	taskStartedMeta, _ := json.Marshal(map[string]any{
		"task_id": "task-bg-subagent-1",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "bg-subagent",
		Meta:      taskStartedMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("task_started meta-update: %v", err)
	}

	// 1. task_updated{completed} arrives — host signalled exit. Triage
	// stashes; no sibling yet, launch stays running.
	stashMeta, _ := json.Marshal(map[string]any{
		"task_id":     "task-bg-subagent-1",
		"tool_use_id": "bg-subagent",
		"status":      "completed",
		"source":      "task_updated",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventBackgroundTaskTerminal,
		ThreadID:  "t1",
		Meta:      stashMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("task_updated stash: %v", err)
	}
	if dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone); len(dones) != 0 {
		t.Fatalf("task_updated{completed} must NOT write a sibling yet, got %d", len(dones))
	}
	if _, found, err := st.GetPendingBackgroundTerminal("t1", "task-bg-subagent-1"); err != nil {
		t.Fatalf("read stash: %v", err)
	} else if !found {
		t.Fatal("expected stash entry after task_updated{completed}")
	}

	// 2. Synthetic XML echo arrives (the parser fix promotes the
	// isReplay envelope to this EventBackgroundTaskNotification).
	// The handler must: drain stash, write tool_completion sibling,
	// then write the notification row (sibling-first — see
	// TestBackgroundTaskNotification_SiblingEmitsBeforeNotificationRow).
	notificationMeta, _ := json.Marshal(map[string]any{
		"task_id":     "task-bg-subagent-1",
		"tool_use_id": "bg-subagent",
		"status":      "completed",
		"source":      "task_notification",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventBackgroundTaskNotification,
		ThreadID:  "t1",
		ItemID:    "bg-subagent",
		Meta:      notificationMeta,
		Content:   `Agent "Background python sleep test" completed`,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("task_notification: %v", err)
	}

	if _, found, err := st.GetPendingBackgroundTerminal("t1", "task-bg-subagent-1"); err != nil {
		t.Fatalf("read drained stash: %v", err)
	} else if found {
		t.Fatal("stash must be drained after task_notification")
	}

	dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(dones) != 1 {
		t.Fatalf("expected 1 tool_completion sibling, got %d (the case-1 bug)", len(dones))
	}
	done := dones[0]
	if done.ID != ToolCompletionID("bg-subagent") {
		t.Fatalf("sibling.id = %q, want %q (stable per-launch derivation)", done.ID, ToolCompletionID("bg-subagent"))
	}
	if done.CompletionOf != "bg-subagent" {
		t.Fatalf("sibling.completionOf = %q, want bg-subagent", done.CompletionOf)
	}
	if done.Status != statusCompleted {
		t.Fatalf("sibling.status = %q, want completed", done.Status)
	}
	if !done.IsBackground {
		t.Fatal("sibling.isBackground = false, want true (inherited from launch)")
	}
	// `buildBackgroundTerminalSummary` joins the launch summary with the
	// outcome marker as "launch -> done" — pin both halves so a refactor
	// of either piece fails loudly. The launch summary for the Agent
	// tool with `description: "Background python sleep test"` derives
	// from the description.
	if !strings.HasSuffix(done.Summary, " -> done") {
		t.Fatalf("sibling.summary = %q, want trailing ` -> done` outcome marker", done.Summary)
	}
	if !strings.Contains(done.Summary, "Background python sleep test") {
		t.Fatalf("sibling.summary = %q, want launch-summary prefix derived from Agent description", done.Summary)
	}
	doneMeta := decodeItemMetaMap(t, done.Meta)
	if doneMeta["task_id"] != "task-bg-subagent-1" {
		t.Fatalf("sibling.meta.task_id = %v, want task-bg-subagent-1", doneMeta["task_id"])
	}
	// `drainTaskNotificationStash` stamps Source="task_notification"
	// on the synthetic terminal it builds from the merged stash+notif
	// fields (background_task_notifications.go), so the persisted
	// status_source reflects the wire envelope that drove the write
	// — the notification, not the stash entry that supplied the rest.
	if doneMeta["status_source"] != "task_notification" {
		t.Fatalf("sibling.meta.status_source = %v, want task_notification (drain stamps the source)", doneMeta["status_source"])
	}
	if doneMeta["tool_use_id"] != "bg-subagent" {
		t.Fatalf("sibling.meta.tool_use_id = %v, want bg-subagent", doneMeta["tool_use_id"])
	}

	// Notification row also written (subagent is is_background=true).
	notifications := findItemsByKind(t, st, "t1", itemKindNotification)
	if len(notifications) != 1 {
		t.Fatalf("expected 1 notification row alongside the sibling, got %d", len(notifications))
	}
}

// TestBackgroundTaskNotification_SiblingEmitsBeforeNotificationRow pins
// the wire ORDER of the two rows the stash-drain path writes. The
// frontend hides the notification row (whose summary is the agent's
// full report text) only once a completed lifecycle row with the same
// task_id exists (filterRedundantNotifications), and a backgrounded
// launch is deliberately held at `running` until the sibling lands — so
// a notification-first emission renders the entire report as a
// full-width timeline row for one flush and then rips it back out when
// the sibling arrives, clamping the reader's scroll position
// (bug-report-20260801T024731Z). The `tool_completion` sibling upsert
// must reach the frontend before the notification row's first upsert.
func TestBackgroundTaskNotification_SiblingEmitsBeforeNotificationRow(t *testing.T) {
	router, st, emissions := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Agent",
		"is_background": true,
		"input": map[string]any{
			"description":       "Background audit",
			"prompt":            "audit then report",
			"run_in_background": true,
		},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "bg-subagent",
		ItemType:  "Agent",
		Meta:      startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	taskStartedMeta, _ := json.Marshal(map[string]any{
		"task_id": "task-bg-subagent-1",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "bg-subagent",
		Meta:      taskStartedMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("task_started meta-update: %v", err)
	}
	stashMeta, _ := json.Marshal(map[string]any{
		"task_id":     "task-bg-subagent-1",
		"tool_use_id": "bg-subagent",
		"status":      "completed",
		"source":      "task_updated",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventBackgroundTaskTerminal,
		ThreadID:  "t1",
		Meta:      stashMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("task_updated stash: %v", err)
	}

	emissions.reset()
	notificationMeta, _ := json.Marshal(map[string]any{
		"task_id":     "task-bg-subagent-1",
		"tool_use_id": "bg-subagent",
		"status":      "completed",
		"source":      "task_notification",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventBackgroundTaskNotification,
		ThreadID:  "t1",
		ItemID:    "bg-subagent",
		Meta:      notificationMeta,
		Content:   `Agent "Background audit" completed: <several thousand pixels of final report>`,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("task_notification: %v", err)
	}

	snapshot := emissions.snapshot()
	firstUpsertIndex := func(itemID string) int {
		for i, e := range snapshot {
			if e.eventName != "provider:item_event" {
				continue
			}
			ev, ok := e.data.(ItemStreamEvent)
			if !ok || ev.Action != itemStreamActionUpsert {
				continue
			}
			if ev.Item != nil && ev.Item.ID == itemID {
				return i
			}
		}
		return -1
	}
	siblingIdx := firstUpsertIndex(ToolCompletionID("bg-subagent"))
	notificationIdx := firstUpsertIndex(nextTaskNotificationID("task-bg-subagent-1", ""))
	if siblingIdx < 0 {
		t.Fatalf("no upsert emission for the tool_completion sibling (emissions: %+v)", snapshot)
	}
	if notificationIdx < 0 {
		t.Fatalf("no upsert emission for the notification row (emissions: %+v)", snapshot)
	}
	if siblingIdx > notificationIdx {
		t.Fatalf("sibling upsert emitted at index %d AFTER notification row at %d — the notification renders unsuppressed for a flush (the report-flash bug)", siblingIdx, notificationIdx)
	}
}

// TestBackgroundTaskNotification_StashedTerminalThenNotificationIsIdempotent
// pins the safety property for the rare scenario where Claude's CLI
// delivers BOTH the structured `system/task_notification` envelope AND
// the synthetic `<task-notification>` XML echo for the same task_id
// (e.g. a future wire change that overlaps the channels). The router
// must produce exactly one `tool_completion` sibling row and exactly
// one notification row, regardless of how many notification events
// arrive on the same task_id. The stash is one-shot via
// `RemovePendingBackgroundTerminal`; the second notification finds it
// empty and no-ops the sibling write.
func TestBackgroundTaskNotification_StashedTerminalThenNotificationIsIdempotent(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Agent",
		"is_background": true,
		"input": map[string]any{
			"description":       "idempotency probe",
			"run_in_background": true,
		},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:     provider.EventToolStart,
		ThreadID: "t1", ItemID: "bg-idemp", ItemType: "Agent",
		Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	stashMeta, _ := json.Marshal(map[string]any{
		"task_id":     "tsk-idemp",
		"tool_use_id": "bg-idemp",
		"status":      "completed",
		"source":      "task_updated",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1",
		Meta: stashMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("task_updated stash: %v", err)
	}

	notifMeta, _ := json.Marshal(map[string]any{
		"task_id":     "tsk-idemp",
		"tool_use_id": "bg-idemp",
		"status":      "completed",
		"source":      "task_notification",
	})
	// First notification — drains stash, writes sibling.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskNotification, ThreadID: "t1", ItemID: "bg-idemp",
		Meta: notifMeta, Content: "first observation", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("first notification: %v", err)
	}
	// Second notification — stash is gone, must not double-write.
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskNotification, ThreadID: "t1", ItemID: "bg-idemp",
		Meta: notifMeta, Content: "duplicate observation", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("second notification: %v", err)
	}

	dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(dones) != 1 {
		t.Fatalf("expected exactly 1 sibling after duplicate notifications, got %d (idempotency broken)", len(dones))
	}
	notifications := findItemsByKind(t, st, "t1", itemKindNotification)
	if len(notifications) != 1 {
		t.Fatalf("expected exactly 1 notification row (id keyed by task_id), got %d", len(notifications))
	}
}

// TestBackgroundTaskNotification_ForegroundToolSkipsNotificationRow
// pins Fix B: Claude's CLI emits `system/task_notification` for EVERY
// Bash/Task lifecycle, including foreground inline calls (not just
// run_in_background:true). The launch's own status flip is the
// user-visible completion signal; an additional notification row
// would be redundant and the frontend filter
// (notificationFilter.ts) already drops it. The triage handler must
// skip the row write so we don't accumulate dead SQLite rows. The
// launch row itself stays untouched (any status flip is the
// EventToolComplete handler's job).
func TestBackgroundTaskNotification_ForegroundToolSkipsNotificationRow(t *testing.T) {
	t.Run("notification_without_stash_skips_row", func(t *testing.T) {
		router, st, _ := newTestRouter(t)
		createTestThread(t, st, "t1")

		// Launch a FOREGROUND Bash (no is_background flag).
		startMeta, _ := json.Marshal(map[string]any{
			"toolName": "Bash",
			"input":    map[string]any{"command": "echo hi"},
		})
		if err := router.Handle(provider.ProviderEvent{
			Kind:      provider.EventToolStart,
			ThreadID:  "t1",
			ItemID:    "fg-bash",
			ItemType:  "Bash",
			Meta:      startMeta,
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("start: %v", err)
		}

		notificationMeta, _ := json.Marshal(map[string]any{
			"task_id":     "task-fg-1",
			"tool_use_id": "fg-bash",
			"status":      "completed",
		})
		if err := router.Handle(provider.ProviderEvent{
			Kind:      provider.EventBackgroundTaskNotification,
			ThreadID:  "t1",
			ItemID:    "fg-bash",
			Meta:      notificationMeta,
			Content:   `Bash "echo hi" completed`,
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("notification: %v", err)
		}

		notifications := findItemsByKind(t, st, "t1", itemKindNotification)
		if len(notifications) != 0 {
			t.Fatalf("foreground task_notification must NOT write a notification row, got %d: %+v", len(notifications), notifications)
		}

		// Launch row unchanged — task_notification carries no
		// EventToolComplete semantics, so the bash's `running` status stays
		// running until the tool_result envelope flips it.
		launch, ok, err := st.GetThreadItem("t1", "fg-bash")
		if err != nil || !ok {
			t.Fatalf("lookup launch: ok=%v err=%v", ok, err)
		}
		if launch.IsBackground {
			t.Fatal("foreground launch must not flip to is_background")
		}
		if launch.Status != statusRunning {
			t.Fatalf("launch status = %q, want running (task_notification is not a lifecycle source)", launch.Status)
		}

		dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
		if len(dones) != 0 {
			t.Fatalf("foreground task_notification must not synthesize completion rows, got %+v", dones)
		}
	})

	// The Fix B early-return preserves the defensive stash drain for the
	// foreground path. Foreground tools don't stash today (only
	// task_updated{completed,failed} for backgrounded launches stages a
	// row), but if a future change ever stages one for a foreground
	// task_id, the drain must still execute so the stash doesn't
	// leak. This subtest fabricates that condition by writing the stash
	// directly through the store API, then asserts the foreground-path
	// notification still drains it. Guards against a refactor that
	// returns early before reaching the drain helper.
	t.Run("notification_with_stash_still_drains_stash", func(t *testing.T) {
		router, st, _ := newTestRouter(t)
		createTestThread(t, st, "t1")

		startMeta, _ := json.Marshal(map[string]any{
			"toolName": "Bash",
			"input":    map[string]any{"command": "echo hi"},
		})
		if err := router.Handle(provider.ProviderEvent{
			Kind:      provider.EventToolStart,
			ThreadID:  "t1",
			ItemID:    "fg-bash-stash",
			ItemType:  "Bash",
			Meta:      startMeta,
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("start: %v", err)
		}

		// Fabricate the stash directly. Foreground task_updated doesn't
		// stash today; we want to prove the drain helper still runs from
		// the foreground notification path even when one exists.
		if err := st.UpsertPendingBackgroundTerminal(store.PendingBackgroundTaskTerminal{
			ThreadID:  "t1",
			TaskID:    "task-fg-stash",
			ToolUseID: "fg-bash-stash",
			Status:    "completed",
			Source:    "task_updated",
			CreatedAt: time.Now().UnixMilli(),
		}); err != nil {
			t.Fatalf("seed stash: %v", err)
		}
		if _, found, err := st.GetPendingBackgroundTerminal("t1", "task-fg-stash"); err != nil {
			t.Fatalf("read stash: %v", err)
		} else if !found {
			t.Fatal("expected stash before notification")
		}

		notificationMeta, _ := json.Marshal(map[string]any{
			"task_id":     "task-fg-stash",
			"tool_use_id": "fg-bash-stash",
			"status":      "completed",
		})
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventBackgroundTaskNotification, ThreadID: "t1",
			ItemID: "fg-bash-stash", Meta: notificationMeta,
			Content: "fg notif w/ stash", Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("notification: %v", err)
		}

		if _, found, err := st.GetPendingBackgroundTerminal("t1", "task-fg-stash"); err != nil {
			t.Fatalf("read drained stash: %v", err)
		} else if found {
			t.Fatal("foreground notification must still drain the stash even though it skips the notification-row write")
		}

		notifications := findItemsByKind(t, st, "t1", itemKindNotification)
		if len(notifications) != 0 {
			t.Fatalf("foreground notification must NOT write a notification row even with stash drain, got %d", len(notifications))
		}
		// Fix C: this exact drain path (foreground launch + a drained
		// task_updated stash) is bug 2's reproduction — see
		// TestBackgroundTaskNotification_InlineAgentSkipsSiblingRow for
		// the full production-shaped scenario. Assert it here too since
		// this subtest already exercises the same drain call.
		if dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone); len(dones) != 0 {
			t.Fatalf("foreground notification must NOT write a tool_completion sibling even with stash drain, got %d: %+v", len(dones), dones)
		}
	})
}

// TestBackgroundTaskNotification_InlineAgentSkipsSiblingRow pins Fix C:
// Claude emits the FULL task lifecycle (task_started/task_updated/
// task_notification) for `local_agent` (Task/Agent) launches regardless
// of whether they ran inline (awaited) or backgrounded — task_started
// fires for every Bash/Task (see provider/claude/CLAUDE.md
// §task_started). An inline launch already completes in place via its
// own real `tool_result` (EventToolComplete with no is_background); the
// task_updated -> task_notification signal reaching
// writeBackgroundCompletionSibling for that same launch must NOT
// produce a redundant "-> done" tool_completion sibling row. Without
// the `!launch.IsBackground` gate in writeBackgroundCompletionSibling,
// drainTaskNotificationStash still finds the task_updated stash and
// writes the sibling anyway (the case-2 bug: six inline launches in
// production each grew a spurious `status_source:"task_notification"`
// sibling). FAILS pre-Fix-C.
func TestBackgroundTaskNotification_InlineAgentSkipsSiblingRow(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	// Foreground/inline Agent launch — no run_in_background anywhere in
	// the input, so the launch never carries is_background.
	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "Agent",
		"input":    map[string]any{"description": "Review 4b: perf/memory lens"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "inline-agent",
		ItemType:  "Agent",
		Meta:      startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	// system/task_started meta-update — fires for EVERY Task launch,
	// inline or backgrounded.
	taskStartedMeta, _ := json.Marshal(map[string]any{
		"task_id": "task-inline-1",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "inline-agent",
		Meta:      taskStartedMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("task_started meta-update: %v", err)
	}

	// The real, awaited tool_result completes the launch in place —
	// no is_background anywhere on this event.
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolComplete,
		ThreadID:  "t1",
		ItemID:    "inline-agent",
		Content:   "the agent's real result text",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("inline complete: %v", err)
	}

	launch, ok, err := st.GetThreadItem("t1", "inline-agent")
	if err != nil || !ok {
		t.Fatalf("lookup launch: ok=%v err=%v", ok, err)
	}
	if launch.Status != statusCompleted {
		t.Fatalf("launch status = %q, want completed (inline result already landed)", launch.Status)
	}
	if launch.IsBackground {
		t.Fatal("inline launch must not carry is_background")
	}

	// Claude still emits the task lifecycle terminal for this inline
	// launch — task_updated stashes it (Tray-A contract, unconditional).
	stashMeta, _ := json.Marshal(map[string]any{
		"task_id":     "task-inline-1",
		"tool_use_id": "inline-agent",
		"status":      "completed",
		"source":      "task_updated",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventBackgroundTaskTerminal,
		ThreadID:  "t1",
		Meta:      stashMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("task_updated stash: %v", err)
	}
	if _, found, err := st.GetPendingBackgroundTerminal("t1", "task-inline-1"); err != nil {
		t.Fatalf("read stash: %v", err)
	} else if !found {
		t.Fatal("expected stash entry after task_updated{completed}")
	}

	// ...then task_notification arrives and must drain the stash
	// without writing a sibling row.
	notificationMeta, _ := json.Marshal(map[string]any{
		"task_id":     "task-inline-1",
		"tool_use_id": "inline-agent",
		"status":      "completed",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventBackgroundTaskNotification,
		ThreadID:  "t1",
		ItemID:    "inline-agent",
		Meta:      notificationMeta,
		Content:   `Agent "Review 4b: perf/memory lens" completed`,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("task_notification: %v", err)
	}

	if _, found, err := st.GetPendingBackgroundTerminal("t1", "task-inline-1"); err != nil {
		t.Fatalf("read drained stash: %v", err)
	} else if found {
		t.Fatal("stash must still be drained even though no sibling is written")
	}

	siblingID := backgroundCompletionID(launch.ID, "task-inline-1")
	if _, found, err := st.GetThreadItem("t1", siblingID); err != nil {
		t.Fatalf("sibling lookup %s: %v", siblingID, err)
	} else if found {
		t.Fatalf("Fix C regression: inline launch %q got a redundant tool_completion sibling row %q", launch.ID, siblingID)
	}
	if dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone); len(dones) != 0 {
		t.Fatalf("expected 0 tool_completion sibling rows for an inline launch, got %d: %+v", len(dones), dones)
	}
	if notifications := findItemsByKind(t, st, "t1", itemKindNotification); len(notifications) != 0 {
		t.Fatalf("expected 0 notification rows for an inline launch, got %d", len(notifications))
	}

	final, ok, err := st.GetThreadItem("t1", "inline-agent")
	if err != nil || !ok {
		t.Fatalf("final launch lookup: ok=%v err=%v", ok, err)
	}
	if final.Status != statusCompleted {
		t.Fatalf("launch status after task_notification = %q, want completed (unchanged by the drain)", final.Status)
	}
	if final.Summary == "" {
		t.Fatal("launch must keep its real summary, not get overwritten by the drained task lifecycle")
	}
}

// TestBackgroundTaskNotification_InlineAgentKilledTerminalSkipsSibling
// pins Fix C on the OTHER route into writeBackgroundCompletionSibling:
// `task_updated{status:"killed"}` bypasses the stash entirely and goes
// through observeBackgroundTaskTerminal directly (the user-stop
// carve-out in handleBackgroundTaskTerminal), so the sibling write is
// attempted immediately — no notification drain involved. An INLINE
// launch that already completed via its own tool_result must not grow
// a sibling from that direct path either. FAILS pre-Fix-C (the direct
// observe path wrote a killed sibling for the completed inline launch).
func TestBackgroundTaskNotification_InlineAgentKilledTerminalSkipsSibling(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "Agent",
		"input":    map[string]any{"description": "Inline agent later killed"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "inline-agent-killed",
		ItemType:  "Agent",
		Meta:      startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	taskStartedMeta, _ := json.Marshal(map[string]any{
		"task_id": "task-inline-killed-1",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "inline-agent-killed",
		Meta:      taskStartedMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("task_started meta-update: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolComplete,
		ThreadID:  "t1",
		ItemID:    "inline-agent-killed",
		Content:   "the agent's real result text",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("inline complete: %v", err)
	}

	// task_updated{killed} routes through observeBackgroundTaskTerminal
	// directly — no stash step, so the gate inside
	// writeBackgroundCompletionSibling is the ONLY thing between this
	// event and a spurious killed sibling on the completed inline row.
	killMeta, _ := json.Marshal(map[string]any{
		"task_id":     "task-inline-killed-1",
		"tool_use_id": "inline-agent-killed",
		"status":      "killed",
		"is_error":    true,
		"source":      "task_updated",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventBackgroundTaskTerminal,
		ThreadID:  "t1",
		ItemID:    "inline-agent-killed",
		Meta:      killMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("task_updated{killed}: %v", err)
	}

	if dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone); len(dones) != 0 {
		t.Fatalf("inline launch must not grow a killed sibling from the direct observe path, got %d: %+v", len(dones), dones)
	}
	if notifications := findItemsByKind(t, st, "t1", itemKindNotification); len(notifications) != 0 {
		t.Fatalf("expected 0 notification rows, got %d", len(notifications))
	}
	launch, ok, err := st.GetThreadItem("t1", "inline-agent-killed")
	if err != nil || !ok {
		t.Fatalf("lookup launch: ok=%v err=%v", ok, err)
	}
	if launch.Status != statusCompleted {
		t.Fatalf("launch status = %q, want completed (unchanged by the killed terminal)", launch.Status)
	}
}

// TestBackgroundTaskNotification_AsyncAgentLaunchStillGetsSibling is
// the regression pin for the backgrounded flow alongside Fix C's new
// gate: an async `local_agent` launch (Shape 2 in claude-wire.md —
// the tool_use input carries NO run_in_background, so is_background is
// only known once the "Async agent launched successfully." ack's
// EventToolComplete arrives, per Fix B's toolResultAsyncLaunch) must
// still get its tool_completion sibling once the task lifecycle
// drains. Passes both before and after Fix C — pins that the new
// `!launch.IsBackground` gate does not regress the real backgrounded
// path.
func TestBackgroundTaskNotification_AsyncAgentLaunchStillGetsSibling(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	// The launch's own tool_use carries no run_in_background — unlike
	// classic backgrounding, is_background is unknown at EventToolStart
	// time for an async local_agent launch.
	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "Agent",
		"input":    map[string]any{"description": "Review 4b: perf/memory lens"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "async-agent",
		ItemType:  "Agent",
		Meta:      startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	taskStartedMeta, _ := json.Marshal(map[string]any{
		"task_id": "task-async-1",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    "async-agent",
		Meta:      taskStartedMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("task_started meta-update: %v", err)
	}

	// The bare async ack — is_background arrives on the COMPLETE event,
	// not the start event, matching what the fixed parser
	// (toolResultAsyncLaunch) emits for Shape 2.
	completeMeta, _ := json.Marshal(map[string]any{
		"is_background": true,
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolComplete,
		ThreadID:  "t1",
		ItemID:    "async-agent",
		Content:   "Async agent launched successfully.",
		Meta:      completeMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("async ack complete: %v", err)
	}

	launch, ok, err := st.GetThreadItem("t1", "async-agent")
	if err != nil || !ok {
		t.Fatalf("lookup launch: ok=%v err=%v", ok, err)
	}
	if launch.Status != statusRunning {
		t.Fatalf("launch status = %q, want running (async ack is a placeholder, not the real result)", launch.Status)
	}
	if !launch.IsBackground {
		t.Fatal("launch must carry is_background after the async ack")
	}

	stashMeta, _ := json.Marshal(map[string]any{
		"task_id":     "task-async-1",
		"tool_use_id": "async-agent",
		"status":      "completed",
		"source":      "task_updated",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventBackgroundTaskTerminal,
		ThreadID:  "t1",
		Meta:      stashMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("task_updated stash: %v", err)
	}

	notificationMeta, _ := json.Marshal(map[string]any{
		"task_id":     "task-async-1",
		"tool_use_id": "async-agent",
		"status":      "completed",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventBackgroundTaskNotification,
		ThreadID:  "t1",
		ItemID:    "async-agent",
		Meta:      notificationMeta,
		Content:   `Agent "Review 4b: perf/memory lens" completed`,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("task_notification: %v", err)
	}

	if notifications := findItemsByKind(t, st, "t1", itemKindNotification); len(notifications) != 1 {
		t.Fatalf("expected 1 notification row for the backgrounded launch, got %d", len(notifications))
	}

	dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(dones) != 1 {
		t.Fatalf("expected 1 tool_completion sibling for the backgrounded launch, got %d", len(dones))
	}
	done := dones[0]
	if done.CompletionOf != "async-agent" {
		t.Fatalf("sibling.completionOf = %q, want async-agent", done.CompletionOf)
	}
	if !done.IsBackground {
		t.Fatal("sibling.isBackground = false, want true (inherited from launch)")
	}
	if done.Status != statusCompleted {
		t.Fatalf("sibling.status = %q, want completed", done.Status)
	}
}

// TestBackgroundTaskNotification_ResumeCarrierBecomesBackgroundCarrier
// pins the resume-carrier design (claude-wire.md §E6): when Claude
// resumes an idle async agent via the harness's SendMessage tool, the
// parser rebinds `system/task_started` onto SendMessage's own
// tool_use_id (parse_system.go) and marks it backgrounded. This test
// drives the exact router.Handle sequence that rebind produces —
// EventToolStart (normal launch) -> EventToolStart (meta-only rebind
// carrying task_id/resumes_tool_use_id/description) -> EventToolComplete
// (ack, is_background:true) — and asserts the SendMessage row becomes
// the resumed round's background carrier: bg=1, running, Summary
// rewritten to the agent-centric form, and visible to the reaper's
// ListRunningBackgroundToolCalls predicate. Round 2's terminal +
// notification then produce a NEW sibling under the carrier while the
// original launch keeps exactly its round-1 sibling untouched.
func TestBackgroundTaskNotification_ResumeCarrierBecomesBackgroundCarrier(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	const (
		originalLaunchID = "original-agent"
		carrierID        = "sendmessage-carrier"
		taskID           = "task-resume-1"
		description      = "Frontend transitive suppression fix"
		// The launch INPUT description deliberately differs from the
		// rebind envelope's `description` so resumeCarrierIdentity's two
		// branches are distinguishable: the prefer-original branch
		// yields "Agent: "+launchInputDescription (the original row's
		// own Summary), the fallback would yield "Agent: "+description.
		// With identical strings the prefer-original behavior would be
		// unpinned — the test would pass with that branch deleted.
		launchInputDescription = "Frontend transitive suppression fix (round-1 input)"
	)

	// --- Round 1: original async launch, backgrounded via its ack,
	// completes through the normal task lifecycle.
	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "Agent",
		"input": map[string]any{
			"description":   launchInputDescription,
			"subagent_type": "general-purpose",
			"model":         "opus",
		},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    originalLaunchID,
		ItemType:  "Agent",
		Meta:      startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("original launch start: %v", err)
	}
	taskStartedMeta, _ := json.Marshal(map[string]any{"task_id": taskID})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    originalLaunchID,
		Meta:      taskStartedMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("original launch task_started: %v", err)
	}
	// The child's first assistant envelope stamps the resolved model onto
	// the launch (the Subn meta-update) — the authoritative source the
	// identity copy must prefer over the input's "opus" alias.
	subnMeta, _ := json.Marshal(map[string]any{"subagent_model": "claude-opus-4-7"})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    originalLaunchID,
		Meta:      subnMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("original launch subagent_model stamp: %v", err)
	}
	ackMeta, _ := json.Marshal(map[string]any{"is_background": true})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolComplete,
		ThreadID:  "t1",
		ItemID:    originalLaunchID,
		Content:   "Async agent launched successfully.",
		Meta:      ackMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("original launch ack: %v", err)
	}
	round1StashMeta, _ := json.Marshal(map[string]any{
		"task_id": taskID, "tool_use_id": originalLaunchID,
		"status": "completed", "source": "task_updated",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1",
		Meta: round1StashMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("round-1 task_updated stash: %v", err)
	}
	round1NotificationMeta, _ := json.Marshal(map[string]any{
		"task_id": taskID, "tool_use_id": originalLaunchID, "status": "completed",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskNotification, ThreadID: "t1",
		ItemID: originalLaunchID, Meta: round1NotificationMeta,
		Content: `Agent "` + description + `" finished`, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("round-1 task_notification: %v", err)
	}

	round1Sibling := backgroundCompletionID(originalLaunchID, taskID)
	if _, found, err := st.GetThreadItem("t1", round1Sibling); err != nil || !found {
		t.Fatalf("round-1 sibling %s: found=%v err=%v", round1Sibling, found, err)
	}
	originalLaunch, ok, err := st.GetThreadItem("t1", originalLaunchID)
	if err != nil || !ok {
		t.Fatalf("lookup original launch: ok=%v err=%v", ok, err)
	}
	if originalLaunch.Summary != "Agent: "+launchInputDescription {
		t.Fatalf("original launch summary = %q, want %q", originalLaunch.Summary, "Agent: "+launchInputDescription)
	}

	// --- Round 2: resume via SendMessage. The parser's rebind emits a
	// normal launch EventToolStart for SendMessage's own tool_use, then
	// an enriched meta-only EventToolStart from the rebind
	// task_started, then the ack's EventToolComplete with
	// is_background:true (SendMessage's own ack carries no async
	// marker — is_background can only come from the rebind).
	carrierStartMeta, _ := json.Marshal(map[string]any{
		"toolName": "SendMessage",
		"input":    map[string]any{"to": "a464e54e96a45cd0c", "summary": "resume"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    carrierID,
		ItemType:  "SendMessage",
		Meta:      carrierStartMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("carrier launch start: %v", err)
	}
	rebindMeta, _ := json.Marshal(map[string]any{
		"task_id":             taskID,
		"task_type":           "local_agent",
		"resumes_tool_use_id": originalLaunchID,
		"description":         description,
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    carrierID,
		Meta:      rebindMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("carrier rebind meta-update: %v", err)
	}
	carrierAckMeta, _ := json.Marshal(map[string]any{"is_background": true})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolComplete,
		ThreadID:  "t1",
		ItemID:    carrierID,
		Content:   `Agent "a464e54e96a45cd0c" had no active task; resumed from transcript in the background with your message.`,
		Meta:      carrierAckMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("carrier ack: %v", err)
	}

	carrier, ok, err := st.GetThreadItem("t1", carrierID)
	if err != nil || !ok {
		t.Fatalf("lookup carrier: ok=%v err=%v", ok, err)
	}
	if !carrier.IsBackground {
		t.Fatal("carrier must carry is_background=true (FAILS pre-fix: SendMessage's own ack carries no async marker)")
	}
	if carrier.Status != statusRunning {
		t.Fatalf("carrier status = %q, want running", carrier.Status)
	}
	// Must be the ORIGINAL launch row's own Summary (prefer-original
	// branch), not "Agent: "+description (the fallback) and not
	// "SendMessage: ..." (pre-fix, no rewrite at all).
	if carrier.Summary != "Agent: "+launchInputDescription {
		t.Fatalf("carrier summary = %q, want %q (prefer-original rewrite)", carrier.Summary, "Agent: "+launchInputDescription)
	}
	// Identity copy: the keep-running flip resolves the original launch
	// and stamps its identity onto the carrier's meta, so the timeline
	// leaf / tray / completion card render the resumed agent's type and
	// model instead of the SendMessage input's raw recipient id and the
	// parent thread's model. subagent_model must be the Subn stamp
	// (claude-opus-4-7), not the input's "opus" alias.
	var carrierMeta map[string]any
	if err := json.Unmarshal([]byte(carrier.Meta), &carrierMeta); err != nil {
		t.Fatalf("carrier meta unmarshal: %v", err)
	}
	if carrierMeta["subagent_type"] != "general-purpose" {
		t.Fatalf("carrier meta.subagent_type = %v, want general-purpose", carrierMeta["subagent_type"])
	}
	if carrierMeta["subagent_model"] != "claude-opus-4-7" {
		t.Fatalf("carrier meta.subagent_model = %v, want claude-opus-4-7 (Subn stamp preferred over input alias)", carrierMeta["subagent_model"])
	}

	// Reaper-protection pin: the carrier must be the row that keeps
	// ListRunningBackgroundToolCalls non-empty while the resumed agent
	// runs quiet. FAILS pre-fix: the carrier would be foreground
	// (is_background=false) and excluded from this query entirely.
	running, err := st.ListRunningBackgroundToolCalls("t1")
	if err != nil {
		t.Fatalf("ListRunningBackgroundToolCalls: %v", err)
	}
	foundCarrier := false
	for _, item := range running {
		if item.ID == carrierID {
			foundCarrier = true
		}
	}
	if !foundCarrier {
		t.Fatalf("ListRunningBackgroundToolCalls must include the carrier %s, got %+v", carrierID, running)
	}

	// --- Round 2 terminal + notification: a NEW sibling under the
	// carrier, distinct from round 1's sibling under the original
	// launch. The task_id is the SAME across both rounds (Claude
	// rebinds, it doesn't mint a new task), so the notification row is
	// upserted in place rather than duplicated.
	round2StashMeta, _ := json.Marshal(map[string]any{
		"task_id": taskID, "tool_use_id": carrierID,
		"status": "completed", "source": "task_updated",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1",
		Meta: round2StashMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("round-2 task_updated stash: %v", err)
	}
	round2NotificationMeta, _ := json.Marshal(map[string]any{
		"task_id": taskID, "tool_use_id": carrierID, "status": "completed",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskNotification, ThreadID: "t1",
		ItemID: carrierID, Meta: round2NotificationMeta,
		Content: `Agent "` + description + `" finished`, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("round-2 task_notification: %v", err)
	}

	round2Sibling := backgroundCompletionID(carrierID, taskID)
	if round2Sibling == round1Sibling {
		t.Fatalf("round-2 sibling id must differ from round-1's, both are %q", round2Sibling)
	}
	round2Item, found, err := st.GetThreadItem("t1", round2Sibling)
	if err != nil || !found {
		t.Fatalf("round-2 sibling %s: found=%v err=%v", round2Sibling, found, err)
	}
	wantSummary := "Agent: " + launchInputDescription + " -> done"
	if round2Item.Summary != wantSummary {
		t.Fatalf("round-2 sibling summary = %q, want %q", round2Item.Summary, wantSummary)
	}
	if round2Item.CompletionOf != carrierID {
		t.Fatalf("round-2 sibling.completionOf = %q, want %s", round2Item.CompletionOf, carrierID)
	}

	// Original launch must still have exactly its ONE round-1 sibling
	// — round 2 must not touch it.
	dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	originalSiblingCount := 0
	for _, item := range dones {
		if item.CompletionOf == originalLaunchID {
			originalSiblingCount++
		}
	}
	if originalSiblingCount != 1 {
		t.Fatalf("original launch must have exactly 1 sibling after round 2, got %d", originalSiblingCount)
	}
	if len(dones) != 2 {
		t.Fatalf("expected exactly 2 tool_completion siblings total (one per round), got %d: %+v", len(dones), dones)
	}

	// The task_id-keyed notification row is upserted, not duplicated.
	if notifications := findItemsByKind(t, st, "t1", itemKindNotification); len(notifications) != 1 {
		t.Fatalf("expected 1 notification row (task_id-keyed, upserted across rounds), got %d", len(notifications))
	}

	// The original launch ROW itself is untouched by round 2 — same
	// Status and Summary it settled into after round 1.
	launchAfterRound2, ok, err := st.GetThreadItem("t1", originalLaunchID)
	if err != nil || !ok {
		t.Fatalf("re-lookup original launch: ok=%v err=%v", ok, err)
	}
	if launchAfterRound2.Status != originalLaunch.Status || launchAfterRound2.Summary != originalLaunch.Summary {
		t.Fatalf("original launch mutated by round 2: status %q -> %q, summary %q -> %q",
			originalLaunch.Status, launchAfterRound2.Status, originalLaunch.Summary, launchAfterRound2.Summary)
	}
}

// TestBackgroundTaskNotification_ResumeCarrierFallbackDescription pins
// resumeCarrierIdentity's description fallback: when the original launch
// row referenced by resumes_tool_use_id no longer exists (retention
// pruned it), the carrier Summary derives from the rebind envelope's
// description — bounded exactly like every other tool summary
// (truncatePreview: newlines stripped, 80-rune cap). FAILS without the
// truncatePreview bound: the raw multi-line description would land
// verbatim in items.summary.
func TestBackgroundTaskNotification_ResumeCarrierFallbackDescription(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	const carrierID = "sendmessage-carrier-fallback"
	longDescription := "Line one of an adversarially long description\n" + strings.Repeat("x", 120)

	carrierStartMeta, _ := json.Marshal(map[string]any{
		"toolName": "SendMessage",
		"input":    map[string]any{"to": "a464e54e96a45cd0c", "summary": "resume"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    carrierID,
		ItemType:  "SendMessage",
		Meta:      carrierStartMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("carrier launch start: %v", err)
	}
	rebindMeta, _ := json.Marshal(map[string]any{
		"task_id":             "task-fallback-1",
		"task_type":           "local_agent",
		"resumes_tool_use_id": "pruned-original-launch",
		"description":         longDescription,
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  "t1",
		ItemID:    carrierID,
		Meta:      rebindMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("carrier rebind meta-update: %v", err)
	}
	ackMeta, _ := json.Marshal(map[string]any{"is_background": true})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolComplete,
		ThreadID:  "t1",
		ItemID:    carrierID,
		Content:   "resumed from transcript",
		Meta:      ackMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("carrier ack: %v", err)
	}

	carrier, ok, err := st.GetThreadItem("t1", carrierID)
	if err != nil || !ok {
		t.Fatalf("lookup carrier: ok=%v err=%v", ok, err)
	}
	want := "Agent: " + truncatePreview(longDescription, 80)
	if carrier.Summary != want {
		t.Fatalf("carrier summary = %q, want %q (bounded description fallback)", carrier.Summary, want)
	}
	if strings.Contains(carrier.Summary, "\n") {
		t.Fatalf("carrier summary contains a raw newline: %q", carrier.Summary)
	}
	if strings.Contains(carrier.Summary, longDescription) {
		t.Fatalf("carrier summary carries the unbounded description verbatim: %q", carrier.Summary)
	}
}

// TestBackgroundTaskNotification_ResumeCarrierReconnectResolvesByTaskID
// pins the reconnect edge: a parser restarted between the original
// launch and the resume stamps NO resumes_tool_use_id (the in-memory
// binding is gone), but the original launch's persisted meta.task_id is
// still on file — so the identity copy resolves it through
// FindOriginalAgentLaunchByTaskID and the carrier still gets the
// original's Summary, subagent_type and subagent_model. The lookup must
// skip the carrier's OWN row, which carries the same task_id by the
// time the flip runs (the rebind meta-update landed first).
func TestBackgroundTaskNotification_ResumeCarrierReconnectResolvesByTaskID(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	const (
		originalLaunchID = "original-agent-reconnect"
		carrierID        = "sendmessage-carrier-reconnect"
		taskID           = "task-reconnect-1"
	)

	// Round 1: original launch with full identity, persisted BEFORE the
	// "restart" — its rows are all the new parser instance has.
	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "Agent",
		"input": map[string]any{
			"description":   "Original round-1 input",
			"subagent_type": "Explore",
			"model":         "opus",
		},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: originalLaunchID,
		ItemType: "Agent", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("original launch start: %v", err)
	}
	taskStartedMeta, _ := json.Marshal(map[string]any{"task_id": taskID})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: originalLaunchID,
		Meta: taskStartedMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("original launch task_started: %v", err)
	}
	subnMeta, _ := json.Marshal(map[string]any{"subagent_model": "claude-opus-4-7"})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: originalLaunchID,
		Meta: subnMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("original launch subagent_model stamp: %v", err)
	}
	ackMeta, _ := json.Marshal(map[string]any{"is_background": true})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: originalLaunchID,
		Content: "Async agent launched successfully.", Meta: ackMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("original launch ack: %v", err)
	}

	// Round 2 after a parser restart: the rebind carries description +
	// subagent_type straight off the wire but NO resumes_tool_use_id.
	carrierStartMeta, _ := json.Marshal(map[string]any{
		"toolName": "SendMessage",
		"input":    map[string]any{"to": "a464e54e96a45cd0c", "summary": "resume"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: carrierID,
		ItemType: "SendMessage", Meta: carrierStartMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("carrier launch start: %v", err)
	}
	rebindMeta, _ := json.Marshal(map[string]any{
		"task_id":       taskID,
		"task_type":     "local_agent",
		"description":   "Original round-1 input",
		"subagent_type": "Explore",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: carrierID,
		Meta: rebindMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("carrier rebind meta-update: %v", err)
	}
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: "t1", ItemID: carrierID,
		Content: "resumed from transcript", Meta: ackMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("carrier ack: %v", err)
	}

	carrier, ok, err := st.GetThreadItem("t1", carrierID)
	if err != nil || !ok {
		t.Fatalf("lookup carrier: ok=%v err=%v", ok, err)
	}
	if !carrier.IsBackground || carrier.Status != statusRunning {
		t.Fatalf("carrier bg=%v status=%q, want background running", carrier.IsBackground, carrier.Status)
	}
	// Summary resolved through the task_id lookup — the original row's
	// own Summary, not the description fallback (identical text here
	// would unpin the branch, so the launch input description above is
	// what the original's Summary derives from and the assertion goes
	// through the row).
	original, ok, err := st.GetThreadItem("t1", originalLaunchID)
	if err != nil || !ok {
		t.Fatalf("lookup original: ok=%v err=%v", ok, err)
	}
	if carrier.Summary != original.Summary {
		t.Fatalf("carrier summary = %q, want the original launch's %q (task_id resolution)", carrier.Summary, original.Summary)
	}
	var carrierMeta map[string]any
	if err := json.Unmarshal([]byte(carrier.Meta), &carrierMeta); err != nil {
		t.Fatalf("carrier meta unmarshal: %v", err)
	}
	if carrierMeta["subagent_model"] != "claude-opus-4-7" {
		t.Fatalf("carrier meta.subagent_model = %v, want claude-opus-4-7 (copied via task_id lookup)", carrierMeta["subagent_model"])
	}
	if carrierMeta["subagent_type"] != "Explore" {
		t.Fatalf("carrier meta.subagent_type = %v, want Explore", carrierMeta["subagent_type"])
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
	notificationData, err := st.GetPayloadData(notification.ThreadID, notification.PayloadID)
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
	doneData, err := st.GetPayloadData(done.ThreadID, done.PayloadID)
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
	if commandMeta.Preview != "" {
		t.Fatalf("completion preview = %q, want empty", commandMeta.Preview)
	}
	if commandMeta.OutputState != "loaded" {
		t.Fatalf("completion output state = %q, want loaded", commandMeta.OutputState)
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

	initial, ok, err := st.GetThreadItem("t1", ToolCompletionID("bg-enrich-notify"))
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

	before := len(emissions.snapshot())
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

	after, ok, err := st.GetThreadItem("t1", ToolCompletionID("bg-enrich-notify"))
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

	upserts := findUpsertedItems(emissions.snapshot()[before:])
	var completionStates []string
	for _, item := range upserts {
		if item.ID != ToolCompletionID("bg-enrich-notify") {
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

	initial, ok, err := st.GetThreadItem("t1", nextTaskNotificationID("task-replay", ""))
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

	after, ok, err := st.GetThreadItem("t1", nextTaskNotificationID("task-replay", ""))
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

// startMonitorLaunch drives the launch half of Claude's Monitor watch
// task (claude-wire.md §E7): a backgrounded `local_bash` launch whose
// completion ack carries `watch_task`, which the keep-running flip
// copies onto the launch row's meta.
func startMonitorLaunch(t *testing.T, router *Router, threadID, itemID, taskID string) {
	t.Helper()
	// The Monitor launch itself carries no run_in_background — §E7's
	// launch ack is what marks it backgrounded, which is also what
	// carries watch_task through the keep-running flip.
	startMeta, _ := json.Marshal(map[string]any{
		"toolName": "Bash",
		"input":    map[string]any{"command": "Read output file for task b1"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  threadID,
		ItemID:    itemID,
		ItemType:  "Bash",
		Meta:      startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("monitor start: %v", err)
	}
	taskStartedMeta, _ := json.Marshal(map[string]any{"task_id": taskID})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolStart,
		ThreadID:  threadID,
		ItemID:    itemID,
		Meta:      taskStartedMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("monitor task_started meta-update: %v", err)
	}
	// The Monitor launch ack: a backgrounded placeholder completion
	// carrying watch_task, which the keep-running flip merges onto the
	// launch row.
	ackMeta, _ := json.Marshal(map[string]any{
		"is_background": true,
		"watch_task":    true,
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventToolComplete,
		ThreadID:  threadID,
		ItemID:    itemID,
		Meta:      ackMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("monitor launch ack: %v", err)
	}
}

func sendTaskNotification(t *testing.T, router *Router, threadID, itemID, taskID, uuid, summary string) {
	t.Helper()
	fields := map[string]any{
		"task_id":     taskID,
		"tool_use_id": itemID,
		"status":      "completed",
		"source":      "task_notification",
	}
	if uuid != "" {
		fields["uuid"] = uuid
	}
	meta, _ := json.Marshal(fields)
	if err := router.Handle(provider.ProviderEvent{
		Kind:      provider.EventBackgroundTaskNotification,
		ThreadID:  threadID,
		ItemID:    itemID,
		Meta:      meta,
		Content:   summary,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("task_notification %q: %v", uuid, err)
	}
}

// A persistent Monitor fires one system/task_notification per output
// event of the stream it watches. All of them share one task_id, and
// Claude sees each as its own message — so the row id has to be
// per-EVENT or every event silently overwrites the last (W1). The
// envelope's own uuid supplies that identity while keeping the id
// deterministic, which is what makes a replay an upsert rather than a
// third row.
func TestBackgroundTaskNotification_DistinctUUIDsProduceOneRowEachAndReplayIsIdempotent(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	startMonitorLaunch(t, router, "t1", "bg-monitor", "task-monitor")

	sendTaskNotification(t, router, "t1", "bg-monitor", "task-monitor", "uuid-1", "Monitor event 1")
	sendTaskNotification(t, router, "t1", "bg-monitor", "task-monitor", "uuid-2", "Monitor event 2")

	notifications := findItemsByKind(t, st, "t1", itemKindNotification)
	if len(notifications) != 2 {
		summaries := make([]string, 0, len(notifications))
		for _, n := range notifications {
			summaries = append(summaries, n.Summary)
		}
		t.Fatalf("expected 2 notification rows (one per uuid), got %d: %v", len(notifications), summaries)
	}
	if notifications[0].ID != nextTaskNotificationID("task-monitor", "uuid-1") {
		t.Fatalf("first notification id = %q", notifications[0].ID)
	}
	if notifications[1].ID != nextTaskNotificationID("task-monitor", "uuid-2") {
		t.Fatalf("second notification id = %q", notifications[1].ID)
	}

	// Reconnect replay of the FIRST envelope: same uuid, same row.
	sendTaskNotification(t, router, "t1", "bg-monitor", "task-monitor", "uuid-1", "Monitor event 1")
	replayed := findItemsByKind(t, st, "t1", itemKindNotification)
	if len(replayed) != 2 {
		t.Fatalf("replay of uuid-1 must upsert, got %d rows", len(replayed))
	}
}

// The legacy shape: an older CLI (and the claude-tui reconstruction)
// carries no envelope uuid. That must keep the pre-existing
// one-row-per-task upsert rather than degrading to an append.
func TestBackgroundTaskNotification_WithoutUUIDStillUpsertsOneRowPerTask(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	startMonitorLaunch(t, router, "t1", "bg-legacy", "task-legacy")

	sendTaskNotification(t, router, "t1", "bg-legacy", "task-legacy", "", "Legacy event 1")
	sendTaskNotification(t, router, "t1", "bg-legacy", "task-legacy", "", "Legacy event 2")

	notifications := findItemsByKind(t, st, "t1", itemKindNotification)
	if len(notifications) != 1 {
		t.Fatalf("expected 1 upserted notification row without a uuid, got %d", len(notifications))
	}
	if notifications[0].ID != nextTaskNotificationID("task-legacy", "") {
		t.Fatalf("legacy notification id = %q", notifications[0].ID)
	}
	if notifications[0].Summary != "Legacy event 2" {
		t.Fatalf("legacy notification summary = %q, want the latest", notifications[0].Summary)
	}
}

// The frontend's redundant-notification filter has to tell a watch
// task's event history from an ordinary task's single bell at RENDER
// time, when the launch row may be windowed out of the pane. Triage
// therefore copies the launch's watch marker onto every notification row
// it writes (W1b).
func TestBackgroundTaskNotification_StampsWatchTaskOntoNotificationMeta(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	startMonitorLaunch(t, router, "t1", "bg-monitor", "task-monitor")
	sendTaskNotification(t, router, "t1", "bg-monitor", "task-monitor", "uuid-1", "Monitor event 1")

	notifications := findItemsByKind(t, st, "t1", itemKindNotification)
	if len(notifications) != 1 {
		t.Fatalf("expected 1 notification row, got %d", len(notifications))
	}
	meta := decodeItemMetaMap(t, notifications[0].Meta)
	if meta["watch_task"] != true {
		t.Fatalf("notification meta watch_task = %v, want true (meta=%s)", meta["watch_task"], notifications[0].Meta)
	}
	if meta["uuid"] != "uuid-1" {
		t.Fatalf("notification meta uuid = %v, want uuid-1", meta["uuid"])
	}
}

// An ordinary background task is untouched: no watch marker, so the
// frontend keeps hiding its redundant bell.
func TestBackgroundTaskNotification_OrdinaryBackgroundTaskCarriesNoWatchMarker(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)

	startMeta, _ := json.Marshal(map[string]any{
		"toolName":      "Bash",
		"is_background": true,
		"input":         map[string]any{"command": "sleep 30"},
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-plain",
		ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	sendTaskNotification(t, router, "t1", "bg-plain", "task-plain", "uuid-1", "Background command completed (exit code 0)")

	notifications := findItemsByKind(t, st, "t1", itemKindNotification)
	if len(notifications) != 1 {
		t.Fatalf("expected 1 notification row, got %d", len(notifications))
	}
	if meta := decodeItemMetaMap(t, notifications[0].Meta); meta["watch_task"] != nil {
		t.Fatalf("ordinary background task must carry no watch marker, got %v", meta["watch_task"])
	}
}

// The hidden notification's summary is the only place the CLI's own
// wording and exit code live, so it is stamped onto the completion
// sibling that replaces it (W3). Both arrival orders must produce the
// same stamped meta.
func TestBackgroundTaskNotification_CompletionSiblingCarriesNotificationSummary(t *testing.T) {
	const summary = `Background command "sleep 1" completed (exit code 0)`

	startPlainBackgroundLaunch := func(t *testing.T, router *Router) {
		t.Helper()
		startMeta, _ := json.Marshal(map[string]any{
			"toolName":      "Bash",
			"is_background": true,
			"input":         map[string]any{"command": "sleep 1"},
		})
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-sum",
			ItemType: "Bash", Meta: startMeta, Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("start: %v", err)
		}
		taskStartedMeta, _ := json.Marshal(map[string]any{"task_id": "task-sum"})
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "bg-sum",
			Meta: taskStartedMeta, Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("task_started meta-update: %v", err)
		}
	}

	stashTerminal := func(t *testing.T, router *Router) {
		t.Helper()
		stashMeta, _ := json.Marshal(map[string]any{
			"task_id":     "task-sum",
			"tool_use_id": "bg-sum",
			"status":      "completed",
			"source":      "task_updated",
		})
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1",
			Meta: stashMeta, Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("task_updated stash: %v", err)
		}
	}

	observeViaTaskOutput := func(t *testing.T, router *Router) {
		t.Helper()
		observeMeta, _ := json.Marshal(map[string]any{
			"task_id":     "task-sum",
			"tool_use_id": "bg-sum",
			"status":      "completed",
			"source":      "task_output",
		})
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "bg-sum",
			Meta: observeMeta, Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("task_output observe: %v", err)
		}
	}

	assertStamped := func(t *testing.T, st *store.Store) {
		t.Helper()
		dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
		if len(dones) != 1 {
			t.Fatalf("expected 1 tool_completion sibling, got %d", len(dones))
		}
		meta := decodeItemMetaMap(t, dones[0].Meta)
		if got := meta["notification_summary"]; got != summary {
			t.Fatalf("sibling notification_summary = %v, want %q (meta=%s)", got, summary, dones[0].Meta)
		}
	}

	t.Run("sibling first (notification drains the stash)", func(t *testing.T) {
		router, st, _ := newTestRouter(t)
		createTestThread(t, st, "t1")
		seedOpenTurn(t, router, st, "t1", 0)
		startPlainBackgroundLaunch(t, router)
		stashTerminal(t, router)
		sendTaskNotification(t, router, "t1", "bg-sum", "task-sum", "uuid-1", summary)
		assertStamped(t, st)
	})

	t.Run("notification first (TaskOutput writes the sibling later)", func(t *testing.T) {
		router, st, _ := newTestRouter(t)
		createTestThread(t, st, "t1")
		seedOpenTurn(t, router, st, "t1", 0)
		startPlainBackgroundLaunch(t, router)
		sendTaskNotification(t, router, "t1", "bg-sum", "task-sum", "uuid-1", summary)
		if dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone); len(dones) != 0 {
			t.Fatalf("notification alone must not synthesize a sibling, got %d", len(dones))
		}
		observeViaTaskOutput(t, router)
		assertStamped(t, st)
	})

	assertNotStamped := func(t *testing.T, st *store.Store) {
		t.Helper()
		dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
		if len(dones) != 1 {
			t.Fatalf("expected 1 tool_completion sibling, got %d", len(dones))
		}
		if meta := decodeItemMetaMap(t, dones[0].Meta); meta["notification_summary"] != nil {
			t.Fatalf("sibling must not gain a caption after its first write, got %v (meta=%s)",
				meta["notification_summary"], dones[0].Meta)
		}
	}

	// The caption's ONE chance is the sibling's first write: a mounted
	// card must not grow a line (chat row-shell contract). When
	// TaskOutput created the sibling before the bell arrived, the enrich
	// path leaves the caption off and the notification ROW stays visible
	// instead (filterRedundantNotifications hides only absorbed bells).
	t.Run("sibling created before any notification never gains a late caption", func(t *testing.T) {
		router, st, _ := newTestRouter(t)
		createTestThread(t, st, "t1")
		seedOpenTurn(t, router, st, "t1", 0)
		startPlainBackgroundLaunch(t, router)
		observeViaTaskOutput(t, router)
		assertNotStamped(t, st)
		sendTaskNotification(t, router, "t1", "bg-sum", "task-sum", "uuid-1", summary)
		assertNotStamped(t, st)
		notifications := findItemsByKind(t, st, "t1", itemKindNotification)
		if len(notifications) != 1 {
			t.Fatalf("uncaptioned order must still persist the notification row, got %d", len(notifications))
		}
	})

	// Same veto through the drain path: a task_updated stash that
	// outlives a TaskOutput-written sibling reaches
	// writeBackgroundCompletionSibling with NotificationSummary set, and
	// the writer clears it because the sibling already exists.
	t.Run("stash surviving past an existing sibling cannot late-caption it", func(t *testing.T) {
		router, st, _ := newTestRouter(t)
		createTestThread(t, st, "t1")
		seedOpenTurn(t, router, st, "t1", 0)
		startPlainBackgroundLaunch(t, router)
		observeViaTaskOutput(t, router)
		stashTerminal(t, router)
		sendTaskNotification(t, router, "t1", "bg-sum", "task-sum", "uuid-1", summary)
		assertNotStamped(t, st)
	})
}

// A notification carrying an output_file never captions the sibling:
// the file's content becomes the sibling's own payload, and for those
// tasks (async agents, Task subagents) the notification "summary" is
// the agent's ENTIRE final report — a caption would dump the full
// report inline as a muted paragraph duplicating the expandable
// payload (2026-08-22, "Test nested agent spawning"). Both stamp
// orders carry the veto.
func TestOutputFileNotificationNeverCaptionsSibling(t *testing.T) {
	writeOutputFile := func(t *testing.T) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "agent.output")
		if err := os.WriteFile(path, []byte("Confirmed: the agent's full report text."), 0o600); err != nil {
			t.Fatalf("write output file: %v", err)
		}
		return path
	}

	startAgentLaunch := func(t *testing.T, router *Router) {
		t.Helper()
		startMeta, _ := json.Marshal(map[string]any{
			"toolName":      "Agent",
			"is_background": true,
			"input":         map[string]any{"description": "Test nested agent spawning"},
		})
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "agent-1",
			ItemType: "Agent", Meta: startMeta, Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("start: %v", err)
		}
		taskStartedMeta, _ := json.Marshal(map[string]any{"task_id": "task-agent"})
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventToolStart, ThreadID: "t1", ItemID: "agent-1",
			Meta: taskStartedMeta, Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("task_started meta-update: %v", err)
		}
	}

	sendOutputFileNotification := func(t *testing.T, router *Router, outputFile string) {
		t.Helper()
		fields := map[string]any{
			"task_id":     "task-agent",
			"tool_use_id": "agent-1",
			"status":      "completed",
			"source":      "task_notification",
			"uuid":        "uuid-agent",
			"output_file": outputFile,
		}
		meta, _ := json.Marshal(fields)
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventBackgroundTaskNotification, ThreadID: "t1",
			ItemID: "agent-1", Meta: meta,
			Content:   "Confirmed: the agent's full report text.",
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("task_notification: %v", err)
		}
	}

	assertUncaptionedWithPayload := func(t *testing.T, st *store.Store) {
		t.Helper()
		dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
		if len(dones) != 1 {
			t.Fatalf("expected 1 tool_completion sibling, got %d", len(dones))
		}
		meta := decodeItemMetaMap(t, dones[0].Meta)
		if meta["notification_summary"] != nil {
			t.Fatalf("output_file notification must not caption the sibling, got %v (meta=%s)",
				meta["notification_summary"], dones[0].Meta)
		}
		if dones[0].PayloadID == "" {
			t.Fatalf("sibling should carry the output-file payload instead (meta=%s)", dones[0].Meta)
		}
	}

	t.Run("sibling first (notification drains the stash)", func(t *testing.T) {
		router, st, _ := newTestRouter(t)
		createTestThread(t, st, "t1")
		seedOpenTurn(t, router, st, "t1", 0)
		startAgentLaunch(t, router)
		stashMeta, _ := json.Marshal(map[string]any{
			"task_id": "task-agent", "tool_use_id": "agent-1",
			"status": "completed", "source": "task_updated",
		})
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1",
			Meta: stashMeta, Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("task_updated stash: %v", err)
		}
		sendOutputFileNotification(t, router, writeOutputFile(t))
		assertUncaptionedWithPayload(t, st)
	})

	t.Run("notification first (TaskOutput writes the sibling later)", func(t *testing.T) {
		router, st, _ := newTestRouter(t)
		createTestThread(t, st, "t1")
		seedOpenTurn(t, router, st, "t1", 0)
		startAgentLaunch(t, router)
		sendOutputFileNotification(t, router, writeOutputFile(t))
		observeMeta, _ := json.Marshal(map[string]any{
			"task_id": "task-agent", "tool_use_id": "agent-1",
			"status": "completed", "source": "task_output",
		})
		if err := router.Handle(provider.ProviderEvent{
			Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1", ItemID: "agent-1",
			Meta: observeMeta, Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("task_output observe: %v", err)
		}
		assertUncaptionedWithPayload(t, st)
	})
}

// A watch task's notification rows are exempt from the redundant-bell
// hide (they ARE the event history), so a caption on its completion
// sibling would render the same text twice on adjacent rows. Neither
// stamp path may produce one.
func TestWatchTaskCompletionSiblingCarriesNoCaption(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	seedOpenTurn(t, router, st, "t1", 0)
	startMonitorLaunch(t, router, "t1", "mon-1", "task-mon")

	stashMeta, _ := json.Marshal(map[string]any{
		"task_id":     "task-mon",
		"tool_use_id": "mon-1",
		"status":      "completed",
		"source":      "task_updated",
	})
	if err := router.Handle(provider.ProviderEvent{
		Kind: provider.EventBackgroundTaskTerminal, ThreadID: "t1",
		Meta: stashMeta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("task_updated stash: %v", err)
	}
	sendTaskNotification(t, router, "t1", "mon-1", "task-mon", "uuid-mon", "Monitor: stream ended")

	dones := findItemsByKind(t, st, "t1", itemKindBackgroundDone)
	if len(dones) != 1 {
		t.Fatalf("expected 1 tool_completion sibling, got %d", len(dones))
	}
	if meta := decodeItemMetaMap(t, dones[0].Meta); meta["notification_summary"] != nil {
		t.Fatalf("watch sibling must carry no caption, got %v (meta=%s)",
			meta["notification_summary"], dones[0].Meta)
	}
}
