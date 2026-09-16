package app

import (
	"slices"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
)

// joinedFlushSeparator is the visible rule between the parts of a JOINED
// queued message. It goes in BOTH the stored row summary and the text written
// to the provider, so the bubble the user reads back, the text the model
// receives, and the message the revert cuts at are one thing.
//
// Claude Code's own boundary-drain merge newline-joins the parts
// (`joinPromptValues`, src/cli/print.ts). AO uses a markdown rule instead
// because its copy is also the row a person reads, edits and reverts to, and
// two prompts run together across a bare newline read as one paragraph.
const joinedFlushSeparator = "\n\n---\n\n"

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
	// One member with no inherited ids is its own message already: reuse the
	// meta resolveUserMessageEnvelope built. A member that inherited a joined
	// row's ids (session-death requeue) still goes through joinFlushMeta so
	// those ids survive the re-dispatch.
	if len(members) == 1 && len(first.payload.JoinedSendIDs) == 0 {
		joined.content = first.resolved.content
		joined.providerContent = first.resolved.providerContent
		joined.attachments = first.resolved.providerAttachments
		joined.meta = first.resolved.userMessageMeta
		return joined, nil
	}

	// BOTH texts are renumbered by the same walk. The stored summary is not
	// display-only: the bubble renders its markers beside the row's attachment
	// strip, and an edit-resend or a session-death requeue re-dispatches that
	// exact string against the joined attachment list.
	joined.content = joinRenumberedText(members, func(member flushMember) string {
		return member.resolved.content
	})
	joined.providerContent = joinRenumberedText(members, func(member flushMember) string {
		return member.resolved.providerContent
	})
	joined.attachments = joinProviderAttachments(members)

	meta, sendID, err := joinFlushMeta(members)
	if err != nil {
		return joinedFlushMessage{}, err
	}
	joined.meta = meta
	joined.sendID = sendID
	return joined, nil
}

// joinRenumberedText concatenates one text per member and RENUMBERS its inline
// image markers into the joined attachment order. `pick` selects which of the
// member's two texts to walk: the stored summary or the provider-bound text.
// They carry identical markers and differ only in what surrounds them (command
// expansion, file attachment lines), so one walk serves both and the two cannot
// drift apart.
//
// The composer numbers `[Image #N]` per message, so a plain concatenation
// leaves two `#1` markers and an image with no marker at all: a reader's split
// claims the first unused occurrence per index, binds attachment 1 to the first
// member's marker, finds no `#2`, and appends the second member's image at the
// end while its literal `#1` stays in the text. Re-emitting each marker from
// the member's attachment offset keeps every image where the user dropped it.
func joinRenumberedText(members []flushMember, pick func(flushMember) string) string {
	var text strings.Builder
	base := 0
	for i, member := range members {
		if i > 0 {
			text.WriteString(joinedFlushSeparator)
		}
		images := member.resolved.providerAttachments
		for _, part := range provider.SplitContentByImageMarkers(pick(member), len(images)) {
			if part.ImageIndex >= 0 {
				text.WriteString(provider.ImagePlaceholderLabel(base + part.ImageIndex + 1))
				continue
			}
			text.WriteString(part.Text)
		}
		base += len(images)
	}
	return text.String()
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

// joinFlushMeta unions the members' row metadata and returns it with the send
// id that names the joined row.
//
// Attachments concatenate in queue order, matching the renumbered markers.
// The singular references (source plan, revision plan, diff review) take the
// first member that carries one, and comment ids are collected only from the
// members pointing at THAT reference: the badges a row renders are scoped to
// its own reference, so folding in another plan's comment ids would label the
// row with comments it does not show. Every member's plan/comment bookkeeping
// is still applied separately by applyProposedPlanAcceptance.
func joinFlushMeta(members []flushMember) (string, string, error) {
	var in usermessage.Input
	for _, member := range members {
		resolved := member.resolved
		in.Attachments = append(in.Attachments, resolved.persistedAttachments...)
		if in.SourcePlan == nil {
			in.SourcePlan = resolved.sourcePlan
		}
		if resolved.revisionSourcePlan != nil {
			if in.RevisionSourcePlan == nil {
				in.RevisionSourcePlan = resolved.revisionSourcePlan
			}
			if samePlanRef(in.RevisionSourcePlan, resolved.revisionSourcePlan) {
				in.RevisionCommentIDs = append(in.RevisionCommentIDs, resolved.revisionPlanCommentIDs...)
			}
		}
		if resolved.revisionSourceDiff != nil {
			if in.RevisionSourceDiff == nil {
				in.RevisionSourceDiff = resolved.revisionSourceDiff
			}
			if sameDiffRef(in.RevisionSourceDiff, resolved.revisionSourceDiff) {
				in.RevisionDiffCommentIDs = append(in.RevisionDiffCommentIDs, resolved.revisionDiffCommentIDs...)
			}
		}
		if in.Command == "" {
			in.Command = resolved.command
		}
		// A crash-rebuilt row must keep the composer's slash semantics for any
		// member that came through the composer.
		in.ExpandComposerCommands = in.ExpandComposerCommands || member.payload.ExpandComposerCommands
		// A member requeued from a joined row answers for every id that row
		// answered for, not just its own.
		ids := member.payload.JoinedSendIDs
		if len(ids) == 0 {
			ids = []string{member.payload.SendID}
		}
		for _, id := range ids {
			if id == "" {
				continue
			}
			if in.SendID == "" {
				in.SendID = id
			}
			if !slices.Contains(in.JoinedSendIDs, id) {
				in.JoinedSendIDs = append(in.JoinedSendIDs, id)
			}
		}
	}
	meta, err := usermessage.Marshal(in)
	if err != nil {
		return "", "", err
	}
	return meta, in.SendID, nil
}

func samePlanRef(a, b *store.ProposedPlanSourceRef) bool {
	return a != nil && b != nil && a.ThreadID == b.ThreadID && a.ItemID == b.ItemID
}

func sameDiffRef(a, b *SourceDiffReview) bool {
	return a != nil && b != nil && a.Scope == b.Scope && a.SourceKey == b.SourceKey
}
