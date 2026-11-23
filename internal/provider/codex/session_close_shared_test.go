package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
)

// endSessionFakeCodexScript is an app-server that many sessions share. Each
// turn/start keeps the root turn running and spawns one agent, whose own
// turn runs too. It logs every interrupt, background-terminal clean and
// unsubscribe it is sent, with the thread it names, to $FAKE_CODEX_LOG, and
// refuses the unsubscribe of each thread in $FAKE_CODEX_NO_CLOSE, so Codex
// never confirms that thread's unload. With $FAKE_CODEX_LATE_AGENT, the
// first interrupt of a root turn spawns a second agent, running a command,
// before Codex answers it. With $FAKE_CODEX_REOPEN_AGENT, each agent's
// thread closes after it starts and loads again, active (activeFlags
// "reopened").
const endSessionFakeCodexScript = `#!/usr/bin/env python3
import json, os, sys

seq = 0
log_path = os.environ["FAKE_CODEX_LOG"]
no_close = set(filter(None, os.environ.get("FAKE_CODEX_NO_CLOSE", "").split(",")))
late_agent = os.environ.get("FAKE_CODEX_LATE_AGENT") == "1"
reopen_agent = os.environ.get("FAKE_CODEX_REOPEN_AGENT") == "1"
late_spawned = set()

def spawn(tid, turn, agent, spawn_id):
    emit({"jsonrpc": "2.0", "method": "thread/started", "params": {"thread": {"id": agent, "parentThreadId": tid, "agentNickname": f"Agent {agent}"}}})
    emit({"jsonrpc": "2.0", "method": "item/completed", "params": {"threadId": tid, "turnId": turn, "item": {
        "type": "collabAgentToolCall", "id": spawn_id, "tool": "spawnAgent", "status": "completed", "receiverThreadIds": [agent]}}})
    emit({"jsonrpc": "2.0", "method": "turn/started", "params": {"threadId": agent, "turn": {"id": f"turn-{agent}"}}})

def emit(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()

def log(line):
    with open(log_path, "a") as f:
        f.write(line + "\n")

for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    msg = json.loads(line)
    rid = msg.get("id")
    method = msg.get("method", "")
    params = msg.get("params") or {}
    tid = params.get("threadId", "")
    if method == "initialize":
        emit({"jsonrpc": "2.0", "id": rid, "result": {"userAgent": "codex_cli_rs/0.162.1 (test)"}})
    elif method == "initialized":
        continue
    elif method == "thread/start":
        seq += 1
        emit({"jsonrpc": "2.0", "id": rid, "result": {
            "thread": {"id": f"thr-{seq}", "sessionId": f"sess-{seq}", "historyMode": "paginated"},
            "approvalsReviewer": params.get("approvalsReviewer", "user"),
        }})
    elif method == "turn/start":
        turn = f"turn-{tid}"
        agent = f"{tid}-agent"
        emit({"jsonrpc": "2.0", "id": rid, "result": {"turn": {"id": turn}}})
        emit({"jsonrpc": "2.0", "method": "turn/started", "params": {"threadId": tid, "turn": {"id": turn}}})
        emit({"jsonrpc": "2.0", "method": "thread/started", "params": {"thread": {"id": agent, "parentThreadId": tid, "agentNickname": f"Agent {tid}"}}})
        emit({"jsonrpc": "2.0", "method": "item/completed", "params": {"threadId": tid, "turnId": turn, "item": {
            "type": "collabAgentToolCall", "id": f"spawn-{tid}", "tool": "spawnAgent", "status": "completed", "receiverThreadIds": [agent]}}})
        emit({"jsonrpc": "2.0", "method": "turn/started", "params": {"threadId": agent, "turn": {"id": f"turn-{agent}"}}})
        if reopen_agent:
            emit({"jsonrpc": "2.0", "method": "thread/closed", "params": {"threadId": agent}})
            emit({"jsonrpc": "2.0", "method": "thread/status/changed", "params": {"threadId": agent, "status": {"type": "active", "activeFlags": ["reopened"]}}})
    elif method == "turn/interrupt":
        log(f"{method} {tid}")
        if late_agent and tid.startswith("thr-") and not tid.endswith("-agent") and not tid.endswith("-late") and tid not in late_spawned:
            late_spawned.add(tid)
            late = f"{tid}-late"
            spawn(tid, f"turn-{tid}", late, f"spawn-late-{tid}")
            emit({"jsonrpc": "2.0", "method": "item/started", "params": {"threadId": late, "turnId": f"turn-{late}", "item": {
                "type": "commandExecution", "id": f"cmd-{late}", "source": "unifiedExecStartup", "status": "inProgress", "processId": "77", "command": "sleep 60"}}})
        emit({"jsonrpc": "2.0", "id": rid, "result": {}})
    elif method == "thread/backgroundTerminals/clean":
        log(f"{method} {tid}")
        emit({"jsonrpc": "2.0", "id": rid, "result": {}})
    elif method == "thread/unsubscribe":
        log(f"{method} {tid}")
        if tid in no_close:
            emit({"jsonrpc": "2.0", "id": rid, "error": {"code": -32603, "message": "unsubscribe failed"}})
        else:
            emit({"jsonrpc": "2.0", "id": rid, "result": {}})
            emit({"jsonrpc": "2.0", "method": "thread/closed", "params": {"threadId": tid}})
    elif method == "config/read":
        emit({"jsonrpc": "2.0", "id": rid, "result": {"config": {}}})
    elif rid is not None:
        emit({"jsonrpc": "2.0", "id": rid, "error": {"code": -32601, "message": f"Method not found: {method}"}})
`

