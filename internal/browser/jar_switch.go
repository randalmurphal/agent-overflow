package browser

import (
	"context"
	"errors"
	"fmt"
	"log"
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
// follows the one write of the saved copy holding those records and the new
// jar, captured once (jarSwitch), and no teardown starts before it, so a
// shutdown or crash during the switch loses no tab, and a failed write
// changes nothing: the thread keeps its jar and its pages, and a retry
// writes again. A page's own changes during the write (its address, title,
// use) never hold the switch: its record is the one written. The tabs come back through the ordinary
// restore path (resolvePage): the active one when the pane presents it or a
// tool uses it, the others when selected or used. A restore fails as any
// restore does, leaving its tab suspended with the error. Page state (forms,
// history, sign-ins kept in the old jar) does not come along, so the caller
// says whether the user agreed to lose it (JarConsent): a switch with
// JarSwitchIfNoLiveTab is refused with ErrJarSwitchUnconfirmed while the
// thread has a live tab, checked in the hold that captures it, before any
// write, and again in the hold that would commit it.
//
// A switch refused after its write (the consent, the Manager's close, a
// forget, the thread's close) writes the thread's saved copy back as the
// thread is before it answers, so the copy never names a jar the thread is
// not in; when that write fails, the answer says the copy still names the
// new jar.
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
func (m *Manager) SetJar(ctx context.Context, access Access, kind string, consent JarConsent) error {
	if err := consent.valid(); err != nil {
		return err
	}
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

	// persistMu is held from the capture to the commit, so the thread's copy
	// on disk is the switch's from its write until the commit, and no other
	// write orders between them; Manager.mu is held only to capture and to
	// commit, never across the write.
	m.persistMu.Lock()
	defer m.persistMu.Unlock()
	m.mu.Lock()
	if err := m.switchAdmitsLocked(ctx, threadID); err != nil {
		m.mu.Unlock()
		return err
	}
	if jarKindOf(m.sessionLocked(threadID)) == kind {
		m.mu.Unlock()
		return nil
	}
	if err := consent.admitsLocked(m, threadID); err != nil {
		m.mu.Unlock()
		return err
	}
	sw := m.captureJarSwitchLocked(threadID, kind)
	m.mu.Unlock()
	if err := m.writeRecordFile(sw.file); err != nil {
		return err
	}
	if m.switchWritten != nil {
		m.switchWritten()
	}
	m.mu.Lock()
	err := m.switchAdmitsLocked(ctx, threadID)
	if err == nil {
		err = consent.admitsLocked(m, threadID)
	}
	var closes []switchClose
	moved := false
	if err == nil {
		closes, moved = sw.commitLocked(m)
	}
	m.mu.Unlock()
	if err != nil {
		// The copy on disk is the switch's: write the thread back as it is,
		// before a close or a forget waiting on persistMu writes its own.
		if werr := m.writeRecordFilesLocked([]string{threadID}); werr != nil {
			return errors.Join(err, fmt.Errorf("browser: the saved tabs still name the %s cookie jar the switch did not make: %w", kind, werr))
		}
		return err
	}
	for _, c := range closes {
		m.startClose(c.scope, c.close)
	}
	if len(closes) == 0 {
		m.emitThreadState(threadID)
	}
	if moved {
		// A tab came, went or moved during the write: the copy follows the
		// tabs the switch left, as any change of them does.
		if err := m.writeRecordFilesLocked([]string{threadID}); err != nil {
			log.Printf("browser: switch the jar of thread %s: save the tabs that changed during the switch: %v", threadID, err)
		}
	}
	return nil
}

// switchAdmitsLocked refuses a jar switch of the thread once the Manager
// closed, while a forget of every page runs (beginForget), or once the
// lifetime ctx is bound to was retired (BindThread). The caller holds m.mu.
func (m *Manager) switchAdmitsLocked(ctx context.Context, threadID string) error {
	switch {
	case m.closed:
		return errManagerClosed
	case m.forgettingLocked():
		// The switch would record pages the forget closes.
		return errForgetting
	}
	return m.lifeAdmitsLocked(ctx, threadID)
}

// JarConsent says whether the user agreed that a jar switch drops the page
// state of the thread's live tabs. Its zero value is no answer, which
// SetJar refuses, so every caller names one.
type JarConsent int

const (
	jarConsentUnset JarConsent = iota
	// JarSwitchConfirmed is a switch the user confirmed: it closes the
	// live tabs into suspended records in the new jar.
	JarSwitchConfirmed
	// JarSwitchIfNoLiveTab is a switch the user has not confirmed: it is
	// refused with ErrJarSwitchUnconfirmed, changing nothing, while the
	// thread has a live tab.
	JarSwitchIfNoLiveTab
)

// ErrJarSwitchUnconfirmed refuses a JarSwitchIfNoLiveTab switch of a thread
// with a live tab, whose page state the switch would drop.
var ErrJarSwitchUnconfirmed = errors.New("browser: switching the cookie jar closes the open tabs")

// valid refuses a consent no constant names.
func (c JarConsent) valid() error {
	switch c {
	case JarSwitchConfirmed, JarSwitchIfNoLiveTab:
		return nil
	case jarConsentUnset:
		return errors.New("browser: a cookie jar switch names no consent")
	default:
		return fmt.Errorf("browser: unknown cookie jar switch consent %d", int(c))
	}
}

// admitsLocked refuses the switch of the thread when the consent does not
// cover it as the thread is now. The caller holds m.mu.
func (c JarConsent) admitsLocked(m *Manager, threadID string) error {
	switch c {
	case JarSwitchConfirmed:
		return nil
	case JarSwitchIfNoLiveTab:
		if m.hasLiveTabLocked(threadID) {
			return ErrJarSwitchUnconfirmed
		}
		return nil
	case jarConsentUnset:
		return c.valid()
	default:
		return c.valid()
	}
}

// hasLiveTabLocked reports whether the thread has a tab with a live page,
// the tabs a switch closes (commitLocked). A tab still being created is not
// live: the switch creates it again in the new jar. The caller holds m.mu.
func (m *Manager) hasLiveTabLocked(threadID string) bool {
	for _, tab := range m.threadTabsLocked(threadID) {
		if tab.live != nil {
			return true
		}
	}
	return false
}

// jarSwitch is one switch of a thread's jar, captured once: the saved copy
// it writes, holding each live page as the record its suspension makes, and
// those records by page, which its commit installs. Its fields are written
// only by captureJarSwitchLocked.
type jarSwitch struct {
	thread, kind string
	file         pageRecordFile
	records      map[*managedPage]*suspendedPage
}

// captureJarSwitchLocked captures a switch of the thread to kind. The
// caller holds m.mu.
func (m *Manager) captureJarSwitchLocked(threadID, kind string) *jarSwitch {
	sw := &jarSwitch{thread: threadID, kind: kind, records: make(map[*managedPage]*suspendedPage)}
	sw.file = pageRecordFile{Version: pageRecordVersion, ThreadID: threadID, ActivePageID: m.sessionLocked(threadID).ActivePageID, Jar: savedJar(kind)}
	for _, tab := range m.threadTabsLocked(threadID) {
		rec := tab.rec
		if tab.live != nil {
			rec = recordOf(tab.live, tab.live.cachedInfo())
			sw.records[tab.live] = rec
		}
		sw.file.Pages = append(sw.file.Pages, rec.saved())
	}
	return sw
}

// errForgetting refuses a jar switch while the browser's pages are being
// closed and forgotten (a clear, the browser turned off).
var errForgetting = errors.New("browser: the browser's pages are being cleared; try again")

// errManagerClosed refuses a change once the Manager closed.
var errManagerClosed = errors.New("browser: manager closed")

// switchClose is one page a jar switch closes, in its scope.
type switchClose struct {
	scope *jarScope
	close *pageClose
}

// commitLocked commits the switch: the generation advances and each live
// page becomes, while it closes, the record the switch captured of it, its
// address, title and use as written, with its place and label as they are
// now. It answers the closes for the caller to start after releasing m.mu,
// and whether the tabs moved during the write (a tab came, went, changed
// place or label, or became live or suspended), so the copy on disk is not
// the tabs it leaves. The caller holds m.mu.
func (sw *jarSwitch) commitLocked(m *Manager) ([]switchClose, bool) {
	info := m.sessionLocked(sw.thread)
	info.Jar, info.jarGen, info.privateJar = sw.kind, info.jarGen+1, ""
	info.UpdatedAt = time.Now()
	// The recent addresses and closed tabs were the old jar's browsing; an
	// open still running in it records nothing (recordRecentAddress). The
	// switch's push carries both.
	m.forgetBrowsingLocked(sw.thread)
	var closes []switchClose
	var left []pageRecord
	for _, tab := range m.threadTabsLocked(sw.thread) {
		rec := tab.rec
		if old := tab.live; old != nil {
			captured := sw.records[old]
			rec = recordOf(old, old.cachedInfo())
			if captured != nil {
				rec.url, rec.title, rec.lastUse = captured.url, captured.title, captured.lastUse
			}
			_, scope := m.livePageLocked(old.id)
			c := m.markClosingLocked(old, scope)
			m.suspended[old.id] = rec
			closes = append(closes, switchClose{scope: scope, close: c})
		}
		left = append(left, rec.saved())
	}
	moved := !slices.Equal(left, sw.file.Pages) || info.ActivePageID != sw.file.ActivePageID
	var teardown []<-chan struct{}
	for _, scope := range m.scopes {
		for _, c := range scope.closing {
			if c.p.owner == sw.thread {
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
	m.sessions[sw.thread] = info
	return closes, moved
}
