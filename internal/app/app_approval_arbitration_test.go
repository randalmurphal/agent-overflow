package app

import (
	"context"
	"errors"
	"testing"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/transport"
)

// One backend serves several screens, and each of them renders the same
// approval prompt. The second person to press a button must be told the
// question was already answered — not handed a failure, and above all not
// have their answer forwarded as a second response to a request the provider
// has already resolved.
func TestASecondAnswerToOnePromptIsReportedAsAlreadyHandled(t *testing.T) {
	app := newTestAppWithStore(t)
	app.ensureTriageRouter()

	var failures []provider.ApprovalEvent
	// testEmitHook, not emitEventFn: a.emit forwards to THIS hook, and the
	// other one observes a different funnel — so the "no failure event"
	// assertion below was passing against a recorder nothing wrote to.
	app.testEmitHook = func(name string, data any) {
		if name != eventchan.ProviderApproval.String() {
			return
		}
		evt, ok := data.(provider.ApprovalEvent)
		if ok && evt.Action == "fail" {
			failures = append(failures, evt)
		}
	}

	// The first answer wins the arbitration and then fails for an unrelated
	// reason (no session in this fixture), which is what a real first answer
	// does to the router: it takes the record.
	if !app.triage.ClaimApprovalResponse("thread-1", "req-1") {
		t.Fatal("the first answer was refused")
	}
	failures = nil

	err := app.RespondToApproval(context.Background(), "thread-1", provider.ApprovalResponse{
		RequestID: "req-1",
		Decision:  "accept",
	})
	if !errors.Is(err, transport.ErrAlreadyHandled) {
		t.Fatalf("second answer error = %v, want transport.ErrAlreadyHandled", err)
	}
	// No failure event: nothing failed. Emitting one would put an error
	// banner on the screen of someone whose only mistake was answering a
	// prompt that was still on it.
	if len(failures) != 0 {
		t.Fatalf("second answer emitted %d approval failures, want 0: %+v", len(failures), failures)
	}
}

// Same arbitration for structured-input forms, which two screens can also
// both submit.
func TestASecondUserInputSubmissionIsReportedAsAlreadyHandled(t *testing.T) {
	app := newTestAppWithStore(t)
	app.ensureTriageRouter()

	if !app.triage.ClaimUserInputResponse("thread-1", "req-1") {
		t.Fatal("the first submission was refused")
	}

	err := app.RespondToUserInput(context.Background(), "thread-1", provider.UserInputResponse{
		RequestID: "req-1",
	})
	if !errors.Is(err, transport.ErrAlreadyHandled) {
		t.Fatalf("second submission error = %v, want transport.ErrAlreadyHandled", err)
	}
}

// A first answer that never reached the provider leaves the prompt open, so
// the same client must be able to press the button again. Without the release
// the arbitration would wedge every prompt whose first answer failed.
func TestAFailedAnswerCanBeRetried(t *testing.T) {
	app := newTestAppWithStore(t)
	app.ensureTriageRouter()

	first := app.RespondToApproval(context.Background(), "thread-1", provider.ApprovalResponse{
		RequestID: "req-1",
		Decision:  "accept",
	})
	if first == nil {
		t.Fatal("expected the no-session failure this fixture produces")
	}
	if errors.Is(first, transport.ErrAlreadyHandled) {
		t.Fatalf("the FIRST answer was refused as already handled: %v", first)
	}

	// The retry must reach the same failure, not a spurious already-handled.
	second := app.RespondToApproval(context.Background(), "thread-1", provider.ApprovalResponse{
		RequestID: "req-1",
		Decision:  "accept",
	})
	if errors.Is(second, transport.ErrAlreadyHandled) {
		t.Fatalf("a retry after a failed write was refused as already handled: %v", second)
	}
}
