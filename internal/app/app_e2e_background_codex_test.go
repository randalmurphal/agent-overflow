package app

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
)

func decodeE2EItemMeta(t *testing.T, raw string) map[string]any {
	t.Helper()
	var meta map[string]any
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		t.Fatalf("decode item meta: %v", err)
	}
	return meta
}

func codexE2EExecResultMeta(t *testing.T, result, processID, command string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"result":     result,
		"process_id": processID,
		"command":    command,
	})
	if err != nil {
		t.Fatalf("marshal codex exec result meta: %v", err)
	}
	return raw
}

// --- Codex scenario 3: yielding command projects as backgrounded ---

// TestE2E_Codex_YieldingCommand_ProjectsAsBackgrounded covers the
// wire-typed Codex projection: an item/started for a unifiedExecStartup
// command is tracked transiently without creating a transcript row. It
// appears in the running tray immediately, then becomes a background
// task after a typed wait/yield signal marks the PTY backgrounded. When
// the command completes, typed item/completed persists the command row
// itself and clears the live tray. TerminalInteractionNotification owns only
// separate waited/interacted marker rows while the PTY is still tracked.
//
// The Codex wire path is driven by direct triage.Handle calls because
// the fake app-server harness only responds to requests — it can't
// push unsolicited notifications into the session. For the integration
// test we want to exercise the triage router end-to-end (projector +
// transient tray state + ListLiveBackgroundTasks) rather than the
// notification parser, which has its own unit tests.
func TestE2E_Codex_YieldingCommand_ProjectsAsBackgrounded(t *testing.T) {
	app, bus := setupE2EApp(t)

	workspace := t.TempDir()
	thread, err := createTestThread(t, app, string(provider.Codex), workspace, "gpt-5", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	// Open a turn so the launch and completion rows have the same
	// write-head context the real turn lifecycle would provide.
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: thread.ID, TurnID: "turn-0",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	// item/started with unifiedExecStartup source — the sanctioned
	// wire-typed signal (invariant 25). Source, item_status, and
	// processId all live top-level on Meta after the parser's
	// enrichItemMeta pass.
	startMeta, _ := json.Marshal(map[string]any{
		"source":      "unifiedExecStartup",
		"item_status": "inProgress",
		"process_id":  "pid-777",
		"toolName":    "command_execution",
		"input": map[string]any{
			"command": "pnpm run server",
		},
		"item": map[string]any{
			"id":        "cmd-e2e",
			"type":      "commandExecution",
			"source":    "unifiedExecStartup",
			"status":    "inProgress",
			"processId": "pid-777",
			"command":   "pnpm run server",
		},
	})
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: thread.ID, ItemID: "cmd-e2e",
		ItemType: "commandExecution", TurnID: "turn-0", Meta: startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}

	// Sanity: Codex unified exec starts are transcript-free, but still
	// visible in the running tray before the model yields. They are not
	// background tasks yet, so IsBackground stays false.
	if _, found, err := app.store.GetThreadItem(thread.ID, "cmd-e2e"); err != nil || found {
		t.Fatalf("unified exec start should be tray-only: found=%v err=%v", found, err)
	}
	liveBefore, err := app.ListLiveBackgroundTasks(thread.ID)
	if err != nil {
		t.Fatalf("ListLiveBackgroundTasks pre-yield: %v", err)
	}
	if len(liveBefore) != 1 || liveBefore[0].ID != "cmd-e2e" || liveBefore[0].IsBackground {
		t.Fatalf("unexpected pre-yield tray state: %+v", liveBefore)
	}
	// The tray row must carry the PTY process id all the way to the
	// binding's caller: it is the only handle the frontend has for
	// TerminateCodexBackgroundTerminal, and the row would render a Stop
	// button with nothing to target without it.
	if got := decodeE2EItemMeta(t, liveBefore[0].Meta)["process_id"]; got != "pid-777" {
		t.Fatalf("tray row meta process_id = %v, want pid-777", got)
	}

	// Model-visible yield: Codex returns "Process running with session ID"
	// to the model from the original exec_command call. Text/reasoning
	// deltas are not enough to classify the PTY as backgrounded because a
	// foreground command may also be followed by model output.
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventCodexExecResult, ThreadID: thread.ID, ItemID: "cmd-e2e",
		TurnID:    "turn-0",
		Meta:      codexE2EExecResultMeta(t, "running", "pid-777", "pnpm run server"),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("exec running result: %v", err)
	}

	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: thread.ID,
		Content:   "letting the server keep running...",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}

	waitUntilE2E(t, 3*time.Second, "projector keeps command in live tray", func() bool {
		live, err := app.ListLiveBackgroundTasks(thread.ID)
		return err == nil && len(live) == 1 && live[0].ID == "cmd-e2e" && live[0].Status == "running"
	})

	// The tray refresh is driven by a dedicated live-state event rather
	// than a provider:item_event upsert, because no timeline row exists.
	sawTrayRefreshEmission := false
	for _, e := range bus.allEvents() {
		if e.Name == "provider:background_tasks_changed" {
			sawTrayRefreshEmission = true
			break
		}
	}
	if !sawTrayRefreshEmission {
		t.Error("no provider:background_tasks_changed emitted for cmd-e2e")
	}

	// Close the streaming text block so the command row isn't deferred
	// behind it. We use EventContentBlockStop with text block type — the
	// same signal the streaming state machine watches.
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: thread.ID,
		Meta:      json.RawMessage(`{"blockType":"text"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("content block stop: %v", err)
	}

	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventCommandOutput, ThreadID: thread.ID, ItemID: "cmd-e2e",
		ItemType: "commandExecution", TurnID: "turn-0", Content: "server ready\n",
		Replace: true, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("command output: %v", err)
	}

	// item/completed for the unifiedExec arrives "eventually" after the
	// command exits. This is the Codex TUI history source for command rows.
	completeMeta, _ := json.Marshal(map[string]any{
		"source":      "unifiedExecStartup",
		"item_status": "completed",
		"process_id":  "pid-777",
		"command":     "pnpm run server",
	})
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: thread.ID, ItemID: "cmd-e2e",
		ItemType: "commandExecution", TurnID: "turn-0", Meta: completeMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool complete: %v", err)
	}

	waitUntilE2E(t, 3*time.Second, "completed command persisted and tray cleared", func() bool {
		live, err := app.ListLiveBackgroundTasks(thread.ID)
		if err != nil || len(live) != 0 {
			return false
		}
		row, found, err := app.store.GetThreadItem(thread.ID, "cmd-e2e")
		return err == nil && found && row.Status == "completed" && row.PayloadKind == "command_output"
	})

	if siblings := findItemsByKindE2E(t, app.store, thread.ID, "tool_completion"); len(siblings) != 0 {
		t.Fatalf("Codex command completion should not create transcript sibling rows: %+v", siblings)
	}

	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventTerminalInteraction, ThreadID: thread.ID,
		Meta:      json.RawMessage(`{"process_id":"pid-777","stdin":""}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("terminal interaction: %v", err)
	}
	waits := findItemsByKindE2E(t, app.store, thread.ID, string(provider.ItemTerminalInteraction))
	if len(waits) != 0 {
		t.Fatalf("post-completion poll should not create detached waits: %+v", waits)
	}
	completion, found, err := app.store.GetThreadItem(thread.ID, "cmd-e2e")
	if err != nil || !found {
		t.Fatalf("command completion missing: found=%v err=%v", found, err)
	}
	completionMeta := decodeE2EItemMeta(t, completion.Meta)
	if _, ok := completionMeta["wait_carrier_id"]; ok {
		t.Fatalf("completion wait_carrier_id = %v, want original command row without wait carrier", completionMeta["wait_carrier_id"])
	}
	if completion.PayloadKind != "command_output" {
		t.Fatalf("completion payload kind = %q, want command_output", completion.PayloadKind)
	}
	data, err := app.store.GetPayloadData(completion.ThreadID, completion.PayloadID)
	if err != nil {
		t.Fatalf("completion payload: %v", err)
	}
	if string(data) != "server ready\n" {
		t.Fatalf("completion payload = %q, want server ready newline", string(data))
	}
}

