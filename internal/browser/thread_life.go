package browser

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
)

// A thread's browser lives as long as the thread: from the first action or
// saved page that needs it until CloseThread (its delete, archive or
// handoff). One threadBrowser is one such lifetime, and it owns the
// thread's browser state: its session, browsing history, pane color and
// download list are its fields, and each tab, in every state (being
// created, live, closing, suspended, adopted from a popup), names it as its
// owner. The Manager's registry (Manager.threads) maps a thread id to its
// current lifetime.
//
// An action binds before its first wait (Bind, BindThread): its Access holds
// the lifetime current then, and everything it does goes through that
// Access, never through a second lookup of the thread id. So an action
// bound to a lifetime reaches that lifetime's tabs and state only.
// CloseThread detaches the lifetime from the registry and retires every tab
// of it in one hold, then tears down what it detached (thread_close.go); a
// bind after that holds a new, empty lifetime. A change an action commits through a detached lifetime is
// refused, in the Manager.mu hold that would commit it, with
// ErrThreadClosed (Access.admitLocked); a read through one finds no tabs.
//
// What is still looked up by thread id is what belongs to whichever
// lifetime is current: a client's read of the thread's state, a state push,
// the pane's presentation of the thread, and the saved copy, which is
// written from the current lifetime only, in persistMu order
// (page_records.go). An engine callback names a page, and the page names
// its lifetime.

// ErrThreadClosed answers an action whose thread's pages were closed (a
// delete, an archive, a handoff) while it ran: it changed nothing after
// the close.
var ErrThreadClosed = errors.New("browser: the thread's pages were closed while the action ran")

// errUnbound refuses a change made with an Access no bind made.
var errUnbound = errors.New("browser: the action is not bound to its thread's browser")

// threadBrowser is one lifetime of a thread's browser. Its fields are
// guarded by m.mu, except detached (written under it, read atomically) and
// downloads (m.downloadsMu).
type threadBrowser struct {
	id ThreadID
	// detached is set when CloseThread took the lifetime out of the
	// registry; it is never cleared.
	detached atomic.Bool
	session  SessionInfo
	// browsing is the closed tabs and recent addresses (companion_tabs.go,
	// recent.go). A forget replaces the object, which voids every claim
	// taken on the old one (tabClaim).
	browsing *browsing
	// paneColor is the newest color the thread's presenting pane reported
	// (SetPaneRect), which a blank page shows as its base (applyPageBase).
	paneColor string
	// switching is set while a jar switch is between its capture and its
	// commit and closed when it ends (jar_switch.go): a structural change to
	// the lifetime's tabs waits for it (awaitStructureLocked).
	switching chan struct{}
	// downloads is the thread's downloads as its pane lists them
	// (downloads_thread.go), nil before the first. Guarded by m.downloadsMu.
	downloads *threadDownloadList
	// provisional is set while the lifetime was begun by a BindThread and
	// nothing has confirmed it yet: no lookup succeeded, no tool call bound
	// it and no saved tab loaded into it. lookups counts the BindThread
	// lookups in flight on it; the last of them to fail takes a provisional
	// lifetime out of the registry. Both guarded by m.mu.
	provisional bool
	lookups     int
}

func newThreadBrowser(id ThreadID) *threadBrowser {
	return &threadBrowser{
		id:       id,
		session:  SessionInfo{ViewportW: defaultViewportWidth, ViewportH: defaultViewportHeight, Jar: JarWorktree},
		browsing: &browsing{},
	}
}

// threadLocked answers the thread's current lifetime, beginning one when it
// has none. The caller holds m.mu.
func (m *Manager) threadLocked(threadID ThreadID) *threadBrowser {
	if tb := m.threads[threadID]; tb != nil {
		// A tool call or a saved tab holds it: no failed lookup drops it.
		tb.provisional = false
		return tb
	}
	if m.threads == nil {
		m.threads = make(map[ThreadID]*threadBrowser)
	}
	tb := newThreadBrowser(threadID)
	m.threads[threadID] = tb
	return tb
}

// currentLocked answers the thread's current lifetime, nil when it has
// none. The caller holds m.mu.
func (m *Manager) currentLocked(threadID ThreadID) *threadBrowser {
	return m.threads[threadID]
}

// currentSessionLocked is the session of the thread's current lifetime, the
// default one when it has none, for what shows the thread as it is now: its
// pane's presentation and its state. The caller holds m.mu.
func (m *Manager) currentSessionLocked(threadID ThreadID) SessionInfo {
	if tb := m.threads[threadID]; tb != nil {
		return tb.session
	}
	return newThreadBrowser(threadID).session
}

// detachLocked takes the thread's current lifetime out of the registry and
// answers it, nil for none; the next bind begins a new one. The caller holds
// m.mu.
func (m *Manager) detachLocked(threadID ThreadID) *threadBrowser {
	tb := m.threads[threadID]
	if tb == nil {
		return nil
	}
	delete(m.threads, threadID)
	tb.detached.Store(true)
	return tb
}

