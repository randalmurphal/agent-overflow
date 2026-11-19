package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
)

// sessionShape is what a session started on a thread now would get of
// spawning, read the way startSessionNow reads it: the thread's place, the
// thread tools its registration lists, and the native subagent tools each
// provider's configuration turns off.
type sessionShape struct {
	level        int
	threadTools  bool
	spawns       bool
	nativeClosed map[string]bool
}

func sessionShapeOf(t *testing.T, app *App, threadID string) sessionShape {
	t.Helper()
	thread, err := app.store.GetThread(threadID)
	if err != nil {
		t.Fatal(err)
	}
	place, err := app.subThreadPlaceOf(threadID)
	if err != nil {
		t.Fatal(err)
	}
	shape := sessionShape{level: place.level, nativeClosed: map[string]bool{}}
	servers, err := app.threadMCPConfigForThread(thread, "shape-"+threadID, place)
	if err != nil {
		t.Fatal(err)
	}
	if entry, ok := servers[threadMCPName].(map[string]any); ok {
		status, reply := remoteMCPRequest(t, entry["url"].(string), "tools/list", map[string]any{})
		var listing struct {
			Tools []map[string]any `json:"tools"`
		}
		if err := json.Unmarshal(reply["result"], &listing); err != nil || status != 200 {
			t.Fatalf("tools/list for %s: %d %s %v", threadID, status, reply, err)
		}
		names := toolNames(listing.Tools)
		shape.threadTools = slices.Contains(names, "thread_send")
		shape.spawns = slices.Contains(names, "thread_spawn")
	}
	for _, name := range []string{string(provider.Claude), string(provider.ClaudeTUI), string(provider.Codex)} {
		var opts provider.SessionOptions
		withoutNativeSubagents(&opts, name, place)
		switch name {
		case string(provider.Codex):
			shape.nativeClosed[name] = codex.DisabledToolConfigOverrides(opts.DisabledTools)["agents.enabled"] == false
		default:
			shape.nativeClosed[name] = slices.Contains(opts.DisabledTools, "Agent") && slices.Contains(opts.DisabledTools, "Task")
		}
	}
	return shape
}

// Every way a session comes to exist, with the sub-thread limit at 1, and
// the level, thread tools and native subagents each gets on every provider.
// An agent's delegation (an owned spawn, a fork it spawns, an assistant's
// helper, an ask copy) continues its creator's chain; a thread the person
// talks to, one with a sidebar row, an assistant, its next thread and a
// person's side chat start their own; a workflow phase has no thread tools
// and keeps its native subagents; a send starts a thread at its own level.
func TestEveryWayASessionComesToBeGetsItsLevel(t *testing.T) {
	f := newRequestFixture(t)
	t.Cleanup(func() { _ = f.app.threadMCPServer().Close() })
	if _, err := f.app.settings.Update(map[string]any{
		"subThreadLevels":  1,
		"claudeBinaryPath": writeArgvRecordingClaude(t, filepath.Join(t.TempDir(), "argv.txt")),
	}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	adapter := f.adapter()
	lead := f.callerIdentity()
	spawn := func(caller threadtools.Caller, call threadtools.SpawnCall) string {
		t.Helper()
		call.Prompt = "work"
		ack, err := adapter.Spawn(ctx, caller, call)
		if err != nil || ack.ThreadID == "" {
			t.Fatalf("spawn %+v: %+v, %v", call, ack, err)
		}
		return ack.ThreadID
	}
	other := f.forkableThread(t, "consulted-session")

	worker := spawn(lead, threadtools.SpawnCall{})
	sidebar := spawn(lead, threadtools.SpawnCall{Placement: threadtools.PlacementStandalone})
	fork := spawn(lead, threadtools.SpawnCall{FromThread: other.ID})
	askAck, err := adapter.Ask(ctx, threadtools.Caller{ThreadID: worker}, threadtools.AskCall{ThreadID: other.ID, Question: "why?"})
	if err != nil || askAck.ThreadID == "" {
		t.Fatalf("ask from the worker: %+v, %v", askAck, err)
	}
	sideChat, err := f.app.ForkSideChat(ctx, worker)
	if err != nil {
		t.Fatal(err)
	}

	assistantThread, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatal(err)
	}
	testAssistant(t, f.app, assistantThread.ID)
	assistant := threadtools.Caller{ThreadID: assistantThread.ID}
	helper := spawn(assistant, threadtools.SpawnCall{})
	created, err := adapter.NewAssistant(ctx, assistant, threadtools.NewAssistantCall{Name: "Second", Role: "Keeps notes."})
	if err != nil {
		t.Fatalf("thread_new_assistant: %v", err)
	}
	successor, err := f.app.StartNextAssistantThread(ctx, assistantThread.ID)
	if err != nil {
		t.Fatalf("next assistant thread: %v", err)
	}

	// The workflow engine creates its phase threads in the store; its
	// session scope comes from the run (derivePhaseScope).
	phase := store.Thread{
		ID: "phase-thread", ProjectID: f.caller.ProjectID, ProjectPath: f.caller.ProjectPath, WorkspacePath: f.caller.WorkspacePath,
		Provider: string(provider.Claude), Model: "claude-opus-4-7", Mode: threadmode.ModeWorkflow,
	}
	if err := f.app.store.CreateThread(phase); err != nil {
		t.Fatal(err)
	}

	if err := f.app.StopSession(worker); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Send(ctx, lead, threadtools.SendCall{ThreadID: worker, Message: "more"}); err != nil {
		t.Fatalf("send to the stopped worker: %v", err)
	}

	type want struct {
		level        int
		threadTools  bool
		spawns       bool
		nativeClosed bool
	}
	cases := []struct {
		name   string
		thread string
		want   want
	}{
		{"the thread the person talks to", f.caller.ID, want{0, true, true, false}},
		{"an owned spawn, restarted by a send", worker, want{1, true, false, true}},
		{"a spawn with a sidebar row", sidebar, want{0, true, true, false}},
		{"a fork from_thread spawns", fork, want{1, true, false, true}},
		{"an agent's ask copy", askAck.ThreadID, want{2, true, false, true}},
		{"a person's side chat", sideChat.ID, want{0, true, true, false}},
		{"an assistant's helper", helper, want{1, true, false, true}},
		{"a new assistant", created.ThreadID, want{0, true, true, false}},
		{"an assistant's next thread", successor.ID, want{0, true, true, false}},
		{"a workflow phase", phase.ID, want{0, false, false, false}},
	}
	for _, tc := range cases {
		got := sessionShapeOf(t, f.app, tc.thread)
		if got.level != tc.want.level || got.threadTools != tc.want.threadTools || got.spawns != tc.want.spawns {
			t.Errorf("%s: level %d, thread tools %v, thread_spawn %v; want %+v", tc.name, got.level, got.threadTools, got.spawns, tc.want)
		}
		for providerName, closed := range got.nativeClosed {
			if closed != tc.want.nativeClosed {
				t.Errorf("%s: %s native subagents off = %v, want %v", tc.name, providerName, closed, tc.want.nativeClosed)
			}
		}
	}
}

