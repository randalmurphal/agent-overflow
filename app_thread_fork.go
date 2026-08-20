package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/closer"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
)

// ForkThread copies a source thread's timeline into a new fork and wires
// the provider-specific resume state. The whole sequence is atomic from
// the caller's point of view: if any step fails, the partially-created
// fork is torn down so no half-forked rows linger.
//
// When atTurnIndex is non-nil, the fork is sliced at that turn (0-indexed):
// items with turn_index > *atTurnIndex are dropped, the provider session
// is forked + truncated to match. Message-anchor rows intentionally stay
// behind with the source thread; the fork starts with none (rollback/fork
// helpers synthesize from item meta when a row is absent). atTurnIndex ==
// nil preserves the existing fork-at-tail behavior (clone everything,
// fork provider state at the latest message).
//
// The "atomic unit" is emulated in the app layer rather than a single
// SQLite transaction because the fork flow crosses a boundary — it has
// to talk to the Codex provider to fork a live session and can write a
// new Claude session JSONL on disk. Wrapping
// the whole sequence in sql.Tx would hold a DB transaction open across
// a network-speed operation and break the rest of the store's
// single-connection model. Instead, we compose with a best-effort
// rollback: each step that has a side-effect appends an undo to a LIFO
// `cleanups` slice; on later failure the chain runs in reverse order
// and any cleanup errors are joined with the primary error.
func (a *App) ForkThread(sourceThreadID string, atTurnIndex *int) (store.Thread, error) {
	// Hold the source thread's action lock for the duration of the fork so
	// concurrent SendMessage / InterruptAndRevertIfClean / etc. can't write
	// to items mid-clone (would produce a torn snapshot in the new fork).
	// Mirrors the un-send path's thread action lock.
	unlock := a.threadLocks().Lock(sourceThreadID)
	defer unlock()

	// Forking DURING an active turn is supported: the fork is a snapshot
	// "as if interrupted right now". The SOURCE is never interrupted and
	// never mutated — it keeps streaming under its own session — and the
	// fork's clone settles through the standard interrupted treatment
	// below (same row shapes as the crash sweep / user interrupt). The
	// provider halves differ: Codex issues `thread/fork` with NO
	// lastTurnId (codex then appends the same turn-aborted marker a real
	// interrupt writes, onto the fork's copy only), and Claude slices the
	// JSONL eagerly at the live session's canonical leaf rather than
	// deferring to `--fork-session`, which would snapshot at a
	// nondeterministic later time.
	//
	// The turn read runs BEFORE the thread row read, deliberately. Claude
	// session init (triage's handleInit) writes UpdateSessionRef — which
	// also clears pending_fork_session_ref — and only THEN inserts the
	// turn row, holding no thread action lock. Reading the row first
	// could therefore observe active=true alongside a SessionRef from
	// before the session started; worst case a never-started lazy fork
	// would slice the PARENT's transcript while the live tracker's leaf
	// lives in the child's. Reading the turn first makes the row
	// at-least-as-fresh as the observation that a turn is running.
	activeTurn, active, err := a.store.GetActiveTurn(sourceThreadID)
	if err != nil {
		return store.Thread{}, fmt.Errorf("fork thread: active turn check: %w", err)
	}
	source, err := a.store.GetThread(sourceThreadID)
	if err != nil {
		return store.Thread{}, fmt.Errorf("fork thread: %w", err)
	}
	if active && atTurnIndex != nil && *atTurnIndex == activeTurn.TurnIndex {
		// "Keep through the running turn" IS the mid-turn tail fork: the
		// in-flight turn has no boundary below it to cut on, and Codex
		// REJECTS a lastTurnId naming an in-progress turn outright.
		// Normalizing here is what keeps every anchored path strictly
		// below the active turn.
		//
		// ONLY the exact index normalizes. An anchor ABOVE the active
		// turn is out of range and must be refused exactly as it is on an
		// idle thread — mapping it to a tail fork would make the same
		// bad argument succeed or fail depending on whether a turn
		// happened to be running. `LastTurnIndex` is MAX over items ∪
		// turns, so the range check below sees the active turn's index
		// even when that turn has no items yet, which is what lets the
		// exact-match case through and stops everything above it.
		atTurnIndex = nil
	}

	if err := a.ensureThreadCanFork(source, atTurnIndex); err != nil {
		return store.Thread{}, err
	}

	// Everything the Claude mid-turn slice needs is resolved HERE, before
	// the clone — the source path, the leaf, and whether the leaf came
	// from the live tracker. Reading the leaf after the clone instead
	// would let a turn complete in between and hand the fork a transcript
	// holding the COMPLETE assistant answer while its cloned timeline
	// shows that answer truncated and flagged " — interrupted": the flag
	// would be a lie about content the fork actually has.
	//
	// Capturing first inverts the skew — the timeline may hold a partial
	// block the transcript lacks — and that is the honest real-interrupt
	// shape. A row flagged interrupted makes no promise that its content
	// reached the provider's transcript; that is exactly what a genuine
	// interrupt leaves behind.
	var midTurnCut *claudeMidTurnCut
	if active {
		// Streaming text is durable only every 250ms/4KB, so the clone
		// would otherwise carry a stale tail. Flush before reading.
		if a.triage != nil {
			if err := a.triage.FlushThread(sourceThreadID); err != nil {
				log.Printf("fork thread: flush source stream buffers for %s: %v", sourceThreadID, err)
			}
		}
		// Mid-turn, an ANCHORED fork can never resolve to a tail fork
		// (LastTurnIndex is at least the active turn's index and the
		// anchor is strictly below it), so nil-after-normalization is
		// exactly the set that takes the live-leaf path.
		if atTurnIndex == nil && source.Provider == string(provider.Claude) {
			cut, err := a.captureClaudeMidTurnCut(source)
			if err != nil {
				return store.Thread{}, err
			}
			midTurnCut = &cut
		}
	}

	fork := store.BuildForkedThread(source)

	// The source lock alone leaves the FORK startable mid-build: a
	// client listing threads right after CreateThread commits can start
	// a session on the half-built fork, which the final UpdateThread
	// below would then clobber (round-7, R7-8). The id is freshly
	// minted, so taking its lock before the row exists is uncontended.
	unlockFork := a.threadLocks().Lock(fork.ID)
	defer unlockFork()

	var cleanups closer.Stack

	if err := a.store.CreateThread(fork); err != nil {
		return store.Thread{}, fmt.Errorf("fork thread: create fork thread: %w", err)
	}
	cleanups.Add(func() error { return a.cleanupForkThread(fork.ID) })

	if _, err := a.store.CloneThreadHistoryThroughTurn(source.ID, fork.ID, atTurnIndex); err != nil {
		return store.Thread{}, errors.Join(
			fmt.Errorf("fork thread: clone timeline: %w", err),
			cleanups.Run(),
		)
	}
	if err := a.settleForkAsInterrupted(fork.ID); err != nil {
		return store.Thread{}, errors.Join(err, cleanups.Run())
	}

	sessionRef, pendingForkRef, uuidMap, providerCleanup, err := a.resolveForkResumeState(source, atTurnIndex, midTurnCut)
	if err != nil {
		return store.Thread{}, errors.Join(err, cleanups.Run())
	}
	if providerCleanup != nil {
		cleanups.Add(providerCleanup)
	}
	fork.SessionRef = sessionRef
	fork.PendingForkRef = pendingForkRef
	fork.UpdatedAt = time.Now().UnixMilli()
	if err := a.remapClaudeProviderIDs(fork.ID, uuidMap); err != nil {
		return store.Thread{}, errors.Join(err, cleanups.Run())
	}

	if err := a.store.UpdateThread(fork); err != nil {
		return store.Thread{}, errors.Join(
			fmt.Errorf("fork thread: persist fork state: %w", err),
			cleanups.Run(),
		)
	}

	return fork, nil
}

