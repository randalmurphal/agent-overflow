package codex

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/providertest"
)

func childCommandFrame(childThreadID, itemID, status string) json.RawMessage {
	return json.RawMessage(`{"threadId":"` + childThreadID + `","turnId":"child-turn","item":{"id":"` + itemID + `","type":"commandExecution","source":"unifiedExecStartup","status":"` + status + `","processId":"77","command":"sleep 60"}}`)
}

// commandOrder is the item and kind of each command start and completion in
// events, in order.
func commandOrder(events []provider.ProviderEvent) []string {
	var order []string
	for _, e := range events {
		if e.Kind == provider.EventToolStart {
			order = append(order, e.ItemID+" start")
		} else if e.Kind == provider.EventToolComplete {
			order = append(order, e.ItemID+" complete")
		}
	}
	return order
}

func sameOrder(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// eventLog collects the events several goroutines emit.
type eventLog struct {
	mu     sync.Mutex
	events []provider.ProviderEvent
}

func (l *eventLog) add(e provider.ProviderEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *eventLog) snapshot() []provider.ProviderEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]provider.ProviderEvent(nil), l.events...)
}

// newConnRoutedSession is a routing session that is the only session on a
// connection, as a production session is. Frames handed to the connection's
// dispatchNotification take the read loop's routing: an owned thread's to the
// session, an unowned child's into the connection's quarantine. The
// app-server reads every request and answers none.
func newConnRoutedSession(t *testing.T, onEvent func(provider.ProviderEvent)) (*Session, *Conn) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := provider.Spawn(ctx, providertest.Lifetime(), provider.SpawnConfig{Binary: "sh", Args: []string{"-c", "cat >/dev/null"}})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	conn := newConnWithProcess(proc, cancel, "sh")
	s := newMultiAgentV2RoutingSession(t, onEvent)
	s.conn, s.proc, s.ctx, s.cancel = conn, proc, ctx, cancel
	root := s.rootThreadID()
	conn.allSessions[s] = struct{}{}
	conn.byThread[root] = s
	conn.loaded[root] = struct{}{}
	t.Cleanup(func() {
		cancel()
		if err := proc.Close(); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("close fake server: %v", err)
		}
		s.collabAsyncWG.Wait()
		conn.routesMu.Lock()
		for _, timer := range conn.deferredChildDeadlines {
			timer.Stop()
		}
		conn.routesMu.Unlock()
	})
	return s, conn
}

// A second drainer of a backlog the first is still delivering hands on
// nothing: a frame the read loop routes meanwhile waits behind the rest of
// the backlog, and every frame goes out in wire order.
func TestASecondDrainerLeavesTheBacklogToTheFirst(t *testing.T) {
	t.Parallel()
	var log eventLog
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s, conn := newConnRoutedSession(t, func(e provider.ProviderEvent) {
		if e.Kind == provider.EventToolStart && e.ItemID == "cmd-1" {
			once.Do(func() { close(entered) })
			<-release
		}
		log.add(e)
	})
	conn.dispatchNotification("item/started", childCommandFrame("child-a", "cmd-1", "inProgress"))
	conn.dispatchNotification("item/completed", childCommandFrame("child-a", "cmd-1", "completed"))
	if !s.registerHistoricalChildOwnership("root-provider-thread", "child-a", "/root/child-a", "spawn-a") {
		t.Fatal("register ownership")
	}
	first := make(chan struct{})
	go func() {
		defer close(first)
		s.flushEarlyFrames()
	}()
	<-entered

	s.flushEarlyFrames()
	conn.dispatchNotification("item/started", childCommandFrame("child-a", "cmd-2", "inProgress"))
	if order := commandOrder(log.snapshot()); len(order) != 0 {
		close(release)
		<-first
		t.Fatalf("dispatched %v while the first drainer was still delivering the backlog", order)
	}
	close(release)
	<-first

	if got, want := commandOrder(log.snapshot()), []string{"cmd-1 start", "cmd-1 complete", "cmd-2 start"}; !sameOrder(got, want) {
		t.Fatalf("delivered = %v, want %v", got, want)
	}
}

