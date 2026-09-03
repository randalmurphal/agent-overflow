package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	attachmentstore "agent-overflow/internal/attachment"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/planrevision"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/workflowhost"

	"github.com/google/uuid"
)

// Local aliases so the rest of main can keep using the short names.
type userMessageMeta = usermessage.Meta
type userMessageAttachmentMeta = usermessage.AttachmentMeta

// SourceProposedPlan records that a user follow-up is acting on a specific
// immutable proposed-plan item. It is traceability metadata, not prompt text.
type SourceProposedPlan = store.ProposedPlanSourceRef
type SourceDiffReview = store.DiffReviewSourceRef

type sendMessageOptions struct {
	AttachmentIDs                []string
	RuntimeMode                  string
	SourceProposedPlan           *SourceProposedPlan
	RevisionSourceProposedPlan   *SourceProposedPlan
	RevisionSourceCommentIDs     []string
	RevisionSourceDiffReview     *SourceDiffReview
	RevisionSourceDiffCommentIDs []string
	OutputSchema                 json.RawMessage
	// PreserveDraft keeps the thread's durable composer draft. Set by the
	// app-internal injectors (the workflow wake) whose text did not come from
	// the composer: a user send consumes the draft, but a system-injected
	// message that cleared it would silently destroy text the user typed and
	// has not sent.
	PreserveDraft bool
	// ExpandComposerCommands admits a leading `/command` word for send-time
	// expansion (D31). Explicit opt-in, set ONLY where text a human typed
	// into a composer enters the app: the bound SendMessage /
	// SendMessageWithOptions / SteerMessageWithOptions wire methods and the
	// flush-queue dispatch of previously queued composer text. Every
	// app-internal caller (workflow wake, seeded triage/PR turns,
	// schema-driven phase sends, discussion drive) leaves it false by
	// default, so a new internal send path can never expand a `/…` opener
	// in a prompt the app itself wrote just by forgetting a flag.
	ExpandComposerCommands bool
	// onProviderDispatch runs under the provider-account read lock immediately
	// before the provider write. It exists so an observer can attribute an error
	// emitted as soon as stdin is written to the exact account generation that
	// sent the turn; reading the session after Send returns is already too late.
	onProviderDispatch func(workflowhost.DispatchIdentity)
	// onCodexReviewStarted observes the review/start acknowledgement for the
	// legacy structured binding. Composer sends leave it nil.
	onCodexReviewStarted func(codex.ReviewStarted)
}

// userMessageInputs is the projection of fields shared by every
// user-message entry point (send, steer, flush). The shape is
// sendMessageOptions minus RuntimeMode and matches flushQueuePayload's
// data fields — by the time the flush trigger fires, the round's
// runtime mode is already established so it never carries one.
type userMessageInputs struct {
	attachmentIDs                []string
	sourceProposedPlan           *SourceProposedPlan
	revisionSourceProposedPlan   *SourceProposedPlan
	revisionSourceCommentIDs     []string
	revisionSourceDiffReview     *SourceDiffReview
	revisionSourceDiffCommentIDs []string
	// expandComposerCommands admits a leading `/command` word for
	// send-time expansion (D31). True for everything a human typed into a
	// composer — direct send, Codex steer, queued flush — and false for
	// app-internal injectors (the workflow wake, schema-driven workflow
	// sends), whose text did not come from a composer and must reach the
	// provider byte-for-byte as composed.
	expandComposerCommands bool
}

// resolvedUserMessage bundles everything resolveUserMessageEnvelope
// produces: the (possibly comment-appended) content, the loaded
// attachments in both provider and store shape, the validated
// plan/diff references and their comment id lists, and the marshaled
// userMessageMeta the caller writes into store.Item.Meta.
type resolvedUserMessage struct {
	content string
	// providerContent is what goes on the wire. It equals content except
	// when a composer command expanded (D31), where it carries the typed
	// text followed by the command's context block. Every entry point
	// persists `content` and sends `providerContent` — the pair exists so
	// a send path cannot accidentally store the block or ship the message
	// without it.
	providerContent string
	// command is the composer command that expanded, without its slash
	// ("workflow"), or "" when the message invoked none. Recorded in the
	// stored item's meta.
	command                string
	providerAttachments    []provider.ImageAttachment
	persistedAttachments   []store.Attachment
	sourcePlan             *SourceProposedPlan
	revisionSourcePlan     *SourceProposedPlan
	revisionPlanCommentIDs []string
	revisionSourceDiff     *SourceDiffReview
	revisionDiffCommentIDs []string
	userMessageMeta        string
}