// ForkThreadFromMessage creates a fork whose conversation stops before the
// selected user message. This is the message-keyed counterpart to revert: the
// selected prompt is not copied into the fork.
func (a *App) ForkThreadFromMessage(sourceThreadID string, userItemID string) (store.Thread, error) {
	unlock := a.threadLocks().Lock(sourceThreadID)
	defer unlock()

	// Forking from a message DURING an active turn is supported, same
	// snapshot semantics as ForkThread. The anchor is a real message, so
	// the cut is always strictly below the in-flight turn on the Codex
	// side (`anchor.TurnIndex - 1`) and lands on rows already on disk on
	// the Claude side — but the anchor turn's cloned PREFIX can still
	// hold running rows (a message queued mid-turn), so the fork settles
	// through the same interrupted treatment below.
	//
	// Turn read before thread row read, same freshness ordering as
	// ForkThread (handleInit writes the session ref before inserting the
	// turn row, under no thread action lock).
	_, active, err := a.store.GetActiveTurn(sourceThreadID)
	if err != nil {
		return store.Thread{}, fmt.Errorf("fork thread from message: active turn check: %w", err)
	}
	source, err := a.store.GetThread(sourceThreadID)
	if err != nil {
		return store.Thread{}, fmt.Errorf("fork thread from message: %w", err)
	}
	if active && a.triage != nil {
		// Streaming text is durable only every 250ms/4KB — flush so the
		// clone carries the freshest tail (mirrors ForkThread).
		if err := a.triage.FlushThread(sourceThreadID); err != nil {
			log.Printf("fork thread from message: flush source stream buffers for %s: %v", sourceThreadID, err)
		}
	}

	item, found, err := a.store.GetThreadItem(sourceThreadID, userItemID)
	if err != nil {
		return store.Thread{}, fmt.Errorf("fork thread from message: load user item: %w", err)
	}
	if !found || item.Kind != "user_text" || item.Role != "user" || store.IsWireOnlyUserItem(item) {
		return store.Thread{}, fmt.Errorf("fork thread from message: %q is not a user message", userItemID)
	}

	// The SQLite clone cuts at the item's position and the provider cut
	// derives from the anchor; resolveMessageAnchor guarantees the two
	// agree by synthesizing from the item row when the persisted anchor
	// is missing or its turn index drifted. Same contract as the un-send
	// path (InterruptAndRevertIfClean).
	anchor := a.resolveMessageAnchor("fork thread from message", sourceThreadID, item)

	fork := store.BuildForkedThread(source)
	if _, err := usermessage.FromItem(item); err != nil {
		return store.Thread{}, fmt.Errorf("fork thread from message: build prompt draft: %w", err)
	}
	promptDraftUpdatedAt := time.Now().UnixMilli()

	// Same mid-build startability guard as ForkThread (round-7, R7-8).
	unlockFork := a.threadLocks().Lock(fork.ID)
	defer unlockFork()

	var cleanups closer.Stack

	if err := a.store.CreateThread(fork); err != nil {
		return store.Thread{}, fmt.Errorf("fork thread from message: create fork thread: %w", err)
	}
	cleanups.Add(func() error { return a.cleanupForkThread(fork.ID) })
	promptDraft, err := a.composerDraftFromUserItemWithClonedAttachments(fork.ID, item, promptDraftUpdatedAt)
	if err != nil {
		return store.Thread{}, errors.Join(
			fmt.Errorf("fork thread from message: build prompt draft: %w", err),
			cleanups.Run(),
		)
	}

	// SQLite truncation granularity must match the provider's fork cut
	// (mirrors rollbackConversationLocked): Codex thread/fork cuts at a turn
	// boundary, so the clone drops the whole anchor turn; Claude's session
	// slice cuts at the message itself, so the clone keeps the anchor
	// turn's provider-order prefix (queued flush messages can share a turn
	// with the prompt that was running when they were enqueued).
	if source.Provider == string(provider.Codex) {
		if item.TurnIndex > 0 {
			lastKeptTurn := item.TurnIndex - 1
			if _, err := a.store.CloneThreadHistoryThroughTurn(source.ID, fork.ID, &lastKeptTurn); err != nil {
				return store.Thread{}, errors.Join(
					fmt.Errorf("fork thread from message: clone timeline: %w", err),
					cleanups.Run(),
				)
			}
		}
	} else {
		if _, err := a.store.CloneThreadHistoryBeforeItem(source.ID, fork.ID, userItemID); err != nil {
			return store.Thread{}, errors.Join(
				fmt.Errorf("fork thread from message: clone timeline: %w", err),
				cleanups.Run(),
			)
		}
	}
	if err := a.settleForkAsInterrupted(fork.ID); err != nil {
		return store.Thread{}, errors.Join(err, cleanups.Run())
	}

	sessionRef, pendingForkRef, uuidMap, providerCleanup, err := a.resolveMessageForkResumeState(source, anchor, item)
	if err != nil {
		return store.Thread{}, errors.Join(err, cleanups.Run())
	}
	if providerCleanup != nil {
		cleanups.Add(providerCleanup)
	}
	fork.SessionRef = sessionRef
	fork.PendingForkRef = pendingForkRef
	fork.UpdatedAt = time.Now().UnixMilli()
	if err := a.remapClaudeProviderIDs(fork.ID, uuidMap); err != nil {
		return store.Thread{}, errors.Join(err, cleanups.Run())
	}

	if err := a.store.UpdateThread(fork); err != nil {
		return store.Thread{}, errors.Join(
			fmt.Errorf("fork thread from message: persist fork state: %w", err),
			cleanups.Run(),
		)
	}
	if err := a.store.UpsertThreadDraft(promptDraft); err != nil {
		return store.Thread{}, errors.Join(
			fmt.Errorf("fork thread from message: restore prompt draft: %w", err),
			cleanups.Run(),
		)
	}
	return fork, nil
}

