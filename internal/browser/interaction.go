package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

func (m *Manager) Pointer(ctx context.Context, access Access, opts PointerOptions) (PageInfo, error) {
	if err := m.bound(access); err != nil {
		return PageInfo{}, err
	}
	p, _, err := m.lookupOrSelectPage(ctx, access, opts.PageID)
	if err != nil {
		return PageInfo{}, err
	}
	if err := m.lockPageForOperation(ctx, p); err != nil {
		return PageInfo{}, err
	}
	defer p.mu.Unlock()
	opCtx, cancel := operationContext(ctx, p.ctx, operationTimeout)
	defer cancel()
	if len(opts.Path) > 100 {
		return PageInfo{}, fmt.Errorf("browser: drag path exceeds 100 points")
	}
	if strings.ToLower(strings.TrimSpace(opts.Action)) != "drag" {
		if err := validatePoint(Point{X: opts.X, Y: opts.Y}); err != nil {
			return PageInfo{}, err
		}
	}
	if err := p.driver.Pointer(opCtx, opts); err != nil {
		return PageInfo{}, err
	}
	return m.finishPageOperation(opCtx, p)
}

func (m *Manager) DOMAction(ctx context.Context, access Access, opts DOMActionOptions) (any, error) {
	if err := m.bound(access); err != nil {
		return nil, err
	}
	if len(opts.Text) > maxBrowserInputBytes {
		return nil, fmt.Errorf("browser: input text exceeds %d bytes", maxBrowserInputBytes)
	}
	action := strings.ToLower(strings.TrimSpace(opts.Action))
	if action == "get_visible_dom" {
		return m.Snapshot(ctx, access, opts.PageID)
	}
	if action == "type" {
		p, _, lookupErr := m.lookupOrSelectPage(ctx, access, opts.PageID)
		if lookupErr != nil {
			return nil, lookupErr
		}
		if err := m.lockPageForOperation(ctx, p); err != nil {
			return nil, err
		}
		defer p.mu.Unlock()
		opCtx, cancel := operationContext(ctx, p.ctx, operationTimeout)
		defer cancel()
		if runErr := p.driver.TypeText(opCtx, opts.Text); runErr != nil {
			return nil, runErr
		}
		return m.finishPageOperation(opCtx, p)
	}
	if action == "keypress" {
		key := strings.TrimSpace(opts.Key)
		if key == "" {
			if len(opts.Keys) == 0 || len(opts.Keys) > 10 {
				return nil, fmt.Errorf("browser: keypress requires between 1 and 10 keys")
			}
			key = strings.Join(opts.Keys, "+")
		}
		return m.Press(ctx, access, opts.PageID, key)
	}
	if action == "scroll" && strings.TrimSpace(opts.NodeID) == "" {
		return m.Scroll(ctx, access, opts.PageID, "", opts.X, opts.Y)
	}
	p, _, lookupErr := m.lookupOrSelectPage(ctx, access, opts.PageID)
	if lookupErr != nil {
		return nil, lookupErr
	}
	ref, err := p.nodeReference(opts.NodeID)
	if err != nil {
		return nil, err
	}
	switch action {
	case "click", "double_click":
		return m.Locator(ctx, access, LocatorOptions{PageID: opts.PageID, Locator: Locator{CSS: ref.Selector, Frames: ref.Frames}, Action: action})
	case "scroll":
		return m.scrollNodeReference(ctx, p, ref, opts.X, opts.Y)
	default:
		return nil, fmt.Errorf("browser: DOM action must be get_visible_dom, click, double_click, type, keypress, or scroll")
	}
}

func (m *Manager) scrollNodeReference(ctx context.Context, p *managedPage, ref nodeReference, x, y float64) (PageInfo, error) {
	if err := validateScrollDelta(x, y); err != nil {
		return PageInfo{}, err
	}
	if err := m.lockPageForOperation(ctx, p); err != nil {
		return PageInfo{}, err
	}
	defer p.mu.Unlock()
	opCtx, cancel := operationContext(ctx, p.ctx, operationTimeout)
	defer cancel()
	if err := p.driver.ScrollNode(opCtx, ref, x, y); err != nil {
		return PageInfo{}, err
	}
	return m.finishPageOperation(opCtx, p)
}

