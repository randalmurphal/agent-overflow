package main

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

// --- Codex scenario 3: yielding command projects as backgrounded ---

// TestE2E_Codex_YieldingCommand_ProjectsAsBackgrounded covers the
// wire-typed Codex projection: an item/started for a unifiedExecStartup
// command, followed by a text_delta (the model yielded while the PTY is
// still running), stamps is_background=true on the launch row and
// emits provider:item_upsert with the flag set. When item/completed
// later arrives, the sibling tool_completion row lands at the latest
// turn's tail.
//
// The Codex wire path is driven by direct triage.Handle calls because
// the fake app-server harness only responds to requests — it can't
// push unsolicited notifications into the session. For the integration
// test we want to exercise the triage router end-to-end (projector +
// sibling synthesis + ListLiveBackgroundTasks) rather than the
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
			"command": "npm run server",
		},
		"item": map[string]any{
			"id":        "cmd-e2e",
			"type":      "commandExecution",
			"source":    "unifiedExecStartup",
			"status":    "inProgress",
			"processId": "pid-777",
			"command":   "npm run server",
		},
	})
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventToolStart, ThreadID: thread.ID, ItemID: "cmd-e2e",
		ItemType: "commandExecution", TurnID: "turn-0", Meta: startMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}

	// Sanity: the launch lands running, NOT backgrounded yet.
	launchBefore, found, err := app.store.GetThreadItem(thread.ID, "cmd-e2e")
	if err != nil || !found {
		t.Fatalf("launch lookup (pre-yield): found=%v err=%v", found, err)
	}
	if launchBefore.IsBackground {
		t.Fatal("launch IsBackground=true before any model yield — projector triggered too early")
	}

	// Model yield: the first text delta authorizes the projector to
	// stamp is_background=true and emit the upsert.
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: thread.ID,
		Content:   "letting the server keep running...",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}

	// The projector's stamp emission fires synchronously from the text-
	// delta handler; poll the store to confirm the row is updated.
	waitUntilE2E(t, 3*time.Second, "projector stamps is_background", func() bool {
		row, _, err := app.store.GetThreadItem(thread.ID, "cmd-e2e")
		return err == nil && row.IsBackground && row.Status == "running"
	})

	// Drain provider:item_upsert emissions off the bus looking for the
	// stamped row. The projector emits exactly once per stamp.
	sawStampEmission := false
	for _, e := range bus.allEvents() {
		if e.Name != "provider:item_upsert" {
			continue
		}
		item, ok := e.Data.(store.Item)
		if !ok || item.ID != "cmd-e2e" {
			continue
		}
		if item.IsBackground {
			sawStampEmission = true
			break
		}
	}
	if !sawStampEmission {
		t.Error("no provider:item_upsert with IsBackground=true emitted for cmd-e2e")
	}

	// Close the streaming text block so the completion sibling isn't
	// deferred behind it. We use EventContentBlockStop with text block
	// type — the same signal the streaming state machine watches.
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: thread.ID,
		Meta:      json.RawMessage(`{"blockType":"text"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("content block stop: %v", err)
	}

	// item/completed for the unifiedExec — arrives "eventually" after
	// the command exits. Triage's projector synthesizes a
	// tool_completion sibling at the latest turn's tail.
	completeMeta, _ := json.Marshal(map[string]any{
		"source":      "unifiedExecStartup",
		"item_status": "completed",
	})
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventToolComplete, ThreadID: thread.ID, ItemID: "cmd-e2e",
		ItemType: "commandExecution", TurnID: "turn-0", Meta: completeMeta,
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("tool complete: %v", err)
	}

	// The sibling tool_completion must land with is_background=true,
	// status=completed, and completion_of pointing at the launch.
	waitUntilE2E(t, 3*time.Second, "sibling tool_completion persisted", func() bool {
		items, _ := app.store.ListItems(thread.ID)
		for _, it := range items {
			if it.Kind == "tool_completion" && it.CompletionOf == "cmd-e2e" {
				return it.Status == "completed" && it.IsBackground
			}
		}
		return false
	})
}

// --- Codex scenario 4: stop-all → clean RPC + per-terminal siblings ---

// TestE2E_Codex_StopAll_CleanRPC drives the Codex Stop-all primitive.
// Two backgrounded Codex terminals on one thread, one
// CleanCodexBackgroundTerminals binding call that fires the
// thread/backgroundTerminals/clean RPC, then simulated item/completed
// events (one per terminated PTY) that triage synthesizes sibling rows
// for. Verifies exactly ONE RPC fired — thread-wide, not per-row.
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

	// Seed two backgrounded unifiedExec launches through the projector
	// (start + yield), mirroring what the real wire path produces.
	for _, id := range []string{"cmd-stop-1", "cmd-stop-2"} {
		startMeta, _ := json.Marshal(map[string]any{
			"source":      "unifiedExecStartup",
			"item_status": "inProgress",
			"process_id":  "pid-" + id,
			"toolName":    "command_execution",
			"input":       map[string]any{"command": "server for " + id},
			"item": map[string]any{
				"id":        id,
				"type":      "commandExecution",
				"source":    "unifiedExecStartup",
				"status":    "inProgress",
				"processId": "pid-" + id,
				"command":   "server for " + id,
			},
		})
		if err := app.triage.Handle(provider.ProviderEvent{
			Kind: provider.EventToolStart, ThreadID: thread.ID, ItemID: id,
			ItemType: "commandExecution", TurnID: "turn-0", Meta: startMeta,
			Timestamp: time.Now(),
		}); err != nil {
			t.Fatalf("tool start %s: %v", id, err)
		}
	}
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: thread.ID,
		Content: "both running in the background", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("text delta: %v", err)
	}
	// Close the text block so completion siblings aren't queued behind
	// an active streaming text block.
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: thread.ID,
		Meta:      json.RawMessage(`{"blockType":"text"}`),
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("content block stop: %v", err)
	}
	waitUntilE2E(t, 3*time.Second, "both launches backgrounded", func() bool {
		for _, id := range []string{"cmd-stop-1", "cmd-stop-2"} {
			row, found, err := app.store.GetThreadItem(thread.ID, id)
			if err != nil || !found {
				return false
			}
			if !row.IsBackground {
				return false
			}
		}
		return true
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
	app.mu.Lock()
	app.sessions[thread.ID] = session{
		provider: string(provider.Codex),
		codex:    fakeSess,
	}
	app.mu.Unlock()

	if err := app.CleanCodexBackgroundTerminals(thread.ID); err != nil {
		t.Fatalf("CleanCodexBackgroundTerminals: %v", err)
	}
	if got := cleanCalls.Load(); got != 1 {
		t.Fatalf("CleanBackgroundTerminals calls = %d, want 1 (thread-wide primitive)", got)
	}

	// The app-server responds by emitting item/completed for each
	// terminated PTY. Triage synthesizes one tool_completion sibling
	// per terminated command. A real app-server would also deliver a
	// failed or completed item_status — we use completed here for the
	// clean-shutdown case.
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

	waitUntilE2E(t, 3*time.Second, "two sibling completions", func() bool {
		items, _ := app.store.ListItems(thread.ID)
		siblings := 0
		for _, it := range items {
			if it.Kind == "tool_completion" && (it.CompletionOf == "cmd-stop-1" || it.CompletionOf == "cmd-stop-2") {
				siblings++
			}
		}
		return siblings == 2
	})

	// Drain bus so the test cleanup doesn't trip on an oversized channel.
	_ = bus.allEvents()

	// The fake session isn't a real codex.Session with a live proc —
	// StopSession would try to close a nil Process. Drop it from the
	// map directly so setupE2EApp's cleanup loop has nothing to close.
	app.mu.Lock()
	delete(app.sessions, thread.ID)
	app.mu.Unlock()
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
// The test exercises the on-start flip directly (flipCodexGhostBackgroundRowsOnStart)
// — that's the same helper startSessionNow calls BEFORE spawning the
// new subprocess. The rest of the reconcile path has unit coverage in
// app_codex_reconcile_test.go; this test pins the end-to-end visible
// state: ghost row → errored/lost, emitted via provider:item_upsert so
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
		if name != "provider:item_upsert" {
			return
		}
		env, ok := data.(SeqEnvelope)
		if !ok {
			return
		}
		item, ok := env.Data.(store.Item)
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
	seedBackgroundLaunchRowE2E(t, app.store, thread.ID, "tool-ghost", "command_execution", "npm run watch")

	// Fire the pre-spawn flip — the same helper startSessionNow calls
	// on every Codex session start. It's intentionally unconditional
	// (see app_codex_reconcile.go rationale).
	app.flipCodexGhostBackgroundRowsOnStart(thread.ID)

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

	// The flip must emit provider:item_upsert with the new state so
	// the tray re-renders without waiting on a provider event.
	var sawFlipEmission bool
	for _, emit := range flipEmits {
		if emit.item.ID == "tool-ghost" && emit.item.Status == "errored" && emit.item.Decision == "lost" {
			sawFlipEmission = true
			break
		}
	}
	if !sawFlipEmission {
		t.Errorf("no provider:item_upsert with the errored ghost row emitted; got %d emissions", len(flipEmits))
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
			app.mu.Lock()
			app.sessions[thread.ID] = session{
				provider: string(provider.Claude),
				token:    "interrupt-bg-" + pc.name,
				claude:   sess,
			}
			app.mu.Unlock()

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
	app.mu.Lock()
	app.sessions[thread.ID] = session{
		provider: string(provider.Codex),
		token:    "delete-clean-token",
		codex:    fakeSess,
	}
	app.mu.Unlock()

	// stopSessionFn fires inside deleteThreadTree after the clean RPC.
	// Record its invocation so we can confirm the ordering.
	app.stopSessionFn = func(threadID string) error {
		order = append(order, "stopSession")
		app.mu.Lock()
		delete(app.sessions, threadID)
		app.mu.Unlock()
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
