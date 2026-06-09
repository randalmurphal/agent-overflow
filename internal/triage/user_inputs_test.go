package triage

import (
	"encoding/json"
	"testing"
	"time"

	"agent-overflow/internal/provider"
)

// TestClaudeAskUserQuestionAnswersPersistOnRunningLaunch is the ordering test
// for the Claude write-back: the user-input resolve merges the submitted
// answers onto the still-running AskUserQuestion launch row WITHOUT completing
// it, and the CLI's own later tool_result completion both settles the row and
// preserves those answers. Without this, the persisted card has no answers to
// render and shows "No answer recorded."
func TestClaudeAskUserQuestionAnswersPersistOnRunningLaunch(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1") // Provider: claude

	// Claude AskUserQuestion input carries no per-question id, so the header
	// is the key the answers come back under (normalizeUserInputQuestionID).
	questions := []provider.UserInputQuestion{{
		Header:   "Retention fix",
		Question: "Which approach?",
		Options: []provider.UserInputQuestionOption{
			{Label: "SECURITY DEFINER fn", Description: "recommended"},
			{Label: "Minimal degrade-only", Description: "defer"},
		},
	}}

	// 1. The AskUserQuestion tool_use creates the launch row; the questions
	//    live under meta.input (this is what the card already renders).
	mustHandle(t, router, provider.ProviderEvent{
		Kind:     provider.EventToolStart,
		ThreadID: "t1",
		TurnID:   "turn-1",
		ItemID:   "ask-1",
		ItemType: "AskUserQuestion",
		Meta: mustMarshalJSON(t, map[string]any{
			"toolName": "AskUserQuestion",
			"input":    map[string]any{"questions": questions},
		}),
		Timestamp: time.Now(),
	})

	// 2. The can_use_tool prompt registers the pending user-input request,
	//    keyed to the same tool_use id as the launch row.
	mustHandle(t, router, provider.ProviderEvent{
		Kind:     provider.EventUserInputRequest,
		ThreadID: "t1",
		TurnID:   "turn-1",
		ItemID:   "req-1",
		Meta: mustMarshalJSON(t, provider.UserInputRequest{
			RequestID: "req-1",
			ThreadID:  "t1",
			TurnID:    "turn-1",
			ToolUseID: "ask-1",
			ToolName:  "AskUserQuestion",
			Title:     "User Input Required",
			Questions: questions,
		}),
		Timestamp: time.Now(),
	})

	// 3. The user answers. Triage merges the answers onto the launch row but
	//    must leave status running so the CLI's tool_result still completes it.
	mustHandle(t, router, provider.ProviderEvent{
		Kind:     provider.EventUserInputResolved,
		ThreadID: "t1",
		ItemID:   "req-1",
		Meta: mustMarshalJSON(t, map[string]any{
			"requestId": "req-1",
			"decision":  "answered",
			"answers": map[string]provider.UserInputAnswer{
				"Retention fix": provider.SingleUserInputAnswer("SECURITY DEFINER fn"),
			},
		}),
		Timestamp: time.Now(),
	})

	launch, found, err := st.GetThreadItem("t1", "ask-1")
	if err != nil || !found {
		t.Fatalf("launch lookup after resolve: found=%v err=%v", found, err)
	}
	if launch.Status != statusRunning {
		t.Fatalf("status after resolve = %q, want running (CLI tool_result still pending)", launch.Status)
	}
	if got := answersFromMeta(t, launch.Meta)["Retention fix"]; len(got) != 1 || got[0] != "SECURITY DEFINER fn" {
		t.Fatalf("answers after resolve = %+v, want Retention fix=[SECURITY DEFINER fn]", got)
	}
	if !metaHasKey(t, launch.Meta, "input") {
		t.Fatal("resolve merge clobbered meta.input; the questions would disappear from the card")
	}

	// 4. The CLI's own tool_result completion arrives later. It settles the
	//    row AND must preserve the merged answers (deep meta merge).
	mustHandle(t, router, provider.ProviderEvent{
		Kind:     provider.EventToolComplete,
		ThreadID: "t1",
		ItemID:   "ask-1",
		ItemType: "AskUserQuestion",
		Meta: mustMarshalJSON(t, map[string]any{
			"toolName":    "AskUserQuestion",
			"tool_result": map[string]any{"content": "answer echo"},
		}),
		Timestamp: time.Now(),
	})

	completed, found, err := st.GetThreadItem("t1", "ask-1")
	if err != nil || !found {
		t.Fatalf("launch lookup after completion: found=%v err=%v", found, err)
	}
	if completed.Status != statusCompleted {
		t.Fatalf("status after completion = %q, want completed", completed.Status)
	}
	if got := answersFromMeta(t, completed.Meta)["Retention fix"]; len(got) != 1 || got[0] != "SECURITY DEFINER fn" {
		t.Fatalf("answers after completion = %+v, want preserved Retention fix=[SECURITY DEFINER fn]", got)
	}
	if !metaHasKey(t, completed.Meta, "tool_result") {
		t.Fatal("completion did not persist tool_result alongside answers")
	}
}

