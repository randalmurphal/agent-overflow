package codex

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

func rootDelta(text string) json.RawMessage {
	return json.RawMessage(`{"threadId":"root-provider-thread","turnId":"root-turn","itemId":"m1","delta":"` + text + `"}`)
}

func textDeltas(events []provider.ProviderEvent) []string {
	var texts []string
	for _, e := range events {
		if e.Kind == provider.EventTextDelta {
			texts = append(texts, e.Content)
		}
	}
	return texts
}

// While a publisher waits for the dispatch token, a frame that arrives
// queues instead of taking the token, so a busy thread cannot starve the
// publisher; the publisher delivers it once it has the token.
func TestAFrameQueuesWhileAPublisherWaitsForTheToken(t *testing.T) {
	t.Parallel()
	var log eventLog
	s := newMultiAgentV2RoutingSession(t, log.add)
	s.earlyFramesMu.Lock()
	s.dispatchWaiters = 1
	s.earlyFramesMu.Unlock()

	s.dispatchNotification("item/agentMessage/delta", rootDelta("queued"))
	s.flushEarlyFrames()
	if texts := textDeltas(log.snapshot()); len(texts) != 0 {
		t.Fatalf("dispatched %v while a publisher waited for the token", texts)
	}
	s.earlyFramesMu.Lock()
	s.dispatchWaiters = 0
	s.dispatching = true
	s.earlyFramesMu.Unlock()
	s.endDispatch()
	if texts := textDeltas(log.snapshot()); !sameOrder(texts, []string{"queued"}) {
		t.Fatalf("delivered = %v once the publisher had the token, want the queued frame", texts)
	}
}

