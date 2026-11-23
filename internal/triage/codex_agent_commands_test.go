package triage

import (
	"encoding/json"
	"strings"
	"sync"
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

// A command that left the live work before its item/completed (its thread
// unloaded, a list no longer held it) can still get its output and
// completion from Codex afterwards. They stay that command's: outside a
// round they write no row, and inside one the row keeps the agent that ran
// the command.
func TestCodexEndedCommandKeepsItsLateCompletion(t *testing.T) {
	t.Parallel()
	r, st, _ := newTestRouter(t)
	const threadID = "thread-ended-late"
	createCodexBackgroundTestThread(t, st, threadID)
	r.MarkThreadActive(threadID, "session-a")
	late := func(itemID, processID, parent, scope string) {
		t.Helper()
		meta := buildUnifiedExecCompleteMeta(t, "completed", processID, "sleep 60", 0)
		for _, evt := range []provider.ProviderEvent{
			{Kind: provider.EventCommandOutput, ThreadID: threadID, ItemID: itemID, ItemType: "commandExecution", ParentToolUseID: parent, ProcessScope: threadScope(scope), Content: "done\n", Replace: true, Meta: meta, Timestamp: time.Now()},
			{Kind: provider.EventToolComplete, ThreadID: threadID, ItemID: itemID, ItemType: "commandExecution", ParentToolUseID: parent, ProcessScope: threadScope(scope), Meta: meta, Timestamp: time.Now()},
		} {
			if err := r.Handle(fromSession(evt, "session-a")); err != nil {
				t.Fatalf("late %s of %s: %v", evt.Kind, itemID, err)
			}
		}
	}

	startCodexCommandFromSession(t, r, threadID, "idle-cmd", "1001", "spawn-1", threadScope("child-1"), "session-a")
	unloadCodexScope(t, r, threadID, "spawn-1", "child-1", "session-a")
	late("idle-cmd", "1001", "spawn-1", "child-1")
	if item, found, err := st.GetThreadItem(threadID, "idle-cmd"); err != nil || found {
		t.Fatalf("outside a round the ended command's late completion wrote %+v (err %v)", item, err)
	}

	seedOpenTurn(t, r, st, threadID, 0)
	seedCodexSpawnCard(t, r, st, threadID, "spawn-2", "child-2")
	seedCodexSpawnCard(t, r, st, threadID, "spawn-3", "child-3")
	startCodexCommandFromSession(t, r, threadID, "gone-cmd", "1003", "spawn-2", threadScope("child-2"), "session-a")
	startCodexCommandFromSession(t, r, threadID, "unlisted-cmd", "1004", "spawn-3", threadScope("child-3"), "session-a")
	unloadCodexScope(t, r, threadID, "spawn-2", "child-2", "session-a")
	check, ok := r.CodexScopeCheckFor(threadID, "child-3")
	if !ok {
		t.Fatal("no check for a scope with a live command")
	}
	if ended := r.ConfirmCodexScope(threadID, check, nil, false); len(ended) != 1 || ended[0] != "unlisted-cmd" {
		t.Fatalf("confirm of an empty list ended %v, want [unlisted-cmd]", ended)
	}
	if got := len(r.ListLiveCodexBackgroundTasks(threadID, 0, 0)); got != 0 {
		t.Fatalf("tray rows after the commands ended = %d, want 0", got)
	}
	for _, command := range []struct{ itemID, processID, parent, scope string }{
		{"gone-cmd", "1003", "spawn-2", "child-2"},
		{"unlisted-cmd", "1004", "spawn-3", "child-3"},
	} {
		late(command.itemID, command.processID, command.parent, command.scope)
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

// An ended command keeps what its history row needs and nothing more: no
// item meta and none of the output it streamed while live, only the
// completion's own aggregated output, which Codex writes as one replace
// just before the completion.
func TestCodexEndedCommandKeepsOnlyItsCompletionOutput(t *testing.T) {
	t.Parallel()
	r, st, _ := newTestRouter(t)
	const threadID = "thread-ended-size"
	createCodexBackgroundTestThread(t, st, threadID)
	r.MarkThreadActive(threadID, "session-a")
	startCodexCommandFromSession(t, r, threadID, "big-cmd", "1001", "spawn-1", threadScope("child-1"), "session-a")
	output := func(content string, replace bool) {
		t.Helper()
		evt := provider.ProviderEvent{Kind: provider.EventCommandOutput, ThreadID: threadID, ItemID: "big-cmd", ItemType: "commandExecution", ParentToolUseID: "spawn-1", ProcessScope: threadScope("child-1"), Content: content, Replace: replace, Timestamp: time.Now()}
		if !r.observeCodexCommandOutput(fromSession(evt, "session-a")) {
			t.Fatal("command output was not taken as the command's")
		}
	}
	output(strings.Repeat("x", 64*1024), false)
	unloadCodexScope(t, r, threadID, "spawn-1", "child-1", "session-a")

	ended := func() endedCodexCommand {
		t.Helper()
		r.mu.Lock()
		defer r.mu.Unlock()
		record := r.codexExecIfPresent(threadID).endedFor("big-cmd")
		if record == nil {
			t.Fatal("the ended command kept no record")
		}
		return *record
	}
	if record := ended(); !record.output.Empty() || record.parentID != "spawn-1" || record.summary == "" {
		t.Fatalf("ended record = %+v, want no output, parent spawn-1 and the summary", record)
	}
	output("streamed after the end\n", false)
	if record := ended(); !record.output.Empty() {
		t.Fatalf("ended record took streamed output: %q", record.output.String())
	}
	output("final\n", true)
	if record := ended(); record.output.String() != "final\n" {
		t.Fatalf("ended record output = %q, want the completion's replace", record.output.String())
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
	startCodexCommandFromSession(t, r, threadID, "old-cmd", "1005", "", threadScope("root"), "session-a")
	check, _ := r.CodexScopeCheckFor(threadID, "root")
	r.ConfirmCodexScope(threadID, check, nil, false)
	startCodexCommandFromSession(t, r, threadID, "new-cmd", "1005", "", threadScope("root"), "session-a")
	completion := provider.ProviderEvent{Kind: provider.EventToolComplete, ThreadID: threadID, ItemID: "old-cmd", ItemType: "commandExecution", ProcessScope: threadScope("root"), Meta: buildUnifiedExecCompleteMeta(t, "completed", "1005", "sleep 60", 0), Timestamp: time.Now()}
	if err := r.Handle(fromSession(completion, "session-a")); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	indexed := r.codexExecIfPresent(threadID).byProcess[codexProcessKey{"root", "1005"}]
	r.mu.Unlock()
	if indexed != "new-cmd" {
		t.Fatalf("process 1005 indexes %q after the ended command completed, want new-cmd", indexed)
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
	check, _ := r.CodexScopeCheckFor(threadID, "root")
	r.ConfirmCodexScope(threadID, check, nil, false)
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
	if _, ok := r.CodexScopeCheckFor(threadID, review.ID); ok {
		t.Fatal("a check was offered for a table no request reaches")
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

// A scope check is pinned to the session that asked and to the commands
// that had started when it asked. A command the answer does not hold ends
// only when the check covers it: Codex answers for its table as it is when
// it reads the request, which says nothing of a later command. A listed
// command stays, a thread that is not loaded ends every covered command,
// and an answer asked of a replaced session changes nothing.
func TestCodexScopeCheckEndsOnlyTheCommandsItCovers(t *testing.T) {
	t.Parallel()
	r, st, _ := newTestRouter(t)
	const threadID = "thread-scope-check"
	createCodexBackgroundTestThread(t, st, threadID)
	r.MarkThreadActive(threadID, "session-a")
	startCodexCommandFromSession(t, r, threadID, "listed-cmd", "1001", "spawn-1", threadScope("child-1"), "session-a")
	startCodexCommandFromSession(t, r, threadID, "gone-cmd", "1002", "spawn-1", threadScope("child-1"), "session-a")
	startCodexCommandFromSession(t, r, threadID, "other-cmd", "1003", "", threadScope("root"), "session-a")
	check, ok := r.CodexScopeCheckFor(threadID, "child-1")
	if !ok {
		t.Fatal("no check for a scope with live commands")
	}
	startCodexCommandFromSession(t, r, threadID, "newer-cmd", "1004", "spawn-1", threadScope("child-1"), "session-a")

	ended := r.ConfirmCodexScope(threadID, check, []string{"listed-cmd"}, false)
	if len(ended) != 1 || ended[0] != "gone-cmd" {
		t.Fatalf("confirm ended %v, want [gone-cmd]", ended)
	}
	for _, id := range []string{"listed-cmd", "newer-cmd", "other-cmd"} {
		if !codexCommandTracked(r, threadID, id) {
			t.Fatalf("%s left the live work; the answer holds it, did not cover it, or is another table's", id)
		}
	}

	stale := check
	stale.Token = "session-old"
	if ended := r.ConfirmCodexScope(threadID, stale, nil, true); len(ended) != 0 {
		t.Fatalf("an answer for a replaced session ended %v", ended)
	}
	if ended := r.ConfirmCodexScope(threadID, check, nil, true); len(ended) != 1 || ended[0] != "listed-cmd" {
		t.Fatalf("not loaded ended %v, want [listed-cmd]: newer-cmd started after the check", ended)
	}
}

// A close item is not proof the agent's thread is gone (Codex writes it
// with the agent's previous status when the close fails), so it ends no
// command. For each named thread that runs commands, the router asks for a
// check of that thread, pinned to now; one that runs none is not checked.
func TestCodexCloseItemAsksForACheckOfEachClosedThread(t *testing.T) {
	t.Parallel()
	r, st, _ := newTestRouter(t)
	const threadID = "thread-close-check"
	createCodexBackgroundTestThread(t, st, threadID)
	r.MarkThreadActive(threadID, "session-a")
	seedOpenTurn(t, r, st, threadID, 0)
	var (
		mu     sync.Mutex
		checks []CodexScopeCheck
	)
	r.SetCodexScopeCheckHook(func(id string, check CodexScopeCheck) {
		mu.Lock()
		defer mu.Unlock()
		if id != threadID {
			t.Errorf("check asked for thread %s, want %s", id, threadID)
		}
		checks = append(checks, check)
	})
	startCodexCommandFromSession(t, r, threadID, "agent-cmd", "1001", "spawn-1", threadScope("child-1"), "session-a")

	closeMeta, err := json.Marshal(map[string]any{"toolName": "close_agent", "item_status": "completed", "input": map[string]any{"tool": "close_agent", "receiverThreadIds": []string{"child-1", "child-2"}}})
	if err != nil {
		t.Fatal(err)
	}
	closeItem := provider.ProviderEvent{Kind: provider.EventToolComplete, ThreadID: threadID, ItemID: "close-1", ItemType: "close_agent", Meta: closeMeta, Timestamp: time.Now()}
	if err := r.Handle(fromSession(closeItem, "session-a")); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := append([]CodexScopeCheck(nil), checks...)
	mu.Unlock()
	if len(got) != 1 || got[0].ScopeID != "child-1" || got[0].Token != "session-a" || got[0].Through == 0 {
		t.Fatalf("checks = %+v, want one pinned check of child-1", got)
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