// --- Codex scenario 4: stop-all → clean RPC + per-terminal command rows ---

// TestE2E_Codex_StopAll_CleanRPC drives the Codex Stop-all primitive.
// Two live Codex terminals on one thread, one
// CleanCodexBackgroundTerminals binding call that fires the
// thread/backgroundTerminals/clean RPC, then simulated item/completed
// events (one per terminated PTY) that persist command rows and clear
// the live tray. Verifies
// exactly ONE RPC fired — thread-wide, not per-row.
func TestE2E_Codex_StopAll_CleanRPC(t *testing.T) {
	app, bus := setupE2EApp(t)

	workspace := t.TempDir()
	thread, err := createTestThread(t, app, string(provider.Codex), workspace, "gpt-5", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: thread.ID, TurnID: "turn-0",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	// Seed two live unifiedExec launches through the projector. The raw
	// running result enriches live state only; typed item/completed below owns
	// transcript history.
	for _, id := range []string{"cmd-stop-1", "cmd-stop-2"} {
		command := "server for " + id
		startMeta, _ := json.Marshal(map[string]any{
			"source":      "unifiedExecStartup",
			"item_status": "inProgress",
			"process_id":  "pid-" + id,
			"toolName":    "command_execution",
			"input":       map[string]any{"command": command},
			"item": map[string]any{
				"id":        id,
				"type":      "commandExecution",
				"source":    "unifiedExecStartup",
				"status":    "inProgress",
				"processId": "pid-" + id,
				"command":   command,
			},
		})
		if err := app.triage.Handle(provider.ProviderEvent{
			Kind: provider.EventToolStart, ThreadID: thread.ID, ItemID: id,
			ItemType: "commandExecution", TurnID: "turn-0", Meta: startMeta,
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("tool start %s: %v", id, err)
		}
		if err := app.triage.Handle(provider.ProviderEvent{
			Kind: provider.EventCodexExecResult, ThreadID: thread.ID, ItemID: id,
			TurnID:    "turn-0",
			Meta:      codexE2EExecResultMeta(t, "running", "pid-"+id, command),
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("exec running result %s: %v", id, err)
		}
	}
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: thread.ID,
		Content: "both running in the background", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}
	// Close the text block so command rows aren't queued behind an
	// active streaming text block.
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: thread.ID,
		Meta:      json.RawMessage(`{"blockType":"text"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("content block stop: %v", err)
	}
	waitUntilE2E(t, 3*time.Second, "both launches in live tray", func() bool {
		live, err := app.ListLiveBackgroundTasks(thread.ID)
		return err == nil && len(live) == 2
	})

	// Install a fake Codex session whose Clean callback records the
	// call and returns nil. The binding must see it go through exactly
	// once — the frontend Stop-all dispatches this ONCE per thread
	// (not per row) because Codex's primitive is thread-wide.
	var cleanCalls atomic.Int32
	fakeSess := codex.NewCleanBackgroundTerminalsTestSession(func(ctx context.Context) error {
		cleanCalls.Add(1)
		if ctx == nil {
			t.Error("nil context passed to CleanBackgroundTerminals")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Error("CleanBackgroundTerminals context missing deadline")
		}
		return nil
	})
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Codex:    fakeSess,
	})
	t.Cleanup(func() {
		app.sessionManager().take(thread.ID)
	})

	if err := app.CleanCodexBackgroundTerminals(thread.ID); err != nil {
		t.Fatalf("CleanCodexBackgroundTerminals: %v", err)
	}
	if got := cleanCalls.Load(); got != 1 {
		t.Fatalf("CleanBackgroundTerminals calls = %d, want 1 (thread-wide primitive)", got)
	}

	// The app-server responds by emitting item/completed for each
	// terminated PTY. Triage persists each command row and clears each
	// live tracker. A real app-server would also deliver a failed or completed
	// item_status — we use completed here for the clean-shutdown case.
	for _, id := range []string{"cmd-stop-1", "cmd-stop-2"} {
		completeMeta, _ := json.Marshal(map[string]any{
			"source":      "unifiedExecStartup",
			"item_status": "completed",
		})
		if err := app.triage.Handle(provider.ProviderEvent{
			Kind: provider.EventToolComplete, ThreadID: thread.ID, ItemID: id,
			ItemType: "commandExecution", TurnID: "turn-0", Meta: completeMeta,
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("tool complete %s: %v", id, err)
		}
	}

	waitUntilE2E(t, 3*time.Second, "two command rows and empty live tray", func() bool {
		live, err := app.ListLiveBackgroundTasks(thread.ID)
		if err != nil || len(live) != 0 {
			return false
		}
		for _, id := range []string{"cmd-stop-1", "cmd-stop-2"} {
			row, found, err := app.store.GetThreadItem(thread.ID, id)
			if err != nil || !found || row.Status != "completed" {
				return false
			}
		}
		return true
	})

	// Drain bus so the test cleanup doesn't trip on an oversized channel.
	_ = bus.allEvents()

	// The fake session isn't a real codex.Session with a live proc —
	// StopSession would try to close a nil Process. Drop it from the
	// map directly so setupE2EApp's cleanup loop has nothing to close.
	app.sessionManager().take(thread.ID)
}

// TestE2E_Codex_PerRowStop_TerminateRPC is the per-row counterpart of the
// Stop-all test above, and it closes the loop the binding unit tests
// cannot: the process id the tray PUBLISHES is the id the terminate RPC
// RECEIVES. The test never fabricates that id — it reads it back off
// ListLiveBackgroundTasks the way the frontend does, so a break anywhere
// in the chain (parser enrichment → tracker → the tray meta allowlist)
// fails here rather than shipping a Stop button that kills nothing.
//
// It also pins the "per-row" half: two live terminals, one stop, and the
// untouched one must still be running afterwards.
func TestE2E_Codex_PerRowStop_TerminateRPC(t *testing.T) {
	app, bus := setupE2EApp(t)

	workspace := t.TempDir()
	thread, err := createTestThread(t, app, string(provider.Codex), workspace, "gpt-5", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: thread.ID, TurnID: "turn-0",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("turn start: %v", err)
	}

	const stopID, keepID = "cmd-row-stop", "cmd-row-keep"
	for _, id := range []string{stopID, keepID} {
		command := "server for " + id
		startMeta, _ := json.Marshal(map[string]any{
			"source":      "unifiedExecStartup",
			"item_status": "inProgress",
			"process_id":  "pid-" + id,
			"toolName":    "command_execution",
			"input":       map[string]any{"command": command},
			"item": map[string]any{
				"id":        id,
				"type":      "commandExecution",
				"source":    "unifiedExecStartup",
				"status":    "inProgress",
				"processId": "pid-" + id,
				"command":   command,
			},
		})
		if err := app.triage.Handle(provider.ProviderEvent{
			Kind: provider.EventToolStart, ThreadID: thread.ID, ItemID: id,
			ItemType: "commandExecution", TurnID: "turn-0", Meta: startMeta,
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("tool start %s: %v", id, err)
		}
		if err := app.triage.Handle(provider.ProviderEvent{
			Kind: provider.EventCodexExecResult, ThreadID: thread.ID, ItemID: id,
			TurnID:    "turn-0",
			Meta:      codexE2EExecResultMeta(t, "running", "pid-"+id, command),
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("exec running result %s: %v", id, err)
		}
	}
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: thread.ID,
		Meta:      json.RawMessage(`{"blockType":"text"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("content block stop: %v", err)
	}
	waitUntilE2E(t, 3*time.Second, "both launches in live tray", func() bool {
		live, err := app.ListLiveBackgroundTasks(thread.ID)
		return err == nil && len(live) == 2
	})

	// Resolve the target exactly as the tray row does: read process_id
	// out of the item meta the binding handed back.
	live, err := app.ListLiveBackgroundTasks(thread.ID)
	if err != nil {
		t.Fatalf("ListLiveBackgroundTasks: %v", err)
	}
	target := ""
	for _, item := range live {
		if item.ID == stopID {
			id, _ := decodeE2EItemMeta(t, item.Meta)["process_id"].(string)
			target = id
		}
	}
	if target == "" {
		t.Fatalf("tray row %s published no process_id; the Stop button would have no target", stopID)
	}

	var terminateCalls atomic.Int32
	var gotProcessID atomic.Value
	fakeSess := codex.NewTerminateBackgroundTerminalTestSession(
		func(ctx context.Context, processID string) (bool, error) {
			terminateCalls.Add(1)
			if ctx == nil {
				t.Error("nil context passed to TerminateBackgroundTerminal")
			} else if _, ok := ctx.Deadline(); !ok {
				t.Error("TerminateBackgroundTerminal context missing deadline")
			}
			gotProcessID.Store(processID)
			return true, nil
		})
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Codex:    fakeSess,
	})

	terminated, err := app.TerminateCodexBackgroundTerminal(thread.ID, target)
	if err != nil {
		t.Fatalf("TerminateCodexBackgroundTerminal: %v", err)
	}
	if !terminated {
		t.Fatal("terminated = false, want the session's true passed through")
	}
	if got := terminateCalls.Load(); got != 1 {
		t.Fatalf("TerminateBackgroundTerminal calls = %d, want exactly 1", got)
	}
	if got, _ := gotProcessID.Load().(string); got != target {
		t.Fatalf("RPC received process id %q, want the tray's %q", got, target)
	}

	// The app-server answers a real termination with item/completed for
	// that PTY only. The stopped row leaves the tray and persists; the
	// untouched one keeps running.
	completeMeta, _ := json.Marshal(map[string]any{
		"source":      "unifiedExecStartup",
		"item_status": "completed",
	})
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: thread.ID, ItemID: stopID,
		ItemType: "commandExecution", TurnID: "turn-0", Meta: completeMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool complete %s: %v", stopID, err)
	}

	waitUntilE2E(t, 3*time.Second, "stopped row cleared, sibling still running", func() bool {
		remaining, err := app.ListLiveBackgroundTasks(thread.ID)
		if err != nil || len(remaining) != 1 || remaining[0].ID != keepID {
			return false
		}
		row, found, err := app.store.GetThreadItem(thread.ID, stopID)
		return err == nil && found && row.Status == "completed"
	})

	_ = bus.allEvents()

	app.sessionManager().take(thread.ID)
}

