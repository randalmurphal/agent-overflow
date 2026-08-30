package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

func (m *Manager) Pointer(ctx context.Context, access Access, opts PointerOptions) (PageInfo, error) {
	p, _, err := m.lookupOrSelectPage(ctx, access, opts.PageID)
	if err != nil {
		return PageInfo{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	opCtx, cancel := operationContext(ctx, p.ctx, operationTimeout)
	defer cancel()
	action := strings.ToLower(strings.TrimSpace(opts.Action))
	if len(opts.Path) > 100 {
		return PageInfo{}, fmt.Errorf("browser: drag path exceeds 100 points")
	}
	if action != "drag" {
		if err := validatePoint(Point{X: opts.X, Y: opts.Y}); err != nil {
			return PageInfo{}, err
		}
	}
	modifiers, err := inputModifiers(opts.Modifiers)
	if err != nil {
		return PageInfo{}, err
	}
	modifierMask := input.ModifierNone
	for _, modifier := range modifiers {
		modifierMask |= modifier
	}
	button, err := inputButton(opts.Button)
	if err != nil {
		return PageInfo{}, err
	}
	targetCtx := targetCommandContext(opCtx)
	switch action {
	case "click", "double_click":
		count := int64(1)
		if action == "double_click" {
			count = 2
		}
		if err := input.DispatchMouseEvent(input.MousePressed, opts.X, opts.Y).WithButton(button).WithModifiers(modifierMask).WithClickCount(count).Do(targetCtx); err != nil {
			return PageInfo{}, err
		}
		if err := input.DispatchMouseEvent(input.MouseReleased, opts.X, opts.Y).WithButton(button).WithModifiers(modifierMask).WithClickCount(count).Do(targetCtx); err != nil {
			return PageInfo{}, err
		}
	case "move":
		if err := input.DispatchMouseEvent(input.MouseMoved, opts.X, opts.Y).WithButton(input.None).WithModifiers(modifierMask).Do(targetCtx); err != nil {
			return PageInfo{}, err
		}
	case "scroll":
		if !finite(opts.ScrollX) || !finite(opts.ScrollY) || opts.ScrollX < -100_000 || opts.ScrollX > 100_000 || opts.ScrollY < -100_000 || opts.ScrollY > 100_000 {
			return PageInfo{}, fmt.Errorf("browser: scroll delta is out of range")
		}
		if err := input.DispatchMouseEvent(input.MouseWheel, opts.X, opts.Y).WithDeltaX(opts.ScrollX).WithDeltaY(opts.ScrollY).WithModifiers(modifierMask).Do(targetCtx); err != nil {
			return PageInfo{}, err
		}
	case "drag":
		if len(opts.Path) < 2 {
			return PageInfo{}, fmt.Errorf("browser: drag path requires at least two points")
		}
		for _, point := range opts.Path {
			if err := validatePoint(point); err != nil {
				return PageInfo{}, err
			}
		}
		first := opts.Path[0]
		dragData := make(chan *input.DragData, 1)
		chromedp.ListenTarget(opCtx, func(event any) {
			if intercepted, ok := event.(*input.EventDragIntercepted); ok && intercepted.Data != nil {
				select {
				case dragData <- intercepted.Data:
				default:
				}
			}
		})
		if err := input.SetInterceptDrags(true).Do(targetCtx); err != nil {
			return PageInfo{}, err
		}
		defer func() { _ = input.SetInterceptDrags(false).Do(targetCtx) }()
		if err := input.DispatchMouseEvent(input.MouseMoved, first.X, first.Y).WithButton(input.None).WithModifiers(modifierMask).Do(targetCtx); err != nil {
			return PageInfo{}, err
		}
		if err := input.DispatchMouseEvent(input.MousePressed, first.X, first.Y).WithButton(input.Left).WithButtons(1).WithModifiers(modifierMask).WithClickCount(1).Do(targetCtx); err != nil {
			return PageInfo{}, err
		}
		for _, point := range opts.Path[1:] {
			if err := input.DispatchMouseEvent(input.MouseMoved, point.X, point.Y).WithButton(input.None).WithButtons(1).WithModifiers(modifierMask).Do(targetCtx); err != nil {
				return PageInfo{}, err
			}
		}
		last := opts.Path[len(opts.Path)-1]
		var intercepted *input.DragData
		select {
		case intercepted = <-dragData:
		case <-time.After(100 * time.Millisecond):
		case <-opCtx.Done():
			return PageInfo{}, opCtx.Err()
		}
		if intercepted == nil {
			intercepted = &input.DragData{Items: []*input.DragDataItem{}, DragOperationsMask: 1}
		}
		for i, point := range opts.Path[1:] {
			kind := input.DragOver
			if i == 0 {
				kind = input.DragEnter
			}
			if err := input.DispatchDragEvent(kind, point.X, point.Y, intercepted).WithModifiers(modifierMask).Do(targetCtx); err != nil {
				return PageInfo{}, err
			}
		}
		if err := input.DispatchDragEvent(input.Drop, last.X, last.Y, intercepted).WithModifiers(modifierMask).Do(targetCtx); err != nil {
			return PageInfo{}, err
		}
		if err := input.DispatchMouseEvent(input.MouseReleased, last.X, last.Y).WithButton(input.Left).WithButtons(0).WithModifiers(modifierMask).WithClickCount(1).Do(targetCtx); err != nil {
			return PageInfo{}, err
		}
	default:
		return PageInfo{}, fmt.Errorf("browser: pointer action must be click, double_click, move, scroll, or drag")
	}
	return m.finishPageOperation(opCtx, p)
}

func validatePoint(point Point) error {
	if !finite(point.X) || !finite(point.Y) || point.X < 0 || point.Y < 0 || point.X > maxCompanionWidth || point.Y > maxCompanionHeight {
		return fmt.Errorf("browser: pointer coordinates are outside the bounded viewport")
	}
	return nil
}

func inputButton(raw string) (input.MouseButton, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "left":
		return input.Left, nil
	case "right":
		return input.Right, nil
	case "middle":
		return input.Middle, nil
	case "back":
		return input.Back, nil
	case "forward":
		return input.Forward, nil
	default:
		return input.None, fmt.Errorf("browser: button must be left, right, middle, back, or forward")
	}
}

func inputModifiers(raw []string) ([]input.Modifier, error) {
	out := make([]input.Modifier, 0, len(raw))
	for _, value := range raw {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "alt":
			out = append(out, input.ModifierAlt)
		case "control", "ctrl":
			out = append(out, input.ModifierCtrl)
		case "meta", "command", "cmd":
			out = append(out, input.ModifierMeta)
		case "shift":
			out = append(out, input.ModifierShift)
		case "controlormeta":
			if runtime.GOOS == "darwin" {
				out = append(out, input.ModifierMeta)
			} else {
				out = append(out, input.ModifierCtrl)
			}
		default:
			return nil, fmt.Errorf("browser: unsupported modifier %q", value)
		}
	}
	return out, nil
}

