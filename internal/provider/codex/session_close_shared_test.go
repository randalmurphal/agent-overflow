package codex

import (
	"context"
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
// never confirms that thread's unload.
const endSessionFakeCodexScript = `#!/usr/bin/env python3
import json, os, sys

seq = 0
log_path = os.environ["FAKE_CODEX_LOG"]
no_close = set(filter(None, os.environ.get("FAKE_CODEX_NO_CLOSE", "").split(",")))

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
    elif method in ("turn/interrupt", "thread/backgroundTerminals/clean"):
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
}

func newSharedCloseFixture(t *testing.T, noClose ...string) sharedCloseFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	logPath := filepath.Join(t.TempDir(), "calls.log")
	conn, err := Dial(ctx, ConnConfig{
		Binary:  writeFakeMultiplexCodex(t, endSessionFakeCodexScript),
		WorkDir: t.TempDir(),
		Env:     map[string]string{"FAKE_CODEX_LOG": logPath, "FAKE_CODEX_NO_CLOSE": strings.Join(noClose, ",")},
		Reaper:  procutiltest.Reaper(),
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	open := func(name string) *Session {
		s, err := conn.OpenPooled(ctx, name, Config{WorkDir: t.TempDir()}, func(provider.ProviderEvent) {}, func() {})
		if err != nil {
			t.Fatalf("open %s: %v", name, err)
		}
		if err := s.Send(ctx, "go", provider.SendOptions{}); err != nil {
			t.Fatalf("send on %s: %v", name, err)
		}
		agent := s.rootThreadID() + "-agent"
		for {
			s.mu.Lock()
			running := s.collab.childRuntimeByThread[agent].phase == childRuntimeRunning && s.turn.activeTurnID != ""
			s.mu.Unlock()
			if running && conn.isThreadLoaded(agent) {
				return s
			}
			select {
			case <-ctx.Done():
				t.Fatalf("%s's turn and agent %s did not start", name, agent)
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	return sharedCloseFixture{conn: conn, a: open("ao-thread-a"), b: open("ao-thread-b"), logPath: logPath}
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
