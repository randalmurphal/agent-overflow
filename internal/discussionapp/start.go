package discussionapp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"agent-overflow/internal/discussion"
	"agent-overflow/internal/store"
)

func (s *Service) Start(threadID, discussionName string) error {
	st, _, _, err := s.services()
	if err != nil {
		return err
	}
	parent, err := st.GetThread(threadID)
	if err != nil {
		return err
	}
	if err := ensureCanStart(st, parent); err != nil {
		return err
	}
	def, err := s.resolveDefinition(st, parent, discussionName)
	if err != nil {
		return err
	}
	return s.startWithDefinition(st, parent, def)
}

func (s *Service) StartByID(threadID, discussionID string) error {
	st, _, _, err := s.services()
	if err != nil {
		return err
	}
	parent, err := st.GetThread(threadID)
	if err != nil {
		return err
	}
	if err := ensureCanStart(st, parent); err != nil {
		return err
	}
	def, err := st.GetDiscussionDefByID(discussionID)
	if err != nil {
		return err
	}
	if err := ensureDefinitionInScope(st, parent, def); err != nil {
		return err
	}
	return s.startWithDefinition(st, parent, def)
}

func (s *Service) startWithDefinition(st *store.Store, parent store.Thread, def store.DiscussionDefinition) error {
	plans, err := discussion.BuildParticipantPlans(parent, def, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	_, err = s.createRuntime(st, parent, plans, def.Settings.MaxTurns)
	return err
}

func ensureCanStart(st *store.Store, parent store.Thread) error {
	if parent.DiscussionID != "" || parent.Mode == "discussion" {
		return fmt.Errorf("thread %s already has an active discussion", parent.ID)
	}
	hasItems, err := st.HasItems(parent.ID)
	if err != nil {
		return fmt.Errorf("check prior items for thread %s: %w", parent.ID, err)
	}
	if hasItems {
		return fmt.Errorf("thread %s has chat history; start a new thread to begin a discussion", parent.ID)
	}
	hasChildren, err := st.HasChildThreads(parent.ID)
	if err != nil {
		return err
	}
	if hasChildren {
		return fmt.Errorf("thread %s already has discussion participants", parent.ID)
	}
	return nil
}

func (s *Service) createRuntime(st *store.Store, parent store.Thread, plans []discussion.ParticipantPlan, maxTurns int) (store.Channel, error) {
	created, err := createParticipantThreads(st, plans)
	if err != nil {
		return store.Channel{}, err
	}
	_, _, channels, err := s.services()
	if err != nil {
		return store.Channel{}, s.cleanupSetup(st, "", nil, created, err)
	}
	channel, err := channels.Create(parent.ID, "deliberation", maxTurns)
	if err != nil {
		return store.Channel{}, s.cleanupSetup(st, "", nil, created, err)
	}
	if err := linkParticipants(st, channel.ID, plans); err != nil {
		return store.Channel{}, s.cleanupSetup(st, channel.ID, nil, created, err)
	}
	participants := make([]string, 0, len(plans))
	for _, plan := range plans {
		participants = append(participants, plan.Thread.ID)
	}
	s.installDeliberation(channel.ID, discussion.NewDeliberation(channel.ID, channel.MaxTurns, participants))
	started, err := s.startParticipants(plans)
	if err != nil {
		return store.Channel{}, s.cleanupSetup(st, channel.ID, started, created, err)
	}
	parent.Mode = "discussion"
	parent.DiscussionID = channel.ID
	if err := st.UpdateThread(parent); err != nil {
		return store.Channel{}, s.cleanupSetup(st, channel.ID, started, created, err)
	}
	s.emitState(channel.ID)
	return channel, nil
}

func createParticipantThreads(st *store.Store, plans []discussion.ParticipantPlan) ([]string, error) {
	created := make([]string, 0, len(plans))
	for _, plan := range plans {
		if err := st.CreateThread(plan.Thread); err != nil {
			var errs = []error{err}
			for _, threadID := range created {
				if deleteErr := st.DeleteThread(threadID); deleteErr != nil {
					errs = append(errs, deleteErr)
				}
			}
			return nil, errors.Join(errs...)
		}
		created = append(created, plan.Thread.ID)
	}
	return created, nil
}

func linkParticipants(st *store.Store, channelID string, plans []discussion.ParticipantPlan) error {
	for _, plan := range plans {
		child := plan.Thread
		child.DiscussionID = channelID
		if err := st.UpdateThread(child); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) startParticipants(plans []discussion.ParticipantPlan) ([]string, error) {
	if s.runtime == nil {
		return nil, fmt.Errorf("discussion participant runtime unavailable")
	}
	started := make([]string, 0, len(plans))
	for _, plan := range plans {
		if err := s.runtime.StartParticipant(context.Background(), plan.Thread.ID, plan.SystemPrompt); err != nil {
			s.runtime.ClearParticipantPrompt(plan.Thread.ID)
			return started, fmt.Errorf("start discussion participant %s: %w", plan.Thread.ID, err)
		}
		started = append(started, plan.Thread.ID)
	}
	return started, nil
}

func (s *Service) cleanupSetup(st *store.Store, channelID string, started, created []string, cause error) error {
	errs := []error{cause}
	if channelID != "" {
		s.Remove(channelID)
	}
	for _, threadID := range started {
		if s.runtime != nil {
			if err := s.runtime.StopParticipant(threadID); err != nil {
				errs = append(errs, fmt.Errorf("stop discussion participant %s: %w", threadID, err))
			}
		}
	}
	for _, threadID := range created {
		if s.runtime != nil {
			s.runtime.ClearParticipantPrompt(threadID)
		}
		if err := st.DeleteThread(threadID); err != nil {
			errs = append(errs, fmt.Errorf("delete discussion participant %s: %w", threadID, err))
		}
	}
	if channelID != "" {
		if err := st.DeleteChannel(channelID); err != nil {
			errs = append(errs, fmt.Errorf("delete discussion channel %s: %w", channelID, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Service) RemoveForThread(thread store.Thread) {
	if thread.DiscussionID != "" && thread.ParentThreadID == "" {
		s.Remove(thread.DiscussionID)
	}
}

func (s *Service) Remove(channelID string) {
	if s == nil || channelID == "" {
		return
	}
	s.mu.Lock()
	delete(s.deliberations, channelID)
	s.mu.Unlock()
}

func (s *Service) installDeliberation(channelID string, deliberation *discussion.Deliberation) {
	s.mu.Lock()
	s.deliberations[channelID] = deliberation
	s.mu.Unlock()
}

// InstallRuntime installs an already-constructed process-local runtime. It is
// primarily useful to reconstruct application fixtures around persisted
// channels; normal production starts install their runtime atomically.
func (s *Service) InstallRuntime(channelID string, deliberation *discussion.Deliberation) {
	s.installDeliberation(channelID, deliberation)
}

// Runtime returns the currently tracked runtime without rebuilding it.
func (s *Service) Runtime(channelID string) (*discussion.Deliberation, bool) {
	return s.deliberation(channelID)
}

// ResolveRuntime returns the tracked runtime or rebuilds it from persistence.
func (s *Service) ResolveRuntime(channelID string) (*discussion.Deliberation, error) {
	return s.deliberationForChannel(channelID)
}

// RuntimeCount returns the number of live process-local runtimes.
func (s *Service) RuntimeCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.deliberations)
}
