package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"agent-overflow/internal/closer"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
)

// ForkThread creates a pointer fork of a source thread and wires the
// provider-specific resume state. The fork copies no history: it reads the
// source's rows before its cut (store.CreatePointerFork). The whole sequence
// is atomic from the caller's point of view: if any step fails, the
// partially-created fork is torn down so no half-forked rows linger.
//
// When atTurnIndex is non-nil, the fork is cut after that turn (0-indexed):
// it does not show turns after *atTurnIndex, and the provider session is
// forked + truncated to match. Message-anchor rows intentionally stay
// behind with the source thread; the fork starts with none (rollback/fork
// helpers synthesize from item meta when a row is absent). atTurnIndex ==
// nil forks at the tail (the whole timeline, provider state at the latest
// message).
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
//ao:route thread
func (a *App) ForkThread(ctx context.Context, sourceThreadID string, atTurnIndex *int) (store.Thread, error) {
	return a.forkThreadAt(ctx, sourceThreadID, atTurnIndex, forkOptions{})
}

// forkOptions overrides what the new fork inherits from its source. The zero
// value is the plain fork the Fork button makes; the thread tools use it to
// cut a hidden read-only scratch fork for a `thread_ask`, and `/side-chat`
// uses the same door.
//
// Only fields a fork may legally differ in are here. Everything else, from
// project and workspace to provider and the provider resume wiring, is the
// source's by definition: a fork that changed them would not be a fork.
// Provider is the hardest of those: a thread is locked to its provider once
// it holds items, which a fork does before its first start.
type forkOptions struct {
	// Mode replaces the source's thread mode. Empty inherits.
	Mode string
	// RuntimeMode replaces the source's permission level. Empty inherits.
	RuntimeMode string
	// Title replaces the "<source> (fork)" default. Empty inherits.
	Title string
	// Model replaces the source's model, within the source's provider. The
	// fork has no session yet, so its first start runs the row's model, the
	// same way a model change on an idle thread does. Empty inherits.
	Model string
	// Effort replaces the source's reasoning effort. It travels with Model
	// because the pair is one catalog choice and the threads table checks
	// them together. Empty inherits.
	Effort string
}

// forkThreadTail forks a thread at its tail with the given overrides. It is
// the internal door onto ForkThread's body for callers that are not the Fork
// button: an agent's `thread_ask`, which forks into a scratch thread.
func (a *App) forkThreadTail(ctx context.Context, sourceThreadID string, opts forkOptions) (store.Thread, error) {
	return a.forkThreadAt(ctx, sourceThreadID, nil, opts)
}

