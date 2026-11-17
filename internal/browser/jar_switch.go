package browser

import (
	"fmt"
	"strings"
	"time"
)

// SetJar sets the cookie jar the thread's pages open in (jar.go) and saves
// the setting with the thread's pages, which is the jar they restore into.
//
// Each live page of the thread is suspended into the new jar: in one m.mu
// hold the thread's jar generation advances and every live page moves to
// closing and is replaced by a suspended record under the same id, tab
// position, label and address, the active tab staying active. The records are
// written to the saved copy before any teardown starts, so a shutdown or crash
// during the switch loses no tab. The tabs come back through the ordinary
// restore path (resolvePage): the active one when the pane presents it or a
// tool uses it, the others when selected or used. A restore fails as any
// restore does, leaving its tab suspended with the error. Page state (forms,
// history, sign-ins kept in the old jar) does not come along; the pane asks
// before it switches.
//
// The switch's teardown barrier belongs to the thread's jar generation
// (SessionInfo.jarTeardown): every page the thread has closing, this
// switch's or an earlier one's, and every profile being disposed. No restore
// of the thread is admitted until all are done (restoreAdmissionLocked), so
// a restore, of a tab this switch suspended or one suspended before it,
// never meets a cap only those pages hold.
// Until then a closing page and its record are one tab: the record holds the
// thread's slot (threadPageCountLocked), and a closed record leaves the slot
// to its closing page.
//
// The generation fences every admission still in flight: a create or restore
// that began in the old jar lands in the new one or nowhere
// (jarCurrentLocked), and a popup needs a live opener, which the switch has
// closed.
func (m *Manager) SetJar(access Access, kind string) error {
	kind = strings.TrimSpace(kind)
	if !validJarKind(kind) {
		return fmt.Errorf("browser: unknown cookie jar %q", truncateUTF8(kind, 32))
	}
	if strings.TrimSpace(access.ThreadID) == "" || strings.TrimSpace(access.Workspace) == "" {
		return fmt.Errorf("browser: invalid caller scope")
	}
	if kind == JarProject && strings.TrimSpace(access.ProjectID) == "" {
		return fmt.Errorf("browser: this thread has no project to share a cookie jar with")
	}
	threadID := access.ThreadID

	type switchClose struct {
		scope *jarScope
		close *pageClose
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("browser: manager closed")
	}
	info := m.sessionLocked(threadID)
	if jarKindOf(info) == kind {
		m.mu.Unlock()
		return nil
	}
	info.Jar, info.jarGen, info.privateJar = kind, info.jarGen+1, ""
	info.UpdatedAt = time.Now()
	// The recent addresses and closed tabs were the old jar's browsing; an
	// open still running in it records nothing (recordRecentAddress). The
	// push below carries both.
	m.forgetBrowsingLocked(threadID)
	var closes []switchClose
	for _, tab := range m.threadTabsLocked(threadID) {
		old := tab.live
		if old == nil {
			continue
		}
		_, scope := m.livePageLocked(old.id)
		c := markClosingLocked(old, scope)
		rec := recordOf(old, old.cachedInfo())
		m.suspended[old.id] = rec
		closes = append(closes, switchClose{scope: scope, close: c})
	}
	var teardown []<-chan struct{}
	for _, scope := range m.scopes {
		for _, c := range scope.closing {
			if c.p.owner == threadID {
				teardown = append(teardown, c.done)
			}
		}
	}
	// A page whose close has left closing may still be disposing its
	// profile, which holds a profile slot until it is done.
	for _, done := range m.disposing {
		teardown = append(teardown, done)
	}
	info.jarTeardown = teardown
	m.sessions[threadID] = info
	m.mu.Unlock()

	err := m.persistThreads([]string{threadID})
	for _, c := range closes {
		m.startClose(c.scope, c.close)
	}
	if len(closes) == 0 {
		m.emitThreadState(threadID)
	}
	return err
}