// resolveUserMessageEnvelope runs the shared prologue that every
// user-message entry point performs before persisting the optimistic
// user_text row: load attachments, resolve the source/revision plan
// references, append revision comments into the content, resolve any
// diff-review revision, and marshal the userMessageMeta.
//
// The returned errors are step-prefixed but unscoped — callers wrap
// them with their entry-point prefix ("send message:", "steer message:")
// or pass them through verbatim (flush queue dispatch).
func (a *App) resolveUserMessageEnvelope(
	threadID, content string,
	inputs userMessageInputs,
) (resolvedUserMessage, error) {
	attachments, err := a.resolveSendMessageAttachments(threadID, inputs.attachmentIDs)
	if err != nil {
		return resolvedUserMessage{}, fmt.Errorf("attachments: %w", err)
	}

	sourcePlan, err := a.resolveSourceProposedPlan(threadID, inputs.sourceProposedPlan, true)
	if err != nil {
		return resolvedUserMessage{}, fmt.Errorf("source proposed plan: %w", err)
	}
	revisionSourcePlan, err := a.resolveSourceProposedPlan(threadID, inputs.revisionSourceProposedPlan, false)
	if err != nil {
		return resolvedUserMessage{}, fmt.Errorf("revision source proposed plan: %w", err)
	}
	if revisionSourcePlan == nil && len(inputs.revisionSourceCommentIDs) > 0 {
		return resolvedUserMessage{}, fmt.Errorf("revision comments require a source proposed plan")
	}
	revisionSourceDiff, err := a.resolveSourceDiffReview(threadID, inputs.revisionSourceDiffReview)
	if err != nil {
		return resolvedUserMessage{}, fmt.Errorf("revision source diff review: %w", err)
	}
	if revisionSourceDiff == nil && len(inputs.revisionSourceDiffCommentIDs) > 0 {
		return resolvedUserMessage{}, fmt.Errorf("diff review comments require a source diff review")
	}

	revisionCommentIDs := inputs.revisionSourceCommentIDs
	if revisionSourcePlan != nil && len(revisionCommentIDs) > 0 {
		nextContent, commentIDs, err := a.appendPlanRevisionCommentsToContent(threadID, content, revisionSourcePlan.ItemID, revisionCommentIDs)
		if err != nil {
			return resolvedUserMessage{}, fmt.Errorf("revision comments: %w", err)
		}
		content = nextContent
		revisionCommentIDs = commentIDs
	}
	revisionDiffCommentIDs := inputs.revisionSourceDiffCommentIDs
	if revisionSourceDiff != nil && len(revisionDiffCommentIDs) > 0 {
		nextContent, commentIDs, err := a.appendDiffReviewCommentsToContent(threadID, content, revisionSourceDiff.Scope, revisionSourceDiff.SourceKey, revisionDiffCommentIDs, revisionSourceDiff.PR)
		if err != nil {
			return resolvedUserMessage{}, fmt.Errorf("diff review comments: %w", err)
		}
		content = nextContent
		revisionDiffCommentIDs = commentIDs
	}
	if revisionSourceDiff != nil && revisionSourceDiff.PR != nil {
		// The per-comment hunk excerpts are prompt inputs already baked into
		// content above; persisting them again in the item meta would bloat
		// every PR-scope send's row. Keep only the PR identity.
		strippedPR := *revisionSourceDiff.PR
		strippedPR.Comments = nil
		strippedRef := *revisionSourceDiff
		strippedRef.PR = &strippedPR
		revisionSourceDiff = &strippedRef
	}

	// Composer command expansion (D31) runs last so the block lands after
	// everything the message already carries, and BEFORE the meta is
	// marshaled so the recognised command is recorded on the row. A
	// resolution failure aborts the whole send: the caller has not yet
	// persisted the user row, so the composer restores the draft and shows
	// the error rather than silently sending the message without the
	// context it asked for.
	providerContent := content
	command := ""
	if inputs.expandComposerCommands {
		providerContent, command, err = a.expandComposerCommand(threadID, content)
		if err != nil {
			return resolvedUserMessage{}, fmt.Errorf("composer command: %w", err)
		}
	}
	providerContent = appendFileAttachmentLines(providerContent, attachments.fileLines)

	userMeta, err := usermessage.Marshal(usermessage.Input{
		Attachments:            attachments.records,
		SourcePlan:             sourcePlan,
		RevisionSourcePlan:     revisionSourcePlan,
		RevisionCommentIDs:     revisionCommentIDs,
		RevisionSourceDiff:     revisionSourceDiff,
		RevisionDiffCommentIDs: revisionDiffCommentIDs,
		Command:                command,
		ExpandComposerCommands: inputs.expandComposerCommands,
	})
	if err != nil {
		return resolvedUserMessage{}, fmt.Errorf("user meta: %w", err)
	}

	return resolvedUserMessage{
		content:                content,
		providerContent:        providerContent,
		command:                command,
		providerAttachments:    attachments.images,
		persistedAttachments:   attachments.records,
		sourcePlan:             sourcePlan,
		revisionSourcePlan:     revisionSourcePlan,
		revisionPlanCommentIDs: revisionCommentIDs,
		revisionSourceDiff:     revisionSourceDiff,
		revisionDiffCommentIDs: revisionDiffCommentIDs,
		userMessageMeta:        userMeta,
	}, nil
}

func (a *App) sendMessage(threadID string, content string, attachmentIDs []string) error {
	_, err := a.sendMessageWithOptions(
		context.Background(), threadID, content, sendMessageOptions{AttachmentIDs: attachmentIDs},
	)
	return err
}

// sendMessagePrepared is what sendMessageWithOptions resolves BEFORE it
// takes the per-thread action lock. Neither field can be produced inside
// the critical section: an invalid runtime mode has to be rejected
// before the workflow takeover detaches a run, and the takeover
// preparation itself round-trips the engine command loop, which
// re-acquires this thread's action lock.
//
// The zero value is the correct input for a caller that already holds
// the lock and has neither concern — no runtime-mode override, no
// workflow takeover (see RevertConversationAndResendMessage, which
// rejects workflow-mode threads outright rather than reaching the
// takeover machinery from inside the lock).
type sendMessagePrepared struct {
	runtimeMode    provider.RuntimeMode
	hasRuntimeMode bool
	// takeoverItemID names the workflow item prepareWorkflowTakeoverSend
	// detached for user steering. Empty for every non-workflow send; the
	// locked tail re-verifies it once the lock is held.
	takeoverItemID string
}

// ctx bounds the per-thread action lock wait and, downstream, a lazy start's
// join on an in-flight start — the two waits that have performed no side
// effects when they are abandoned. It never reaches the provider write: a
// cancelled send would be indistinguishable from a delivered one. Interactive
// callers pass context.Background() and behave exactly as before.
func (a *App) sendMessageWithOptions(
	ctx context.Context, threadID string, content string, opts sendMessageOptions,
) (store.Item, error) {
	if a.shuttingDown.Load() {
		return store.Item{}, ErrShuttingDown
	}

	runtimeMode, hasRuntimeMode, err := threadmode.ParseOptionalRuntime(opts.RuntimeMode)
	if err != nil {
		return store.Item{}, fmt.Errorf("send message: %w", err)
	}
	prepared := sendMessagePrepared{runtimeMode: runtimeMode, hasRuntimeMode: hasRuntimeMode}

	// Workflow phase threads: a user send steers a taken-over run. The
	// preparation round-trips the engine command loop, whose takeover
	// teardown interrupts the live phase turn via InterruptTurn — and
	// InterruptTurn acquires this thread's action lock. Holding the lock
	// across that round-trip deadlocks, so preparation runs BEFORE the
	// critical section and is re-verified cheaply once the lock is held
	// (RegisterTakeover is idempotent for a live registration and fails
	// typed when the takeover raced away in between).
	if len(opts.OutputSchema) == 0 {
		peek, err := a.store.GetThread(threadID)
		if err != nil {
			return store.Item{}, fmt.Errorf("send message: load thread: %w", err)
		}
		if peek.Mode == threadmode.ModeWorkflow {
			itemID, err := a.prepareWorkflowTakeoverSend(ctx, peek)
			if err != nil {
				return store.Item{}, fmt.Errorf("send message: %w", err)
			}
			prepared.takeoverItemID = itemID
		}
	}

	// Per-thread critical section: only one Send per thread at a time.
	// This keeps the runtime-mode update, turn-index read, optimistic
	// user-item persist, lazy session start, pending-send registration, and
	// provider dispatch sequence atomic for a single thread while letting
	// different threads proceed in parallel.
	unlock, err := a.threadLocks().LockCtx(ctx, threadID)
	if err != nil {
		return store.Item{}, fmt.Errorf("send message: thread %s: %w", threadID, err)
	}
	defer unlock()

	return a.sendMessageLocked(ctx, threadID, content, opts, prepared)
}

