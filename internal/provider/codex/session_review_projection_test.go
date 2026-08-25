package codex

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

func newReviewProjectionSession(events *[]provider.ProviderEvent) *Session {
	s := &Session{
		threadID: "ao-thread",
		pending:  make(map[int64]chan json.RawMessage),
		onEvent: func(event provider.ProviderEvent) {
			*events = append(*events, event)
		},
		review: &reviewRun{
			turnIndex:     7,
			target:        ReviewUncommittedChanges(),
			model:         "review-model",
			responseBound: true,
		},
		turn: sessionTurnState{
			schemaedTurnIDs:        make(map[string]struct{}),
			structuredOutputByTurn: make(map[string]json.RawMessage),
		},
		origins: sessionTurnOrigins{
			byTurn:                 make(map[string]turnOrigin),
			pendingLocalTurnStarts: 1,
		},
		turnConfig: sessionTurnConfig{
			model: "parent-model",
		},
	}
	s.setRootThreadID("codex-thread")
	return s
}

func TestReviewProjectionCanCompleteBeforeStartResponseIsRead(t *testing.T) {
	var events []provider.ProviderEvent
	s := newReviewProjectionSession(&events)
	s.review.responseBound = false
	s.handleReviewSpecialNotification("item/started", json.RawMessage(`{
		"threadId":"codex-thread","turnId":"outer-turn",
		"item":{"type":"enteredReviewMode","id":"enter-1","review":"Review"}
	}`))
	s.handleReviewSpecialNotification("turn/completed", json.RawMessage(`{
		"threadId":"codex-thread","turn":{"id":"outer-turn","status":"completed","items":[]}
	}`))
	s.mu.Lock()
	if s.review == nil || !s.review.completed {
		state := s.review
		s.mu.Unlock()
		t.Fatalf("completed review was released before review/start response: %+v", state)
	}
	s.mu.Unlock()

	if err := s.bindReviewResponse("outer-turn"); err != nil {
		t.Fatalf("bindReviewResponse: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.review != nil {
		t.Fatalf("review reservation survived response bind: %+v", s.review)
	}
}

func TestReviewProjectionKeepsControlTurnPrivateAndScopesActivity(t *testing.T) {
	var events []provider.ProviderEvent
	s := newReviewProjectionSession(&events)

	if !s.handleReviewSpecialNotification("item/started", json.RawMessage(`{
		"threadId":"codex-thread","turnId":"outer-turn",
		"item":{"type":"enteredReviewMode","id":"enter-1","review":"Review the working tree"}
	}`)) {
		t.Fatal("enteredReviewMode was not consumed")
	}
	if len(events) != 2 || events[0].Kind != provider.EventTurnStart || events[1].Kind != provider.EventToolStart {
		t.Fatalf("entered events = %#v, want visible turn then launch", events)
	}
	launchID := events[1].ItemID
	if launchID != "codex-review:enter-1" || events[1].TurnIndex != 7 {
		t.Fatalf("launch = %+v", events[1])
	}

	if !s.handleReviewSpecialNotification("turn/started", json.RawMessage(`{
		"threadId":"codex-thread","turn":{"id":"private-turn","status":"inProgress","items":[]}
	}`)) {
		t.Fatal("private review turn was not consumed")
	}
	s.mu.Lock()
	activeTurnID := s.turn.activeTurnID
	s.mu.Unlock()
	if activeTurnID != "private-turn" {
		t.Fatalf("activeTurnID = %q, want private interrupt authority", activeTurnID)
	}
	if len(events) != 2 {
		t.Fatalf("private turn leaked a visible event: %#v", events[2:])
	}

	toolEvents := ClassifyNotification("ao-thread", "item/started", json.RawMessage(`{
		"threadId":"codex-thread","turnId":"outer-turn",
		"item":{"type":"commandExecution","id":"cmd-1","command":"git diff","status":"inProgress"}
	}`))
	toolEvents = s.scopeReviewEvents(toolEvents)
	if len(toolEvents) != 1 || toolEvents[0].ParentToolUseID != launchID || toolEvents[0].TurnIndex != 7 {
		t.Fatalf("scoped tool events = %+v", toolEvents)
	}
}

func TestReviewProjectionFlushesIntermediateProseAndPublishesOneSourcedResult(t *testing.T) {
	var events []provider.ProviderEvent
	s := newReviewProjectionSession(&events)
	s.handleReviewSpecialNotification("item/started", json.RawMessage(`{
		"threadId":"codex-thread","turnId":"outer-turn",
		"item":{"type":"enteredReviewMode","id":"enter-1","review":"Review the working tree"}
	}`))
	launchID := events[1].ItemID

	s.handleReviewSpecialNotification("item/started", json.RawMessage(`{
		"threadId":"codex-thread","turnId":"outer-turn",
		"item":{"type":"agentMessage","id":"prose-1","text":"I am checking the locking."}
	}`))
	s.handleReviewSpecialNotification("item/started", json.RawMessage(`{
		"threadId":"codex-thread","turnId":"outer-turn",
		"item":{"type":"agentMessage","id":"raw-final","text":"{\"findings\":[]}"}
	}`))
	if len(events) != 3 || events[2].Kind != provider.EventContentBlockStop || events[2].Content != "I am checking the locking." {
		t.Fatalf("intermediate prose events = %#v", events)
	}
	if events[2].ParentToolUseID != launchID {
		t.Fatalf("intermediate prose scope = %q, want %q", events[2].ParentToolUseID, launchID)
	}

	s.handleReviewSpecialNotification("item/completed", json.RawMessage(`{
		"threadId":"codex-thread","turnId":"outer-turn",
		"item":{"type":"exitedReviewMode","id":"exit-1","review":"No findings."}
	}`))
	s.handleReviewSpecialNotification("item/started", json.RawMessage(`{
		"threadId":"codex-thread","turnId":"outer-turn",
		"item":{"type":"agentMessage","id":"answer-1","text":"No findings."}
	}`))
	s.handleReviewSpecialNotification("item/completed", json.RawMessage(`{
		"threadId":"codex-thread","turnId":"outer-turn",
		"item":{"type":"agentMessage","id":"answer-1","text":"No findings."}
	}`))
	s.handleReviewSpecialNotification("turn/completed", json.RawMessage(`{
		"threadId":"codex-thread","turn":{"id":"outer-turn","status":"completed","items":[]}
	}`))

	var childResults, commandResults, completes, turnCompletes int
	for _, event := range events {
		switch event.Kind {
		case provider.EventContentBlockStop:
			if event.Content == "No findings." && event.ParentToolUseID == launchID {
				childResults++
			}
		case provider.EventCommandResult:
			commandResults++
			if event.Content != "No findings." || event.ParentToolUseID != "" {
				t.Errorf("command result = %+v", event)
			}
			var meta provider.CommandResultMeta
			if err := json.Unmarshal(event.Meta, &meta); err != nil {
				t.Fatalf("decode command result meta: %v", err)
			}
			if meta.AgentResult == nil || meta.AgentResult.LaunchID != launchID || meta.AgentResult.SourceKind != "review" {
				t.Errorf("agent result meta = %+v", meta.AgentResult)
			}
		case provider.EventToolComplete:
			completes++
		case provider.EventTurnComplete:
			turnCompletes++
		}
	}
	if childResults != 1 || commandResults != 1 || completes != 1 || turnCompletes != 1 {
		t.Fatalf(
			"terminal counts child=%d command=%d tool=%d turn=%d; events=%#v",
			childResults, commandResults, completes, turnCompletes, events,
		)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.review != nil || s.turn.activeTurnID != "" {
		t.Fatalf("terminal state leaked: review=%+v active=%q", s.review, s.turn.activeTurnID)
	}
}

func TestReviewProjectionMarksInterruptedLaunchStopped(t *testing.T) {
	var events []provider.ProviderEvent
	s := newReviewProjectionSession(&events)
	s.handleReviewSpecialNotification("item/started", json.RawMessage(`{
		"threadId":"codex-thread","turnId":"outer-turn",
		"item":{"type":"enteredReviewMode","id":"enter-1","review":"Review"}
	}`))
	s.handleReviewSpecialNotification("turn/completed", json.RawMessage(`{
		"threadId":"codex-thread","turn":{"id":"outer-turn","status":"interrupted","items":[]}
	}`))
	for _, event := range events {
		if event.Kind != provider.EventToolComplete {
			continue
		}
		var meta struct {
			ItemStatus string `json:"item_status"`
		}
		if err := json.Unmarshal(event.Meta, &meta); err != nil {
			t.Fatal(err)
		}
		if meta.ItemStatus != "killed" {
			t.Fatalf("item_status = %q, want killed", meta.ItemStatus)
		}
		return
	}
	t.Fatal("no review tool completion")
}

func TestReviewProjectionKeepsAReportedResultSuccessfulAfterProviderFailure(t *testing.T) {
	var events []provider.ProviderEvent
	s := newReviewProjectionSession(&events)
	s.handleReviewSpecialNotification("item/started", json.RawMessage(`{
		"threadId":"codex-thread","turnId":"outer-turn",
		"item":{"type":"enteredReviewMode","id":"enter-1","review":"Review"}
	}`))
	s.handleReviewSpecialNotification("item/completed", json.RawMessage(`{
		"threadId":"codex-thread","turnId":"outer-turn",
		"item":{"type":"exitedReviewMode","id":"exit-1","review":"One finding."}
	}`))
	s.handleReviewSpecialNotification("item/completed", json.RawMessage(`{
		"threadId":"codex-thread","turnId":"outer-turn",
		"item":{"type":"agentMessage","id":"answer-1","text":"One finding."}
	}`))
	s.handleReviewSpecialNotification("turn/completed", json.RawMessage(`{
		"threadId":"codex-thread","turn":{"id":"outer-turn","status":"failed","error":{"message":"report delivery failed"},"items":[]}
	}`))

	for _, event := range events {
		switch event.Kind {
		case provider.EventCommandResult:
			if event.Content != "One finding." {
				t.Fatalf("result = %q", event.Content)
			}
		case provider.EventToolComplete:
			var meta struct {
				ItemStatus string `json:"item_status"`
			}
			if err := json.Unmarshal(event.Meta, &meta); err != nil {
				t.Fatal(err)
			}
			if meta.ItemStatus != "completed" {
				t.Fatalf("review launch status = %q, want completed", meta.ItemStatus)
			}
		case provider.EventTurnComplete:
			complete := event.TurnComplete.(*provider.WireTurnCompleteMeta)
			if complete.StopReason != "end_turn" || event.Failure != nil {
				t.Fatalf("logical review completion = %+v, failure=%+v", complete, event.Failure)
			}
		}
	}
}
