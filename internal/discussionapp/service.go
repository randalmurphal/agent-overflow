// Package discussionapp owns application-level discussion coordination.
//
// The provider-agnostic state machines remain in internal/discussion. This
// package composes them with persistence, participant sessions, and live UI
// events without making either the Wails App or a provider runtime part of the
// domain model.
package discussionapp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"

	"agent-overflow/internal/discussion"
	"agent-overflow/internal/store"
)

// ParticipantRuntime is the narrow session-lifecycle boundary used by a
// discussion. The App adapter remains responsible for session ownership and
// provider-specific behavior.
type ParticipantRuntime interface {
	StartParticipant(context.Context, string, string) error
	StopParticipant(string) error
	ClearParticipantPrompt(string)
	SendParticipantMessage(string, string) error
}

// MessageEvent is emitted after a channel message has committed.
type MessageEvent struct {
	ChannelID string
	ThreadID  string
	Message   store.ChannelMessage
}

// ParticipantState is the application projection of one participant.
type ParticipantState struct {
	ThreadID           string
	Role               string
	Provider           string
	Model              string
	ProposedConclusion bool
}

// State is the application projection of a discussion channel.
type State struct {
	ChannelID              string
	ThreadID               string
	Status                 string
	TurnCount              int
	MaxTurns               int
	AwaitingResponse       bool
	CurrentSpeakerThreadID string
	CurrentSpeakerRole     string
	Participants           []ParticipantState
}

// Events receives committed discussion projections. Callbacks are synchronous
// and should return promptly; Service holds none of its own locks while calling.
type Events interface {
	Message(MessageEvent)
	State(State)
	Error(string, string)
}

// Config provides the explicit side-effect boundaries used by Service.
type Config struct {
	Store   func() *store.Store
	Runtime ParticipantRuntime
	Events  Events
}

// Service owns all process-local discussion coordination state.
type Service struct {
	storeSource func() *store.Store
	runtime     ParticipantRuntime
	events      Events

	servicesMu sync.Mutex
	boundStore *store.Store
	registry   *discussion.Registry
	channels   *discussion.ChannelService

	mu            sync.RWMutex
	deliberations map[string]*discussion.Deliberation
}

func New(config Config) *Service {
	return &Service{
		storeSource:   config.Store,
		runtime:       config.Runtime,
		events:        config.Events,
		deliberations: make(map[string]*discussion.Deliberation),
	}
}

func (s *Service) services() (*store.Store, *discussion.Registry, *discussion.ChannelService, error) {
	if s == nil || s.storeSource == nil {
		return nil, nil, nil, fmt.Errorf("discussion services unavailable")
	}
	st := s.storeSource()
	if st == nil {
		return nil, nil, nil, fmt.Errorf("discussion services unavailable")
	}

	// App wiring installs the store during startup, while test fixtures may
	// replace it with a reopened handle. Bind the two stateless domain services
	// together whenever that concrete store identity changes.
	s.servicesMu.Lock()
	defer s.servicesMu.Unlock()
	if s.boundStore != st {
		s.boundStore = st
		s.registry = discussion.NewRegistry(st)
		s.channels = discussion.NewChannelService(st)
	}
	return st, s.registry, s.channels, nil
}

func (s *Service) List(scope string) ([]store.DiscussionDefinition, error) {
	_, registry, _, err := s.services()
	if err != nil {
		return nil, fmt.Errorf("discussion registry unavailable")
	}
	return registry.List(scope)
}

func (s *Service) ListForThread(threadID string) ([]store.DiscussionDefinition, error) {
	st, _, _, err := s.services()
	if err != nil {
		return nil, fmt.Errorf("discussion store unavailable")
	}
	thread, err := st.GetThread(threadID)
	if err != nil {
		return nil, err
	}

	var defs []store.DiscussionDefinition
	projectPath, err := projectPathForThread(st, thread)
	if err == nil && projectPath != "" {
		projectDefs, listErr := st.ListDiscussionDefs("project", projectPath)
		if listErr != nil {
			return nil, listErr
		}
		defs = append(defs, projectDefs...)
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	globalDefs, err := st.ListDiscussionDefs("global", "")
	if err != nil {
		return nil, err
	}
	return append(defs, globalDefs...), nil
}

func (s *Service) Get(name, scope string) (store.DiscussionDefinition, error) {
	_, registry, _, err := s.services()
	if err != nil {
		return store.DiscussionDefinition{}, fmt.Errorf("discussion registry unavailable")
	}
	return registry.Get(name, scope)
}

func (s *Service) Create(def store.DiscussionDefinition) error {
	_, registry, _, err := s.services()
	if err != nil {
		return fmt.Errorf("discussion registry unavailable")
	}
	return registry.Create(def)
}

func (s *Service) Update(prevName, prevScope string, def store.DiscussionDefinition) error {
	_, registry, _, err := s.services()
	if err != nil {
		return fmt.Errorf("discussion registry unavailable")
	}
	return registry.Update(prevName, prevScope, def)
}

func (s *Service) Delete(name, scope string) error {
	_, registry, _, err := s.services()
	if err != nil {
		return fmt.Errorf("discussion registry unavailable")
	}
	return registry.Delete(name, scope)
}

func (s *Service) GetMessages(channelID string, afterSeq, limit int) ([]store.ChannelMessage, error) {
	_, _, channels, err := s.services()
	if err != nil {
		return nil, fmt.Errorf("channel service unavailable")
	}
	return channels.GetMessages(channelID, afterSeq, limit)
}

func (s *Service) resolveDefinition(st *store.Store, thread store.Thread, name string) (store.DiscussionDefinition, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return store.DiscussionDefinition{}, fmt.Errorf("discussion name is required")
	}
	if thread.ProjectID != "" {
		project, err := st.GetProject(thread.ProjectID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return store.DiscussionDefinition{}, err
		}
		if err == nil && project.Path != "" {
			def, getErr := st.GetDiscussionDef(name, "project", project.Path)
			if getErr == nil {
				return def, nil
			}
			if !errors.Is(getErr, sql.ErrNoRows) {
				return store.DiscussionDefinition{}, getErr
			}
		}
	}
	return st.GetDiscussionDef(name, "global", "")
}

// ResolveDefinition applies project-first, global-fallback resolution for a
// thread. Start uses the same path; the exported form supports diagnostics and
// focused application-service tests without duplicating scope policy at root.
func (s *Service) ResolveDefinition(thread store.Thread, name string) (store.DiscussionDefinition, error) {
	st, _, _, err := s.services()
	if err != nil {
		return store.DiscussionDefinition{}, err
	}
	return s.resolveDefinition(st, thread, name)
}

func projectPathForThread(st *store.Store, thread store.Thread) (string, error) {
	if strings.TrimSpace(thread.ProjectID) == "" {
		return "", fmt.Errorf("thread %s has no project", thread.ID)
	}
	project, err := st.GetProject(thread.ProjectID)
	if err != nil {
		return "", err
	}
	return project.Path, nil
}

func ensureDefinitionInScope(st *store.Store, parent store.Thread, def store.DiscussionDefinition) error {
	if def.Scope != "project" {
		return nil
	}
	projectPath, err := projectPathForThread(st, parent)
	if err != nil {
		return err
	}
	if def.ProjectID != projectPath {
		return fmt.Errorf("discussion %q belongs to a different project", def.Name)
	}
	return nil
}
