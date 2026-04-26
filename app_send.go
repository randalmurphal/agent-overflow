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
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
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
	Attachments                []userMessageAttachmentMeta `json:"attachments,omitempty"`
	SourceProposedPlan         *SourceProposedPlan         `json:"sourceProposedPlan,omitempty"`
	RevisionSourceProposedPlan *SourceProposedPlan         `json:"revisionSourceProposedPlan,omitempty"`
	RevisionSourceCommentIDs   []string                    `json:"revisionSourceCommentIds,omitempty"`
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
	AttachmentIDs              []string
	RuntimeMode                string
	SourceProposedPlan         *SourceProposedPlan
	RevisionSourceProposedPlan *SourceProposedPlan
	RevisionSourceCommentIDs   []string
}

func (a *App) sendMessage(threadID string, content string, attachmentIDs []string) error {
	_, err := a.sendMessageWithOptions(threadID, content, sendMessageOptions{AttachmentIDs: attachmentIDs})
	return err
}

func (a *App) sendMessageWithOptions(threadID string, content string, opts sendMessageOptions) (store.Item, error) {
	if a.shuttingDown.Load() {
		return store.Item{}, ErrShuttingDown
	}
	if a.sendMessageFn != nil {
		return store.Item{}, a.sendMessageFn(threadID, content, opts.AttachmentIDs)
	}

	runtimeMode, hasRuntimeMode, err := parseOptionalRuntimeMode(opts.RuntimeMode)
	if err != nil {
		return store.Item{}, fmt.Errorf("send message: %w", err)
	}

	providerAttachments, persistedAttachments, err := a.resolveSendMessageAttachments(threadID, opts.AttachmentIDs)
	if err != nil {
		return store.Item{}, fmt.Errorf("send message: attachments: %w", err)
	}

	// Per-thread critical section: only one Send per thread at a time.
	// This keeps the lazy-session-start + read-turn-index + insert-user-item +
	// call-provider sequence atomic for a single thread while letting
	// different threads proceed in parallel. Held across the lazy start so
	// concurrent sends on a session-less thread don't race on spawn
	// ordering.
	unlock := sendThreadMuRegistry.lockFor(threadID)
	defer unlock()

	if hasRuntimeMode {
		if err := a.applyRuntimeModeLocked(threadID, runtimeMode); err != nil {
			return store.Item{}, fmt.Errorf("send message: runtime mode: %w", err)
		}
	}

	sourcePlan, err := a.resolveSourceProposedPlan(threadID, opts.SourceProposedPlan, true)
	if err != nil {
		return store.Item{}, fmt.Errorf("send message: source proposed plan: %w", err)
	}
	if sourcePlan != nil {
		state, found, err := a.store.GetProposedPlanState(sourcePlan.ThreadID, sourcePlan.ItemID)
		if err != nil {
			return store.Item{}, fmt.Errorf("send message: source proposed plan state: %w", err)
		}
		if found && state.ImplementedAt > 0 {
			return store.Item{}, fmt.Errorf("send message: source proposed plan: %w", store.ErrProposedPlanAlreadyImplemented)
		}
	}
	revisionSourcePlan, err := a.resolveSourceProposedPlan(threadID, opts.RevisionSourceProposedPlan, false)
	if err != nil {
		return store.Item{}, fmt.Errorf("send message: revision source proposed plan: %w", err)
	}
	if revisionSourcePlan == nil && len(opts.RevisionSourceCommentIDs) > 0 {
		return store.Item{}, fmt.Errorf("send message: revision comments require a source proposed plan")
	}
	if revisionSourcePlan != nil && len(opts.RevisionSourceCommentIDs) > 0 {
		nextContent, commentIDs, err := a.appendPlanRevisionCommentsToContent(threadID, content, revisionSourcePlan.ItemID, opts.RevisionSourceCommentIDs)
		if err != nil {
			return store.Item{}, fmt.Errorf("send message: revision comments: %w", err)
		}
		content = nextContent
		opts.RevisionSourceCommentIDs = commentIDs
	}

	userMeta, err := marshalUserMessageMeta(persistedAttachments, sourcePlan, revisionSourcePlan, opts.RevisionSourceCommentIDs)
	if err != nil {
		return store.Item{}, fmt.Errorf("send message: user meta: %w", err)
	}

	// A thread is session-less until the first message is sent — we don't
	// spawn the provider subprocess at thread creation. Lazy-start here so
	// the user's "new thread → type → send" flow works without an explicit
	// Start step. runSessionStart dedupes with any in-flight start kicked
	// off by SwitchThread auto-resume.
	a.mu.Lock()
	sess, ok := a.sessions[threadID]
	a.mu.Unlock()
	if !ok {
		if err := a.startSession(threadID); err != nil {
			return store.Item{}, fmt.Errorf("send message: start session: %w", err)
		}
		a.mu.Lock()
		sess, ok = a.sessions[threadID]
		a.mu.Unlock()
		if !ok {
			return store.Item{}, fmt.Errorf("send message: session unavailable after start for thread %s", threadID)
		}
	}

	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return store.Item{}, fmt.Errorf("send message: get thread: %w", err)
	}
	if a.triage == nil {
		a.triage = triage.NewRouter(a.store, a.emitWithReplay())
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
	if err := a.triage.PersistItem(userItem, nil); err != nil {
		return store.Item{}, fmt.Errorf("send message: persist user message: %w", err)
	}

	if err := sendToProvider(sess, threadID, content, provider.NormalizeInteractionMode(thread.Mode), providerAttachments); err != nil {
		// Allocate an error id from the same per-turn counter the
		// EventError handler uses so a subsequent provider error on
		// the same turn doesn't collide on "error:<turn>:0".
		errSeq := a.triage.NextErrorSequence(threadID, turnIndex, "")
		errNow := time.Now().UnixMilli()
		errorItem := store.Item{
			ID:        triage.NewErrorID(turnIndex, "", errSeq),
			ThreadID:  threadID,
			TurnIndex: turnIndex,
			Kind:      "error",
			Role:      "system",
			Status:    "completed",
			Summary:   fmt.Sprintf("Failed to send: %v", err),
			CreatedAt: errNow,
			UpdatedAt: errNow,
		}
		if persistErr := a.triage.PersistItem(errorItem, nil); persistErr != nil {
			log.Printf("send message: persist send-failure error: %v", persistErr)
		}
		if completeErr := a.triage.Handle(provider.ProviderEvent{
			Kind:      provider.EventTurnComplete,
			ThreadID:  threadID,
			Meta:      []byte(`{"truncated":true}`),
			Timestamp: time.Now(),
		}); completeErr != nil {
			// Log rather than propagate — the send error we're about to
			// return is the primary failure. A secondary triage hiccup
			// here (e.g. store closed mid-teardown) shouldn't swallow the
			// original send error.
			log.Printf("send message: turn complete after send failure: %v", completeErr)
		}
		return store.Item{}, err
	}

	// Synthesize EventTurnStart only after a successful provider write for
	// providers that don't emit their own turn/started wire notification.
	// Codex emits the authoritative provider-assigned turn_id itself.
	if thread.Provider != "codex" {
		if err := a.triage.Handle(provider.ProviderEvent{
			Kind:      provider.EventTurnStart,
			ThreadID:  threadID,
			TurnIndex: turnIndex,
			Timestamp: time.Now(),
		}); err != nil {
			return store.Item{}, fmt.Errorf("send message: turn start: %w", err)
		}
	}

	a.maybeGenerateThreadTitleWithAttachments(thread, content, hasPriorItems, persistedAttachments)
	return userItem, nil
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

func marshalUserMessageMeta(attachments []store.Attachment, sourcePlan, revisionSourcePlan *SourceProposedPlan, revisionCommentIDs []string) (string, error) {
	if len(attachments) == 0 && sourcePlan == nil && revisionSourcePlan == nil && len(revisionCommentIDs) == 0 {
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
		Attachments:                metaAttachments,
		SourceProposedPlan:         sourcePlan,
		RevisionSourceProposedPlan: revisionSourcePlan,
		RevisionSourceCommentIDs:   revisionCommentIDs,
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	return string(data), nil
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
	prompt := buildPlanRevisionPrompt(comments)
	if strings.TrimSpace(content) == "" {
		return prompt, idsFromProposedPlanComments(comments), nil
	}
	return strings.TrimSpace(content) + "\n\n" + prompt, idsFromProposedPlanComments(comments), nil
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
	return providerSess.Send(context.Background(), content, provider.SendOptions{
		InteractionMode: mode,
		Attachments:     attachments,
	})
}
