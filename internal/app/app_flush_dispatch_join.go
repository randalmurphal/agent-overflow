package app

import (
	"agent-overflow/internal/agentmeta"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
)

// joinedFlushSeparator is the visible rule between the parts of a JOINED
// queued message, shared with the triage-side fold of a merge the CLI made
// across separate drains so the two produce the same row.
const joinedFlushSeparator = agentmeta.JoinSeparator

// flushMember is one queued item resolved up to the last fallible step before
// anything durable happens. A joined group resolves EVERY member first, so an
// envelope failure on the third message still leaves all three requeueable and
// nothing on the wire.
type flushMember struct {
	item     triage.QueuedFlushItem
	payload  flushQueuePayload
	resolved resolvedUserMessage
}

// joinedFlushMessage is the ONE outbound message a flush group becomes: one
// stdin envelope, one uuid, one `user_text` row, one message anchor, one
// response turn. For a single-member group every field is that member's own
// resolved value, so the join is a no-op on the common path.
type joinedFlushMessage struct {
	// content is the row summary: the members' stored texts (what the user
	// typed, without composer-command expansion) joined by the separator.
	content string
	// providerContent is what reaches the wire: the members' provider-bound
	// texts joined by the same separator.
	providerContent string
	// attachments is every member's images in queue order. BOTH texts above
	// carry inline `[Image #N]` markers renumbered into this order
	// (joinRenumberedText), so the bubble, an edit-resend and the wire all
	// bind the same marker to the same image.
	attachments []provider.ImageAttachment
	meta        string
	// sendID is the first member's client-minted id. It names the row (the
	// opaque id every member's acknowledgement resolves to) and stays on
	// `$.sendId`; the rest ride Meta.JoinedSendIDs.
	sendID string
	// parts are the members as they appear in content, which a session-death
	// restore splits the message by (agentmeta.JoinedPart); nil for one
	// message, or when a member is a joined message whose parts are unknown.
	parts []agentmeta.JoinedPart
	// guardSlashCommand comes from the FIRST member alone: Claude's command
	// router tests the first character of the assembled message, so only the
	// leading member's text can enter it.
	guardSlashCommand bool
}

// groupFlushDispatch splits one drained batch into the groups the dispatcher
// delivers as single provider messages. Every group is dispatched in order and
// settles as a unit.
func (a *App) groupFlushDispatch(threadID string, items []triage.QueuedFlushItem) [][]triage.QueuedFlushItem {
	if len(items) > 1 && a.flushDispatchJoinsMessages(threadID) {
		return [][]triage.QueuedFlushItem{items}
	}
	groups := make([][]triage.QueuedFlushItem, 0, len(items))
	for i := range items {
		groups = append(groups, items[i:i+1])
	}
	return groups
}

// flushDispatchJoinsMessages reports whether this thread's live provider
// collapses several consecutively queued messages into ONE message. Joining is
// not a product choice: it is AO recording what the provider actually did.
//
//   - Headless Claude DOES. When its command queue drains at a turn boundary
//     it batches consecutive prompt-mode commands into one `ask()` whose
//     transcript entry carries the concatenated content and only the LAST
//     command's uuid; the other uuids are acknowledged on stdout and never
//     written to the session file (claude-wire.md §Queued-message consumption,
//     boundary-drain merge). One AO row per member would leave rows whose
//     uuid no transcript contains, which revert cannot slice at.
//   - Codex does NOT. Each queued item is its own `turn/steer` pending_input
//     item and its cuts are turn-granular.
//   - claude-tui does NOT. The interactive REPL's queue processor drains the
//     batch into one turn but each command stays "its own user message with
//     its own UUID" (src/utils/queueProcessor.ts; executeUserInput loops
//     processUserInput per command), so AO's per-item rows already match the
//     transcript one for one.
func (a *App) flushDispatchJoinsMessages(threadID string) bool {
	sess, ok := a.sessionManager().get(threadID)
	return ok && sess.Claude != nil
}

