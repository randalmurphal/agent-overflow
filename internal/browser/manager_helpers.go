package browser

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/chromedp"
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

func (p *managedPage) touch() { p.lastUse.Store(time.Now().UnixNano()) }

func operationContext(caller, pageCtx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(pageCtx, timeout)
	stop := context.AfterFunc(caller, cancel)
	return ctx, func() { stop(); cancel() }
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

func pageInfo(ctx context.Context, id string) (PageInfo, error) {
	var location, title string
	if err := chromedp.Run(ctx, chromedp.Location(&location), chromedp.Title(&title)); err != nil {
		return PageInfo{}, fmt.Errorf("browser: read page state: %w", err)
	}
	return PageInfo{ID: id, URL: location, Title: title}, nil
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
		_, err := m.authorizeFile(access, filepath.FromSlash(parsed.Path))
		return err == nil
	default:
		return false
	}
}