// cleanupForkThread removes the fork row created by a failed fork. The
// FK CASCADE on items.thread_id, thread_drafts.thread_id,
// message_anchors.thread_id, and attachments.thread_id handles cloned
// rows; DeleteThreadDir clears any attachment bytes already written for the
// fork. Returns nil on success OR when the row was already gone (ErrNoRows is
// treated as idempotent). Any other error is returned so the caller can
// errors.Join it with the primary fork error — swallowing cleanup failures
// lets orphan fork rows accumulate silently.
func (a *App) cleanupForkThread(threadID string) error {
	if threadID == "" {
		return nil
	}
	var errs []error
	if a.attachments != nil {
		if err := a.attachments.DeleteThreadDir(threadID); err != nil {
			errs = append(errs, fmt.Errorf("fork thread cleanup: delete attachment files for %s: %w", threadID, err))
		}
	}
	if err := a.store.DeleteThread(threadID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.Join(errs...)
		}
		errs = append(errs, fmt.Errorf("fork thread cleanup: delete fork %s: %w", threadID, err))
	}
	return errors.Join(errs...)
}

// ensureThreadCanFork rejects forks against threads that have no
// messages or where atTurnIndex points outside the existing turn range.
func (a *App) ensureThreadCanFork(source store.Thread, atTurnIndex *int) error {
	items, err := a.store.ListItems(source.ID)
	if err != nil {
		return fmt.Errorf("fork thread: list source items: %w", err)
	}
	if len(items) == 0 {
		return fmt.Errorf("fork thread: thread %q has no messages and cannot be forked", source.ID)
	}
	if atTurnIndex != nil {
		if *atTurnIndex < 0 {
			return fmt.Errorf("fork thread: atTurnIndex must be >= 0, got %d", *atTurnIndex)
		}
		lastTurn, err := a.store.LastTurnIndex(source.ID)
		if err != nil {
			return fmt.Errorf("fork thread: load source last turn index: %w", err)
		}
		if *atTurnIndex > lastTurn {
			return fmt.Errorf("fork thread: atTurnIndex %d exceeds source last turn %d", *atTurnIndex, lastTurn)
		}
	}
	return nil
}

