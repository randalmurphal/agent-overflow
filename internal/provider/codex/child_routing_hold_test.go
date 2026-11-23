package codex

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

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

func (fx *connFixture) heldFrames(providerThreadID string) int {
	fx.conn.routesMu.Lock()
	defer fx.conn.routesMu.Unlock()
	return len(fx.conn.deferredChild[providerThreadID])
}

// TestLateSpawnDrainsSameSessionChildFromTheConnection runs a same-session
// child through the connection: its thread/started reaches the session by the
// root's session id and is handed back to the connection, and its later
// frames carry only its threadId and wait there too. The late spawn releases
// both from the one queue, with the child's answer under the spawn.
func TestLateSpawnDrainsSameSessionChildFromTheConnection(t *testing.T) {
	t.Parallel()
	fx, _, events := recordingRootSession(t)

	fx.feed([]byte(`{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"child-a","sessionId":"root-session","parentThreadId":"root-provider-thread"}}}`))
	fx.feed([]byte(`{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"child-a","turnId":"child-turn","itemId":"answer","delta":"owned"}}`))
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

// TestChildFrameHandedAfterItsSpawnDrainedIsReleased is the window between
// a session finding a thread unowned and handing its frame to the
// connection: a spawn recorded and drained in that window has already
// emptied the thread's queue, so the frame must be released by the hand-off
// itself rather than wait for a deadline.
func TestChildFrameHandedAfterItsSpawnDrainedIsReleased(t *testing.T) {
	t.Parallel()
	fx, s, events := recordingRootSession(t)
	if !s.registerChildOwnership("root-provider-thread", "child-a", "/root/child-a", "spawn-a") {
		t.Fatal("register child ownership")
	}
	s.drainDeferredChildWireEvents("child-a")

	held := s.handChildFrame("child-a", deferredChildWireEvent{
		Method: "item/agentMessage/delta",
		Params: []byte(`{"threadId":"child-a","turnId":"child-turn","itemId":"answer","delta":"late"}`),
	}, nil)
	if !held {
		t.Fatal("connection refused the frame")
	}
	if got := fx.heldFrames("child-a"); got != 0 {
		t.Fatalf("connection still holds %d frames for a claimed thread", got)
	}
	for _, event := range events() {
		if event.ItemID == "answer" && event.ParentToolUseID == "spawn-a" {
			return
		}
	}
	t.Fatalf("frame handed after its spawn drained was not delivered: %+v", events())
}

// TestHeldThreadExpiryIsLoggedNotShown pins what the deadline does with a
// thread no spawn claimed: its request is rejected, its notification dropped,
// nothing reaches any thread, and one log line names the thread, both counts
// and every session on the connection, since a held frame names no root.
func TestHeldThreadExpiryIsLoggedNotShown(t *testing.T) {
	fx, _, events := recordingRootSession(t)
	var otherEvents []provider.ProviderEvent
	fx.session("ao-thread-2", "other-root", func(event provider.ProviderEvent) { otherEvents = append(otherEvents, event) })
	fx.feed([]byte(`{"jsonrpc":"2.0","id":51,"method":"item/commandExecution/requestApproval","params":{"threadId":"child-x","turnId":"t","itemId":"cmd","command":"go test"}}`))
	fx.feed([]byte(`{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"child-x","turnId":"t","itemId":"m","delta":"x"}}`))

	logged := captureLog(t, func() { fx.conn.expireDeferredChild("child-x") })

	want := "codex: no spawn claimed provider thread child-x before the routing deadline: rejected 1 requests, dropped 1 notifications; sessions on the connection: [ao-thread ao-thread-2]"
	if !strings.Contains(logged, want) {
		t.Fatalf("expiry log = %q, want it to contain %q", logged, want)
	}
	rejected := false
	for _, line := range fx.written(1) {
		var response struct {
			ID    int64 `json:"id"`
			Error *struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(line, &response) == nil && response.ID == 51 && response.Error != nil && response.Error.Code == -32000 {
			rejected = true
		}
	}
	if !rejected {
		t.Fatalf("held request 51 was not rejected: %q", fx.written(1))
	}
	if len(events()) != 0 || len(otherEvents) != 0 {
		t.Fatalf("expiry reached a thread: %+v %+v", events(), otherEvents)
	}
}

// TestForkAnnouncementWithOneSessionWaitsUnseen covers a revert's live-session
// fork when its session is the only one on the connection. Codex answers
// thread/fork and then announces the fork with a thread/started that names no
// parent, which the connection routes to the only session; that session does
// not own the fork, so the frame waits on the connection. The fork returns
// before the announcement, the source thread shows nothing while the frame
// waits, and either the reverted session that resumes the fork receives the
// announcement (which shows nothing either) or the deadline drops it.
func TestForkAnnouncementWithOneSessionWaitsUnseen(t *testing.T) {
	for _, resumed := range []bool{true, false} {
		name := "deadline"
		if resumed {
			name = "resumed"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fx, s, events := recordingRootSession(t)
			version := "0.169.0"
			s.appServerVersion.Store(&version)
			fx.answer("thread/fork", `{"thread":{"id":"fork-thread"}}`)
			recovered := make(chan struct{})
			fx.answer("thread/read", func(json.RawMessage) any {
				defer close(recovered)
				return `{"thread":{"id":"fork-thread"}}`
			})

			ctx, cancel := context.WithTimeout(context.Background(), fixtureWait)
			defer cancel()
			forkID, err := s.ForkAt(ctx, "")
			if err != nil || forkID != "fork-thread" {
				t.Fatalf("ForkAt = %q, %v", forkID, err)
			}
			fx.feed([]byte(`{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"fork-thread","sessionId":"fork-session"}}}`))
			if got := fx.heldFrames("fork-thread"); got != 1 {
				t.Fatalf("connection held %d fork frames, want 1", got)
			}
			// The only session looks for the fork's spawn, finds no parent,
			// and keeps waiting for one until the deadline.
			<-recovered
			s.collabAsyncWG.Wait()
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
				fx.conn.expireDeferredChild("fork-thread")
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
