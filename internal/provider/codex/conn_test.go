package codex

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
)

func writeFakeMultiplexCodex(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fake requires unix")
	}
	path := filepath.Join(t.TempDir(), "fake-codex-mux")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}
	return path
}

// multiThreadFakeCodexScript runs a single python3-free POSIX sh+awk or
// python3 fake Codex app-server supporting initialize, thread/start (unique
// thread ids), thread/resume, thread/unsubscribe -> thread/closed, turn/start,
// skills/list, and account/usage/read over one multiplexed stdio pipe.
const multiThreadFakeCodexScript = `#!/usr/bin/env python3
import json, sys, os

seq = 0
loaded = set()
unsubscribed = []

def emit(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()

for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    msg = json.loads(line)
    rid = msg.get("id")
    method = msg.get("method", "")
    params = msg.get("params") or {}

    if method == "initialize":
        emit({"jsonrpc": "2.0", "id": rid, "result": {"userAgent": "codex_cli_rs/0.162.1 (test)"}})
    elif method == "initialized":
        continue
    elif method == "thread/start":
        seq += 1
        tid = f"codex-thr-{seq}"
        loaded.add(tid)
        emit({"jsonrpc": "2.0", "id": rid, "result": {
            "thread": {"id": tid, "sessionId": f"sess-{seq}", "historyMode": "paginated"},
            "approvalsReviewer": params.get("approvalsReviewer", "user"),
        }})
    elif method == "thread/resume":
        tid = params.get("threadId", "codex-thr-resumed")
        loaded.add(tid)
        emit({"jsonrpc": "2.0", "id": rid, "result": {
            "thread": {"id": tid, "sessionId": f"sess-{tid}", "historyMode": "paginated"},
            "approvalsReviewer": params.get("approvalsReviewer", "user"),
        }})
    elif method == "thread/unsubscribe":
        tid = params.get("threadId", "")
        loaded.discard(tid)
        unsubscribed.append(tid)
        emit({"jsonrpc": "2.0", "id": rid, "result": {}})
        emit({"jsonrpc": "2.0", "method": "thread/closed", "params": {"threadId": tid}})
    elif method == "thread/backgroundTerminals/clean":
        emit({"jsonrpc": "2.0", "id": rid, "result": {}})
    elif method == "turn/start":
        tid = params.get("threadId", "")
        text = ""
        for item in params.get("input") or []:
            if item.get("type") == "text":
                text = item.get("text", "")
        if text == "CRASH_NOW":
            emit({"jsonrpc": "2.0", "id": rid, "result": {"turn": {"id": "turn-crash"}}})
            os._exit(2)
        if text == "BROADCAST_ALL":
            emit({"jsonrpc": "2.0", "method": "skills/changed", "params": {}})
            emit({"jsonrpc": "2.0", "method": "account/rateLimits/updated", "params": {"rateLimits": {"limitId": "codex", "planType": "pro"}}})
            emit({"jsonrpc": "2.0", "method": "configWarning", "params": {"summary": "cfg notice"}})
        turn_id = f"turn-{tid}"
        emit({"jsonrpc": "2.0", "id": rid, "result": {"turn": {"id": turn_id}}})
        emit({"jsonrpc": "2.0", "method": "turn/started", "params": {"threadId": tid, "turn": {"id": turn_id}}})
        emit({"jsonrpc": "2.0", "method": "item/completed", "params": {"threadId": tid, "turnId": turn_id, "item": {"id": f"msg-{tid}", "type": "agentMessage", "text": f"reply:{tid}:{text}"}}})
        emit({"jsonrpc": "2.0", "method": "turn/completed", "params": {"threadId": tid, "turn": {"id": turn_id, "status": "completed"}}})
    elif method == "skills/list":
        cwds = params.get("cwds") or []
        emit({"jsonrpc": "2.0", "id": rid, "result": {"data": [{"cwd": c, "skills": [], "errors": []} for c in cwds]}})
    elif method == "account/usage/read":
        emit({"jsonrpc": "2.0", "id": rid, "result": {"summary": {"lifetimeTokens": 42}, "dailyUsageBuckets": []}})
    elif method == "account/read":
        emit({"jsonrpc": "2.0", "id": rid, "result": {"account": {"type": "chatgpt", "email": "mux@example.com", "planType": "pro"}}})
    elif method == "config/read":
        emit({"jsonrpc": "2.0", "id": rid, "result": {"config": {}}})
    elif rid is not None:
        emit({"jsonrpc": "2.0", "id": rid, "error": {"code": -32601, "message": f"Method not found: {method}"}})
`

