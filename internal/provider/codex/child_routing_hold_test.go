package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// recordingRootSession attaches a session to a new fixture connection the way
// Conn.open does, with the root's session id known when the route is bound,
// so frames that name it route by it as they do in the product.
func recordingRootSession(t *testing.T) (*connFixture, *Session, func() []provider.ProviderEvent) {
	t.Helper()
	fx := newConnFixture(t)
	answerChildProfileReads(fx)
	var mu sync.Mutex
	var events []provider.ProviderEvent
	s := fx.conn.newSession(context.Background(), "ao-thread", Config{ResumeThreadID: "root-provider-thread"}, func(event provider.ProviderEvent) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	}, true, nil, nil)
	fresh := fx.conn.registerSession(s, "root-provider-thread")
	s.setRootSessionID("root-session")
	fx.conn.bindSessionRoot(s, fresh)
	return fx, s, func() []provider.ProviderEvent {
		mu.Lock()
		defer mu.Unlock()
		return append([]provider.ProviderEvent(nil), events...)
	}
}

func childDelta(threadID, itemID, delta string) []byte {
	return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":%q,"turnId":"child-turn","itemId":%q,"delta":%q}}`, threadID, itemID, delta))
}

func childApproval(threadID string, id int) []byte {
	return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"item/commandExecution/requestApproval","params":{"threadId":%q,"turnId":"t","itemId":"cmd","command":"go test"}}`, id, threadID))
}

// TestLateSpawnDrainsSameSessionChildFromTheConnection runs a same-session
// child through the connection: a frame that reaches the session by the
// root's session id is held on the connection with the child's later frames,
// which carry only its threadId. The late spawn releases both from the one
// hold, with the child's answer under the spawn.
func TestLateSpawnDrainsSameSessionChildFromTheConnection(t *testing.T) {
	t.Parallel()
	fx, _, events := recordingRootSession(t)

	fx.feed([]byte(`{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"child-a","sessionId":"root-session","parentThreadId":"root-provider-thread"}}}`))
	fx.feed(childDelta("child-a", "answer", "owned"))
	if got := fx.heldFrames("child-a"); got != 2 {
		t.Fatalf("connection held %d frames before the spawn, want 2", got)
	}
	if early := events(); len(early) != 0 {
		t.Fatalf("child frames reached the thread before their spawn: %+v", early)
	}

	fx.feed(v2ActivityLine("root-provider-thread", "root-turn", "spawn-a", "started", "child-a", "/root/child-a"))
	if got := fx.heldFrames("child-a"); got != 0 {
		t.Fatalf("connection still holds %d frames after the spawn", got)
	}
	for _, event := range events() {
		if event.ItemID == "answer" {
			if event.ParentToolUseID != "spawn-a" {
				t.Fatalf("child answer escaped its spawn: %+v", event)
			}
			return
		}
	}
	t.Fatalf("late spawn did not deliver the child answer held on the connection: %+v", events())
}

// A spawn recovered from history records the child's ownership before it
// emits the spawn row. A child frame that arrives between the two is held,
// and reaches the thread only after the spawn row, under it.
func TestChildFrameBetweenRecoveredOwnershipAndItsSpawnWaitsForTheSpawn(t *testing.T) {
	t.Parallel()
	fx := newConnFixture(t)
	answerChildProfileReads(fx)
	fx.answer("thread/read", `{"thread":{"id":"child-r","parentThreadId":"root-provider-thread"}}`)
	fx.answer("thread/items/list", `{"data":[{"turnId":"root-turn","item":{"type":"subAgentActivity","id":"spawn-r","kind":"started","agentThreadId":"child-r","agentPath":"/root/child-r"}}]}`)
	var mu sync.Mutex
	var events []provider.ProviderEvent
	spawnEmitting := make(chan struct{})
	release := make(chan struct{})
	releaseSpawn := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseSpawn)
	var once sync.Once
	s := fx.session("ao-thread", "root-provider-thread", func(event provider.ProviderEvent) {
		if event.ItemID == "spawn-r" && event.Kind == provider.EventToolStart {
			once.Do(func() {
				close(spawnEmitting)
				<-release
			})
		}
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	})
	fx.feed(childDelta("child-r", "first", "held before recovery"))

	recovered := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), fixtureWait)
		defer cancel()
		recovered <- s.recoverChildOwnership(ctx, "child-r", make(map[string]bool))
	}()
	select {
	case <-spawnEmitting:
	case <-time.After(fixtureWait):
		t.Fatal("recovery did not emit the spawn")
	}
	if s.parentToolUseForProviderThread("child-r") != "spawn-r" {
		releaseSpawn()
		t.Fatal("recovery emitted the spawn before recording its ownership")
	}
	err := fx.pipe.deliver(childDelta("child-r", "second", "arrives mid-emission"))
	held := fx.heldFrames("child-r")
	releaseSpawn()
	if err != nil {
		t.Fatalf("the frame that arrived mid-emission was not handled: %v", err)
	}
	if held != 2 {
		t.Fatalf("connection held %d frames while the spawn was emitting, want 2", held)
	}
	if err := <-recovered; err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	spawnDone, delivered := false, 0
	for _, event := range events {
		switch {
		case event.ItemID == "spawn-r" && event.Kind == provider.EventToolComplete:
			spawnDone = true
		case event.ItemID == "first" || event.ItemID == "second":
			if !spawnDone || event.ParentToolUseID != "spawn-r" {
				t.Fatalf("child frame reached the thread before or outside its spawn: %+v", events)
			}
			delivered++
		}
	}
	if delivered != 2 {
		t.Fatalf("delivered %d child frames after the spawn, want 2: %+v", delivered, events)
	}
}

// TestHeldThreadExpiryIsLoggedNotShown pins what the deadline does with a
// thread no spawn claimed: its request is rejected, its notification dropped,
// nothing reaches any thread, and one log line names the thread, the counts
// and how many sessions are on the connection, since a held frame names no
// root.
func TestHeldThreadExpiryIsLoggedNotShown(t *testing.T) {
	fx, _, events := recordingRootSession(t)
	deadlines := fx.deadlines()
	var otherEvents []provider.ProviderEvent
	fx.session("ao-thread-2", "other-root", func(event provider.ProviderEvent) { otherEvents = append(otherEvents, event) })
	fx.feed(childApproval("child-x", 51))
	fx.feed(childDelta("child-x", "m", "x"))

	logged := captureLog(t, func() { deadlines.fire(t, 0) })

	want := "codex: no spawn claimed provider thread child-x before the routing deadline: rejected 1 requests, dropped 1 notifications (0 and 0 more refused for its full queue); 0 frames refused connection-wide for a full routing budget; 2 sessions on the connection"
	if !strings.Contains(logged, want) {
		t.Fatalf("expiry log = %q, want it to contain %q", logged, want)
	}
	if !fx.rejectedRequest(51) {
		t.Fatalf("held request 51 was not rejected: %q", fx.written(1))
	}
	if len(events()) != 0 || len(otherEvents) != 0 {
		t.Fatalf("expiry reached a thread: %+v %+v", events(), otherEvents)
	}
}

// The deadline a hold arms is what ends it: firing the scheduled callback
// rejects the held request and returns the hold's frames to the budget, and
// closing the connection cancels a deadline still pending.
func TestHoldDeadlineRejectsAndReleasesTheBudget(t *testing.T) {
	t.Parallel()
	fx, _, _ := recordingRootSession(t)
	deadlines := fx.deadlines()
	fx.feed(childApproval("child-d", 61))
	fx.feed(childDelta("child-e", "m", "pending at close"))
	if got := deadlines.count(); got != 2 {
		t.Fatalf("the connection armed %d deadlines for two holds, want 2", got)
	}
	if after := deadlines.armed(t, 0).after; after != deferredChildOwnershipTimeout {
		t.Fatalf("deadline armed for %v, want %v", after, deferredChildOwnershipTimeout)
	}

	deadlines.fire(t, 0)
	if !fx.rejectedRequest(61) {
		t.Fatalf("the deadline did not reject held request 61: %q", fx.written(1))
	}
	fx.conn.routesMu.Lock()
	frames, bytes := fx.conn.heldFrames, fx.conn.heldBytes
	_, stillHeld := fx.conn.holds["child-d"]
	fx.conn.routesMu.Unlock()
	if stillHeld || frames != 1 {
		t.Fatalf("after the deadline: hold kept %v, %d frames in the budget, want only child-e's", stillHeld, frames)
	}
	if want := len(childDelta("child-e", "m", "pending at close")); bytes <= 0 || bytes > want {
		t.Fatalf("budget holds %d bytes, want only child-e's frame", bytes)
	}

	if err := fx.conn.Close(); err != nil {
		t.Fatal(err)
	}
	if !deadlines.armed(t, 1).stopped.Load() {
		t.Fatal("closing the connection left child-e's deadline armed")
	}
}

// An expiry owns only the hold that armed it. A second hold for the same
// thread, started while the first expiry is writing its rejections, keeps
// its frames, and the first deadline firing again does nothing to it.
func TestAnExpiryLeavesANewerHoldForTheSameThread(t *testing.T) {
	t.Parallel()
	fx, _, _ := recordingRootSession(t)
	deadlines := fx.deadlines()
	writing := make(chan struct{})
	release := make(chan struct{})
	var gate sync.Once
	fx.pipe.onWrite = func(line []byte) {
		if strings.Contains(string(line), `"id":81`) && strings.Contains(string(line), `"error"`) {
			gate.Do(func() {
				close(writing)
				<-release
			})
		}
		fx.observeWrite(line)
	}
	fx.feed(childApproval("child-y", 81))

	expired := make(chan struct{})
	go func() {
		defer close(expired)
		deadlines.fire(t, 0)
	}()
	select {
	case <-writing:
	case <-time.After(fixtureWait):
		t.Fatal("the expiry did not write its rejection")
	}
	err := fx.pipe.deliver(childDelta("child-y", "m", "second hold"))
	close(release)
	<-expired
	if err != nil {
		t.Fatal(err)
	}
	if got := fx.heldFrames("child-y"); got != 1 {
		t.Fatalf("the newer hold has %d frames after the first expiry, want 1", got)
	}
	deadlines.fire(t, 0)
	if got := fx.heldFrames("child-y"); got != 1 {
		t.Fatalf("the first deadline firing again took the newer hold: %d frames", got)
	}
	if deadlines.armed(t, 1).stopped.Load() {
		t.Fatal("the first expiry stopped the newer hold's deadline")
	}
	rejections := 0
	for _, line := range fx.written(1) {
		if strings.Contains(string(line), `"id":81`) && strings.Contains(string(line), `"error"`) {
			rejections++
		}
	}
	if rejections != 1 {
		t.Fatalf("request 81 was rejected %d times, want once", rejections)
	}
}

// A frame refused for a full thread queue is counted on the hold and logged
// once when a spawn claims it; one refused for the full connection budget is
// counted on the connection and logged once with the next claim or expiry.
func TestRefusedFramesAreCountedAndLoggedOnce(t *testing.T) {
	fx, _, events := recordingRootSession(t)
	deadlines := fx.deadlines()
	for i := 0; i < maxDeferredChildEventsPerThread+2; i++ {
		fx.feed(childDelta("child-q", fmt.Sprintf("q-%d", i), "x"))
	}
	if got := fx.heldFrames("child-q"); got != maxDeferredChildEventsPerThread {
		t.Fatalf("held %d frames, want the per-thread bound %d", got, maxDeferredChildEventsPerThread)
	}
	logged := captureLog(t, func() {
		fx.feed(v2ActivityLine("root-provider-thread", "root-turn", "spawn-q", "started", "child-q", "/root/child-q"))
	})
	want := "codex: a spawn claimed provider thread child-q after its full queue dropped 2 notifications and rejected 0 requests; 0 frames refused connection-wide for a full routing budget"
	if strings.Count(logged, "a spawn claimed provider thread") != 1 || !strings.Contains(logged, want) {
		t.Fatalf("claim log = %q, want it once: %q", logged, want)
	}
	delivered := 0
	for _, event := range events() {
		if strings.HasPrefix(event.ItemID, "q-") {
			delivered++
		}
	}
	if delivered == 0 {
		t.Fatal("the claim did not deliver the held frames")
	}

	// Fill the connection budget across threads, then refuse one more.
	threads := maxDeferredChildEventsTotal / maxDeferredChildEventsPerThread
	for thread := 0; thread < threads; thread++ {
		for i := 0; i < maxDeferredChildEventsPerThread; i++ {
			fx.feed(childDelta(fmt.Sprintf("full-%d", thread), fmt.Sprintf("f-%d", i), "x"))
		}
	}
	fx.feed(childDelta("over-budget", "o", "x"))
	if got := fx.heldFrames("over-budget"); got != 0 {
		t.Fatalf("held %d frames past the connection budget", got)
	}
	fx.conn.routesMu.Lock()
	holds := len(fx.conn.holds)
	fx.conn.routesMu.Unlock()
	if holds != threads {
		t.Fatalf("the connection has %d holds, want %d: a refused frame made one", holds, threads)
	}
	// Deadline 0 was child-q's, which the spawn claimed; 1 and 2 are the
	// first two full threads.
	logged = captureLog(t, func() { deadlines.fire(t, 1) })
	if !strings.Contains(logged, "1 frames refused connection-wide for a full routing budget") {
		t.Fatalf("expiry log = %q, want the budget refusal", logged)
	}
	logged = captureLog(t, func() { deadlines.fire(t, 2) })
	if !strings.Contains(logged, "0 frames refused connection-wide") {
		t.Fatalf("second expiry log = %q, want the budget refusal logged only once", logged)
	}
}

// A first frame over the routing budget arms no hold, so no expiry can report
// it: the connection reports it at the next claim of any child, or at
// teardown when no claim comes, and only once.
func TestAnOversizedFirstFrameIsReportedAtTheNextClaimOrTeardown(t *testing.T) {
	oversized := childDelta("child-big", "big", strings.Repeat("x", maxDeferredChildEventBytes+1))
	t.Run("claim", func(t *testing.T) {
		fx, _, _ := recordingRootSession(t)
		deadlines := fx.deadlines()
		fx.feed(oversized)
		if got := fx.heldFrames("child-big"); got != 0 || deadlines.count() != 0 {
			t.Fatalf("held %d frames and armed %d deadlines for a frame over the budget", got, deadlines.count())
		}
		logged := captureLog(t, func() {
			fx.feed(v2ActivityLine("root-provider-thread", "root-turn", "spawn-c", "started", "child-c", "/root/child-c"))
		})
		want := "codex: a spawn claimed provider thread child-c after its full queue dropped 0 notifications and rejected 0 requests; 1 frames refused connection-wide for a full routing budget"
		if !strings.Contains(logged, want) {
			t.Fatalf("claim log = %q, want %q", logged, want)
		}
		logged = captureLog(t, func() {
			fx.feed(v2ActivityLine("root-provider-thread", "root-turn", "spawn-d", "started", "child-d", "/root/child-d"))
			fx.exitAppServer()
		})
		if strings.Contains(logged, "refused connection-wide") {
			t.Fatalf("the refusal was reported again: %q", logged)
		}
	})
	// A held thread whose deadline has not fired yet does not report it
	// either: the connection closes first.
	t.Run("teardown before an expiry", func(t *testing.T) {
		fx, _, _ := recordingRootSession(t)
		deadlines := fx.deadlines()
		fx.feed(childDelta("child-h", "h", "x"))
		fx.feed(oversized)
		if deadlines.count() != 1 {
			t.Fatalf("armed %d deadlines, want child-h's only", deadlines.count())
		}
		logged := captureLog(t, fx.exitAppServer)
		want := "codex: the connection closed with 1 frames refused connection-wide for a full routing budget since the last claim or expiry"
		if strings.Count(logged, want) != 1 {
			t.Fatalf("teardown log = %q, want %q once", logged, want)
		}
	})
}

// The connection picks source session A for a fork's frame by the
// single-session fallback. Before A dispatches it, session B resumes the fork
// and binds its id, which claims the fork's hold. The point between the two is
// the gate: A dispatches the frame only after B bound. The connection hands
// the frame to B, the thread's owner, instead of opening a hold nobody will
// claim.
func TestAFrameForARoutedThreadGoesToItsOwner(t *testing.T) {
	fx, a, _ := recordingRootSession(t)
	deadlines := fx.deadlines()
	delta := json.RawMessage(`{"threadId":"fork-thread","turnId":"fork-turn","itemId":"fork-answer","delta":"forked"}`)
	if got := fx.conn.routeTarget("fork-thread", "", "", true); got != a {
		t.Fatalf("the fallback routed the fork's frame to %v, want the only session", got)
	}

	var mu sync.Mutex
	var bEvents []provider.ProviderEvent
	fx.session("ao-thread-b", "fork-thread", func(event provider.ProviderEvent) {
		mu.Lock()
		bEvents = append(bEvents, event)
		mu.Unlock()
	})
	a.dispatchNotificationDirect("item/agentMessage/delta", delta)

	if got := fx.heldFrames("fork-thread"); got != 0 || deadlines.count() != 0 {
		t.Fatalf("held %d frames and armed %d deadlines for a thread routed to B", got, deadlines.count())
	}
	mu.Lock()
	defer mu.Unlock()
	reached := false
	for _, event := range bEvents {
		reached = reached || event.ItemID == "fork-answer"
	}
	if !reached {
		t.Fatalf("the fork's frame did not reach B: %+v", bEvents)
	}
}

// TestForkAnnouncementWithOneSessionWaitsUnseen covers a revert's live-session
// fork when its session is the only one on the connection. Codex answers
// thread/fork and then announces the fork with a thread/started that names no
// parent and its own session. The connection hands it to the only session,
// which does not route the fork and so returns it to the connection's hold:
// the session keeps nothing for it and asks Codex nothing about it.
// The fork returns before the announcement, and either the session that
// resumes the fork receives it (which shows nothing) or the deadline drops it.
func TestForkAnnouncementWithOneSessionWaitsUnseen(t *testing.T) {
	for _, resumed := range []bool{true, false} {
		name := "deadline"
		if resumed {
			name = "resumed"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fx, s, events := recordingRootSession(t)
			deadlines := fx.deadlines()
			version := "0.169.0"
			s.appServerVersion.Store(&version)
			fx.answer("thread/fork", `{"thread":{"id":"fork-thread"}}`)

			ctx, cancel := context.WithTimeout(context.Background(), fixtureWait)
			defer cancel()
			forkID, err := s.ForkAt(ctx, "")
			if err != nil || forkID != "fork-thread" {
				t.Fatalf("ForkAt = %q, %v", forkID, err)
			}
			before := len(fx.written(1))
			fx.feed([]byte(`{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"fork-thread","sessionId":"fork-session"}}}`))
			if got := fx.heldFrames("fork-thread"); got != 1 {
				t.Fatalf("connection held %d fork frames, want 1", got)
			}
			s.collabAsyncWG.Wait()
			if after := len(fx.written(before)); after != before {
				t.Fatalf("the announcement made the connection write %d lines: %q", after-before, fx.written(after)[before:])
			}
			if got := events(); len(got) != 0 {
				t.Fatalf("the source thread showed the fork announcement: %+v", got)
			}

			if resumed {
				var revertedEvents []provider.ProviderEvent
				fx.session("ao-thread", "fork-thread", func(event provider.ProviderEvent) { revertedEvents = append(revertedEvents, event) })
				if got := fx.heldFrames("fork-thread"); got != 0 {
					t.Fatalf("connection still holds %d fork frames after the resume", got)
				}
				if len(revertedEvents) != 0 {
					t.Fatalf("the reverted thread showed the fork announcement: %+v", revertedEvents)
				}
			} else {
				deadlines.fire(t, 0)
				if got := fx.heldFrames("fork-thread"); got != 0 {
					t.Fatalf("connection still holds %d fork frames after the deadline", got)
				}
			}
			if got := events(); len(got) != 0 {
				t.Fatalf("the source thread showed the fork announcement: %+v", got)
			}
		})
	}
}
