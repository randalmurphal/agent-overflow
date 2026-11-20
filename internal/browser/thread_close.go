package browser

import (
	"context"
	"errors"
	"fmt"
	"log"
)

// CloseThread ends the thread's browser lifetime (thread_life.go): every
// tab it has, in every state, its session, browsing history, pane color and
// download list. One hold detaches the lifetime from the registry and
// retires every tab of it (retireThreadLocked): its live pages move to
// closing, its creations are closed, its suspended pages and their restores
// are dropped, and the order of every pane mount attached to it ends. From
// that hold an action bound to it changes nothing (ErrThreadClosed) and
// finds no tab, a popup it opened is never listed (its opener is no tab),
// and a bind begins a new, empty lifetime. It then saves the thread's copy
// as its current lifetime is, which removes it unless a new lifetime saved
// tabs meanwhile.
//
// The engine teardown that follows belongs to the detached lifetime, not to
// ctx (threadTeardown): ctx bounds only how long the call waits for it, and
// once ctx ends the call answers so while the teardown goes on. A later
// CloseThread of the thread waits for every teardown of it still running,
// and the Manager's Close joins them. A workspace profile that fails to
// dispose once its last page is gone (a browser process that would not
// stop, ephemeral site data left on disk) is in the answer, as ClosePage
// returns it; the profile is no longer registered, so a second close does
// not repeat the failure. A saved copy that cannot be written is logged
// instead: its pages are gone from memory, and the next boot removes the
// saved copy of a deleted or archived thread.
func (m *Manager) CloseThread(ctx context.Context, threadID ThreadID) error {
	// persistMu first, so the detach and the save that follows are one step
	// in the order of the thread's saves: a save of a new lifetime is
	// ordered after this one. A jar switch holds it from its capture to its
	// commit, so no switch of the lifetime is between them here.
	m.persistMu.Lock()
	m.mu.Lock()
	retired, closes := m.retireThreadLocked(threadID)
	teardowns := m.teardownsLocked(threadID)
	m.mu.Unlock()
	if err := m.writeRecordFilesLocked([]ThreadID{threadID}); err != nil {
		log.Printf("browser: close thread %s: %v", threadID, err)
	}
	m.persistMu.Unlock()
	if retired != nil {
		m.emitThreadState(threadID)
		for _, c := range closes {
			m.startClose(c.scope, c.close)
		}
		if !m.goLife(retired.run) {
			retired.run()
		}
	}
	var errs []error
	for _, t := range teardowns {
		select {
		case <-t.done:
			errs = append(errs, t.err)
		case <-ctx.Done():
			return fmt.Errorf("browser: close thread %s: its pages are still closing: %w", threadID, ctx.Err())
		}
	}
	return errors.Join(errs...)
}

// threadTeardown is the engine half of one detached lifetime's close: the
// closes of its pages, and then its download list. It runs once, on its own
// (run), and done closes when it has finished, err written before. The
// Manager keeps it (retiring) until then, so every CloseThread of the thread
// meanwhile waits for it. A creation or a restore the close ended finishes
// on its own: its admission refuses the detached lifetime, so it registers
// nothing, and it reports its own cleanup (reportTeardown).
type threadTeardown struct {
	m      *Manager
	tb     *threadBrowser
	closes []*pageClose
	done   chan struct{}
	err    error
}

// retireThreadLocked detaches the thread's current lifetime and retires
// every tab of it in the caller's hold, answering its teardown, which the
// caller runs, and the closes to start after releasing m.mu; nil for a
// thread with no lifetime. The caller holds persistMu and m.mu.
func (m *Manager) retireThreadLocked(threadID ThreadID) (*threadTeardown, []switchClose) {
	tb := m.detachLocked(threadID)
	if tb == nil {
		return nil, nil
	}
	t := &threadTeardown{m: m, tb: tb, done: make(chan struct{})}
	// The lifetime's browsing goes with it: a cache of the thread's recent
	// addresses reads the forget generation (forgetBrowsingLocked).
	m.recentGen++
	for _, rec := range m.suspended {
		if rec.owner == tb {
			m.dropRecordLocked(rec)
		}
	}
	for _, mount := range m.panes {
		if mount.owner == tb && mount.order != nil {
			mount.order.End()
		}
	}
	var closes []switchClose
	for _, scope := range m.scopes {
		for _, c := range scope.closing {
			if c.p.owner == tb {
				t.closes = append(t.closes, c)
			}
		}
		for _, p := range scope.pages {
			if p.owner == tb {
				c := m.markClosingLocked(p, scope)
				t.closes = append(t.closes, c)
				closes = append(closes, switchClose{scope: scope, close: c})
			}
		}
	}
	for id, pending := range m.pending {
		switch {
		case pending.p.owner != tb:
		case pending.err != "":
			delete(m.pending, id)
		case !pending.closed:
			pending.closed = true
			pending.cancel()
		}
	}
	if m.retiring == nil {
		m.retiring = make(map[*threadBrowser]*threadTeardown)
	}
	m.retiring[tb] = t
	return t, closes
}

// teardownsLocked answers the teardowns of the thread's lifetimes still
// running. The caller holds m.mu.
func (m *Manager) teardownsLocked(threadID ThreadID) []*threadTeardown {
	var out []*threadTeardown
	for tb, t := range m.retiring {
		if tb.id == threadID {
			out = append(out, t)
		}
	}
	return out
}

// run waits for every part of the teardown, forgets the lifetime's
// downloads, and ends it.
func (t *threadTeardown) run() {
	var errs []error
	for _, c := range t.closes {
		<-c.done
		if c.err != nil {
			errs = append(errs, fmt.Errorf("browser: close thread %s: %w", t.tb.id, c.err))
		}
	}
	t.m.forgetThreadDownloads(t.tb)
	t.err = errors.Join(errs...)
	t.m.mu.Lock()
	delete(t.m.retiring, t.tb)
	t.m.mu.Unlock()
	close(t.done)
}
