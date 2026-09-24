//go:build !nogui

package main

import (
	"net/http"
	"sync"
	"time"

	"agent-overflow/internal/startuppage"
	"agent-overflow/internal/uiwindow"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// desktopPages are the startup pages a desktop window shows before, or
// instead of, the app's own page (startuppage.Serve), served as the Wails
// application's assets: the update helper's loading page, and a failure
// the boot shows instead of starting. The app's own page comes from the
// transport.
type desktopPages struct {
	status  startuppage.Status
	loading []byte

	mu      sync.Mutex
	failure []byte
	window  *application.WebviewWindow
}

func newDesktopPages(status string) *desktopPages {
	return &desktopPages{loading: startuppage.Loading(status)}
}

func (p *desktopPages) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	report := func() startuppage.Report { return p.status.Report(time.Now()) }
	if !startuppage.Serve(w, r, p.loading, report, p.failurePage) {
		http.NotFound(w, r)
	}
}

func (p *desktopPages) failurePage() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failure == nil {
		return startuppage.Failure{Title: "Agent Overflow could not start.", Detail: "Start Agent Overflow again."}.HTML()
	}
	return p.failure
}

// attach is the window the pages show in, which opened on openedOn. A
// failure shown before the window existed is shown now.
func (p *desktopPages) attach(w *application.WebviewWindow, openedOn string) {
	p.mu.Lock()
	p.window = w
	failing := p.failure != nil
	p.mu.Unlock()
	if failing && openedOn != startuppage.FailurePath {
		w.SetURL(startuppage.FailurePath)
	}
}

// attached is the window, nil before attach.
func (p *desktopPages) attached() *application.WebviewWindow {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.window
}

// showing reports whether a failure is shown. A nil pages shows none.
func (p *desktopPages) showing() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.failure != nil
}

// showFailure puts page in the window, in front of the user.
func (p *desktopPages) showFailure(page startuppage.Failure) {
	html := page.HTML()
	p.mu.Lock()
	p.failure = html
	w := p.window
	p.mu.Unlock()
	if w != nil {
		w.SetURL(startuppage.FailurePath)
		uiwindow.Reveal(w)
	}
}

// showLoading puts the loading page in the window, its clock started now.
func (p *desktopPages) showLoading() {
	p.status.Begin(time.Now())
	p.mu.Lock()
	failing := p.failure != nil
	p.failure = nil
	w := p.window
	p.mu.Unlock()
	if w != nil && failing {
		w.SetURL(startuppage.LoadingPath)
	}
}
