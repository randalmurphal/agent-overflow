package discussion

import (
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/store"

	"github.com/google/uuid"
)

// Registry manages persisted discussion definitions.
type Registry struct {
	store *store.Store
}

// NewRegistry constructs a discussion registry service.
func NewRegistry(st *store.Store) *Registry {
	return &Registry{store: st}
}

// List returns definitions for the requested scope.
func (r *Registry) List(scope string) ([]store.DiscussionDefinition, error) {
	if r.store == nil {
		return nil, fmt.Errorf("discussion registry unavailable")
	}
	return r.store.ListDiscussionDefs(strings.TrimSpace(scope), "")
}

// Get returns a single definition by name and scope.
func (r *Registry) Get(name, scope string) (store.DiscussionDefinition, error) {
	if r.store == nil {
		return store.DiscussionDefinition{}, fmt.Errorf("discussion registry unavailable")
	}
	return r.findDefinition(strings.TrimSpace(name), strings.TrimSpace(scope))
}

// Create validates and persists a new definition.
func (r *Registry) Create(def store.DiscussionDefinition) error {
	if r.store == nil {
		return fmt.Errorf("discussion registry unavailable")
	}
	normalized, err := normalizeDiscussionDefinition(def, 0)
	if err != nil {
		return err
	}
	return r.store.CreateDiscussionDef(normalized)
}

// Update validates and replaces an existing definition.
func (r *Registry) Update(previousName, previousScope string, def store.DiscussionDefinition) error {
	if r.store == nil {
		return fmt.Errorf("discussion registry unavailable")
	}
	existing, err := r.findDefinition(strings.TrimSpace(previousName), strings.TrimSpace(previousScope))
	if err != nil {
		return err
	}
	normalized, err := normalizeDiscussionDefinition(def, existing.CreatedAt)
	if err != nil {
		return err
	}
	return r.store.UpdateDiscussionDef(existing.Name, existing.Scope, existing.ProjectID, normalized)
}

// Delete removes a definition by name and scope.
func (r *Registry) Delete(name, scope string) error {
	if r.store == nil {
		return fmt.Errorf("discussion registry unavailable")
	}
	existing, err := r.findDefinition(strings.TrimSpace(name), strings.TrimSpace(scope))
	if err != nil {
		return err
	}
	return r.store.DeleteDiscussionDef(existing.Name, existing.Scope, existing.ProjectID)
}

func normalizeDiscussionDefinition(def store.DiscussionDefinition, createdAt int64) (store.DiscussionDefinition, error) {
	def.Name = strings.TrimSpace(def.Name)
	def.Description = strings.TrimSpace(def.Description)
	def.Scope = normalizeDiscussionScope(def.Scope)
	def.ProjectID = strings.TrimSpace(def.ProjectID)

	if def.Name == "" {
		return store.DiscussionDefinition{}, fmt.Errorf("discussion name is required")
	}
	if len(def.Participants) < 2 {
		return store.DiscussionDefinition{}, fmt.Errorf("discussion requires at least 2 participants")
	}
	if def.Scope == "project" && def.ProjectID == "" {
		return store.DiscussionDefinition{}, fmt.Errorf("project-scoped discussion requires project ID")
	}
	if def.Scope != "project" {
		def.ProjectID = ""
	}

	for i := range def.Participants {
		def.Participants[i].Role = strings.TrimSpace(def.Participants[i].Role)
		def.Participants[i].Description = strings.TrimSpace(def.Participants[i].Description)
		def.Participants[i].System = strings.TrimSpace(def.Participants[i].System)
		def.Participants[i].Provider = strings.TrimSpace(def.Participants[i].Provider)
		def.Participants[i].Model = strings.TrimSpace(def.Participants[i].Model)

		if def.Participants[i].Role == "" {
			return store.DiscussionDefinition{}, fmt.Errorf("participant %d role is required", i)
		}
		if def.Participants[i].System == "" {
			return store.DiscussionDefinition{}, fmt.Errorf("participant %d system prompt is required", i)
		}
	}

	if def.Settings.MaxTurns <= 0 {
		def.Settings.MaxTurns = 8
	}

	now := time.Now().UnixMilli()
	if def.ID == "" {
		def.ID = uuid.New().String()
	}
	if createdAt <= 0 {
		createdAt = now
	}
	def.CreatedAt = createdAt
	def.UpdatedAt = now
	return def, nil
}

func normalizeDiscussionScope(scope string) string {
	switch strings.TrimSpace(scope) {
	case "", "global":
		return "global"
	case "project":
		return "project"
	default:
		return "global"
	}
}

func (r *Registry) findDefinition(name, scope string) (store.DiscussionDefinition, error) {
	if scope != "project" {
		return r.store.GetDiscussionDef(name, scope, "")
	}

	defs, err := r.store.ListDiscussionDefs(scope, "")
	if err != nil {
		return store.DiscussionDefinition{}, err
	}

	var matches []store.DiscussionDefinition
	for _, def := range defs {
		if def.Name == name {
			matches = append(matches, def)
		}
	}

	switch len(matches) {
	case 0:
		return store.DiscussionDefinition{}, fmt.Errorf("discussion %q not found in scope %q", name, scope)
	case 1:
		return matches[0], nil
	default:
		return store.DiscussionDefinition{}, fmt.Errorf("discussion %q in scope %q is ambiguous across projects", name, scope)
	}
}
