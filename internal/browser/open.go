package browser

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

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
	return m.navigate(ctx, access, parsed.String(), opts, nil)
}

func (m *Manager) NewPage(ctx context.Context, access Access) (PageInfo, error) {
	p, err := m.pageForOpen(ctx, access, "", false)
	if err != nil {
		return PageInfo{}, err
	}
	if err := m.lockPageForOperation(ctx, p); err != nil {
		return PageInfo{}, err
	}
	defer p.mu.Unlock()
	opCtx, cancel := operationContext(ctx, p.ctx, 5*time.Second)
	defer cancel()
	info, _, err := m.readInfo(opCtx, p, pageListing{})
	if err == nil {
		m.pageUsed(p)
	}
	return info, err
}

func (m *Manager) OpenFile(ctx context.Context, access Access, path string, opts OpenOptions) (PageInfo, error) {
	return m.openFile(ctx, access, path, nil, opts)
}

// errDownloadReplaced refuses to open a download's file that was moved,
// changed or replaced since it finished.
var errDownloadReplaced = errors.New("was moved, changed or replaced since it downloaded")

// openFile opens path in a page. A file opened as a finished download's,
// expect naming that file as it finished or the authorization granting it
// only as a listed download's, opens only while it is still that file
// (sameDownloadedFile), checked in the page's operation lock immediately
// before the navigation, after any tab creation.
func (m *Manager) openFile(ctx context.Context, access Access, path string, expect os.FileInfo, opts OpenOptions) (PageInfo, error) {
	stillExpected := func(path string) error {
		current, err := os.Stat(path)
		if err != nil || !sameDownloadedFile(expect, current) {
			return fmt.Errorf("browser: %s %w", filepath.Base(path), errDownloadReplaced)
		}
		return nil
	}
	if expect != nil {
		// Its own reason, before the authorization, which a replaced row's
		// file no longer passes.
		if err := stillExpected(path); err != nil {
			return PageInfo{}, err
		}
	}
	resolved, granted, err := m.authorizeFileGrant(access.Caller(), path)
	if err != nil {
		return PageInfo{}, err
	}
	if expect == nil {
		expect = granted
	}
	var check func() error
	if expect != nil {
		check = func() error { return stillExpected(resolved) }
	}
	// The URL must name the file as the RENDERER sees it. An engine whose
	// renderer is on the other side of a machine boundary answers through
	// engineFileURL; everywhere else the backend path is the renderer path.
	fileURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(resolved)}).String()
	if engine, ok := m.engine.(engineFileURL); ok {
		if fileURL, err = engine.FileURL(ctx, resolved); err != nil {
			return PageInfo{}, err
		}
	}
	return m.navigate(ctx, access, fileURL, opts, check)
}

// navigate loads targetURL in the page opts names, or a new one. check, when
// set, runs in the page's operation lock immediately before the load and
// refuses it with its error.
func (m *Manager) navigate(ctx context.Context, access Access, targetURL string, opts OpenOptions, check func() error) (PageInfo, error) {
	p, err := m.pageForOpen(ctx, access, opts.PageID, true)
	if err != nil {
		return PageInfo{}, err
	}
	if err := m.lockPageForOperation(ctx, p); err != nil {
		return PageInfo{}, err
	}
	defer p.mu.Unlock()
	defer p.runUserNavigation(ctx)()
	opCtx, cancel := operationContext(ctx, p.ctx, operationTimeout)
	defer cancel()
	if check != nil {
		if err := check(); err != nil {
			return PageInfo{}, err
		}
	}
	if err := p.driver.Navigate(opCtx, targetURL); err != nil {
		return PageInfo{}, err
	}
	p.touch()
	info, _, err := m.readInfo(opCtx, p, pageListing{})
	if err == nil {
		m.recordRecentAddress(p, info)
	}
	m.pageUsed(p)
	return info, err
}

// pageForOpen answers the page an open navigates: the requested one, or a new
// page. A suspended page asked for by a caller about to navigate it is
// restored blank (skipLoad), since loading its old address first is wasted.
func (m *Manager) pageForOpen(ctx context.Context, access Access, requested string, skipLoad bool) (*managedPage, error) {
	if strings.TrimSpace(access.ThreadID) == "" || strings.TrimSpace(access.Workspace) == "" {
		return nil, fmt.Errorf("browser: invalid caller scope")
	}
	if requested = strings.TrimSpace(requested); requested != "" {
		p, _, err := m.resolvePage(ctx, access, requested, skipLoad)
		return p, err
	}
	return m.createPage(ctx, access)
}

