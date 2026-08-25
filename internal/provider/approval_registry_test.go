package provider

import (
	"encoding/json"
	"testing"
)

func TestApprovalRegistryClaimIsOnceAndKindChecked(t *testing.T) {
	var r ApprovalRegistry
	r.Track("req-1", EventApprovalResolved, nil)

	if r.Claim("req-1", EventUserInputResolved) {
		t.Fatal("a user-input answer claimed a tool approval")
	}
	if !r.Claim("req-1", EventApprovalResolved) {
		t.Fatal("first claim refused")
	}
	if r.Claim("req-1", EventApprovalResolved) {
		t.Fatal("second claim succeeded — a duplicate response would reach the provider")
	}
	if r.Claim("never-tracked", EventApprovalResolved) {
		t.Fatal("claimed a request that was never tracked")
	}
}

func TestApprovalRegistryTrackForgetsAPriorResolution(t *testing.T) {
	var r ApprovalRegistry
	r.Track("req-1", EventApprovalResolved, nil)
	if !r.Claim("req-1", EventApprovalResolved) {
		t.Fatal("first claim refused")
	}

	// The provider re-sent the request: it is asking again, not repeating
	// itself, so the id has to be claimable a second time.
	r.Track("req-1", EventApprovalResolved, nil)
	if !r.Claim("req-1", EventApprovalResolved) {
		t.Fatal("a re-sent request stayed deduped and could never be answered")
	}
}

func TestApprovalRegistryQuestionsAreOwnedCopies(t *testing.T) {
	var r ApprovalRegistry
	questions := []UserInputQuestion{{ID: "q1", Header: "Pick one"}}
	r.Track("req-1", EventUserInputResolved, questions)
	questions[0].Header = "mutated after tracking"

	got := r.Questions("req-1")
	if len(got) != 1 || got[0].Header != "Pick one" {
		t.Fatalf("registry aliased the caller's slice: %+v", got)
	}
	got[0].Header = "mutated after reading"
	if again := r.Questions("req-1"); again[0].Header != "Pick one" {
		t.Fatalf("Questions returned an aliased slice: %+v", again)
	}
	if r.Questions("never-tracked") != nil {
		t.Fatal("questions returned for an unknown request")
	}
}

func TestApprovalRegistryCancelIsIdempotentAndKindBlind(t *testing.T) {
	var r ApprovalRegistry
	r.Track("req-1", EventUserInputResolved, nil)

	released, ok := r.Cancel("req-1")
	if !ok {
		t.Fatal("cancel refused a tracked request")
	}
	if released.RequestID != "req-1" || released.ResolveKind != EventUserInputResolved {
		t.Fatalf("cancel returned %+v", released)
	}
	if _, ok := r.Cancel("req-1"); ok {
		t.Fatal("cancel resolved the same request twice")
	}
	if r.Claim("req-1", EventUserInputResolved) {
		t.Fatal("a cancelled request stayed claimable")
	}
}

func TestApprovalRegistryDrainClosesAndRefusesLateTracks(t *testing.T) {
	var r ApprovalRegistry
	r.Track("req-1", EventApprovalResolved, nil)
	r.Track("req-2", EventUserInputResolved, nil)

	released := r.Drain(true)
	if len(released) != 2 {
		t.Fatalf("drain released %d requests, want 2", len(released))
	}
	if again := r.Drain(true); len(again) != 0 {
		t.Fatalf("second drain released %d requests — double resolution", len(again))
	}

	// A prompt registered after teardown could never be answered or drained.
	r.Track("req-3", EventApprovalResolved, nil)
	if r.Claim("req-3", EventApprovalResolved) {
		t.Fatal("a closed registry accepted a new pending request")
	}
}

func TestApprovalRegistryMidLifeDrainKeepsAccepting(t *testing.T) {
	var r ApprovalRegistry
	r.Track("req-1", EventApprovalResolved, nil)

	if released := r.Drain(false); len(released) != 1 {
		t.Fatalf("drain released %d requests, want 1", len(released))
	}
	r.Track("req-2", EventApprovalResolved, nil)
	if !r.Claim("req-2", EventApprovalResolved) {
		t.Fatal("an interrupt drain latched the registry shut")
	}
}

func TestResolvedApprovalMetaCarriesAnswersOnlyForUserInput(t *testing.T) {
	approval := ResolvedApproval{RequestID: "req-1", ResolveKind: EventApprovalResolved}
	var fields map[string]any
	if err := json.Unmarshal(approval.Meta("cancel"), &fields); err != nil {
		t.Fatalf("approval meta: %v", err)
	}
	if fields["requestId"] != "req-1" || fields["decision"] != "cancel" {
		t.Fatalf("approval meta: %+v", fields)
	}
	if _, ok := fields["answers"]; ok {
		t.Fatalf("approval meta carried an answers map: %+v", fields)
	}

	userInput := ResolvedApproval{RequestID: "req-2", ResolveKind: EventUserInputResolved}
	fields = nil
	if err := json.Unmarshal(userInput.Meta("lost"), &fields); err != nil {
		t.Fatalf("user-input meta: %v", err)
	}
	answers, ok := fields["answers"].(map[string]any)
	if !ok || len(answers) != 0 {
		t.Fatalf("user-input meta must carry an empty answers map: %+v", fields)
	}
}