func (m *Manager) Clipboard(ctx context.Context, access Access, opts ClipboardOptions) (any, error) {
	if err := m.bound(access); err != nil {
		return nil, err
	}
	p, _, err := m.lookupOrSelectPage(ctx, access, opts.PageID)
	if err != nil {
		return nil, err
	}
	p.clipboardMu.Lock()
	defer p.clipboardMu.Unlock()
	switch strings.ToLower(strings.TrimSpace(opts.Action)) {
	case "read":
		return cloneClipboard(p.clipboard), nil
	case "read_text":
		for _, item := range p.clipboard {
			for _, entry := range item.Entries {
				if entry.MIMEType == "text/plain" {
					return entry.Text, nil
				}
			}
		}
		return "", nil
	case "write_text":
		if len(opts.Text) > maxClipboardBytes {
			return nil, fmt.Errorf("browser: clipboard text exceeds %d bytes", maxClipboardBytes)
		}
		p.clipboard = []ClipboardItem{{Entries: []ClipboardEntry{{MIMEType: "text/plain", Text: opts.Text}}}}
		return map[string]any{"written": true, "bytes": len(opts.Text)}, nil
	case "write":
		if len(opts.Items) > 100 {
			return nil, fmt.Errorf("browser: clipboard exceeds 100 items")
		}
		encoded, marshalErr := json.Marshal(opts.Items)
		if marshalErr != nil || len(encoded) > maxClipboardBytes {
			return nil, fmt.Errorf("browser: clipboard items exceed %d bytes", maxClipboardBytes)
		}
		for _, item := range opts.Items {
			if len(item.Entries) > 100 {
				return nil, fmt.Errorf("browser: clipboard item exceeds 100 entries")
			}
			if item.PresentationStyle != "" && item.PresentationStyle != "unspecified" && item.PresentationStyle != "inline" && item.PresentationStyle != "attachment" {
				return nil, fmt.Errorf("browser: invalid clipboard presentation style %q", item.PresentationStyle)
			}
			for _, entry := range item.Entries {
				if entry.MIMEType == "" {
					return nil, fmt.Errorf("browser: clipboard MIME type is required")
				}
				if entry.Base64 != "" {
					if _, decodeErr := base64.StdEncoding.DecodeString(entry.Base64); decodeErr != nil {
						return nil, fmt.Errorf("browser: invalid clipboard base64")
					}
				}
			}
		}
		p.clipboard = cloneClipboard(opts.Items)
		return map[string]any{"written": true, "bytes": len(encoded)}, nil
	default:
		return nil, fmt.Errorf("browser: clipboard action must be read, read_text, write, or write_text")
	}
}

func cloneClipboard(items []ClipboardItem) []ClipboardItem {
	encoded, _ := json.Marshal(items)
	var cloned []ClipboardItem
	_ = json.Unmarshal(encoded, &cloned)
	return cloned
}

func (m *Manager) WaitAdvanced(ctx context.Context, access Access, opts WaitOptions) (PageInfo, error) {
	if err := m.bound(access); err != nil {
		return PageInfo{}, err
	}
	p, _, err := m.lookupOrSelectPage(ctx, access, opts.PageID)
	if err != nil {
		return PageInfo{}, err
	}
	if err := m.lockPageForOperation(ctx, p); err != nil {
		return PageInfo{}, err
	}
	defer p.mu.Unlock()
	timeout, err := boundedTimeout(opts.TimeoutMS)
	if err != nil {
		return PageInfo{}, err
	}
	if opts.TimeoutMS == 0 && opts.Milliseconds > 0 {
		timeout = time.Duration(opts.Milliseconds+1000) * time.Millisecond
	}
	opCtx, cancel := operationContext(ctx, p.ctx, timeout)
	defer cancel()
	if opts.Milliseconds < 0 || opts.Milliseconds > 30_000 {
		return PageInfo{}, fmt.Errorf("browser: wait duration must be between 0 and 30000 ms")
	}
	if opts.Milliseconds > 0 {
		timer := time.NewTimer(time.Duration(opts.Milliseconds) * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-opCtx.Done():
			return PageInfo{}, opCtx.Err()
		}
	}
	if opts.Selector != "" {
		opts.Locator = &Locator{CSS: opts.Selector}
	}
	if opts.Locator != nil {
		if _, err := m.waitLocator(opCtx, p, *opts.Locator, opts.State); err != nil {
			return PageInfo{}, err
		}
	}
	if opts.URL != "" || opts.LoadState != "" {
		if err := m.waitForPage(opCtx, p, opts.URL, opts.LoadState); err != nil {
			return PageInfo{}, err
		}
	}
	if opts.Milliseconds == 0 && opts.Locator == nil && opts.URL == "" && opts.LoadState == "" {
		return PageInfo{}, fmt.Errorf("browser: wait condition is required")
	}
	return m.finishPageOperation(opCtx, p)
}

