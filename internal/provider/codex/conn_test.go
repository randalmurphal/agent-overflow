package codex

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/procutil/procutiltest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/testutil/mockexec"
)

func writeFakeMultiplexCodex(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fake requires unix")
	}
	path := filepath.Join(t.TempDir(), "fake-codex-mux")
	mockexec.WriteFile(t, path, []byte(script), 0o755)
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
	defer func() { _ = conn.Close() }()

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

func TestConnRouteTargetQuarantinesUnclaimedChildWhileSiblingStarts(t *testing.T) {
	t.Parallel()
	sessA := &Session{threadID: "ao-a"}
	sessB := &Session{threadID: "ao-b"}
	conn := &Conn{
		allSessions:    map[*Session]struct{}{sessA: {}, sessB: {}},
		byThread:       map[string]*Session{"codex-a": sessA},
		bySessionID:    map[string]*Session{},
		starting:       sessB,
		holds:          map[string]*childHold{},
		startHoldTimer: func(d time.Duration, f func()) holdTimer { return time.AfterFunc(d, f) },
	}

	// An unclaimed child frame or approval (allowStartingFallback=false) MUST NOT
	// route to sibling sessB while sessB is starting.
	if got := conn.routeTarget("codex-child-of-a", "", "", false); got != nil {
		t.Fatalf("routeTarget(child, allowStartingFallback=false) = %v, want nil (quarantine)", got)
	}
	// A root thread/started notification (allowStartingFallback=true) still routes to sessB.
	if got := conn.routeTarget("codex-b", "", "", true); got != sessB {
		t.Fatalf("routeTarget(root thread/started, allowStartingFallback=true) = %v, want sessB", got)
	}
}

func TestConnThreadScopedMCPStartupUpdateNotifiesConnHandler(t *testing.T) {
	t.Parallel()
	sessA := &Session{threadID: "ao-a"}
	var gotUpdates []MCPStartupUpdate
	conn := &Conn{
		allSessions:             map[*Session]struct{}{sessA: {}},
		byThread:                map[string]*Session{"codex-a": sessA},
		bySessionID:             map[string]*Session{},
		mcpStartupUpdateHandler: func(u MCPStartupUpdate) { gotUpdates = append(gotUpdates, u) },
	}
	params := json.RawMessage(`{"threadId":"codex-a","name":"github","status":"ready"}`)
	conn.dispatchNotification("mcpServer/startupStatus/updated", params)
	if len(gotUpdates) != 1 || gotUpdates[0].Name != "github" || gotUpdates[0].State != "ready" {
		t.Fatalf("conn mcpStartupUpdateHandler got %+v, want github/ready", gotUpdates)
	}
}