// sendMessageLocked is the send pipe's critical section: runtime-mode
// update, envelope resolution, optimistic user-item persist, lazy
// session start, pending-send registration, and provider dispatch. The
// CALLER must already hold a.threadLocks().Lock(threadID) — it is split
// out so a saga that has to hold that lock across more than a send (the
// edit-and-resend revert) can dispatch the send inside its own critical
// section instead of dropping the lock and racing the window.
func (a *App) sendMessageLocked(
	ctx context.Context, threadID string, content string,
	opts sendMessageOptions, prepared sendMessagePrepared,
) (item store.Item, err error) {
	// The test seam lives at the narrowest waist every send path passes
	// through, not at the outer entry point: a saga that dispatches its
	// send here (RevertConversationAndResendMessage) would otherwise
	// bypass a test's stub and drive a real provider session.
	if a.sendMessageFn != nil {
		return store.Item{}, a.sendMessageFn(threadID, content, opts.AttachmentIDs)
	}

	if prepared.hasRuntimeMode {
		if err := a.applyRuntimeModeLocked(threadID, prepared.runtimeMode); err != nil {
			return store.Item{}, fmt.Errorf("send message: runtime mode: %w", err)
		}
	}

	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return store.Item{}, fmt.Errorf("send message: load thread: %w", err)
	}
	if len(opts.OutputSchema) == 0 && thread.Mode == threadmode.ModeWorkflow {
		if prepared.takeoverItemID == "" {
			return store.Item{}, fmt.Errorf("send message: workflow takeover: thread entered workflow mode mid-send; retry")
		}
		if err := a.workflowApplication().RegisterTakeover(ctx, prepared.takeoverItemID, threadID); err != nil {
			return store.Item{}, fmt.Errorf("send message: workflow takeover: %w", err)
		}
	}

	var (
		reviewTarget  codex.ReviewTarget
		isCodexReview bool
	)
	if opts.ExpandComposerCommands && thread.Provider == string(provider.Codex) {
		reviewTarget, isCodexReview, err = codexReviewCommandTarget(content)
		if err != nil {
			return store.Item{}, fmt.Errorf("send message: /review: %w", err)
		}
		if isCodexReview && (len(opts.AttachmentIDs) > 0 || opts.SourceProposedPlan != nil ||
			opts.RevisionSourceProposedPlan != nil || len(opts.RevisionSourceCommentIDs) > 0 ||
			opts.RevisionSourceDiffReview != nil || len(opts.RevisionSourceDiffCommentIDs) > 0 ||
			len(opts.OutputSchema) > 0) {
			return store.Item{}, fmt.Errorf("send message: /review cannot include attachments or review-context inputs")
		}
	}

	// implemented_at is durable UI/history state, not a send-time lock.
	// An explicit source-plan send is still valid after the plan is marked
	// accepted, including restored drafts after a conversation revert.
	resolved, err := a.resolveUserMessageEnvelope(threadID, content, userMessageInputs{
		attachmentIDs:                opts.AttachmentIDs,
		sourceProposedPlan:           opts.SourceProposedPlan,
		revisionSourceProposedPlan:   opts.RevisionSourceProposedPlan,
		revisionSourceCommentIDs:     opts.RevisionSourceCommentIDs,
		revisionSourceDiffReview:     opts.RevisionSourceDiffReview,
		revisionSourceDiffCommentIDs: opts.RevisionSourceDiffCommentIDs,
		expandComposerCommands:       opts.ExpandComposerCommands,
	})
	if err != nil {
		return store.Item{}, fmt.Errorf("send message: %w", err)
	}
	content = resolved.content
	// The wire payload and the persisted row diverge only here: a `/command`
	// send puts the command's context block on the wire (D31) and keeps the
	// typed text in the transcript, the draft-rename heuristic, and the
	// generated thread title below.
	providerContent := resolved.providerContent
	providerAttachments := resolved.providerAttachments
	persistedAttachments := resolved.persistedAttachments
	sourcePlan := resolved.sourcePlan
	userMeta := resolved.userMessageMeta
	if isCodexReview {
		userMeta, err = usermessage.MergeCommand(userMeta, "review")
		if err != nil {
			return store.Item{}, fmt.Errorf("send message: mark /review command: %w", err)
		}
	}

	thread, restoreImplementMode, err := a.beginImplementModeSwitch(thread, sourcePlan)
	if err != nil {
		return store.Item{}, fmt.Errorf("send message: %w", err)
	}
	// Register the mode-switch rollback BEFORE any fallible work below.
	// beginImplementModeSwitch may have persisted a plan→chat switch, so
	// every early return from here on must be able to roll it back — the
	// defer therefore has to dominate the pre-stamp block, which can return
	// on a MergeProviderItemID error. userMsgKept flips true once the user
	// row lands, at which point the switch is a committed part of the send.
	userMsgKept := false
	defer func() {
		if err != nil && !userMsgKept {
			restoreImplementMode()
		}
	}()

	// Mint the user message id and stamp it onto the row meta BEFORE the
	// optimistic persist and anchor record below, then send it to the
	// provider as the envelope's message id. Claude honours a client-supplied
	// top-level uuid verbatim (see claude.Session.Send), so the row's
	// provider_item_id and the anchor's ProviderUserMessageID both point
	// at the real transcript uuid from the moment of persist — no dependency
	// on the replay echo. This closes the fast send→escape race: a revert
	// firing before the echo arrives still finds a stable id and takes the
	// UUID-keyed slice instead of the ordinal-walk fallback. Gated on Claude:
	// Codex assigns its own item ids and ignores a supplied uuid, so stamping
	// a fabricated id there would only mis-seed the row and trip the
	// echo-time drift log on every send.
	var sendUUID string
	if thread.Provider == string(provider.Claude) {
		sendUUID = uuid.NewString()
		userMeta, err = usermessage.MergeProviderItemID(userMeta, sendUUID)
		if err != nil {
			return store.Item{}, fmt.Errorf("send message: stamp send uuid: %w", err)
		}
	}
	a.ensureTriageRouter()
	if err := a.ensureClaudeContextReadyForUserSendLocked(thread); err != nil {
		return store.Item{}, fmt.Errorf("send message: %w", err)
	}
	if err := a.ensureProviderAccountReadyForSendLocked(thread); err != nil {
		return store.Item{}, fmt.Errorf("send message: %w", err)
	}
	hasPriorItems, err := a.store.HasItems(threadID)
	if err != nil {
		return store.Item{}, fmt.Errorf("send message: check prior items: %w", err)
	}
	turnIndex, err := a.store.LastTurnIndex(threadID)
	if err != nil {
		return store.Item{}, fmt.Errorf("send message: get turn index: %w", err)
	}
	if hasPriorItems {
		turnIndex++
	}
	if !isCodexReview {
		a.maybeRenameTemporaryWorktreeBranch(threadID, content)
	}

	now := time.Now().UnixMilli()
	userItem := store.Item{
		ID:        fmt.Sprintf("user:%d", turnIndex),
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   content,
		Meta:      userMeta,
		CreatedAt: now,
		UpdatedAt: now,
	}
	// Route through the triage chokepoint so parent_id validation,
	// emit order, and ItemsPersisted metric stay consistent with
	// provider-sourced items.
	if err = a.triage.PersistItem(userItem, nil); err != nil {
		return store.Item{}, fmt.Errorf("send message: persist user message: %w", err)
	}
	userMsgKept = true
	if !opts.PreserveDraft {
		if draftErr := a.removeThreadDraft(transport.ClientIdentity{}, threadID); draftErr != nil {
			log.Printf("send message: delete draft for thread %s: %v", threadID, draftErr)
		}
	}
	a.recordMessageAnchor(userItem)
	// Click-time plan/diff-review acceptance. Sticky: a subsequent
	// sendToProvider failure does NOT revert the marks — the user
	// committed to send and the on-screen badge reflects that, while
	// the failed-send sibling error stays available for retry. See
	// applyProposedPlanAcceptance.
	a.applyProposedPlanAcceptance(threadID, userItem, resolved)

	// A thread is session-less until the first message is sent — we don't
	// spawn the provider subprocess at thread creation. Lazy-start only
	// after the user row has landed so a cold provider startup cannot leave
	// the visible transcript looking idle while the app is already trying to
	// send. runSessionStart dedupes with any in-flight start kicked off by
	// SwitchThread auto-resume.
	sess, ok := a.sessionManager().get(threadID)
	if !ok {
		if startErr := a.startSession(ctx, threadID); startErr != nil {
			err = fmt.Errorf("start session: %w", startErr)
			a.recordSendFailureAndCompleteTurn(threadID, turnIndex, err)
			return store.Item{}, fmt.Errorf("send message: %w", err)
		}
		sess, ok = a.sessionManager().get(threadID)
		if !ok {
			err = fmt.Errorf("session unavailable after start for thread %s", threadID)
			a.recordSendFailureAndCompleteTurn(threadID, turnIndex, err)
			return store.Item{}, fmt.Errorf("send message: %w", err)
		}
	}
	// A cold session can finish starting while the user switches accounts in
	// Settings. Its immutable credential snapshot is intentionally allowed to
	// finish, but it must be rechecked before this first turn is dispatched.
	sess, unlockAccount, accountErr := a.lockProviderAccountForSendLocked(thread)
	if accountErr != nil {
		err = accountErr
		a.recordSendFailureAndCompleteTurn(threadID, turnIndex, err)
		return store.Item{}, fmt.Errorf("send message: %w", err)
	}
	defer unlockAccount()

	if opts.onProviderDispatch != nil {
		opts.onProviderDispatch(workflowhost.DispatchIdentity{
			Provider: sess.Provider, AccountID: sess.CredentialAccountID,
			CredentialGeneration: sess.CredentialGeneration,
		})
	}
	if isCodexReview {
		if sess.Codex == nil {
			err = fmt.Errorf("/review is only available on Codex threads")
			a.recordSendFailureAndCompleteTurn(threadID, turnIndex, err)
			return store.Item{}, fmt.Errorf("send message: %w", err)
		}
		reviewCtx, cancel := context.WithTimeout(context.Background(), codexReviewRPCTimeout)
		defer cancel()
		started, startErr := sess.Codex.StartReviewForTurn(
			reviewCtx,
			reviewTarget,
			codex.ReviewDeliveryInline,
			codex.ReviewRunOptions{TurnIndex: turnIndex},
		)
		if startErr != nil {
			err = fmt.Errorf("start code review: %w", startErr)
			a.recordSendFailureAndCompleteTurn(threadID, turnIndex, err)
			return store.Item{}, fmt.Errorf("send message: %w", err)
		}
		if started.Detached {
			err = fmt.Errorf("codex returned detached review thread %s for an inline review", started.ReviewThreadID)
			a.recordSendFailureAndCompleteTurn(threadID, turnIndex, err)
			return store.Item{}, fmt.Errorf("send message: %w", err)
		}
		if opts.onCodexReviewStarted != nil {
			opts.onCodexReviewStarted(started)
		}
		return userItem, nil
	}

	// Register the pending-send marker BEFORE sendToProvider writes to
	// stdin. The wire-init from Claude (or wire turn/started from Codex)
	// can otherwise race ahead of the marker and miss the
	// pending-send-present branch in handleInit / handleUserText. A review
	// deliberately has no marker: its literal command is not a provider user
	// item, and its synthetic visible turn carries TurnIndex directly.
	// The marker is consumed by handleUserText when the matching replay
	// envelope arrives, or cleared on send failure below. The identity the
	// echo matches by — and the wire stamp it rides in on — come from
	// providerSendIdentity, one derivation for every send entry point.
	clientUserMessageID, sendExpect := providerSendIdentity(sess, userItem.ID, sendUUID)
	a.triage.RegisterPendingSendWithExpectation(threadID, userItem.ID, turnIndex, sendExpect)

	if err := sendToProvider(sess, threadID, providerContent, provider.SendOptions{
		InteractionMode:         provider.NormalizeInteractionMode(thread.Mode),
		Attachments:             providerAttachments,
		UserMessageUUID:         sendUUID,
		ClientUserMessageID:     clientUserMessageID,
		OutputSchema:            opts.OutputSchema,
		GuardClaudeSlashCommand: !opts.ExpandComposerCommands || resolved.command != "",
	}); err != nil {
		// Drop the pending-send marker before persisting the error row.
		// Without this, the marker would still be live when the next AO
		// send registers a new entry, and a stale wire init for an orphaned
		// subprocess could hijack the new send's turn-start path.
		a.triage.ClearPendingSendForFailure(threadID, userItem.ID)
		a.recordSendFailureAndCompleteTurn(threadID, turnIndex, err)
		return store.Item{}, err
	}

	// Wire-driven turn-start: both providers now derive round 1 of the
	// logical turn from native wire signals — Claude from `system/init`
	// (routed through triage.handleInit when a pending-send is present),
	// Codex from `turn/started`. There is no synthetic EventTurnStart
	// emission here.

	if !isCodexReview && !threadmode.IsSagaOwned(thread.Mode) {
		a.maybeGenerateThreadTitleWithAttachments(thread, content, persistedAttachments, hasPriorItems)
	}
	return userItem, nil
}