// waitForPage polls the driver's load-state sample. The vocabulary and the URL
// glob are the tool's contract; only the sampling is engine-specific.
func (m *Manager) waitForPage(ctx context.Context, p *managedPage, urlPattern, loadState string) error {
	state := strings.ToLower(strings.TrimSpace(loadState))
	if state == "" {
		state = "load"
	}
	if state != "commit" && state != "domcontentloaded" && state != "load" && state != "networkidle" {
		return fmt.Errorf("browser: load state must be commit, domcontentloaded, load, or networkidle")
	}
	matcher, err := globMatcher(urlPattern)
	if err != nil {
		return err
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if status, statusErr := p.driver.PageStatus(ctx); statusErr == nil && matchStatus(status, matcher, state) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("browser: wait for page: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// pageNavigation tells p's navigations apart: its engine's mark and the
// documents its main frame committed (docEpoch), which a reload advances
// where an engine's mark (WebKit's address and history length) stays.
type pageNavigation struct {
	mark    navigationMark
	commits uint64
}

// readNavigation reads p's navigation; the commits are read after the mark,
// so a commit before the read is part of it.
func readNavigation(ctx context.Context, p *managedPage) (pageNavigation, error) {
	mark, err := p.driver.NavigationMark(ctx)
	if err != nil {
		return pageNavigation{}, err
	}
	return pageNavigation{mark: mark, commits: p.docEpoch.Load()}, nil
}

// movedFrom reports whether a navigation happened between before and n.
func (n pageNavigation) movedFrom(before pageNavigation) bool {
	if n.commits != before.commits || n.mark.URL != before.mark.URL {
		return true
	}
	return before.mark.Loader != "" && n.mark.Loader != "" && n.mark.Loader != before.mark.Loader
}

// navigatedNotReady is an expected navigation that happened while the page
// did not reach the load state or address the caller waited for before
// the deadline: the navigation is a fact, the wait's failure another.
type navigatedNotReady struct {
	loadState, urlPattern string
	err                   error
}

func (e *navigatedNotReady) Error() string {
	state := strings.ToLower(strings.TrimSpace(e.loadState))
	if state == "" {
		state = "load"
	}
	target := fmt.Sprintf("the load state %q", state)
	if e.urlPattern != "" {
		target += fmt.Sprintf(" at an address matching %q", e.urlPattern)
	}
	return fmt.Sprintf("the page navigated but did not reach %s: %v", target, e.err)
}

func (e *navigatedNotReady) Unwrap() error { return e.err }

// waitForNavigation waits until p has navigated since before, then for its
// load state and address (waitForPage), and answers whether it navigated:
// a navigation known to have happened is reported whatever the load wait
// answers, which a navigatedNotReady error carries. The Manager's own
// count of main-frame commits is read first, free and final: a commit it
// counted is a navigation whatever the engine's mark answers. The mark is
// read only while the count has not moved, and the count again after it,
// since a mark read can take the rest of the deadline. A failed mark read
// is no evidence either way: the wait reads again, and a wait that ends
// on one says so.
func (m *Manager) waitForNavigation(ctx context.Context, p *managedPage, before pageNavigation, urlPattern, loadState string) (bool, error) {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	committed := func() bool { return p.docEpoch.Load() != before.commits }
	var readErr error
	for {
		moved := committed()
		if !moved {
			current, err := readNavigation(ctx, p)
			readErr = err
			moved = committed() || err == nil && current.movedFrom(before)
		}
		if moved {
			if err := m.waitForPage(ctx, p, urlPattern, loadState); err != nil {
				return true, &navigatedNotReady{loadState: loadState, urlPattern: urlPattern, err: err}
			}
			return true, nil
		}
		select {
		case <-ctx.Done():
			if readErr != nil {
				return false, errors.Join(ctx.Err(), fmt.Errorf("read the page's navigation: %w", readErr))
			}
			return false, ctx.Err()
		case <-ticker.C:
		}
	}
}

func globMatcher(pattern string) (*regexp.Regexp, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return nil, nil
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		if pattern[i] == '*' {
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				b.WriteString(".*")
				i++
			} else {
				b.WriteString("[^/]*")
			}
		} else {
			b.WriteString(regexp.QuoteMeta(string(pattern[i])))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
