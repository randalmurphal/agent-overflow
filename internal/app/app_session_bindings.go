package app

import (
	"context"
	"log"
	"time"

	"agent-overflow/internal/chatmodel"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// markThreadReadTimeout bounds a read-state stamp's wait for the single
// writer connection. It matches the busy_timeout PRAGMA on purpose: past
// five seconds the writer isn't contended, it's wedged, and waiting
// longer only parks a goroutine that shutdown then has to join.
const markThreadReadTimeout = 5 * time.Second

// StartSession is the Wails-bound entry point for "bring this thread's
// provider subprocess up." The sendMessage path also calls
// startSessionNow via runSessionStart when a thread has no active
// session yet.
//
// Takes the per-thread action lock: an unserialized start racing a
// revert can read the pre-revert SessionRef, clear the stopped-thread
// gate mid-revert (MarkThreadActive), and register a session bound to
// the old provider thread after the revert repointed the row. Internal
// callers that already hold the lock (sends, deferred config restarts)
// go through a.startSession / runSessionStart directly.
//
//ao:scope threads:operate
func (a *App) StartSession(threadID string) error {
	return a.startSessionTakingLock(context.Background(), threadID)
}

// StopSession tears down the thread's provider session. Idempotent: a thread
// with no active session still runs triage cleanup so stale state doesn't leak.
//
//ao:scope threads:operate
func (a *App) StopSession(threadID string) error {
	sess, _ := a.sessionManager().take(threadID)
	// teardownAndCloseSession tolerates the zero-value session — when
	// no entry was registered, closeProviderSession returns nil and
	// only triage state is reset.
	return a.teardownAndCloseSession(threadID, sess)
}

// SendMessage is the Wails-bound compatibility entry point for user-typed
// content. The options-aware path below owns newer composer controls.
//
//ao:scope threads:operate
func (a *App) SendMessage(threadID string, content string, attachmentIDs []string) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	_, err := a.sendMessageWithOptions(context.Background(), threadID, content, sendMessageOptions{
		AttachmentIDs: attachmentIDs,
		// Wire entry: this text was typed into a composer (D31). The internal
		// a.sendMessage helper deliberately does NOT set this — its other
		// caller (discussion drive) sends app-composed prompts.
		ExpandComposerCommands: true,
	})
	return err
}

// SendMessageWithOptions applies send-time composer settings and dispatches the
// user turn. RuntimeMode is staged in the composer and persisted here, under
// the same per-thread action lock as provider session start/send.
//
//ao:scope threads:operate
func (a *App) SendMessageWithOptions(ctx context.Context, threadID string, content string, opts SendMessageOptions) (store.Thread, error) {
	if a.shuttingDown.Load() {
		return store.Thread{}, ErrShuttingDown
	}
	if err := a.requireAutonomy(ctx, opts.RuntimeMode); err != nil {
		return store.Thread{}, err
	}
	// The send itself runs on Background, NOT on ctx. ctx belongs to the
	// caller's connection, and a client that drops mid-send must not cancel
	// a turn the provider has already been told about.
	if _, err := a.sendMessageWithOptions(context.Background(), threadID, content, sendMessageOptions{
		AttachmentIDs:                opts.AttachmentIDs,
		RuntimeMode:                  opts.RuntimeMode,
		SourceProposedPlan:           opts.SourceProposedPlan,
		RevisionSourceProposedPlan:   opts.RevisionSourceProposedPlan,
		RevisionSourceCommentIDs:     opts.RevisionSourceCommentIDs,
		RevisionSourceDiffReview:     opts.RevisionSourceDiffReview,
		RevisionSourceDiffCommentIDs: opts.RevisionSourceDiffCommentIDs,
		// Wire entry: this text was typed into a composer (D31).
		ExpandComposerCommands: true,
	}); err != nil {
		return store.Thread{}, err
	}
	return a.store.GetThread(threadID)
}

