package triage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
)

// startCodexCommandFromSession starts a unified-exec command as session
// token's frame: a root thread command for parent "", an agent's command
// for its spawn launch id otherwise.
func startCodexCommandFromSession(t *testing.T, r *Router, threadID, itemID, processID, parent, token string) {
	t.Helper()
	start := provider.ProviderEvent{Kind: provider.EventToolStart, ThreadID: threadID, ItemID: itemID, ItemType: "commandExecution", ParentToolUseID: parent, Meta: buildUnifiedExecStartMeta(t, processID, "sleep 60"), Timestamp: time.Now()}
	if !r.observeCodexToolStart(fromSession(start, token)) {
		t.Fatalf("command %s start was not taken as a live command", itemID)
	}
}

func codexCommandTracked(r *Router, threadID, itemID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.codexExecIfPresent(threadID).lookup(itemID) != nil
}

func (r *Router) codexEndedCount(threadID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if execs := r.codexExecIfPresent(threadID); execs != nil {
		return len(execs.ended)
	}
	return 0
}

func trayRefreshes(log *emissionLog, threadID string) int {
	count := 0
	for _, e := range log.snapshot() {
		if e.eventName != eventchan.ProviderBackgroundTray.String() {
			continue
		}
		if event, ok := e.data.(BackgroundTrayEvent); ok && event.ThreadID == threadID && event.Refresh {
			count++
		}
	}
	return count
}

// A thread Codex unloads has no processes left, so its commands leave the
// live work with it. Only the unloaded agent's commands go: another agent's
// and the root thread's stay, and an agent's turn ending, however it ends,
// removes none of them.
func TestCodexAgentCommandsEndWhenTheAgentThreadUnloads(t *testing.T) {
	t.Parallel()
	r, st, emissions := newTestRouter(t)
	const threadID = "thread-agent-unload"
	createCodexBackgroundTestThread(t, st, threadID)
	r.MarkThreadActive(threadID, "session-a")
	seedOpenTurn(t, r, st, threadID, 0)
	seedCodexSpawnCard(t, r, st, threadID, "spawn-1", "child-1")
	seedCodexSpawnCard(t, r, st, threadID, "spawn-2", "child-2")
	startCodexCommandFromSession(t, r, threadID, "root-cmd", "1001", "", "session-a")
	startCodexCommandFromSession(t, r, threadID, "agent-1-cmd", "1002", "spawn-1", "session-a")
	startCodexCommandFromSession(t, r, threadID, "agent-2-cmd", "1003", "spawn-2", "session-a")

	status := func(launch, child, value, turnID string) {
		t.Helper()
		evt := provider.ProviderEvent{Kind: provider.EventSubagentStatus, ThreadID: threadID, ItemID: launch, ParentToolUseID: launch, TurnID: turnID, Meta: json.RawMessage(`{"agent_path":"` + child + `","status":"` + value + `"}`), Timestamp: time.Now()}
		if err := r.observeCodexSubagentStatus(fromSession(evt, "session-a")); err != nil {
			t.Fatalf("status %s for %s: %v", value, launch, err)
		}
	}
	for _, ended := range []string{"completed", "errored", "interrupted", "idle"} {
		status("spawn-1", "child-1", "running", "child-turn-"+ended)
		status("spawn-1", "child-1", ended, "child-turn-"+ended)
	}
	for _, id := range []string{"root-cmd", "agent-1-cmd", "agent-2-cmd"} {
		if !codexCommandTracked(r, threadID, id) {
			t.Fatalf("command %s left the live work when agent-1's turns ended", id)
		}
	}
	// The three commands and agent-2, whose child the spawn reported running.
	if count := r.CountLiveCodexBackgroundTasks(threadID); count != 4 {
		t.Fatalf("live work after agent-1's turns ended = %d, want 4", count)
	}

	emissions.reset()
	status("spawn-1", "child-1", "notLoaded", "")
	if codexCommandTracked(r, threadID, "agent-1-cmd") {
		t.Fatal("the unloaded agent's command is still live")
	}
	if !codexCommandTracked(r, threadID, "agent-2-cmd") || !codexCommandTracked(r, threadID, "root-cmd") {
		t.Fatal("unloading one agent ended commands it does not run")
	}
	if got := len(r.ListLiveCodexBackgroundTasks(threadID, 0, 0)); got != 2 {
		t.Fatalf("tray rows = %d, want 2", got)
	}
	if trayRefreshes(emissions, threadID) == 0 {
		t.Fatal("no tray refresh announced the removed command")
	}
}

