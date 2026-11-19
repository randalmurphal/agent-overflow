package browser

import (
	"errors"
	"fmt"
	"reflect"
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
// history, sign-ins kept in the old jar) does not come along: SetJar is the
// switch the user confirmed, and the pane sends SetJarIfNoLiveTab until they
// do.
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
	return m.setJar(access, kind, true)
}

// ErrJarSwitchUnconfirmed is SetJarIfNoLiveTab's refusal: the thread has a
// live tab, whose page state the switch would drop.
var ErrJarSwitchUnconfirmed = errors.New("browser: switching the cookie jar closes the open tabs")

// SetJarIfNoLiveTab is SetJar for a switch the user has not confirmed: it
// refuses with ErrJarSwitchUnconfirmed, changing nothing, when the thread
// has a live tab in the m.mu hold that would commit the switch. A tab still
// being created is not live; the switch creates it again in the new jar.
// When the saved copy was already written for the switch, the refusal
// writes it back as the thread is (the old jar) before it answers.
func (m *Manager) SetJarIfNoLiveTab(access Access, kind string) error {
	return m.setJar(access, kind, false)
}

func (m *Manager) setJar(access Access, kind string, confirmed bool) error {
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

	// persistMu is held from the first write to the commit, so no other
	// write of the thread lands between them.
	m.persistMu.Lock()
	var written *pageRecordFile
	var closes []switchClose
	for {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			m.persistMu.Unlock()
			return fmt.Errorf("browser: manager closed")
		}
		info := m.sessionLocked(threadID)
		if jarKindOf(info) == kind {
			m.mu.Unlock()
			m.persistMu.Unlock()
			return nil
		}
		if !confirmed && m.hasLiveTabLocked(threadID) {
			m.mu.Unlock()
			var err error
			if written != nil {
				err = m.writeRecordFilesLocked([]string{threadID})
			}
			m.persistMu.Unlock()
			return errors.Join(ErrJarSwitchUnconfirmed, err)
		}
		file := m.recordFileLocked(threadID, true)
		file.ActivePageID, file.Jar = info.ActivePageID, savedJar(kind)
		if written != nil && reflect.DeepEqual(*written, file) {
			closes = m.switchJarLocked(threadID, info, kind)
			m.mu.Unlock()
			break
		}
		m.mu.Unlock()
		// The switch commits only once the saved copy holds exactly the
		// tabs it leaves; a tab that changed during the write is written
		// again.
		if err := m.writeRecordFile(file); err != nil {
			m.persistMu.Unlock()
			return err
		}
		written = &file
		if m.jarSwitchSaved != nil {
			m.jarSwitchSaved()
		}
	}
	m.persistMu.Unlock()
	for _, c := range closes {
		m.startClose(c.scope, c.close)
	}
	if len(closes) == 0 {
		m.emitThreadState(threadID)
	}
	return nil
}

// switchClose is one page a jar switch closes, in its scope.
type switchClose struct {
	scope *jarScope
	close *pageClose
}

// switchJarLocked commits a switch of the thread to kind: the generation
// advances and each live page becomes a suspended record while it closes.
// The caller holds m.mu and starts the closes after releasing it.
func (m *Manager) switchJarLocked(threadID string, info SessionInfo, kind string) []switchClose {
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
	return closes
}

// hasLiveTabLocked reports whether the thread has a tab with a live page, the
// tabs a switch closes (switchJarLocked). The caller holds m.mu.
func (m *Manager) hasLiveTabLocked(threadID string) bool {
	for _, tab := range m.threadTabsLocked(threadID) {
		if tab.live != nil {
			return true
		}
	}
	return false
}
