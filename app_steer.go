package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/triage"
)

// SteerMessageWithOptions injects a user message into the active Codex
// turn's pending_input queue via Codex's `turn/steer` JSON-RPC. This is
// the mid-turn-injection counterpart to SendMessageWithOptions: when a
// user types while a Codex turn is already running, the frontend routes
// here so the message lands on the wire immediately (drained at the
// next iteration of Codex's run_turn loop) rather than being held in a
// client-side queue until turn/completed.
//
// Codex-only. Claude has no equivalent steering primitive — its
// frontend keeps the existing client-side enqueue path. Calling this on
// a non-Codex thread fails fast.
//
// REQUIRES an active turn — callers should check the active-turn
// registry before calling. If the active turn has just ended (race
// between the frontend reading the registry and this RPC arriving), a
// codex.ErrNoActiveTurn surfaces and the frontend falls back to the
// queue + drain path.
//
// Wire shape and server semantics live in
// codex-rs/app-server-protocol/src/protocol/v2.rs (TurnSteerParams) and
// codex-rs/core/src/session/mod.rs:2983 — Codex appends to the active
// turn's pending_input vec and emits an `item/completed userMessage`
// inside the same turn so triage handleUserText correlates the wire
// echo with the pending-send marker we register here.
func (a *App) SteerMessageWithOptions(threadID string, content string, opts SendMessageOptions) (store.Thread, error) {
	if a.shuttingDown.Load() {
		return store.Thread{}, ErrShuttingDown
	}
	if _, err := a.steerMessageWithOptions(threadID, content, sendMessageOptions{
		AttachmentIDs:                opts.AttachmentIDs,
		RuntimeMode:                  opts.RuntimeMode,
		SourceProposedPlan:           opts.SourceProposedPlan,
		RevisionSourceProposedPlan:   opts.RevisionSourceProposedPlan,
		RevisionSourceCommentIDs:     opts.RevisionSourceCommentIDs,
		RevisionSourceDiffReview:     opts.RevisionSourceDiffReview,
		RevisionSourceDiffCommentIDs: opts.RevisionSourceDiffCommentIDs,
	}); err != nil {
		return store.Thread{}, err
	}
	return a.store.GetThread(threadID)
}

