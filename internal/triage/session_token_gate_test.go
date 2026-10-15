package triage

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
)

func fromSession(evt provider.ProviderEvent, token string) provider.ProviderEvent {
	evt.SessionToken = token
	return evt
}

func compactingOpen(r *Router, threadID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.threadStateIfPresent(threadID)
	return st != nil && st.compactingSinceSet
}

// Handle admits a provider frame only from the session MarkThreadActive
// last committed the thread to. A frame of an earlier session stays
// refused across every later transition, including a stop and a new
// start, because tokens are never reused.
func TestHandleAdmitsOnlyTheCommittedSessionsFrames(t *testing.T) {
	t.Parallel()
	r := NewRouter(nil, func(eventchan.Channel, any) {})
	const threadID = "thread-session-gate"
	fireAt := time.Now().Add(30 * time.Minute).UnixMilli()
	job := provider.ScheduledJob{ID: "a", Cron: "0 9 * * 1", FireByUnixMs: time.Now().Add(6 * 24 * time.Hour).UnixMilli()}
	armed := func(token string) bool {
		t.Helper()
		if err := r.Handle(fromSession(wakeupEvent(t, threadID, fireAt), token)); err != nil {
			t.Fatalf("handle wakeup from %q: %v", token, err)
		}
		if err := r.Handle(fromSession(scheduledJobsEvent(t, threadID, job), token)); err != nil {
			t.Fatalf("handle jobs from %q: %v", token, err)
		}
		_, ok := r.PendingWakeupAt(threadID)
		r.ClearProcessSchedule(threadID)
		return ok
	}

	if armed("session-a") {
		t.Fatal("a frame reached a thread no session was committed to")
	}

	r.MarkThreadActive(threadID, "session-a")
	if !armed("session-a") {
		t.Fatal("the live session's frame was refused")
	}
	if !r.SessionCurrent(threadID, "session-a") {
		t.Fatal("SessionCurrent refused the live session")
	}

	r.MarkThreadActive(threadID, "session-b")
	if armed("session-a") {
		t.Fatal("a retired session's frame armed the replacement's schedule")
	}
	if r.SessionCurrent(threadID, "session-a") {
		t.Fatal("SessionCurrent admitted a retired session")
	}
	if err := r.Handle(fromSession(compactionStatusEvent(t, threadID, provider.CompactionStatusMeta{Active: true}, time.Now()), "session-a")); err != nil {
		t.Fatalf("handle compaction: %v", err)
	}
	if compactingOpen(r, threadID) {
		t.Fatal("a retired session's frame opened a compacting window on the replacement")
	}
	if !armed("session-b") {
		t.Fatal("the replacement's own frame was refused")
	}

	// Retired, then live again under a new session: neither earlier
	// session's frames come back.
	r.CleanupThread(threadID)
	if armed("session-b") {
		t.Fatal("a stopped thread admitted its last session's frame")
	}
	r.MarkThreadActive(threadID, "session-c")
	for _, old := range []string{"session-a", "session-b"} {
		if armed(old) {
			t.Fatalf("%s's frame was admitted after the thread went live again", old)
		}
	}
	if !armed("session-c") {
		t.Fatal("the new session's frame was refused")
	}

	// A host event carries no token: refused only while the thread is
	// stopped.
	if !armed("") {
		t.Fatal("a host event was refused on a live thread")
	}
	r.CleanupThread(threadID)
	if armed("") {
		t.Fatal("a host event reached a stopped thread through Handle")
	}
}

