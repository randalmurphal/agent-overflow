package main

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"agent-overflow/internal/discussion"
	"agent-overflow/internal/store"
)

// ListDiscussions returns persisted discussion definitions for the given scope.
func (a *App) ListDiscussions(scope string) ([]store.DiscussionDefinition, error) {
	if a.registry == nil {
		return nil, fmt.Errorf("discussion registry unavailable")
	}
	return a.registry.List(scope)
}

// GetDiscussion returns a persisted discussion definition by name and scope.
func (a *App) GetDiscussion(name, scope string) (store.DiscussionDefinition, error) {
	if a.registry == nil {
		return store.DiscussionDefinition{}, fmt.Errorf("discussion registry unavailable")
	}
	return a.registry.Get(name, scope)
}

// CreateDiscussion validates and persists a discussion definition.
func (a *App) CreateDiscussion(def store.DiscussionDefinition) error {
	if a.registry == nil {
		return fmt.Errorf("discussion registry unavailable")
	}
	return a.registry.Create(def)
}

// UpdateDiscussion replaces an existing persisted discussion definition.
func (a *App) UpdateDiscussion(prevName, prevScope string, def store.DiscussionDefinition) error {
	if a.registry == nil {
		return fmt.Errorf("discussion registry unavailable")
	}
	return a.registry.Update(prevName, prevScope, def)
}

// DeleteDiscussion removes a persisted discussion definition.
func (a *App) DeleteDiscussion(name, scope string) error {
	if a.registry == nil {
		return fmt.Errorf("discussion registry unavailable")
	}
	return a.registry.Delete(name, scope)
}

// StartDiscussion creates a deliberation channel and marks the thread as operating in discussion mode.
func (a *App) StartDiscussion(threadID, discussionName string) error {
	return a.startDiscussion(threadID, discussionName)
}

// GetChannelMessages returns ordered messages for a discussion channel.
func (a *App) GetChannelMessages(channelID string, afterSeq, limit int) ([]store.ChannelMessage, error) {
	if a.channels == nil {
		return nil, fmt.Errorf("channel service unavailable")
	}
	return a.channels.GetMessages(channelID, afterSeq, limit)
}

// PostChannelMessage posts a human-authored intervention into the channel.
func (a *App) PostChannelMessage(channelID, content string) error {
	if a.channels == nil {
		return fmt.Errorf("channel service unavailable")
	}
	_, err := a.channels.PostMessage(discussion.PostMessageInput{
		ChannelID: channelID,
		FromType:  "human",
		FromID:    "user",
		FromRole:  "human",
		Content:   content,
	})
	return err
}

func (a *App) resolveDiscussionDefinition(thread store.Thread, discussionName string) (store.DiscussionDefinition, error) {
	discussionName = strings.TrimSpace(discussionName)
	if discussionName == "" {
		return store.DiscussionDefinition{}, fmt.Errorf("discussion name is required")
	}
	// Resolve the thread's project path via the project id so we can look up
	// project-scoped discussion definitions. ProjectID is required on every
	// thread post-v13; a missing project row is a fatal error rather than
	// silent fallback to global.
	if thread.ProjectID != "" {
		project, err := a.store.GetProject(thread.ProjectID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return store.DiscussionDefinition{}, err
		}
		if err == nil && project.Path != "" {
			def, err := a.store.GetDiscussionDef(discussionName, "project", project.Path)
			if err == nil {
				return def, nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return store.DiscussionDefinition{}, err
			}
		}
	}
	if a.registry == nil {
		return store.DiscussionDefinition{}, fmt.Errorf("discussion registry unavailable")
	}
	return a.registry.Get(discussionName, "global")
}