// A replacement session commits the thread to a new app-server; the
// replaced one's commands, the root thread's and its agents', died with it.
// A frame the replaced session sends late adds none back.
func TestCodexReplacementSessionDropsTheReplacedProcessCommands(t *testing.T) {
	t.Parallel()
	r, st, emissions := newTestRouter(t)
	const threadID = "thread-agent-replace"
	createCodexBackgroundTestThread(t, st, threadID)
	r.MarkThreadActive(threadID, "session-a")
	startCodexCommandFromSession(t, r, threadID, "root-cmd", "1001", "", "session-a")
	startCodexCommandFromSession(t, r, threadID, "agent-cmd", "1002", "spawn-1", "session-a")
	if count := r.CountLiveCodexBackgroundTasks(threadID); count != 2 {
		t.Fatalf("live work before the replacement = %d, want 2", count)
	}

	emissions.reset()
	r.MarkThreadActive(threadID, "session-b")
	if count := r.CountLiveCodexBackgroundTasks(threadID); count != 0 {
		t.Fatalf("live work after the replacement = %d, want 0", count)
	}
	if ids := r.ThreadIDsWithLiveCodexBackgroundTasks(); len(ids) != 0 {
		t.Fatalf("threads with live work after the replacement = %v, want none", ids)
	}
	if trayRefreshes(emissions, threadID) == 0 {
		t.Fatal("the replacement did not refresh the tray that listed the dead commands")
	}
	late := provider.ProviderEvent{Kind: provider.EventToolStart, ThreadID: threadID, ItemID: "late-cmd", ItemType: "commandExecution", ParentToolUseID: "spawn-1", Meta: buildUnifiedExecStartMeta(t, "1003", "sleep 60"), Timestamp: time.Now()}
	r.observeCodexToolStart(fromSession(late, "session-a"))
	if count := r.CountLiveCodexBackgroundTasks(threadID); count != 0 {
		t.Fatalf("a late frame of the replaced session added live work: %d", count)
	}
}

// A session that ends takes its commands with it, the agents' included.
func TestCodexSessionEndDropsAgentCommands(t *testing.T) {
	t.Parallel()
	r, st, _ := newTestRouter(t)
	const threadID = "thread-agent-session-end"
	createCodexBackgroundTestThread(t, st, threadID)
	r.MarkThreadActive(threadID, "session-a")
	startCodexCommandFromSession(t, r, threadID, "agent-cmd", "1002", "spawn-1", "session-a")
	r.CleanupThread(threadID)
	if count := r.CountLiveCodexBackgroundTasks(threadID); count != 0 {
		t.Fatalf("live work after the session ended = %d, want 0", count)
	}
}

// A live command is memory only: nothing persisted says it runs, so a
// router started over the same store (an app restart) holds none.
func TestCodexAgentCommandDoesNotOutliveTheRouter(t *testing.T) {
	t.Parallel()
	r, st, _ := newTestRouter(t)
	const threadID = "thread-agent-restart"
	createCodexBackgroundTestThread(t, st, threadID)
	r.MarkThreadActive(threadID, "session-a")
	seedOpenTurn(t, r, st, threadID, 0)
	startCodexCommandFromSession(t, r, threadID, "agent-cmd", "1002", "spawn-1", "session-a")
	if _, found, err := st.GetThreadItem(threadID, "agent-cmd"); err != nil || found {
		t.Fatalf("a running command was persisted: found=%v err=%v", found, err)
	}
	if count, err := st.CountLiveRunningBackgroundToolCalls(threadID); err != nil || count != 0 {
		t.Fatalf("store live count = %d (err %v), want 0", count, err)
	}
	restarted := NewRouter(st, func(eventchan.Channel, any) {})
	if count := restarted.CountLiveCodexBackgroundTasks(threadID); count != 0 {
		t.Fatalf("a restarted router counts %d live commands, want 0", count)
	}
}