// A frame already past Handle's entry when a replacement commits cannot
// write state belonging to the provider process for the replacement: every
// writer of that state, which MarkThreadActive clears or which opens a turn
// or round, compares the token again under the lock it writes under. Each
// case calls its writer directly with the retired session's token after the
// commit, and once with the live session's token to show the assertion
// reads state the writer does write.
func TestProcessStateWritersRefuseARetiredSessionsFrame(t *testing.T) {
	t.Parallel()
	const threadID = "thread-session-writers"
	future := time.Now().Add(30 * time.Minute)
	modelFallback := func(t *testing.T) provider.ProviderEvent {
		meta, err := json.Marshal(modelFallbackMeta{OriginalModel: "claude-fable-5", FallbackModel: "claude-opus-4-8"})
		if err != nil {
			t.Fatal(err)
		}
		return provider.ProviderEvent{Kind: provider.EventModelFallback, ThreadID: threadID, ItemID: "model-fallback:1", Meta: meta, Timestamp: time.Now()}
	}
	progressTick := func(t *testing.T) provider.ProviderEvent {
		meta, err := json.Marshal(provider.SubagentProgressMeta{TaskID: "task-1", ToolUses: 2})
		if err != nil {
			t.Fatal(err)
		}
		return provider.ProviderEvent{ThreadID: threadID, ItemID: "launch-1", Meta: meta, Timestamp: time.Now()}
	}
	launch := store.Item{ID: "launch-1", ThreadID: threadID, Kind: itemKindToolCall, IsBackground: true}
	cases := []struct {
		// state names the processBoundState entry the writer writes.
		state string
		name  string
		write func(t *testing.T, r *Router, token string)
		held  func(r *Router) bool
	}{
		{"schedule", "session wakeup", func(t *testing.T, r *Router, token string) {
			if err := r.handleSessionWakeup(fromSession(wakeupEvent(t, threadID, future.UnixMilli()), token)); err != nil {
				t.Fatal(err)
			}
		}, func(r *Router) bool { _, ok := r.PendingWakeupAt(threadID); return ok }},
		{"schedule", "scheduled jobs", func(t *testing.T, r *Router, token string) {
			jobs := scheduledJobsEvent(t, threadID, provider.ScheduledJob{ID: "a", Cron: "* * * * *", Recurring: true, FireByUnixMs: future.UnixMilli()})
			if err := r.handleScheduledJobs(fromSession(jobs, token)); err != nil {
				t.Fatal(err)
			}
		}, func(r *Router) bool { _, ok := r.PendingWakeupAt(threadID); return ok }},
		{"compacting window", "compacting window", func(t *testing.T, r *Router, token string) {
			if err := r.handleCompactionStatus(fromSession(compactionStatusEvent(t, threadID, provider.CompactionStatusMeta{Active: true}, time.Now()), token)); err != nil {
				t.Fatal(err)
			}
		}, func(r *Router) bool { return compactingOpen(r, threadID) }},
		{"subagent progress", "subagent progress", func(t *testing.T, r *Router, token string) {
			if err := r.handleSubagentProgress(fromSession(progressTick(t), token)); err != nil {
				t.Fatal(err)
			}
		}, func(r *Router) bool { return len(r.LiveSubagentProgress(threadID)) != 0 }},
		{"subagent progress", "final subagent progress", func(t *testing.T, r *Router, token string) {
			if err := r.persistSubagentFinalProgress(launch, provider.SubagentProgressMeta{ToolUses: 3}, token); err != nil {
				t.Fatal(err)
			}
		}, func(r *Router) bool { _, ok := r.PeekSubagentProgress(threadID, launch.ID); return ok }},
		{"codex live tray", "codex execution tools", func(t *testing.T, r *Router, token string) {
			if err := r.startCodexExecutionTools(launch, 0, token); err != nil {
				t.Fatal(err)
			}
		}, func(r *Router) bool { _, ok := r.PeekSubagentProgress(threadID, launch.ID); return ok }},
		{"codex live tray", "codex agent runtime", func(t *testing.T, r *Router, token string) {
			if err := r.setCodexAgentRuntime(launch, token); err != nil {
				t.Fatal(err)
			}
		}, func(r *Router) bool { return len(r.CodexAgentRuntimeSnapshot(threadID)) != 0 }},
		{"codex live tray", "codex tool start", func(t *testing.T, r *Router, token string) {
			start := provider.ProviderEvent{Kind: provider.EventToolStart, ThreadID: threadID, ItemID: "exec-1", ItemType: "commandExecution", Meta: buildUnifiedExecStartMeta(t, "pid-1", "sleep 60"), Timestamp: time.Now()}
			r.observeCodexToolStart(fromSession(start, token))
		}, func(r *Router) bool {
			r.mu.Lock()
			defer r.mu.Unlock()
			state := r.codexBackgroundIfPresent(threadID)
			return state != nil && state.unifiedExec["exec-1"] != nil
		}},
		{"codex live tray", "codex subagent status", func(t *testing.T, r *Router, token string) {
			if err := r.store.UpdateProvider(threadID, "codex"); err != nil {
				t.Fatal(err)
			}
			seedCodexSpawnCard(t, r, r.store, threadID, "spawn-1", "child-1")
			status := provider.ProviderEvent{Kind: provider.EventSubagentStatus, ThreadID: threadID, ItemID: "spawn-1", Meta: json.RawMessage(`{"agent_path":"child-1","status":"completed"}`), Timestamp: time.Now()}
			if err := r.observeCodexSubagentStatus(fromSession(status, token)); err != nil {
				t.Fatal(err)
			}
		}, func(r *Router) bool {
			// The child's completion ends the execution the live tray tracks.
			r.mu.Lock()
			defer r.mu.Unlock()
			state := r.codexBackgroundIfPresent(threadID)
			return state == nil || state.spawnAgent["spawn-1"] == nil
		}},
		{"codex live tray", "codex child terminal", func(t *testing.T, r *Router, token string) {
			if err := r.store.UpdateProvider(threadID, "codex"); err != nil {
				t.Fatal(err)
			}
			seedCodexSpawnCard(t, r, r.store, threadID, "spawn-1", "child-1")
			r.observeCodexSpawnChildTerminalInMemory(threadID, "spawn-1", true, token)
		}, func(r *Router) bool {
			r.mu.Lock()
			defer r.mu.Unlock()
			state := r.codexBackgroundIfPresent(threadID)
			return state == nil || state.spawnAgent["spawn-1"] == nil
		}},
		{"command lifecycle", "command lifecycle", func(t *testing.T, r *Router, token string) {
			r.rememberCommandLifecycle(threadID, "command-1", commandLifecycleEntry{}, token)
		}, func(r *Router) bool { _, ok := r.peekCommandLifecycle(threadID, "command-1"); return ok }},
		{"deliveries awaiting a turn", "send started between turns", func(t *testing.T, r *Router, token string) {
			// Claude acks `started` for a held message before its turn's
			// init, so the delivery waits for that turn start.
			row := store.Item{ID: "user:0:flush:1", ThreadID: threadID, Kind: "user_text", Meta: `{"sendId":"thread-request:tok"}`}
			r.RegisterPendingQuietFlushSendWithExpectation(threadID, "queue:1", row, 1, 1, PendingSendExpectation{ProviderItemID: "uuid-1"})
			r.noteSendStarted(threadID, token, "uuid-1")
		}, func(r *Router) bool {
			r.mu.Lock()
			defer r.mu.Unlock()
			st := r.threadStateIfPresent(threadID)
			return st != nil && len(st.deliveriesAwaitingTurn) != 0
		}},
		{"effective model", "model fallback", func(t *testing.T, r *Router, token string) {
			if err := r.handleModelFallback(fromSession(modelFallback(t), token)); err != nil {
				t.Fatal(err)
			}
		}, func(r *Router) bool { model, _ := r.EffectiveModelSnapshot(threadID); return model != "" }},
		{"turn settlement", "turn settlement", func(t *testing.T, r *Router, token string) {
			r.claimTurnSettlement(threadID, 4, token)
		}, func(r *Router) bool { return r.isTurnSettled(threadID, 4) }},
		{"open turn and round", "turn start", func(t *testing.T, r *Router, token string) {
			if err := r.handleTurnStart(fromSession(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: threadID, TurnID: "turn-1", Timestamp: time.Now()}, token)); err != nil {
				t.Fatal(err)
			}
		}, func(r *Router) bool {
			// A retired session's turn neither opens nor leaves a turns row.
			next, err := r.store.NextTurnIndex(threadID)
			return r.HasInFlightTurnOrRound(threadID) || err != nil || next != 0
		}},
		{"open turn and round", "turn complete", func(t *testing.T, r *Router, token string) {
			// The live session's turn is open; a complete must close it only
			// when it is that session's own.
			if err := r.handleTurnStart(fromSession(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: threadID, TurnID: "turn-b", Timestamp: time.Now()}, "session-b")); err != nil {
				t.Fatal(err)
			}
			if err := r.handleTurnComplete(fromSession(provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: threadID, TurnID: "turn-b", TurnComplete: normalTurnCompleteMeta(), Timestamp: time.Now()}, token)); err != nil {
				t.Fatal(err)
			}
		}, func(r *Router) bool { return r.openRoundID(threadID) == "" || !r.HasInFlightTurnOrRound(threadID) }},
		{"queued sends dispatch", "turn complete queue flush", func(t *testing.T, r *Router, token string) {
			// The live session is idle with a message queued: only its own
			// turn boundary may send it.
			r.SetFlushDispatcher((&recordingDispatcher{}).dispatch)
			r.RegisterQueueItem(threadID, makeQueueItem("queue:0", "for the live session"))
			if err := r.handleTurnComplete(fromSession(provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: threadID, TurnComplete: normalTurnCompleteMeta(), Timestamp: time.Now()}, token)); err != nil {
				t.Fatal(err)
			}
		}, func(r *Router) bool { return !r.HasQueuedFlushItems(threadID) }},
		{"queued sends dispatch", "boundary queue flush", func(t *testing.T, r *Router, token string) {
			r.SetFlushDispatcher((&recordingDispatcher{}).dispatch)
			r.RegisterQueueItem(threadID, makeQueueItem("queue:0", "for the live session"))
			r.maybeFlushQueueAtBoundary(threadID, token)
		}, func(r *Router) bool { return !r.HasQueuedFlushItems(threadID) }},
		{"approvals", "approval request", func(t *testing.T, r *Router, token string) {
			request := provider.ApprovalRequest{RequestID: "req-1", ThreadID: threadID, ToolUseID: "tool-1", ToolName: "Bash", Kind: "command", Input: []byte(`{"command":"make"}`)}
			if err := r.handleApprovalRequest(fromSession(approvalRequestEvent(t, threadID, request, ""), token)); err != nil {
				t.Fatal(err)
			}
		}, func(r *Router) bool { approvals, _ := pendingRequestIDs(r, threadID); return len(approvals) != 0 }},
		{"user inputs", "user input request", func(t *testing.T, r *Router, token string) {
			evt := provider.ProviderEvent{
				Kind: provider.EventUserInputRequest, ThreadID: threadID, ItemID: "q-1", Timestamp: time.Now(),
				Meta: parkMeta(t, map[string]any{
					"requestId": "q-1", "threadId": threadID, "toolUseId": "tool-q", "toolName": "AskUserQuestion",
					"questions": []provider.UserInputQuestion{{ID: "q1", Header: "Target", Question: "Which target?"}},
				}),
			}
			if err := r.handleUserInputRequest(fromSession(evt, token)); err != nil {
				t.Fatal(err)
			}
		}, func(r *Router) bool { _, questions := pendingRequestIDs(r, threadID); return len(questions) != 0 }},
		{"streams", "text stream", func(t *testing.T, r *Router, token string) {
			if err := r.handleTextDelta(fromSession(provider.ProviderEvent{Kind: provider.EventTextDelta, ThreadID: threadID, Content: "text", Timestamp: time.Now()}, token)); err != nil {
				t.Fatal(err)
			}
		}, func(r *Router) bool { return r.hasActiveStreamingItem(threadID) }},
		{"streams", "thinking stream", func(t *testing.T, r *Router, token string) {
			if err := r.handleThinking(fromSession(provider.ProviderEvent{Kind: provider.EventThinking, ThreadID: threadID, Content: "thinking", Timestamp: time.Now()}, token)); err != nil {
				t.Fatal(err)
			}
		}, func(r *Router) bool { return r.hasActiveStreamingItem(threadID) }},
		{"streams", "compaction reasoning stream", func(t *testing.T, r *Router, token string) {
			if err := r.handleCompactionReasoning(fromSession(provider.ProviderEvent{Kind: provider.EventThinking, ThreadID: threadID, ParentToolUseID: provider.CompactionReasoningScope, Content: "summary", Timestamp: time.Now()}, token)); err != nil {
				t.Fatal(err)
			}
		}, func(r *Router) bool { return r.hasActiveStreamingItem(threadID) }},
		{"open turn and round", "open turn", func(t *testing.T, r *Router, token string) {
			if _, err := r.setOpenTurn(threadID, 2, token); err != nil {
				t.Fatal(err)
			}
		}, func(r *Router) bool { return r.HasInFlightTurnOrRound(threadID) }},
		{"open turn and round", "open round", func(t *testing.T, r *Router, token string) {
			r.setOpenRoundSnapshot(ActiveTurnSnapshot{ThreadID: threadID, TurnID: "round-1", TurnIndex: 2, StartedAt: 1}, token)
		}, func(r *Router) bool { return r.openRoundID(threadID) != "" }},
		{"open turn and round", "queued echo turn", func(t *testing.T, r *Router, token string) {
			r.openQueuedEchoTurn(threadID, 1, time.Now().UnixMilli(), -1, token)
		}, func(r *Router) bool { return r.HasInFlightTurnOrRound(threadID) }},
	}
	// processBoundState is the list of what a retired frame must not
	// write: every entry a frame can write has a case here, and every case
	// names an entry.
	covered := make(map[string]bool)
	for _, c := range cases {
		covered[c.state] = true
	}
	for _, entry := range processBoundState {
		if covered[entry.name] == entry.hostWritten {
			t.Errorf("processBoundState %q (hostWritten=%v) has refusal cases=%v: a frame-written entry needs one, a host-written entry has no frame writer", entry.name, entry.hostWritten, covered[entry.name])
		}
		delete(covered, entry.name)
	}
	for state := range covered {
		t.Errorf("a case names %q, which is not in processBoundState", state)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			for _, writer := range []string{"session-a", "session-b"} {
				r, st, _ := newTestRouter(t)
				createTestThread(t, st, threadID)
				r.MarkThreadActive(threadID, "session-a")
				r.MarkThreadActive(threadID, "session-b")
				c.write(t, r, writer)
				if held := c.held(r); held != (writer == "session-b") {
					t.Fatalf("a frame of %s wrote the state: %v; only the live session-b may", writer, held)
				}
			}
		})
	}
}