// lockStructure takes m.mu at a moment no jar switch of tb is between its
// capture and its commit (jar_switch.go), and answers with it held: a
// structural change to tb's tabs (one added, closed, moved, relabeled,
// selected, suspended, restored, crashed or adopted) made in that hold is
// one the switch's captured tabs already hold or one made after its
// commit. A switch is one write of the saved copy long. ctx ends the wait,
// answering its error with m.mu not held. A nil tb waits for nothing.
func (m *Manager) lockStructure(ctx context.Context, tb *threadBrowser) error {
	for {
		m.mu.Lock()
		if tb == nil || tb.switching == nil {
			return nil
		}
		switching := tb.switching
		m.mu.Unlock()
		select {
		case <-switching:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Access is one action's Caller bound to its thread's browser lifetime.
// Only a bind (Bind, BindThread) makes one, and its fields are unexported:
// which thread it acts for is its lifetime's (ThreadID), so no assignment
// and no copy can point it at another thread. Its zero value is bound to no
// lifetime: every entry point refuses it with errUnbound.
type Access struct {
	// caller is the action's workspace and project; its ThreadID is never
	// read (Caller).
	caller Caller
	tb     *threadBrowser
}

// ThreadID is the thread a's lifetime belongs to, "" for the zero Access.
func (a Access) ThreadID() ThreadID {
	if a.tb == nil {
		return ""
	}
	return a.tb.id
}

// Workspace is a's caller's workspace.
func (a Access) Workspace() string { return a.caller.Workspace }

// ProjectID is a's caller's project.
func (a Access) ProjectID() string { return a.caller.ProjectID }

// Caller is the caller a, without its lifetime, naming its lifetime's
// thread.
func (a Access) Caller() Caller {
	c := a.caller
	c.ThreadID = a.ThreadID()
	return c
}

// bound refuses the zero Access, which no bind made.
func (a Access) bound() error {
	if a.tb == nil {
		return errUnbound
	}
	return nil
}

// End answers ErrThreadClosed when the thread's pages were closed while the
// action held a: what it changed before the close, the close closed.
func (a Access) End() error {
	if err := a.bound(); err != nil {
		return err
	}
	if a.tb.detached.Load() {
		return ErrThreadClosed
	}
	return nil
}

// admitLocked answers nil while a change through a may commit: a is bound
// and its lifetime is still the thread's. The caller holds m.mu, which
// CloseThread holds as it detaches the lifetime.
func (a Access) admitLocked() error {
	return a.End()
}

// BindThread binds an action to the thread's current browser lifetime and
// then reads, through lookup, the Caller it acts as: an app action reads the
// thread's row there. Binding first means a close of the thread after that
// read is one the Access sees (ErrThreadClosed). A failed lookup, or one
// naming another thread, answers its error and leaves nothing of the bind:
// a lifetime begun for it, which no lookup confirmed and no tool call holds,
// leaves the registry, so ids of threads that do not exist are not kept.
func (m *Manager) BindThread(threadID ThreadID, lookup func() (Caller, error)) (Access, error) {
	threadID = threadID.trim()
	if threadID == "" {
		return Access{}, errUnbound
	}
	m.mu.Lock()
	tb := m.threads[threadID]
	if tb == nil {
		tb = m.threadLocked(threadID)
		tb.provisional = true
	}
	tb.lookups++
	m.mu.Unlock()
	caller, err := callerOf(threadID, lookup)
	m.mu.Lock()
	defer m.mu.Unlock()
	tb.lookups--
	if err != nil {
		if tb.provisional && tb.lookups == 0 && m.threads[threadID] == tb {
			delete(m.threads, threadID)
			tb.detached.Store(true)
		}
		return Access{}, err
	}
	tb.provisional = false
	return Access{caller: caller, tb: tb}, nil
}

// callerOf reads through lookup the Caller an action bound to threadID acts
// as, and refuses one naming another thread.
func callerOf(threadID ThreadID, lookup func() (Caller, error)) (Caller, error) {
	caller, err := lookup()
	if err != nil {
		return Caller{}, err
	}
	if caller.ThreadID.trim() != threadID {
		return Caller{}, fmt.Errorf("browser: an action bound to thread %s cannot act for thread %s", threadID, caller.ThreadID)
	}
	return caller, nil
}

// Bind binds an action for caller to its thread's current lifetime: an MCP
// tool call at its entry.
func (m *Manager) Bind(caller Caller) Access {
	caller.ThreadID = caller.ThreadID.trim()
	if caller.ThreadID == "" {
		// No thread to bind: the zero Access, which every entry refuses.
		return Access{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return Access{caller: caller, tb: m.threadLocked(caller.ThreadID)}
}