// A command the host ended before its item/completed arrived (a Stop found
// no process, its agent's thread is gone, a revert cleaned it) is no longer
// live work, but Codex can still write the command's output and completion
// afterwards. They stay that command's: outside a round they write no row,
// and inside one the row keeps the agent that ran the command.
func TestCodexEndedCommandKeepsItsLateCompletion(t *testing.T) {
	t.Parallel()
	r, st, _ := newTestRouter(t)
	const threadID = "thread-ended-late"
	createCodexBackgroundTestThread(t, st, threadID)
	r.MarkThreadActive(threadID, "session-a")
	late := func(itemID, processID, parent string) {
		t.Helper()
		meta := buildUnifiedExecCompleteMeta(t, "completed", processID, "sleep 60", 0)
		for _, evt := range []provider.ProviderEvent{
			{Kind: provider.EventCommandOutput, ThreadID: threadID, ItemID: itemID, ItemType: "commandExecution", ParentToolUseID: parent, Content: "done\n", Replace: true, Meta: meta, Timestamp: time.Now()},
			{Kind: provider.EventToolComplete, ThreadID: threadID, ItemID: itemID, ItemType: "commandExecution", ParentToolUseID: parent, Meta: meta, Timestamp: time.Now()},
		} {
			if err := r.Handle(fromSession(evt, "session-a")); err != nil {
				t.Fatalf("late %s of %s: %v", evt.Kind, itemID, err)
			}
		}
	}

	startCodexCommandFromSession(t, r, threadID, "idle-cmd", "1001", "spawn-1", "session-a")
	if !r.SettleCodexTerminal(threadID, "idle-cmd") {
		t.Fatal("settle of a live command reported nothing removed")
	}
	late("idle-cmd", "1001", "spawn-1")
	if item, found, err := st.GetThreadItem(threadID, "idle-cmd"); err != nil || found {
		t.Fatalf("outside a round the ended command's late completion wrote %+v (err %v)", item, err)
	}

	seedOpenTurn(t, r, st, threadID, 0)
	seedCodexSpawnCard(t, r, st, threadID, "spawn-1", "child-1")
	seedCodexSpawnCard(t, r, st, threadID, "spawn-2", "child-2")
	seedCodexSpawnCard(t, r, st, threadID, "spawn-3", "child-3")
	startCodexCommandFromSession(t, r, threadID, "stopped-cmd", "1002", "spawn-1", "session-a")
	startCodexCommandFromSession(t, r, threadID, "gone-cmd", "1003", "spawn-2", "session-a")
	startCodexCommandFromSession(t, r, threadID, "cleaned-cmd", "1004", "spawn-3", "session-a")
	r.SettleCodexTerminal(threadID, "stopped-cmd")
	r.SettleCodexAgentTerminals(threadID, "spawn-2")
	r.ClearLiveCodexBackgroundTasks(threadID)
	if got := len(r.ListLiveCodexBackgroundTasks(threadID, 0, 0)); got != 0 {
		t.Fatalf("tray rows after the commands ended = %d, want 0", got)
	}
	for _, command := range []struct{ itemID, processID, parent string }{
		{"stopped-cmd", "1002", "spawn-1"},
		{"gone-cmd", "1003", "spawn-2"},
		{"cleaned-cmd", "1004", "spawn-3"},
	} {
		late(command.itemID, command.processID, command.parent)
		item, found, err := st.GetThreadItem(threadID, command.itemID)
		if err != nil || !found {
			t.Fatalf("the late completion of %s wrote no row: found=%v err=%v", command.itemID, found, err)
		}
		if item.ParentID != command.parent || item.Status != statusCompleted {
			t.Fatalf("%s row: parent=%q status=%q, want parent %q completed", command.itemID, item.ParentID, item.Status, command.parent)
		}
	}
	if r.codexEndedCount(threadID) != 0 {
		t.Fatalf("ended commands kept after their completions: %d", r.codexEndedCount(threadID))
	}
}