func TestConnOpenPooledFailsWithErrThreadUnloadFailedWhenUnsubscribeUnsupported(t *testing.T) {
	t.Parallel()
	const noUnsubscribeScript = `#!/usr/bin/env python3
import json, sys

def emit(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()

for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    msg = json.loads(line)
    mid = msg.get("id")
    method = msg.get("method")
    if method == "initialize":
        emit({"jsonrpc": "2.0", "id": mid, "result": {"userAgent": "codex-old"}})
    elif method == "thread/start":
        emit({"jsonrpc": "2.0", "id": mid, "result": {"thread": {"id": "codex-thr-loaded"}}})
        emit({"jsonrpc": "2.0", "method": "thread/started", "params": {"thread": {"id": "codex-thr-loaded"}}})
    elif method == "thread/unsubscribe":
        emit({"jsonrpc": "2.0", "id": mid, "error": {"code": -32601, "message": "Method not found"}})
`
	bin := writeFakeMultiplexCodex(t, noUnsubscribeScript)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := Dial(ctx, ConnConfig{
		Binary:  bin,
		WorkDir: t.TempDir(),
		Reaper:  procutiltest.Reaper(),
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// First open starts the thread on Conn; Close fails to unsubscribe so
	// Conn keeps codex-thr-loaded marked as loaded.
	sess1, err := conn.OpenPooled(ctx, "ao-resume-old", Config{
		WorkDir: t.TempDir(),
	}, func(provider.ProviderEvent) {}, func() {})
	if err != nil {
		t.Fatalf("initial OpenPooled: %v", err)
	}
	_ = sess1.Close()

	var released atomic.Int32
	_, err = conn.OpenPooled(ctx, "ao-resume-old", Config{
		WorkDir:        t.TempDir(),
		ResumeThreadID: "codex-thr-loaded",
	}, func(provider.ProviderEvent) {}, func() { released.Add(1) })
	if !errors.Is(err, ErrThreadUnloadFailed) {
		t.Fatalf("OpenPooled err = %v, want ErrThreadUnloadFailed", err)
	}
	if released.Load() != 1 {
		t.Fatalf("release callback called %d times, want 1", released.Load())
	}
}

func TestConnAtomicRouteOrQuarantineAndEarlyRootFIFO(t *testing.T) {
	t.Parallel()
	var gotOrder []string
	var orderMu sync.Mutex
	sessA := &Session{
		threadID: "ao-a",
		onEvent: func(ev provider.ProviderEvent) {
			if ev.Kind == provider.EventTextDelta {
				orderMu.Lock()
				gotOrder = append(gotOrder, ev.Content)
				orderMu.Unlock()
			}
		},
	}
	sessA.setRootThreadID("codex-a")
	sessB := &Session{threadID: "ao-b"}
	conn := &Conn{
		allSessions:    map[*Session]struct{}{sessA: {}, sessB: {}},
		byThread:       map[string]*Session{"codex-b": sessB},
		bySessionID:    map[string]*Session{},
		loaded:         map[string]struct{}{"codex-b": {}},
		holds:          map[string]*childHold{},
		startHoldTimer: func(d time.Duration, f func()) holdTimer { return time.AfterFunc(d, f) },
	}

	// 1. Unregistered thread frame is atomically quarantined under routesMu.
	earlyParams := json.RawMessage(`{"threadId":"codex-a","turnId":"turn-1","itemId":"m1","delta":"first"}`)
	if target := conn.routeOrQuarantine("codex-a", "", "", false, deferredChildWireEvent{
		Method: "item/agentMessage/delta",
		Params: earlyParams,
	}, nil); target != nil {
		t.Fatalf("expected quarantine before route registration, got %v", target)
	}

	// 2. Register route and transfer quarantined frames under routesMu before
	// delivering a newer frame: strict FIFO order must emit "first" then "second".
	conn.routesMu.Lock()
	conn.byThread["codex-a"] = sessA
	early := conn.takeHoldLocked("codex-a")
	sessA.enqueueEarlyFrames(early.frames)
	conn.routesMu.Unlock()

	laterParams := json.RawMessage(`{"threadId":"codex-a","turnId":"turn-1","itemId":"m1","delta":"second"}`)
	target := conn.routeOrQuarantine("codex-a", "", "", false, deferredChildWireEvent{
		Method: "item/agentMessage/delta",
		Params: laterParams,
	}, nil)
	if target != sessA {
		t.Fatalf("routeOrQuarantine after registration = %v, want sessA", target)
	}
	target.dispatchNotification("item/agentMessage/delta", laterParams)

	orderMu.Lock()
	firstTwo := append([]string(nil), gotOrder...)
	orderMu.Unlock()
	if len(firstTwo) != 2 || firstTwo[0] != "first" || firstTwo[1] != "second" {
		t.Fatalf("early/later delivery order = %v, want [first second]", firstTwo)
	}

	// 3. Re-entrant child-route registration during flushEarlyFrames must NEVER
	// deadlock when the newly registered child already has quarantined frames.
	rootID := "codex-a"
	sessA.conn = conn
	sessA.codexThreadID.Store(&rootID)
	conn.routesMu.Lock()
	if rejection := conn.holdLocked("child-reentrant", deferredChildWireEvent{
		Method: "item/agentMessage/delta",
		Params: json.RawMessage(`{"threadId":"child-reentrant","turnId":"t-c","itemId":"m-c","delta":"child-after-spawn"}`),
	}); rejection != "" {
		t.Fatal(rejection)
	}
	conn.routesMu.Unlock()
	spawnCompleteParams := json.RawMessage(`{"threadId":"codex-a","turnId":"turn-1","item":{"type":"collabAgentToolCall","id":"spawn-1","tool":"spawnAgent","status":"completed","receiverThreadIds":["child-reentrant"]}}`)
	sessA.enqueueEarlyFrames([]deferredChildWireEvent{{
		Method: "item/completed",
		Params: spawnCompleteParams,
	}})
	reentrantDone := make(chan struct{})
	go func() {
		defer close(reentrantDone)
		sessA.flushEarlyFrames()
	}()
	select {
	case <-reentrantDone:
	case <-time.After(3 * time.Second):
		t.Fatal("flushEarlyFrames deadlocked on re-entrant child route registration")
	}
	conn.routesMu.Lock()
	gotChildOwner := conn.byThread["child-reentrant"]
	conn.routesMu.Unlock()
	if gotChildOwner != sessA {
		t.Fatalf("child-reentrant route = %v, want sessA", gotChildOwner)
	}
	orderMu.Lock()
	finalOrder := append([]string(nil), gotOrder...)
	orderMu.Unlock()
	if len(finalOrder) != 3 || finalOrder[2] != "child-after-spawn" {
		t.Fatalf("re-entrant child frame delivery order = %v, want [first second child-after-spawn]", finalOrder)
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