// resolveForkResumeState wires the provider-specific resume reference for
// the new fork and returns an optional cleanup callback. The cleanup runs
// only if a later step in the fork sequence fails — it is responsible for
// any provider-side artifacts the fork created (e.g. a Claude JSONL slice
// on disk). Codex thread/fork already-spawned forks cannot be deleted via
// JSON-RPC; orphan rollouts are accepted there.
//
// uuidMap is the source-UUID → fork-UUID rewrite produced by the
// inline Claude JSONL slice (nil for Codex, nil for lazy fork-at-tail
// where the actual fork happens at `--fork-session` start time and we
// have no slice yet). When non-nil, the caller must call
// `remapClaudeProviderIDs(fork.ID, uuidMap)` so cloned items'
// `meta.provider_item_id` points at the fork's NEW UUIDs — keeping the
// "stored UUID matches active session JSONL" invariant intact for
// forks-of-forks.
//
// midTurnCut is non-nil exactly for a Claude TAIL fork taken while the
// source has an in-flight turn: it is the transcript cut captured
// BEFORE the clone, so the slice this function writes and the timeline
// already cloned describe the same moment. Codex needs no equivalent —
// `forkCodexThreadAt(source, "")` already sends `thread/fork` with no
// lastTurnId, which is exactly the mid-turn call: codex copies persisted
// history and appends the same turn-aborted marker a real interrupt
// writes, onto the FORK's copy only (ForkSnapshot::Interrupted,
// rust-v0.147.0). The throwaway-resume fallback works mid-turn for the
// same reason — the on-disk rollout ends mid-turn and codex synthesizes
// the marker.
//
// A nil midTurnCut on a Claude tail fork means the source is idle, and
// only then may the lazy `--fork-session` path run. Mid-turn it is
// FORBIDDEN: it defers the actual cut to the fork's first send, which
// would snapshot the source's transcript at a nondeterministic later
// point, quite possibly several turns on. The eager slice at the
// captured leaf is the only cut that means "now".
func (a *App) resolveForkResumeState(source store.Thread, atTurnIndex *int, midTurnCut *claudeMidTurnCut) (
	sessionRef string,
	pendingForkRef string,
	uuidMap map[string]string,
	cleanup func() error,
	err error,
) {
	switch source.Provider {
	case string(provider.Codex):
		ref, err := a.forkCodexThread(source, atTurnIndex)
		if err != nil {
			return "", "", nil, nil, fmt.Errorf("fork thread: fork codex provider state: %w", err)
		}
		return ref, "", nil, nil, nil
	case string(provider.Claude):
		return a.forkClaudeThread(source, atTurnIndex, midTurnCut)
	default:
		return "", "", nil, nil, fmt.Errorf("fork thread: unsupported provider %q", source.Provider)
	}
}

