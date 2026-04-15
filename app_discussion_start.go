package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/discussion"
	"agent-overflow/internal/store"

	"github.com/google/uuid"
)

type discussionParticipantPlan struct {
	thread       store.Thread
	systemPrompt string
}

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

	plans, err := buildDiscussionParticipantPlans(parent, def)
	if err != nil {
		return err
	}

	channel, err := a.createDiscussionRuntime(parent, plans)
	if err != nil {
		return err
	}

	a.installDeliberation(channel.ID, def.Settings.MaxTurns)
	return nil
}

func (a *App) ensureDiscussionCanStart(parent store.Thread) error {
	if parent.DiscussionID != "" || parent.InteractionMode == "discussion" {
		return fmt.Errorf("thread %s already has an active discussion", parent.ID)
	}

	threads, err := a.store.ListThreads()
	if err != nil {
		return err
	}
	for _, thread := range threads {
		if thread.ParentThreadID == parent.ID {
			return fmt.Errorf("thread %s already has discussion participants", parent.ID)
		}
	}
	return nil
}

func buildDiscussionParticipantPlans(
	parent store.Thread,
	def store.DiscussionDefinition,
) ([]discussionParticipantPlan, error) {
	plans := make([]discussionParticipantPlan, 0, len(def.Participants))
	now := time.Now().UnixMilli()

	for _, participant := range def.Participants {
		role := strings.TrimSpace(participant.Role)
		providerName := firstNonEmpty(participant.Provider, parent.Provider)
		model := firstNonEmpty(participant.Model, parent.Model)
		if providerName == "" {
			return nil, fmt.Errorf("discussion participant %q is missing a provider", role)
		}
		if model == "" {
			return nil, fmt.Errorf("discussion participant %q is missing a model", role)
		}

		child := store.Thread{
			ID:              uuid.NewString(),
			Title:           fmt.Sprintf("%s - %s", parent.Title, formatDiscussionRole(role)),
			Provider:        providerName,
			WorkspacePath:   parent.WorkspacePath,
			Model:           model,
			ProjectPath:     parent.ProjectPath,
			WorktreePath:    parent.WorktreePath,
			Branch:          parent.Branch,
			InteractionMode: "discussion",
			ParentThreadID:  parent.ID,
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		plans = append(plans, discussionParticipantPlan{
			thread:       child,
			systemPrompt: buildDiscussionParticipantPrompt(role, participant.System),
		})
	}

	return plans, nil
}

func (a *App) createDiscussionRuntime(
	parent store.Thread,
	plans []discussionParticipantPlan,
) (store.Channel, error) {
	createdIDs, err := a.createDiscussionParticipantThreads(plans)
	if err != nil {
		return store.Channel{}, err
	}

	startedIDs, err := a.startDiscussionParticipantSessions(plans)
	if err != nil {
		return store.Channel{}, a.cleanupDiscussionSetup("", startedIDs, createdIDs, err)
	}

	channel, err := a.channels.Create(parent.ID, "deliberation")
	if err != nil {
		return store.Channel{}, a.cleanupDiscussionSetup("", startedIDs, createdIDs, err)
	}

	if err := a.persistDiscussionRuntime(parent, channel, plans); err != nil {
		return store.Channel{}, a.cleanupDiscussionSetup(channel.ID, startedIDs, createdIDs, err)
	}

	return channel, nil
}

func (a *App) createDiscussionParticipantThreads(plans []discussionParticipantPlan) ([]string, error) {
	created := make([]string, 0, len(plans))
	for _, plan := range plans {
		if err := a.store.CreateThread(plan.thread); err != nil {
			cleanupErr := a.cleanupDiscussionSetup("", nil, created, err)
			return nil, cleanupErr
		}
		created = append(created, plan.thread.ID)
	}
	return created, nil
}

func (a *App) startDiscussionParticipantSessions(plans []discussionParticipantPlan) ([]string, error) {
	started := make([]string, 0, len(plans))
	for _, plan := range plans {
		a.setThreadSystemPrompt(plan.thread.ID, plan.systemPrompt)
		if err := a.startSession(plan.thread.ID); err != nil {
			a.clearThreadSystemPrompt(plan.thread.ID)
			return started, fmt.Errorf("start discussion participant %s: %w", plan.thread.ID, err)
		}
		started = append(started, plan.thread.ID)
	}
	return started, nil
}

func (a *App) persistDiscussionRuntime(
	parent store.Thread,
	channel store.Channel,
	plans []discussionParticipantPlan,
) error {
	for _, plan := range plans {
		child := plan.thread
		child.DiscussionID = channel.ID
		child.UpdatedAt = maxInt64(child.UpdatedAt+1, channel.CreatedAt)
		if err := a.store.UpdateThread(child); err != nil {
			return err
		}
	}

	parent.InteractionMode = "discussion"
	parent.DiscussionID = channel.ID
	parent.UpdatedAt = maxInt64(parent.UpdatedAt+1, channel.CreatedAt)
	return a.store.UpdateThread(parent)
}

func (a *App) cleanupDiscussionSetup(
	channelID string,
	startedIDs []string,
	createdIDs []string,
	cause error,
) error {
	var errs []error

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

func buildDiscussionParticipantPrompt(role, rawSystem string) string {
	return joinSystemPrompts(
		fmt.Sprintf("You are the %s participant in a discussion thread.", formatDiscussionRole(role)),
		rawSystem,
	)
}

func formatDiscussionRole(role string) string {
	parts := strings.FieldsFunc(strings.TrimSpace(role), func(r rune) bool {
		return r == '-' || r == '_' || r == ' '
	})
	for i, part := range parts {
		if part == "" {
			continue
		}
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	if len(parts) == 0 {
		return "Participant"
	}
	return strings.Join(parts, " ")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}
