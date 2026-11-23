package codex

import (
	"encoding/json"
	"strconv"
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
// A closing session lets it in without waiting.
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
		if len(texts) != earlyFramesMaxBuffered+1 || texts[len(texts)-1] != "last" {
			t.Fatalf("closing=%v: delivered %v, want %d frames ending with the frame that waited", closing, texts, earlyFramesMaxBuffered+1)
		}
	}
}