func (m *Manager) DOMAction(ctx context.Context, access Access, opts DOMActionOptions) (any, error) {
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
		p.mu.Lock()
		defer p.mu.Unlock()
		opCtx, cancel := operationContext(ctx, p.ctx, operationTimeout)
		defer cancel()
		if runErr := chromedp.Run(opCtx, chromedp.KeyEvent(opts.Text)); runErr != nil {
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
	if !finite(x) || !finite(y) || x < -100_000 || x > 100_000 || y < -100_000 || y > 100_000 {
		return PageInfo{}, fmt.Errorf("browser: scroll delta is out of range")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	opCtx, cancel := operationContext(ctx, p.ctx, operationTimeout)
	defer cancel()
	root, err := locatorFrameRoot(opCtx, ref.Frames)
	if err != nil {
		return PageInfo{}, err
	}
	var nodes []*cdp.Node
	if err := chromedp.Run(opCtx, chromedp.Nodes(ref.Selector, &nodes, chromedp.ByQueryAll, chromedp.AtLeast(0), chromedp.FromNode(root))); err != nil || len(nodes) != 1 {
		return PageInfo{}, fmt.Errorf("browser: node_id is stale")
	}
	fn := fmt.Sprintf(`function(){this.scrollBy({left:%f,top:%f,behavior:"instant"})}`, x, y)
	if err := callElementFunction(opCtx, nodes[0], fn); err != nil {
		return PageInfo{}, err
	}
	return m.finishPageOperation(opCtx, p)
}

func (m *Manager) Clipboard(ctx context.Context, access Access, opts ClipboardOptions) (any, error) {
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
	p, _, err := m.lookupOrSelectPage(ctx, access, opts.PageID)
	if err != nil {
		return PageInfo{}, err
	}
	p.mu.Lock()
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
		if err := waitForPage(opCtx, p, opts.URL, opts.LoadState); err != nil {
			return PageInfo{}, err
		}
	}
	if opts.Milliseconds == 0 && opts.Locator == nil && opts.URL == "" && opts.LoadState == "" {
		return PageInfo{}, fmt.Errorf("browser: wait condition is required")
	}
	return m.finishPageOperation(opCtx, p)
}

func waitForPage(ctx context.Context, p *managedPage, urlPattern, loadState string) error {
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
		var status struct{ URL, Ready string }
		expression := `({url:location.href,ready:document.readyState})`
		if evalErr := chromedp.Run(ctx, chromedp.Evaluate(expression, &status)); evalErr == nil {
			urlOK := matcher == nil || matcher.MatchString(status.URL)
			networkIdle := false
			if state == "networkidle" {
				p.networkMu.Lock()
				networkIdle = len(p.requests) == 0 && time.Since(p.lastNetwork) >= 500*time.Millisecond
				p.networkMu.Unlock()
			}
			stateOK := state == "commit" || (state == "domcontentloaded" && status.Ready != "loading") || (state == "load" && status.Ready == "complete") || (state == "networkidle" && status.Ready == "complete" && networkIdle)
			if urlOK && stateOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("browser: wait for page: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func waitForNavigation(ctx context.Context, p *managedPage, beforeURL string, beforeLoader cdp.LoaderID, urlPattern, loadState string) error {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		var current string
		_ = chromedp.Run(ctx, chromedp.Location(&current))
		changed := current != beforeURL
		if beforeLoader != "" {
			if tree, err := page.GetFrameTree().Do(targetCommandContext(ctx)); err == nil && tree != nil && tree.Frame != nil && tree.Frame.LoaderID != beforeLoader {
				changed = true
			}
		}
		if changed {
			return waitForPage(ctx, p, urlPattern, loadState)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
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
