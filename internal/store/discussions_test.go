package store

import (
	"testing"
	"time"
)

func TestDiscussionDefinitionCRUD(t *testing.T) {
	s := newTestStore(t)

	now := time.Now().UnixMilli()
	def := DiscussionDefinition{
		ID:          "disc-1",
		Name:        "Architects",
		Description: "Review architecture decisions",
		Scope:       "project",
		ProjectID:   "proj-1",
		Participants: []DiscussionParticipant{
			{Role: "proposer", Description: "Owns the change", System: "Push for the plan", Provider: "claude", Model: "claude-sonnet-4-6"},
			{Role: "reviewer", Description: "Challenges tradeoffs", System: "Find weak spots", Provider: "codex", Model: "gpt-5.4"},
		},
		Settings:  DiscussionSettings{MaxTurns: 12},
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := s.CreateDiscussionDef(def); err != nil {
		t.Fatalf("CreateDiscussionDef: %v", err)
	}

	got, err := s.GetDiscussionDef("Architects", "project", "proj-1")
	if err != nil {
		t.Fatalf("GetDiscussionDef: %v", err)
	}
	assertDiscussionDefinitionEqual(t, got, def)

	listed, err := s.ListDiscussionDefs("project", "proj-1")
	if err != nil {
		t.Fatalf("ListDiscussionDefs: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("listed len = %d, want 1", len(listed))
	}
	assertDiscussionDefinitionEqual(t, listed[0], def)

	updated := def
	updated.ID = "disc-2"
	updated.Name = "Architects Revised"
	updated.Description = "Updated discussion"
	updated.Settings.MaxTurns = 20
	updated.Participants[1].System = "Challenge assumptions harder"
	updated.UpdatedAt = now + 5000

	if err := s.UpdateDiscussionDef(def.Name, def.Scope, def.ProjectID, updated); err != nil {
		t.Fatalf("UpdateDiscussionDef: %v", err)
	}

	afterUpdate, err := s.GetDiscussionDef(updated.Name, updated.Scope, updated.ProjectID)
	if err != nil {
		t.Fatalf("GetDiscussionDef(updated): %v", err)
	}
	assertDiscussionDefinitionEqual(t, afterUpdate, updated)

	if err := s.DeleteDiscussionDef(updated.Name, updated.Scope, updated.ProjectID); err != nil {
		t.Fatalf("DeleteDiscussionDef: %v", err)
	}

	if _, err := s.GetDiscussionDef(updated.Name, updated.Scope, updated.ProjectID); err == nil {
		t.Fatal("expected deleted discussion definition lookup to fail")
	}
}

func TestListDiscussionDefsFiltersByScopeAndProject(t *testing.T) {
	s := newTestStore(t)

	now := time.Now().UnixMilli()
	defs := []DiscussionDefinition{
		{
			ID:          "global-1",
			Name:        "Global Review",
			Description: "Global defaults",
			Scope:       "global",
			Participants: []DiscussionParticipant{
				{Role: "a", System: "a"},
				{Role: "b", System: "b"},
			},
			Settings:  DiscussionSettings{MaxTurns: 10},
			CreatedAt: now,
			UpdatedAt: now,
		},
		{
			ID:          "project-1",
			Name:        "Project Review",
			Description: "Project only",
			Scope:       "project",
			ProjectID:   "proj-1",
			Participants: []DiscussionParticipant{
				{Role: "a", System: "a"},
				{Role: "b", System: "b"},
			},
			Settings:  DiscussionSettings{MaxTurns: 10},
			CreatedAt: now + 1,
			UpdatedAt: now + 1,
		},
		{
			ID:          "project-2",
			Name:        "Another Project",
			Description: "Different project",
			Scope:       "project",
			ProjectID:   "proj-2",
			Participants: []DiscussionParticipant{
				{Role: "a", System: "a"},
				{Role: "b", System: "b"},
			},
			Settings:  DiscussionSettings{MaxTurns: 10},
			CreatedAt: now + 2,
			UpdatedAt: now + 2,
		},
	}

	for _, def := range defs {
		if err := s.CreateDiscussionDef(def); err != nil {
			t.Fatalf("CreateDiscussionDef(%s): %v", def.Name, err)
		}
	}

	projectDefs, err := s.ListDiscussionDefs("project", "proj-1")
	if err != nil {
		t.Fatalf("ListDiscussionDefs(project): %v", err)
	}
	if len(projectDefs) != 1 {
		t.Fatalf("projectDefs len = %d, want 1", len(projectDefs))
	}
	if projectDefs[0].Name != "Project Review" {
		t.Fatalf("projectDefs[0].Name = %q, want %q", projectDefs[0].Name, "Project Review")
	}

	globalDefs, err := s.ListDiscussionDefs("global", "")
	if err != nil {
		t.Fatalf("ListDiscussionDefs(global): %v", err)
	}
	if len(globalDefs) != 1 {
		t.Fatalf("globalDefs len = %d, want 1", len(globalDefs))
	}
	if globalDefs[0].Name != "Global Review" {
		t.Fatalf("globalDefs[0].Name = %q, want %q", globalDefs[0].Name, "Global Review")
	}
}

func assertDiscussionDefinitionEqual(t *testing.T, got, want DiscussionDefinition) {
	t.Helper()

	if got.ID != want.ID {
		t.Fatalf("ID = %q, want %q", got.ID, want.ID)
	}
	if got.Name != want.Name {
		t.Fatalf("Name = %q, want %q", got.Name, want.Name)
	}
	if got.Description != want.Description {
		t.Fatalf("Description = %q, want %q", got.Description, want.Description)
	}
	if got.Scope != want.Scope {
		t.Fatalf("Scope = %q, want %q", got.Scope, want.Scope)
	}
	if got.ProjectID != want.ProjectID {
		t.Fatalf("ProjectID = %q, want %q", got.ProjectID, want.ProjectID)
	}
	if got.Settings.MaxTurns != want.Settings.MaxTurns {
		t.Fatalf("Settings.MaxTurns = %d, want %d", got.Settings.MaxTurns, want.Settings.MaxTurns)
	}
	if got.CreatedAt != want.CreatedAt {
		t.Fatalf("CreatedAt = %d, want %d", got.CreatedAt, want.CreatedAt)
	}
	if got.UpdatedAt != want.UpdatedAt {
		t.Fatalf("UpdatedAt = %d, want %d", got.UpdatedAt, want.UpdatedAt)
	}
	if len(got.Participants) != len(want.Participants) {
		t.Fatalf("Participants len = %d, want %d", len(got.Participants), len(want.Participants))
	}
	for i := range want.Participants {
		if got.Participants[i] != want.Participants[i] {
			t.Fatalf("Participants[%d] = %+v, want %+v", i, got.Participants[i], want.Participants[i])
		}
	}
}