// The probes below are the verifier's, kept as they found the gaps: a frame
// already being handled when the replacement commits.
func TestInFlightRetiredFrameDoesNotSeedSubagentProgress(t *testing.T) {
	const threadID = "thread-probe-progress"
	r := NewRouter(nil, func(eventchan.Channel, any) {})
	r.MarkThreadActive(threadID, "session-a")
	meta, _ := json.Marshal(provider.SubagentProgressMeta{TaskID: "task-1"})
	tick := fromSession(provider.ProviderEvent{ThreadID: threadID, ItemID: "launch-a", Meta: meta, Timestamp: time.Now()}, "session-a")
	r.MarkThreadActive(threadID, "session-b")
	if err := r.handleSubagentProgress(tick); err != nil {
		t.Fatal(err)
	}
	if got := r.LiveSubagentProgress(threadID); len(got) != 0 {
		t.Fatalf("replacement's live state carries the retired session's subagent progress: %+v", got)
	}
}

func TestInFlightRetiredTurnStartDoesNotOpenATurn(t *testing.T) {
	r, _, _ := newTestRouter(t)
	const threadID = "thread-probe-turn"
	r.MarkThreadActive(threadID, "session-a")
	start := fromSession(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: threadID, TurnID: "turn-a", Timestamp: time.Now()}, "session-a")
	r.MarkThreadActive(threadID, "session-b")
	if err := r.handleTurnStart(start); err != nil {
		t.Fatal(err)
	}
	if r.HasInFlightTurnOrRound(threadID) {
		t.Fatalf("replacement holds an open turn opened by the retired session's in-flight frame (AnyUnfinishedWork=%v)", r.AnyUnfinishedWork(time.Minute))
	}
}

