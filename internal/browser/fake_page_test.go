package browser

// sameDocumentNavigation changes the address the way history.pushState
// does: no new document.
func (p *fakePage) sameDocumentNavigation(rawURL string) {
	p.mu.Lock()
	p.url, p.title = rawURL, fakeTitle(rawURL)
	p.mu.Unlock()
}

// lastViewport is the viewport the page was last given, 0x0 for none.
func (p *fakePage) lastViewport() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.viewports) == 0 {
		return 0, 0
	}
	last := p.viewports[len(p.viewports)-1]
	return last[0], last[1]
}