// prepareWorkflowTakeoverSend detaches a workflow run for user steering.
// It MUST NOT run while holding the thread's action lock: TakeOver
// round-trips the engine command loop, whose teardown interrupts the
// live phase turn via InterruptTurn — which acquires that same lock.
func (a *App) prepareWorkflowTakeoverSend(ctx context.Context, thread store.Thread) (string, error) {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return "", err
	}
	// A fan-out unit thread resolves to a unit, not to a phase attempt. It is
	// checked first because a unit's thread is never also a phase's, and because
	// taking over one unit must not park the whole item the way taking over a
	// phase does.
	unit, isUnit, err := a.store.GetWorkItemUnitByThread(thread.ID)
	if err != nil {
		return "", fmt.Errorf("workflow takeover: %w", err)
	}
	if isUnit {
		return a.prepareWorkflowUnitTakeoverSend(ctx, workflowEngine, thread, unit)
	}
	item, err := a.store.GetWorkItemByPhaseThread(thread.ID)
	if err != nil {
		return "", fmt.Errorf("workflow takeover: %w", err)
	}
	if item.State != string(engine.StateRunning) && item.State != string(engine.StateNeedsHuman) {
		return "", fmt.Errorf("workflow takeover: item %s has no live or parked attempt", item.ID)
	}
	phases, err := a.store.ListWorkItemPhases(item.ID)
	if err != nil {
		return "", fmt.Errorf("workflow takeover: list phase attempts: %w", err)
	}
	current, ok := currentWorkflowPhaseAttempt(phases)
	if !ok || current.ThreadID != thread.ID {
		return "", fmt.Errorf("workflow takeover: thread %s is not the current attempt for item %s", thread.ID, item.ID)
	}
	if item.State == string(engine.StateNeedsHuman) && item.Reason == string(engine.ReasonTakenOver) {
		if _, active, activeErr := a.store.GetActiveTurn(thread.ID); activeErr != nil {
			return "", fmt.Errorf("workflow takeover: inspect steering turn: %w", activeErr)
		} else if active {
			return "", fmt.Errorf("workflow takeover: the prior turn must yield before steering again")
		}
		if err := a.workflowApplication().RegisterTakeover(ctx, item.ID, thread.ID); err != nil {
			return "", err
		}
		return item.ID, nil
	}
	if err := workflowEngine.TakeOver(item.ID); err != nil {
		return "", fmt.Errorf("workflow takeover: detach item: %w", err)
	}
	if err := a.workflowApplication().RegisterTakeover(ctx, item.ID, thread.ID); err != nil {
		return "", fmt.Errorf("workflow takeover: register schema-less steering: %w", err)
	}
	return item.ID, nil
}

