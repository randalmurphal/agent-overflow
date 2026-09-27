package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sync"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/threadmode"
)

// A terminal thread (mode terminal) lives as long as its shells. When its
// last terminal exits, this backend deletes it through the ordinary thread
// deletion, whether or not a client is connected, and every client drops
// the row and closes its panes on the `thread:updated` deletion. A restart
// ends every terminal thread the same way (endTerminalThreadsAtBoot). A
// chat thread's drawer terminals are only tabs: their exits end nothing.
//
// StartTerminal writes the row and the pane that shows it opens the first
// shell (OpenTerminal). After that first shell, a terminal thread with no
// terminal left has ended, and OpenTerminal refuses it, so a pane mounting
// while the delete runs cannot give it a new shell. A delete that fails
// leaves the thread ended the same way: every client is told
// (reportTerminalEndFailed), and deleting it from the sidebar retries.

// errTerminalThreadEnded refuses a shell for a terminal thread whose last
// terminal exited. Its delete is under way.
var errTerminalThreadEnded = errorsx.Public("terminal_ended", "This terminal has ended.", nil)

// terminalThreads is the App's record of terminal threads and the deletes
// that end them. Zero value ready.
type terminalThreads struct {
	mu sync.Mutex
	// started holds each terminal thread that has had a shell: its first
	// shell opened in this process, or the last run left it, and its
	// shells ended with that process (endTerminalThreadsAtBoot). An entry
	// lives until the thread's row is deleted.
	started map[string]struct{}
	// stopped refuses new ends once shutdown joins them; wg joins them.
	stopped bool
	wg      sync.WaitGroup
}

func (t *terminalThreads) noteStarted(threadID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.started == nil {
		t.started = make(map[string]struct{})
	}
	t.started[threadID] = struct{}{}
}

func (t *terminalThreads) hasStarted(threadID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.started[threadID]
	return ok
}

func (t *terminalThreads) forget(threadID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.started, threadID)
}

// terminalThreadForOpen reports whether threadID names a terminal thread,
// refusing one that has ended and an id with no thread row. A draft
// placeholder has no row and is not a terminal thread. Call under the
// thread's mutation lock, which OpenTerminal, RestartTerminal and the final
// step of a delete all hold, so the answer cannot change before the open.
func (a *App) terminalThreadForOpen(threadID string) (bool, error) {
	if isDraftPlaceholderThreadID(threadID) {
		return false, nil
	}
	thread, err := a.threadApplication().Get(threadID)
	if err != nil {
		return false, fmt.Errorf("terminal: resolve thread %s: %w", threadID, err)
	}
	if thread.Mode != threadmode.ModeTerminal {
		return false, nil
	}
	if a.terminalThreadEndedLocked(threadID) {
		return true, errTerminalThreadEnded
	}
	return true, nil
}

// terminalThreadEndedLocked reports whether a terminal thread's shells have
// all exited: it had one, and none is left. Call under the thread's
// mutation lock.
func (a *App) terminalThreadEndedLocked(threadID string) bool {
	return a.terminalThreads.hasStarted(threadID) && len(a.terminals.List(threadID)) == 0
}

// endTerminalThreadAfterExit runs, off the PTY's goroutine, the delete of
// the thread whose terminal just exited if that exit ended it. Shutdown
// closes every PTY after it stops these, so a quit ends no thread here;
// the next boot does (endTerminalThreadsAtBoot).
func (a *App) endTerminalThreadAfterExit(threadID string) {
	if a.store == nil || a.terminals == nil || isDraftPlaceholderThreadID(threadID) {
		return
	}
	t := &a.terminalThreads
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return
	}
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		ctx := a.lifeCtx()
		title, err := a.endTerminalThread(ctx, threadID)
		// A failure while shutting down is the next boot's to end.
		if err != nil && ctx.Err() == nil {
			a.reportTerminalEndFailed(threadID, title, err)
		}
	}()
}

// endTerminalThread deletes threadID if it is a terminal thread that has
// ended, under the action lock and ownership check DeleteThread uses, and
// returns its title for a report of a failed delete. The check takes the
// mutation lock, so it never sees the moment inside a RestartTerminal
// between the old shell's exit and the new one's start.
func (a *App) endTerminalThread(ctx context.Context, threadID string) (string, error) {
	unlock, err := a.threadLocks().LockCtx(ctx, threadID)
	if err != nil {
		return "", nil // Shutting down: the next boot ends it.
	}
	defer unlock()
	ended, err := a.terminalThreadEnded(ctx, threadID)
	if err != nil || !ended {
		return "", err
	}
	thread, err := a.threadApplication().Get(threadID)
	if err != nil {
		return "", err
	}
	return thread.Title, a.deleteThreadTreeLocked(threadID)
}

// TerminalEndFailedEvent is the payload of `terminal:end_failed`.
type TerminalEndFailedEvent struct {
	ThreadID string `json:"threadID"`
	// Message is the sentence clients show. The cause stays in the log.
	Message string `json:"message"`
}

// reportTerminalEndFailed tells every client that the delete ending a
// terminal thread failed: no caller waits on it. A delete that failed
// before the thread left every read leaves it listed and ended, so
// OpenTerminal refuses it and deleting it retries. One that failed after
// has already dropped the row from every client (threadapp.DeletePorts
// Deleted), and the next boot finishes it.
func (a *App) reportTerminalEndFailed(threadID, title string, err error) {
	log.Printf("terminal: end thread %s: %v", threadID, err)
	_, readErr := a.threadApplication().Get(threadID)
	name := "A terminal"
	if title != "" {
		name = `The terminal "` + title + `"`
	}
	message := name + " ended and was removed, but part of its cleanup failed."
	if !errors.Is(readErr, sql.ErrNoRows) {
		message = name + " ended, but it could not be removed. Delete it from the sidebar to try again."
	}
	a.emit(eventchan.TerminalEndFailed, TerminalEndFailedEvent{ThreadID: threadID, Message: message})
}

// terminalThreadEnded is terminalThreadEndedLocked under the thread's
// mutation lock. Only a terminal thread is recorded as opened, a thread
// never leaves terminal mode, and a deleted one is forgotten, so the record
// answers for the row.
func (a *App) terminalThreadEnded(ctx context.Context, threadID string) (bool, error) {
	unlock, err := a.threadApplication().LockMutable(ctx, threadID)
	if err != nil {
		return false, err
	}
	defer unlock()
	return a.terminalThreadEndedLocked(threadID), nil
}

// stopTerminalThreadEnds refuses new ends and joins the running ones.
// Shutdown cancels the app context first, so an end still waiting for a
// thread lock returns.
func (a *App) stopTerminalThreadEnds() {
	t := &a.terminalThreads
	t.mu.Lock()
	t.stopped = true
	t.mu.Unlock()
	t.wg.Wait()
}

// endTerminalThreadsAtBoot deletes the terminal threads the last run left:
// their shells ended with its process, whether it quit or crashed. It runs
// before any client can list them, through the ordinary deletion. Each is
// recorded as started first, so one whose delete fails stays ended and is
// refused a shell rather than reopened in its folder.
func (a *App) endTerminalThreadsAtBoot() error {
	ids, err := a.store.ListTerminalThreads()
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		a.terminalThreads.noteStarted(id)
		if err := a.DeleteThread(id); err != nil {
			errs = append(errs, fmt.Errorf("end terminal thread %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}
