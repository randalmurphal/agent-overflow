package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	attachmentstore "agent-overflow/internal/attachment"
	"agent-overflow/internal/planrevision"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/triage"
)

// sendThreadMuRegistry owns a mutex per thread so the "compute turn index
// / persist user item / call provider" sequence can't interleave for the
// same thread (Bug B11). Concurrent sends on DIFFERENT threads proceed
// in parallel — that's the whole point of splitting the lock from a
// global sendMu. Also avoids the audit #52 misattribution where two
// in-flight sends on one thread both attributed assistant replies to
// max(turn_index) instead of the turn that actually spoke.
var sendThreadMuRegistry = &threadMutexRegistry{
	mus: make(map[string]*sync.Mutex),
}

type userMessageMeta struct {
	Attachments                  []userMessageAttachmentMeta `json:"attachments,omitempty"`
	SourceProposedPlan           *SourceProposedPlan         `json:"sourceProposedPlan,omitempty"`
	RevisionSourceProposedPlan   *SourceProposedPlan         `json:"revisionSourceProposedPlan,omitempty"`
	RevisionSourceCommentIDs     []string                    `json:"revisionSourceCommentIds,omitempty"`
	RevisionSourceDiffReview     *SourceDiffReview           `json:"revisionSourceDiffReview,omitempty"`
	RevisionSourceDiffCommentIDs []string                    `json:"revisionSourceDiffCommentIds,omitempty"`
}

type userMessageAttachmentMeta struct {
	ID       string `json:"id"`
	ThreadID string `json:"threadId"`
	Filename string `json:"filename"`
	MimeType string `json:"mimeType"`
	Size     int64  `json:"size"`
}

// SourceProposedPlan records that a user follow-up is acting on a specific
// immutable proposed-plan item. It is traceability metadata, not prompt text.
type SourceProposedPlan = store.ProposedPlanSourceRef
type SourceDiffReview = store.DiffReviewSourceRef

type threadMutexRegistry struct {
	mu  sync.Mutex
	mus map[string]*sync.Mutex
}

// lockFor returns an unlock function that must be called once the
// per-thread critical section completes. The registry caches one mutex
// per thread; ForgetThread should be called when the thread is deleted
// so a very long-lived process doesn't accumulate one dead mutex per
// deleted thread indefinitely. (Each struct is tiny, but bounded
// cleanup is still the right posture.)
func (r *threadMutexRegistry) lockFor(threadID string) func() {
	r.mu.Lock()
	m, ok := r.mus[threadID]
	if !ok {
		m = &sync.Mutex{}
		r.mus[threadID] = m
	}
	r.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// ForgetThread drops the per-thread mutex entry. Called from the thread
// deletion path so the registry doesn't keep dead mutexes forever.
// Safe to call for an unknown threadID. No-op if there's no entry.
//
// Callers must ensure no goroutine is holding the mutex for this
// thread; by the time deletion runs, the per-thread session has already
// been stopped (see deleteThreadTree) and no new sendMessage call can
// arrive because the frontend only sends for live threads.
func (r *threadMutexRegistry) ForgetThread(threadID string) {
	r.mu.Lock()
	delete(r.mus, threadID)
	r.mu.Unlock()
}

type sendMessageOptions struct {
	AttachmentIDs                []string
	RuntimeMode                  string
	SourceProposedPlan           *SourceProposedPlan
	RevisionSourceProposedPlan   *SourceProposedPlan
	RevisionSourceCommentIDs     []string
	RevisionSourceDiffReview     *SourceDiffReview
	RevisionSourceDiffCommentIDs []string
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
}

// resolvedUserMessage bundles everything resolveUserMessageEnvelope
// produces: the (possibly comment-appended) content, the loaded
// attachments in both provider and store shape, the validated
// plan/diff references and their comment id lists, and the marshaled
// userMessageMeta the caller writes into store.Item.Meta.
type resolvedUserMessage struct {
	content                string
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
	providerAttachments, persistedAttachments, err := a.resolveSendMessageAttachments(threadID, inputs.attachmentIDs)
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
		nextContent, commentIDs, err := a.appendDiffReviewCommentsToContent(threadID, content, revisionSourceDiff.Scope, revisionSourceDiff.SourceKey, revisionDiffCommentIDs)
		if err != nil {
			return resolvedUserMessage{}, fmt.Errorf("diff review comments: %w", err)
		}
		content = nextContent
		revisionDiffCommentIDs = commentIDs
	}

	userMeta, err := marshalUserMessageMeta(
		persistedAttachments,
		sourcePlan,
		revisionSourcePlan,
		revisionCommentIDs,
		revisionSourceDiff,
		revisionDiffCommentIDs,
	)
	if err != nil {
		return resolvedUserMessage{}, fmt.Errorf("user meta: %w", err)
	}

	return resolvedUserMessage{
		content:                content,
		providerAttachments:    providerAttachments,
		persistedAttachments:   persistedAttachments,
		sourcePlan:             sourcePlan,
		revisionSourcePlan:     revisionSourcePlan,
		revisionPlanCommentIDs: revisionCommentIDs,
		revisionSourceDiff:     revisionSourceDiff,
		revisionDiffCommentIDs: revisionDiffCommentIDs,
		userMessageMeta:        userMeta,
	}, nil
}