// prepareWorkflowUnitTakeoverSend is prepareWorkflowTakeoverSend for one fan-out
// unit's thread: a send into a running unit detaches that unit for steering, and
// a send into an already-detached one re-registers it. The unit's siblings are
// untouched — they keep running, and the attempt parks once they rest.
func (a *App) prepareWorkflowUnitTakeoverSend(
	ctx context.Context, workflowEngine *engine.Engine, thread store.Thread, unit store.WorkItemUnit,
) (string, error) {
	item, err := a.store.GetWorkItem(unit.ItemID)
	if err != nil {
		return "", fmt.Errorf("workflow takeover: %w", err)
	}
	if unit.Status == store.WorkItemUnitTakenOver {
		if _, active, activeErr := a.store.GetActiveTurn(thread.ID); activeErr != nil {
			return "", fmt.Errorf("workflow takeover: inspect steering turn: %w", activeErr)
		} else if active {
			return "", fmt.Errorf("workflow takeover: the prior turn must yield before steering again")
		}
		if err := a.workflowApplication().RegisterTakeover(ctx, item.ID, thread.ID); err != nil {
			return "", err
		}
		return item.ID, nil
	}
	if unit.Status != store.WorkItemUnitRunning {
		return "", fmt.Errorf(
			"workflow takeover: unit %q of item %s is %s; only a running unit can be taken over",
			unit.UnitID, item.ID, unit.Status,
		)
	}
	if err := workflowEngine.TakeOverUnit(item.ID, unit.UnitID); err != nil {
		return "", fmt.Errorf("workflow takeover: detach unit %q: %w", unit.UnitID, err)
	}
	if err := a.workflowApplication().RegisterTakeover(ctx, item.ID, thread.ID); err != nil {
		return "", fmt.Errorf("workflow takeover: register schema-less steering: %w", err)
	}
	return item.ID, nil
}

func (a *App) recordSendFailureAndCompleteTurn(threadID string, turnIndex int, sendErr error) {
	a.ensureTriageRouter()
	turnID := ""
	canCompleteTurn := false
	turn, found, lookupErr := a.store.GetTurnByThreadIndex(threadID, turnIndex)
	if lookupErr != nil {
		log.Printf("send message: inspect send-failure turn: %v", lookupErr)
	} else if found {
		turnID = turn.TurnID
		canCompleteTurn = true
	} else if codex.IsAmbiguousTurnStartTimeout(sendErr) {
		// The provider may still announce the turn. A synthetic replacement
		// would collide with that late authority and falsely settle work that
		// is still running.
	} else {
		turnID = fmt.Sprintf("send-failure:%s:%d", threadID, turnIndex)
		if startErr := a.triage.HandleSynthetic(provider.ProviderEvent{
			Kind:      provider.EventTurnStart,
			ThreadID:  threadID,
			TurnID:    turnID,
			TurnIndex: turnIndex,
			Timestamp: time.Now(),
		}); startErr != nil {
			log.Printf("send message: start synthetic send-failure turn: %v", startErr)
		} else {
			canCompleteTurn = true
		}
	}
	// Allocate an error id from the same per-turn counter the EventError
	// handler uses so a subsequent provider error on the same turn doesn't
	// collide on "error:<turn>:0".
	errSeq := a.triage.NextErrorSequence(threadID, turnIndex, "")
	errNow := time.Now().UnixMilli()
	errorItem := store.Item{
		ID:        triage.NewErrorID(turnIndex, "", errSeq),
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		Kind:      triage.ItemKindError,
		Role:      "system",
		Status:    "completed",
		Summary:   fmt.Sprintf("Failed to send: %v", sendErr),
		CreatedAt: errNow,
		UpdatedAt: errNow,
	}
	if persistErr := a.triage.PersistItem(errorItem, nil); persistErr != nil {
		log.Printf("send message: persist send-failure error: %v", persistErr)
	}
	// HandleSynthetic, not Handle: a send can fail precisely because the
	// thread's session was stopped (the start attempt errored), and the
	// stopped-thread gate would silently drop this settle — leaving the
	// turn open from triage's perspective.
	if !canCompleteTurn {
		return
	}
	if completeErr := a.triage.HandleSynthetic(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     threadID,
		TurnID:       turnID,
		TurnIndex:    turnIndex,
		TurnComplete: &provider.TruncatedTurnCompleteMeta{},
		Timestamp:    time.Now(),
	}); completeErr != nil {
		// Log rather than propagate — the send error we're about to return is
		// the primary failure. A secondary triage hiccup here (e.g. store
		// closed mid-teardown) shouldn't swallow the original send error.
		log.Printf("send message: turn complete after send failure: %v", completeErr)
	}
}