// openPageLocked starts the engine if it is not running, applies the jar,
// workspace and process caps, and creates p's engine page in the jar its
// thread's setting names, laid out at its thread's viewport. It stamps p with
// the setting's generation and its workspace's authority, and returns with the scope reserved for p
// (scope.creating): the caller registers p or abandons it (abandonPage). The
// thread's cap is the caller's: a new page is admitted when its tab appears
// (createPage), and a restore's record already holds its thread's slot. A
// profile of the jar still being disposed is waited for, so two profiles
// never hold one jar's site data. admit, when set, decides the page's
// admission under m.mu, after any such wait and before the caps. The caller
// holds startMu.
func (m *Manager) openPageLocked(ctx context.Context, access Caller, p *managedPage, admit func() error) (*jarScope, error) {
	workspace, err := canonicalRoot(access.Workspace)
	if err != nil {
		return nil, fmt.Errorf("browser: resolve workspace: %w", err)
	}
	access.Workspace = workspace
	p.access = access
	if err := m.startLocked(ctx); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		m.scheduleIdleClose()
		return nil, err
	}
	if m.engine.DownloadSaving() != downloadsSavedByEngine {
		// Once per Manager: artifacts a download kept, here or in an earlier
		// version that kept this engine's downloads as artifacts, go after
		// artifactMaxAge.
		m.prepareArtifacts()
	}
	authorityDir := ""
	if m.downloadsToArtifacts() {
		authorityDir = m.workspaceDownloadDir(workspace)
		if err := os.MkdirAll(authorityDir, 0o700); err != nil {
			m.scheduleIdleClose()
			return nil, fmt.Errorf("browser: create download directory: %w", err)
		}
	}
	m.mu.Lock()
	var jar jarIdentity
	for {
		if m.closed {
			m.mu.Unlock()
			return nil, fmt.Errorf("browser: manager closed")
		}
		if m.idleTimer != nil {
			m.idleTimer.Stop()
			m.idleTimer = nil
		}
		if jar, err = m.jarForLocked(p.owner, access, jarKindOf(p.owner.session)); err != nil {
			m.mu.Unlock()
			m.scheduleIdleClose()
			return nil, err
		}
		disposing := m.disposing[jar.key]
		if m.scopes[jar.key] != nil || disposing == nil {
			break
		}
		m.mu.Unlock()
		select {
		case <-disposing:
		case <-ctx.Done():
			m.scheduleIdleClose()
			return nil, ctx.Err()
		}
		m.mu.Lock()
	}
	// Before the caps: a refused admission waits and tries again, where a
	// cap only its own wait would lift fails.
	if admit != nil {
		if err := admit(); err != nil {
			m.mu.Unlock()
			m.scheduleIdleClose()
			return nil, err
		}
	}
	scope := m.scopes[jar.key]
	if scope == nil && len(m.scopes)+len(m.disposing) >= maxWorkspaceContexts {
		m.mu.Unlock()
		return nil, fmt.Errorf("browser: browser profile limit reached (%d); close the pages of another workspace or cookie jar", maxWorkspaceContexts)
	}
	if load, closing := m.workspaceLoadLocked(workspace); load >= maxPagesPerWorkspace {
		m.mu.Unlock()
		return nil, fmt.Errorf("browser: page limit reached for workspace (%d)%s", maxPagesPerWorkspace, closingHint(closing))
	}
	if scope != nil && scopeLoadLocked(scope) >= maxPagesPerWorkspace {
		m.mu.Unlock()
		return nil, fmt.Errorf("browser: page limit reached for the cookie jar (%d)%s", maxPagesPerWorkspace, closingHint(len(scope.closing)))
	}
	if countPagesLocked(m.scopes) >= maxPagesTotal {
		m.mu.Unlock()
		return nil, fmt.Errorf("browser: process page limit reached (%d)", maxPagesTotal)
	}
	p.jarGen = p.owner.session.jarGen
	// The reservation keeps a concurrent close of the scope's last page from
	// disposing the profile this page is being created in, and the page's
	// workspace authority is taken in the same hold: pruneAuthoritiesLocked
	// keeps the authority of every reserved page, so a close elsewhere cannot
	// forget the counter this page holds.
	if scope != nil {
		scope.creating[p] = struct{}{}
		p.authority = m.authorityLocked(workspace, authorityDir)
	}
	m.mu.Unlock()

	if scope == nil {
		scope, err = m.createScope(jar)
		if err != nil {
			// The engine may be running for this page alone.
			m.scheduleIdleClose()
			return nil, err
		}
		scope.creating[p] = struct{}{}
		m.mu.Lock()
		m.scopes[jar.key] = scope
		p.authority = m.authorityLocked(workspace, authorityDir)
		m.mu.Unlock()
	}

	if err := m.newPage(ctx, scope, p); err != nil {
		m.mu.Lock()
		delete(scope.creating, p)
		release := m.releaseScopeLocked(scope)
		m.mu.Unlock()
		if release {
			err = errors.Join(err, asTeardown(m.disposeScope(context.Background(), scope)))
		}
		return nil, err
	}
	return scope, nil
}

