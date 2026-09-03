package app

import (
	"testing"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/transport"
)

// A `fail` frame on either interactive channel is broadcast to every client,
// because the channel is not thread-filtered and the RESOLUTION half of it
// (the prompt closing) is everybody's business. The FAILURE half is not: the
// prompt is still open on every other screen, and a sticky "Failed to respond
// to approval" banner there is both wrong and unclearable by the person
// reading it. The stamp is what lets a client tell the two apart.
//
// The CONNECTION and not the device: two tabs of one browser answer
// independently, so a device stamp would put the losing tab's error on the
// other one.
func TestAFailedApprovalNamesTheConnectionThatAnswered(t *testing.T) {
	app := newTestAppWithStore(t)
	app.ensureTriageRouter()

	var failures []provider.ApprovalEvent
	app.testEmitHook = func(name string, data any) {
		if name != eventchan.ProviderApproval.String() {
			return
		}
		if evt, ok := data.(provider.ApprovalEvent); ok && evt.Action == "fail" {
			failures = append(failures, evt)
		}
	}

	ctx := ctxFromClient(transport.ClientIdentity{DeviceID: "laptop-1", ConnectionID: "conn-9"})
	// No session in this fixture, so the write fails — which is exactly the
	// event under test.
	if err := app.RespondToApproval(ctx, "thread-1", provider.ApprovalResponse{
		RequestID: "req-1",
		Decision:  "accept",
	}); err == nil {
		t.Fatal("expected the no-session failure this fixture produces")
	}

	if len(failures) != 1 {
		t.Fatalf("emitted %d approval failures, want 1: %+v", len(failures), failures)
	}
	if failures[0].ConnectionID != "conn-9" {
		t.Fatalf("connectionId = %q, want conn-9", failures[0].ConnectionID)
	}
}

func TestAFailedUserInputNamesTheConnectionThatSubmitted(t *testing.T) {
	app := newTestAppWithStore(t)
	app.ensureTriageRouter()

	var failures []provider.UserInputEvent
	app.testEmitHook = func(name string, data any) {
		if name != eventchan.ProviderUserInput.String() {
			return
		}
		if evt, ok := data.(provider.UserInputEvent); ok && evt.Action == "fail" {
			failures = append(failures, evt)
		}
	}

	ctx := ctxFromClient(transport.ClientIdentity{DeviceID: "laptop-1", ConnectionID: "conn-9"})
	if err := app.RespondToUserInput(ctx, "thread-1", provider.UserInputResponse{
		RequestID: "req-1",
	}); err == nil {
		t.Fatal("expected the no-session failure this fixture produces")
	}

	if len(failures) != 1 {
		t.Fatalf("emitted %d user-input failures, want 1: %+v", len(failures), failures)
	}
	if failures[0].ConnectionID != "conn-9" {
		t.Fatalf("connectionId = %q, want conn-9", failures[0].ConnectionID)
	}
}

// An in-process caller — a saga, a workflow phase, a test — has no connection,
// and the frame it produces carries no stamp. A client must show that one:
// unstamped has to keep meaning what it meant before the stamp existed, or a
// bundle running against an older backend would swallow the only surfacing
// this failure has.
func TestAFailureWithNoConnectionCarriesNoStamp(t *testing.T) {
	app := newTestAppWithStore(t)
	app.ensureTriageRouter()

	var failures []provider.ApprovalEvent
	app.testEmitHook = func(name string, data any) {
		if name != eventchan.ProviderApproval.String() {
			return
		}
		if evt, ok := data.(provider.ApprovalEvent); ok && evt.Action == "fail" {
			failures = append(failures, evt)
		}
	}

	if err := app.RespondToApproval(t.Context(), "thread-1", provider.ApprovalResponse{
		RequestID: "req-1",
		Decision:  "accept",
	}); err == nil {
		t.Fatal("expected the no-session failure this fixture produces")
	}

	if len(failures) != 1 {
		t.Fatalf("emitted %d approval failures, want 1: %+v", len(failures), failures)
	}
	if failures[0].ConnectionID != "" {
		t.Fatalf("connectionId = %q, want empty", failures[0].ConnectionID)
	}
}
