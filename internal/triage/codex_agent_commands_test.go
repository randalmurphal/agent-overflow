package triage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
)

// threadScope is the process scope of a Codex thread's frames.
func threadScope(id string) provider.ProcessScope {
	return provider.ProcessScope{ID: id, Addressable: true}
}

// startCodexCommandFromSession starts a unified-exec command as session
// token's frame, displayed under parent ("" for the root thread) and run in
// the process table scope names.
func startCodexCommandFromSession(t *testing.T, r *Router, threadID, itemID, processID, parent string, scope provider.ProcessScope, token string) {
	t.Helper()
	start := provider.ProviderEvent{Kind: provider.EventToolStart, ThreadID: threadID, ItemID: itemID, ItemType: "commandExecution", ParentToolUseID: parent, ProcessScope: scope, Meta: buildUnifiedExecStartMeta(t, processID, "sleep 60"), Timestamp: time.Now()}
	if !r.observeCodexToolStart(fromSession(start, token)) {
		t.Fatalf("command %s start was not taken as a live command", itemID)
	}
}

func codexCommandTracked(r *Router, threadID, itemID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.codexExecIfPresent(threadID).lookup(itemID) != nil
}

// codexCommandStateHeld counts what the thread keeps of Codex commands.
func (r *Router) codexCommandStateHeld(threadID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.threadStateIfPresent(threadID)
	if st == nil {
		return 0
	}
	return len(st.codexExec.byLaunch) + len(st.codexExec.byProcess)
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

// unloadCodexScope delivers the notLoaded status that says scope's process
// table ended: an agent thread's, on its spawn launch.
func unloadCodexScope(t *testing.T, r *Router, threadID, launch, scope, token string) {
	t.Helper()
	evt := provider.ProviderEvent{Kind: provider.EventSubagentStatus, ThreadID: threadID, ItemID: launch, ParentToolUseID: launch, ProcessScope: threadScope(scope), Meta: json.RawMessage(`{"agent_path":"` + scope + `","status":"notLoaded"}`), Timestamp: time.Now()}
	if err := r.observeCodexSubagentStatus(fromSession(evt, token)); err != nil {
		t.Fatalf("notLoaded for %s: %v", scope, err)
	}
}

// A thread Codex unloads has no processes left, so the commands its table
// holds leave the live work with it, and only those: one spawn can start
// several agent threads, whose commands are all displayed under that
// spawn, and an agent's turn ending, however it ends, removes nothing.
func TestCodexAgentCommandsEndWhenTheirThreadUnloads(t *testing.T) {
	t.Parallel()
	r, st, emissions := newTestRouter(t)
	const threadID = "thread-agent-unload"
	createCodexBackgroundTestThread(t, st, threadID)
	r.MarkThreadActive(threadID, "session-a")
	seedOpenTurn(t, r, st, threadID, 0)
	seedCodexSpawnCard(t, r, st, threadID, "spawn-1", "child-1a")
	startCodexCommandFromSession(t, r, threadID, "root-cmd", "1001", "", threadScope("root"), "session-a")
	startCodexCommandFromSession(t, r, threadID, "agent-1a-cmd", "1002", "spawn-1", threadScope("child-1a"), "session-a")
	startCodexCommandFromSession(t, r, threadID, "agent-1b-cmd", "1003", "spawn-1", threadScope("child-1b"), "session-a")

	status := func(value, turnID string) {
		t.Helper()
		evt := provider.ProviderEvent{Kind: provider.EventSubagentStatus, ThreadID: threadID, ItemID: "spawn-1", ParentToolUseID: "spawn-1", TurnID: turnID, ProcessScope: threadScope("child-1a"), Meta: json.RawMessage(`{"agent_path":"child-1a","status":"` + value + `"}`), Timestamp: time.Now()}
		if err := r.observeCodexSubagentStatus(fromSession(evt, "session-a")); err != nil {
			t.Fatalf("status %s: %v", value, err)
		}
	}
	for _, ended := range []string{"completed", "errored", "interrupted", "idle"} {
		status("running", "child-turn-"+ended)
		status(ended, "child-turn-"+ended)
	}
	for _, id := range []string{"root-cmd", "agent-1a-cmd", "agent-1b-cmd"} {
		if !codexCommandTracked(r, threadID, id) {
			t.Fatalf("command %s left the live work when an agent's turns ended", id)
		}
	}

	emissions.reset()
	unloadCodexScope(t, r, threadID, "spawn-1", "child-1a", "session-a")
	if codexCommandTracked(r, threadID, "agent-1a-cmd") {
		t.Fatal("the unloaded thread's command is still live")
	}
	if !codexCommandTracked(r, threadID, "agent-1b-cmd") || !codexCommandTracked(r, threadID, "root-cmd") {
		t.Fatal("unloading one agent thread ended commands another thread runs")
	}
	if got := len(r.ListLiveCodexBackgroundTasks(threadID, 0, 0)); got != 2 {
		t.Fatalf("tray rows = %d, want 2", got)
	}
	if trayRefreshes(emissions, threadID) == 0 {
		t.Fatal("no tray refresh announced the removed command")
	}
	// A retired session's unload ends nothing the current one runs.
	unloadCodexScope(t, r, threadID, "spawn-1", "child-1b", "session-old")
	if !codexCommandTracked(r, threadID, "agent-1b-cmd") {
		t.Fatal("a retired session's notLoaded ended a live command")
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
	startCodexCommandFromSession(t, r, threadID, "root-cmd", "1001", "", threadScope("root"), "session-a")
	startCodexCommandFromSession(t, r, threadID, "agent-cmd", "1002", "spawn-1", threadScope("child-1"), "session-a")
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
	late := provider.ProviderEvent{Kind: provider.EventToolStart, ThreadID: threadID, ItemID: "late-cmd", ItemType: "commandExecution", ParentToolUseID: "spawn-1", ProcessScope: threadScope("child-1"), Meta: buildUnifiedExecStartMeta(t, "1003", "sleep 60"), Timestamp: time.Now()}
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
	startCodexCommandFromSession(t, r, threadID, "agent-cmd", "1002", "spawn-1", threadScope("child-1"), "session-a")
	if count := r.CountLiveCodexBackgroundTasks(threadID); count != 1 {
		t.Fatalf("live work before the session ended = %d, want the agent's command", count)
	}
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
	startCodexCommandFromSession(t, r, threadID, "agent-cmd", "1002", "spawn-1", threadScope("child-1"), "session-a")
	if count := r.CountLiveCodexBackgroundTasks(threadID); count != 1 {
		t.Fatalf("live work before the restart = %d, want the agent's command", count)
	}
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

// A command whose thread unloaded leaves nothing behind: Codex can still
// write its completion afterwards, and that completion is handled like any
// untracked one.
func TestCodexUnloadedThreadKeepsNothingOfItsCommands(t *testing.T) {
	t.Parallel()
	r, st, _ := newTestRouter(t)
	const threadID = "thread-unload-nothing"
	createCodexBackgroundTestThread(t, st, threadID)
	r.MarkThreadActive(threadID, "session-a")
	startCodexCommandFromSession(t, r, threadID, "agent-cmd", "1001", "spawn-1", threadScope("child-1"), "session-a")
	unloadCodexScope(t, r, threadID, "spawn-1", "child-1", "session-a")
	if held := r.codexCommandStateHeld(threadID); held != 0 {
		t.Fatalf("the unloaded thread's command left %d entries behind", held)
	}
	meta := buildUnifiedExecCompleteMeta(t, "completed", "1001", "sleep 60", 0)
	late := provider.ProviderEvent{Kind: provider.EventToolComplete, ThreadID: threadID, ItemID: "agent-cmd", ItemType: "commandExecution", ParentToolUseID: "spawn-1", ProcessScope: threadScope("child-1"), Meta: meta, Timestamp: time.Now()}
	if err := r.Handle(fromSession(late, "session-a")); err != nil {
		t.Fatal(err)
	}
	if held := r.codexCommandStateHeld(threadID); held != 0 {
		t.Fatalf("the late completion left %d entries behind", held)
	}
}

// Codex gives a process id to a newer command once the older command's
// process has left the table, which can be before the older command's
// completion arrives. The older command's completion leaves the newer
// command's process correlation alone, and only the newer command holds the
// process a stop would terminate (CodexCommandHoldsProcess).
func TestCodexReusedProcessIDBelongsToTheNewerCommand(t *testing.T) {
	t.Parallel()
	r, st, _ := newTestRouter(t)
	const threadID = "thread-reused-pid"
	createCodexBackgroundTestThread(t, st, threadID)
	r.MarkThreadActive(threadID, "session-a")
	startCodexCommandFromSession(t, r, threadID, "old-cmd", "1005", "", threadScope("root"), "session-a")
	startCodexCommandFromSession(t, r, threadID, "new-cmd", "1005", "", threadScope("root"), "session-a")
	old, _ := r.LiveCodexCommand(threadID, "old-cmd")
	newer, _ := r.LiveCodexCommand(threadID, "new-cmd")
	if r.CodexCommandHoldsProcess(threadID, old) || !r.CodexCommandHoldsProcess(threadID, newer) {
		t.Fatal("process 1005 must be held by the newer command only")
	}
	completion := provider.ProviderEvent{Kind: provider.EventToolComplete, ThreadID: threadID, ItemID: "old-cmd", ItemType: "commandExecution", ProcessScope: threadScope("root"), Meta: buildUnifiedExecCompleteMeta(t, "completed", "1005", "sleep 60", 0), Timestamp: time.Now()}
	if err := r.Handle(fromSession(completion, "session-a")); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	indexed := r.codexExecIfPresent(threadID).byProcess[codexProcessKey{"root", "1005"}]
	r.mu.Unlock()
	if indexed != "new-cmd" || !r.CodexCommandHoldsProcess(threadID, newer) {
		t.Fatalf("process 1005 indexes %q after the older command completed, want new-cmd", indexed)
	}
}

// A command read for a stop names its process only while the session that
// read it runs the thread: a replacement's command with the same id in the
// same table is not the read one's.
func TestCodexCommandHoldsItsProcessOnlyInItsSession(t *testing.T) {
	t.Parallel()
	r, st, _ := newTestRouter(t)
	const threadID = "thread-holder-session"
	createCodexBackgroundTestThread(t, st, threadID)
	r.MarkThreadActive(threadID, "session-a")
	startCodexCommandFromSession(t, r, threadID, "cmd", "1001", "", threadScope("root"), "session-a")
	read, ok := r.LiveCodexCommand(threadID, "cmd")
	if !ok || read.Token != "session-a" || !r.CodexCommandHoldsProcess(threadID, read) {
		t.Fatalf("read command = %+v (ok %v), want session-a's holder of process 1001", read, ok)
	}
	r.MarkThreadActive(threadID, "session-b")
	startCodexCommandFromSession(t, r, threadID, "cmd", "1001", "", threadScope("root"), "session-b")
	if r.CodexCommandHoldsProcess(threadID, read) {
		t.Fatal("a command read in the replaced session holds the replacement's process")
	}
}

// Each Codex thread numbers its own processes, so the same process id can
// name a root command and an agent's. Correlation stays within the process
// table: the agent's poll backgrounds the agent's command only. A stop by
// item id reaches each command in its own table, and a stop by process id,
// which only an older window sends, names only a root command.
func TestCodexCommandProcessIDsAreScopedToTheirTable(t *testing.T) {
	t.Parallel()
	r, st, _ := newTestRouter(t)
	const threadID = "thread-agent-pid"
	createCodexBackgroundTestThread(t, st, threadID)
	r.MarkThreadActive(threadID, "session-a")
	startCodexCommandFromSession(t, r, threadID, "root-cmd", "4242", "", threadScope("root"), "session-a")
	startCodexCommandFromSession(t, r, threadID, "agent-cmd", "4242", "spawn-1", threadScope("child-1"), "session-a")

	poll := provider.ProviderEvent{Kind: provider.EventTerminalInteraction, ThreadID: threadID, ParentToolUseID: "spawn-1", ProcessScope: threadScope("child-1"), Meta: json.RawMessage(`{"process_id":"4242"}`), Timestamp: time.Now()}
	if err := r.handleTerminalInteraction(fromSession(poll, "session-a")); err != nil {
		t.Fatal(err)
	}
	agent, _ := r.codexTerminalForProcess(threadID, "child-1", "4242")
	root, _ := r.codexTerminalForProcess(threadID, "root", "4242")
	if !agent.backgrounded || root.backgrounded {
		t.Fatalf("agent backgrounded=%v root backgrounded=%v; the agent's poll names only the agent's process", agent.backgrounded, root.backgrounded)
	}

	for _, want := range []struct{ launch, scope string }{{"root-cmd", "root"}, {"agent-cmd", "child-1"}} {
		command, ok := r.LiveCodexCommand(threadID, want.launch)
		if !ok || command.ScopeID != want.scope || command.ProcessID != "4242" || !command.Addressable {
			t.Fatalf("stop target of %s = %+v (ok %v), want process 4242 in %s", want.launch, command, ok, want.scope)
		}
	}
	if command, ok := r.LiveRootCodexCommandForProcess(threadID, "4242"); !ok || command.LaunchID != "root-cmd" {
		t.Fatalf("stop by process 4242 = %+v (ok %v), want the root command", command, ok)
	}
	rootDone := provider.ProviderEvent{Kind: provider.EventToolComplete, ThreadID: threadID, ItemID: "root-cmd", ItemType: "commandExecution", ProcessScope: threadScope("root"), Meta: buildUnifiedExecCompleteMeta(t, "completed", "4242", "sleep 60", 0), Timestamp: time.Now()}
	if err := r.Handle(fromSession(rootDone, "session-a")); err != nil {
		t.Fatal(err)
	}
	if command, ok := r.LiveRootCodexCommandForProcess(threadID, "4242"); ok {
		t.Fatalf("stop by process 4242 with only the agent's command live = %+v, want none: an agent's id names nothing in the root table", command)
	}
}

// The tray row says how its Stop names the command: by item id when the
// command's table takes requests and its process is named, and by process
// id too for a root command, which is the only one a window older than the
// item-id Stop can reach.
func TestCodexCommandTrayRowsSayHowTheyStop(t *testing.T) {
	t.Parallel()
	r, st, _ := newTestRouter(t)
	const threadID = "thread-row-stop"
	createCodexBackgroundTestThread(t, st, threadID)
	r.MarkThreadActive(threadID, "session-a")
	startCodexCommandFromSession(t, r, threadID, "root-cmd", "1001", "", threadScope("root"), "session-a")
	startCodexCommandFromSession(t, r, threadID, "agent-cmd", "1002", "spawn-1", threadScope("child-1"), "session-a")
	startCodexCommandFromSession(t, r, threadID, "unnamed-cmd", "", "spawn-1", threadScope("child-1"), "session-a")
	type rowMeta struct {
		Stop      bool   `json:"stop_by_item"`
		ProcessID string `json:"process_id"`
	}
	want := map[string]rowMeta{
		"root-cmd":    {Stop: true, ProcessID: "1001"},
		"agent-cmd":   {Stop: true},
		"unnamed-cmd": {},
	}
	rows := r.ListLiveCodexBackgroundTasks(threadID, 0, 0)
	if len(rows) != len(want) {
		t.Fatalf("tray rows = %d, want %d", len(rows), len(want))
	}
	for _, row := range rows {
		var got rowMeta
		if err := json.Unmarshal([]byte(row.Meta), &got); err != nil {
			t.Fatal(err)
		}
		if got != want[row.ID] || CodexCommandStopsByItem(row) != want[row.ID].Stop {
			t.Fatalf("row %s meta = %+v, want %+v", row.ID, got, want[row.ID])
		}
	}
}

// A code review's commands run in the review delegate's process table,
// though their frames come on the root thread's. No request reaches that
// table, so the rows have no Stop, and the commands end with the review.
func TestCodexReviewCommandsEndWithTheirRun(t *testing.T) {
	t.Parallel()
	r, st, _ := newTestRouter(t)
	const threadID = "thread-review-cmd"
	createCodexBackgroundTestThread(t, st, threadID)
	r.MarkThreadActive(threadID, "session-a")
	review := provider.ProcessScope{ID: "codex-review-run:review-1"}
	startCodexCommandFromSession(t, r, threadID, "review-cmd", "1001", "review-1", review, "session-a")
	startCodexCommandFromSession(t, r, threadID, "root-cmd", "1001", "", threadScope("root"), "session-a")

	command, ok := r.LiveCodexCommand(threadID, "review-cmd")
	if !ok || command.Addressable {
		t.Fatalf("review command = %+v (ok %v), want live and not addressable", command, ok)
	}
	for _, row := range r.ListLiveCodexBackgroundTasks(threadID, 0, 0) {
		if row.ID == "review-cmd" && (CodexCommandStopsByItem(row) || strings.Contains(row.Meta, "process_id")) {
			t.Fatalf("review row meta %s offers a Stop", row.Meta)
		}
	}

	ended := provider.ProviderEvent{Kind: provider.EventSubagentStatus, ThreadID: threadID, ItemID: "review-1", ParentToolUseID: "review-1", ProcessScope: review, Meta: json.RawMessage(`{"agent_path":"` + review.ID + `","status":"notLoaded"}`), Timestamp: time.Now()}
	if err := r.Handle(fromSession(ended, "session-a")); err != nil {
		t.Fatal(err)
	}
	if codexCommandTracked(r, threadID, "review-cmd") || !codexCommandTracked(r, threadID, "root-cmd") {
		t.Fatal("the review's end must end its commands and only those")
	}
}

// A close item is not proof the agent's thread is gone (Codex writes it
// with the agent's previous status when the close fails), so it ends no
// command. The thread's unload, which the provider reports, ends them.
func TestCodexCloseItemEndsNoCommand(t *testing.T) {
	t.Parallel()
	r, st, _ := newTestRouter(t)
	const threadID = "thread-close-item"
	createCodexBackgroundTestThread(t, st, threadID)
	r.MarkThreadActive(threadID, "session-a")
	seedOpenTurn(t, r, st, threadID, 0)
	startCodexCommandFromSession(t, r, threadID, "agent-cmd", "1001", "spawn-1", threadScope("child-1"), "session-a")

	closeMeta, err := json.Marshal(map[string]any{"toolName": "close_agent", "item_status": "completed", "input": map[string]any{"tool": "close_agent", "receiverThreadIds": []string{"child-1"}}})
	if err != nil {
		t.Fatal(err)
	}
	closeItem := provider.ProviderEvent{Kind: provider.EventToolComplete, ThreadID: threadID, ItemID: "close-1", ItemType: "close_agent", Meta: closeMeta, Timestamp: time.Now()}
	if err := r.Handle(fromSession(closeItem, "session-a")); err != nil {
		t.Fatal(err)
	}
	if !codexCommandTracked(r, threadID, "agent-cmd") {
		t.Fatal("the close item ended a command")
	}
}

// A revert forgets the agent completions held for an answer; the commands
// stay live until each leaves by its own way.
func TestCodexDropHeldCompletionsLeavesCommandsLive(t *testing.T) {
	t.Parallel()
	r, st, _ := newTestRouter(t)
	const threadID = "thread-drop-held"
	createCodexBackgroundTestThread(t, st, threadID)
	r.MarkThreadActive(threadID, "session-a")
	startCodexCommandFromSession(t, r, threadID, "root-cmd", "1001", "", threadScope("root"), "session-a")
	r.DropHeldCodexCompletions(threadID)
	if count := r.CountLiveCodexBackgroundTasks(threadID); count != 1 {
		t.Fatalf("live work after the drop = %d, want the command", count)
	}
}

// A waiter for a thread's commands to leave wakes at that thread's change,
// and not at another thread's.
func TestCodexExecChangedWakesOnTheThreadsChange(t *testing.T) {
	t.Parallel()
	r, st, _ := newTestRouter(t)
	createCodexBackgroundTestThread(t, st, "thread-wait-a")
	createCodexBackgroundTestThread(t, st, "thread-wait-b")
	r.MarkThreadActive("thread-wait-a", "session-a")
	r.MarkThreadActive("thread-wait-b", "session-b")
	changed := r.CodexExecChanged("thread-wait-a")
	startCodexCommandFromSession(t, r, "thread-wait-b", "b-cmd", "1001", "", threadScope("root-b"), "session-b")
	select {
	case <-changed:
		t.Fatal("another thread's change woke the waiter")
	default:
	}
	startCodexCommandFromSession(t, r, "thread-wait-a", "a-cmd", "1001", "", threadScope("root-a"), "session-a")
	select {
	case <-changed:
	default:
		t.Fatal("the thread's change did not wake the waiter")
	}
}
