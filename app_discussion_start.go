package main

import (
	"errors"
	"fmt"
	"time"

	"agent-overflow/internal/discussion"
	"agent-overflow/internal/store"
)

func (a *App) startDiscussion(threadID, discussionName string) error {
	if a.store == nil || a.channels == nil {
		return fmt.Errorf("discussion services unavailable")
	}

	parent, err := a.store.GetThread(threadID)
	if err != nil {
		return err
	}
	if err := a.ensureDiscussionCanStart(parent); err != nil {
		return err
	}

	def, err := a.resolveDiscussionDefinition(parent, discussionName)
	if err != nil {
		return err
	}

	return a.startDiscussionWithDefinition(parent, def)
}

func (a *App) startDiscussionWithDefinition(parent store.Thread, def store.DiscussionDefinition) error {
	plans, err := discussion.BuildParticipantPlans(parent, def, time.Now().UnixMilli())
	if err != nil {
		return err
	}

	if _, err := a.createDiscussionRuntime(parent, plans, def.Settings.MaxTurns); err != nil {
		return err
	}
	return nil
}

func (a *App) ensureDiscussionCanStart(parent store.Thread) error {
	if parent.DiscussionID != "" || parent.Mode == "discussion" {
		return fmt.Errorf("thread %s already has an active discussion", parent.ID)
	}

	// Once a thread has carried a normal chat (any persisted item),
	// promoting it into a discussion would orphan those messages behind
	// the DiscussionView and leave the user without a way to see them.
	// Same rule as the provider lock: once a thread has picked a lane,
	// that's the lane. Discussion participants are spawned in fresh
	// child threads instead.
	hasItems, err := a.store.HasItems(parent.ID)
	if err != nil {
		return fmt.Errorf("check prior items for thread %s: %w", parent.ID, err)
	}
	if hasItems {
		return fmt.Errorf("thread %s has chat history; start a new thread to begin a discussion", parent.ID)
	}

	hasChildren, err := a.store.HasChildThreads(parent.ID)
	if err != nil {
		return err
	}
	if hasChildren {
		return fmt.Errorf("thread %s already has discussion participants", parent.ID)
	}
	return nil
}

func (a *App) createDiscussionRuntime(
	parent store.Thread,
	plans []discussion.ParticipantPlan,
	maxTurns int,
) (store.Channel, error) {
	createdIDs, err := a.createDiscussionParticipantThreads(plans)
	if err != nil {
		return store.Channel{}, err
	}

	channel, err := a.channels.Create(parent.ID, "deliberation")
	if err != nil {
		return store.Channel{}, a.cleanupDiscussionSetup("", nil, createdIDs, err)
	}

	if err := a.linkDiscussionParticipants(channel.ID, plans); err != nil {
		return store.Channel{}, a.cleanupDiscussionSetup(channel.ID, nil, createdIDs, err)
	}

	a.installDeliberation(channel.ID, maxTurns)

	startedIDs, err := a.startDiscussionParticipantSessions(plans)
	if err != nil {
		return store.Channel{}, a.cleanupDiscussionSetup(channel.ID, startedIDs, createdIDs, err)
	}

	if err := a.persistDiscussionParent(parent, channel); err != nil {
		return store.Channel{}, a.cleanupDiscussionSetup(channel.ID, startedIDs, createdIDs, err)
	}

	return channel, nil
}

func (a *App) createDiscussionParticipantThreads(plans []discussion.ParticipantPlan) ([]string, error) {
	created := make([]string, 0, len(plans))
	for _, plan := range plans {
		if err := a.store.CreateThread(plan.Thread); err != nil {
			cleanupErr := a.cleanupDiscussionSetup("", nil, created, err)
			return nil, cleanupErr
		}
		created = append(created, plan.Thread.ID)
	}
	return created, nil
}

func (a *App) startDiscussionParticipantSessions(plans []discussion.ParticipantPlan) ([]string, error) {
	started := make([]string, 0, len(plans))
	for _, plan := range plans {
		a.setThreadSystemPrompt(plan.Thread.ID, plan.SystemPrompt)
		if err := a.startSession(plan.Thread.ID); err != nil {
			a.clearThreadSystemPrompt(plan.Thread.ID)
			return started, fmt.Errorf("start discussion participant %s: %w", plan.Thread.ID, err)
		}
		started = append(started, plan.Thread.ID)
	}
	return started, nil
}

func (a *App) linkDiscussionParticipants(channelID string, plans []discussion.ParticipantPlan) error {
	for _, plan := range plans {
		child := plan.Thread
		child.DiscussionID = channelID
		child.UpdatedAt = max(child.UpdatedAt+1, time.Now().UnixMilli())
		if err := a.store.UpdateThread(child); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) persistDiscussionParent(parent store.Thread, channel store.Channel) error {
	parent.Mode = "discussion"
	parent.DiscussionID = channel.ID
	parent.UpdatedAt = max(parent.UpdatedAt+1, channel.CreatedAt)
	return a.store.UpdateThread(parent)
}

func (a *App) cleanupDiscussionSetup(
	channelID string,
	startedIDs []string,
	createdIDs []string,
	cause error,
) error {
	var errs []error

	if channelID != "" {
		a.removeDeliberationByID(channelID)
	}

	for _, threadID := range startedIDs {
		if err := a.stopSession(threadID); err != nil {
			errs = append(errs, fmt.Errorf("stop discussion participant %s: %w", threadID, err))
		}
	}

	for _, threadID := range createdIDs {
		a.clearThreadSystemPrompt(threadID)
		if err := a.store.DeleteThread(threadID); err != nil {
			errs = append(errs, fmt.Errorf("delete discussion participant %s: %w", threadID, err))
		}
	}

	if channelID != "" {
		if err := a.store.DeleteChannel(channelID); err != nil {
			errs = append(errs, fmt.Errorf("delete discussion channel %s: %w", channelID, err))
		}
	}

	errs = append([]error{cause}, errs...)
	return errors.Join(errs...)
}

func (a *App) installDeliberation(channelID string, maxTurns int) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.deliberations == nil {
		a.deliberations = make(map[string]*discussion.Deliberation)
	}
	a.deliberations[channelID] = discussion.NewDeliberation(channelID, maxTurns)
}

