package harnessclient

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"agent-overflow/internal/testutil/reporoot"
	"agent-overflow/internal/transport"
)

// The page's default send is the reference the client's options are
// checked against: frontend/src/test/fixtures/pageDefaultSend.json, which
// a frontend test holds to buildSendOptions. Only the send id differs.
func TestPageSendOptionsMatchThePageDefaultSend(t *testing.T) {
	raw, err := os.ReadFile(reporoot.Path(t, "frontend/src/test/fixtures/pageDefaultSend.json"))
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("decode the page's default send: %v", err)
	}
	encoded, err := json.Marshal(pageSendOptions("send-1"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if got["sendId"] != "send-1" {
		t.Fatalf("sendId = %v, want the one given", got["sendId"])
	}
	delete(got, "sendId")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("options = %s\nthe page's default send = %s", encoded, raw)
	}
}

// The completion SendAndAwaitTurn answers is the one for the turn of the
// row its send wrote. A completion the thread reports first, announced
// late for an earlier turn, and a row another send wrote are not it.
func TestSendAndAwaitTurnAnswersTheTurnOfItsOwnRow(t *testing.T) {
	backend := newFakeBackend(t)
	backend.handle = func(frame transport.ClientFrame) (json.RawMessage, *transport.FrameError) {
		switch frame.Method {
		case ThreadWorkMethod:
			return json.RawMessage(`[]`), nil
		case SendMethod:
			var options struct {
				SendID string `json:"sendId"`
			}
			if len(frame.Params) != 3 || json.Unmarshal(frame.Params[2], &options) != nil || options.SendID == "" {
				t.Errorf("send params = %s, want thread, text and options with a send id", frame.Params)
			}
			row := func(turn int, sendID string) map[string]any {
				meta, _ := json.Marshal(map[string]string{"sendId": sendID})
				return map[string]any{"action": "upsert", "threadId": "t1", "item": map[string]any{
					"id": "user:" + sendID, "threadId": "t1", "turnIndex": turn, "kind": "user_text", "meta": string(meta),
				}}
			}
			backend.pushEvent(t, "provider:turn_completed", map[string]any{"threadId": "t1", "turnIndex": 2})
			backend.pushEvent(t, "provider:item_event", row(2, "someone-else"))
			backend.pushEvent(t, "provider:item_event", row(3, options.SendID))
			backend.pushEvent(t, "provider:turn_completed", map[string]any{"threadId": "t1", "turnIndex": 3})
			return json.RawMessage(`{}`), nil
		default:
			return json.RawMessage(`null`), nil
		}
	}
	client := backend.dial(t)
	resting, err := client.ThreadAtRest(testContext(t), "t1")
	if err != nil {
		t.Fatalf("ThreadAtRest: %v", err)
	}
	event, err := resting.SendAndAwaitTurn(testContext(t), "hello")
	if err != nil {
		t.Fatalf("SendAndAwaitTurn: %v", err)
	}
	var completed struct {
		TurnIndex int `json:"turnIndex"`
	}
	if err := json.Unmarshal(event.Data, &completed); err != nil {
		t.Fatal(err)
	}
	if completed.TurnIndex != 3 {
		t.Fatalf("answered the completion of turn %d, want 3, the turn of the row the send wrote", completed.TurnIndex)
	}
}