// settleForkAsInterrupted applies the standard interrupted treatment to
// the fork's freshly-cloned rows: running/streaming items flip to
// errored with the " — interrupted" suffix, open turn rows close with
// stop_reason='interrupted'. Same shapes as the boot crash sweep and a
// user interrupt, written at the STORE level rather than through triage
// — the Router has no state for a thread that has never had a session,
// and driving it for a non-live write is the mistake the session
// importer already exists to avoid.
//
// Unconditional: an idle source clones no open rows and the call is a
// no-op. No event is emitted; the fork goes back through the RPC
// response and is rendered fresh.
func (a *App) settleForkAsInterrupted(forkThreadID string) error {
	if err := a.store.SettleForkedThreadAsInterrupted(
		forkThreadID, triage.InterruptedSummary, time.Now().UnixMilli(),
	); err != nil {
		return fmt.Errorf("fork thread: settle fork as interrupted: %w", err)
	}
	return nil
}

func (a *App) resolveMessageForkResumeState(source store.Thread, anchor store.MessageAnchor, anchorItem store.Item) (
	sessionRef string,
	pendingForkRef string,
	uuidMap map[string]string,
	cleanup func() error,
	err error,
) {
	switch source.Provider {
	case string(provider.Codex):
		// Codex forks are turn-granular (thread/fork cuts at a turn
		// boundary), so the anchor's intra-turn position is irrelevant:
		// the whole anchor turn is dropped, matching the turn-granular
		// SQLite clone.
		if anchor.TurnIndex == 0 {
			return "", "", nil, nil, nil
		}
		lastKeptTurn := anchor.TurnIndex - 1
		ref, err := a.forkCodexThread(source, &lastKeptTurn)
		if err != nil {
			return "", "", nil, nil, fmt.Errorf("fork thread from message: fork codex provider state: %w", err)
		}
		return ref, "", nil, nil, nil
	case string(provider.Claude):
		return a.forkClaudeThreadBeforeMessage(source, anchor, anchorItem)
	default:
		return "", "", nil, nil, fmt.Errorf("fork thread from message: unsupported provider %q", source.Provider)
	}
}