// sharedCloseFixture is two sessions on one connection, each with a running
// turn and a running agent.
type sharedCloseFixture struct {
	conn    *Conn
	a, b    *Session
	logPath string
	events  map[*Session]*eventLog
}

func newSharedCloseFixture(t *testing.T, noClose ...string) sharedCloseFixture {
	t.Helper()
	return newSharedCloseFixtureWith(t, map[string]string{"FAKE_CODEX_NO_CLOSE": strings.Join(noClose, ",")}, "")
}

// newSharedCloseFixtureWith runs the fake with env, and opens each session
// once its agent runs and, with ready set, once an agent status carries
// ready in its active flags.
func newSharedCloseFixtureWith(t *testing.T, env map[string]string, ready string) sharedCloseFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	logPath := filepath.Join(t.TempDir(), "calls.log")
	env["FAKE_CODEX_LOG"] = logPath
	conn, err := Dial(ctx, ConnConfig{
		Binary:  writeFakeMultiplexCodex(t, endSessionFakeCodexScript),
		WorkDir: t.TempDir(),
		Env:     env,
		Reaper:  procutiltest.Reaper(),
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	events := map[*Session]*eventLog{}
	open := func(name string) *Session {
		var log eventLog
		s, err := conn.OpenPooled(ctx, name, Config{WorkDir: t.TempDir()}, log.add, func() {})
		if err != nil {
			t.Fatalf("open %s: %v", name, err)
		}
		events[s] = &log
		if err := s.Send(ctx, "go", provider.SendOptions{}); err != nil {
			t.Fatalf("send on %s: %v", name, err)
		}
		agent := s.rootThreadID() + "-agent"
		for {
			s.mu.Lock()
			running := s.collab.childRuntimeByThread[agent].phase == childRuntimeRunning && s.turn.activeTurnID != ""
			s.mu.Unlock()
			if running && (ready == "" || log.statusFlagged(ready)) {
				return s
			}
			select {
			case <-ctx.Done():
				t.Fatalf("%s's turn and agent %s did not start", name, agent)
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	return sharedCloseFixture{conn: conn, a: open("ao-thread-a"), b: open("ao-thread-b"), logPath: logPath, events: events}
}

// statusFlagged reports whether an agent status in the log carries flag in
// its active flags.
func (l *eventLog) statusFlagged(flag string) bool {
	for _, evt := range l.snapshot() {
		if evt.Kind == provider.EventSubagentStatus && strings.Contains(string(evt.Meta), `"`+flag+`"`) {
			return true
		}
	}
	return false
}

func (f sharedCloseFixture) calls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(f.logPath)
	if err != nil {
		t.Fatalf("read the fake's call log: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// End Session on one of two threads sharing an app-server cleans the
// background terminals of that thread and of its agent before it unloads
// them, and touches nothing of the other thread: its turn and its agent keep
// running.
func TestEndSessionCleansItsOwnThreadAndAgentBeforeUnloading(t *testing.T) {
	t.Parallel()
	f := newSharedCloseFixture(t)
	root, agent := f.a.rootThreadID(), f.a.rootThreadID()+"-agent"
	if err := f.a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	want := []string{
		"turn/interrupt " + root,
		"turn/interrupt " + agent,
		"thread/backgroundTerminals/clean " + root,
		"thread/backgroundTerminals/clean " + agent,
		"thread/unsubscribe " + agent,
		"thread/unsubscribe " + root,
	}
	if got := f.calls(t); !sameOrder(got, want) {
		t.Fatalf("calls = %q, want %q", got, want)
	}

	other := f.b.rootThreadID()
	f.b.mu.Lock()
	turn, agentPhase := f.b.turn.activeTurnID, f.b.collab.childRuntimeByThread[other+"-agent"].phase
	f.b.mu.Unlock()
	if turn == "" || agentPhase != childRuntimeRunning || !f.conn.isThreadLoaded(other) || !f.conn.isThreadLoaded(other+"-agent") {
		t.Fatalf("the other thread after the close: turn %q, agent %v, loaded %v/%v; want its turn and agent still running and loaded",
			turn, agentPhase, f.conn.isThreadLoaded(other), f.conn.isThreadLoaded(other+"-agent"))
	}
}

// An unload Codex does not confirm is the close's error, one per thread,
// naming the agent whose thread it is; the threads that did close are not in
// it.
func TestEndSessionNamesEachThreadCodexDidNotClose(t *testing.T) {
	t.Parallel()
	f := newSharedCloseFixture(t, "thr-1", "thr-1-agent")
	if f.a.rootThreadID() != "thr-1" {
		t.Fatalf("the first session's thread is %q, want thr-1", f.a.rootThreadID())
	}
	err := f.a.Close()
	notClosed := ThreadsNotClosed(err)
	if len(notClosed) != 2 {
		t.Fatalf("Close = %v; threads not closed = %+v, want the agent's and the root's", err, notClosed)
	}
	if notClosed[0].ThreadID != "thr-1-agent" || notClosed[0].Root || notClosed[0].Agent != "Agent thr-1" ||
		notClosed[1].ThreadID != "thr-1" || !notClosed[1].Root {
		t.Fatalf("threads not closed = %+v %+v, want thr-1-agent (Agent thr-1), then thr-1 (the session's own)", *notClosed[0], *notClosed[1])
	}
	if err := f.b.Close(); err != nil || len(ThreadsNotClosed(err)) != 0 {
		t.Fatalf("the other session's Close = %v, want nil", err)
	}
}

// indexOf is the position of call in calls, or -1.
func indexOf(calls []string, call string) int {
	for i, c := range calls {
		if c == call {
			return i
		}
	}
	return -1
}

// An agent the root's turn spawned, and that started a command, before Codex
// answered the root's interrupt is stopped, cleaned and unloaded like the
// agents known when End Session began.
func TestEndSessionStopsAnAgentStartedBeforeTheInterruptWasAnswered(t *testing.T) {
	t.Parallel()
	f := newSharedCloseFixtureWith(t, map[string]string{"FAKE_CODEX_LATE_AGENT": "1"}, "")
	late := f.a.rootThreadID() + "-late"
	if err := f.a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	calls := f.calls(t)
	interrupt, clean, unsubscribe := indexOf(calls, "turn/interrupt "+late), indexOf(calls, "thread/backgroundTerminals/clean "+late), indexOf(calls, "thread/unsubscribe "+late)
	if interrupt < 0 || clean < interrupt || unsubscribe < clean {
		t.Fatalf("calls = %q, want %s interrupted, then cleaned, then unloaded", calls, late)
	}
}

// An agent thread Codex closed and then loaded again is loaded: End Session
// cleans and unloads it. Its thread/closed was its unload, reported in its
// place before it loaded again.
func TestEndSessionClosesAnAgentCodexLoadedAgain(t *testing.T) {
	t.Parallel()
	f := newSharedCloseFixtureWith(t, map[string]string{"FAKE_CODEX_REOPEN_AGENT": "1"}, "reopened")
	agent := f.a.rootThreadID() + "-agent"
	var statuses []string
	for _, evt := range f.events[f.a].snapshot() {
		if evt.Kind == provider.EventSubagentStatus && evt.ProcessScope.ID == agent {
			var meta struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal(evt.Meta, &meta); err != nil {
				t.Fatalf("agent status meta %s: %v", evt.Meta, err)
			}
			statuses = append(statuses, meta.Status)
		}
	}
	if unloaded, reloaded := indexOf(statuses, "notLoaded"), len(statuses)-1; unloaded < 0 || statuses[reloaded] != "running" || unloaded > reloaded {
		t.Fatalf("agent statuses = %q, want its unload, then running again", statuses)
	}
	if err := f.a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	calls := f.calls(t)
	if clean, unsubscribe := indexOf(calls, "thread/backgroundTerminals/clean "+agent), indexOf(calls, "thread/unsubscribe "+agent); clean < 0 || unsubscribe < clean {
		t.Fatalf("calls = %q, want %s cleaned, then unloaded", calls, agent)
	}
}

// Frames a closing session refused for its full queue are the close's error.
func TestEndSessionReportsTheFramesItRefused(t *testing.T) {
	t.Parallel()
	f := newSharedCloseFixture(t)
	f.a.earlyFramesMu.Lock()
	f.a.framesDropped = 3
	f.a.earlyFramesMu.Unlock()
	if err := f.a.Close(); err == nil || !strings.Contains(err.Error(), "3 frames Codex sent while the session was ending were not recorded") {
		t.Fatalf("Close = %v, want it to report the 3 refused frames", err)
	}
}