// newPage creates p's engine page in scope, laid out at its thread's
// viewport.
func (m *Manager) newPage(ctx context.Context, scope *jarScope, p *managedPage) error {
	driver, err := scope.profile.NewPage(ctx, m.pageHooks(p))
	if err != nil {
		return err
	}
	// Under m.mu: the jar's policy resolves a request to the page being
	// created (jarAllows) while it loads, before it is registered.
	m.mu.Lock()
	p.attach(driver)
	held, hasHeld := m.takeHeldInfoLocked(driver.Handle())
	m.mu.Unlock()
	m.applyHeldInfo(p, held, hasHeld)
	if err := m.applyViewport(p); err != nil {
		driver.Close()
		return err
	}
	m.pageAttached(p)
	p.touch()
	return nil
}

// pageHooks binds one page's AO-owned state to the engine events its driver
// reports. Nothing here decides ownership or limits.
func (m *Manager) pageHooks(p *managedPage) pageHooks {
	return pageHooks{
		Console:           p.appendLog,
		PageURL:           func() string { return p.cachedInfo().URL },
		Allow:             func(rawURL string) bool { return m.navigationAllowed(p.access, rawURL) },
		DocumentCommitted: func() func() { return m.documentCommitted(p) },
		Loading:           func(loading bool, order uint64) { m.pageLoading(p, loading, order) },
		Info:              func(url, title string, order uint64) { m.pageInfoReport(p, url, title, order) },
		DocumentTitle:     func(url, title string) { m.pageDocumentTitle(p, url, title) },
	}
}

// createScope creates the profile of one jar. Its profile-level navigation
// policy answers with the authority of the page a request's frame belongs
// to, or its opener's (jarAllows). The engine writes the jar's downloads
// under jarDownloadingDir, apart from every workspace's finished ones, which
// authorizeFile opens.
func (m *Manager) createScope(jar jarIdentity) (*jarScope, error) {
	downloadDir := ""
	if m.downloadsToArtifacts() {
		downloadDir = filepath.Join(m.artifactRoot, jarDownloadingDir, jarDirName(jar.key))
		if err := os.MkdirAll(downloadDir, 0o700); err != nil {
			return nil, fmt.Errorf("browser: create download directory: %w", err)
		}
	}
	var stage func(string) (string, error)
	var releaseStaged func(string)
	if m.downloadsStagedHere() {
		stage, releaseStaged = m.stageDownload, m.releaseStagedDownload
	}
	scope := &jarScope{
		jar: jar, downloadDir: downloadDir,
		pages: make(map[string]*managedPage), closing: make(map[string]*pageClose),
		creating: make(map[*managedPage]struct{}),
	}
	profile, err := m.engine.NewProfile(context.Background(), profileOptions{
		Jar: jar.key, DownloadDir: downloadDir, StageDownload: stage, ReleaseStaged: releaseStaged, Ephemeral: jar.ephemeral,
		Allow: func(owners []frameID, rawURL string) (bool, bool) { return m.jarAllows(scope, owners, rawURL) },
	})
	if err != nil {
		return nil, err
	}
	scope.profile = profile
	return scope, nil
}

// startLocked starts the engine unless it is running. The caller holds
// startMu.
func (m *Manager) startLocked(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("browser: manager closed")
	}
	if !m.config.Enabled {
		m.mu.Unlock()
		return fmt.Errorf("browser: tools are disabled")
	}
	m.mu.Unlock()
	if m.engine.Running() {
		return nil
	}
	return m.engine.Start(ctx)
}