// A publisher off the read loop registers between two frames, never inside
// one: a resume that starts while the read loop is dispatching a root frame
// registers the agent's route, and delivers the agent's backlog, once that
// dispatch has ended.
func TestAResumeRegistersBetweenFramesNotDuringOne(t *testing.T) {
	t.Parallel()
	var log eventLog
	rootEntered, rootRelease := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s, conn := newConnRoutedSession(t, func(e provider.ProviderEvent) {
		if e.Kind == provider.EventTextDelta && e.Content == "root text" {
			once.Do(func() { close(rootEntered) })
			<-rootRelease
		}
		log.add(e)
	})
	conn.dispatchNotification("item/started", childCommandFrame("child-a", "cmd-1", "inProgress"))

	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		conn.dispatchNotification("item/agentMessage/delta", json.RawMessage(`{"threadId":"root-provider-thread","turnId":"root-turn","itemId":"m1","delta":"root text"}`))
	}()
	<-rootEntered
	resumed := make(chan struct{})
	go func() {
		defer close(resumed)
		s.rehydrateCollabOwnership([]ResumeCollabLaunch{{ItemID: "spawn-a", Meta: json.RawMessage(`{"input":{"tool":"spawn_agent","receiverThreadIds":["child-a"]}}`)}})
	}()
	for deadline := time.Now().Add(200 * time.Millisecond); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if parent := s.parentToolUseForProviderThread("child-a"); parent != "" {
			close(rootRelease)
			<-readerDone
			<-resumed
			t.Fatalf("the resume registered the agent's route (%q) while the read loop was still dispatching a frame", parent)
		}
	}
	close(rootRelease)
	<-readerDone
	<-resumed

	if parent := s.parentToolUseForProviderThread("child-a"); parent != "spawn-a" {
		t.Fatalf("route after the resume = %q, want spawn-a", parent)
	}
	if got, want := commandOrder(log.snapshot()), []string{"cmd-1 start"}; !sameOrder(got, want) {
		t.Fatalf("delivered = %v, want %v", got, want)
	}
}

// A resume reads each recovered agent's state only after the agent's
// backlog is delivered, so a status the read recovers comes after the
// frames that preceded it on the wire; a frame the read loop routes while
// the backlog is delivered waits behind it.
func TestAResumeReadsAnAgentsStateAfterItsBacklog(t *testing.T) {
	t.Parallel()
	var log eventLog
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s, conn := newConnRoutedSession(t, func(e provider.ProviderEvent) {
		if e.Kind == provider.EventToolStart && e.ItemID == "cmd-1" {
			once.Do(func() { close(entered) })
			<-release
		}
		log.add(e)
	})
	conn.dispatchNotification("item/started", childCommandFrame("child-a", "cmd-1", "inProgress"))
	conn.dispatchNotification("item/started", childCommandFrame("child-a", "cmd-2", "inProgress"))

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.rehydrateCollabOwnership([]ResumeCollabLaunch{{ItemID: "spawn-a", Meta: json.RawMessage(`{"input":{"tool":"spawn_agent","receiverThreadIds":["child-a"]}}`)}})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the resume did not deliver the agent's backlog")
	}
	conn.dispatchNotification("item/completed", childCommandFrame("child-a", "cmd-1", "completed"))
	reading := func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.collabHistory.running || len(s.collabHistory.queue) > 0 || s.collabHistory.visited["child-a"] != 0
	}
	readEarly := reading()
	close(release)
	<-done
	if readEarly {
		t.Fatal("the agent's state was read while its backlog was still being delivered")
	}
	if got, want := commandOrder(log.snapshot()), []string{"cmd-1 start", "cmd-2 start", "cmd-1 complete"}; !sameOrder(got, want) {
		t.Fatalf("delivered = %v, want %v", got, want)
	}
	if !reading() {
		t.Fatal("the agent's state was never read")
	}
}

// A delivered frame that retires the route, the unload of a closed agent,
// leaves the frames after it as an unowned child's quarantine under an
// ownership deadline: none of them reaches the root thread's projection.
func TestABacklogFrameThatRetiresTheRouteLeavesTheRestQuarantined(t *testing.T) {
	t.Parallel()
	var log eventLog
	s, conn := newConnRoutedSession(t, log.add)
	conn.dispatchNotification("thread/status/changed", json.RawMessage(`{"threadId":"child-a","status":{"type":"notLoaded"}}`))
	conn.dispatchNotification("item/started", childCommandFrame("child-a", "cmd-1", "inProgress"))
	if !s.registerHistoricalChildOwnership("root-provider-thread", "child-a", "/root/child-a", "spawn-a") {
		t.Fatal("register ownership")
	}
	s.mu.Lock()
	s.collab.childResidency = map[string]childResidency{"child-a": {closed: true}}
	s.mu.Unlock()

	s.flushEarlyFrames()
	if parent := s.parentToolUseForProviderThread("child-a"); parent != "" {
		t.Fatalf("route = %q after the closed agent's unload, want retired", parent)
	}
	if order := commandOrder(log.snapshot()); len(order) != 0 {
		t.Fatalf("frames after the retirement were dispatched: %v", order)
	}
	s.mu.Lock()
	queued := len(s.childRouting.deferredChildWireEvents["child-a"])
	deadline := s.childRouting.deferredChildDeadlines["child-a"]
	s.mu.Unlock()
	if queued != 1 || deadline == nil {
		t.Fatalf("after the retirement: %d frames queued, deadline %v; want the command start quarantined under a deadline", queued, deadline != nil)
	}
}