// A publisher waiting for a token another goroutine holds gives up, holding
// nothing, when the session closes, so it never keeps Close waiting.
func TestAPublisherWaitingForTheTokenGivesUpWhenTheSessionCloses(t *testing.T) {
	t.Parallel()
	s := newMultiAgentV2RoutingSession(t, nil)
	s.earlyFramesMu.Lock()
	s.dispatching = true
	s.earlyFramesMu.Unlock()
	took := make(chan bool, 1)
	go func() { took <- s.takeDispatch() }()
	select {
	case got := <-took:
		t.Fatalf("takeDispatch = %v while another goroutine held the token, want it to wait", got)
	case <-time.After(50 * time.Millisecond):
	}
	s.closing.Store(true)
	s.earlyFramesMu.Lock()
	s.earlyFramesCondLocked().Broadcast()
	s.earlyFramesMu.Unlock()
	select {
	case got := <-took:
		if got {
			t.Fatal("takeDispatch took the token while another goroutine held it")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("takeDispatch kept waiting after the session closed")
	}
}

// The queue behind a token holder is bounded: a frame that finds it full
// waits for the holder to take a batch, and is delivered in order after it.
// A closing session does not keep the reader waiting and keeps the bound:
// it refuses the frame and counts it for Close's error.
func TestAFullQueueHoldsTheReaderUntilTheHolderTakesABatch(t *testing.T) {
	t.Parallel()
	for _, closing := range []bool{false, true} {
		var log eventLog
		s := newMultiAgentV2RoutingSession(t, log.add)
		s.earlyFramesMu.Lock()
		s.dispatching = true
		s.earlyFramesMu.Unlock()
		for i := 0; i < earlyFramesMaxBuffered; i++ {
			s.dispatchNotification("item/agentMessage/delta", rootDelta(strconv.Itoa(i)))
		}
		last := make(chan struct{})
		go func() {
			defer close(last)
			s.dispatchNotification("item/agentMessage/delta", rootDelta("last"))
		}()
		select {
		case <-last:
			t.Fatalf("closing=%v: a frame joined the full queue without waiting", closing)
		case <-time.After(50 * time.Millisecond):
		}
		if closing {
			s.closing.Store(true)
			s.earlyFramesMu.Lock()
			s.earlyFramesCondLocked().Broadcast()
			s.earlyFramesMu.Unlock()
			select {
			case <-last:
			case <-time.After(5 * time.Second):
				t.Fatal("a frame kept waiting on the full queue after the session closed")
			}
			s.earlyFramesMu.Lock()
			queued := len(s.earlyFrames)
			s.earlyFramesMu.Unlock()
			if queued != earlyFramesMaxBuffered {
				t.Fatalf("closing: %d frames queued after the session closed, want the bound %d", queued, earlyFramesMaxBuffered)
			}
		}
		s.endDispatch()
		<-last
		s.earlyFramesMu.Lock()
		held, queued := s.dispatching, len(s.earlyFrames)
		s.earlyFramesMu.Unlock()
		if held || queued != 0 {
			t.Fatalf("closing=%v: after the holder let go: token held %v, %d frames queued; want neither", closing, held, queued)
		}
		texts := textDeltas(log.snapshot())
		want, dropped := earlyFramesMaxBuffered+1, 0
		if closing {
			want, dropped = earlyFramesMaxBuffered, 1
		}
		if len(texts) != want || closing == (texts[len(texts)-1] == "last") {
			t.Fatalf("closing=%v: delivered %v, want %d frames", closing, texts, want)
		}
		if got := s.takeDroppedFrames(); got != dropped {
			t.Fatalf("closing=%v: refused frames counted = %d, want %d", closing, got, dropped)
		}
	}
}

// A holder that delivers the queue lets the token go after the batch in
// hand when a publisher waits for it, so a thread whose frames keep coming
// cannot keep the publisher out; the publisher then delivers the rest.
func TestAHolderGivesTheTokenToAWaitingPublisherAfterTheBatchInHand(t *testing.T) {
	t.Parallel()
	var (
		s       *Session
		orderMu sync.Mutex
		order   []string
	)
	note := func(step string) {
		orderMu.Lock()
		defer orderMu.Unlock()
		order = append(order, step)
	}
	took := make(chan bool, 1)
	s = newMultiAgentV2RoutingSession(t, func(evt provider.ProviderEvent) {
		if evt.Kind != provider.EventTextDelta {
			return
		}
		note(evt.Content)
		// Each frame delivered brings another, as a busy thread does.
		if evt.Content == "first" {
			go s.dispatchNotification("item/agentMessage/delta", rootDelta("second"))
			deadline := time.Now().Add(5 * time.Second)
			for {
				s.earlyFramesMu.Lock()
				n := len(s.earlyFrames)
				s.earlyFramesMu.Unlock()
				if n == 1 || time.Now().After(deadline) {
					return
				}
				time.Sleep(time.Millisecond)
			}
		}
	})
	s.earlyFramesMu.Lock()
	s.dispatching = true
	s.earlyFramesMu.Unlock()
	s.dispatchNotification("item/agentMessage/delta", rootDelta("first"))
	published := make(chan struct{})
	go func() {
		defer close(published)
		ok := s.takeDispatch()
		took <- ok
		if ok {
			note("publisher")
			s.endDispatch()
		}
	}()
	for {
		s.earlyFramesMu.Lock()
		waiting := s.dispatchWaiters
		s.earlyFramesMu.Unlock()
		if waiting == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	s.endDispatch()
	if !<-took {
		t.Fatal("the waiting publisher did not get the token")
	}
	<-published
	orderMu.Lock()
	got := append([]string(nil), order...)
	orderMu.Unlock()
	if !sameOrder(got, []string{"first", "publisher", "second"}) {
		t.Fatalf("order = %v, want the publisher's turn between the batch in hand and the frame that came during it", got)
	}
}

// DispatchReadFrames returns once the frames that reached the session before
// it are dispatched, the ones a holder still had queued included, and gives
// up with an error at its context's end.
func TestDispatchReadFramesWaitsForTheFramesBeforeIt(t *testing.T) {
	t.Parallel()
	var log eventLog
	s := newMultiAgentV2RoutingSession(t, log.add)
	s.earlyFramesMu.Lock()
	s.dispatching = true
	s.earlyFramesMu.Unlock()
	s.dispatchNotification("item/agentMessage/delta", rootDelta("queued"))

	expired, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := s.DispatchReadFrames(expired); err == nil {
		t.Fatal("DispatchReadFrames = nil while the holder still had the frame queued")
	}
	done := make(chan error, 1)
	go func() { done <- s.DispatchReadFrames(context.Background()) }()
	select {
	case err := <-done:
		t.Fatalf("DispatchReadFrames = %v before the holder delivered the frame", err)
	case <-time.After(20 * time.Millisecond):
	}
	s.endDispatch()
	if err := <-done; err != nil {
		t.Fatalf("DispatchReadFrames: %v", err)
	}
	if texts := textDeltas(log.snapshot()); !sameOrder(texts, []string{"queued"}) {
		t.Fatalf("delivered = %v, want the queued frame", texts)
	}
}