// TestClaudeAskUserQuestionAnswersMergeOntoCompletedLaunch covers the reverse
// (and far less likely) ordering: the CLI's tool_result completes the row
// before the resolve event is processed. The additive merge is status-agnostic,
// so the answers still land on the already-completed row and the resolve never
// reopens it.
func TestClaudeAskUserQuestionAnswersMergeOntoCompletedLaunch(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	questions := []provider.UserInputQuestion{{
		Header:   "Scope",
		Question: "Pick a scope",
		Options:  []provider.UserInputQuestionOption{{Label: "turn"}, {Label: "session"}},
	}}
	mustHandle(t, router, provider.ProviderEvent{
		Kind:     provider.EventToolStart,
		ThreadID: "t1",
		TurnID:   "turn-1",
		ItemID:   "ask-1",
		ItemType: "AskUserQuestion",
		Meta: mustMarshalJSON(t, map[string]any{
			"toolName": "AskUserQuestion",
			"input":    map[string]any{"questions": questions},
		}),
		Timestamp: time.Now(),
	})
	mustHandle(t, router, provider.ProviderEvent{
		Kind:     provider.EventUserInputRequest,
		ThreadID: "t1",
		TurnID:   "turn-1",
		ItemID:   "req-1",
		Meta: mustMarshalJSON(t, provider.UserInputRequest{
			RequestID: "req-1",
			ThreadID:  "t1",
			TurnID:    "turn-1",
			ToolUseID: "ask-1",
			ToolName:  "AskUserQuestion",
			Title:     "User Input Required",
			Questions: questions,
		}),
		Timestamp: time.Now(),
	})

	// Completion lands first.
	mustHandle(t, router, provider.ProviderEvent{
		Kind:     provider.EventToolComplete,
		ThreadID: "t1",
		ItemID:   "ask-1",
		ItemType: "AskUserQuestion",
		Meta: mustMarshalJSON(t, map[string]any{
			"toolName":    "AskUserQuestion",
			"tool_result": map[string]any{"content": "done"},
		}),
		Timestamp: time.Now(),
	})

	// Resolve lands second; answers must still merge onto the completed row.
	mustHandle(t, router, provider.ProviderEvent{
		Kind:     provider.EventUserInputResolved,
		ThreadID: "t1",
		ItemID:   "req-1",
		Meta: mustMarshalJSON(t, map[string]any{
			"requestId": "req-1",
			"decision":  "answered",
			"answers": map[string]provider.UserInputAnswer{
				"Scope": provider.SingleUserInputAnswer("turn"),
			},
		}),
		Timestamp: time.Now(),
	})

	item, found, err := st.GetThreadItem("t1", "ask-1")
	if err != nil || !found {
		t.Fatalf("item lookup: found=%v err=%v", found, err)
	}
	if item.Status != statusCompleted {
		t.Fatalf("status = %q, want completed (resolve must not reopen the row)", item.Status)
	}
	if got := answersFromMeta(t, item.Meta)["Scope"]; len(got) != 1 || got[0] != "turn" {
		t.Fatalf("answers = %+v, want Scope=[turn] merged onto completed row", got)
	}
	if !metaHasKey(t, item.Meta, "tool_result") {
		t.Fatal("answer merge dropped the existing tool_result")
	}
}