// steerMessageWithOptions is the unexported implementation. Mirrors
// sendMessageWithOptions's validation and persistence path but skips
// lazy session start (Steer requires a live session by definition),
// implement-mode switch (mid-turn — there's no fresh turn to flip the
// mode for), and worktree-rename / thread-title generation (those fire
// on first send, not on mid-turn injection). Persists the user_text
// row optimistically under a steer-suffixed id so it doesn't collide
// with the existing `user:<turnIndex>` row that started the active
// turn.
func (a *App) steerMessageWithOptions(threadID string, content string, opts sendMessageOptions) (item store.Item, err error) {
	if strings.TrimSpace(threadID) == "" {
		return store.Item{}, fmt.Errorf("steer message: empty thread id")
	}

	// Per-thread critical section: matches the send-side action lock so a
	// concurrent Send and Steer on the same thread can't interleave
	// pending-send registration / wire dispatch ordering.
	unlock := a.threadLocks().Lock(threadID)
	defer unlock()

	runtimeMode, hasRuntimeMode, err := threadmode.ParseOptionalRuntime(opts.RuntimeMode)
	if err != nil {
		return store.Item{}, fmt.Errorf("steer message: %w", err)
	}
	if hasRuntimeMode {
		if err := a.applyRuntimeModeLocked(threadID, runtimeMode); err != nil {
			return store.Item{}, fmt.Errorf("steer message: runtime mode: %w", err)
		}
	}

	// Resolve plan refs the same way send does so traceability metadata
	// stays accurate when a steer carries plan-revision context.
	resolved, err := a.resolveUserMessageEnvelope(threadID, content, userMessageInputs{
		attachmentIDs:                opts.AttachmentIDs,
		sourceProposedPlan:           opts.SourceProposedPlan,
		revisionSourceProposedPlan:   opts.RevisionSourceProposedPlan,
		revisionSourceCommentIDs:     opts.RevisionSourceCommentIDs,
		revisionSourceDiffReview:     opts.RevisionSourceDiffReview,
		revisionSourceDiffCommentIDs: opts.RevisionSourceDiffCommentIDs,
	})
	if err != nil {
		return store.Item{}, fmt.Errorf("steer message: %w", err)
	}
	content = resolved.content
	providerAttachments := resolved.providerAttachments
	userMeta := resolved.userMessageMeta

	sess, ok := a.sessionManager().get(threadID)
	if !ok {
		return store.Item{}, fmt.Errorf("steer message: no active session for thread %s", threadID)
	}
	codexSess := sess.codex
	if codexSess == nil {
		return store.Item{}, fmt.Errorf("steer message: not supported for provider %q", sess.provider)
	}

	// Find the in-flight turn so we know which turnIndex to attach the
	// new user_text row to. Steer requires an active turn — if the store
	// has none, the frontend's read of the active-turn registry was
	// stale and we surface the same shape the codex package would have.
	activeTurn, found, err := a.store.GetActiveTurn(threadID)
	if err != nil {
		return store.Item{}, fmt.Errorf("steer message: lookup active turn: %w", err)
	}
	if !found {
		return store.Item{}, codex.ErrNoActiveTurn
	}
	turnIndex := activeTurn.TurnIndex

	if a.triage == nil {
		a.triage = triage.NewRouter(a.store, a.emitWithReplay())
		a.configureTriageQueueCallbacks()
	}

	steerItemID, err := a.nextSteerUserItemID(threadID, turnIndex)
	if err != nil {
		return store.Item{}, fmt.Errorf("steer message: allocate item id: %w", err)
	}

	now := time.Now().UnixMilli()
	userItem := store.Item{
		ID:        steerItemID,
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
	if err = a.triage.PersistItem(userItem, nil); err != nil {
		return store.Item{}, fmt.Errorf("steer message: persist user message: %w", err)
	}
	// Click-time plan/diff-review acceptance. Same sticky semantics as
	// the send path — a downstream Steer failure does NOT revert.
	a.applyProposedPlanAcceptance(threadID, userItem, resolved)

	// Register the pending-send marker BEFORE the steer RPC writes to
	// stdin — the wire `item/completed userMessage` echo can otherwise
	// race ahead of the marker and miss the pending-send-present branch
	// in handleUserText. Cleared on Steer failure below.
	a.triage.RegisterPendingSend(threadID, userItem.ID, turnIndex)

	thread, err := a.store.GetThread(threadID)
	if err != nil {
		a.triage.ClearPendingSendForFailure(threadID, userItem.ID)
		return store.Item{}, fmt.Errorf("steer message: load thread: %w", err)
	}

	// Stamp activity before stdin write so the idle reaper can't race
	// a slow Steer-and-respawn against its own teardown. Mirrors the
	// pre-Send stamp in sendToProvider.
	sess.liveness.bumpActivity(time.Now())
	steerErr := codexSess.Steer(context.Background(), content, provider.SendOptions{
		InteractionMode: provider.NormalizeInteractionMode(thread.Mode),
		Attachments:     providerAttachments,
	})
	if steerErr != nil {
		// Drop the pending-send marker so a stale wire echo from the
		// (now ended) turn can't hijack a subsequent send's correlation.
		a.triage.ClearPendingSendForFailure(threadID, userItem.ID)

		// Persist a sibling error row so the timeline records the
		// failed steer attempt next to the user_text we already
		// optimistically wrote. The provider turn may still be
		// running on the wire — DO NOT emit a synthetic
		// EventTurnComplete here; the active turn settles on its own
		// via the wire `turn/completed` notification.
		errSeq := a.triage.NextErrorSequence(threadID, turnIndex, "")
		errNow := time.Now().UnixMilli()
		errorItem := store.Item{
			ID:        triage.NewErrorID(turnIndex, "", errSeq),
			ThreadID:  threadID,
			TurnIndex: turnIndex,
			Kind:      "error",
			Role:      "system",
			Status:    "completed",
			Summary:   fmt.Sprintf("Failed to steer: %v", steerErr),
			CreatedAt: errNow,
			UpdatedAt: errNow,
		}
		if persistErr := a.triage.PersistItem(errorItem, nil); persistErr != nil {
			log.Printf("steer message: persist steer-failure error: %v", persistErr)
		}
		// Surface the error verbatim, including codex.ErrNoActiveTurn
		// (the frontend uses errors.Is to fall back to enqueue).
		return store.Item{}, steerErr
	}

	return userItem, nil
}

// nextSteerUserItemID is the steer-scope wrapper around
// nextSequencedUserItemID. The original turn-opening user message
// lives at `user:<turnIndex>`; subsequent steers count up from
// `user:<turnIndex>:steer:1` so the ids stay sortable and never
// collide with the seed row.
func (a *App) nextSteerUserItemID(threadID string, turnIndex int) (string, error) {
	return a.nextSequencedUserItemID(threadID, turnIndex, "steer")
}