// --- Codex scenario 5: app restart → ghost rows flipped to errored ---

// TestE2E_Codex_AppRestart_GhostRowsFlipped pins the Phase-4 pre-spawn
// ghost flip: a persisted is_background=true + status=running tool_call
// for a Codex thread from a prior session must flip to errored/lost on
// the next session start, BEFORE any wire event can re-upsert it. This
// is the "app-restart-with-dead-PTY" contract — we can't recover those
// PTYs in a fresh subprocess, so the row must reflect that before the
// user sends the next turn.
//
// The test exercises the on-start retirement directly (retireCodexBackgroundRuntime)
// — that's the same helper startSessionNow calls BEFORE spawning the
// new subprocess. The rest of the reconcile path has unit coverage in
// app_codex_reconcile_test.go; this test pins the end-to-end visible
// state: ghost row → errored/lost, emitted via provider:item_event so
// the tray reconciles.
func TestE2E_Codex_AppRestart_GhostRowsFlipped(t *testing.T) {
	app, _ := setupE2EApp(t)

	// The ghost flip uses app.emit (not the triage emit function), so
	// the lifecycle bus's triage hook doesn't see it. Install a
	// testEmitHook so we can assert the frontend-facing emission
	// directly — same pattern app_codex_reconcile_test.go uses for
	// the parallel Phase-4 unit tests.
	type capturedFlipEmit struct {
		name string
		item store.Item
	}
	var flipEmits []capturedFlipEmit
	app.testEmitHook = func(name string, data any) {
		if name != "provider:item_event" {
			return
		}
		item, ok := itemFromItemStreamEvent(data)
		if !ok {
			return
		}
		flipEmits = append(flipEmits, capturedFlipEmit{name: name, item: item})
	}

	workspace := t.TempDir()
	thread, err := createTestThread(t, app, string(provider.Codex), workspace, "gpt-5", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	// Seed a persisted ghost row: the kind of state a Codex session
	// leaves behind when the subprocess dies with a PTY still running.
	// On the next app run this row must flip BEFORE any replay can
	// re-upsert it as running — otherwise it would render as
	// live-forever in the tray.
	seedBackgroundLaunchRowE2E(t, app.store, thread.ID, "tool-ghost", "command_execution", "pnpm run watch")

	// Fire the pre-spawn retirement — the same helper startSessionNow calls
	// on every Codex session start. It's intentionally unconditional
	// (see internal/codexthread rationale).
	if err := app.retireCodexBackgroundRuntime(thread.ID); err != nil {
		t.Fatalf("retire Codex background runtime: %v", err)
	}

	// The ghost row must now be errored/lost with the " — session
	// ended" summary suffix.
	got, found, err := app.store.GetThreadItem(thread.ID, "tool-ghost")
	if err != nil || !found {
		t.Fatalf("ghost row lookup post-flip: found=%v err=%v", found, err)
	}
	if got.Status != "errored" {
		t.Fatalf("ghost row status = %q, want errored", got.Status)
	}
	if got.Decision != "lost" {
		t.Fatalf("ghost row decision = %q, want lost", got.Decision)
	}
	if !strings.HasSuffix(got.Summary, " — session ended") {
		t.Fatalf("ghost row summary = %q, want ' — session ended' suffix", got.Summary)
	}

	// The flip must emit provider:item_event with the new state so
	// the tray re-renders without waiting on a provider event.
	var sawFlipEmission bool
	for _, emit := range flipEmits {
		if emit.item.ID == "tool-ghost" && emit.item.Status == "errored" && emit.item.Decision == "lost" {
			sawFlipEmission = true
			break
		}
	}
	if !sawFlipEmission {
		t.Errorf("no provider:item_event upsert with the errored ghost row emitted; got %d emissions", len(flipEmits))
	}
}

// --- Cross-phase sanity 6: interrupt does not kill background ---

// TestE2E_InterruptDoesNotKillBackground pins the interrupt exemption
// for BOTH providers: when a user interrupts the active turn, any
// backgrounded tool_call rows must stay running. The frontend's
// interrupt button is a turn-scoped cancel — it stops the model, NOT
// the backgrounded PTYs / subagents. Those have their own stop
// primitives (StopClaudeTask / CleanCodexBackgroundTerminals).
//
// Structure follows the existing TestInterrupt_LeavesBackgroundTasksRunning
// coverage in app_send_test.go but routes through the full E2E harness
// (real triage + store) so a regression in triage.flipTurnItemsErrored's
// IsBackground guard surfaces here, not just in the unit test.
func TestE2E_InterruptDoesNotKillBackground(t *testing.T) {
	providers := []struct {
		name         string
		providerName string
	}{
		{"claude", string(provider.Claude)},
		{"codex", string(provider.Codex)},
	}
	for _, pc := range providers {
		t.Run(pc.name, func(t *testing.T) {
			app, _ := setupE2EApp(t)
			workspace := t.TempDir()
			thread, err := createTestThread(t, app, pc.providerName, workspace, "m", "chat")
			if err != nil {
				t.Fatalf("CreateThread: %v", err)
			}

			// Open a turn so the interrupt path has a current turn to
			// operate on. Seed a backgrounded tool_call row inside that
			// turn — the shape MarkUserInterrupt would otherwise flip
			// to errored absent the is_background guard.
			if err := app.triage.Handle(provider.ProviderEvent{
				Kind: provider.EventTurnStart, ThreadID: thread.ID, TurnID: "turn-0",
				Timestamp: time.Now(),
			}); err != nil {
				t.Fatalf("turn start: %v", err)
			}
			seedBackgroundLaunchRowE2E(t, app.store, thread.ID, "tool-bg-interrupt", "Bash", "long-running: sleep 3600")

			// Install a passthrough Claude session so InterruptTurn has
			// something to call Interrupt on without needing a real
			// Codex subprocess. The cross-provider exemption lives in
			// triage.flipTurnItemsErrored (provider-agnostic), so using
			// a Claude session for the Codex branch is fine — the Codex
			// thread.Provider column drives the projector, not the
			// session-type.
			passthroughBinary := writeSilentClaudeBinary(t)
			sess, err := claude.NewSession(
				context.Background(),
				thread.ID,
				claude.Config{Binary: passthroughBinary, WorkDir: workspace},
				func(provider.ProviderEvent) {},
			)
			if err != nil {
				t.Fatalf("NewSession: %v", err)
			}
			t.Cleanup(func() { _ = sess.Close() })
			app.sessionManager().put(thread.ID, session{
				Provider: string(provider.Claude),
				Token:    "interrupt-bg-" + pc.name,
				Claude:   sess,
			})

			if err := app.InterruptTurn(thread.ID); err != nil {
				t.Fatalf("InterruptTurn: %v", err)
			}

			// The backgrounded row must be untouched: still running,
			// no errored-suffix summary rewrite, no decision stamp.
			after, found, err := app.store.GetThreadItem(thread.ID, "tool-bg-interrupt")
			if err != nil || !found {
				t.Fatalf("GetThreadItem: found=%v err=%v", found, err)
			}
			if after.Status != "running" {
				t.Errorf("%s: bg row status = %q, want running", pc.name, after.Status)
			}
			if after.Summary != "long-running: sleep 3600" {
				t.Errorf("%s: bg row summary rewritten: %q", pc.name, after.Summary)
			}
			if after.Decision != "" {
				t.Errorf("%s: bg row decision stamped: %q", pc.name, after.Decision)
			}

			_ = app.StopSession(thread.ID)
		})
	}
}

// --- Cross-phase sanity 7: thread delete cleans Codex background ---

// TestE2E_ThreadDelete_CleansCodexBackground pins the Phase-4 delete-
// time cleanup ordering: `thread/backgroundTerminals/clean` must fire
// BEFORE the session close because the RPC needs a live JSON-RPC
// transport. Once stopSession closes the subprocess, the wire is gone
// and any remaining PTYs would leak. The app.go delete path enforces
// this ordering; the test pins it end-to-end against a live thread +
// fake session.
func TestE2E_ThreadDelete_CleansCodexBackground(t *testing.T) {
	app, _ := setupE2EApp(t)

	workspace := t.TempDir()
	thread, err := createTestThread(t, app, string(provider.Codex), workspace, "gpt-5", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	// Seed a backgrounded launch so the ghost-flip path would also
	// have work to do — confirming that the pipeline doesn't short-
	// circuit away clean+delete when there are pre-existing rows.
	seedBackgroundLaunchRowE2E(t, app.store, thread.ID, "tool-bg-delete", "command_execution", "long-running command")

	// Track call ordering: clean must run BEFORE the session closes
	// (closeProviderSession) because once the subprocess is gone the
	// RPC has no transport.
	var order []string
	var cleanCalls atomic.Int32
	fakeSess := codex.NewCleanBackgroundTerminalsTestSession(func(ctx context.Context) error {
		cleanCalls.Add(1)
		order = append(order, "clean")
		if ctx == nil {
			t.Error("clean called with nil ctx")
		}
		return nil
	})
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Codex),
		Token:    "delete-clean-token",
		Codex:    fakeSess,
	})

	// stopSessionFn fires inside deleteThreadTree after the clean RPC.
	// Record its invocation so we can confirm the ordering.
	app.stopSessionFn = func(threadID string) error {
		order = append(order, "stopSession")
		app.sessionManager().take(threadID)
		return nil
	}

	if err := app.DeleteThread(thread.ID); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}

	if got := cleanCalls.Load(); got != 1 {
		t.Fatalf("CleanBackgroundTerminals calls = %d, want 1", got)
	}
	if len(order) < 2 || order[0] != "clean" || order[1] != "stopSession" {
		t.Fatalf("call order = %v, want [clean, stopSession]", order)
	}

	// Thread row must be gone. A DeleteThread success deletes the
	// thread + all its items via FK CASCADE.
	if _, err := app.store.GetThread(thread.ID); err == nil {
		t.Fatal("thread row should be gone after DeleteThread")
	}
	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems post-delete: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected 0 items after thread delete, got %d", len(items))
	}
}