// A Codex execution's live tool count belongs to the session that started
// it: a child row a retired session's in-flight frame writes after the
// replacement committed does not seed the replacement's progress.
func TestInFlightRetiredChildRowDoesNotCountTowardTheReplacement(t *testing.T) {
	t.Parallel()
	r, st, _ := newTestRouter(t)
	const threadID = "thread-inflight-tool-count"
	createTestThread(t, st, threadID)
	r.MarkThreadActive(threadID, "session-a")
	launch := store.Item{ID: "launch-1", ThreadID: threadID, Kind: itemKindToolCall, TurnIndex: 1}
	if err := r.startCodexExecutionTools(launch, 0, "session-a"); err != nil {
		t.Fatal(err)
	}
	r.MarkThreadActive(threadID, "session-b")
	r.countCodexExecutionToolCall(store.Item{ID: "child-tool-1", ThreadID: threadID, Kind: itemKindToolCall, ParentID: launch.ID, TurnIndex: 1, ItemIndex: 5})
	if progress, ok := r.PeekSubagentProgress(threadID, launch.ID); ok {
		t.Fatalf("the replacement carries the retired session's execution tool count: %+v", progress)
	}
}

// HandleSessionFrame answers the host with the same final check: false for
// a frame whose session was retired before or during its handling, true for
// the live session's frame and for a stopped thread's own frame.
func TestHandleSessionFrameReportsWhetherTheFramesSessionStillOwnsTheThread(t *testing.T) {
	t.Parallel()
	const threadID = "thread-session-frame"
	r := NewRouter(nil, func(eventchan.Channel, any) {})
	r.MarkThreadActive(threadID, "session-a")
	frame := func(token string) bool {
		t.Helper()
		current, err := r.HandleSessionFrame(fromSession(compactionStatusEvent(t, threadID, provider.CompactionStatusMeta{}, time.Now()), token))
		if err != nil {
			t.Fatal(err)
		}
		return current
	}
	if !frame("session-a") {
		t.Fatal("the live session's frame reported retired")
	}
	// Retired while the frame is being handled: the hook runs inside it.
	r.SetEventHook(func(provider.ProviderEvent) { r.MarkThreadActive(threadID, "session-b") })
	if frame("session-a") {
		t.Fatal("a frame whose session was retired while it was handled reported current")
	}
	r.SetEventHook(nil)
	if frame("session-a") {
		t.Fatal("a retired session's frame reported current")
	}
	r.CleanupThread(threadID)
	if !frame("session-b") {
		t.Fatal("a stopped thread's own frame reported retired")
	}
}