// A published route ends its backlog's ownership deadline, and an expiry
// that fired before the route was published finds nothing to reject: the
// backlog is already in the session's frame queue.
func TestAPublishedRoutesBacklogDoesNotExpire(t *testing.T) {
	t.Parallel()
	var events []provider.ProviderEvent
	s := newMultiAgentV2RoutingSession(t, func(e provider.ProviderEvent) { events = append(events, e) })
	if got := s.routeChildWireEvent("child-a", deferredChildWireEvent{Method: "item/started", Params: childCommandFrame("child-a", "cmd-1", "inProgress")}); got != childWireDeferred {
		t.Fatalf("unowned child's start = %v, want quarantined", got)
	}
	if !s.registerHistoricalChildOwnership("root-provider-thread", "child-a", "/root/child-a", "spawn-a") {
		t.Fatal("register ownership")
	}
	s.mu.Lock()
	deadline := s.childRouting.deferredChildDeadlines["child-a"]
	s.mu.Unlock()
	if deadline != nil {
		t.Fatal("the published route's backlog kept its ownership deadline")
	}

	s.expireDeferredChildWireEvents("child-a")
	s.flushEarlyFrames()
	if order := commandOrder(events); !sameOrder(order, []string{"cmd-1 start"}) {
		t.Fatalf("delivered = %v (events %+v), want the command start", order, events)
	}
}

// The raw spawn output publishes the agent's route on the connection, so the
// frames the agent sent before it go out with it rather than waiting out the
// connection's ownership deadline, and its later frames reach the session.
func TestARawSpawnOutputOpensTheAgentsRouteOnTheConnection(t *testing.T) {
	t.Parallel()
	var log eventLog
	s, conn := newConnRoutedSession(t, log.add)
	conn.dispatchNotification("item/started", childCommandFrame("agent-x", "cmd-1", "inProgress"))
	if order := commandOrder(log.snapshot()); len(order) != 0 {
		t.Fatalf("an unowned agent's frame reached triage: %v", order)
	}
	conn.dispatchNotification("rawResponseItem/completed", json.RawMessage(`{"threadId":"root-provider-thread","turnId":"turn-1","item":{"type":"function_call","name":"spawn_agent","call_id":"spawn-1","arguments":"{\"agent_type\":\"default\",\"message\":\"go\"}"}}`))
	conn.dispatchNotification("rawResponseItem/completed", json.RawMessage(`{"threadId":"root-provider-thread","turnId":"turn-1","item":{"type":"function_call_output","call_id":"spawn-1","output":"{\"agent_id\":\"agent-x\"}"}}`))

	conn.routesMu.Lock()
	held, deadline, owner := len(conn.deferredChild["agent-x"]), conn.deferredChildDeadlines["agent-x"], conn.byThread["agent-x"]
	conn.routesMu.Unlock()
	if held != 0 || deadline != nil || owner != s {
		t.Fatalf("after the raw spawn output: %d frames held on the connection, deadline %v, route to the session %v; want none held, no deadline, and the route", held, deadline != nil, owner == s)
	}
	conn.dispatchNotification("item/completed", childCommandFrame("agent-x", "cmd-1", "completed"))
	events := log.snapshot()
	var commands []provider.ProviderEvent
	for _, e := range events {
		if e.ItemID == "cmd-1" {
			commands = append(commands, e)
		}
	}
	if got := commandOrder(commands); !sameOrder(got, []string{"cmd-1 start", "cmd-1 complete"}) {
		t.Fatalf("the agent's command = %v (events %+v), want its start, then its completion", got, events)
	}
	for _, e := range commands {
		if e.ParentToolUseID != "spawn-1" {
			t.Fatalf("the agent's command %+v is not under spawn-1", e)
		}
	}
}
