package browser

import (
	"context"
	"log"

	"agent-overflow/internal/browser/ids"
)

// A page's info has two kinds of writer. An engine report (pageInfoReport)
// carries the engine's address and title as they changed, numbered in the
// engine's order; it is committed unless a newer report already was. A read
// asks the engine and commits what it was answered (commitRead). The address
// and the history state are fenced apart: each holds the stamp of the write
// that set it (readStamp, or a stamp taken at the commit for any other
// write), and a read's part is committed only if no write that began after
// the read set that field. A read run for an operation holds the
// page-operation lock from the read through its commit, so no other
// operation's read lands in between.

// pageUpdate is one write of a page's cached info. commitInfo applies it,
// and nothing else changes the info of a page once it exists.
type pageUpdate struct {
	// Location is the address and title; nil keeps them (a history refresh,
	// a label).
	Location *pageLocation
	// History is the back/forward state; nil when the source does not know
	// it (an engine report, a snapshot script), and the tab keeps the state
	// it shows rather than turn its buttons off.
	History *pageHistory
	// Listing is what only a browser_pages answer means; a committed
	// Location replaces it, so any other read clears it.
	Listing pageListing
	// Label, when set, replaces the label; nil keeps it.
	Label *string
	// DocumentTitle is the page's document reporting its own title and the
	// address it was at (pageHooks.DocumentTitle). It becomes the page's
	// docTitle, and the tab's title while the tab shows that address. An
	// engine report of that address shows the document's title in place of
	// its own: the two arrive on separate subscriptions in either order,
	// and a report's title is the browser's placeholder until the document
	// has one. A read is not changed by it; it asks the document itself.
	DocumentTitle *pageLocation
	// Report is an engine report's order (pageHooks.Info), zero for any
	// other write. A report is committed only if newer than every report
	// committed before it.
	Report uint64
	// Stamp, for a read's answer, is the stamp the read took before it
	// asked (readStamp): each part is committed only if its field was not
	// set by a write that began later. Zero takes a stamp at the commit,
	// newer than every read begun: an engine report, a label, or a page's
	// own write before it is listed (openAndLoad).
	Stamp uint64
}

type pageLocation struct{ URL, Title string }

type pageHistory struct{ CanGoBack, CanGoForward bool }

// pageListing is PageInfo.Selected and LastOpened, which each browser_pages
// call rewrites and no tab shows.
type pageListing struct {
	Selected   bool
	LastOpened string
}

// commitInfo applies the parts of u that are newer than what the page holds
// (Report, Stamp), bounding the address and title, and sets the page's
// unpushed record when what the tab shows changed. It answers the page's
// info after, and whether the address u carries was committed.
func (p *managedPage) commitInfo(u pageUpdate) (PageInfo, bool) {
	p.metaMu.Lock()
	defer p.metaMu.Unlock()
	if u.Report != 0 {
		if u.Report <= p.infoReport {
			return p.info, false
		}
		p.infoReport = u.Report
	}
	stamp := u.Stamp
	if stamp == 0 {
		p.infoStamp++
		stamp = p.infoStamp
	}
	info := p.info
	located := false
	if u.Location != nil && stamp > p.locationStamp {
		info.URL = truncateUTF8(u.Location.URL, maxBrowserURLBytes)
		info.Title = truncateUTF8(u.Location.Title, maxBrowserTitleBytes)
		if u.Report != 0 && p.docTitle.URL == info.URL {
			info.Title = p.docTitle.Title
		}
		info.Selected, info.LastOpened = u.Listing.Selected, u.Listing.LastOpened
		p.locationStamp = stamp
		located = true
	}
	if doc := u.DocumentTitle; doc != nil {
		p.docTitle = pageLocation{
			URL:   truncateUTF8(doc.URL, maxBrowserURLBytes),
			Title: truncateUTF8(doc.Title, maxBrowserTitleBytes),
		}
		// A title write like any other: a read begun before it carries an
		// older title and is not committed over it.
		if p.docTitle.URL == info.URL && stamp > p.locationStamp {
			info.Title = p.docTitle.Title
			p.locationStamp = stamp
		}
	}
	if u.History != nil && stamp > p.historyStamp {
		info.CanGoBack, info.CanGoForward = u.History.CanGoBack, u.History.CanGoForward
		p.historyStamp = stamp
	}
	if u.Label != nil {
		info.Label = *u.Label
	}
	if pushedInfo(info) != pushedInfo(p.info) {
		p.load.unpushed = true
	}
	p.info = info
	return info, located
}

// readStamp answers a stamp newer than every write committed so far; a read
// takes one before it asks the engine.
func (p *managedPage) readStamp() uint64 {
	p.metaMu.Lock()
	defer p.metaMu.Unlock()
	p.infoStamp++
	return p.infoStamp
}

// commitRead commits u, the answer of a read that took stamp before it
// asked (commitInfo).
func (m *Manager) commitRead(p *managedPage, u pageUpdate, stamp uint64) (PageInfo, bool) {
	if m.infoRead != nil {
		m.infoRead(p)
	}
	u.Stamp = stamp
	info, located := p.commitInfo(u)
	if located {
		m.applyPageBase(p)
	}
	return info, located
}

// pushedInfo is info without its listing (pageListing).
func pushedInfo(info PageInfo) PageInfo {
	info.Selected, info.LastOpened = false, ""
	return info
}

func (p *managedPage) cachedInfo() PageInfo {
	p.metaMu.RLock()
	info := p.info
	p.metaMu.RUnlock()
	return info
}

