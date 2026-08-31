package app

import (
	"context"

	"agent-overflow/internal/discussion"
	"agent-overflow/internal/discussionapp"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
)

// ListDiscussions returns persisted discussion definitions for the given scope.
func (a *App) ListDiscussions(scope string) ([]store.DiscussionDefinition, error) {
	return a.discussionService().List(scope)
}

func (a *App) ListDiscussionsForThread(threadID string) ([]store.DiscussionDefinition, error) {
	return a.discussionService().ListForThread(threadID)
}

// GetDiscussion returns a persisted discussion definition by name and scope.
func (a *App) GetDiscussion(name, scope string) (store.DiscussionDefinition, error) {
	return a.discussionService().Get(name, scope)
}

// CreateDiscussion validates and persists a discussion definition.
func (a *App) CreateDiscussion(def store.DiscussionDefinition) error {
	return a.discussionService().Create(def)
}

// UpdateDiscussion replaces an existing persisted discussion definition.
func (a *App) UpdateDiscussion(prevName, prevScope string, def store.DiscussionDefinition) error {
	return a.discussionService().Update(prevName, prevScope, def)
}

// DeleteDiscussion removes a persisted discussion definition.
func (a *App) DeleteDiscussion(name, scope string) error {
	return a.discussionService().Delete(name, scope)
}

// StartDiscussion creates a deliberation channel and marks the thread as operating in discussion mode.
func (a *App) StartDiscussion(threadID, discussionName string) error {
	return a.discussionService().Start(threadID, discussionName)
}

// GetChannelMessages returns ordered messages for a discussion channel.
func (a *App) GetChannelMessages(channelID string, afterSeq, limit int) ([]store.ChannelMessage, error) {
	return a.discussionService().GetMessages(channelID, afterSeq, limit)
}

// GetChannelState returns a snapshot of the deliberation FSM for a
// discussion channel: status, turn/max-turn counts, whether a
// participant turn is currently in flight, and the participant roster
// with role/provider/model. Rebuilds the FSM from SQLite when the
// process restarted since the channel was opened (deliberationForChannel).
// Read-only — same LAN-safety class as GetChannelMessages.
func (a *App) GetChannelState(channelID string) (ChannelStatePayload, error) {
	state, err := a.discussionService().ChannelState(channelID)
	return projectChannelState(state), err
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
	return a.discussionService().Post(channelID, content)
}

// ConcludeDiscussion lets the human moderator end an open discussion
// immediately — independent of the MaxTurns circuit breaker and
// unanimous CONCLUDE-marker proposals (internal/discussion/conclusion.go),
// this is the third way a discussion ends. Deliberately does NOT resolve
// or rebuild the deliberation FSM: the moderator-stop message
// (discussion.ConclusionModerator) carries no proposals/roster summary,
// so there's nothing the FSM would contribute — reconstructing one just
// to discard it would only add failure modes (a rebuild error blocking
// a stop the human explicitly asked for).
//
// This does NOT interrupt an in-flight participant turn: the provider
// session that's already mid-turn finishes normally. Its late reply
// mirror into the channel is then dropped as a benign no-op once the
// channel is no longer open (syncDiscussionTurn's ErrChannelNotOpen
// handling in app_discussion_runtime.go) — the reply stays visible only
// in that participant's own child thread.
func (a *App) ConcludeDiscussion(channelID string) (ChannelStatePayload, error) {
	state, err := a.discussionService().Conclude(channelID)
	return projectChannelState(state), err
}

func (a *App) discussionService() *discussionapp.Service {
	a.discussionAppOnce.Do(func() {
		a.discussionApp = discussionapp.New(discussionapp.Config{
			Store:   func() *store.Store { return a.store },
			Runtime: appDiscussionRuntime{app: a},
			Events:  appDiscussionEvents{app: a},
		})
	})
	return a.discussionApp
}

type appDiscussionRuntime struct{ app *App }

func (r appDiscussionRuntime) StartParticipant(ctx context.Context, threadID, systemPrompt string) error {
	r.app.setThreadSystemPrompt(threadID, systemPrompt)
	return r.app.startSession(ctx, threadID)
}

func (r appDiscussionRuntime) StopParticipant(threadID string) error {
	return r.app.stopSession(threadID)
}

func (r appDiscussionRuntime) ClearParticipantPrompt(threadID string) {
	r.app.clearThreadSystemPrompt(threadID)
}