// The gate is what keeps a retired session's history out of the thread: a
// row its frame would write is not written once a replacement commits.
func TestGateKeepsARetiredSessionsRowsOutOfTheThread(t *testing.T) {
	t.Parallel()
	const threadID = "thread-session-rows"
	r, st, _ := newTestRouter(t)
	createTestThread(t, st, threadID)
	r.MarkThreadActive(threadID, "session-a")
	r.MarkThreadActive(threadID, "session-b")
	errorFrame := func(token string) {
		t.Helper()
		if err := r.Handle(fromSession(provider.ProviderEvent{Kind: provider.EventError, ThreadID: threadID, Content: "provider failed: " + token, Timestamp: time.Now()}, token)); err != nil {
			t.Fatal(err)
		}
	}
	errorFrame("session-a")
	items, err := st.ListItems(threadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("a retired session's frame wrote %d row(s): %+v", len(items), items)
	}
	errorFrame("session-b")
	if items, err = st.ListItems(threadID); err != nil || len(items) == 0 {
		t.Fatalf("the live session's frame wrote no row: %v", err)
	}
}

// The round-2 verifier probes, kept as they found the gaps.

// A child row an in-flight retired frame persists stays in history but is
// not counted; the replacement's own execution of the same launch counts
// from its start.
func TestRetiredChildRowStaysInHistoryAndTheReplacementCountsItsOwn(t *testing.T) {
	r, st, _ := newTestRouter(t)
	const threadID = "thread-r2-count"
	createTestThread(t, st, threadID)
	r.MarkThreadActive(threadID, "session-a")
	now := time.Now().UnixMilli()
	launch, err := r.persistItemQuietReturning(store.Item{ID: "launch-1", ThreadID: threadID, Kind: itemKindToolCall, Role: "assistant", TurnIndex: 1, Status: "running", CreatedAt: now, UpdatedAt: now}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.startCodexExecutionTools(launch, launch.ItemIndex, "session-a"); err != nil {
		t.Fatal(err)
	}
	r.MarkThreadActive(threadID, "session-b")
	// A's in-flight frame persists its child row after the commit.
	childA, err := r.persistItemQuietReturning(store.Item{ID: "child-a", ThreadID: threadID, Kind: itemKindToolCall, Role: "assistant", ParentID: launch.ID, TurnIndex: 1, Status: "completed", CreatedAt: now, UpdatedAt: now}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := st.GetThreadItem(threadID, childA.ID); err != nil || !found {
		t.Fatalf("the retired frame's child row is not in history: found=%v err=%v", found, err)
	}
	if p, ok := r.PeekSubagentProgress(threadID, launch.ID); ok && p.ToolUses != 0 {
		t.Fatalf("the retired frame's child row counted: %+v", p)
	}
	// B starts its own execution of the launch after A's row.
	if err := r.startCodexExecutionTools(launch, childA.ItemIndex, "session-b"); err != nil {
		t.Fatal(err)
	}
	if p, _ := r.PeekSubagentProgress(threadID, launch.ID); p.ToolUses != 0 {
		t.Fatalf("B's execution did not start from zero: %+v", p)
	}
	if _, err := r.persistItemQuietReturning(store.Item{ID: "child-b", ThreadID: threadID, Kind: itemKindToolCall, Role: "assistant", ParentID: launch.ID, TurnIndex: 1, Status: "completed", CreatedAt: now, UpdatedAt: now}, nil); err != nil {
		t.Fatal(err)
	}
	if p, _ := r.PeekSubagentProgress(threadID, launch.ID); p.ToolUses != 1 {
		t.Fatalf("B's own child row did not count: %+v", p)
	}
}

// A retired session's in-flight text delta opens no stream on the
// replacement. Its block stop is queued behind it and refused at the gate,
// so nothing of A's would ever settle it.
func TestInFlightRetiredTextDeltaOpensNoStream(t *testing.T) {
	r, st, _ := newTestRouter(t)
	const threadID = "thread-r2-stream"
	createTestThread(t, st, threadID)
	r.MarkThreadActive(threadID, "session-a")
	r.MarkThreadActive(threadID, "session-b")
	delta := fromSession(provider.ProviderEvent{Kind: provider.EventTextDelta, ThreadID: threadID, Content: "late text from A", Timestamp: time.Now()}, "session-a")
	if err := r.handleTextDelta(delta); err != nil {
		t.Fatal(err)
	}
	if r.hasActiveStreamingItem(threadID) {
		t.Fatalf("the replacement holds a stream opened by the retired session's in-flight delta (AnyUnfinishedWork=%v)", r.AnyUnfinishedWork(time.Minute))
	}
}

// After the replacement serves a full turn, nothing of the retired
// session's in-flight delta holds the thread.
func TestInFlightRetiredTextDeltaLeavesNothingAfterTheReplacementsTurn(t *testing.T) {
	r, st, _ := newTestRouter(t)
	const threadID = "thread-r2-stream-turn"
	createTestThread(t, st, threadID)
	r.MarkThreadActive(threadID, "session-a")
	r.MarkThreadActive(threadID, "session-b")
	if err := r.handleTextDelta(fromSession(provider.ProviderEvent{Kind: provider.EventTextDelta, ThreadID: threadID, Content: "late text from A", Timestamp: time.Now()}, "session-a")); err != nil {
		t.Fatal(err)
	}
	if err := r.Handle(fromSession(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: threadID, TurnID: "turn-b", Timestamp: time.Now()}, "session-b")); err != nil {
		t.Fatal(err)
	}
	if err := r.Handle(fromSession(provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: threadID, TurnID: "turn-b", TurnComplete: normalTurnCompleteMeta(), Timestamp: time.Now()}, "session-b")); err != nil {
		t.Fatal(err)
	}
	r.DrainWireItemRefresh()
	if r.hasActiveStreamingItem(threadID) || r.AnyUnfinishedWork(time.Minute) {
		t.Fatalf("after B's own turn ended, A's in-flight stream still holds the thread: streaming=%v unfinished=%v", r.hasActiveStreamingItem(threadID), r.AnyUnfinishedWork(time.Minute))
	}
}