func (a *App) forkThreadAt(ctx context.Context, sourceThreadID string, atTurnIndex *int, opts forkOptions) (store.Thread, error) {
	timing := startForkTiming(sourceThreadID, "thread")
	defer timing.finish()
	// Hold the source thread's action lock for the duration of the fork so
	// concurrent SendMessage / InterruptAndRevertIfClean / etc. can't move
	// the source's history between the provider cut and the fork's cut.
	// Mirrors the un-send path's thread action lock.
	unlock, err := a.threadLocks().LockCtx(ctx, sourceThreadID)
	if err != nil {
		return store.Thread{}, err
	}
	defer unlock()
	timing.next("validate")
	if err := ctx.Err(); err != nil {
		return store.Thread{}, err
	}
	if err := a.checkForkSource("fork thread", sourceThreadID); err != nil {
		return store.Thread{}, err
	}

	// Forking DURING an active turn is supported: the fork is a snapshot
	// "as if interrupted right now". The SOURCE is never interrupted and
	// never mutated — it keeps streaming under its own session — and the
	// fork's copy of its running rows settles through the standard
	// interrupted treatment (same row shapes as the crash sweep / user
	// interrupt; store.CreatePointerFork). The
	// provider halves differ: Codex issues `thread/fork` with NO
	// lastTurnId (codex then appends the same turn-aborted marker a real
	// interrupt writes, onto the fork's copy only), and Claude PINS the
	// lazy `--fork-session` cut at the live session's canonical leaf —
	// the fork's first start passes `--resume-session-at <leaf>` so the
	// CLI's own fork cuts where the timeline was cut rather than at
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
	source, err := a.forkSource("fork thread", sourceThreadID)
	if err != nil {
		return store.Thread{}, err
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
	// before the fork's cut. Reading the leaf after the cut instead
	// would let a turn complete in between and hand the fork a transcript
	// holding the COMPLETE assistant answer while its timeline shows that
	// answer truncated and flagged " — interrupted": the flag would be a
	// lie about content the fork actually has.
	//
	// Capturing first inverts the skew — the timeline may hold a partial
	// block the transcript lacks — and that is the honest real-interrupt
	// shape. A row flagged interrupted makes no promise that its content
	// reached the provider's transcript; that is exactly what a genuine
	// interrupt leaves behind.
	var midTurnCut *claudeMidTurnCut
	timing.next("flush")
	if live {
		// Streaming text is durable only every 250ms/4KB, so the fork
		// would otherwise carry a stale tail. Flush before cutting.
		if a.triage != nil {
			if err := a.triage.FlushThread(sourceThreadID); err != nil {
				return store.Thread{}, fmt.Errorf("fork thread: flush source stream buffers: %w", err)
			}
		}
	}
	// Tail anchors were normalized above. Pin idle sources too: they may
	// receive another turn before the fork's first send.
	timing.next("checkpoint")
	if atTurnIndex == nil && source.Provider == string(provider.Claude) {
		cut, err := a.captureClaudeMidTurnCut(source)
		if err != nil {
			return store.Thread{}, err
		}
		if !live && cut.degenerate() {
			return store.Thread{}, fmt.Errorf("fork thread: source thread %q has no resumable Claude session", source.ID)
		}
		midTurnCut = &cut
	}

	timing.next("origin")
	fork := store.BuildForkedThread(source)
	fork.ForkPreparing = true
	// Overrides before the creation stamp and the row write, so the thread
	// exists exactly once in its final shape: a scratch fork that was listed
	// as an ordinary thread first and corrected after would show up in every
	// attached sidebar for the width of that window.
	if opts.Mode != "" {
		fork.Mode = opts.Mode
	}
	if opts.RuntimeMode != "" {
		fork.RuntimeMode = opts.RuntimeMode
	}
	if opts.Title != "" {
		fork.Title = opts.Title
	}
	if opts.Model != "" {
		fork.Model = opts.Model
	}
	if opts.Effort != "" {
		fork.ReasoningEffort = opts.Effort
	}
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

	timing.next("create")
	if err := a.threadApplication().CreatePointerFork(fork, source.ID, store.ForkCut{ThroughTurn: atTurnIndex}); err != nil {
		return store.Thread{}, forkRefusal(fmt.Errorf("fork thread: create fork thread: %w", err))
	}
	cleanups.Add(func() error { return a.cleanupForkThread(fork.ID) })
	a.broadcastThreadRow(triage.ThreadActionListed, fork)

	if err := ctx.Err(); err != nil {
		return store.Thread{}, errors.Join(err, cleanups.Run())
	}
	timing.next("provider")
	resume, err := a.resolveForkResumeState(ctx, source, atTurnIndex, midTurnCut)
	if err != nil {
		return store.Thread{}, errors.Join(err, cleanups.Run())
	}
	if resume.Cleanup != nil {
		cleanups.Add(resume.Cleanup)
	}
	timing.next("publish")
	fork.SessionRef = resume.SessionRef
	fork.PendingForkRef = resume.PendingForkRef
	fork.PendingForkResumeAt = resume.PinnedResumeAt

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

	// Publish readiness only after history and provider identity agree.
	if err := ctx.Err(); err != nil {
		return store.Thread{}, errors.Join(err, cleanups.Run())
	}
	ready, err := a.store.FinishForkPreparation(fork.ID)
	if err != nil {
		return store.Thread{}, errors.Join(err, cleanups.Run())
	}
	a.broadcastThreadRow(triage.ThreadActionListed, ready)
	return ready, nil
}

// ForkThreadFromMessage creates a fork whose conversation stops before the
// selected user message. This is the message-keyed counterpart to revert: the
// selected prompt is not copied into the fork.
//
//ao:scope threads:operate
//ao:route thread
func (a *App) ForkThreadFromMessage(ctx context.Context, sourceThreadID string, userItemID string) (store.Thread, error) {
	timing := startForkTiming(sourceThreadID, "message")
	defer timing.finish()
	unlock, err := a.threadLocks().LockCtx(ctx, sourceThreadID)
	if err != nil {
		return store.Thread{}, err
	}
	defer unlock()
	timing.next("validate")
	if err := ctx.Err(); err != nil {
		return store.Thread{}, err
	}
	if err := a.checkForkSource("fork thread from message", sourceThreadID); err != nil {
		return store.Thread{}, err
	}

	// Forking from a message DURING an active turn is supported, same
	// snapshot semantics as ForkThread. The anchor is a real message, so
	// the cut is always strictly below the in-flight turn on the Codex
	// side (`anchor.TurnIndex - 1`) and lands on rows already on disk on
	// the Claude side — but the anchor turn's inherited PREFIX can still
	// hold running rows (a message queued mid-turn), which the fork
	// settles through the same interrupted treatment.
	//
	// Turn read before thread row read, same freshness ordering as
	// ForkThread (handleInit writes the session ref before inserting the
	// turn row, under no thread action lock).
	_, active, err := a.store.GetActiveTurn(sourceThreadID)
	if err != nil {
		return store.Thread{}, fmt.Errorf("fork thread from message: active turn check: %w", err)
	}
	source, err := a.forkSource("fork thread from message", sourceThreadID)
	if err != nil {
		return store.Thread{}, err
	}
	// Same widened liveness as ForkThread: a registered session can
	// stream (background-task re-invocations) with the turn row closed.
	live := active
	if !live {
		_, live = a.activeClaudeSession(sourceThreadID)
	}
	timing.next("flush")
	if live && a.triage != nil {
		// Streaming text is durable only every 250ms/4KB — flush so the
		// fork carries the freshest tail (mirrors ForkThread).
		if err := a.triage.FlushThread(sourceThreadID); err != nil {
			return store.Thread{}, fmt.Errorf("fork thread from message: flush source stream buffers: %w", err)
		}
	}

	timing.next("anchor")
	item, found, err := a.store.GetThreadItem(sourceThreadID, userItemID)
	if err != nil {
		return store.Thread{}, fmt.Errorf("fork thread from message: load user item: %w", err)
	}
	if !found || item.Kind != "user_text" || item.Role != "user" || store.IsWireOnlyUserItem(item) {
		return store.Thread{}, fmt.Errorf("fork thread from message: %q is not a user message", userItemID)
	}

	// The fork's cut sits at the item's position and the provider cut
	// derives from the anchor; resolveMessageAnchor guarantees the two
	// agree by synthesizing from the item row when the persisted anchor
	// is missing or its turn index drifted. Same contract as the un-send
	// path (InterruptAndRevertIfClean).
	anchor := a.resolveMessageAnchor("fork thread from message", sourceThreadID, item)

	timing.next("origin")
	fork := store.BuildForkedThread(source)
	fork.ForkPreparing = true
	a.stampThreadCreation(ctx, &fork)
	if _, err := usermessage.FromItem(item); err != nil {
		return store.Thread{}, fmt.Errorf("fork thread from message: build prompt draft: %w", err)
	}
	promptDraftUpdatedAt := time.Now().UnixMilli()

	// Same mid-build startability guard as ForkThread (round-7, R7-8).
	unlockFork := a.threadLocks().Lock(fork.ID)
	defer unlockFork()

	var cleanups closer.Stack

	// The fork's cut granularity must match the provider's fork cut
	// (mirrors rollbackConversationLocked): Codex thread/fork cuts at a turn
	// boundary, so the fork drops the whole anchor turn (a first-turn anchor
	// keeps nothing); Claude's session slice cuts at the message itself, so
	// the fork keeps the anchor turn's provider-order prefix (queued flush
	// messages can share a turn with the prompt that was running when they
	// were enqueued).
	cut := store.ForkCut{BeforeItemID: userItemID}
	if source.Provider == string(provider.Codex) {
		lastKeptTurn := item.TurnIndex - 1
		cut = store.ForkCut{ThroughTurn: &lastKeptTurn}
	}
	timing.next("create")
	if err := a.threadApplication().CreatePointerFork(fork, source.ID, cut); err != nil {
		return store.Thread{}, forkRefusal(fmt.Errorf("fork thread from message: create fork thread: %w", err))
	}
	cleanups.Add(func() error { return a.cleanupForkThread(fork.ID) })
	a.broadcastThreadRow(triage.ThreadActionListed, fork)
	timing.next("attachments")
	promptDraft, err := a.composerDraftFromUserItemWithClonedAttachments(fork.ID, item, promptDraftUpdatedAt)
	if err != nil {
		return store.Thread{}, errors.Join(
			fmt.Errorf("fork thread from message: build prompt draft: %w", err),
			cleanups.Run(),
		)
	}

	if err := ctx.Err(); err != nil {
		return store.Thread{}, errors.Join(err, cleanups.Run())
	}
	timing.next("provider")
	resume, err := a.resolveMessageForkResumeState(ctx, source, anchor, item)
	if err != nil {
		return store.Thread{}, errors.Join(err, cleanups.Run())
	}
	if resume.Cleanup != nil {
		cleanups.Add(resume.Cleanup)
	}
	timing.next("publish")
	fork.SessionRef = resume.SessionRef
	fork.PendingForkRef = resume.PendingForkRef
	fork.PendingForkResumeAt = resume.PinnedResumeAt

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
	if err := ctx.Err(); err != nil {
		return store.Thread{}, errors.Join(err, cleanups.Run())
	}
	ready, err := a.store.FinishForkPreparation(fork.ID)
	if err != nil {
		return store.Thread{}, errors.Join(err, cleanups.Run())
	}
	a.broadcastThreadRow(triage.ThreadActionListed, ready)
	return ready, nil
}

// cleanupForkThread removes the fork row created by a failed fork. The
// FK CASCADE on items.thread_id, thread_drafts.thread_id,
// message_anchors.thread_id, attachment_owners.thread_id and the fork
// lineage tables handles the fork's own rows; DeleteThreadDir clears any
// attachment bytes already written for the fork. Returns nil on success OR when the row was already gone (ErrNoRows is
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
	} else {
		a.broadcastThreadDeleted(threadID)
	}
	return errors.Join(errs...)
}
