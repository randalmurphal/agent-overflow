package browser

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/chromium"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// cdpEngine is the managed headless Chrome half of the seam: one installed
// browser process, one BrowserContext per workspace profile, and the
// browser-level event stream every page-level router is fed from.
type cdpEngine struct {
	installer *chromium.Installer
	events    engineEvents

	mu            sync.Mutex
	allocCtx      context.Context
	allocCancel   context.CancelFunc
	browserCtx    context.Context
	browserCancel context.CancelFunc
	startupCancel context.CancelFunc
}

func newCDPEngine(installer *chromium.Installer, events engineEvents) *cdpEngine {
	return &cdpEngine{installer: installer, events: events}
}

func (e *cdpEngine) Running() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.browserCtx != nil && e.browserCtx.Err() == nil
}

func (e *cdpEngine) Interrupt() {
	e.mu.Lock()
	cancel := e.startupCancel
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (e *cdpEngine) Stop() {
	e.mu.Lock()
	browserCancel, allocCancel := e.browserCancel, e.allocCancel
	e.browserCtx, e.browserCancel = nil, nil
	e.allocCtx, e.allocCancel = nil, nil
	e.mu.Unlock()
	if browserCancel != nil {
		browserCancel()
	}
	if allocCancel != nil {
		allocCancel()
	}
}

func (e *cdpEngine) Start(context.Context) error {
	if e.installer == nil {
		return fmt.Errorf("browser: installer unavailable")
	}
	// Provider MCP clients commonly cap a tool call near one minute, while the
	// first managed-Chrome download can take longer. Once requested, let the
	// bounded install finish even if that individual HTTP request disconnects;
	// the next call then reuses the warm artifact/process. Disabling the feature
	// or shutting down explicitly cancels this startup context.
	startupCtx, startupCancel := context.WithTimeout(context.Background(), 30*time.Minute)
	e.mu.Lock()
	e.startupCancel = startupCancel
	e.mu.Unlock()
	defer func() {
		startupCancel()
		e.mu.Lock()
		e.startupCancel = nil
		e.mu.Unlock()
	}()
	installed, err := e.installer.Install(startupCtx)
	if err != nil {
		return fmt.Errorf("browser: install Chrome: %w", err)
	}
	opts := []chromedp.ExecAllocatorOption{
		chromedp.ExecPath(installed.BinaryPath),
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.Flag("enable-automation", true),
		chromedp.Flag("disable-default-apps", true),
		chromedp.Flag("disable-background-networking", true),
		chromedp.Flag("disable-breakpad", true),
		chromedp.Flag("disable-component-update", true),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("disable-sync", true),
		chromedp.Flag("force-color-profile", "srgb"),
		chromedp.Flag("password-store", "basic"),
		// ExecAllocator otherwise silently adds --no-sandbox when the parent
		// runs as root. Failing to launch is safer than weakening isolation.
		chromedp.Flag("no-sandbox", false),
		chromedp.ModifyCmdFunc(func(*exec.Cmd) {}),
		chromedp.CombinedOutput(browserLogWriter{}),
		chromedp.Flag("headless", "new"),
	}
	if runtime.GOOS == "darwin" {
		// Chrome's temporary automation profile has no durable secrets. AO
		// persists the selected cookie/storage subset itself, encrypted. Avoid
		// Chromium Safe Storage prompts for an ephemeral profile.
		opts = append(opts, chromedp.Flag("use-mock-keychain", true))
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
	chromedp.ListenBrowser(browserCtx, e.dispatch)
	e.mu.Lock()
	e.allocCtx, e.allocCancel = allocCtx, allocCancel
	e.browserCtx, e.browserCancel = browserCtx, browserCancel
	e.mu.Unlock()
	return nil
}

func (e *cdpEngine) dispatch(ev any) {
	switch event := ev.(type) {
	case *target.EventTargetCreated:
		if event.TargetInfo != nil && event.TargetInfo.Type == "page" && event.TargetInfo.OpenerID != "" {
			info := *event.TargetInfo
			go e.events.PopupOpened(enginePopup{
				Profile: string(info.BrowserContextID), Opener: string(info.OpenerID),
				Handle: string(info.TargetID), URL: info.URL, Title: info.Title,
			})
		}
	case *target.EventTargetDestroyed:
		go e.events.PageClosed(string(event.TargetID))
	case *target.EventTargetInfoChanged:
		if event.TargetInfo != nil && event.TargetInfo.Type == "page" {
			e.events.PageInfoChanged(string(event.TargetInfo.TargetID), event.TargetInfo.URL, event.TargetInfo.Title)
		}
	case *cdpbrowser.EventDownloadWillBegin:
		e.events.DownloadStarted(downloadStart{
			Frame: string(event.FrameID), ID: event.GUID,
			URL: event.URL, SuggestedName: event.SuggestedFilename,
		})
	case *cdpbrowser.EventDownloadProgress:
		state := downloadInProgress
		switch event.State {
		case cdpbrowser.DownloadProgressStateCompleted:
			state = downloadCompleted
		case cdpbrowser.DownloadProgressStateCanceled:
			state = downloadCanceled
		}
		e.events.DownloadProgress(downloadProgress{
			ID: event.GUID, Received: event.ReceivedBytes, State: state, FilePath: event.FilePath,
		})
	}
}

func (e *cdpEngine) DiscardPage(handle string) {
	e.mu.Lock()
	browserCtx := e.browserCtx
	e.mu.Unlock()
	if browserCtx == nil {
		return
	}
	_ = target.CloseTarget(target.ID(handle)).Do(browserCommandContext(browserCtx))
}

func (e *cdpEngine) NewProfile(_ context.Context, opts profileOptions) (engineProfile, error) {
	e.mu.Lock()
	browserCtx := e.browserCtx
	e.mu.Unlock()
	if browserCtx == nil {
		return nil, fmt.Errorf("browser: process unavailable")
	}
	contextID, err := target.CreateBrowserContext().WithDisposeOnDetach(false).Do(browserCommandContext(browserCtx))
	if err != nil {
		return nil, fmt.Errorf("browser: create workspace context: %w", err)
	}
	profile := &cdpProfile{ctx: browserCtx, contextID: contextID}
	if err := cdpbrowser.SetDownloadBehavior(cdpbrowser.SetDownloadBehaviorBehaviorAllowAndName).WithDownloadPath(opts.DownloadDir).WithEventsEnabled(true).WithBrowserContextID(contextID).Do(browserCommandContext(browserCtx)); err != nil {
		_ = profile.Dispose(context.Background())
		return nil, fmt.Errorf("browser: configure downloads: %w", err)
	}
	// Browser tools must never turn a website permission prompt into ambient
	// access to the user's machine. CDP permission names unsupported by a future
	// Chrome build are logged and remain at Chrome's safer default (prompt).
	for _, name := range []string{"geolocation", "notifications", "midi", "camera", "microphone", "clipboard-read", "clipboard-write", "idle-detection"} {
		if err := cdpbrowser.SetPermission(&cdpbrowser.PermissionDescriptor{Name: name}, cdpbrowser.PermissionSettingDenied).WithBrowserContextID(contextID).Do(browserCommandContext(browserCtx)); err != nil {
			log.Printf("browser: deny %s permission: %v", name, err)
		}
	}
	if len(opts.Cookies) > 0 {
		if err := storage.SetCookies(opts.Cookies).WithBrowserContextID(contextID).Do(browserCommandContext(browserCtx)); err != nil {
			_ = profile.Dispose(context.Background())
			return nil, fmt.Errorf("browser: restore cookies: %w", err)
		}
	}
	return profile, nil
}

// cdpProfile is one workspace's incognito BrowserContext.
type cdpProfile struct {
	ctx       context.Context
	contextID cdp.BrowserContextID

	mu     sync.Mutex
	opened bool
}

func (p *cdpProfile) Handle() string { return string(p.contextID) }

func (p *cdpProfile) NewPage(_ context.Context, hooks pageHooks, restore map[string]map[string]string) (pageDriver, error) {
	p.mu.Lock()
	first := !p.opened
	p.opened = true
	p.mu.Unlock()
	var pageCtx context.Context
	var pageCancel context.CancelFunc
	if first {
		// A new incognito profile has no Browser window yet. Current full
		// Chrome rejects Target.createTarget's explicit newWindow=false in that
		// state, so create the context's first target as a new window and attach
		// to it. Later pages can be normal tabs in that window.
		targetID, createErr := target.CreateTarget("about:blank").WithBrowserContextID(p.contextID).WithNewWindow(true).Do(browserCommandContext(p.ctx))
		if createErr != nil {
			return nil, fmt.Errorf("browser: create workspace window: %w", createErr)
		}
		pageCtx, pageCancel = chromedp.NewContext(p.ctx, chromedp.WithTargetID(targetID))
	} else {
		pageCtx, pageCancel = chromedp.NewContext(p.ctx, chromedp.WithExistingBrowserContext(p.contextID))
	}
	return startCDPPage(p.ctx, pageCtx, pageCancel, hooks, restore)
}

func (p *cdpProfile) AttachPage(_ context.Context, handle string, hooks pageHooks, restore map[string]map[string]string) (pageDriver, error) {
	pageCtx, pageCancel := chromedp.NewContext(p.ctx, chromedp.WithTargetID(target.ID(handle)))
	return startCDPPage(p.ctx, pageCtx, pageCancel, hooks, restore)
}

func (p *cdpProfile) Cookies(caller context.Context) ([]*network.CookieParam, error) {
	ctx, cancel := operationContext(caller, p.ctx, 3*time.Second)
	defer cancel()
	cookies, err := storage.GetCookies().WithBrowserContextID(p.contextID).Do(browserCommandContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("browser: checkpoint cookies: %w", err)
	}
	return cookieParams(cookies), nil
}

func (p *cdpProfile) CancelDownload(id string) {
	_ = cdpbrowser.CancelDownload(id).WithBrowserContextID(p.contextID).Do(browserCommandContext(p.ctx))
}

func (p *cdpProfile) Dispose(caller context.Context) error {
	ctx, cancel := operationContext(caller, p.ctx, 5*time.Second)
	defer cancel()
	return target.DisposeBrowserContext(p.contextID).Do(browserCommandContext(ctx))
}

func browserCommandContext(ctx context.Context) context.Context {
	chromedpContext := chromedp.FromContext(ctx)
	if chromedpContext == nil || chromedpContext.Browser == nil {
		return ctx
	}
	return cdp.WithExecutor(ctx, chromedpContext.Browser)
}

func targetCommandContext(ctx context.Context) context.Context {
	chromedpContext := chromedp.FromContext(ctx)
	if chromedpContext == nil || chromedpContext.Target == nil {
		return ctx
	}
	return cdp.WithExecutor(ctx, chromedpContext.Target)
}

type browserLogWriter struct{}

func (browserLogWriter) Write(data []byte) (int, error) {
	for _, line := range strings.Split(string(data), "\n") {
		message := strings.TrimSpace(line)
		if message == "" || strings.Contains(message, "CVDisplayLinkCreateWithCGDisplay failed") || strings.HasPrefix(message, "DevTools listening on ") {
			continue
		}
		log.Printf("browser: chrome: %s", message)
	}
	return len(data), nil
}
