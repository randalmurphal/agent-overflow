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

func (a *App) ListDiscussionsForThread(threadID string) ([]store.DiscussionDefinition, error) {
	if a.store == nil {
		return nil, fmt.Errorf("discussion store unavailable")
	}
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return nil, err
	}

	var defs []store.DiscussionDefinition
	projectPath, err := a.projectPathForThread(thread)
	if err == nil && projectPath != "" {
		projectDefs, err := a.store.ListDiscussionDefs("project", projectPath)
		if err != nil {
			return nil, err
		}
		defs = append(defs, projectDefs...)
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	globalDefs, err := a.store.ListDiscussionDefs("global", "")
	if err != nil {
		return nil, err
	}
	defs = append(defs, globalDefs...)
	return defs, nil
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

// GetChannelState returns a snapshot of the deliberation FSM for a
// discussion channel: status, turn/max-turn counts, whether a
// participant turn is currently in flight, and the participant roster
// with role/provider/model. Rebuilds the FSM from SQLite when the
// process restarted since the channel was opened (deliberationForChannel).
// Read-only — same LAN-safety class as GetChannelMessages.
func (a *App) GetChannelState(channelID string) (ChannelStatePayload, error) {
	return a.buildChannelState(channelID)
}

// PostChannelMessage posts a human-authored intervention into the
// channel and returns the created message so the frontend can merge
// its own post immediately rather than waiting on the discussion:message
// echo. If the deliberation isn't already mid-turn (no participant is
// currently being awaited), this also kicks off the next participant's
// turn: a human posting into a fresh discussion — or one where the
// last participant already answered — is what drives the conversation
// forward. A human interjecting WHILE a participant turn is in flight
// does not re-prompt; the interjection lands in that participant's
// next-turn context automatically (see promptDiscussionSpeaker's
// unseen-messages window).
//
// PostChannelMessage now has a side-effecting path (it can dispatch a
// prompt into a participant's live provider session via
// promptDiscussionSpeakerAsync → sendMessage), so it is classified
// LocalOnly in internal/transport/internalmethods.go alongside
// SendMessage — see the category-2 comment there.
func (a *App) PostChannelMessage(channelID, content string) (store.ChannelMessage, error) {
	if a.channels == nil {
		return store.ChannelMessage{}, fmt.Errorf("channel service unavailable")
	}
	msg, err := a.channels.PostMessage(discussion.PostMessageInput{
		ChannelID: channelID,
		FromType:  "human",
		FromID:    "user",
		FromRole:  "human",
		Content:   content,
	})
	if err != nil {
		return store.ChannelMessage{}, err
	}
	a.emitDiscussionMessage(channelID, msg)
	a.maybePromptNextDiscussionSpeaker(channelID)
	return msg, nil
}

// maybePromptNextDiscussionSpeaker prompts the current speaker after a
// human post lands, unless the channel is closed/concluded or a
// participant turn is already in flight (AwaitingResponse).
// Resolution failures (channel isn't open, or isn't a
// live/rebuildable deliberation) are not surfaced as errors here:
// PostChannelMessage's own persistence already succeeded, and a
// channel with nothing left to coordinate simply has nothing further
// to do. The claim itself goes through claimAndPromptNextSpeaker so a
// human post racing a participant's in-flight turn-complete can't
// double-prompt the same speaker.
func (a *App) maybePromptNextDiscussionSpeaker(channelID string) {
	channel, err := a.store.GetChannel(channelID)
	if err != nil || channel.Status != "open" {
		return
	}
	deliberation, err := a.deliberationForChannel(channelID)
	if err != nil {
		return
	}
	a.claimAndPromptNextSpeaker(channelID, deliberation)
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
