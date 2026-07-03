package main

import (
	"fmt"
	"log"

	"agent-overflow/internal/discussion"
	"agent-overflow/internal/store"
)

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

// emitDiscussionMessage publishes discussion:message for a single
// channel post. Looks up the channel to resolve the parent ThreadID —
// callers only have the channel id in hand at every post site. Logs
// and no-ops on resolution failure: the post itself already committed,
// and a missed live-update is recoverable — the frontend re-fetches
// via GetChannelMessages on its transport gap-recovery path.
func (a *App) emitDiscussionMessage(channelID string, msg store.ChannelMessage) {
	if a.store == nil {
		return
	}
	channel, err := a.store.GetChannel(channelID)
	if err != nil {
		log.Printf("discussion: resolve channel for message emit %s: %v", channelID, err)
		return
	}
	a.emit("discussion:message", ChannelMessageEvent{
		ChannelID: channel.ID,
		ThreadID:  channel.ThreadID,
		Message:   msg,
	})
}

// emitDiscussionState resolves buildChannelState and publishes
// discussion:state. Best-effort: a resolution failure is logged and
// dropped rather than propagated, since every caller already completed
// the mutation (post, turn advance, conclusion) that triggered the
// emission — a missed live-update is not a reason to fail that
// mutation.
func (a *App) emitDiscussionState(channelID string) {
	payload, err := a.buildChannelState(channelID)
	if err != nil {
		log.Printf("discussion: build state for emit %s: %v", channelID, err)
		return
	}
	a.emit("discussion:state", payload)
}

// buildChannelState is the single projector behind both the
// GetChannelState binding and every discussion:state emission, so a
// poll-on-demand caller and a push subscriber always see the same
// shape.
//
// The common case resolves (and, after a restart, rebuilds) the live
// FSM via deliberationForChannel. A channel with no rebuildable FSM —
// concluded/closed, or simply not a "deliberation" channel — still
// needs a coherent read (e.g. the frontend opening a concluded
// discussion after an app restart), so that case recomputes just the
// turn count and roster directly from SQLite instead of erroring out.
func (a *App) buildChannelState(channelID string) (ChannelStatePayload, error) {
	if a.store == nil {
		return ChannelStatePayload{}, fmt.Errorf("discussion store unavailable")
	}
	channel, err := a.store.GetChannel(channelID)
	if err != nil {
		return ChannelStatePayload{}, err
	}

	payload := ChannelStatePayload{
		ChannelID: channel.ID,
		ThreadID:  channel.ThreadID,
		Status:    channel.Status,
		MaxTurns:  channel.MaxTurns,
	}

	// One ListChildThreads query serves the participant metadata for
	// both branches below — discussion:state fires on every turn
	// advance, so a per-participant GetThread fan-out here would be an
	// N+1 on a hot-ish path.
	roster, err := a.discussionRoster(channel)
	if err != nil {
		return ChannelStatePayload{}, err
	}

	participantRows := roster
	// conclusionProposals stays nil on the SQLite-fallback branch, so
	// every participant's ProposedConclusion below reports false — a
	// concluded/closed channel's FSM (and its ConclusionProposals map)
	// no longer exists to ask.
	var conclusionProposals map[string]string
	if d, dErr := a.deliberationForChannel(channelID); dErr == nil {
		state := d.State()
		payload.TurnCount = state.TurnCount
		payload.MaxTurns = state.MaxTurns
		payload.AwaitingResponse = state.AwaitingResponse
		payload.CurrentSpeakerThreadID = state.CurrentSpeaker
		conclusionProposals = state.ConclusionProposals
		// The live FSM's roster order is authoritative — order (and
		// filter) the fetched rows by it. The two orders only diverge
		// when the FSM lazily learned a poster that isn't a linked
		// child thread (Deliberation.rememberParticipant); such an ID
		// has no thread row to describe, so it is skipped.
		rowsByID := make(map[string]store.Thread, len(roster))
		for _, child := range roster {
			rowsByID[child.ID] = child
		}
		fsmOrder := d.Participants()
		participantRows = make([]store.Thread, 0, len(fsmOrder))
		for _, threadID := range fsmOrder {
			if child, ok := rowsByID[threadID]; ok {
				participantRows = append(participantRows, child)
			}
		}
	} else {
		turnCount, countErr := a.store.CountChannelMessagesByType(channelID, discussion.FromTypeAgent)
		if countErr != nil {
			return ChannelStatePayload{}, countErr
		}
		payload.TurnCount = turnCount
	}

	participants := make([]ChannelParticipantState, 0, len(participantRows))
	for _, child := range participantRows {
		role := discussion.RoleFromThreadTitle(child.Title)
		_, proposedConclusion := conclusionProposals[child.ID]
		participants = append(participants, ChannelParticipantState{
			ThreadID:           child.ID,
			Role:               role,
			Provider:           child.Provider,
			Model:              child.Model,
			ProposedConclusion: proposedConclusion,
		})
		if child.ID == payload.CurrentSpeakerThreadID {
			payload.CurrentSpeakerRole = role
		}
	}
	payload.Participants = participants

	return payload, nil
}
