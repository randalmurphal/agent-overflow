package browser

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// SetJar sets the cookie jar the thread's pages open in (jar.go) and saves
// the setting with the thread's pages, which is the jar they restore into.
//
// Each live page of the thread is suspended into the new jar: in one m.mu
// hold the thread's jar generation advances and every live page moves to
// closing and is replaced by a suspended record under the same id, tab
// position, label and address, the active tab staying active. That hold
// follows a write of the saved copy holding exactly those records and the new
// jar, and no teardown starts before it, so a shutdown or crash during the
// switch loses no tab, and a failed write changes nothing: the thread keeps
// its jar and its pages, and a retry writes again. The tabs come back through the ordinary
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

	// Each attempt holds persistMu from its write to its commit, so no other
	// write of the thread lands between them, and lets go before the next,
	// so another thread's save never waits for more than one attempt.
	var closes []switchClose
	for {
		m.persistMu.Lock()
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			m.persistMu.Unlock()
			return fmt.Errorf("browser: manager closed")
		}
		if jarKindOf(m.sessionLocked(threadID)) == kind {
			m.mu.Unlock()
			m.persistMu.Unlock()
			return nil
		}
		snap := m.switchSnapshotLocked(threadID, kind)
		m.mu.Unlock()
		if err := m.writeRecordFile(snap.file); err != nil {
			m.persistMu.Unlock()
			return err
		}
		if m.switchWritten != nil {
			m.switchWritten()
		}
		m.mu.Lock()
		// The switch commits the copy it wrote. A tab that came, went,
		// moved or changed between live and suspended during the write is
		// written again; a page's own address, title and use, which it can
		// change at any rate, are the copy's.
		committed := !m.closed && m.switchSnapshotLocked(threadID, kind).sameTabs(snap)
		if committed {
			closes = m.switchJarLocked(threadID, m.sessionLocked(threadID), kind, snap)
		}
		m.mu.Unlock()
		m.persistMu.Unlock()
		if committed {
			break
		}
	}
	for _, c := range closes {
		m.startClose(c.scope, c.close)
	}
	if len(closes) == 0 {
		m.emitThreadState(threadID)
	}
	return nil
}

// switchSnapshot is the saved copy a jar switch writes and then commits:
// the thread's tabs with each live page as the record its suspension
// makes, and which page or record each tab was.
type switchSnapshot struct {
	file pageRecordFile
	tabs []switchTab
}

type switchTab struct {
	live *managedPage
	rec  *suspendedPage
}

// switchSnapshotLocked takes the copy a switch of the thread to kind writes.
// The caller holds m.mu.
func (m *Manager) switchSnapshotLocked(threadID, kind string) switchSnapshot {
	file := m.recordFileLocked(threadID, true)
	file.ActivePageID, file.Jar = m.sessionLocked(threadID).ActivePageID, savedJar(kind)
	snap := switchSnapshot{file: file}
	for _, tab := range m.threadTabsLocked(threadID) {
		snap.tabs = append(snap.tabs, switchTab{live: tab.live, rec: tab.rec})
	}
	return snap
}

// sameTabs reports whether s has the tabs of written: the same pages and
// records in the same places, with the same labels and the same active tab,
// whatever their pages' addresses, titles and use.
func (s switchSnapshot) sameTabs(written switchSnapshot) bool {
	if !slices.Equal(s.tabs, written.tabs) || len(s.file.Pages) != len(written.file.Pages) ||
		s.file.ActivePageID != written.file.ActivePageID || s.file.Jar != written.file.Jar {
		return false
	}
	for i, page := range s.file.Pages {
		was := written.file.Pages[i]
		if page.ID != was.ID || page.Order != was.Order || page.Label != was.Label {
			return false
		}
	}
	return true
}

// switchClose is one page a jar switch closes, in its scope.
type switchClose struct {
	scope *jarScope
	close *pageClose
}

// switchJarLocked commits a switch of the thread to kind: the generation
// advances and each live page becomes the suspended record snap wrote of it
// while it closes. The caller holds m.mu, with snap's tabs the thread's
// (sameTabs), and starts the closes after releasing it.
func (m *Manager) switchJarLocked(threadID string, info SessionInfo, kind string, snap switchSnapshot) []switchClose {
	written := make(map[string]pageRecord, len(snap.file.Pages))
	for _, page := range snap.file.Pages {
		written[page.ID] = page
	}
	info.Jar, info.jarGen, info.privateJar = kind, info.jarGen+1, ""
	info.UpdatedAt = time.Now()
	// The recent addresses and closed tabs were the old jar's browsing; an
	// open still running in it records nothing (recordRecentAddress). The
	// switch's push carries both.
	m.forgetBrowsingLocked(threadID)
	var closes []switchClose
	for _, tab := range m.threadTabsLocked(threadID) {
		old := tab.live
		if old == nil {
			continue
		}
		_, scope := m.livePageLocked(old.id)
		c := m.markClosingLocked(old, scope)
		page := written[old.id]
		m.suspended[old.id] = &suspendedPage{
			id: page.ID, owner: threadID,
			url: page.URL, title: page.Title, label: page.Label,
			tabOrder: page.Order, createdAt: page.Created, lastUse: page.LastUsed,
		}
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
	return closes
}
