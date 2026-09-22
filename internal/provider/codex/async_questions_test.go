package codex

import (
	"encoding/json"
	"testing"
	"testing/synctest"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/userquestion"
)

func TestAsyncQuestionIdentifiedAtStart(t *testing.T) {
	params := json.RawMessage(`{"threadId":"native","turnId":"turn","item":{"id":"call","type":"agentMessage","text":"Question prose","delivery":"async","questions":[{"title":"Which?","options":["A","B"]},{"title":"Details?"}]}}`)
	for _, method := range []string{"item/started", "item/completed"} {
		events, handled := classifyItemNotification("ao", method, params, time.Now())
		if !handled || len(events) != 1 {
			t.Fatalf("%s: %+v", method, events)
		}
		event := events[0]
		meta, structured, err := userquestion.Decode(event.Meta)
		if err != nil || !structured || len(meta.Questions) != 2 || event.ItemID != "call" {
			t.Fatalf("%s lost questions: %+v %v", method, event, err)
		}
		if method == "item/started" && event.Kind != provider.EventContentBlockStart {
			t.Fatal("question start took the prose/tool path")
		}
	}
}

func TestUserInputWaitsWithoutTimeoutUntilExplicitResolution(t *testing.T) {
	for _, blocking := range []string{"true", "false"} {
		t.Run(blocking, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, events := externalTurnTestSession(t, "native")
				s.dispatchLine([]byte(`{"jsonrpc":"2.0","id":7,"method":"item/tool/requestUserInput","params":{"threadId":"native","turnId":"turn","itemId":"ask","isBlocking":` + blocking + `,"autoResolutionMs":1,"questions":[{"id":"q","question":"Which?","header":"Scope"}]}}`))
				time.Sleep(24 * time.Hour)
				for _, event := range *events {
					if event.Kind == provider.EventUserInputResolved {
						t.Fatal("question expired without a response")
					}
				}
				if !s.approvals.Claim("7", provider.EventUserInputResolved) {
					t.Fatal("question was no longer answerable")
				}
			})
		})
	}
}

func TestProviderResolvedQuestionClosesOnlyItsOwnRequest(t *testing.T) {
	s, events := externalTurnTestSession(t, "native")
	request := func(id string) {
		s.dispatchLine([]byte(`{"jsonrpc":"2.0","id":` + id + `,"method":"item/tool/requestUserInput","params":{"threadId":"native","turnId":"turn","itemId":"ask-` + id + `","questions":[{"id":"q","question":"Which?","header":"Scope"}]}}`))
	}
	request("7")
	request("8")
	*events = nil
	resolved := []byte(`{"jsonrpc":"2.0","method":"serverRequest/resolved","params":{"threadId":"native","requestId":7}}`)
	s.dispatchLine(resolved)
	s.dispatchLine(resolved)
	if len(*events) != 1 || (*events)[0].Kind != provider.EventUserInputResolved || (*events)[0].ItemID != "7" {
		t.Fatalf("resolved request emitted wrong or duplicate event: %+v", *events)
	}
	if s.approvals.Claim("7", provider.EventUserInputResolved) {
		t.Fatal("provider-resolved question still accepts an answer")
	}
	if !s.approvals.Claim("8", provider.EventUserInputResolved) {
		t.Fatal("resolving one request cancelled another")
	}
	// A fresh request may reuse an ID. Once our response claims it, Codex's
	// acknowledgement must not overwrite the answer with a cancellation.
	request("7")
	if !s.approvals.Claim("7", provider.EventUserInputResolved) {
		t.Fatal("reused request ID was not answerable")
	}
	*events = nil
	s.dispatchLine(resolved)
	if len(*events) != 0 {
		t.Fatalf("own answer was resolved twice: %+v", *events)
	}
}
