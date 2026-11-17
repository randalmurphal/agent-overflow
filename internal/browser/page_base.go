package browser

import "strings"

// A page's base is the color its engine paints wherever the page's document
// leaves its own background transparent or has not painted yet. It is a
// property of the page, not of its presentation: a hidden page is laid out,
// screenshotted and shown again on it, so the Manager applies it whenever an
// input changes, shown or not (applyPageBase), and a presentation carries
// geometry only.

// documentBase is the base under a page that shows a document: the white
// every browser paints where a document leaves its own background
// transparent.
const documentBase = "#ffffff"

// pageShowsDocument answers whether a page at url shows a document. A blank
// new tab, and a page the engine has not given an address yet, show none.
func pageShowsDocument(url string) bool {
	switch {
	case url == "", url == "about:blank":
		return false
	case strings.HasPrefix(url, "about:blank#"), strings.HasPrefix(url, "about:blank?"):
		return false
	}
	return true
}

// pageBase is a page's base: the document base when it shows a document,
// and otherwise the pane's own color, so an empty tab is the pane. A blank
// page of a thread whose pane never reported a color has no base ("") and
// keeps whatever its engine paints, which is white on every engine.
func pageBase(document bool, pane string) string {
	if document {
		return documentBase
	}
	return pane
}

// paneColor answers a pane report's color when it is one ("#rrggbb"). Any
// other spelling is no answer, never a guess: the value reaches a native
// color on every engine.
func paneColor(value string) (string, bool) {
	if len(value) != 7 || value[0] != '#' {
		return "", false
	}
	for i := 1; i < 7; i++ {
		c := value[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return "", false
		}
	}
	return strings.ToLower(value), true
}

// applyPageBase decides p's base from its address and its thread's pane
// color, and tells the engine when that changed. Its callers are the inputs:
// a committed address (info.go), a pane color (notePaneColorLocked) and p
// joining its tabs, created or restored (pageAdded). A page not in its tabs
// is left alone: its driver is published to other goroutines by its listing,
// and pageAdded applies its base once it is listed.
//
// Applications of one page are ordered by baseMu and each reads the inputs
// as they are under it, so the last one the engine hears is decided from
// the newest inputs. An engine that presents no page (headless Chromium)
// has no pane and keeps its own white.
func (m *Manager) applyPageBase(p *managedPage) {
	host, ok := m.engine.(paneHost)
	if !ok {
		return
	}
	p.baseMu.Lock()
	defer p.baseMu.Unlock()
	m.mu.Lock()
	listed := m.pageListedLocked(p)
	pane := m.paneColors[p.owner]
	m.mu.Unlock()
	if !listed {
		return
	}
	base := pageBase(pageShowsDocument(p.cachedInfo().URL), pane)
	if base == "" || base == p.base {
		return
	}
	p.base = base
	host.SetPageBase(p.driver.Handle(), base)
}

// notePaneColorLocked records the color of the thread's presenting pane and
// answers the thread's pages when it changed, for the caller to apply their
// bases once it releases m.mu. The caller holds m.mu.
func (m *Manager) notePaneColorLocked(threadID string, rect PaneRect) []*managedPage {
	color, ok := paneColor(rect.Background)
	if !ok || m.paneColors[threadID] == color {
		return nil
	}
	if m.paneColors == nil {
		m.paneColors = make(map[string]string)
	}
	m.paneColors[threadID] = color
	var pages []*managedPage
	for _, scope := range m.scopes {
		for _, p := range scope.pages {
			if p.owner == threadID {
				pages = append(pages, p)
			}
		}
	}
	return pages
}