// turnAttachments is what one turn's attachment ids resolve to. A struct
// rather than three returns because the three are not interchangeable and
// two of them are slices of near-identical shape: `images` is positional
// and binds to the `[Image #N]` markers, `records` is everything for the
// persisted meta, and `fileLines` is prompt text.
type turnAttachments struct {
	// images is the provider slice, in attachmentIDs order over the IMAGE
	// subset. Files are excluded: they have no marker and no slot.
	images []provider.ImageAttachment
	// records is every requested attachment, both kinds, in order. The
	// timeline row's meta is built from this.
	records []store.Attachment
	// fileLines is one attachment.PromptLine per `file`, in order. The
	// envelope appends them to providerContent; nothing persists them.
	fileLines []string
}

// resolveSendMessageAttachments validates the requested attachment IDs,
// checks thread ownership, and loads what each kind needs: bytes or a path
// for an image, a prompt line for a file.
func (a *App) resolveSendMessageAttachments(threadID string, attachmentIDs []string) (turnAttachments, error) {
	if len(attachmentIDs) == 0 {
		return turnAttachments{}, nil
	}
	// The cap is on the UNION: an attachment costs the user a slot whether
	// it arrives as an image block or as a path line.
	if len(attachmentIDs) > attachmentstore.DefaultMaxCount {
		return turnAttachments{}, fmt.Errorf("too many attachments: got %d, max %d", len(attachmentIDs), attachmentstore.DefaultMaxCount)
	}
	if a.attachments == nil {
		return turnAttachments{}, fmt.Errorf("attachment store not initialized")
	}

	// Two providers ingest an image by its on-disk PATH and read the file
	// themselves (provider.PathImageIngestion) — claude-tui pastes the path into the
	// real TUI composer, and Codex takes a `localImage` input item (which also earns
	// Codex's native numbered <image name=…> tag). Headless Claude has no local-path
	// image source on the Anthropic API, so it base64-encodes the bytes inline. Look
	// up the capability once — only on the rarer send that actually carries
	// attachments — so a path-ingesting provider loads just the path and never reads
	// bytes it won't use.
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return turnAttachments{}, fmt.Errorf("load thread: %w", err)
	}
	pathOnly := provider.CapabilitiesForProvider(thread.Provider).ImageIngestion == provider.PathImageIngestion

	// Order is load-bearing: `images` comes out in attachmentIDs order over the
	// image subset, the same order the composer numbered its "[Image #N]" markers
	// against — the composer numbers markers over images only, so a file between
	// two images does not consume a number. Every provider's send splits the
	// message at those markers and indexes this slice positionally (image #i →
	// images[i-1]; see provider.SplitContentByImageMarkers), so this loop must stay
	// a straight walk — don't reorder or dedup here or inline images would bind to
	// the wrong file.
	out := turnAttachments{
		images:  make([]provider.ImageAttachment, 0, len(attachmentIDs)),
		records: make([]store.Attachment, 0, len(attachmentIDs)),
	}
	for _, attachmentID := range attachmentIDs {
		// A file is always resolved by path, whatever the provider ingests
		// images as: the path IS its delivery, and its bytes are never read
		// into this process.
		record, path, err := a.attachments.PathForThread(threadID, attachmentID)
		if err != nil {
			return turnAttachments{}, err
		}
		out.records = append(out.records, record)
		if record.Kind == store.AttachmentKindFile {
			out.fileLines = append(out.fileLines, attachmentstore.PromptLine(record, path))
			continue
		}
		att := provider.ImageAttachment{
			ID:       record.ID,
			Filename: record.Filename,
			MimeType: record.MimeType,
			Size:     record.Size,
		}
		if pathOnly {
			att.Path = path
		} else {
			// The inline bytes come through ReadThreadBytes rather than off
			// `path`, because that accessor is the ONE place "only an
			// image's bytes are ever read" is enforced; the cost is a second
			// single-row metadata lookup, against a read of up to 10 MiB, on
			// a send that carries attachments at all.
			if _, att.Data, err = a.attachments.ReadThreadBytes(threadID, attachmentID); err != nil {
				return turnAttachments{}, err
			}
		}
		out.images = append(out.images, att)
	}
	return out, nil
}

// appendFileAttachmentLines returns providerContent with one
// `[Attached file …]` line per file attachment, after a blank line.
//
// It runs LAST, after composer-command expansion, so every content
// transform is already applied and a new one cannot be sandwiched between
// the user's text and the lines that describe what they attached. The
// lines go on providerContent only — the persisted content and the
// timeline row carry the attachment in `meta`, not as text — and they
// carry no `[Image #N]` marker, so the positional split is untouched.
func appendFileAttachmentLines(providerContent string, fileLines []string) string {
	if len(fileLines) == 0 {
		return providerContent
	}
	size := len(providerContent) + 2
	for _, line := range fileLines {
		size += len(line) + 1
	}
	var b strings.Builder
	b.Grow(size)
	b.WriteString(providerContent)
	b.WriteString("\n\n")
	for i, line := range fileLines {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
	}
	return b.String()
}