func (r appDiscussionRuntime) SendParticipantMessage(threadID, prompt string) error {
	return r.app.sendMessage(threadID, prompt, nil)
}

// The following unbound helpers keep root integration tests at the real App
// boundary while all state remains owned and locked by discussionapp.Service.
func (a *App) syncDiscussionTurn(threadID string) error {
	return a.discussionService().SyncTurn(threadID)
}

func (a *App) installDeliberation(channelID string, participants []string, maxTurns int) {
	a.discussionService().InstallRuntime(channelID, discussion.NewDeliberation(channelID, maxTurns, participants))
}

func (a *App) deliberation(channelID string) (*discussion.Deliberation, bool) {
	return a.discussionService().Runtime(channelID)
}

func (a *App) deliberationForChannel(channelID string) (*discussion.Deliberation, error) {
	return a.discussionService().ResolveRuntime(channelID)
}

// ChannelMessageEvent is the discussion:message wire payload, emitted
// at every app-layer post site: a human PostChannelMessage, the agent
// mirror in syncDiscussionTurn, and the conclusion system message.
// ThreadID is the PARENT thread ID (channel.ThreadID) — the frontend's
// DiscussionView is keyed by the parent thread the channel hangs off
// of, not any one participant's child thread id.
type ChannelMessageEvent struct {
	ChannelID string               `json:"channelId"`
	ThreadID  string               `json:"threadId"`
	Message   store.ChannelMessage `json:"message"`
}

// ChannelParticipantState is one entry in ChannelStatePayload's
// Participants list.
type ChannelParticipantState struct {
	ThreadID string `json:"threadId"`
	Role     string `json:"role"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// ProposedConclusion is true when this participant's latest channel
	// post carried a CONCLUDE marker (discussion.ParseConclusionProposal)
	// — i.e. it has a live entry in the FSM's ConclusionProposals map.
	// Only meaningful on the live-FSM branch of buildChannelState; the
	// SQLite-fallback branch (concluded/non-open channels, where
	// ConclusionProposals no longer exists) always reports false.
	ProposedConclusion bool `json:"proposedConclusion"`
}

// ChannelStatePayload is the discussion:state wire payload and the
// GetChannelState binding's return shape — a snapshot of the
// deliberation FSM plus enough participant metadata for the frontend
// to render "whose turn is it" without a second round-trip.
type ChannelStatePayload struct {
	ChannelID              string                    `json:"channelId"`
	ThreadID               string                    `json:"threadId"`
	Status                 string                    `json:"status"`
	TurnCount              int                       `json:"turnCount"`
	MaxTurns               int                       `json:"maxTurns"`
	AwaitingResponse       bool                      `json:"awaitingResponse"`
	CurrentSpeakerThreadID string                    `json:"currentSpeakerThreadId"`
	CurrentSpeakerRole     string                    `json:"currentSpeakerRole"`
	Participants           []ChannelParticipantState `json:"participants"`
}

type appDiscussionEvents struct{ app *App }

func (e appDiscussionEvents) Message(event discussionapp.MessageEvent) {
	e.app.emit(eventchan.DiscussionMessage, ChannelMessageEvent{
		ChannelID: event.ChannelID,
		ThreadID:  event.ThreadID,
		Message:   event.Message,
	})
}

func (e appDiscussionEvents) State(state discussionapp.State) {
	e.app.emit(eventchan.DiscussionState, projectChannelState(state))
}

func (e appDiscussionEvents) Error(threadID, message string) {
	e.app.emitWireErrorToThread(threadID, message)
}

func projectChannelState(state discussionapp.State) ChannelStatePayload {
	participants := make([]ChannelParticipantState, 0, len(state.Participants))
	for _, participant := range state.Participants {
		participants = append(participants, ChannelParticipantState{
			ThreadID:           participant.ThreadID,
			Role:               participant.Role,
			Provider:           participant.Provider,
			Model:              participant.Model,
			ProposedConclusion: participant.ProposedConclusion,
		})
	}
	return ChannelStatePayload{
		ChannelID:              state.ChannelID,
		ThreadID:               state.ThreadID,
		Status:                 state.Status,
		TurnCount:              state.TurnCount,
		MaxTurns:               state.MaxTurns,
		AwaitingResponse:       state.AwaitingResponse,
		CurrentSpeakerThreadID: state.CurrentSpeakerThreadID,
		CurrentSpeakerRole:     state.CurrentSpeakerRole,
		Participants:           participants,
	}
}