// TestClaudeAskUserQuestionDeclineWritesNoAnswers locks the empty-answers early
// return: a declined/cancelled AskUserQuestion (no answers submitted) must not
// write an `answers` key onto the launch row or flip its status. Claude's own
// tool_result settles the row; without the `len(answers) == 0` guard a decline
// could stamp an empty answers map and render the row as answered-with-nothing.
func TestClaudeAskUserQuestionDeclineWritesNoAnswers(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	questions := []provider.UserInputQuestion{{
		Header:   "Scope",
		Question: "Pick a scope",
		Options:  []provider.UserInputQuestionOption{{Label: "turn"}, {Label: "session"}},
	}}
	mustHandle(t, router, provider.ProviderEvent{
		Kind:     provider.EventToolStart,
		ThreadID: "t1",
		TurnID:   "turn-1",
		ItemID:   "ask-1",
		ItemType: "AskUserQuestion",
		Meta: mustMarshalJSON(t, map[string]any{
			"toolName": "AskUserQuestion",
			"input":    map[string]any{"questions": questions},
		}),
		Timestamp: time.Now(),
	})
	mustHandle(t, router, provider.ProviderEvent{
		Kind:     provider.EventUserInputRequest,
		ThreadID: "t1",
		TurnID:   "turn-1",
		ItemID:   "req-1",
		Meta: mustMarshalJSON(t, provider.UserInputRequest{
			RequestID: "req-1",
			ThreadID:  "t1",
			TurnID:    "turn-1",
			ToolUseID: "ask-1",
			ToolName:  "AskUserQuestion",
			Title:     "User Input Required",
			Questions: questions,
		}),
		Timestamp: time.Now(),
	})

	// User declines: decision is not "answered" and no answers are submitted.
	mustHandle(t, router, provider.ProviderEvent{
		Kind:     provider.EventUserInputResolved,
		ThreadID: "t1",
		ItemID:   "req-1",
		Meta: mustMarshalJSON(t, map[string]any{
			"requestId": "req-1",
			"decision":  "declined",
			"answers":   map[string]provider.UserInputAnswer{},
		}),
		Timestamp: time.Now(),
	})

	item, found, err := st.GetThreadItem("t1", "ask-1")
	if err != nil || !found {
		t.Fatalf("item lookup: found=%v err=%v", found, err)
	}
	if item.Status != statusRunning {
		t.Fatalf("status after decline = %q, want running (CLI tool_result settles it)", item.Status)
	}
	if metaHasKey(t, item.Meta, "answers") {
		t.Fatalf("decline wrote an answers key onto the row: %s", item.Meta)
	}
}

// TestClaudeAskUserQuestionResolveWithoutLaunchIsNoOp covers the defensive
// !found branch: a resolve whose tool_use id has no persisted launch row must
// no-op cleanly (no error, no fabricated row), not panic or invent state.
func TestClaudeAskUserQuestionResolveWithoutLaunchIsNoOp(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Register a request whose ToolUseID was never persisted as a launch row.
	mustHandle(t, router, provider.ProviderEvent{
		Kind:     provider.EventUserInputRequest,
		ThreadID: "t1",
		TurnID:   "turn-1",
		ItemID:   "req-1",
		Meta: mustMarshalJSON(t, provider.UserInputRequest{
			RequestID: "req-1",
			ThreadID:  "t1",
			TurnID:    "turn-1",
			ToolUseID: "ghost-1",
			ToolName:  "AskUserQuestion",
			Title:     "User Input Required",
			Questions: []provider.UserInputQuestion{{Header: "Scope", Question: "Pick?"}},
		}),
		Timestamp: time.Now(),
	})

	mustHandle(t, router, provider.ProviderEvent{
		Kind:     provider.EventUserInputResolved,
		ThreadID: "t1",
		ItemID:   "req-1",
		Meta: mustMarshalJSON(t, map[string]any{
			"requestId": "req-1",
			"decision":  "answered",
			"answers": map[string]provider.UserInputAnswer{
				"Scope": provider.SingleUserInputAnswer("turn"),
			},
		}),
		Timestamp: time.Now(),
	})

	if _, found, err := st.GetThreadItem("t1", "ghost-1"); err != nil || found {
		t.Fatalf("ghost launch row must not exist: found=%v err=%v", found, err)
	}
}

