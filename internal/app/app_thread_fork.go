package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"agent-overflow/internal/closer"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
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
//
//ao:scope threads:operate
func (a *App) ForkThread(ctx context.Context, sourceThreadID string, atTurnIndex *int) (store.Thread, error) {
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
	// interrupt writes, onto the fork's copy only), and Claude PINS the
	// lazy `--fork-session` cut at the live session's canonical leaf —
	// the fork's first start passes `--resume-session-at <leaf>` so the
	// CLI's own fork cuts where the timeline was cloned rather than at
	// a nondeterministic later time.
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
	// "Live" is deliberately WIDER than "has an open turn row". The
	// Claude CLI closes a turn (end_turn) and then self-re-invokes when a
	// background task completes — for hours, with no user send and no new
	// turn row — so the transcript can grow whenever the session process
	// is registered, open turn row or not. Keying the snapshot decision on
	// the turn row alone shipped a fork whose transcript was cut 44s after
	// its timeline (incident 2026-08-22: turn row closed 2.6h earlier
	// while the session streamed on). A registered live session is the
	// truth about whether the file can still move.
	live := active
	if !live {
		_, live = a.activeClaudeSession(sourceThreadID)
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
	// A Claude anchor AT the last turn is a tail fork and must be
	// normalized HERE, not inside forkClaudeThread where it historically
	// lived: the mid-turn capture below keys on atTurnIndex == nil, and a
	// live source whose anchored-at-tail fork skipped capture would fall
	// through to the lazy `--fork-session` path — the exact
	// snapshot-at-first-send bug the capture exists to prevent. Only ==
	// is reachable (ensureThreadCanFork already refused anything above).
	// Codex keeps its anchor: it has no lazy path, and the anchored call
	// verifies the fork's tail against the anchor, which nil would skip.
	if atTurnIndex != nil && source.Provider == string(provider.Claude) {
		lastTurn, err := a.store.LastTurnIndex(source.ID)
		if err != nil {
			return store.Thread{}, fmt.Errorf("fork thread: load source last turn index: %w", err)
		}
		if *atTurnIndex >= lastTurn {
			atTurnIndex = nil
		}
	}

	// The Claude mid-turn cut — the source path and the leaf the fork
	// will PIN its lazy `--fork-session` start at — is resolved HERE,
	// before the clone. Reading the leaf after the clone instead
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
	if live {
		// Streaming text is durable only every 250ms/4KB, so the clone
		// would otherwise carry a stale tail. Flush before reading.
		if a.triage != nil {
			if err := a.triage.FlushThread(sourceThreadID); err != nil {
				log.Printf("fork thread: flush source stream buffers for %s: %v", sourceThreadID, err)
			}
		}
		// Every anchor that means "tail" has been normalized to nil by
		// now (the active-turn exact match, then the Claude
		// at-last-turn hoist), so nil-after-normalization is exactly
		// the set that takes the live-leaf path; a surviving anchor
		// cuts strictly below anything still moving.
		if atTurnIndex == nil && source.Provider == string(provider.Claude) {
			cut, err := a.captureClaudeMidTurnCut(source)
			if err != nil {
				return store.Thread{}, err
			}
			midTurnCut = &cut
		}
	}

	fork := store.BuildForkedThread(source)
	// Observed now, not copied from the source: a fork shares the source's
	// workspace, and that workspace has kept moving since the source thread
	// was created. The fork's creation coordinates are where the workspace
	// stands at the fork, which is what a later transfer needs to reproduce.
	a.stampThreadCreation(ctx, &fork)

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

	resume, err := a.resolveForkResumeState(source, atTurnIndex, midTurnCut)
	if err != nil {
		return store.Thread{}, errors.Join(err, cleanups.Run())
	}
	if resume.Cleanup != nil {
		cleanups.Add(resume.Cleanup)
	}
	fork.SessionRef = resume.SessionRef
	fork.PendingForkRef = resume.PendingForkRef
	fork.PendingForkResumeAt = resume.PinnedResumeAt
	if err := a.remapClaudeProviderIDs(fork.ID, resume.UUIDMap); err != nil {
		return store.Thread{}, errors.Join(err, cleanups.Run())
	}

	// The pin is one-shot state no whole-row UpdateThread may carry (see
	// SetThreadForkResume) — and the rest of the fork row was already
	// written by CreateThread above, so the resume wiring is all that is
	// left to persist.
	if err := a.store.SetThreadForkResume(
		fork.ID, fork.SessionRef, fork.PendingForkRef, fork.PendingForkResumeAt,
	); err != nil {
		return store.Thread{}, errors.Join(
			fmt.Errorf("fork thread: persist fork state: %w", err),
			cleanups.Run(),
		)
	}

	// The fork row carries cloned history, so it is sidebar-visible the
	// moment it exists: broadcast it as `listed` so every other attached
	// client shows it beside the source, as the forking client does.
	a.broadcastThreadRow(triage.ThreadActionListed, fork)
	return fork, nil
}

// ForkThreadFromMessage creates a fork whose conversation stops before the
// selected user message. This is the message-keyed counterpart to revert: the
// selected prompt is not copied into the fork.
//
//ao:scope threads:operate
func (a *App) ForkThreadFromMessage(ctx context.Context, sourceThreadID string, userItemID string) (store.Thread, error) {
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
	// Same widened liveness as ForkThread: a registered session can
	// stream (background-task re-invocations) with the turn row closed.
	live := active
	if !live {
		_, live = a.activeClaudeSession(sourceThreadID)
	}
	if live && a.triage != nil {
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
	a.stampThreadCreation(ctx, &fork)
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

	resume, err := a.resolveMessageForkResumeState(source, anchor, item)
	if err != nil {
		return store.Thread{}, errors.Join(err, cleanups.Run())
	}
	if resume.Cleanup != nil {
		cleanups.Add(resume.Cleanup)
	}
	fork.SessionRef = resume.SessionRef
	fork.PendingForkRef = resume.PendingForkRef
	fork.PendingForkResumeAt = resume.PinnedResumeAt
	if err := a.remapClaudeProviderIDs(fork.ID, resume.UUIDMap); err != nil {
		return store.Thread{}, errors.Join(err, cleanups.Run())
	}

	// Same narrow write as ForkThread: CreateThread already wrote the row,
	// and the pin may not ride a whole-row update.
	if err := a.store.SetThreadForkResume(
		fork.ID, fork.SessionRef, fork.PendingForkRef, fork.PendingForkResumeAt,
	); err != nil {
		return store.Thread{}, errors.Join(
			fmt.Errorf("fork thread from message: persist fork state: %w", err),
			cleanups.Run(),
		)
	}
	if err := a.writeThreadDraft(transport.ClientIdentity{}, promptDraft); err != nil {
		return store.Thread{}, errors.Join(
			fmt.Errorf("fork thread from message: restore prompt draft: %w", err),
			cleanups.Run(),
		)
	}
	a.broadcastThreadRow(triage.ThreadActionListed, fork)
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
	return a.threadApplication().EnsureCanFork(source, atTurnIndex)
}

// forkResumeState is the provider resume wiring resolveForkResumeState
// hands back for a new fork:
//
//   - SessionRef: the fork's own provider session id, set when the fork
//     materialized its transcript up front (a Claude anchored slice, a
//     Codex thread/fork child).
//   - PendingForkRef: the SOURCE session id for a lazy Claude fork —
//     the first session start passes `--fork-session --resume <ref>`.
//   - PinnedResumeAt: the transcript cut for a PINNED lazy Claude fork
//     (a tail fork of a live source): the leaf uuid captured when Fork
//     was clicked. The first session start repairs it against the CLI's
//     resume filters and passes `--resume-session-at`, so the CLI's own
//     fork cuts exactly where the timeline was cloned instead of
//     wherever the source has grown to by first send. Empty on an
//     idle-source lazy fork, whose tail IS the cut.
//   - UUIDMap: the source-UUID → fork-UUID rewrite an inline Claude
//     JSONL slice produced (nil for Codex and both lazy shapes). When
//     non-nil the caller must run remapClaudeProviderIDs so cloned
//     items' meta.provider_item_id points at the fork's NEW uuids.
//   - Cleanup: undoes provider-side artifacts (a JSONL slice on disk)
//     when a later fork step fails. Codex thread/fork children cannot
//     be deleted over JSON-RPC; orphan rollouts are accepted there.
type forkResumeState struct {
	SessionRef     string
	PendingForkRef string
	PinnedResumeAt string
	UUIDMap        map[string]string
	Cleanup        func() error
}

// resolveForkResumeState wires the provider-specific resume reference for
// the new fork. See forkResumeState for the field contract.
//
// midTurnCut is non-nil exactly for a Claude TAIL fork taken while the
// source is live: it is the transcript cut captured BEFORE the clone, so
// the pin stored here and the timeline already cloned describe the same
// moment. Codex needs no equivalent — `forkCodexThreadAt(source, "")`
// already sends `thread/fork` with no lastTurnId, which is exactly the
// mid-turn call: codex copies persisted history and appends the same
// turn-aborted marker a real interrupt writes, onto the FORK's copy only
// (ForkSnapshot::Interrupted, rust-v0.147.0). The throwaway-resume
// fallback works mid-turn for the same reason — the on-disk rollout ends
// mid-turn and codex synthesizes the marker.
//
// A nil midTurnCut on a Claude tail fork means the source is idle, and
// only then may the UNPINNED lazy `--fork-session` path run: with
// nothing streaming, the source's tail at first send IS the tail the
// timeline was cloned at. On a live source the cut must be pinned —
// deferring it unpinned snapshots the transcript at a nondeterministic
// later point (the 2026-08-22 44s-skew incident).
func (a *App) resolveForkResumeState(source store.Thread, atTurnIndex *int, midTurnCut *claudeMidTurnCut) (forkResumeState, error) {
	switch source.Provider {
	case string(provider.Codex):
		ref, err := a.forkCodexThread(source, atTurnIndex)
		if err != nil {
			return forkResumeState{}, fmt.Errorf("fork thread: fork codex provider state: %w", err)
		}
		return forkResumeState{SessionRef: ref}, nil
	case string(provider.Claude):
		return a.forkClaudeThread(source, atTurnIndex, midTurnCut)
	default:
		return forkResumeState{}, fmt.Errorf("fork thread: unsupported provider %q", source.Provider)
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
	return a.threadApplication().SettleForkAsInterrupted(forkThreadID)
}

func (a *App) resolveMessageForkResumeState(source store.Thread, anchor store.MessageAnchor, anchorItem store.Item) (forkResumeState, error) {
	switch source.Provider {
	case string(provider.Codex):
		// Codex forks are turn-granular (thread/fork cuts at a turn
		// boundary), so the anchor's intra-turn position is irrelevant:
		// the whole anchor turn is dropped, matching the turn-granular
		// SQLite clone.
		if anchor.TurnIndex == 0 {
			return forkResumeState{}, nil
		}
		lastKeptTurn := anchor.TurnIndex - 1
		ref, err := a.forkCodexThread(source, &lastKeptTurn)
		if err != nil {
			return forkResumeState{}, fmt.Errorf("fork thread from message: fork codex provider state: %w", err)
		}
		return forkResumeState{SessionRef: ref}, nil
	case string(provider.Claude):
		return a.forkClaudeThreadBeforeMessage(source, anchor, anchorItem)
	default:
		return forkResumeState{}, fmt.Errorf("fork thread from message: unsupported provider %q", source.Provider)
	}
}
