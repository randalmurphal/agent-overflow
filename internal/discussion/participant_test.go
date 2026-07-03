package discussion

import (
	"strings"
	"testing"

	"agent-overflow/internal/store"
)

func TestBuildParticipantPlansFallsBackToParentProviderAndModel(t *testing.T) {
	parent := store.Thread{
		ID:            "parent",
		Provider:      "claude",
		Model:         "sonnet-4-6",
		WorkspacePath: "/work",
		Title:         "Design",
	}
	def := store.DiscussionDefinition{
		Participants: []store.DiscussionParticipant{
			{Role: "advocate"},
			{Role: "skeptic", Provider: "codex", Model: "o4-mini"},
		},
	}
	plans, err := BuildParticipantPlans(parent, def, 100)
	if err != nil {
		t.Fatalf("BuildParticipantPlans: %v", err)
	}
	if len(plans) != 2 {
		t.Fatalf("got %d plans, want 2", len(plans))
	}
	if plans[0].Thread.Provider != "claude" || plans[0].Thread.Model != "sonnet-4-6" {
		t.Fatalf("advocate fallbacks not applied: %+v", plans[0].Thread)
	}
	if plans[1].Thread.Provider != "codex" || plans[1].Thread.Model != "o4-mini" {
		t.Fatalf("skeptic overrides not preserved: %+v", plans[1].Thread)
	}
	if plans[0].Thread.ParentThreadID != "parent" {
		t.Fatalf("ParentThreadID = %q, want parent", plans[0].Thread.ParentThreadID)
	}
	if plans[0].Thread.Mode != "discussion" {
		t.Fatalf("Mode = %q, want discussion", plans[0].Thread.Mode)
	}
	if plans[0].Thread.CreatedAt != 100 || plans[0].Thread.UpdatedAt != 100 {
		t.Fatalf("timestamps not threaded through: %+v", plans[0].Thread)
	}
	if !strings.Contains(plans[0].Thread.Title, "Advocate") {
		t.Fatalf("title not role-formatted: %q", plans[0].Thread.Title)
	}
}

func TestBuildParticipantPlansRequiresProviderAndModel(t *testing.T) {
	parent := store.Thread{} // empty parent — fallbacks have nothing to fill in
	def := store.DiscussionDefinition{
		Participants: []store.DiscussionParticipant{{Role: "advocate", Model: "claude-sonnet"}},
	}
	if _, err := BuildParticipantPlans(parent, def, 0); err == nil {
		t.Fatal("expected provider-missing error, got nil")
	}

	def = store.DiscussionDefinition{
		Participants: []store.DiscussionParticipant{{Role: "advocate", Provider: "claude"}},
	}
	if _, err := BuildParticipantPlans(parent, def, 0); err == nil {
		t.Fatal("expected model-missing error, got nil")
	}
}

func TestBuildParticipantPlansUniqueThreadIDs(t *testing.T) {
	parent := store.Thread{Provider: "claude", Model: "sonnet"}
	def := store.DiscussionDefinition{
		Participants: []store.DiscussionParticipant{
			{Role: "advocate"},
			{Role: "skeptic"},
			{Role: "moderator"},
		},
	}
	plans, err := BuildParticipantPlans(parent, def, 0)
	if err != nil {
		t.Fatalf("BuildParticipantPlans: %v", err)
	}
	seen := make(map[string]struct{}, len(plans))
	for _, p := range plans {
		if _, dup := seen[p.Thread.ID]; dup {
			t.Fatalf("duplicate child thread id %q", p.Thread.ID)
		}
		seen[p.Thread.ID] = struct{}{}
	}
}

func TestBuildParticipantPromptIncludesContextAndBody(t *testing.T) {
	got := BuildParticipantPrompt("skeptic", "Disagree with everything.")
	if !strings.Contains(got, "Skeptic participant") {
		t.Fatalf("prompt missing role preamble: %q", got)
	}
	if !strings.Contains(got, discussionProtocolPreamble) {
		t.Fatalf("prompt missing discussion protocol paragraph: %q", got)
	}
	if !strings.Contains(got, "Disagree with everything.") {
		t.Fatalf("prompt missing body: %q", got)
	}
	if !strings.Contains(got, "\n\n") {
		t.Fatalf("prompt sections not blank-separated: %q", got)
	}
}

// TestBuildParticipantPromptOmitsBlankBody guards the joinPrompts
// contract now that the discussion-protocol paragraph is a permanent
// second segment: a blank rawSystem must not add its OWN separator on
// top of the one already sitting between the role preamble and the
// protocol paragraph (no dangling trailing "\n\n", no doubled-up
// separator).
func TestBuildParticipantPromptOmitsBlankBody(t *testing.T) {
	got := BuildParticipantPrompt("advocate", "   ")
	if !strings.Contains(got, "Advocate participant") {
		t.Fatalf("prompt missing role preamble: %q", got)
	}
	if !strings.Contains(got, discussionProtocolPreamble) {
		t.Fatalf("prompt missing discussion protocol paragraph: %q", got)
	}
	if strings.HasSuffix(got, "\n\n") {
		t.Fatalf("blank body should not leave a dangling trailing separator: %q", got)
	}
	if count := strings.Count(got, "\n\n"); count != 1 {
		t.Fatalf("expected exactly one blank-line separator with blank body, got %d: %q", count, got)
	}
}

func TestFormatRoleVariants(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"advocate", "Advocate"},
		{"red-team", "Red Team"},
		{"red_team", "Red Team"},
		{"red team", "Red Team"},
		{"  advocate  ", "Advocate"},
		{"", "Participant"},
		{"  ", "Participant"},
		{"DEVIL-ADVOCATE", "DEVIL ADVOCATE"},
	}
	for _, tc := range cases {
		if got := FormatRole(tc.in); got != tc.want {
			t.Errorf("FormatRole(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRoleFromThreadTitle(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// Standard BuildParticipantPlans output.
		{"Design - Advocate", "Advocate"},
		{"Design - Devil Advocate", "Devil Advocate"},
		// Multiple " - " segments: take the trailing component only.
		{"Project - Phase 1 - Advocate", "Advocate"},
		// Trailing whitespace on the role.
		{"Design -   Advocate   ", "Advocate"},
		// Whitespace-only role falls back to the full title.
		{"Design - ", "Design -"},
		// Legacy threads with no separator.
		{"Renamed Thread", "Renamed Thread"},
		// Empty / whitespace-only titles.
		{"", ""},
		{"   ", ""},
	}
	for _, tc := range cases {
		if got := RoleFromThreadTitle(tc.in); got != tc.want {
			t.Errorf("RoleFromThreadTitle(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
