package discussion

import (
	"path/filepath"
	"testing"

	"agent-overflow/internal/store"
)

func TestRegistryCRUD(t *testing.T) {
	st := newDiscussionTestStore(t)
	registry := NewRegistry(st)

	def := store.DiscussionDefinition{
		Name:        "Architects",
		Description: "Review tradeoffs",
		Scope:       "global",
		Participants: []store.DiscussionParticipant{
			{Role: "proposer", System: "Drive the proposal"},
			{Role: "reviewer", System: "Critique the proposal"},
		},
		Settings: store.DiscussionSettings{MaxTurns: 6},
	}

	if err := registry.Create(def); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got, err := registry.Get("Architects", "global")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.ID == "" {
		t.Fatal("expected generated discussion ID")
	}
	if got.Settings.MaxTurns != 6 {
		t.Fatalf("MaxTurns = %d, want 6", got.Settings.MaxTurns)
	}

	defs, err := registry.List("global")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(defs) != 1 {
		t.Fatalf("len(List()) = %d, want 1", len(defs))
	}

	got.Description = "Updated"
	if err := registry.Update("Architects", "global", got); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	updated, err := registry.Get("Architects", "global")
	if err != nil {
		t.Fatalf("Get(updated) error = %v", err)
	}
	if updated.Description != "Updated" {
		t.Fatalf("Description = %q, want Updated", updated.Description)
	}
	if updated.CreatedAt != got.CreatedAt {
		t.Fatalf("CreatedAt changed from %d to %d", got.CreatedAt, updated.CreatedAt)
	}

	if err := registry.Delete("Architects", "global"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := registry.Get("Architects", "global"); err == nil {
		t.Fatal("expected deleted definition lookup to fail")
	}
}

func TestRegistryValidation(t *testing.T) {
	st := newDiscussionTestStore(t)
	registry := NewRegistry(st)

	tests := []struct {
		name string
		def  store.DiscussionDefinition
	}{
		{
			name: "missing name",
			def: store.DiscussionDefinition{
				Participants: []store.DiscussionParticipant{
					{Role: "a", System: "x"},
					{Role: "b", System: "y"},
				},
			},
		},
		{
			name: "not enough participants",
			def: store.DiscussionDefinition{
				Name: "Solo",
				Participants: []store.DiscussionParticipant{
					{Role: "a", System: "x"},
				},
			},
		},
		{
			name: "missing role",
			def: store.DiscussionDefinition{
				Name: "Missing Role",
				Participants: []store.DiscussionParticipant{
					{Role: "", System: "x"},
					{Role: "b", System: "y"},
				},
			},
		},
		{
			name: "missing system",
			def: store.DiscussionDefinition{
				Name: "Missing System",
				Participants: []store.DiscussionParticipant{
					{Role: "a", System: ""},
					{Role: "b", System: "y"},
				},
			},
		},
	}

	for _, tt := range tests {
		if err := registry.Create(tt.def); err == nil {
			t.Fatalf("%s: expected validation error", tt.name)
		}
	}
}

func TestRegistryProjectScopeResolution(t *testing.T) {
	st := newDiscussionTestStore(t)
	registry := NewRegistry(st)

	projectDef := store.DiscussionDefinition{
		Name:      "Architects",
		Scope:     "project",
		ProjectID: "project-a",
		Participants: []store.DiscussionParticipant{
			{Role: "proposer", System: "Design the change"},
			{Role: "reviewer", System: "Review the change"},
		},
	}
	if err := registry.Create(projectDef); err != nil {
		t.Fatalf("Create(project) error = %v", err)
	}

	got, err := registry.Get("Architects", "project")
	if err != nil {
		t.Fatalf("Get(project) error = %v", err)
	}
	if got.ProjectID != "project-a" {
		t.Fatalf("ProjectID = %q, want project-a", got.ProjectID)
	}

	got.Description = "Updated"
	if err := registry.Update("Architects", "project", got); err != nil {
		t.Fatalf("Update(project) error = %v", err)
	}

	updated, err := registry.Get("Architects", "project")
	if err != nil {
		t.Fatalf("Get(updated project) error = %v", err)
	}
	if updated.Description != "Updated" {
		t.Fatalf("Description = %q, want Updated", updated.Description)
	}

	if err := registry.Delete("Architects", "project"); err != nil {
		t.Fatalf("Delete(project) error = %v", err)
	}
	if _, err := registry.Get("Architects", "project"); err == nil {
		t.Fatal("expected deleted project discussion lookup to fail")
	}
}

func TestRegistryProjectScopeRejectsAmbiguousMatches(t *testing.T) {
	st := newDiscussionTestStore(t)
	registry := NewRegistry(st)

	for _, projectID := range []string{"project-a", "project-b"} {
		if err := registry.Create(store.DiscussionDefinition{
			Name:      "Architects",
			Scope:     "project",
			ProjectID: projectID,
			Participants: []store.DiscussionParticipant{
				{Role: "proposer", System: "Design the change"},
				{Role: "reviewer", System: "Review the change"},
			},
		}); err != nil {
			t.Fatalf("Create(%s) error = %v", projectID, err)
		}
	}

	if _, err := registry.Get("Architects", "project"); err == nil {
		t.Fatal("expected ambiguous project lookup to fail")
	}
}

func TestRegistryDefaultsScopeAndMaxTurns(t *testing.T) {
	st := newDiscussionTestStore(t)
	registry := NewRegistry(st)

	if err := registry.Create(store.DiscussionDefinition{
		Name: "Defaults",
		Participants: []store.DiscussionParticipant{
			{Role: "proposer", System: "Lead"},
			{Role: "reviewer", System: "Challenge"},
		},
		Settings: store.DiscussionSettings{MaxTurns: 0},
	}); err != nil {
		t.Fatalf("Create(defaults) error = %v", err)
	}

	got, err := registry.Get("Defaults", "global")
	if err != nil {
		t.Fatalf("Get(defaults) error = %v", err)
	}
	if got.Scope != "global" {
		t.Fatalf("Scope = %q, want global", got.Scope)
	}
	if got.Settings.MaxTurns != 8 {
		t.Fatalf("MaxTurns = %d, want 8", got.Settings.MaxTurns)
	}
}

func newDiscussionTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "discussion.db"))
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close()
	})
	return st
}
