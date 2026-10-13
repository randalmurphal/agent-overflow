package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
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
	for _, c := range []struct {
		name  string
		write func(t *testing.T, r *Router, token string)
		held  func(r *Router) bool
	}{
		{"session wakeup", func(t *testing.T, r *Router, token string) {
			if err := r.handleSessionWakeup(fromSession(wakeupEvent(t, threadID, future.UnixMilli()), token)); err != nil {
				t.Fatal(err)
			}
		}, func(r *Router) bool { _, ok := r.PendingWakeupAt(threadID); return ok }},
		{"scheduled jobs", func(t *testing.T, r *Router, token string) {
			jobs := scheduledJobsEvent(t, threadID, provider.ScheduledJob{ID: "a", Cron: "* * * * *", Recurring: true, FireByUnixMs: future.UnixMilli()})
			if err := r.handleScheduledJobs(fromSession(jobs, token)); err != nil {
				t.Fatal(err)
			}
		}, func(r *Router) bool { _, ok := r.PendingWakeupAt(threadID); return ok }},
		{"compacting window", func(t *testing.T, r *Router, token string) {
			if err := r.handleCompactionStatus(fromSession(compactionStatusEvent(t, threadID, provider.CompactionStatusMeta{Active: true}, time.Now()), token)); err != nil {
				t.Fatal(err)
			}
		}, func(r *Router) bool { return compactingOpen(r, threadID) }},
		{"subagent progress", func(t *testing.T, r *Router, token string) {
			if err := r.handleSubagentProgress(fromSession(progressTick(t), token)); err != nil {
				t.Fatal(err)
			}
		}, func(r *Router) bool { return len(r.LiveSubagentProgress(threadID)) != 0 }},
		{"final subagent progress", func(t *testing.T, r *Router, token string) {
			if err := r.persistSubagentFinalProgress(launch, provider.SubagentProgressMeta{ToolUses: 3}, token); err != nil {
				t.Fatal(err)
			}
		}, func(r *Router) bool { _, ok := r.PeekSubagentProgress(threadID, launch.ID); return ok }},
		{"codex execution tools", func(t *testing.T, r *Router, token string) {
			if err := r.startCodexExecutionTools(launch, 0, token); err != nil {
				t.Fatal(err)
			}
		}, func(r *Router) bool { _, ok := r.PeekSubagentProgress(threadID, launch.ID); return ok }},
		{"codex agent runtime", func(t *testing.T, r *Router, token string) {
			if err := r.setCodexAgentRuntime(launch, token); err != nil {
				t.Fatal(err)
			}
		}, func(r *Router) bool { return len(r.CodexAgentRuntimeSnapshot(threadID)) != 0 }},
		{"codex tool start", func(t *testing.T, r *Router, token string) {
			start := provider.ProviderEvent{Kind: provider.EventToolStart, ThreadID: threadID, ItemID: "exec-1", ItemType: "commandExecution", Meta: buildUnifiedExecStartMeta(t, "pid-1", "sleep 60"), Timestamp: time.Now()}
			r.observeCodexToolStart(fromSession(start, token))
		}, func(r *Router) bool {
			r.mu.Lock()
			defer r.mu.Unlock()
			state := r.codexBackgroundIfPresent(threadID)
			return state != nil && state.unifiedExec["exec-1"] != nil
		}},
		{"codex subagent status", func(t *testing.T, r *Router, token string) {
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
		{"codex child terminal", func(t *testing.T, r *Router, token string) {
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
		{"command lifecycle", func(t *testing.T, r *Router, token string) {
			r.rememberCommandLifecycle(threadID, "command-1", commandLifecycleEntry{}, token)
		}, func(r *Router) bool { _, ok := r.peekCommandLifecycle(threadID, "command-1"); return ok }},
		{"model fallback", func(t *testing.T, r *Router, token string) {
			if err := r.handleModelFallback(fromSession(modelFallback(t), token)); err != nil {
				t.Fatal(err)
			}
		}, func(r *Router) bool { model, _ := r.EffectiveModelSnapshot(threadID); return model != "" }},
		{"turn settlement", func(t *testing.T, r *Router, token string) {
			r.claimTurnSettlement(threadID, 4, token)
		}, func(r *Router) bool { return r.isTurnSettled(threadID, 4) }},
		{"turn start", func(t *testing.T, r *Router, token string) {
			if err := r.handleTurnStart(fromSession(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: threadID, TurnID: "turn-1", Timestamp: time.Now()}, token)); err != nil {
				t.Fatal(err)
			}
		}, func(r *Router) bool {
			// A retired session's turn neither opens nor leaves a turns row.
			next, err := r.store.NextTurnIndex(threadID)
			return r.HasInFlightTurnOrRound(threadID) || err != nil || next != 0
		}},
		{"turn complete", func(t *testing.T, r *Router, token string) {
			// The live session's turn is open; a complete must close it only
			// when it is that session's own.
			if err := r.handleTurnStart(fromSession(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: threadID, TurnID: "turn-b", Timestamp: time.Now()}, "session-b")); err != nil {
				t.Fatal(err)
			}
			if err := r.handleTurnComplete(fromSession(provider.ProviderEvent{Kind: provider.EventTurnComplete, ThreadID: threadID, TurnID: "turn-b", TurnComplete: normalTurnCompleteMeta(), Timestamp: time.Now()}, token)); err != nil {
				t.Fatal(err)
			}
		}, func(r *Router) bool { return r.openRoundID(threadID) == "" || !r.HasInFlightTurnOrRound(threadID) }},
		{"open turn", func(t *testing.T, r *Router, token string) {
			if _, err := r.setOpenTurn(threadID, 2, token); err != nil {
				t.Fatal(err)
			}
		}, func(r *Router) bool { return r.HasInFlightTurnOrRound(threadID) }},
		{"open round", func(t *testing.T, r *Router, token string) {
			r.setOpenRoundSnapshot(ActiveTurnSnapshot{ThreadID: threadID, TurnID: "round-1", TurnIndex: 2, StartedAt: 1}, token)
		}, func(r *Router) bool { return r.openRoundID(threadID) != "" }},
		{"queued echo turn", func(t *testing.T, r *Router, token string) {
			r.openQueuedEchoTurn(threadID, 1, time.Now().UnixMilli(), -1, token)
		}, func(r *Router) bool { return r.HasInFlightTurnOrRound(threadID) }},
	} {
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
