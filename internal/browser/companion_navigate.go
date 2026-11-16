package browser

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// NavigateCompanion is the pane's address bar: a file address opens the file
// under the file authorization (OpenFile), anything else is a web address, a
// host, or a search. A navigation the user stopped (StopCompanionLoad) while
// it ran answers its own abort without an error; any other failure stands.
func (m *Manager) NavigateCompanion(ctx context.Context, access Access, pageID, address string) (PageInfo, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return PageInfo{}, fmt.Errorf("browser: address is required")
	}
	nav := &userNavigation{}
	info, err := m.navigateCompanion(context.WithValue(ctx, userNavigationKey{}, nav), access, pageID, address)
	if errors.Is(err, errNavigationAborted) && nav.wasStopped() {
		return info, nil
	}
	return info, err
}

// errNavigationAborted marks a navigation the engine abandoned because the
// load was stopped (CDP net::ERR_ABORTED). The WebKit engines end a stopped
// load without an error.
var errNavigationAborted = errors.New("browser: navigation aborted")

// userNavigation is one address-bar navigation. The page records it while
// the navigation holds the page (runUserNavigation), so a stop marks the
// navigation it actually interrupted and no other.
type userNavigation struct {
	stopped atomic.Bool
}

type userNavigationKey struct{}

func (n *userNavigation) wasStopped() bool { return n.stopped.Load() }

// runUserNavigation records the context's address-bar navigation as the one
// running on p; the result clears it. Call it holding p.mu. Registering and
// clearing take p.navMu, which a stop holds across its engine call
// (StopCompanionLoad), so neither happens while a stop is in flight: the
// navigation a stop marks is the one whose load it stopped, and a stopped
// navigation finishes only after its mark.
func (p *managedPage) runUserNavigation(ctx context.Context) func() {
	nav, _ := ctx.Value(userNavigationKey{}).(*userNavigation)
	if nav == nil {
		return func() {}
	}
	p.navMu.Lock()
	p.userNav = nav
	p.navMu.Unlock()
	return func() {
		p.navMu.Lock()
		p.userNav = nil
		p.navMu.Unlock()
	}
}

func (m *Manager) navigateCompanion(ctx context.Context, access Access, pageID, address string) (PageInfo, error) {
	target, isFile, err := fileAddress(address)
	if err != nil {
		return PageInfo{}, err
	}
	if isFile {
		path := target.path
		if target.windows != "" {
			if path, err = m.backendPathForWindows(ctx, target.windows); err != nil {
				return PageInfo{}, err
			}
		}
		return m.OpenFile(ctx, access, path, OpenOptions{PageID: pageID})
	}
	if filepath.IsAbs(address) {
		return m.OpenFile(ctx, access, address, OpenOptions{PageID: pageID})
	}
	workspaceFile := filepath.Join(access.Workspace, filepath.FromSlash(address))
	if info, err := os.Stat(workspaceFile); err == nil && info.Mode().IsRegular() {
		return m.OpenFile(ctx, access, workspaceFile, OpenOptions{PageID: pageID})
	}
	lower := strings.ToLower(address)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		host := address
		if before, _, ok := strings.Cut(address, ":"); ok {
			host = before
		}
		isHost := strings.Contains(host, ".") || strings.EqualFold(host, "localhost") || net.ParseIP(strings.Trim(host, "[]")) != nil
		if strings.ContainsAny(address, " \t\r\n") || !isHost {
			address = "https://www.google.com/search?q=" + url.QueryEscape(address)
		} else if strings.EqualFold(host, "localhost") || net.ParseIP(strings.Trim(host, "[]")) != nil {
			address = "http://" + address
		} else {
			address = "https://" + address
		}
	}
	return m.Open(ctx, access, address, OpenOptions{PageID: pageID})
}

// fileTarget is the file a typed address names: a path in the backend's own
// syntax, or a Windows path (a drive path or a UNC path such as
// \\wsl.localhost\<distro>\...) that only an engine rendering on Windows can
// map to one.
type fileTarget struct {
	path    string
	windows string
}

