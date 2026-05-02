package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
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
		AttachmentIDs:              opts.AttachmentIDs,
		RuntimeMode:                opts.RuntimeMode,
		SourceProposedPlan:         opts.SourceProposedPlan,
		RevisionSourceProposedPlan: opts.RevisionSourceProposedPlan,
		RevisionSourceCommentIDs:   opts.RevisionSourceCommentIDs,
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

	providerAttachments, persistedAttachments, err := a.resolveSendMessageAttachments(threadID, opts.AttachmentIDs)
	if err != nil {
		return store.Item{}, fmt.Errorf("steer message: attachments: %w", err)
	}

	// Per-thread critical section: matches the send-side lock so a
	// concurrent Send and Steer on the same thread can't interleave
	// pending-send registration / wire dispatch ordering.
	unlock := sendThreadMuRegistry.lockFor(threadID)
	defer unlock()

	runtimeMode, hasRuntimeMode, err := parseOptionalRuntimeMode(opts.RuntimeMode)
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
	sourcePlan, err := a.resolveSourceProposedPlan(threadID, opts.SourceProposedPlan, true)
	if err != nil {
		return store.Item{}, fmt.Errorf("steer message: source proposed plan: %w", err)
	}
	revisionSourcePlan, err := a.resolveSourceProposedPlan(threadID, opts.RevisionSourceProposedPlan, false)
	if err != nil {
		return store.Item{}, fmt.Errorf("steer message: revision source proposed plan: %w", err)
	}
	if revisionSourcePlan == nil && len(opts.RevisionSourceCommentIDs) > 0 {
		return store.Item{}, fmt.Errorf("steer message: revision comments require a source proposed plan")
	}
	if revisionSourcePlan != nil && len(opts.RevisionSourceCommentIDs) > 0 {
		nextContent, commentIDs, err := a.appendPlanRevisionCommentsToContent(threadID, content, revisionSourcePlan.ItemID, opts.RevisionSourceCommentIDs)
		if err != nil {
			return store.Item{}, fmt.Errorf("steer message: revision comments: %w", err)
		}
		content = nextContent
		opts.RevisionSourceCommentIDs = commentIDs
	}

	userMeta, err := marshalUserMessageMeta(persistedAttachments, sourcePlan, revisionSourcePlan, opts.RevisionSourceCommentIDs)
	if err != nil {
		return store.Item{}, fmt.Errorf("steer message: user meta: %w", err)
	}

	a.mu.Lock()
	sess, ok := a.sessions[threadID]
	a.mu.Unlock()
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

// nextSteerUserItemID returns the next deterministic id for a steer
// user_text row on (threadID, turnIndex). The original turn-opening
// user message lives at `user:<turnIndex>`; subsequent steers count up
// from `user:<turnIndex>:steer:1` so the ids stay sortable and never
// collide with the seed row. Reads existing rows so a session reopen
// sees the right next sequence even after a restart.
func (a *App) nextSteerUserItemID(threadID string, turnIndex int) (string, error) {
	prefix := fmt.Sprintf("user:%d:steer:", turnIndex)
	items, err := a.store.ListItemsForTurn(threadID, turnIndex)
	if err != nil {
		return "", err
	}
	highest := 0
	for _, it := range items {
		if !strings.HasPrefix(it.ID, prefix) {
			continue
		}
		// Parse the trailing integer; ignore unparsable suffixes so a
		// future id-format extension can't crash this counter.
		var n int
		if _, scanErr := fmt.Sscanf(it.ID[len(prefix):], "%d", &n); scanErr != nil {
			continue
		}
		if n > highest {
			highest = n
		}
	}
	return fmt.Sprintf("%s%d", prefix, highest+1), nil
}

// errIsNoActiveTurn reports whether err signals the "active turn ended
// before steer arrived" race. Provided so future call sites can fall
// back to a fresh Send rather than surfacing the steer failure to the
// user. The frontend handles the same check on the wire side via
// substring matching against the JSON-RPC error string, which is the
// only signal that survives the wire encoding.
//
//nolint:unused // currently exercised only via the codex.ErrNoActiveTurn
// path inside Steer; kept exported-internally for future Go-side
// fallback callers and as documentation that the typed sentinel is the
// idiomatic check rather than substring matching.
func errIsNoActiveTurn(err error) bool {
	return errors.Is(err, codex.ErrNoActiveTurn)
}