func TestConnMultiplexesSessionsAndIsolatesClose(t *testing.T) {
	t.Parallel()
	bin := writeFakeMultiplexCodex(t, multiThreadFakeCodexScript)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn, err := Dial(ctx, ConnConfig{
		Binary:  bin,
		WorkDir: t.TempDir(),
		Reaper:  procutiltest.Reaper(),
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	var skillsCalls atomic.Int32
	conn.SetSkillsChangedHandler(func() {
		skillsCalls.Add(1)
	})

	var muA, muB sync.Mutex
	var eventsA, eventsB []provider.ProviderEvent
	doneA := make(chan string, 2)
	doneB := make(chan string, 2)

	var releasedA, releasedB atomic.Int32
	sessA, err := conn.OpenPooled(ctx, "ao-thread-a", Config{WorkDir: t.TempDir()}, func(e provider.ProviderEvent) {
		muA.Lock()
		eventsA = append(eventsA, e)
		muA.Unlock()
		if e.Kind == provider.EventContentBlockStop && strings.HasPrefix(e.Content, "reply:") {
			doneA <- e.Content
		}
	}, func() { releasedA.Add(1) })
	if err != nil {
		t.Fatalf("Open A: %v", err)
	}

	sessB, err := conn.OpenPooled(ctx, "ao-thread-b", Config{WorkDir: t.TempDir()}, func(e provider.ProviderEvent) {
		muB.Lock()
		eventsB = append(eventsB, e)
		muB.Unlock()
		if e.Kind == provider.EventContentBlockStop && strings.HasPrefix(e.Content, "reply:") {
			doneB <- e.Content
		}
	}, func() { releasedB.Add(1) })
	if err != nil {
		t.Fatalf("Open B: %v", err)
	}

	if sessA.PID() != conn.PID() || sessB.PID() != conn.PID() {
		t.Fatalf("sessions did not share Conn PID: A=%d B=%d conn=%d", sessA.PID(), sessB.PID(), conn.PID())
	}

	// Send on both sessions concurrently; also trigger connection broadcasts.
	if err := sessA.Send(ctx, "BROADCAST_ALL", provider.SendOptions{}); err != nil {
		t.Fatalf("Send A: %v", err)
	}
	if err := sessB.Send(ctx, "hello-b", provider.SendOptions{}); err != nil {
		t.Fatalf("Send B: %v", err)
	}

	replyA := <-doneA
	replyB := <-doneB
	if !strings.Contains(replyA, "codex-thr-1:BROADCAST_ALL") {
		t.Fatalf("replyA = %q, want codex-thr-1", replyA)
	}
	if !strings.Contains(replyB, "codex-thr-2:hello-b") {
		t.Fatalf("replyB = %q, want codex-thr-2", replyB)
	}

	// skills/changed fired once on the connection even with 2 sessions attached.
	if got := skillsCalls.Load(); got != 1 {
		t.Fatalf("skillsChanged calls = %d, want 1", got)
	}

	// Both sessions received the connection-wide rateLimits + configWarning broadcasts.
	hasKind := func(events []provider.ProviderEvent, kind provider.EventKind) bool {
		for _, e := range events {
			if e.Kind == kind {
				return true
			}
		}
		return false
	}
	muA.Lock()
	aHasRate := hasKind(eventsA, provider.EventRateLimits)
	aHasWarn := hasKind(eventsA, provider.EventNotification)
	muA.Unlock()
	muB.Lock()
	bHasRate := hasKind(eventsB, provider.EventRateLimits)
	bHasWarn := hasKind(eventsB, provider.EventNotification)
	muB.Unlock()
	if !aHasRate || !bHasRate || !aHasWarn || !bHasWarn {
		t.Fatalf("broadcast fan-out missing: A(rate=%v,warn=%v) B(rate=%v,warn=%v)", aHasRate, aHasWarn, bHasRate, bHasWarn)
	}

	// Process-wide reads run directly on Conn without touching any session.
	usage, err := conn.ReadAccountUsage(ctx)
	if err != nil || usage.LifetimeTokens == nil || *usage.LifetimeTokens != 42 {
		t.Fatalf("conn.ReadAccountUsage = %+v, %v", usage, err)
	}
	skills, err := conn.ListSkills(ctx, []string{"/tmp/ws"}, false)
	if err != nil || len(skills) != 1 || skills[0].Cwd != "/tmp/ws" {
		t.Fatalf("conn.ListSkills = %+v, %v", skills, err)
	}

	// Closing session A detaches & unsubscribes A while leaving Conn and B alive.
	if err := sessA.Close(); err != nil {
		t.Fatalf("Close A: %v", err)
	}
	if releasedA.Load() != 1 {
		t.Fatalf("releasedA = %d, want 1", releasedA.Load())
	}
	select {
	case <-conn.Done():
		t.Fatal("closing shared session A closed the shared Conn")
	default:
	}

	// Session B keeps running turns on the same Conn!
	if err := sessB.Send(ctx, "after-a-closed", provider.SendOptions{}); err != nil {
		t.Fatalf("Send B after A closed: %v", err)
	}
	replyB2 := <-doneB
	if !strings.Contains(replyB2, "codex-thr-2:after-a-closed") {
		t.Fatalf("replyB2 = %q", replyB2)
	}

	if err := sessB.Close(); err != nil {
		t.Fatalf("Close B: %v", err)
	}
	if releasedB.Load() != 1 {
		t.Fatalf("releasedB = %d, want 1", releasedB.Load())
	}
}

func TestConnCrashTearsDownEveryAttachedSession(t *testing.T) {
	t.Parallel()
	bin := writeFakeMultiplexCodex(t, multiThreadFakeCodexScript)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn, err := Dial(ctx, ConnConfig{
		Binary:  bin,
		WorkDir: t.TempDir(),
		Reaper:  procutiltest.Reaper(),
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	errA := make(chan bool, 1)
	discA := make(chan bool, 1)
	errB := make(chan bool, 1)
	discB := make(chan bool, 1)

	sessA, err := conn.Open(ctx, "ao-crash-a", Config{WorkDir: t.TempDir()}, func(e provider.ProviderEvent) {
		if e.Kind == provider.EventSessionStatus && e.Content == "error" {
			errA <- true
		}
		if e.Kind == provider.EventSessionStatus && e.Content == "disconnected" {
			discA <- true
		}
	})
	if err != nil {
		t.Fatalf("Open A: %v", err)
	}
	_, err = conn.Open(ctx, "ao-crash-b", Config{WorkDir: t.TempDir()}, func(e provider.ProviderEvent) {
		if e.Kind == provider.EventSessionStatus && e.Content == "error" {
			errB <- true
		}
		if e.Kind == provider.EventSessionStatus && e.Content == "disconnected" {
			discB <- true
		}
	})
	if err != nil {
		t.Fatalf("Open B: %v", err)
	}

	_ = sessA.Send(ctx, "CRASH_NOW", provider.SendOptions{})

	for _, ch := range []chan bool{errA, discA, errB, discB} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for crash teardown on all sessions")
		}
	}
	select {
	case <-conn.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Conn.Done did not close after crash")
	}
}

func TestCodexEventLogRedactorMasksOutboundMCPHeaders(t *testing.T) {
	t.Parallel()
	redactor := newCodexProviderEventLogRedactor()
	frame := []byte(`{"jsonrpc":"2.0","id":2,"method":"thread/start","params":{"cwd":"/tmp","config":{"mcp_servers":{"ao-thread-tools":{"url":"http://127.0.0.1:9999/mcp","http_headers":{"Authorization":"Bearer super-secret-token"}}}}}}`)
	redacted := string(redactor("out", frame))
	if strings.Contains(redacted, "super-secret-token") {
		t.Fatalf("outbound MCP header secret leaked in event log: %s", redacted)
	}
	if !strings.Contains(redacted, "[redacted]") {
		t.Fatalf("expected [redacted] marker in redacted outbound frame: %s", redacted)
	}
}