// TestClaudeAskUserQuestionDuplicateHeadersResolveByNormalizedID covers two
// questions sharing a header in one AskUserQuestion call. The launch row is
// created from the raw tool_use input (no per-question id), so the card would
// resolve both questions to the first answer by header. The resolve merge
// refreshes meta.input.questions with the normalized list (deduped ids
// Scope/Scope-2), which the card prefers over the header, so each question
// resolves to its own answer. Without the input refresh both questions persist
// with empty ids and the second renders the first's answer.
func TestClaudeAskUserQuestionDuplicateHeadersResolveByNormalizedID(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")

	// Two questions, identical header. The raw launch input has no ids.
	rawQuestions := []provider.UserInputQuestion{
		{Header: "Scope", Question: "Scope for fix A?", Options: []provider.UserInputQuestionOption{{Label: "turn"}, {Label: "session"}}},
		{Header: "Scope", Question: "Scope for fix B?", Options: []provider.UserInputQuestionOption{{Label: "turn"}, {Label: "session"}}},
	}
	mustHandle(t, router, provider.ProviderEvent{
		Kind:     provider.EventToolStart,
		ThreadID: "t1",
		TurnID:   "turn-1",
		ItemID:   "ask-1",
		ItemType: "AskUserQuestion",
		Meta: mustMarshalJSON(t, map[string]any{
			"toolName": "AskUserQuestion",
			"input":    map[string]any{"questions": rawQuestions},
		}),
		Timestamp: time.Now(),
	})

	// The request carries the normalized questions exactly as parse_control.go
	// marshals them: deduped ids Scope and Scope-2.
	normalized := provider.NormalizeUserInputQuestions(rawQuestions)
	mustHandle(t, router, provider.ProviderEvent{
		Kind:     provider.EventUserInputRequest,
		ThreadID: "t1",
		TurnID:   "turn-1",
		ItemID:   "req-1",
		Meta: mustMarshalJSON(t, provider.UserInputRequest{
			RequestID: "req-1",
			ThreadID:  "t1",
			TurnID:    "turn-1",
			ToolUseID: "ask-1",
			ToolName:  "AskUserQuestion",
			Title:     "User Input Required",
			Questions: normalized,
		}),
		Timestamp: time.Now(),
	})

	// Distinct answers keyed by the deduped ids.
	mustHandle(t, router, provider.ProviderEvent{
		Kind:     provider.EventUserInputResolved,
		ThreadID: "t1",
		ItemID:   "req-1",
		Meta: mustMarshalJSON(t, map[string]any{
			"requestId": "req-1",
			"decision":  "answered",
			"answers": map[string]provider.UserInputAnswer{
				"Scope":   provider.SingleUserInputAnswer("turn"),
				"Scope-2": provider.SingleUserInputAnswer("session"),
			},
		}),
		Timestamp: time.Now(),
	})

	launch, found, err := st.GetThreadItem("t1", "ask-1")
	if err != nil || !found {
		t.Fatalf("launch lookup: found=%v err=%v", found, err)
	}
	// The persisted questions must carry the deduped ids so the card can
	// disambiguate; without the refresh both ids are empty and render under Scope.
	if ids := questionIDsFromMeta(t, launch.Meta); len(ids) != 2 || ids[0] != "Scope" || ids[1] != "Scope-2" {
		t.Fatalf("persisted question ids = %v, want [Scope Scope-2]", ids)
	}
	answers := answersFromMeta(t, launch.Meta)
	if got := answers["Scope"]; len(got) != 1 || got[0] != "turn" {
		t.Fatalf("Scope answer = %+v, want [turn]", got)
	}
	if got := answers["Scope-2"]; len(got) != 1 || got[0] != "session" {
		t.Fatalf("Scope-2 answer = %+v, want [session]", got)
	}
}

func mustHandle(t *testing.T, router *Router, evt provider.ProviderEvent) {
	t.Helper()
	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle %s: %v", evt.Kind, err)
	}
}

func answersFromMeta(t *testing.T, meta string) map[string]provider.UserInputAnswer {
	t.Helper()
	var decoded struct {
		Answers map[string]provider.UserInputAnswer `json:"answers"`
	}
	if err := json.Unmarshal([]byte(meta), &decoded); err != nil {
		t.Fatalf("unmarshal meta answers from %q: %v", meta, err)
	}
	return decoded.Answers
}

func questionIDsFromMeta(t *testing.T, meta string) []string {
	t.Helper()
	var decoded struct {
		Input struct {
			Questions []struct {
				ID string `json:"id"`
			} `json:"questions"`
		} `json:"input"`
	}
	if err := json.Unmarshal([]byte(meta), &decoded); err != nil {
		t.Fatalf("unmarshal meta questions from %q: %v", meta, err)
	}
	ids := make([]string, len(decoded.Input.Questions))
	for i, q := range decoded.Input.Questions {
		ids[i] = q.ID
	}
	return ids
}

func metaHasKey(t *testing.T, meta, key string) bool {
	t.Helper()
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal([]byte(meta), &decoded); err != nil {
		t.Fatalf("unmarshal meta from %q: %v", meta, err)
	}
	_, ok := decoded[key]
	return ok
}