// nextSequenceForScope returns the next available sequence number for
// `user:<turnIndex>:<scope>:<n>` ids on the given thread/turn. Used
// by both the steer (`:steer:<n>`) and flush (`:flush:<n>`) paths to
// allocate non-colliding mid-turn user-message ids without each
// repeating the same MAX-by-suffix scan.
//
// Returns 1 on an empty turn (highest+1 with highest=0). Unparsable
// suffixes are skipped so a future id-format extension doesn't crash
// this counter.
func (a *App) nextSequenceForScope(threadID string, turnIndex int, scope string) (int, error) {
	prefix := fmt.Sprintf("user:%d:%s:", turnIndex, scope)
	items, err := a.store.ListItemsForTurn(threadID, turnIndex)
	if err != nil {
		return 0, err
	}
	highest := 0
	for _, it := range items {
		if !strings.HasPrefix(it.ID, prefix) {
			continue
		}
		var n int
		if _, scanErr := fmt.Sscanf(it.ID[len(prefix):], "%d", &n); scanErr != nil {
			continue
		}
		if n > highest {
			highest = n
		}
	}
	return highest + 1, nil
}

// nextSequencedUserItemID formats the next sequenced id for the given
// scope (`steer` or `flush`). Wrapper around nextSequenceForScope so
// callers that want the formatted id don't have to do their own
// Sprintf.
func (a *App) nextSequencedUserItemID(threadID string, turnIndex int, scope string) (string, error) {
	seq, err := a.nextSequenceForScope(threadID, turnIndex, scope)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("user:%d:%s:%d", turnIndex, scope, seq), nil
}

func (a *App) resolveSourceDiffReview(threadID string, source *SourceDiffReview) (*SourceDiffReview, error) {
	if source == nil || strings.TrimSpace(source.Scope) == "" {
		return nil, nil
	}
	scope, err := store.NormalizeDiffReviewScope(source.Scope)
	if err != nil {
		return nil, err
	}
	sourceThreadID := strings.TrimSpace(source.ThreadID)
	if sourceThreadID == "" {
		sourceThreadID = threadID
	}
	if sourceThreadID != threadID {
		return nil, fmt.Errorf("diff review source must be on the current thread")
	}
	sourceKey, err := store.NormalizeDiffReviewSourceKey(source.SourceKey)
	if err != nil {
		return nil, err
	}
	return &SourceDiffReview{ThreadID: sourceThreadID, Scope: scope, SourceKey: sourceKey, PR: source.PR}, nil
}

func (a *App) resolveSourceProposedPlan(threadID string, source *SourceProposedPlan, allowCrossThread bool) (*SourceProposedPlan, error) {
	if source == nil || strings.TrimSpace(source.ItemID) == "" {
		return nil, nil
	}
	sourceThreadID := strings.TrimSpace(source.ThreadID)
	if sourceThreadID == "" {
		sourceThreadID = threadID
	}
	if !allowCrossThread && sourceThreadID != threadID {
		return nil, fmt.Errorf("source plan thread %s does not match target thread %s", sourceThreadID, threadID)
	}
	item, found, err := a.store.GetThreadItem(sourceThreadID, strings.TrimSpace(source.ItemID))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("source proposed plan %s not found", source.ItemID)
	}
	if item.PayloadKind != "proposed_plan" || item.PayloadID == "" || item.Role != "assistant" {
		return nil, fmt.Errorf("source item %s is not an assistant proposed plan", source.ItemID)
	}
	if _, err := a.store.EnsureProposedPlanState(sourceThreadID, item.ID, time.Now().UnixMilli()); err != nil {
		return nil, fmt.Errorf("ensure source proposed plan state: %w", err)
	}
	// A first-ensure creates the proposed_plans row that makes
	// hasActionableProposedPlan true; a repeat ensure re-states a row and
	// the `full` broadcast is idempotent on the receiving side.
	a.broadcastThreadRowByID(sourceThreadID)
	resolved := &SourceProposedPlan{
		ThreadID:  sourceThreadID,
		ItemID:    item.ID,
		PayloadID: item.PayloadID,
	}
	var payloadMeta struct {
		Title string `json:"title"`
	}
	_ = json.Unmarshal([]byte(item.PayloadMeta), &payloadMeta)
	resolved.Title = strings.TrimSpace(payloadMeta.Title)
	return resolved, nil
}

func (a *App) appendPlanRevisionCommentsToContent(threadID, content, planItemID string, commentIDs []string) (string, []string, error) {
	if len(store.UniqueNonEmptyStringsForApp(commentIDs)) > store.MaxProposedPlanRevisionCommentIDs {
		return "", nil, fmt.Errorf("too many comments selected")
	}
	comments, err := a.store.ListDraftProposedPlanCommentsByID(threadID, planItemID, commentIDs)
	if err != nil {
		return "", nil, err
	}
	if len(comments) == 0 {
		return "", nil, fmt.Errorf("no draft comments selected")
	}
	prompt := planrevision.BuildPrompt(comments)
	ids := planrevision.IDsOf(comments)
	if strings.TrimSpace(content) == "" {
		return prompt, ids, nil
	}
	return strings.TrimSpace(content) + "\n\n" + prompt, ids, nil
}

// beginImplementModeSwitch loads the current thread row and, when
// this is an implement send (sourcePlan != nil) on a plan-mode
// thread, flips the mode to chat. The returned restore function
// rolls the flip back; the caller defers it for the
// pre-PersistItem failure window. When no flip happens the restore
// is a no-op so the call site doesn't need to branch on whether
// the saga actually ran.
//
// Keeping the flip+restore here and out of sendMessageWithOptions
// means the send pipe stays single-purpose: it persists, sends,
// and synthesizes; it does not own transactional mode coordination.
func (a *App) beginImplementModeSwitch(thread store.Thread, sourcePlan *SourceProposedPlan) (store.Thread, func(), error) {
	if sourcePlan == nil || thread.Mode != "plan" {
		return thread, func() {}, nil
	}
	prevMode := thread.Mode
	if _, err := a.UpdateThreadMode(thread.ID, "chat"); err != nil {
		return store.Thread{}, func() {}, fmt.Errorf("switch mode for plan implementation: %w", err)
	}
	thread.Mode = "chat"
	restore := func() {
		if _, err := a.UpdateThreadMode(thread.ID, prevMode); err != nil {
			log.Printf("send message: revert mode after implement failure for thread %s: %v", thread.ID, err)
		}
	}
	return thread, restore, nil
}