// joinFlushMembers folds a resolved group into the single message the
// dispatcher sends and persists. Members must be non-empty.
func joinFlushMembers(members []flushMember) (joinedFlushMessage, error) {
	first := members[0]
	joined := joinedFlushMessage{
		sendID:            first.payload.SendID,
		guardSlashCommand: !first.payload.ExpandComposerCommands || first.resolved.command != "",
	}
	// One member that is not itself joined is its own message already: reuse
	// the meta resolveUserMessageEnvelope built. A member that inherited a
	// joined row's ids and parts (session-death requeue) still goes through
	// joinFlushMeta so they survive the re-dispatch.
	if len(members) == 1 && len(first.payload.JoinedSendIDs) == 0 && len(first.payload.JoinedParts) == 0 {
		joined.content = first.resolved.content
		joined.providerContent = first.resolved.providerContent
		joined.attachments = first.resolved.providerAttachments
		joined.meta = first.resolved.userMessageMeta
		return joined, nil
	}

	// BOTH texts are renumbered by the same walk, the summary's inside
	// joinFlushMeta with the parts it measures. The stored summary is not
	// display-only: the bubble renders its markers beside the row's attachment
	// strip, and an edit-resend or a session-death requeue re-dispatches that
	// exact string against the joined attachment list.
	joined.providerContent = joinRenumberedText(members, func(member flushMember) string {
		return member.resolved.providerContent
	})
	joined.attachments = joinProviderAttachments(members)

	content, meta, parts, sendID, err := joinFlushMeta(members)
	if err != nil {
		return joinedFlushMessage{}, err
	}
	joined.content = content
	joined.meta = meta
	joined.parts = parts
	joined.sendID = sendID
	return joined, nil
}

// joinRenumberedText concatenates one text per member and renumbers its inline
// image markers into the joined attachment order (usermessage.JoinRenumberedText).
// `pick` selects which of the member's two texts to walk: the stored summary or
// the provider-bound text. They carry identical markers and differ only in what
// surrounds them (command expansion, file attachment lines), so one walk serves
// both and the two cannot drift apart.
func joinRenumberedText(members []flushMember, pick func(flushMember) string) string {
	parts := make([]usermessage.TextPart, 0, len(members))
	for _, member := range members {
		parts = append(parts, usermessage.TextPart{
			Text:       pick(member),
			ImageCount: len(member.resolved.providerAttachments),
		})
	}
	return usermessage.JoinRenumberedText(parts)
}

// joinProviderAttachments is the image list those renumbered markers index
// into: every member's images in queue order, the same order joinFlushMeta
// keeps the persisted records in.
func joinProviderAttachments(members []flushMember) []provider.ImageAttachment {
	var attachments []provider.ImageAttachment
	for _, member := range members {
		attachments = append(attachments, member.resolved.providerAttachments...)
	}
	return attachments
}

// joinFlushMeta joins the members' stored texts and row metadata
// (usermessage.Join) and returns the joined summary, its encoded meta, the
// joined message's parts, which the meta records too, and the send id that
// names the joined row.
func joinFlushMeta(members []flushMember) (string, string, []agentmeta.JoinedPart, string, error) {
	joinMembers := make([]usermessage.JoinMember, 0, len(members))
	for _, member := range members {
		resolved := member.resolved
		meta := usermessage.Input{
			Attachments:            resolved.persistedAttachments,
			SourcePlan:             resolved.sourcePlan,
			RevisionSourcePlan:     resolved.revisionSourcePlan,
			RevisionCommentIDs:     resolved.revisionPlanCommentIDs,
			RevisionSourceDiff:     resolved.revisionSourceDiff,
			RevisionDiffCommentIDs: resolved.revisionDiffCommentIDs,
			Command:                resolved.command,
			ExpandComposerCommands: member.payload.ExpandComposerCommands,
			SendID:                 member.payload.SendID,
			JoinedSendIDs:          member.payload.JoinedSendIDs,
			// Attribution joins with the rest: JoinMetas keeps the first
			// member's, so a wake or an agent's message that waited on the
			// queue keeps its chip when it is joined with others.
			Origin:       member.payload.Origin,
			OriginThread: member.payload.OriginThread,
			OriginWake:   member.payload.OriginWake,
		}.Projection()
		meta.JoinedParts = member.payload.JoinedParts
		joinMembers = append(joinMembers, usermessage.JoinMember{
			Meta: meta, Text: resolved.content, ImageCount: len(resolved.providerAttachments),
		})
	}
	content, joined := usermessage.Join(joinMembers)
	// The joined text without the review comments any member rendered,
	// renumbered by the same walk as the summary.
	for _, member := range members {
		if member.resolved.textBeforeReviewComments != nil {
			text := joinRenumberedText(members, func(member flushMember) string {
				if member.resolved.textBeforeReviewComments != nil {
					return *member.resolved.textBeforeReviewComments
				}
				return member.resolved.content
			})
			joined.TextBeforeReviewComments = &text
			break
		}
	}
	meta, err := usermessage.MarshalMeta(joined)
	if err != nil {
		return "", "", nil, "", err
	}
	return content, meta, joined.JoinedParts, joined.SendID, nil
}