// InterruptTurn fires a provider-level interrupt on the thread's active
// session. Returns nil when no session is active — the subprocess is
// already gone, there is nothing to interrupt, and surfacing an error
// banner here would just paper over the more useful "Reconnect" path.
// Other failure modes (the provider's Interrupt call failed, the CLI
// never acked the control_request) DO surface as errors so a wedged
// Claude Code CLI is visible to the user.
//
// Spec: on user interrupt, any streaming items on the current turn are
// flipped to errored with a " — stopped" suffix, and a system `error`
// row with Summary "Stopped by user" is appended. This happens AFTER
// the provider interrupt signal is sent so the UI gets a consistent
// "signal sent, now here's the record" ordering. If the triage
// bookkeeping fails we log — the provider interrupt already fired, so
// the session state is correct even if the timeline marker is missing.
//
//ao:scope threads:operate
func (a *App) InterruptTurn(threadID string) error {
	return a.interruptTurnCtx(context.Background(), threadID)
}

// SwitchThread returns the requested thread. Provider sessions are
// started lazily on first send, not on focus.
//
// The read-state stamp runs OFF this RPC's critical path. It is a write,
// so it queues for the store's single writer connection behind whatever
// that connection is already doing — a retention sweep's delete batch, a
// streaming flush, a checkpoint — and doing it first put the thread load
// the UI is blocked on behind an unrelated write.
//
// Nothing downstream depends on the returned row carrying the new stamp.
// The sidebar badge is the frontend's own state: ChatView patches
// lastReadAt locally the moment the thread is focused and persists it
// through the MarkThreadRead binding on a debounce, and it deliberately
// re-enters that path when a row arrives carrying an OLDER lastReadAt
// than it holds. So a returned row that predates the stamp is a state
// the frontend already handles rather than a stale badge. The write
// itself cannot lose a race either: MarkThreadReadNow never moves
// last_read_at backward, so a late async stamp cannot revert a newer one
// the user's own mark-read landed in the meantime.
//
//ao:scope threads:operate
func (a *App) SwitchThread(threadID string) (store.Thread, error) {
	thread, err := a.loadThreadForFocus(threadID)
	if err != nil {
		return store.Thread{}, err
	}
	a.markThreadFocused(threadID)
	return thread, nil
}

// AutoResumeThread is a no-op retained for wire compatibility. Provider
// sessions are now started lazily on first send (app_send.go) rather than
// eagerly on thread focus. Eager resume was spawning ~240 MB Claude CLI
// processes (plus MCP servers) for every thread the user clicked on,
// accumulating gigabytes of resident memory across a handful of navigations
// before the 30-minute idle reaper could reclaim them.
//
//ao:scope threads:operate
func (a *App) AutoResumeThread(threadID string) error {
	return nil
}

// markThreadFocused stamps the thread's read-state in the background.
// Registration happens synchronously under a.markThreadRead.mu so the
// WaitGroup can never be joined by a stamp that was accepted after
// stopMarkThreadReads decided nothing more would run.
func (a *App) markThreadFocused(threadID string) {
	a.markThreadRead.mu.Lock()
	if a.markThreadRead.stopped {
		a.markThreadRead.mu.Unlock()
		return
	}
	a.markThreadRead.wg.Add(1)
	a.markThreadRead.mu.Unlock()

	go func() {
		defer a.markThreadRead.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), markThreadReadTimeout)
		defer cancel()
		row, changed, err := a.store.MarkThreadReadNow(ctx, threadID)
		if err != nil {
			// Nobody is waiting on this write, so a failure would
			// otherwise be invisible: the sidebar keeps showing the
			// frontend's optimistic read state and the next launch
			// reverts to an unread badge with no explanation.
			log.Printf("app: mark thread %s read: %v", threadID, err)
			return
		}
		// Same broadcast the explicit MarkThreadRead binding makes: focus
		// on one device clears the Completed pill on the others. A focus
		// that found the marker already current changes nothing and stays
		// off the wire, which is the common case.
		a.broadcastThreadRowIfChanged(triage.ThreadActionFull, row, changed)
	}()
}