// A retired session's in-flight approval request registers no prompt on
// the replacement: only A's dead process could take the answer.
func TestInFlightRetiredApprovalRequestRegistersNoPrompt(t *testing.T) {
	r, st, _ := newTestRouter(t)
	const threadID = "thread-r2-approval"
	createTestThread(t, st, threadID)
	r.MarkThreadActive(threadID, "session-a")
	r.MarkThreadActive(threadID, "session-b")
	evt := approvalRequestEvent(t, threadID, provider.ApprovalRequest{RequestID: "req-a", ThreadID: threadID, ToolUseID: "tool-a", ToolName: "Bash", Kind: "command", Input: []byte(`{"command":"make"}`)}, "")
	if err := r.handleApprovalRequest(fromSession(evt, "session-a")); err != nil {
		t.Fatal(err)
	}
	approvals, questions := pendingRequestIDs(r, threadID)
	if len(approvals) != 0 || len(questions) != 0 {
		t.Fatalf("the replacement holds the retired session's prompt: approvals=%v questions=%v unfinished=%v", approvals, questions, r.AnyUnfinishedWork(time.Minute))
	}
}

func TestInFlightRetiredUserInputRequestRegistersNoQuestion(t *testing.T) {
	r, st, _ := newTestRouter(t)
	const threadID = "thread-r2-question"
	createTestThread(t, st, threadID)
	r.MarkThreadActive(threadID, "session-a")
	r.MarkThreadActive(threadID, "session-b")
	evt := provider.ProviderEvent{
		Kind: provider.EventUserInputRequest, ThreadID: threadID, ItemID: "q-a", Timestamp: time.Now(),
		Meta: parkMeta(t, map[string]any{
			"requestId": "q-a", "threadId": threadID, "toolUseId": "tool-q", "toolName": "AskUserQuestion",
			"questions": []provider.UserInputQuestion{{ID: "q1", Header: "Target", Question: "Which target?"}},
		}),
	}
	if err := r.handleUserInputRequest(fromSession(evt, "session-a")); err != nil {
		t.Fatal(err)
	}
	approvals, questions := pendingRequestIDs(r, threadID)
	if len(approvals) != 0 || len(questions) != 0 {
		t.Fatalf("the replacement holds the retired session's question: approvals=%v questions=%v unfinished=%v", approvals, questions, r.AnyUnfinishedWork(time.Minute))
	}
}

