package browser

import "context"

// closeLivePage closes a live page of tb and waits for its teardown. It
// answers errPageNotFound when the page is not live or closing.
func (m *Manager) closeLivePage(ctx context.Context, tb *threadBrowser, pageID PageID) error {
	if err := m.lockStructure(ctx, tb); err != nil {
		return err
	}
	c, started, err := m.beginCloseLiveLocked(tb, pageID, false)
	m.mu.Unlock()
	if started != nil {
		m.startClose(started, c)
	}
	if err != nil {
		return err
	}
	return c.wait(ctx)
}