func (a *App) sendMessage(threadID string, content string, attachmentIDs []string) error {
	_, err := a.sendMessageWithOptions(threadID, content, sendMessageOptions{AttachmentIDs: attachmentIDs})
	return err
}

func (a *App) sendMessageWithOptions(threadID string, content string, opts sendMessageOptions) (item store.Item, err error) {
	if a.shuttingDown.Load() {
		return store.Item{}, ErrShuttingDown
	}
	if a.sendMessageFn != nil {
		return store.Item{}, a.sendMessageFn(threadID, content, opts.AttachmentIDs)
	}

	runtimeMode, hasRuntimeMode, err := threadmode.ParseOptionalRuntime(opts.RuntimeMode)
	if err != nil {
		return store.Item{}, fmt.Errorf("send message: %w", err)
	}

	// Per-thread critical section: only one Send per thread at a time.
	// This keeps the runtime-mode update, turn-index read, optimistic
	// user-item persist, lazy session start, pending-send registration, and
	// provider dispatch sequence atomic for a single thread while letting
	// different threads proceed in parallel.
	unlock := sendThreadMuRegistry.lockFor(threadID)
	defer unlock()

	if hasRuntimeMode {
		if err := a.applyRuntimeModeLocked(threadID, runtimeMode); err != nil {
			return store.Item{}, fmt.Errorf("send message: runtime mode: %w", err)
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
	})
	if err != nil {
		return store.Item{}, fmt.Errorf("send message: %w", err)
	}
	content = resolved.content
	providerAttachments := resolved.providerAttachments
	persistedAttachments := resolved.persistedAttachments
	sourcePlan := resolved.sourcePlan
	revisionSourceDiff := resolved.revisionSourceDiff
	revisionDiffCommentIDs := resolved.revisionDiffCommentIDs
	userMeta := resolved.userMessageMeta

	thread, restoreImplementMode, err := a.beginImplementModeSwitch(threadID, sourcePlan)
	if err != nil {
		return store.Item{}, fmt.Errorf("send message: %w", err)
	}
	userMsgKept := false
	defer func() {
		if err != nil && !userMsgKept {
			restoreImplementMode()
		}
	}()
	if a.triage == nil {
		a.triage = triage.NewRouter(a.store, a.emitWithReplay())
		a.configureTriageQueueCallbacks()
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
	a.maybeRenameTemporaryWorktreeBranch(threadID, content)

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
	a.captureMessageCheckpoint(thread, userItem)

	// A thread is session-less until the first message is sent — we don't
	// spawn the provider subprocess at thread creation. Lazy-start only
	// after the user row has landed so a cold provider startup cannot leave
	// the visible transcript looking idle while the app is already trying to
	// send. runSessionStart dedupes with any in-flight start kicked off by
	// SwitchThread auto-resume.
	a.mu.Lock()
	sess, ok := a.sessions[threadID]
	a.mu.Unlock()
	if !ok {
		if startErr := a.startSession(threadID); startErr != nil {
			err = fmt.Errorf("start session: %w", startErr)
			a.recordSendFailureAndCompleteTurn(threadID, turnIndex, err)
			return store.Item{}, fmt.Errorf("send message: %w", err)
		}
		a.mu.Lock()
		sess, ok = a.sessions[threadID]
		a.mu.Unlock()
		if !ok {
			err = fmt.Errorf("session unavailable after start for thread %s", threadID)
			a.recordSendFailureAndCompleteTurn(threadID, turnIndex, err)
			return store.Item{}, fmt.Errorf("send message: %w", err)
		}
	}

	// Register the pending-send marker BEFORE sendToProvider writes to
	// stdin. The wire-init from Claude (or wire turn/started from Codex)
	// can otherwise race ahead of the marker and miss the
	// pending-send-present branch in handleInit / handleUserText. The
	// marker is consumed by handleUserText when the matching replay
	// envelope arrives, or cleared on send failure below.
	a.triage.RegisterPendingSend(threadID, userItem.ID, turnIndex)

	// Plan-implemented marking happens via the wire-driven turn-start
	// path: Claude's `system/init` (consumed by triage.handleInit when a
	// pending-send is present, which routes to handleTurnStart) for
	// Claude, and the native `turn/started` for Codex. Pre-send marking
	// would hide the Implement button before sendToProvider's outcome
	// is known, so a transient send failure could not be retried.
	// Restart durability comes from
	// ReconcileProposedPlanStateFromAcceptedTurns, which LEFT-JOINs
	// turns and excludes user_text rows with sibling kind='error' so a
	// failed-send remains retryable across restarts too.

	if err := sendToProvider(sess, threadID, content, provider.NormalizeInteractionMode(thread.Mode), providerAttachments); err != nil {
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

	a.maybeGenerateThreadTitleWithAttachments(thread, content, hasPriorItems, persistedAttachments)
	if revisionSourceDiff != nil && len(revisionDiffCommentIDs) > 0 {
		if err := a.store.MarkDiffReviewCommentsSent(threadID, revisionSourceDiff.Scope, revisionSourceDiff.SourceKey, revisionDiffCommentIDs, time.Now().UnixMilli(), userItem.ID); err != nil {
			log.Printf("send message: mark diff review comments sent: %v", err)
		}
	}
	return userItem, nil
}

func (a *App) recordSendFailureAndCompleteTurn(threadID string, turnIndex int, sendErr error) {
	if a.triage == nil {
		a.triage = triage.NewRouter(a.store, a.emitWithReplay())
		a.configureTriageQueueCallbacks()
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
		Kind:      "error",
		Role:      "system",
		Status:    "completed",
		Summary:   fmt.Sprintf("Failed to send: %v", sendErr),
		CreatedAt: errNow,
		UpdatedAt: errNow,
	}
	if persistErr := a.triage.PersistItem(errorItem, nil); persistErr != nil {
		log.Printf("send message: persist send-failure error: %v", persistErr)
	}
	if completeErr := a.triage.Handle(provider.ProviderEvent{
		Kind:         provider.EventTurnComplete,
		ThreadID:     threadID,
		TurnComplete: &provider.TruncatedTurnCompleteMeta{},
		Timestamp:    time.Now(),
	}); completeErr != nil {
		// Log rather than propagate — the send error we're about to return is
		// the primary failure. A secondary triage hiccup here (e.g. store
		// closed mid-teardown) shouldn't swallow the original send error.
		log.Printf("send message: turn complete after send failure: %v", completeErr)
	}
}

// resolveSendMessageAttachments validates the requested attachment IDs,
// checks thread ownership, and loads the provider-ready bytes.
func (a *App) resolveSendMessageAttachments(threadID string, attachmentIDs []string) ([]provider.ImageAttachment, []store.Attachment, error) {
	if len(attachmentIDs) == 0 {
		return nil, nil, nil
	}
	if len(attachmentIDs) > attachmentstore.DefaultMaxCount {
		return nil, nil, fmt.Errorf("too many attachments: got %d, max %d", len(attachmentIDs), attachmentstore.DefaultMaxCount)
	}
	if a.attachments == nil {
		return nil, nil, fmt.Errorf("attachment store not initialized")
	}

	providerAttachments := make([]provider.ImageAttachment, 0, len(attachmentIDs))
	persistedAttachments := make([]store.Attachment, 0, len(attachmentIDs))
	for _, attachmentID := range attachmentIDs {
		record, data, err := a.attachments.ReadThreadBytes(threadID, attachmentID)
		if err != nil {
			return nil, nil, err
		}
		persistedAttachments = append(persistedAttachments, record)
		providerAttachments = append(providerAttachments, provider.ImageAttachment{
			ID:       record.ID,
			Filename: record.Filename,
			MimeType: record.MimeType,
			Size:     record.Size,
			Data:     data,
		})
	}
	return providerAttachments, persistedAttachments, nil
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

func marshalUserMessageMeta(
	attachments []store.Attachment,
	sourcePlan, revisionSourcePlan *SourceProposedPlan,
	revisionCommentIDs []string,
	revisionSourceDiff *SourceDiffReview,
	revisionDiffCommentIDs []string,
) (string, error) {
	if len(attachments) == 0 &&
		sourcePlan == nil &&
		revisionSourcePlan == nil &&
		len(revisionCommentIDs) == 0 &&
		revisionSourceDiff == nil &&
		len(revisionDiffCommentIDs) == 0 {
		return "", nil
	}
	metaAttachments := make([]userMessageAttachmentMeta, 0, len(attachments))
	for _, attachment := range attachments {
		metaAttachments = append(metaAttachments, userMessageAttachmentMeta{
			ID:       attachment.ID,
			ThreadID: attachment.ThreadID,
			Filename: attachment.Filename,
			MimeType: attachment.MimeType,
			Size:     attachment.Size,
		})
	}
	meta := userMessageMeta{
		Attachments:                  metaAttachments,
		SourceProposedPlan:           sourcePlan,
		RevisionSourceProposedPlan:   revisionSourcePlan,
		RevisionSourceCommentIDs:     revisionCommentIDs,
		RevisionSourceDiffReview:     revisionSourceDiff,
		RevisionSourceDiffCommentIDs: revisionDiffCommentIDs,
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	return string(data), nil
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
	return &SourceDiffReview{ThreadID: sourceThreadID, Scope: scope, SourceKey: sourceKey}, nil
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
func (a *App) beginImplementModeSwitch(threadID string, sourcePlan *SourceProposedPlan) (store.Thread, func(), error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return store.Thread{}, func() {}, fmt.Errorf("get thread: %w", err)
	}
	if sourcePlan == nil || thread.Mode != "plan" {
		return thread, func() {}, nil
	}
	prevMode := thread.Mode
	if _, err := a.UpdateThreadMode(threadID, "chat"); err != nil {
		return store.Thread{}, func() {}, fmt.Errorf("switch mode for plan implementation: %w", err)
	}
	thread.Mode = "chat"
	restore := func() {
		if _, err := a.UpdateThreadMode(threadID, prevMode); err != nil {
			log.Printf("send message: revert mode after implement failure for thread %s: %v", threadID, err)
		}
	}
	return thread, restore, nil
}

// sendToProvider forwards the user content to the active provider
// session. Extracted so sendMessage keeps the provider routing and
// logging in one place after persisting the optimistic user item.
func sendToProvider(
	sess session,
	threadID string,
	content string,
	mode provider.InteractionMode,
	attachments []provider.ImageAttachment,
) error {
	providerSess := sess.providerSession()
	if providerSess == nil {
		log.Printf("send message: session for thread %s has no provider", threadID)
		return fmt.Errorf("session has no provider")
	}
	// Stamp user-send activity before stdin write so the idle reaper
	// can't race a slow send-and-respawn against its own teardown.
	// The matching provider events that follow will bump again — the
	// reaper only cares about the latest stamp.
	sess.liveness.bumpActivity(time.Now())
	return providerSess.Send(context.Background(), content, provider.SendOptions{
		InteractionMode: mode,
		Attachments:     attachments,
	})
}