// applyProposedPlanAcceptance is the click-time mark for every plan or
// diff-review interaction the user just committed to. The badge flips
// on the user's click rather than on the model's echo.
//
// Call-site timing differs by path:
//   - Send / steer call it BEFORE the provider write. A subsequent
//     provider failure does NOT revert the mark — the click itself is
//     the commitment.
//   - Flush-queue dispatch calls it AFTER a successful (or ambiguous,
//     write-but-no-ack) dispatch. A genuine dispatch failure leaves the
//     queue item retryable and the plan unmarked; marking pre-dispatch
//     would attribute the implementation to a message that never reached
//     the provider.
//
// MarkProposedPlanImplemented is gated on `WHERE implemented_at = 0`,
// so a retry / re-click no-ops on the mark and keeps the original
// implementing-item attribution stable.
//
// All three table writes are independent; we log-and-continue per write
// rather than failing the user message. The plan/comment badge is UI
// state, not a precondition for the outgoing message.
func (a *App) applyProposedPlanAcceptance(threadID string, userItem store.Item, resolved resolvedUserMessage) {
	now := time.Now().UnixMilli()
	// Group all comments-sent-as-one-click under a stable turn-scoped
	// id. Matches the legacy resolveTurnID Claude fallback and lets the
	// frontend filter "comments sent in this turn" without enumerating
	// per-item ids.
	sentTurnID := fmt.Sprintf("%s:%d", threadID, userItem.TurnIndex)

	if sp := resolved.sourcePlan; sp != nil && strings.TrimSpace(sp.ItemID) != "" {
		err := a.store.MarkProposedPlanImplemented(sp.ThreadID, sp.ItemID, threadID, userItem.ID, now)
		switch {
		case err == nil:
			a.refreshProposedPlanItem(sp.ThreadID, sp.ItemID)
			// The plan stopped being actionable, which is a derived
			// column of the thread row. Off-pane sidebars read the pill
			// from there, not from the item upsert above.
			a.broadcastThreadRowByID(sp.ThreadID)
		case errors.Is(err, store.ErrProposedPlanAlreadyImplemented):
			// Re-click on an already-implemented plan: keep the
			// existing attribution and skip the emit (nothing changed).
		default:
			log.Printf("apply plan acceptance: mark implemented %s/%s: %v", sp.ThreadID, sp.ItemID, err)
		}
	}

	if rp := resolved.revisionSourcePlan; rp != nil && strings.TrimSpace(rp.ItemID) != "" && len(resolved.revisionPlanCommentIDs) > 0 {
		if err := a.store.MarkProposedPlanCommentsSent(rp.ThreadID, rp.ItemID, resolved.revisionPlanCommentIDs, now, sentTurnID); err != nil {
			log.Printf("apply plan acceptance: mark revision comments sent %s/%s: %v", rp.ThreadID, rp.ItemID, err)
		} else {
			a.refreshProposedPlanItem(rp.ThreadID, rp.ItemID)
			a.announcePlanCommentsChanged(rp.ThreadID, rp.ItemID)
		}
	}

	if rd := resolved.revisionSourceDiff; rd != nil && len(resolved.revisionDiffCommentIDs) > 0 {
		if err := a.store.MarkDiffReviewCommentsSent(threadID, rd.Scope, rd.SourceKey, resolved.revisionDiffCommentIDs, now, sentTurnID); err != nil {
			log.Printf("apply plan acceptance: mark diff review comments sent: %v", err)
		} else {
			a.announceDiffCommentsChanged(threadID, rd.Scope, rd.SourceKey)
		}
	}
}

// refreshProposedPlanItem re-reads the decorated plan item and emits
// the upsert so the frontend's Accepted badge flips without a thread
// reload. Best-effort: a missing row (plan deleted under us) is logged
// and ignored so applyProposedPlanAcceptance never blocks the send.
func (a *App) refreshProposedPlanItem(threadID, itemID string) {
	plan, found, err := a.store.GetThreadProposedPlanItem(threadID, itemID)
	if err != nil {
		log.Printf("apply plan acceptance: refresh proposed plan %s/%s: %v", threadID, itemID, err)
		return
	}
	if !found {
		return
	}
	a.emit(eventchan.ProviderItemEvent, triage.NewItemStreamUpsert(plan))
}

// providerSendIdentity is the ONE constructor of a user send's wire
// correlation: the `clientUserMessageId` stamped on the provider call and
// the pending-send expectation its echo is matched against. The two are
// one decision — an entry registered ByClientID is invisible to an
// id-less echo, so a send that stamps without registering (or the
// reverse) is the 2026-08-24 mispop — which is why every send entry
// point derives the pair here instead of assembling it by hand.
//
// Codex names the row from AO's side: the row id rides
// `clientUserMessageId` and the `userMessage` echo returns it as
// `clientId`. The Claude family runs the other way: AO mints sendUUID
// onto the user envelope and the CLI echoes it verbatim as the item id
// (claude-tui echoes id-less, which the registry's head-pop carve-out
// absorbs). Codex callers, which never mint a uuid, pass sendUUID "".
func providerSendIdentity(sess session, rowID, sendUUID string) (clientUserMessageID string, expect triage.PendingSendExpectation) {
	if sess.Codex != nil {
		return rowID, triage.PendingSendExpectation{ByClientID: true}
	}
	return "", triage.PendingSendExpectation{ProviderItemID: sendUUID}
}

// sendToProvider forwards the user content to the active provider
// session. Extracted so sendMessage keeps the provider routing and
// logging in one place after persisting the optimistic user item.
//
// Takes the assembled provider.SendOptions rather than its fields so a new
// per-turn option cannot reach one send entry point and miss another — the
// flush dispatcher's dispatchFlushToProvider has the same shape for the same
// reason.
func sendToProvider(sess session, threadID string, content string, opts provider.SendOptions) error {
	providerSess := sess.ProviderSession()
	if providerSess == nil {
		log.Printf("send message: session for thread %s has no provider", threadID)
		return fmt.Errorf("session has no provider")
	}
	// Stamp user-send activity before stdin write so the idle reaper
	// can't race a slow send-and-respawn against its own teardown.
	// The matching provider events that follow will bump again — the
	// reaper only cares about the latest stamp.
	sess.Liveness.BumpActivity(time.Now())
	return providerSess.Send(context.Background(), content, opts)
}

func (a *App) sendWorkflowMessage(
	ctx context.Context, threadID, content string,
	outputSchema json.RawMessage, onDispatch func(workflowhost.DispatchIdentity),
) error {
	if len(outputSchema) == 0 {
		return fmt.Errorf("workflow send: output schema is required")
	}
	_, err := a.sendMessageWithOptions(ctx, threadID, content, sendMessageOptions{
		OutputSchema: outputSchema, onProviderDispatch: onDispatch,
	})
	return err
}