// A settle goroutine outlives Handle, and CleanupThread does not wait for
// it. When it finishes after the thread was cleaned up and a replacement
// opened a stream of its own, it releases nothing: the count it would
// release was dropped with the old state, and the live count is the
// replacement's.
func TestLateSettleOfACleanedUpStreamKeepsTheReplacementsStreamCount(t *testing.T) {
	r, st, _ := newTestRouter(t)
	const threadID = "thread-r2-settle"
	createTestThread(t, st, threadID)
	r.MarkThreadActive(threadID, "session-a")
	if err := r.handleTextDelta(fromSession(provider.ProviderEvent{Kind: provider.EventTextDelta, ThreadID: threadID, Content: "A streams", Timestamp: time.Now()}, "session-a")); err != nil {
		t.Fatal(err)
	}
	// A's block stop takes the block; its settle goroutine has not run.
	turnIndex, err := r.currentTurnIndex(threadID)
	if err != nil {
		t.Fatal(err)
	}
	blockA, taken := r.takeActiveTextBlock(threadID, turnIndex, "", "")
	if !taken {
		t.Fatal("setup: A's stream is not open")
	}
	r.CleanupThread(threadID)
	r.MarkThreadActive(threadID, "session-b")
	if err := r.handleTextDelta(fromSession(provider.ProviderEvent{Kind: provider.EventTextDelta, ThreadID: threadID, Content: "B streams", Timestamp: time.Now()}, "session-b")); err != nil {
		t.Fatal(err)
	}
	if !r.hasActiveStreamingItem(threadID) {
		t.Fatal("setup: B's stream did not open")
	}
	// The tail of A's settle goroutine.
	if err := r.doSettleStreamingText(blockA, statusCompleted, "", false, nil); err != nil {
		t.Fatal(err)
	}
	if !r.hasActiveStreamingItem(threadID) {
		t.Fatal("a late settle of the cleaned-up session's stream released the replacement's live stream count")
	}
}

// The session-config restart (reconcileSessionConfig) commits a new
// session without CleanupThread, so a stream the old session opened stays
// on the live state. Whoever settles it later releases its count: the
// release follows the state the count was taken on, not the session that
// opened the stream.
func TestSettleOfAStreamCarriedAcrossACommitReleasesItsCount(t *testing.T) {
	r, st, _ := newTestRouter(t)
	const threadID = "thread-carried-stream"
	createTestThread(t, st, threadID)
	r.MarkThreadActive(threadID, "session-a")
	if err := r.handleTextDelta(fromSession(provider.ProviderEvent{Kind: provider.EventTextDelta, ThreadID: threadID, Content: "A streams", Timestamp: time.Now()}, "session-a")); err != nil {
		t.Fatal(err)
	}
	r.MarkThreadActive(threadID, "session-b")
	turnIndex, err := r.currentTurnIndex(threadID)
	if err != nil {
		t.Fatal(err)
	}
	block, taken := r.takeActiveTextBlock(threadID, turnIndex, "", "")
	if !taken {
		t.Fatal("setup: the carried stream is not open")
	}
	if err := r.doSettleStreamingText(block, statusCompleted, "", false, nil); err != nil {
		t.Fatal(err)
	}
	if r.hasActiveStreamingItem(threadID) {
		t.Fatal("settling the carried stream left its count on the live state")
	}
}

