package browser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/webview2host"
)

// The hosted engine's profiles (hostedProfile): one named WebView2 profile
// per cookie jar in the launcher's pane environment, and the clear of every
// profile's site data.

// NewProfile derives a launcher profile id from the jar identity: one
// profile per jar. The digest is what makes the id stable across restarts,
// which is what makes a persistent jar's logins survive one.
func (e *hostedEngine) NewProfile(ctx context.Context, opts profileOptions) (engineProfile, error) {
	id, err := hostedProfileID(opts.Jar)
	if err != nil {
		return nil, err
	}
	// Site data is the engine's own (spec §4): a hosted profile is a real
	// browser profile on disk and WebView2 persists it, so AO seeds nothing
	// into it and reads nothing out of it.
	if opts.Allow != nil {
		e.mu.Lock()
		e.policies[id] = opts.Allow
		e.mu.Unlock()
	}
	return &hostedProfile{engine: e, id: id, ephemeral: opts.Ephemeral}, nil
}

// hostedProfileID is the WebView2 profile name of a jar. A worktree jar's
// identity is its workspace path, so its name is the one every earlier build
// derived.
func hostedProfileID(jar jarKey) (profileHandle, error) {
	trimmed := strings.TrimSpace(string(jar))
	if trimmed == "" {
		return "", errors.New("browser: a cookie jar is required for a pane profile")
	}
	digest := sha256.Sum256([]byte(trimmed))
	id := hostProfilePrefix + hex.EncodeToString(digest[:16])
	if err := webview2host.ValidateProfileID(id); err != nil {
		return "", fmt.Errorf("browser: derive pane profile id: %w", err)
	}
	return profileHandle(id), nil
}

// ClearSiteData is the engineSiteData half of Settings → Clear site data.
//
// On this deployment the Manager's own `browser-profiles/` tree is EMPTY:
// every cookie jar lives in the launcher's WebView2 user-data folder on the
// Windows side of the WSL boundary, one folder holding every workspace's
// named profile. The backend cannot delete it by path — it is a different
// machine's filesystem, and a live WebView2 environment holds it open — so
// the clear is a directive like every other pane-host operation, and the
// launcher's answer is what makes it a real clear rather than a hope.
//
// The Manager calls this only after closeBrowser, so no page or profile is
// live. The launcher still closes every controller of its own accord: its
// controller set is the authority on what is actually open, and a stale one
// would keep a handle on the folder being deleted.
func (e *hostedEngine) ClearSiteData(ctx context.Context) error {
	// The clear addresses no page, but the launcher's report machinery is
	// keyed on a page id, so it carries a correlation id minted the same
	// way. Registering the waiter BEFORE dispatching is what makes an
	// instant answer impossible to miss.
	correlationID := newHostedPageID()
	waiter, err := e.waiters.watch(correlationID)
	if err != nil {
		return err
	}
	defer e.waiters.unwatch(correlationID)

	if !e.dispatch(webview2host.Directive{Op: webview2host.OpClearData, PageID: string(correlationID)}) {
		return errors.New("browser: pane host refused the clear-data directive")
	}

	report, err := awaitHostReport(ctx, waiter, e.clearTimeout)
	if err != nil {
		// The site data may or may not be gone. Saying so is the honest
		// answer: a Settings button that reported success here would leave
		// the user believing cookies were destroyed that are still on disk.
		return fmt.Errorf("browser: clear pane site data: %w", err)
	}
	if report.kind != webview2host.ReportCleared {
		return fmt.Errorf("browser: pane host could not clear site data (%s): %s", report.kind, report.detail)
	}
	return nil
}

// hostedProfile is one workspace's named CoreWebView2Profile in the
// launcher's pane environment.
type hostedProfile struct {
	engine    *hostedEngine
	id        profileHandle
	ephemeral bool
}

func (p *hostedProfile) Handle() profileHandle { return p.id }

func (p *hostedProfile) NewPage(ctx context.Context, hooks pageHooks) (pageDriver, error) {
	return p.engine.createPage(ctx, p, hooks)
}

// AttachPage adopts a popup the launcher reported (hosted_popups.go).
func (p *hostedProfile) AttachPage(_ context.Context, handle ids.PageHandle, hooks pageHooks) (pageDriver, error) {
	return p.engine.attachPopup(p, handle, hooks)
}

func (p *hostedProfile) CancelDownload(id downloadID) error { return p.engine.cancelDownload(id) }

func (p *hostedProfile) Dispose(context.Context) error {
	p.engine.mu.Lock()
	delete(p.engine.policies, p.id)
	p.engine.mu.Unlock()
	p.engine.dispatch(webview2host.Directive{Op: webview2host.OpCloseProfile, ProfileID: string(p.id), Ephemeral: p.ephemeral})
	return nil
}