// waitMarkThreadReads blocks until every read-state stamp already
// registered has landed, WITHOUT refusing later ones. It is the
// observation point for tests that need the durable row.
func (a *App) waitMarkThreadReads() {
	a.markThreadRead.wg.Wait()
}

// stopMarkThreadReads refuses new read-state stamps and waits out the
// in-flight ones. Shutdown calls it before the store closes — each stamp
// writes to SQLite, and markThreadReadTimeout caps how long that wait
// can add to quit latency.
func (a *App) stopMarkThreadReads() {
	a.markThreadRead.mu.Lock()
	a.markThreadRead.stopped = true
	a.markThreadRead.mu.Unlock()
	a.markThreadRead.wg.Wait()
}

func (a *App) loadThreadForFocus(threadID string) (store.Thread, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return store.Thread{}, err
	}
	sanitized := chatmodel.SanitizeThread(thread)
	if chatmodel.SameModelFields(thread, sanitized) {
		return thread, nil
	}
	if err := a.store.UpdateThread(sanitized); err != nil {
		return store.Thread{}, err
	}
	return sanitized, nil
}

// ReconnectSession tears down the current session and starts a fresh one using
// the thread's stored resume cursor.
//
// Two guards compose here, in order:
//
//  1. Single-flight across the stop-then-start pair: a second concurrent
//     caller returns nil without doing any work. Without the gate, a
//     second call's stopSession can yank the new session out from under
//     the first call's in-flight startSession (runSessionStart serialises
//     starts but not stops). This matters for the auto-reconnect path
//     racing a manual click on the banner Reconnect button, and it runs
//     BEFORE the lock so the no-op answer stays immediate.
//  2. The per-thread action lock, so the stop-then-start pair cannot
//     interleave with a revert's stop-and-repoint sequence (a reconnect
//     landing mid-revert would resume the pre-revert cursor and clear the
//     stopped-thread gate). Callers already holding the lock (workspace-
//     change restarts) use reconnectSessionLocked instead.
//
//ao:scope threads:operate
func (a *App) ReconnectSession(threadID string) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	if !a.acquireReconnect(threadID) {
		return nil
	}
	defer a.releaseReconnect(threadID)
	unlock := a.threadLocks().Lock(threadID)
	defer unlock()
	if err := a.stopSession(threadID); err != nil {
		return err
	}
	return a.startSession(context.Background(), threadID)
}

// reconnectSessionLocked is the reconnect body for callers that already
// hold the per-thread action lock. Same single-flight gate as
// ReconnectSession; when an unlocked reconnect holds the gate while
// waiting on this caller's lock, the no-op answer here matches the old
// ReconnectSession-vs-ReconnectSession behavior.
func (a *App) reconnectSessionLocked(ctx context.Context, threadID string) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	if !a.acquireReconnect(threadID) {
		return nil
	}
	defer a.releaseReconnect(threadID)
	// The stop half stays uncancellable on purpose: it is provider IO, and it
	// has already run by the time the start half's join could be abandoned.
	if err := a.stopSession(threadID); err != nil {
		return err
	}
	return a.startSession(ctx, threadID)
}

func (a *App) acquireReconnect(threadID string) bool {
	return a.sessionManager().runtime.BeginReconnect(threadID)
}

func (a *App) releaseReconnect(threadID string) {
	a.sessionManager().runtime.FinishReconnect(threadID)
}

// startSession brings the thread's provider subprocess up, sharing an
// in-flight start with any concurrent caller. ctx bounds only the JOIN — the
// wait for somebody else's start — never the spawn itself; see
// runSessionStart. Callers with no cancellation of their own pass
// context.Background().
func (a *App) startSession(ctx context.Context, threadID string) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	return a.runSessionStart(ctx, threadID, func() error {
		if a.startSessionFn != nil {
			return a.startSessionFn(threadID)
		}
		return a.startSessionNow(threadID)
	})
}

func (a *App) stopSession(threadID string) error {
	if a.stopSessionFn != nil {
		return a.stopSessionFn(threadID)
	}
	return a.StopSession(threadID)
}
