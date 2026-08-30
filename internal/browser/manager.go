package browser

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agent-overflow/internal/chromium"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/google/uuid"
)

const (
	operationTimeout        = 30 * time.Second
	idleBrowserDelay        = 2 * time.Minute
	maxPagesPerThread       = 8
	maxPagesPerWorkspace    = 24
	maxPagesTotal           = 64
	maxWorkspaceContexts    = 12
	maxSnapshotText         = 100_000
	maxSnapshotElements     = 500
	maxEvaluateBytes        = 256_000
	maxLocalStorageChars    = 1 << 20
	maxLocalStorageOrigins  = 64
	maxScreenshotBytes      = 20 << 20
	maxFullScreenshotHeight = 12_000
	maxFullScreenshotWidth  = 4_000
)

type Manager struct {
	installer *chromium.Installer
	state     *stateStore

	startMu sync.Mutex
	mu      sync.Mutex
	config  Config
	closed  bool

	allocCtx      context.Context
	allocCancel   context.CancelFunc
	browserCtx    context.Context
	browserCancel context.CancelFunc
	startupCancel context.CancelFunc
	scopes        map[string]*workspaceScope
	idleTimer     *time.Timer
	// pageAdopted is a test seam for the asynchronous popup ownership handoff.
	// Production leaves it nil; the managed-Chrome integration test uses the
	// signal instead of polling on wall-clock sleeps.
	pageAdopted func()
}

type workspaceScope struct {
	workspace string
	ctx       context.Context
	cancel    func(context.Context) error
	contextID cdp.BrowserContextID
	state     storageState
	pages     map[string]*managedPage
}

type managedPage struct {
	id      string
	owner   string
	target  target.ID
	access  Access
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	lastUse atomic.Int64
}

func NewManager(installer *chromium.Installer, configDir string, config Config) *Manager {
	return &Manager{
		installer: installer,
		state:     newStateStore(configDir),
		config:    config,
		scopes:    make(map[string]*workspaceScope),
	}
}

func (m *Manager) Reconfigure(config Config) error {
	m.mu.Lock()
	changedLaunch := m.config.ShowWindow != config.ShowWindow
	changedPersistence := m.config.PersistSiteData != config.PersistSiteData
	m.config = config
	m.mu.Unlock()
	if !config.Enabled || changedLaunch || changedPersistence {
		return m.closeBrowser(context.Background(), true)
	}
	return nil
}

func (m *Manager) Open(ctx context.Context, access Access, rawURL string, opts OpenOptions) (PageInfo, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return PageInfo{}, fmt.Errorf("browser: invalid URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return PageInfo{}, fmt.Errorf("browser: URL scheme %q is not allowed; use browser_open_file for local files", parsed.Scheme)
	}
	if parsed.Host == "" {
		return PageInfo{}, fmt.Errorf("browser: URL host is required")
	}
	return m.navigate(ctx, access, parsed.String(), opts)
}

func (m *Manager) OpenFile(ctx context.Context, access Access, path string, opts OpenOptions) (PageInfo, error) {
	resolved, err := m.authorizeFile(access, path)
	if err != nil {
		return PageInfo{}, err
	}
	return m.navigate(ctx, access, (&url.URL{Scheme: "file", Path: filepath.ToSlash(resolved)}).String(), opts)
}