// A frame that passed the gate can have its session retired before it
// reaches its boundary's queue flush. Each boundary that flushes is driven
// through HandleSessionFrame with the replacement committing at the first
// emission of the frame's handling, which comes after the gate and before
// the flush. The replacement's queued message must wait for a boundary of
// its own. The same run without the commit dispatches it, so each case
// reaches the flush it names.
func TestInFlightRetiredBoundaryDoesNotSendTheReplacementsQueue(t *testing.T) {
	t.Parallel()
	const threadID = "thread-inflight-flush"
	bash := func(id string, meta map[string]any) provider.ProviderEvent {
		return provider.ProviderEvent{ItemID: id, ItemType: "Bash", Meta: parkMeta(t, meta)}
	}
	for _, c := range []struct {
		name  string
		codex bool
		setup []provider.ProviderEvent
		frame provider.ProviderEvent
	}{
		{"background task terminal", false, []provider.ProviderEvent{
			withKind(bash("bg-1", map[string]any{"toolName": "Bash", "is_background": true, "input": map[string]any{"command": "sleep 60", "run_in_background": true}}), provider.EventToolStart),
			withKind(bash("bg-1", map[string]any{"toolName": "Bash", "backgroundTaskId": "task-1"}), provider.EventToolComplete),
		}, provider.ProviderEvent{Kind: provider.EventBackgroundTaskTerminal, ItemID: "bg-1", Meta: parkMeta(t, map[string]any{"task_id": "task-1", "tool_use_id": "bg-1", "status": "completed", "source": "task_updated"})}},
		{"codex unified exec complete", true, []provider.ProviderEvent{
			{Kind: provider.EventToolStart, ItemID: "exec-1", ItemType: "commandExecution", Meta: buildUnifiedExecStartMeta(t, "pid-1", "sleep 1")},
		}, provider.ProviderEvent{Kind: provider.EventToolComplete, ItemID: "exec-1", ItemType: "commandExecution", Meta: buildUnifiedExecCompleteMeta(t, "completed", "pid-1", "sleep 1", 0)}},
		{"tool complete", false, []provider.ProviderEvent{
			{Kind: provider.EventToolStart, ItemID: "tool-1", ItemType: "Read", Meta: parkMeta(t, map[string]any{"toolName": "Read", "input": map[string]any{"file_path": "/tmp/x"}})},
		}, provider.ProviderEvent{Kind: provider.EventToolComplete, ItemID: "tool-1", ItemType: "Read", Content: "contents", Meta: parkMeta(t, map[string]any{"toolName": "Read"})}},
		{"turn complete", false, []provider.ProviderEvent{
			{Kind: provider.EventTurnStart, TurnID: "turn-a"},
		}, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnID: "turn-a", TurnComplete: normalTurnCompleteMeta()}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			for _, commit := range []bool{true, false} {
				st := storetest.Clone(t)
				var r *Router
				var armed atomic.Bool
				var once sync.Once
				r = NewRouter(st, func(eventchan.Channel, any) {
					if commit && armed.Load() {
						once.Do(func() { r.MarkThreadActive(threadID, "session-b") })
					}
				})
				holdWireRefresh(r)
				if c.codex {
					createCodexBackgroundTestThread(t, st, threadID)
				} else {
					createTestThread(t, st, threadID)
				}
				r.MarkThreadActive(threadID, "session-a")
				for _, e := range c.setup {
					e.ThreadID, e.Timestamp = threadID, time.Now()
					if _, err := r.HandleSessionFrame(fromSession(e, "session-a")); err != nil {
						t.Fatal(err)
					}
				}
				rec := &recordingDispatcher{}
				r.SetFlushDispatcher(rec.dispatch)
				r.RegisterQueueItem(threadID, makeQueueItem("queue:0", "for the live session"))
				armed.Store(true)
				frame := c.frame
				frame.ThreadID, frame.Timestamp = threadID, time.Now()
				if _, err := r.HandleSessionFrame(fromSession(frame, "session-a")); err != nil {
					t.Fatal(err)
				}
				r.DrainWireItemRefresh()
				sent := len(rec.snapshot()) != 0 || !r.HasQueuedFlushItems(threadID)
				if commit && sent {
					t.Fatalf("a boundary of the retired session sent the replacement's queued message: %+v", rec.snapshot())
				}
				if !commit && !sent {
					t.Fatal("control: the live session's own boundary did not send its queued message, so the case does not reach the flush")
				}
			}
		})
	}
}

func withKind(evt provider.ProviderEvent, kind provider.EventKind) provider.ProviderEvent {
	evt.Kind = kind
	return evt
}
