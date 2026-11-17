package browser

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (m *Manager) ownedPages(threadID string) []*managedPage {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*managedPage
	for _, scope := range m.scopes {
		for _, p := range scope.pages {
			if p.owner == threadID {
				out = append(out, p)
			}
		}
	}
	return out
}

// errPageNotFound answers a page that is gone or that another thread owns.
var errPageNotFound = errors.New("browser: page not found")

// errViewClosed answers an operation that reached a native engine's page
// after the page closed: the view is never touched then.
var errViewClosed = errors.New("browser: the page is closed")

func (m *Manager) lookupOwnedPage(access Access, pageID string) (*managedPage, *jarScope, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, scope := range m.scopes {
		if p := scope.pages[pageID]; p != nil {
			if p.owner != access.ThreadID {
				return nil, nil, errPageNotFound
			}
			return p, scope, nil
		}
	}
	return nil, nil, errPageNotFound
}

// pageByHandleLocked resolves an engine handle to the page it drives. Caller
// holds m.mu.
func (m *Manager) pageByHandleLocked(handle string) *managedPage {
	for _, scope := range m.scopes {
		for _, p := range scope.pages {
			if p.driver.Handle() == handle {
				return p
			}
		}
	}
	return nil
}

func (m *Manager) workspaceForPage(pageID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, _ := m.livePageLocked(pageID); p != nil {
		return p.access.Workspace
	}
	return ""
}

// threadPageCountLocked counts the thread's tabs, live, suspended and
// pending, and its pages still closing: a suspended page holds no engine
// resource but keeps its slot in the thread's page cap, which is what bounds
// the saved pages a thread keeps, and a closed page holds its slot until its
// engine page is gone (lifecycle.go). A page a jar switch closed and the
// record it left under the same id are one tab, counted once; a closed
// record leaves the slot to the page until its teardown ends (SetJar). The
// caller holds m.mu.
func (m *Manager) threadPageCountLocked(threadID string) int {
	count := 0
	for _, scope := range m.scopes {
		for _, p := range scope.pages {
			if p.owner == threadID {
				count++
			}
		}
		for _, c := range scope.closing {
			if c.p.owner == threadID && m.suspended[c.p.id] == nil {
				count++
			}
		}
	}
	for _, pending := range m.pending {
		if pending.p.owner == threadID {
			count++
		}
	}
	for _, rec := range m.suspended {
		if rec.owner == threadID {
			count++
		}
	}
	return count
}

// scopeLoadLocked counts the engine pages a jar holds, is creating or is
// still closing: a jar is one profile, and it holds no more pages than one
// workspace may. Suspended pages hold none. The caller holds m.mu.
func scopeLoadLocked(scope *jarScope) int {
	return len(scope.pages) + len(scope.closing) + len(scope.creating)
}

// countPagesLocked counts the engine pages of every scope, the quantity the
// process cap bounds. The caller holds m.mu.
func countPagesLocked(scopes map[string]*jarScope) int {
	count := 0
	for _, scope := range scopes {
		count += scopeLoadLocked(scope)
	}
	return count
}

// ambiguousPageError names the thread's tabs, in tab order, for a call that
// omitted page_id while the thread has several.
func ambiguousPageError(tabs []threadTab) error {
	refs := make([]string, 0, len(tabs))
	for _, tab := range tabs {
		info := tab.info
		ref := info.ID
		if info.Label != "" {
			ref += " (" + info.Label + ")"
		} else if info.Title != "" {
			ref += " (" + truncateUTF8(info.Title, 80) + ")"
		}
		refs = append(refs, ref)
	}
	return fmt.Errorf("browser: page_id is required because this thread has %d open pages; call browser_pages and pass the intended page_id (%s)", len(tabs), strings.Join(refs, ", "))
}

func (p *managedPage) touch() { p.lastUse.Store(time.Now().UnixNano()) }

func operationContext(caller, pageCtx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(pageCtx, timeout)
	stop := context.AfterFunc(caller, cancel)
	return ctx, func() { stop(); cancel() }
}

// pageRead is what a read of a page's info found: the address and title it
// read, and whether they were committed, which they are unless a write that
// began later (an engine report) set the address during the read.
type pageRead struct {
	Location pageLocation
	Located  bool
}

// readInfo reads one page's live URL, title, and history state and commits
// them with listing as a read stamped before it asked (commitRead). It
// answers the info the tab shows after, whose fields a write that began
// during the read may hold instead, and what it read.
func (m *Manager) readInfo(ctx context.Context, p *managedPage, listing pageListing) (PageInfo, pageRead, error) {
	stamp := p.readStamp()
	location, title, err := p.driver.Info(ctx)
	if err != nil {
		return PageInfo{}, pageRead{}, err
	}
	back, forward, err := p.driver.HistoryState(ctx)
	if err != nil {
		return PageInfo{}, pageRead{}, err
	}
	read := pageRead{Location: pageLocation{URL: location, Title: title}}
	info, located := m.commitRead(p, pageUpdate{
		Location: &read.Location,
		History:  &pageHistory{CanGoBack: back, CanGoForward: forward},
		Listing:  listing,
	}, stamp)
	read.Located = located
	return info, read, nil
}

