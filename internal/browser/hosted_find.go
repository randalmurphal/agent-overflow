package browser

import (
	"context"
	"errors"

	"agent-overflow/internal/webview2host"
)

// noteFindUnavailable records a page the launcher says has no find API. It
// arrives while the page's create waits, while a popup waits for the
// Manager, or after the page is bound; any other page id is not one this
// engine holds.
func (e *hostedEngine) noteFindUnavailable(pageID string) {
	e.mu.Lock()
	_, creating := e.pending[pageID]
	_, popup := e.popups[pageID]
	_, bound := e.targetByPage[pageID]
	known := creating || popup || bound
	if known {
		e.noFind[pageID] = true
	}
	e.mu.Unlock()
	if !known {
		e.logf("browser: pane host reported no find for unknown page %s", pageID)
	}
}

// FindMode is the engine's own find bar, unless the page's runtime has none.
func (p *hostedPage) FindMode() string {
	p.engine.mu.Lock()
	defer p.engine.mu.Unlock()
	if p.engine.noFind[p.id] {
		return ""
	}
	return FindModeEngine
}

// FindStart opens the page's find bar in the launcher. The bar counts and
// steps itself, so there is no answer to wait for.
func (p *hostedPage) FindStart(_ context.Context, query string) (findResult, error) {
	if !p.engine.dispatch(webview2host.Directive{Op: webview2host.OpFind, PageID: p.id, Query: query}) {
		return findResult{}, errors.New("browser: pane host refused the find directive")
	}
	return findResult{}, nil
}

func (p *hostedPage) FindStep(context.Context, bool) (findResult, error) {
	return findResult{}, errors.New("browser: the pane host steps through matches in its own find bar")
}

func (p *hostedPage) FindStop(context.Context) error {
	if !p.engine.dispatch(webview2host.Directive{Op: webview2host.OpFindStop, PageID: p.id}) {
		return errors.New("browser: pane host refused the find-stop directive")
	}
	return nil
}

var _ pageFind = (*hostedPage)(nil)