// updatePageInfo is engineEvents.PageInfoChanged, a CDP engine's report of
// a page by its handle. A report for a handle no page answers to yet (an
// engine page the Manager has not attached or adopted) is held, the newest
// by order, until its page takes it (takeHeldInfoLocked).
func (m *Manager) updatePageInfo(handle ids.PageHandle, url, title string, order uint64) {
	m.mu.Lock()
	found := m.pageByHandleLocked(handle)
	if found == nil {
		found = m.creatingPageByHandleLocked(handle)
	}
	if found == nil {
		// A handle that will not be listed again (unlistedLocked) has
		// nothing to hold a report for.
		_, unlisted := m.unlistedLocked().get(handle)
		if held, ok := m.heldInfo.get(handle); !unlisted && (!ok || held.order < order) {
			m.heldInfo.put(handle, heldInfo{url: url, title: title, order: order})
		}
	}
	m.mu.Unlock()
	if found != nil {
		m.pageInfoReport(found, url, title, order)
	}
}

// heldInfo is an engine report of a handle no page answered to yet.
type heldInfo struct {
	url, title string
	order      uint64
}

// takeHeldInfoLocked takes the report held for handle, in the hold of m.mu
// that makes a page answer to it, so every report for the handle either was
// held before or finds the page after. The caller commits it to the page
// (applyHeldInfo) once it releases m.mu.
func (m *Manager) takeHeldInfoLocked(handle ids.PageHandle) (heldInfo, bool) {
	return m.heldInfo.take(handle)
}

// applyHeldInfo commits a report taken by takeHeldInfoLocked to p.
func (m *Manager) applyHeldInfo(p *managedPage, held heldInfo, ok bool) {
	if ok {
		m.pageInfoReport(p, held.url, held.title, held.order)
	}
}

// commitHeldInfoLocked commits the report held for handle to p in the hold
// of m.mu that lists p, so the listing's own push carries it and nothing
// pushes it apart. The caller holds m.mu and pushes p's listing.
func (m *Manager) commitHeldInfoLocked(p *managedPage, handle ids.PageHandle) {
	held, ok := m.heldInfo.take(handle)
	if !ok {
		return
	}
	update := pageUpdate{Report: held.order}
	if held.url != "" {
		update.Location = &pageLocation{URL: held.url, Title: held.title}
	}
	p.commitInfo(update)
}

// pageInfoReport is pageHooks.Info: an engine report of p's address and
// title. It is committed to p wherever p is, so the report order fence sees
// every report from the moment p's engine page exists: a page being created,
// restored or adopted takes it into its unlisted info, which its own load
// read replaces when that read began later, and a page that left its
// thread's tabs takes it into info nothing reads again. Only a listed page
// pushes it and refreshes its history state; an unlisted page's load read
// carries the history and its listing pushes.
func (m *Manager) pageInfoReport(p *managedPage, url, title string, order uint64) {
	if url == "" {
		// No address is the engine losing its document (WebKitGTK clears
		// a view's URI when its web process ends), never a navigation: the
		// tab keeps the page's last address, which a crash restores. The
		// report still takes its place in the engine's order.
		p.commitInfo(pageUpdate{Report: order})
		return
	}
	if _, committed := p.commitInfo(pageUpdate{Location: &pageLocation{URL: url, Title: title}, Report: order}); !committed {
		// A newer report was committed first and runs its own refresh.
		return
	}
	m.applyPageBase(p)
	m.mu.Lock()
	listed := m.pageListedLocked(p)
	m.mu.Unlock()
	if !listed {
		return
	}
	stamp := p.readStamp()
	// An engine reports the same address and title again (a target info
	// change that touched neither); that alone pushes nothing.
	m.publishPages(p.owner.id)
	go m.refreshHistoryState(p, stamp)
}

// pageDocumentTitle is pageHooks.DocumentTitle. Like an engine report it is
// committed wherever p is, and only a listed page pushes it.
func (m *Manager) pageDocumentTitle(p *managedPage, url, title string) {
	p.commitInfo(pageUpdate{DocumentTitle: &pageLocation{URL: url, Title: title}})
	m.mu.Lock()
	listed := m.pageListedLocked(p)
	m.mu.Unlock()
	if listed {
		m.publishPages(p.owner.id)
	}
}

// refreshHistoryState re-reads one page's back/forward availability after an
// engine announced a navigation, off the engine's event goroutine — the read
// is a driver round trip, and blocking the event dispatcher on it could
// deadlock an engine whose events and commands share a loop. It runs without
// the page-operation lock under the stamp taken after the report (stamp): an
// operation's read that began later carries the history state itself and
// stands, and an address commit does not touch its answer.
func (m *Manager) refreshHistoryState(p *managedPage, stamp uint64) {
	ctx, cancel := operationContext(context.Background(), p.driver.Lifetime(), operationTimeout)
	defer cancel()
	back, forward, err := p.driver.HistoryState(ctx)
	if err != nil {
		// A page that closed meanwhile has nothing to show; any other
		// failure leaves Back and Forward as the last read had them.
		if p.ctx.Err() == nil {
			log.Printf("browser: read the history of page %s: %v", p.id, err)
		}
		return
	}
	m.commitRead(p, pageUpdate{History: &pageHistory{CanGoBack: back, CanGoForward: forward}}, stamp)
	m.publishPages(p.owner.id)
}

// creatingPageByHandleLocked returns the page being created or restored
// whose engine page has handle, or nil. The caller holds m.mu.
func (m *Manager) creatingPageByHandleLocked(handle ids.PageHandle) *managedPage {
	for _, scope := range m.scopes {
		for p := range scope.creating {
			if p.driver != nil && p.driver.Handle() == handle {
				return p
			}
		}
	}
	return nil
}

// pageListedLocked reports whether p is in its thread's tabs. The caller
// holds m.mu.
func (m *Manager) pageListedLocked(p *managedPage) bool {
	for _, scope := range m.scopes {
		if scope.pages[p.id] == p {
			return true
		}
	}
	return false
}
