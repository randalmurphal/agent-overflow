package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/discussion"
	"agent-overflow/internal/store"
	"agent-overflow/internal/stringsx"

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
			ID:             uuid.NewString(),
			ProjectID:      parent.ProjectID,
			Title:          fmt.Sprintf("%s - %s", parent.Title, formatDiscussionRole(role)),
			Provider:       providerName,
			WorkspacePath:  parent.WorkspacePath,
			Model:          model,
			WorktreePath:   parent.WorktreePath,
			Branch:         parent.Branch,
			Mode:           "discussion",
			ParentThreadID: parent.ID,
			CreatedAt:      now,
			UpdatedAt:      now,
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

func (a *App) linkDiscussionParticipants(channelID string, plans []discussionParticipantPlan) error {
	for _, plan := range plans {
		child := plan.thread
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

// firstNonEmpty returns the first input that is non-blank after TrimSpace,
// with the whitespace stripped. Thin wrapper over stringsx.FirstNonEmptyTrimmed
// so call sites in the main package stay concise.
func firstNonEmpty(values ...string) string {
	return stringsx.FirstNonEmptyTrimmed(values...)
}
