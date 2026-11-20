package browser

import (
	"context"
	"errors"
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
// position, label and address, the active tab staying active. That hold
// follows the one write of the saved copy holding those records and the new
// jar, captured once (jarSwitch), and no teardown starts before it, so a
// shutdown or crash during the switch loses no tab, and a failed write
// changes nothing: the thread keeps its jar and its pages, and a retry
// writes again.
//
// From the capture to the commit the switch holds its lifetime's
// structural gate (threadBrowser.switching): every change to the
// lifetime's tabs (one added, closed, moved, relabeled, selected,
// suspended, restored, crashed or adopted) waits for the commit in
// lockStructure, so the tabs the commit suspends are the tabs it captured
// and wrote, and a tab opened during the write is added after the commit,
// in the new jar. A page's own changes during the write (its address,
// title, use) never wait: its record is the one written. Other threads
// never wait for it.
//
// The tabs come back through the ordinary restore path (resolvePage): the
// active one when the pane presents it or a tool uses it, the others when
// selected or used. A restore fails as any restore does, leaving its tab
// suspended with the error. Page state (forms, history, sign-ins kept in
// the old jar) does not come along, so the caller says whether the user
// agreed to lose it (JarConsent): a switch with JarSwitchIfNoLiveTab is
// refused with ErrJarSwitchUnconfirmed while the thread has a live tab,
// checked in the hold that captures it, before any write; the gate keeps
// that answer until the commit.
//
// A switch refused after its write (the Manager's close) writes the
// thread's saved copy back as the thread is before it answers, so the copy
// never names a jar the thread is not in; when that write fails, the answer
// says the copy still names the new jar. A forget of every page and the
// thread's close wait for the switch's persistMu hold, so neither refuses
// it after its write.
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
	if err := m.bound(access); err != nil {
		return err
	}
	if err := consent.valid(); err != nil {
		return err
	}
	kind = strings.TrimSpace(kind)
	if !validJarKind(kind) {
		return fmt.Errorf("browser: unknown cookie jar %q", truncateUTF8(kind, 32))
	}
	if strings.TrimSpace(access.Workspace()) == "" {
		return fmt.Errorf("browser: invalid caller scope")
	}
	if kind == JarProject && strings.TrimSpace(access.ProjectID()) == "" {
		return fmt.Errorf("browser: this thread has no project to share a cookie jar with")
	}
	tb := access.tb

	// persistMu is held from the capture to the commit, so the thread's copy
	// on disk is the switch's from its write until the commit, and no other
	// write orders between them; Manager.mu is held only to capture and to
	// commit, never across the write. Neither is held to start the closes.
	m.persistMu.Lock()
	persisting := true
	defer func() {
		if persisting {
			m.persistMu.Unlock()
		}
	}()
	if err := m.lockStructure(ctx, tb); err != nil {
		return err
	}
	if err := m.switchAdmitsLocked(access); err != nil {
		m.mu.Unlock()
		return err
	}
	if jarKindOf(tb.session) == kind {
		m.mu.Unlock()
		return nil
	}
	if err := consent.admitsLocked(m, tb); err != nil {
		m.mu.Unlock()
		return err
	}
	sw := m.captureJarSwitchLocked(tb, kind)
	gate := make(chan struct{})
	tb.switching = gate
	m.mu.Unlock()
	// endGate opens the gate; the caller holds m.mu.
	endGate := func() {
		tb.switching = nil
		close(gate)
	}
	if err := m.writeRecordFile(sw.file); err != nil {
		m.mu.Lock()
		endGate()
		m.mu.Unlock()
		return err
	}
	if m.switchWritten != nil {
		m.switchWritten()
	}
	m.mu.Lock()
	err := m.switchAdmitsLocked(access)
	var closes []switchClose
	if err == nil {
		closes = sw.commitLocked(m)
	}
	endGate()
	m.mu.Unlock()
	if err != nil {
		// The copy on disk is the switch's: write the thread back as it is,
		// before a write waiting on persistMu writes its own.
		if werr := m.writeRecordFilesLocked([]ThreadID{access.ThreadID()}); werr != nil {
			return errors.Join(err, fmt.Errorf("browser: the saved tabs still name the %s cookie jar the switch did not make: %w", kind, werr))
		}
		return err
	}
	persisting = false
	m.persistMu.Unlock()
	for _, c := range closes {
		m.startClose(c.scope, c.close)
	}
	if len(closes) == 0 {
		m.emitThreadState(access.ThreadID())
	}
	return nil
}