// fileAddress classifies a typed address. A Windows drive path (C:\x, C:/x),
// a UNC path, and a file URL of any of those or of a backend path are files;
// anything else is not, and the caller treats it as a web address. A file
// URL that cannot be read is an error, never a search.
func fileAddress(address string) (fileTarget, bool, error) {
	if isWindowsDrivePath(address) || strings.HasPrefix(address, `\\`) {
		return fileTarget{windows: strings.ReplaceAll(address, "/", `\`)}, true, nil
	}
	if len(address) < len("file:") || !strings.EqualFold(address[:len("file:")], "file:") {
		return fileTarget{}, false, nil
	}
	parsed, err := url.Parse(address)
	if err != nil {
		return fileTarget{}, false, fmt.Errorf("browser: %s is not a usable file address: %w", address, err)
	}
	host := parsed.Host
	if strings.EqualFold(host, "localhost") {
		host = ""
	}
	path := parsed.Path
	if parsed.Opaque != "" {
		path = parsed.Opaque
	}
	switch {
	case host != "":
		if strings.Trim(path, "/") == "" {
			return fileTarget{}, false, fmt.Errorf("browser: %s names no file", address)
		}
		return fileTarget{windows: `\\` + host + strings.ReplaceAll(path, "/", `\`)}, true, nil
	case isWindowsDrivePath(strings.TrimPrefix(path, "/")):
		return fileTarget{windows: strings.ReplaceAll(strings.TrimPrefix(path, "/"), "/", `\`)}, true, nil
	case strings.HasPrefix(path, "/"):
		return fileTarget{path: filepath.FromSlash(path)}, true, nil
	}
	return fileTarget{}, false, fmt.Errorf("browser: %s names no file", address)
}

func isWindowsDrivePath(s string) bool {
	if len(s) < 3 || s[1] != ':' || (s[2] != '\\' && s[2] != '/') {
		return false
	}
	letter := s[0] | 0x20
	return letter >= 'a' && letter <= 'z'
}

// backendPathForWindows resolves a Windows path to the backend path behind
// it. Only an engine whose renderer is on Windows (the WSL build's hosted
// WebView2) has that mapping (engineFileURL); its own translation answers,
// and a path it cannot reach (another distribution, a network share) is an
// error naming the path. Every other engine shares the backend's
// filesystem, where no Windows path exists.
func (m *Manager) backendPathForWindows(ctx context.Context, windowsPath string) (string, error) {
	engine, ok := m.engine.(engineFileURL)
	if !ok {
		return "", fmt.Errorf("browser: %s is a Windows path; this computer's browser opens files on its own filesystem only", windowsPath)
	}
	fileURL, err := windowsFileURL(windowsPath)
	if err == nil {
		var path string
		if path, err = engine.BackendFilePath(ctx, fileURL); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("browser: %s is not reachable from this computer's WSL distribution: %w", windowsPath, err)
}

// StopCompanionLoad is the pane's stop button: it stops the page's load in
// flight. It takes no operation lock, because the load it stops is often an
// address-bar navigation that holds the page's lock until its load ends.
// Once the engine has stopped, that navigation (and only it) answers its
// abort without an error (NavigateCompanion). A tab with no live page (being
// created, suspended) has nothing loading.
func (m *Manager) StopCompanionLoad(ctx context.Context, access Access, pageID string) error {
	pageID = strings.TrimSpace(pageID)
	p, _, err := m.lookupOwnedPage(access, pageID)
	if errors.Is(err, errPageNotFound) && m.hasTab(access.ThreadID, pageID) {
		return nil
	}
	if err != nil {
		return err
	}
	// navMu is held across the engine's stop (runUserNavigation): the
	// navigation registered now is the one loading until the stop returns.
	// It is taken only by address-bar navigations and stops, never from an
	// engine callback, so the engine may run its stop on its UI thread.
	p.navMu.Lock()
	defer p.navMu.Unlock()
	opCtx, cancel := operationContext(ctx, p.ctx, 5*time.Second)
	defer cancel()
	if err := p.driver.History(opCtx, "stop"); err != nil {
		return err
	}
	if p.userNav != nil {
		p.userNav.stopped.Store(true)
	}
	return nil
}
