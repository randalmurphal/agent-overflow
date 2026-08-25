package codex

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"agent-overflow/internal/provider"
)

func TestRespondToApprovalAccept(t *testing.T) {
	s, _ := newTestCodexSession(t)
	s.trackPendingApproval(42, provider.EventApprovalResolved)

	// Call the actual RespondToApproval method with an accept decision.
	// The cat-backed session writes the JSON-RPC response to stdin successfully.
	err := s.RespondToApproval(context.Background(), provider.ApprovalResponse{
		RequestID: "42",
		Decision:  "accept",
	})
	if err != nil {
		t.Fatalf("RespondToApproval(accept): %v", err)
	}
}

func TestRespondToApprovalDecline(t *testing.T) {
	s, _ := newTestCodexSession(t)
	s.trackPendingApproval(42, provider.EventApprovalResolved)

	// Call the actual RespondToApproval method with a decline decision.
	err := s.RespondToApproval(context.Background(), provider.ApprovalResponse{
		RequestID: "42",
		Decision:  "decline",
	})
	if err != nil {
		t.Fatalf("RespondToApproval(decline): %v", err)
	}
}

func TestBuildApprovalResponseResultDecision(t *testing.T) {
	// Codex-native decision values are passed through directly -- no translation.
	tests := []struct {
		name     string
		decision string
		want     string
	}{
		{name: "accept", decision: "accept", want: "accept"},
		{name: "decline", decision: "decline", want: "decline"},
		{name: "acceptForSession", decision: "acceptForSession", want: "acceptForSession"},
		{name: "cancel", decision: "cancel", want: "cancel"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rpcID, result, err := buildApprovalResponseResult(provider.ApprovalResponse{
				RequestID: "42",
				Decision:  tt.decision,
			})
			if err != nil {
				t.Fatalf("buildApprovalResponseResult(%s): %v", tt.decision, err)
			}
			if rpcID != 42 {
				t.Fatalf("rpcID = %d, want 42", rpcID)
			}

			payload, ok := result.(map[string]any)
			if !ok {
				t.Fatalf("result type = %T, want map[string]any", result)
			}
			if got := payload["decision"]; got != tt.want {
				t.Fatalf("decision = %v, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildUserInputResponseResultAnswers(t *testing.T) {
	rpcID, result, err := buildUserInputResponseResult(provider.UserInputResponse{
		RequestID: "7",
		Answers: map[string]provider.UserInputAnswer{
			"framework": provider.SingleUserInputAnswer("React"),
			"scope":     provider.UserInputAnswer{"turn", "session"},
		},
	})
	if err != nil {
		t.Fatalf("buildApprovalResponseResult(): %v", err)
	}
	if rpcID != 7 {
		t.Fatalf("rpcID = %d, want 7", rpcID)
	}

	payload, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result type = %T, want map[string]any", result)
	}
	answers, ok := payload["answers"].(map[string]codexUserInputAnswer)
	if !ok {
		t.Fatalf("answers type = %T, want map[string]codexUserInputAnswer", payload["answers"])
	}
	if got := answers["framework"].Answers; len(got) != 1 || got[0] != "React" {
		t.Fatalf("framework answers = %v, want [React]", got)
	}
	if got := answers["scope"].Answers; len(got) != 2 || got[0] != "turn" || got[1] != "session" {
		t.Fatalf("scope answers = %v, want [turn session]", got)
	}
}

func TestBuildUserInputResponseResultRejectsUnknownDecision(t *testing.T) {
	_, _, err := buildUserInputResponseResult(provider.UserInputResponse{
		RequestID: "7",
		Decision:  "bogus",
	})
	if !errors.Is(err, provider.ErrInvalidUserInputDecision) {
		t.Fatalf("error = %v, want ErrInvalidUserInputDecision", err)
	}
}

func TestBuildUserInputResponseResultAcceptsDeclineDecisions(t *testing.T) {
	for _, decision := range []string{"decline", "cancel", "deny"} {
		t.Run(decision, func(t *testing.T) {
			_, result, err := buildUserInputResponseResult(provider.UserInputResponse{
				RequestID: "7",
				Decision:  decision,
			})
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", decision, err)
			}
			if result == nil {
				t.Fatal("result is nil")
			}
		})
	}
}

func TestBuildApprovalResponseResultPermission(t *testing.T) {
	enabled := true
	rpcID, result, err := buildApprovalResponseResult(provider.ApprovalResponse{
		RequestID: "9",
		Scope:     "session",
		Permissions: &provider.PermissionProfile{
			Network: &provider.NetworkPermissions{Enabled: &enabled},
		},
	})
	if err != nil {
		t.Fatalf("buildApprovalResponseResult(): %v", err)
	}
	if rpcID != 9 {
		t.Fatalf("rpcID = %d, want 9", rpcID)
	}

	payload, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result type = %T, want map[string]any", result)
	}
	if got := payload["scope"]; got != "session" {
		t.Fatalf("scope = %v, want session", got)
	}
	if payload["permissions"] == nil {
		t.Fatal("permissions should be present")
	}
}

func TestBuildApprovalResponseResultInvalidRequestID(t *testing.T) {
	_, _, err := buildApprovalResponseResult(provider.ApprovalResponse{RequestID: "not-a-number"})
	if err == nil {
		t.Fatal("expected invalid request ID error")
	}
}

func TestCodexRespondToApprovalMethod(t *testing.T) {
	s, _ := newTestCodexSession(t)
	s.trackPendingApproval(42, provider.EventApprovalResolved)

	err := s.RespondToApproval(context.Background(), provider.ApprovalResponse{
		RequestID: "42",
		Decision:  "allow",
	})
	if err != nil {
		t.Fatalf("RespondToApproval(): %v", err)
	}
}

// -- buildApprovalResponseResult: elicitation branch --

func TestBuildApprovalResponseResultElicitation(t *testing.T) {
	rpcID, result, err := buildApprovalResponseResult(provider.ApprovalResponse{
		RequestID: "15",
		Elicitation: &provider.ElicitationResolution{
			Action:  "confirm",
			Content: json.RawMessage(`{"key":"value"}`),
			Meta:    json.RawMessage(`{"source":"test"}`),
		},
	})
	if err != nil {
		t.Fatalf("buildApprovalResponseResult(): %v", err)
	}
	if rpcID != 15 {
		t.Fatalf("rpcID = %d, want 15", rpcID)
	}
	payload, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result type = %T", result)
	}
	if payload["action"] != "confirm" {
		t.Errorf("action: got %v, want confirm", payload["action"])
	}
	if payload["content"] == nil {
		t.Error("expected non-nil content")
	}
	if payload["_meta"] == nil {
		t.Error("expected non-nil _meta")
	}
}