// switchAdmitsLocked refuses a jar switch of access's thread once the
// Manager closed, while a forget of every page runs (beginForget), or once
// the thread's close detached the lifetime access is bound to
// (thread_life.go). The caller holds m.mu.
func (m *Manager) switchAdmitsLocked(access Access) error {
	switch {
	case m.closed:
		return errManagerClosed
	case m.forgettingLocked():
		// The switch would record pages the forget closes.
		return errForgetting
	}
	return access.admitLocked()
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

// admitsLocked refuses the switch of tb when the consent does not cover it
// as tb is now. The caller holds m.mu.
func (c JarConsent) admitsLocked(m *Manager, tb *threadBrowser) error {
	switch c {
	case JarSwitchConfirmed:
		return nil
	case JarSwitchIfNoLiveTab:
		if m.hasLiveTabLocked(tb) {
			return ErrJarSwitchUnconfirmed
		}
		return nil
	case jarConsentUnset:
		return c.valid()
	default:
		return c.valid()
	}
}

// hasLiveTabLocked reports whether tb has a tab with a live page, the tabs
// a switch closes (commitLocked). A tab still being created is not live:
// the switch creates it again in the new jar. The caller holds m.mu.
func (m *Manager) hasLiveTabLocked(tb *threadBrowser) bool {
	for _, tab := range m.threadTabsLocked(tb) {
		if tab.live != nil {
			return true
		}
	}
	return false
}

// jarSwitch is one switch of a thread lifetime's jar, captured once: the
// saved copy it writes, holding each live page as the record its suspension
// makes, and those records by page, which its commit installs. Its fields
// are written only by captureJarSwitchLocked.
type jarSwitch struct {
	tb      *threadBrowser
	kind    string
	file    pageRecordFile
	records map[*managedPage]*suspendedPage
}

// captureJarSwitchLocked captures a switch of tb to kind. The caller holds
// m.mu.
func (m *Manager) captureJarSwitchLocked(tb *threadBrowser, kind string) *jarSwitch {
	sw := &jarSwitch{tb: tb, kind: kind, records: make(map[*managedPage]*suspendedPage)}
	sw.file = pageRecordFile{Version: pageRecordVersion, ThreadID: string(tb.id), ActivePageID: tb.session.ActivePageID, Jar: savedJar(kind)}
	for _, tab := range m.threadTabsLocked(tb) {
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
// page the switch captured becomes, while it closes, the record it wrote.
// The structural gate held the tabs as captured, so there is nothing to
// reconcile. It answers the closes for the caller to start after releasing
// m.mu. The caller holds m.mu.
func (sw *jarSwitch) commitLocked(m *Manager) []switchClose {
	tb := sw.tb
	info := &tb.session
	info.Jar, info.jarGen, info.privateJar = sw.kind, info.jarGen+1, ""
	info.UpdatedAt = time.Now()
	// The recent addresses and closed tabs were the old jar's browsing; an
	// open still running in it records nothing (recordRecentAddress). The
	// switch's push carries both.
	m.forgetBrowsingLocked(tb)
	var closes []switchClose
	for old, rec := range sw.records {
		_, scope := m.livePageLocked(old.id)
		c := m.markClosingLocked(old, scope)
		m.suspended[old.id] = rec
		closes = append(closes, switchClose{scope: scope, close: c})
	}
	var teardown []<-chan struct{}
	for _, scope := range m.scopes {
		for _, c := range scope.closing {
			if c.p.owner == tb {
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
	return closes
}