// End to end on Claude: the copy a worker at the limit asks through starts
// with Agent and Task disallowed and a thread tools registration with no
// thread_spawn.
func TestStartSession_AnAskCopyFromTheLimitStartsWithoutSpawning(t *testing.T) {
	f := newRequestFixture(t)
	t.Cleanup(func() { _ = f.app.threadMCPServer().Close() })
	argvPath := filepath.Join(t.TempDir(), "argv.txt")
	if _, err := f.app.settings.Update(map[string]any{
		"subThreadLevels":  1,
		"claudeBinaryPath": writeArgvRecordingClaude(t, argvPath),
	}); err != nil {
		t.Fatal(err)
	}
	worker, err := createTestThread(t, f.app, string(provider.Claude), f.caller.WorkspacePath, "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatal(err)
	}
	ownSpawn(t, f.app, "spawn-worker", f.caller.ID, worker.ID)
	other := f.forkableThread(t, "consulted-session")
	ack, err := f.adapter().Ask(context.Background(), threadtools.Caller{ThreadID: worker.ID}, threadtools.AskCall{ThreadID: other.ID, Question: "why?"})
	if err != nil {
		t.Fatal(err)
	}
	if copyThread, err := f.app.store.GetThread(ack.ThreadID); err != nil || !copyThread.AskCopy {
		t.Fatalf("the ask's thread = %+v, %v", copyThread, err)
	}
	argv := spawnedClaudeArgv(t, argvPath)
	for _, tool := range []string{"Agent", "Task"} {
		if !hasArgPair(argv, "--disallowedTools", tool) {
			t.Errorf("the ask copy's argv does not disallow %s: %v", tool, argv)
		}
	}
	raw, _ := argValue(argv, "--mcp-config")
	var config struct {
		MCPServers map[string]struct {
			URL string `json:"url"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(raw), &config); err != nil || config.MCPServers[threadMCPName].URL == "" {
		t.Fatalf("--mcp-config = %q, %v", raw, err)
	}
	status, reply := remoteMCPRequest(t, config.MCPServers[threadMCPName].URL, "tools/list", map[string]any{})
	var listing struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(reply["result"], &listing); err != nil || status != 200 {
		t.Fatalf("tools/list: %d %s %v", status, reply, err)
	}
	if names := toolNames(listing.Tools); slices.Contains(names, "thread_spawn") || !slices.Contains(names, "thread_reply") {
		t.Errorf("the ask copy lists %v", names)
	}
	appendPath, _ := argValue(argv, "--append-system-prompt-file")
	if content, err := os.ReadFile(appendPath); err != nil || strings.Contains(string(content), "`thread_spawn`") {
		t.Errorf("the ask copy's guide offers thread_spawn: %v", err)
	}
}

// An agent may not move a sub-thread on its own delegation chain, itself
// included, into the sidebar: that would end the chain's delegation and
// lift the limit, which is the person's to do. Its own workers it may
// place, and a thread at the top of its chain may group itself.
func TestAnAgentCannotFreeItsOwnChainFromTheLimit(t *testing.T) {
	t.Parallel()
	f := newThreadToolsFixture(t)
	lead := f.thread(t, "lead")
	worker := f.thread(t, "worker")
	below := f.thread(t, "below")
	ownSpawn(t, f.app, "spawn-worker", lead.ID, worker.ID)
	ownSpawn(t, f.app, "spawn-below", worker.ID, below.ID)
	standalone := store.PlacementStandalone
	group := "Mine"
	update := func(caller, target string, patch threadtools.UpdateCall) threadtools.ThreadUpdateResult {
		t.Helper()
		patch.ThreadIDs = []string{target}
		report, err := f.adapter.UpdateThreads(context.Background(), threadtools.Caller{ThreadID: caller}, patch)
		if err != nil || len(report.Results) != 1 {
			t.Fatalf("UpdateThreads: %+v, %v", report, err)
		}
		return report.Results[0]
	}
	for _, tc := range []struct {
		name           string
		caller, target string
		patch          threadtools.UpdateCall
	}{
		{"the worker into the sidebar", worker.ID, worker.ID, threadtools.UpdateCall{Placement: &standalone}},
		{"the worker into a group", worker.ID, worker.ID, threadtools.UpdateCall{Group: &group}},
		{"the worker's spawner, from below", below.ID, worker.ID, threadtools.UpdateCall{Placement: &standalone}},
	} {
		result := update(tc.caller, tc.target, tc.patch)
		if result.Updated || result.ErrorCode != threadtools.CodeInvalidRequest || !strings.Contains(result.Error, "Only the user can move this thread") {
			t.Errorf("%s: %+v", tc.name, result)
		}
	}
	if level, err := f.app.store.SubThreadLevel(below.ID); err != nil || level != 2 {
		t.Fatalf("below = %d, %v; want 2 after the refusals", level, err)
	}
	if result := update(lead.ID, lead.ID, threadtools.UpdateCall{Group: &group}); !result.Updated {
		t.Fatalf("the lead grouping itself: %+v", result)
	}
	if result := update(worker.ID, below.ID, threadtools.UpdateCall{Placement: &standalone}); !result.Updated {
		t.Fatalf("the worker placing its own worker: %+v", result)
	}
}

// A spawn's group is resolved where the thread is made: a fork named into
// a group is made in it, never first in its source's, and a group made for
// a thread that is then not made is taken back.
func TestASpawnIsMadeInTheGroupItNames(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.holdCallerTurn(t)
	installMockClaudeReplies(t, f.app, "on it")
	source := f.forkableThread(t, "grouped-source")
	old, err := f.app.store.CreateThreadGroup(source.ProjectID, "Old sweep")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.store.SetThreadGroup([]string{source.ID}, old.ID); err != nil {
		t.Fatal(err)
	}
	ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{
		Prompt: "carry on", FromThread: source.ID, Group: "New sweep",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	fork, err := f.app.store.GetThread(ack.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	groups, err := f.app.store.ListThreadGroups()
	if err != nil {
		t.Fatal(err)
	}
	named := map[string]string{}
	for _, group := range groups {
		named[group.ID] = group.Name
	}
	if named[fork.GroupID] != "New sweep" || fork.Placement != store.PlacementGrouped || fork.ProjectID != source.ProjectID {
		t.Fatalf("the fork is %s in group %q of project %s, want grouped in New sweep in its source's project", fork.Placement, named[fork.GroupID], fork.ProjectID)
	}

	// A fork that cannot be made: its source's session is gone.
	broken := f.forkableThread(t, "broken-source")
	broken.SessionRef = "missing-session"
	if err := f.app.store.UpdateThread(broken); err != nil {
		t.Fatal(err)
	}
	if _, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{
		Prompt: "carry on", FromThread: broken.ID, Group: "Never made",
	}); err == nil {
		t.Fatal("a fork of a source with no session was made")
	}
	if groups, err = f.app.store.ListThreadGroups(); err != nil {
		t.Fatal(err)
	}
	for _, group := range groups {
		if group.Name == "Never made" {
			t.Fatalf("the group made for a thread that was not made stayed: %+v", group)
		}
	}
}