func (m *Manager) navigate(ctx context.Context, access Access, targetURL string, opts OpenOptions) (PageInfo, error) {
	p, err := m.pageFor(ctx, access, opts.PageID, opts.NewPage)
	if err != nil {
		return PageInfo{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	opCtx, cancel := operationContext(ctx, p.ctx, operationTimeout)
	defer cancel()
	m.captureLocalStorage(opCtx, p)
	if err := chromedp.Run(opCtx, chromedp.Navigate(targetURL)); err != nil {
		return PageInfo{}, fmt.Errorf("browser: navigate: %w", err)
	}
	p.touch()
	info, err := pageInfo(opCtx, p.id)
	if err == nil {
		m.checkpointPage(opCtx, p)
	}
	return info, err
}

func (m *Manager) Pages(ctx context.Context, access Access) ([]PageInfo, error) {
	pages := m.ownedPages(access.ThreadID)
	out := make([]PageInfo, 0, len(pages))
	for _, p := range pages {
		p.mu.Lock()
		opCtx, cancel := operationContext(ctx, p.ctx, 5*time.Second)
		info, err := pageInfo(opCtx, p.id)
		cancel()
		p.mu.Unlock()
		if err == nil {
			out = append(out, info)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *Manager) ClosePage(ctx context.Context, access Access, pageID string) error {
	p, scope, err := m.lookupOwnedPage(access, pageID)
	if err != nil {
		return err
	}
	p.mu.Lock()
	opCtx, cancel := operationContext(ctx, p.ctx, 5*time.Second)
	m.captureLocalStorage(opCtx, p)
	cancel()
	p.cancel()
	p.mu.Unlock()
	m.mu.Lock()
	delete(scope.pages, p.id)
	empty := len(scope.pages) == 0
	m.mu.Unlock()
	if empty {
		return m.disposeScope(ctx, scope.workspace)
	}
	return nil
}

func (m *Manager) CloseThread(ctx context.Context, threadID string) error {
	pages := m.ownedPages(threadID)
	var errs []error
	for _, p := range pages {
		access := Access{ThreadID: threadID, Workspace: m.workspaceForPage(p.id)}
		if err := m.ClosePage(ctx, access, p.id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) ClearSiteData(ctx context.Context) error {
	if err := m.closeBrowser(ctx, false); err != nil {
		return err
	}
	return m.state.clear()
}

func (m *Manager) Close() error {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	return m.closeBrowser(context.Background(), true)
}

func (m *Manager) pageFor(ctx context.Context, access Access, requested string, forceNew bool) (*managedPage, error) {
	if strings.TrimSpace(access.ThreadID) == "" || strings.TrimSpace(access.Workspace) == "" {
		return nil, fmt.Errorf("browser: invalid caller scope")
	}
	if requested != "" {
		p, _, err := m.lookupOwnedPage(access, requested)
		return p, err
	}
	if !forceNew {
		owned := m.ownedPages(access.ThreadID)
		if len(owned) > 0 {
			sort.Slice(owned, func(i, j int) bool { return owned[i].lastUse.Load() > owned[j].lastUse.Load() })
			return owned[0], nil
		}
	}
	return m.createPage(ctx, access)
}

func (m *Manager) createPage(ctx context.Context, access Access) (*managedPage, error) {
	if err := m.ensureStarted(ctx); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		m.scheduleIdleClose()
		return nil, err
	}
	workspace, err := canonicalRoot(access.Workspace)
	if err != nil {
		return nil, fmt.Errorf("browser: resolve workspace: %w", err)
	}
	access.Workspace = workspace

	m.startMu.Lock()
	defer m.startMu.Unlock()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, fmt.Errorf("browser: manager closed")
	}
	if m.idleTimer != nil {
		m.idleTimer.Stop()
		m.idleTimer = nil
	}
	scope := m.scopes[workspace]
	newScope := scope == nil
	if scope == nil && len(m.scopes) >= maxWorkspaceContexts {
		m.mu.Unlock()
		return nil, fmt.Errorf("browser: workspace context limit reached (%d)", maxWorkspaceContexts)
	}
	if countOwnedPagesLocked(m.scopes, access.ThreadID) >= maxPagesPerThread {
		m.mu.Unlock()
		return nil, fmt.Errorf("browser: page limit reached for thread (%d)", maxPagesPerThread)
	}
	if scope != nil && len(scope.pages) >= maxPagesPerWorkspace {
		m.mu.Unlock()
		return nil, fmt.Errorf("browser: page limit reached for workspace (%d)", maxPagesPerWorkspace)
	}
	if countPagesLocked(m.scopes) >= maxPagesTotal {
		m.mu.Unlock()
		return nil, fmt.Errorf("browser: process page limit reached (%d)", maxPagesTotal)
	}
	m.mu.Unlock()

	if scope == nil {
		scope, err = m.createScope(workspace)
		if err != nil {
			return nil, err
		}
		m.mu.Lock()
		m.scopes[workspace] = scope
		m.mu.Unlock()
	}

	var pageCtx context.Context
	var pageCancel context.CancelFunc
	if newScope {
		// A new incognito profile has no Browser window yet. Current full
		// Chrome rejects Target.createTarget's explicit newWindow=false in that
		// state, so create the context's first target as a new window and attach
		// to it. Later pages can be normal tabs in that window.
		targetID, createErr := target.CreateTarget("about:blank").WithBrowserContextID(scope.contextID).WithNewWindow(true).Do(browserCommandContext(scope.ctx))
		if createErr != nil {
			_ = m.disposeScope(context.Background(), workspace)
			return nil, fmt.Errorf("browser: create workspace window: %w", createErr)
		}
		pageCtx, pageCancel = chromedp.NewContext(scope.ctx, chromedp.WithTargetID(targetID))
	} else {
		pageCtx, pageCancel = chromedp.NewContext(scope.ctx, chromedp.WithExistingBrowserContext(scope.contextID))
	}
	if err := chromedp.Run(pageCtx); err != nil {
		pageCancel()
		if newScope {
			_ = m.disposeScope(context.Background(), workspace)
		}
		return nil, fmt.Errorf("browser: create page: %w (controller: %v)", err, m.browserCtx.Err())
	}
	if err := installStorageRestoreScript(pageCtx, m.localStorageSnapshot(scope)); err != nil {
		pageCancel()
		if newScope {
			_ = m.disposeScope(context.Background(), workspace)
		}
		return nil, err
	}
	targetID := chromedp.FromContext(pageCtx).Target.TargetID
	p := &managedPage{id: uuid.NewString(), owner: access.ThreadID, target: targetID, access: access, ctx: pageCtx, cancel: pageCancel}
	if err := m.installPageHandlers(p); err != nil {
		pageCancel()
		if newScope {
			_ = m.disposeScope(context.Background(), workspace)
		}
		return nil, err
	}
	p.touch()
	m.mu.Lock()
	scope.pages[p.id] = p
	m.mu.Unlock()
	return p, nil
}

func (m *Manager) createScope(workspace string) (*workspaceScope, error) {
	m.mu.Lock()
	browserCtx := m.browserCtx
	persist := m.config.PersistSiteData
	m.mu.Unlock()
	if browserCtx == nil {
		return nil, fmt.Errorf("browser: process unavailable")
	}
	state := storageState{Version: 1, Workspace: workspace, LocalStorage: make(map[string]map[string]string)}
	if persist {
		loaded, err := m.state.load(workspace)
		if err != nil {
			log.Printf("browser: load site data for %s: %v", workspace, err)
		} else {
			state = loaded
		}
	}
	contextID, err := target.CreateBrowserContext().WithDisposeOnDetach(false).Do(browserCommandContext(browserCtx))
	if err != nil {
		return nil, fmt.Errorf("browser: create workspace context: %w", err)
	}
	dispose := func(ctx context.Context) error {
		disposeCtx, cancel := operationContext(ctx, browserCtx, 5*time.Second)
		defer cancel()
		return target.DisposeBrowserContext(contextID).Do(browserCommandContext(disposeCtx))
	}
	if err := browser.SetDownloadBehavior(browser.SetDownloadBehaviorBehaviorDeny).WithBrowserContextID(contextID).Do(browserCommandContext(browserCtx)); err != nil {
		_ = dispose(context.Background())
		return nil, fmt.Errorf("browser: deny downloads: %w", err)
	}
	// Browser tools must never turn a website permission prompt into ambient
	// access to the user's machine. CDP permission names unsupported by a future
	// Chrome build are logged and remain at Chrome's safer default (prompt).
	for _, name := range []string{"geolocation", "notifications", "midi", "camera", "microphone", "clipboard-read", "clipboard-write", "idle-detection"} {
		if err := browser.SetPermission(&browser.PermissionDescriptor{Name: name}, browser.PermissionSettingDenied).WithBrowserContextID(contextID).Do(browserCommandContext(browserCtx)); err != nil {
			log.Printf("browser: deny %s permission: %v", name, err)
		}
	}
	if len(state.Cookies) > 0 {
		if err := storage.SetCookies(state.Cookies).WithBrowserContextID(contextID).Do(browserCommandContext(browserCtx)); err != nil {
			_ = dispose(context.Background())
			return nil, fmt.Errorf("browser: restore cookies: %w", err)
		}
	}
	return &workspaceScope{workspace: workspace, ctx: browserCtx, cancel: dispose, contextID: contextID, state: state, pages: make(map[string]*managedPage)}, nil
}

func (m *Manager) ensureStarted(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.startMu.Lock()
	defer m.startMu.Unlock()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("browser: manager closed")
	}
	if !m.config.Enabled {
		m.mu.Unlock()
		return fmt.Errorf("browser: tools are disabled")
	}
	if m.browserCtx != nil && m.browserCtx.Err() == nil {
		m.mu.Unlock()
		return nil
	}
	showWindow := m.config.ShowWindow
	m.mu.Unlock()
	if m.installer == nil {
		return fmt.Errorf("browser: installer unavailable")
	}
	// Provider MCP clients commonly cap a tool call near one minute, while the
	// first managed-Chrome download can take longer. Once requested, let the
	// bounded install finish even if that individual HTTP request disconnects;
	// the next call then reuses the warm artifact/process. Disabling the feature
	// or shutting down explicitly cancels this startup context.
	startupCtx, startupCancel := context.WithTimeout(context.Background(), 30*time.Minute)
	m.mu.Lock()
	m.startupCancel = startupCancel
	m.mu.Unlock()
	defer func() {
		startupCancel()
		m.mu.Lock()
		m.startupCancel = nil
		m.mu.Unlock()
	}()
	installed, err := m.installer.Install(startupCtx)
	if err != nil {
		return fmt.Errorf("browser: install Chrome: %w", err)
	}
	opts := []chromedp.ExecAllocatorOption{
		chromedp.ExecPath(installed.BinaryPath),
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.Flag("enable-automation", true),
		chromedp.Flag("disable-default-apps", true),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("disable-sync", true),
		chromedp.Flag("force-color-profile", "srgb"),
		// ExecAllocator otherwise silently adds --no-sandbox when the parent
		// runs as root. Failing to launch is safer than weakening isolation.
		chromedp.Flag("no-sandbox", false),
		chromedp.ModifyCmdFunc(func(*exec.Cmd) {}),
		chromedp.CombinedOutput(browserLogWriter{}),
	}
	if !showWindow || (runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "") {
		opts = append(opts, chromedp.Flag("headless", "new"))
	}
	if runtime.GOOS == "linux" {
		opts = append(opts, chromedp.Flag("disable-dev-shm-usage", true))
	}
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	browserCtx, browserCancel := chromedp.NewContext(allocCtx, chromedp.WithErrorf(func(format string, args ...any) {
		log.Printf("browser: chromedp: "+format, args...)
	}))
	if err := chromedp.Run(browserCtx); err != nil {
		browserCancel()
		allocCancel()
		return fmt.Errorf("browser: launch Chrome: %w", err)
	}
	chromedp.ListenBrowser(browserCtx, func(ev any) {
		switch event := ev.(type) {
		case *target.EventTargetCreated:
			if event.TargetInfo != nil && event.TargetInfo.Type == "page" && event.TargetInfo.OpenerID != "" {
				info := *event.TargetInfo
				go m.adoptPopup(info)
			}
		case *target.EventTargetDestroyed:
			go m.removeDestroyedTarget(event.TargetID)
		}
	})
	m.mu.Lock()
	m.allocCtx, m.allocCancel = allocCtx, allocCancel
	m.browserCtx, m.browserCancel = browserCtx, browserCancel
	m.mu.Unlock()
	return nil
}

func (m *Manager) installPageHandlers(p *managedPage) error {
	chromedp.ListenTarget(p.ctx, func(ev any) {
		switch event := ev.(type) {
		case *page.EventJavascriptDialogOpening:
			accept := event.Type == page.DialogTypeBeforeunload
			go func() {
				ctx, cancel := operationContext(context.Background(), p.ctx, 3*time.Second)
				defer cancel()
				_ = page.HandleJavaScriptDialog(accept).Do(targetCommandContext(ctx))
			}()
		case *fetch.EventRequestPaused:
			if event.Request == nil {
				return
			}
			requestID, rawURL := event.RequestID, event.Request.URL
			go func() {
				ctx, cancel := operationContext(context.Background(), p.ctx, 5*time.Second)
				defer cancel()
				if m.navigationAllowed(p.access, rawURL) {
					_ = fetch.ContinueRequest(requestID).Do(targetCommandContext(ctx))
				} else {
					_ = fetch.FailRequest(requestID, network.ErrorReasonBlockedByClient).Do(targetCommandContext(ctx))
				}
			}()
		}
	})
	patterns := []*fetch.RequestPattern{
		{ResourceType: network.ResourceTypeDocument, RequestStage: fetch.RequestStageRequest},
		// Document-only interception still lets a workspace HTML page embed an
		// outside-workspace file as an image/script. Intercept every local-file
		// request so the same authority check covers subresources too.
		{URLPattern: "file://*", RequestStage: fetch.RequestStageRequest},
	}
	if err := fetch.Enable().WithPatterns(patterns).Do(targetCommandContext(p.ctx)); err != nil {
		return fmt.Errorf("browser: install navigation policy: %w", err)
	}
	return nil
}

func (m *Manager) adoptPopup(info target.Info) {
	m.mu.Lock()
	var scope *workspaceScope
	var owner string
	var access Access
	for _, candidate := range m.scopes {
		if candidate.contextID != info.BrowserContextID {
			continue
		}
		scope = candidate
		for _, p := range candidate.pages {
			if p.target == info.OpenerID {
				owner = p.owner
				access = p.access
				break
			}
		}
		break
	}
	tooMany := owner != "" && (countOwnedPagesLocked(m.scopes, owner) >= maxPagesPerThread || len(scope.pages) >= maxPagesPerWorkspace || countPagesLocked(m.scopes) >= maxPagesTotal)
	browserCtx := m.browserCtx
	m.mu.Unlock()
	if scope == nil || owner == "" || tooMany || browserCtx == nil {
		if browserCtx != nil {
			_ = target.CloseTarget(info.TargetID).Do(browserCommandContext(browserCtx))
		}
		return
	}

	pageCtx, pageCancel := chromedp.NewContext(browserCtx, chromedp.WithTargetID(info.TargetID))
	if err := chromedp.Run(pageCtx); err != nil {
		pageCancel()
		return
	}
	if err := installStorageRestoreScript(pageCtx, m.localStorageSnapshot(scope)); err != nil {
		pageCancel()
		return
	}
	p := &managedPage{id: uuid.NewString(), owner: owner, target: info.TargetID, access: access, ctx: pageCtx, cancel: pageCancel}
	p.touch()
	if err := m.installPageHandlers(p); err != nil {
		pageCancel()
		return
	}
	m.mu.Lock()
	current := m.scopes[scope.workspace]
	if current != scope || countOwnedPagesLocked(m.scopes, owner) >= maxPagesPerThread || len(scope.pages) >= maxPagesPerWorkspace || countPagesLocked(m.scopes) >= maxPagesTotal {
		m.mu.Unlock()
		pageCancel()
		return
	}
	scope.pages[p.id] = p
	m.mu.Unlock()
	if m.pageAdopted != nil {
		m.pageAdopted()
	}
}

func (m *Manager) removeDestroyedTarget(targetID target.ID) {
	m.mu.Lock()
	var emptyWorkspace string
	for workspace, scope := range m.scopes {
		for id, p := range scope.pages {
			if p.target == targetID {
				delete(scope.pages, id)
				if len(scope.pages) == 0 {
					emptyWorkspace = workspace
				}
				break
			}
		}
		if emptyWorkspace != "" {
			break
		}
	}
	m.mu.Unlock()
	if emptyWorkspace != "" {
		_ = m.disposeScope(context.Background(), emptyWorkspace)
	}
}

func (m *Manager) disposeScope(ctx context.Context, workspace string) error {
	m.mu.Lock()
	scope := m.scopes[workspace]
	if scope == nil {
		m.mu.Unlock()
		return nil
	}
	delete(m.scopes, workspace)
	persist := m.config.PersistSiteData
	noScopes := len(m.scopes) == 0
	m.mu.Unlock()
	var saveErr error
	if persist {
		saveErr = m.checkpointScope(ctx, scope)
	}
	disposeErr := scope.cancel(ctx)
	if noScopes {
		m.scheduleIdleClose()
	}
	return errors.Join(saveErr, disposeErr)
}

func (m *Manager) closeBrowser(caller context.Context, save bool) error {
	m.mu.Lock()
	startupCancel := m.startupCancel
	m.mu.Unlock()
	if startupCancel != nil {
		startupCancel()
	}
	m.startMu.Lock()
	defer m.startMu.Unlock()
	m.mu.Lock()
	scopes := make([]*workspaceScope, 0, len(m.scopes))
	for _, scope := range m.scopes {
		scopes = append(scopes, scope)
	}
	m.scopes = make(map[string]*workspaceScope)
	persist := save && m.config.PersistSiteData
	if m.idleTimer != nil {
		m.idleTimer.Stop()
		m.idleTimer = nil
	}
	browserCancel, allocCancel := m.browserCancel, m.allocCancel
	m.browserCtx, m.browserCancel = nil, nil
	m.allocCtx, m.allocCancel = nil, nil
	m.mu.Unlock()
	closeCtx, closeCancel := context.WithTimeout(caller, 20*time.Second)
	defer closeCancel()
	errs := make(chan error, len(scopes))
	var wg sync.WaitGroup
	for _, scope := range scopes {
		wg.Go(func() {
			var err error
			if persist {
				err = m.checkpointScope(closeCtx, scope)
			}
			err = errors.Join(err, scope.cancel(closeCtx))
			if err != nil {
				errs <- err
			}
		})
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	timedOut := false
	select {
	case <-done:
	case <-closeCtx.Done():
		timedOut = true
	}
	if browserCancel != nil {
		browserCancel()
	}
	if allocCancel != nil {
		allocCancel()
	}
	if timedOut {
		// Browser cancellation releases any CDP calls that were still waiting.
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
	joined := make([]error, 0, len(errs)+1)
	for len(errs) > 0 {
		err := <-errs
		joined = append(joined, err)
	}
	if timedOut {
		joined = append(joined, closeCtx.Err())
	} else if caller.Err() != nil {
		joined = append(joined, caller.Err())
	}
	return errors.Join(joined...)
}

func (m *Manager) scheduleIdleClose() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.idleTimer != nil {
		m.idleTimer.Stop()
	}
	m.idleTimer = time.AfterFunc(idleBrowserDelay, func() {
		m.mu.Lock()
		empty := len(m.scopes) == 0
		m.mu.Unlock()
		if empty {
			_ = m.closeBrowser(context.Background(), true)
		}
	})
}
