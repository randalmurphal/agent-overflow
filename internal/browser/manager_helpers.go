package browser

import (
	"context"
	"fmt"
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

func (m *Manager) lookupOwnedPage(access Access, pageID string) (*managedPage, *workspaceScope, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, scope := range m.scopes {
		if p := scope.pages[pageID]; p != nil {
			if p.owner != access.ThreadID {
				return nil, nil, fmt.Errorf("browser: page not found")
			}
			return p, scope, nil
		}
	}
	return nil, nil, fmt.Errorf("browser: page not found")
}

func (m *Manager) workspaceForPage(pageID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for workspace, scope := range m.scopes {
		if scope.pages[pageID] != nil {
			return workspace
		}
	}
	return ""
}

func countOwnedPagesLocked(scopes map[string]*workspaceScope, threadID string) int {
	count := 0
	for _, scope := range scopes {
		for _, p := range scope.pages {
			if p.owner == threadID {
				count++
			}
		}
	}
	return count
}

func countPagesLocked(scopes map[string]*workspaceScope) int {
	count := 0
	for _, scope := range scopes {
		count += len(scope.pages)
	}
	return count
}

func ambiguousPageError(pages []*managedPage) error {
	sortPagesByTabOrder(pages)
	refs := make([]string, 0, len(pages))
	for _, p := range pages {
		info := p.cachedInfo()
		ref := info.ID
		if info.Label != "" {
			ref += " (" + info.Label + ")"
		} else if info.Title != "" {
			ref += " (" + truncateUTF8(info.Title, 80) + ")"
		}
		refs = append(refs, ref)
	}
	return fmt.Errorf("browser: page_id is required because this thread has %d open pages; call browser_pages and pass the intended page_id (%s)", len(pages), strings.Join(refs, ", "))
}

func (p *managedPage) touch() { p.lastUse.Store(time.Now().UnixNano()) }

func operationContext(caller, pageCtx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(pageCtx, timeout)
	stop := context.AfterFunc(caller, cancel)
	return ctx, func() { stop(); cancel() }
}

// pageInfo reads and bounds one page's live URL, title, and history state.
func (m *Manager) pageInfo(ctx context.Context, p *managedPage) (PageInfo, error) {
	location, title, err := p.driver.Info(ctx)
	if err != nil {
		return PageInfo{}, err
	}
	back, forward, err := p.driver.HistoryState(ctx)
	if err != nil {
		return PageInfo{}, err
	}
	return PageInfo{
		ID:        p.id,
		URL:       truncateUTF8(location, maxBrowserURLBytes),
		Title:     truncateUTF8(title, maxBrowserTitleBytes),
		CanGoBack: back, CanGoForward: forward,
	}, nil
}

func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	for limit > 0 && (value[limit]&0xc0) == 0x80 {
		limit--
	}
	return value[:limit]
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
	for _, root := range []string{access.Workspace, access.ProjectRoot} {
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
