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
	t.Parallel()
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
	t.Parallel()
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

// A review's commands come on the root thread's frames but run in the
// review delegate's process table, which no background-terminal call
// reaches: they carry the review run's scope, not the root thread's, and the
// run's end reports that table ended, whether the review completed or was
// interrupted.
func TestReviewCommandsRunInTheReviewRunsTableAndEndWithIt(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"completed", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			var events []provider.ProviderEvent
			s := newReviewProjectionSession(&events)
			s.handleReviewSpecialNotification("item/started", json.RawMessage(`{
				"threadId":"codex-thread","turnId":"outer-turn",
				"item":{"type":"enteredReviewMode","id":"enter-1","review":"Review"}
			}`))
			launchID := events[1].ItemID
			command := func(turnID string) provider.ProviderEvent {
				t.Helper()
				scoped := ClassifyNotification("ao-thread", "item/started", json.RawMessage(`{
					"threadId":"codex-thread","turnId":"`+turnID+`",
					"item":{"type":"commandExecution","id":"cmd-`+turnID+`","source":"unifiedExecStartup","processId":"1001","command":"sleep 60","status":"inProgress"}
				}`))
				s.stampProcessScope(scoped, "codex-thread")
				scoped = s.scopeReviewEvents(scoped)
				if len(scoped) != 1 {
					t.Fatalf("command events = %+v", scoped)
				}
				return scoped[0]
			}
			if got := command("outer-turn").ProcessScope; got != reviewProcessScope(launchID) || got.Addressable {
				t.Fatalf("review command scope = %+v, want the review run's, not addressable", got)
			}
			if got := command("other-turn").ProcessScope; got != (provider.ProcessScope{ID: "codex-thread", Addressable: true}) {
				t.Fatalf("root command scope = %+v, want the root thread's", got)
			}

			events = nil
			s.handleReviewSpecialNotification("turn/completed", json.RawMessage(`{
				"threadId":"codex-thread","turn":{"id":"outer-turn","status":"`+status+`","items":[]}
			}`))
			ended := -1
			for i, event := range events {
				if event.Kind == provider.EventSubagentStatus && event.ProcessScope == reviewProcessScope(launchID) &&
					event.ParentToolUseID == launchID && string(event.Meta) == `{"agent_path":"`+reviewProcessScope(launchID).ID+`","status":"notLoaded"}` {
					ended = i
				}
			}
			if ended < 0 {
				t.Fatalf("review end events = %+v, want the review run's table reported ended", events)
			}
		})
	}
}

func TestReviewProjectionFlushesIntermediateProseAndPublishesOneSourcedResult(t *testing.T) {
	t.Parallel()
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
		if event.Kind == provider.EventContentBlockStop {
			if event.Content == "No findings." && event.ParentToolUseID == launchID {
				childResults++
			}
		} else if event.Kind == provider.EventCommandResult {
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
		} else if event.Kind == provider.EventToolComplete {
			completes++
		} else if event.Kind == provider.EventTurnComplete {
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
	t.Parallel()
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
	t.Parallel()
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

	var results, launches, turns int
	for _, event := range events {
		if event.Kind == provider.EventCommandResult {
			results++
			if event.Content != "One finding." {
				t.Fatalf("result = %q", event.Content)
			}
		} else if event.Kind == provider.EventToolComplete {
			launches++
			var meta struct {
				ItemStatus string `json:"item_status"`
			}
			if err := json.Unmarshal(event.Meta, &meta); err != nil {
				t.Fatal(err)
			}
			if meta.ItemStatus != "completed" {
				t.Fatalf("review launch status = %q, want completed", meta.ItemStatus)
			}
		} else if event.Kind == provider.EventTurnComplete {
			turns++
			complete := event.TurnComplete.(*provider.WireTurnCompleteMeta)
			if complete.StopReason != "end_turn" || event.Failure != nil {
				t.Fatalf("logical review completion = %+v, failure=%+v", complete, event.Failure)
			}
		}
	}
	if results != 1 || launches != 1 || turns != 1 {
		t.Fatalf("review ended with %d result(s), %d launch completion(s) and %d turn completion(s), want one of each", results, launches, turns)
	}
}
