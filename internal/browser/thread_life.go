package browser

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
// CloseThread detaches the lifetime from the registry in its first hold and
// cleans up what it detached; a bind after that holds a new, empty
// lifetime. A change an action commits through a detached lifetime is
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
	id string
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
}

func newThreadBrowser(id string) *threadBrowser {
	return &threadBrowser{
		id:       id,
		session:  SessionInfo{ViewportW: defaultViewportWidth, ViewportH: defaultViewportHeight, Jar: JarWorktree},
		browsing: &browsing{},
	}
}

// threadLocked answers the thread's current lifetime, beginning one when it
// has none. The caller holds m.mu.
func (m *Manager) threadLocked(threadID string) *threadBrowser {
	if tb := m.threads[threadID]; tb != nil {
		return tb
	}
	if m.threads == nil {
		m.threads = make(map[string]*threadBrowser)
	}
	tb := newThreadBrowser(threadID)
	m.threads[threadID] = tb
	return tb
}

// currentLocked answers the thread's current lifetime, nil when it has
// none. The caller holds m.mu.
func (m *Manager) currentLocked(threadID string) *threadBrowser {
	return m.threads[threadID]
}

// currentSessionLocked is the session of the thread's current lifetime, the
// default one when it has none, for what shows the thread as it is now: its
// pane's presentation and its state. The caller holds m.mu.
func (m *Manager) currentSessionLocked(threadID string) SessionInfo {
	if tb := m.threads[threadID]; tb != nil {
		return tb.session
	}
	return newThreadBrowser(threadID).session
}

// detachLocked takes the thread's current lifetime out of the registry and
// answers it, nil for none; the next bind begins a new one. The caller holds
// m.mu.
func (m *Manager) detachLocked(threadID string) *threadBrowser {
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

// boundCaller is the Caller an Access embeds. The alias keeps the field
// unexported and the pointer keeps its fields out of a composite literal
// (Access{ThreadID: ...} does not compile), so only a bind fills it.
type boundCaller = Caller

// Access is one action's Caller bound to its thread's browser lifetime.
// Only Bind and BindThread make one. Its zero value admits nothing and
// names no caller: it is only an error path's placeholder.
type Access struct {
	*boundCaller
	tb *threadBrowser
}

// Caller is the caller a, without its lifetime.
func (a Access) Caller() Caller {
	if a.boundCaller == nil {
		return Caller{}
	}
	return *a.boundCaller
}

// End answers ErrThreadClosed when the thread's pages were closed while the
// action held a: what it changed before the close, the close closed.
func (a Access) End() error {
	switch {
	case a.tb == nil:
		return errUnbound
	case a.tb.detached.Load():
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

// ThreadBinding is an action's hold on its thread's lifetime before the
// action knows its Caller: an app action binds before it reads the
// thread's row. Its Access binds the Caller to it.
type ThreadBinding struct {
	tb *threadBrowser
}

// BindThread binds an action to the thread's current browser lifetime. A
// caller binds before it reads the thread's row, so a close after that read
// is one the binding sees.
func (m *Manager) BindThread(threadID string) ThreadBinding {
	m.mu.Lock()
	defer m.mu.Unlock()
	return ThreadBinding{tb: m.threadLocked(strings.TrimSpace(threadID))}
}

// Access answers caller bound to b's lifetime. A caller of another thread
// is refused.
func (b ThreadBinding) Access(caller Caller) (Access, error) {
	if b.tb == nil {
		return Access{}, errUnbound
	}
	if b.tb.id != caller.ThreadID {
		return Access{}, fmt.Errorf("browser: an action bound to thread %s cannot act for thread %s", b.tb.id, caller.ThreadID)
	}
	return Access{boundCaller: &caller, tb: b.tb}, nil
}

// End answers as Access.End does.
func (b ThreadBinding) End() error {
	return Access{tb: b.tb}.End()
}

// Bind binds an action for caller to its thread's current lifetime: an MCP
// tool call at its entry.
func (m *Manager) Bind(caller Caller) Access {
	if strings.TrimSpace(caller.ThreadID) == "" {
		// No thread to bind: every change through it is refused.
		return Access{boundCaller: &caller}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return Access{boundCaller: &caller, tb: m.threadLocked(caller.ThreadID)}
}
