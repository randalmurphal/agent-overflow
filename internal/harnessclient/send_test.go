package harnessclient

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"agent-overflow/internal/transport"
)

// The completion SendAndAwaitTurn answers is the one for the turn the send
// opened, sequenced after the backend read its floor. An earlier turn that
// held the same index completed below the floor and is not it; a completion
// that lands inside the send's own round trip is. Completions are the only
// traffic it subscribes to.
func TestSendAndAwaitTurnAnswersTheCompletionAfterItsSend(t *testing.T) {
	backend := newFakeBackend(t)
	sent := make(chan []json.RawMessage, 1)
	backend.handle = func(frame transport.ClientFrame) (json.RawMessage, *transport.FrameError) {
		if frame.Method != SendMethod {
			return json.RawMessage(`null`), nil
		}
		sent <- frame.Params
		completion := func(turn int) map[string]any { return map[string]any{"threadId": "t1", "turnIndex": turn} }
		// Turn 3 completed, was discarded, and its index is reused.
		backend.pushEvent(t, "provider:turn_completed", completion(3))
		backend.pushEvent(t, "provider:turn_completed", map[string]any{"threadId": "t2", "turnIndex": 3})
		backend.mu.Lock()
		floor := backend.seq
		backend.mu.Unlock()
		backend.pushEvent(t, "provider:turn_completed", completion(2))
		backend.pushEvent(t, "provider:turn_completed", completion(3))
		return json.RawMessage(fmt.Sprintf(`{"queued":false,"turnIndex":3,"completionsAfter":%d}`, floor)), nil
	}
	client := backend.dial(t)
	event, err := client.SendAndAwaitTurn(testContext(t), "t1", "hello")
	if err != nil {
		t.Fatalf("SendAndAwaitTurn: %v", err)
	}
	if event.Seq != 4 {
		t.Fatalf("answered the completion at sequence %d (%s), want 4, turn 3's after the send", event.Seq, event.Data)
	}
	if got, want := rawStrings(<-sent), []string{`"t1"`, `"hello"`, `true`}; !slices.Equal(got, want) {
		t.Fatalf("send params = %v, want %v: the thread, the text, and admitted only at rest", got, want)
	}
	if subs := backend.subscriptions(); !slices.Equal(subs, []string{"provider:turn_completed"}) {
		t.Fatalf("subscriptions = %v, want provider:turn_completed alone", subs)
	}
}

// Send queues behind a working thread's work: it is not admitted only at
// rest.
func TestSendQueuesBehindWork(t *testing.T) {
	backend := newFakeBackend(t)
	sent := make(chan []json.RawMessage, 1)
	backend.handle = func(frame transport.ClientFrame) (json.RawMessage, *transport.FrameError) {
		sent <- frame.Params
		return json.RawMessage(`{"queued":true,"turnIndex":0,"completionsAfter":0}`), nil
	}
	client := backend.dial(t)
	if err := client.Send(testContext(t), "t1", "hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got, want := rawStrings(<-sent), []string{`"t1"`, `"hello"`, `false`}; !slices.Equal(got, want) {
		t.Fatalf("send params = %v, want %v", got, want)
	}
}

func rawStrings(raw []json.RawMessage) []string {
	out := make([]string, len(raw))
	for i, r := range raw {
		out[i] = string(r)
	}
	return out
}