// Codex can give an ended command's process id to a newer command. The
// ended command's late completion leaves the newer command's process
// correlation alone.
func TestCodexEndedCommandLeavesANewerCommandsProcessID(t *testing.T) {
	t.Parallel()
	r, st, _ := newTestRouter(t)
	const threadID = "thread-ended-pid"
	createCodexBackgroundTestThread(t, st, threadID)
	r.MarkThreadActive(threadID, "session-a")
	startCodexCommandFromSession(t, r, threadID, "old-cmd", "1005", "", "session-a")
	r.SettleCodexTerminal(threadID, "old-cmd")
	startCodexCommandFromSession(t, r, threadID, "new-cmd", "1005", "", "session-a")
	completion := provider.ProviderEvent{Kind: provider.EventToolComplete, ThreadID: threadID, ItemID: "old-cmd", ItemType: "commandExecution", Meta: buildUnifiedExecCompleteMeta(t, "completed", "1005", "sleep 60", 0), Timestamp: time.Now()}
	if err := r.Handle(fromSession(completion, "session-a")); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	indexed := r.codexExecIfPresent(threadID).byProcess[codexProcessKey{"", "1005"}]
	r.mu.Unlock()
	if indexed != "new-cmd" {
		t.Fatalf("process 1005 indexes %q after the ended command completed, want new-cmd", indexed)
	}
}

// Each Codex thread numbers its own processes, so the same process id can
// name a root command and an agent's. Correlation stays within the owner:
// the agent's poll backgrounds the agent's command only, and a stop by the
// bare id refuses to guess.
func TestCodexCommandProcessIDsAreScopedToTheirOwner(t *testing.T) {
	t.Parallel()
	r, st, _ := newTestRouter(t)
	const threadID = "thread-agent-pid"
	createCodexBackgroundTestThread(t, st, threadID)
	r.MarkThreadActive(threadID, "session-a")
	startCodexCommandFromSession(t, r, threadID, "root-cmd", "4242", "", "session-a")
	startCodexCommandFromSession(t, r, threadID, "agent-cmd", "4242", "spawn-1", "session-a")

	poll := provider.ProviderEvent{Kind: provider.EventTerminalInteraction, ThreadID: threadID, ParentToolUseID: "spawn-1", Meta: json.RawMessage(`{"process_id":"4242"}`), Timestamp: time.Now()}
	if err := r.handleTerminalInteraction(fromSession(poll, "session-a")); err != nil {
		t.Fatal(err)
	}
	agent, _ := r.codexTerminalForProcess(threadID, "spawn-1", "4242")
	root, _ := r.codexTerminalForProcess(threadID, "", "4242")
	if !agent.backgrounded || root.backgrounded {
		t.Fatalf("agent backgrounded=%v root backgrounded=%v; the agent's poll names only the agent's process", agent.backgrounded, root.backgrounded)
	}

	if _, _, _, err := r.CodexTerminalForProcess(threadID, "4242"); err == nil || !strings.Contains(err.Error(), "cannot tell them apart") {
		t.Fatalf("stop by a shared process id: err = %v, want a refusal", err)
	}
	launch, owner, ok, err := r.CodexTerminalForProcess(threadID, "9999")
	if err != nil || ok || launch != "" || owner != "" {
		t.Fatalf("unknown process id: launch=%q owner=%q ok=%v err=%v, want not found", launch, owner, ok, err)
	}
	if got := r.CodexTerminalOwners(threadID); len(got) != 1 || got[0] != "spawn-1" {
		t.Fatalf("terminal owners = %v, want [spawn-1]", got)
	}

	if !r.SettleCodexTerminal(threadID, "root-cmd") {
		t.Fatal("settle of a live command reported nothing removed")
	}
	launch, owner, ok, err = r.CodexTerminalForProcess(threadID, "4242")
	if err != nil || !ok || launch != "agent-cmd" || owner != "spawn-1" {
		t.Fatalf("after the root command settled: launch=%q owner=%q ok=%v err=%v, want agent-cmd under spawn-1", launch, owner, ok, err)
	}
	if removed := r.SettleCodexAgentTerminals(threadID, "spawn-1"); removed != 1 {
		t.Fatalf("agent settle removed %d, want 1", removed)
	}
	if count := r.CountLiveCodexBackgroundTasks(threadID); count != 0 {
		t.Fatalf("live work after both settles = %d, want 0", count)
	}
}