// maxSettleReads bounds settledAddress on a page that keeps navigating.
const maxSettleReads = 4

// settledAddress reads p's address until a read commits with no engine
// report landing during it, which makes that read the engine's address as
// of then: a report committed during a read may be newer than the read's
// answer (the page navigated itself) or older (delivered late), and a stamp
// alone cannot tell which, but a read is at least as new as every report
// committed before it began. After maxSettleReads it answers the last read.
// The caller holds the page-operation lock, so no operation moves the page
// between the reads.
func (m *Manager) settledAddress(ctx context.Context, p *managedPage) (pageLocation, error) {
	for attempt := 1; ; attempt++ {
		_, read, err := m.readInfo(ctx, p, pageListing{})
		if err != nil {
			return pageLocation{}, err
		}
		if read.Located {
			return read.Location, nil
		}
		if attempt == maxSettleReads {
			log.Printf("browser: page %s reported a new address during each of %d reads; keeping the last read, %s", p.id, maxSettleReads, read.Location.URL)
			return read.Location, nil
		}
	}
}

// truncateUTF8 bounds value to limit bytes at a rune boundary. A truncated
// result is a copy, so keeping it does not keep the engine's whole value.
func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	for limit > 0 && (value[limit]&0xc0) == 0x80 {
		limit--
	}
	return strings.Clone(value[:limit])
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

// validateScrollDelta bounds every scroll a tool can request, wherever the
// gesture originates.
func validateScrollDelta(x, y float64) error {
	if !finite(x) || !finite(y) || x < -100_000 || x > 100_000 || y < -100_000 || y > 100_000 {
		return fmt.Errorf("browser: scroll delta is out of range")
	}
	return nil
}

func validatePoint(point Point) error {
	if !finite(point.X) || !finite(point.Y) || point.X < 0 || point.Y < 0 || point.X > maxCompanionWidth || point.Y > maxCompanionHeight {
		return fmt.Errorf("browser: pointer coordinates are outside the bounded viewport")
	}
	return nil
}

func canonicalRoot(path string) (string, error) {
	abs, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return filepath.Clean(abs), nil
}

func (m *Manager) authorizeFile(access Access, path string) (string, error) {
	resolved, err := canonicalRoot(path)
	if err != nil {
		return "", fmt.Errorf("browser: resolve file: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("browser: open file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("browser: local path is not a regular file")
	}
	m.mu.Lock()
	allowOutside := m.config.AllowOutsideWorkspace
	m.mu.Unlock()
	if allowOutside {
		return resolved, nil
	}
	roots := []string{access.Workspace, access.ProjectRoot}
	// The workspace's own downloads, which the pane opens in a tab
	// (OpenThreadDownload): its artifact directory on an engine whose
	// downloads stay AO's, and otherwise the files of its listed finished
	// downloads in the user's Downloads folder, never the folder
	// (download_files.go).
	if workspace, err := canonicalRoot(access.Workspace); err == nil && strings.TrimSpace(access.Workspace) != "" {
		if m.downloadsToArtifacts() {
			roots = append(roots, m.workspaceDownloadDir(workspace))
		} else if m.listedDownloadAllows(workspace, resolved) {
			return resolved, nil
		}
	}
	for _, root := range roots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		canonical, err := canonicalRoot(root)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(canonical, resolved)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("browser: file is outside the workspace; enable outside-workspace file access to open it")
}

func (m *Manager) navigationAllowed(access Access, rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "about", "data", "blob":
		return true
	case "chrome-extension":
		// Chrome's built-in PDF viewer. Keep arbitrary installed-extension
		// pages out of the navigation allow-list.
		return strings.EqualFold(parsed.Host, "mhjfbmdgcfjbbpaeojofohoefgiehjai")
	case "file":
		backendPath := filepath.FromSlash(parsed.Path)
		if engine, ok := m.engine.(engineFileURL); ok {
			// The URL is in the RENDERER's form (file:///C:/...,
			// file://wsl.localhost/...); authorize the backend path behind
			// it. A URL the engine cannot map names a filesystem the
			// backend cannot reach, so it stays blocked.
			mapped, err := engine.BackendFilePath(context.Background(), rawURL)
			if err != nil {
				return false
			}
			backendPath = mapped
		}
		_, err := m.authorizeFile(access, backendPath)
		return err == nil
	default:
		return false
	}
}
