package discussion

import (
	"strings"
	"testing"

	"agent-overflow/internal/store"
)

func TestBuildTurnPromptLabelsHumanAgentAndSystemMessages(t *testing.T) {
	messages := []store.ChannelMessage{
		{FromType: "human", Content: "Let's start with the tradeoffs."},
		{FromType: "agent", FromRole: "Reviewer", Content: "I'd weigh performance first."},
		{FromType: "system", FromRole: "moderator", Content: "Discussion concluded: reached the 8-turn limit."},
	}
	got := BuildTurnPrompt("architect", messages)

	if !strings.Contains(got, "Human:\nLet's start with the tradeoffs.") {
		t.Fatalf("missing Human-labeled message, got:\n%s", got)
	}
	if !strings.Contains(got, "Reviewer:\nI'd weigh performance first.") {
		t.Fatalf("missing role-labeled agent message, got:\n%s", got)
	}
	if !strings.Contains(got, "Moderator:\nDiscussion concluded: reached the 8-turn limit.") {
		t.Fatalf("missing Moderator-labeled system message, got:\n%s", got)
	}
}

func TestBuildTurnPromptFallsBackToParticipantWhenRoleBlank(t *testing.T) {
	messages := []store.ChannelMessage{
		{FromType: "agent", FromRole: "", Content: "No role on this one."},
	}
	got := BuildTurnPrompt("architect", messages)
	if !strings.Contains(got, "Participant:\nNo role on this one.") {
		t.Fatalf("expected fallback label Participant, got:\n%s", got)
	}
}

func TestBuildTurnPromptPreservesContentVerbatim(t *testing.T) {
	content := "Line one.\n\nLine two with *markdown* and a \"quote\"."
	messages := []store.ChannelMessage{
		{FromType: "human", Content: content},
	}
	got := BuildTurnPrompt("architect", messages)
	if !strings.Contains(got, content) {
		t.Fatalf("expected verbatim content preserved, got:\n%s", got)
	}
}

func TestBuildTurnPromptOmitsTranscriptHeaderWhenNoMessages(t *testing.T) {
	got := BuildTurnPrompt("architect", nil)
	if strings.Contains(got, "New messages in the discussion") {
		t.Fatalf("expected no transcript header for empty messages, got:\n%s", got)
	}
	if !strings.HasPrefix(got, "It's your turn to contribute") {
		t.Fatalf("expected the prompt to open directly with the turn instruction, got:\n%s", got)
	}
}

func TestBuildTurnPromptInstructionMentionsFormattedRole(t *testing.T) {
	got := BuildTurnPrompt("code-reviewer", nil)
	if !strings.Contains(got, "Stay in your role as Code Reviewer") {
		t.Fatalf("expected formatted role in the turn instruction, got:\n%s", got)
	}
}

func TestBuildTurnPromptMultipleMessagesAreSeparatedAndOrdered(t *testing.T) {
	messages := []store.ChannelMessage{
		{FromType: "human", Content: "first"},
		{FromType: "agent", FromRole: "Reviewer", Content: "second"},
	}
	got := BuildTurnPrompt("architect", messages)
	firstIdx := strings.Index(got, "first")
	secondIdx := strings.Index(got, "second")
	if firstIdx < 0 || secondIdx < 0 || firstIdx > secondIdx {
		t.Fatalf("expected messages to appear in order, got:\n%s", got)
	}
	if !strings.Contains(got, "first\n\nReviewer:\nsecond") {
		t.Fatalf("expected a blank-line separator between messages, got:\n%s", got)
	}
}
