package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"runtime"
	"strings"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

func (m *Manager) Snapshot(ctx context.Context, access Access, pageID string) (Snapshot, error) {
	p, _, err := m.lookupOrSelectPage(ctx, access, pageID)
	if err != nil {
		return Snapshot{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	opCtx, cancel := operationContext(ctx, p.ctx, operationTimeout)
	defer cancel()
	var snapshot Snapshot
	if err := chromedp.Run(opCtx, chromedp.Evaluate(snapshotExpression(), &snapshot)); err != nil {
		return Snapshot{}, fmt.Errorf("browser: snapshot: %w", err)
	}
	snapshot.ID = p.id
	for i := range snapshot.Elements {
		snapshot.Elements[i].NodeID = p.rememberNode(nodeReference{Selector: snapshot.Elements[i].Selector, Tag: snapshot.Elements[i].Tag, Text: snapshot.Elements[i].Text})
	}
	p.setInfo(snapshot.PageInfo)
	m.pageChanged(p)
	return snapshot, nil
}

func (m *Manager) Screenshot(ctx context.Context, access Access, opts ScreenshotOptions) ([]byte, error) {
	p, _, err := m.lookupOrSelectPage(ctx, access, opts.PageID)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	opCtx, cancel := operationContext(ctx, p.ctx, operationTimeout)
	defer cancel()
	params := page.CaptureScreenshot().WithFormat(page.CaptureScreenshotFormatJpeg).WithQuality(85).WithFromSurface(true)
	if opts.FullPage && opts.Clip != nil {
		return nil, fmt.Errorf("browser: screenshot clip and full_page are mutually exclusive")
	}
	if opts.Clip != nil {
		clip := opts.Clip
		if !finite(clip.X) || !finite(clip.Y) || !finite(clip.Width) || !finite(clip.Height) || clip.X < 0 || clip.Y < 0 || clip.Width <= 0 || clip.Height <= 0 || clip.Width > maxFullScreenshotWidth || clip.Height > maxFullScreenshotHeight {
			return nil, fmt.Errorf("browser: screenshot clip is outside the bounded capture area")
		}
		params = params.WithCaptureBeyondViewport(true).WithClip(&page.Viewport{X: clip.X, Y: clip.Y, Width: clip.Width, Height: clip.Height, Scale: 1})
	} else if opts.FullPage {
		_, _, contentSize, _, _, cssContentSize, metricsErr := page.GetLayoutMetrics().Do(targetCommandContext(opCtx))
		if metricsErr != nil {
			return nil, fmt.Errorf("browser: screenshot metrics: %w", metricsErr)
		}
		size := cssContentSize
		if size == nil {
			size = contentSize
		}
		if size != nil {
			height := size.Height
			width := size.Width
			if height > maxFullScreenshotHeight {
				height = maxFullScreenshotHeight
			}
			if width > maxFullScreenshotWidth {
				width = maxFullScreenshotWidth
			}
			params = params.WithCaptureBeyondViewport(true).WithClip(&page.Viewport{X: 0, Y: 0, Width: width, Height: height, Scale: 1})
		}
	}
	data, err := params.Do(targetCommandContext(opCtx))
	if err != nil {
		return nil, fmt.Errorf("browser: screenshot: %w", err)
	}
	if len(data) > maxScreenshotBytes {
		return nil, fmt.Errorf("browser: screenshot exceeds %d bytes; use a viewport capture or reduce the page size", maxScreenshotBytes)
	}
	m.refreshPageAfterOperation(opCtx, p)
	return data, nil
}

func (m *Manager) Click(ctx context.Context, access Access, pageID, selector string) (PageInfo, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return PageInfo{}, fmt.Errorf("browser: selector is required")
	}
	p, _, err := m.lookupOrSelectPage(ctx, access, pageID)
	if err != nil {
		return PageInfo{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	opCtx, cancel := operationContext(ctx, p.ctx, operationTimeout)
	defer cancel()
	if err := chromedp.Run(opCtx, chromedp.ScrollIntoView(selector, chromedp.ByQuery), chromedp.Click(selector, chromedp.ByQuery)); err != nil {
		return PageInfo{}, fmt.Errorf("browser: click %q: %w", selector, err)
	}
	m.captureLocalStorage(opCtx, p)
	return m.finishPageOperation(opCtx, p)
}

func (m *Manager) Type(ctx context.Context, access Access, opts TypeOptions) (PageInfo, error) {
	if len(opts.Text) > maxBrowserInputBytes {
		return PageInfo{}, fmt.Errorf("browser: input text exceeds %d bytes", maxBrowserInputBytes)
	}
	opts.Selector = strings.TrimSpace(opts.Selector)
	if opts.Selector == "" {
		return PageInfo{}, fmt.Errorf("browser: selector is required")
	}
	p, _, err := m.lookupOrSelectPage(ctx, access, opts.PageID)
	if err != nil {
		return PageInfo{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	opCtx, cancel := operationContext(ctx, p.ctx, operationTimeout)
	defer cancel()
	actions := []chromedp.Action{chromedp.Focus(opts.Selector, chromedp.ByQuery)}
	if opts.Clear {
		actions = append(actions, chromedp.KeyEvent("a", browserKeyOptions("ControlOrMeta+a", controlOrMetaModifier())...), chromedp.KeyEvent(kb.Backspace))
	}
	actions = append(actions, chromedp.SendKeys(opts.Selector, opts.Text, chromedp.ByQuery))
	if err := chromedp.Run(opCtx, actions...); err != nil {
		return PageInfo{}, fmt.Errorf("browser: type into %q: %w", opts.Selector, err)
	}
	m.captureLocalStorage(opCtx, p)
	return m.finishPageOperation(opCtx, p)
}

func (m *Manager) Press(ctx context.Context, access Access, pageID, key string) (PageInfo, error) {
	if len(key) > maxBrowserInputBytes {
		return PageInfo{}, fmt.Errorf("browser: key input exceeds %d bytes", maxBrowserInputBytes)
	}
	rawKey := key
	key, modifiers := browserKey(key)
	if key == "" {
		return PageInfo{}, fmt.Errorf("browser: key is required")
	}
	p, _, err := m.lookupOrSelectPage(ctx, access, pageID)
	if err != nil {
		return PageInfo{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	opCtx, cancel := operationContext(ctx, p.ctx, operationTimeout)
	defer cancel()
	if isModifierChord(rawKey, "v") {
		if text := p.clipboardText(); text != "" {
			if err := chromedp.Run(opCtx, chromedp.KeyEvent(text)); err != nil {
				return PageInfo{}, fmt.Errorf("browser: paste clipboard: %w", err)
			}
			return m.finishPageOperation(opCtx, p)
		}
	}
	keyOptions := browserKeyOptions(rawKey, modifiers)
	if err := chromedp.Run(opCtx, chromedp.KeyEvent(key, keyOptions...)); err != nil {
		return PageInfo{}, fmt.Errorf("browser: press key: %w", err)
	}
	if isModifierChord(rawKey, "c") {
		var selected string
		_ = chromedp.Run(opCtx, chromedp.Evaluate(`(()=>{const a=document.activeElement;if(a&&(a instanceof HTMLInputElement||a instanceof HTMLTextAreaElement)&&a.selectionStart!==null)return a.value.slice(a.selectionStart,a.selectionEnd);return String(getSelection()||"")})()`, &selected))
		if selected != "" {
			p.setClipboardText(selected)
		}
	}
	m.captureLocalStorage(opCtx, p)
	return m.finishPageOperation(opCtx, p)
}

func isModifierChord(raw, key string) bool {
	parts := strings.Split(strings.ToLower(strings.ReplaceAll(strings.TrimSpace(raw), " ", "")), "+")
	hasModifier, hasKey := false, false
	for _, part := range parts {
		if part == "control" || part == "ctrl" || part == "meta" || part == "command" || part == "cmd" || part == "controlormeta" {
			hasModifier = true
		}
		if part == key {
			hasKey = true
		}
	}
	return hasModifier && hasKey
}

func browserEditingCommand(command string) chromedp.KeyOption {
	return func(event *input.DispatchKeyEventParams) *input.DispatchKeyEventParams {
		if event.Type == input.KeyDown || event.Type == input.KeyRawDown {
			event.Commands = []string{command}
		}
		return event
	}
}

func browserKeyOptions(raw string, modifiers input.Modifier) []chromedp.KeyOption {
	options := []chromedp.KeyOption{chromedp.KeyModifiers(modifiers)}
	if isModifierChord(raw, "a") {
		options = append(options, browserEditingCommand("selectAll"))
	}
	return options
}

func (p *managedPage) clipboardText() string {
	p.clipboardMu.Lock()
	defer p.clipboardMu.Unlock()
	for _, item := range p.clipboard {
		for _, entry := range item.Entries {
			if entry.MIMEType == "text/plain" {
				return entry.Text
			}
		}
	}
	return ""
}
func (p *managedPage) setClipboardText(text string) {
	if len(text) > maxClipboardBytes {
		text = text[:maxClipboardBytes]
	}
	p.clipboardMu.Lock()
	p.clipboard = []ClipboardItem{{Entries: []ClipboardEntry{{MIMEType: "text/plain", Text: text}}}}
	p.clipboardMu.Unlock()
}

func (m *Manager) Scroll(ctx context.Context, access Access, pageID, selector string, x, y float64) (PageInfo, error) {
	if !finite(x) || !finite(y) || x < -100_000 || x > 100_000 || y < -100_000 || y > 100_000 {
		return PageInfo{}, fmt.Errorf("browser: scroll delta is out of range")
	}
	p, _, err := m.lookupOrSelectPage(ctx, access, pageID)
	if err != nil {
		return PageInfo{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	opCtx, cancel := operationContext(ctx, p.ctx, operationTimeout)
	defer cancel()
	selectorJSON, _ := json.Marshal(selector)
	expression := fmt.Sprintf(`(() => { const s=%s; const el=s?document.querySelector(s):window; if(!el) throw new Error("selector not found"); el.scrollBy({left:%f,top:%f,behavior:"instant"}); return true; })()`, selectorJSON, x, y)
	var ok bool
	if err := chromedp.Run(opCtx, chromedp.Evaluate(expression, &ok)); err != nil {
		return PageInfo{}, fmt.Errorf("browser: scroll: %w", err)
	}
	return m.finishPageOperation(opCtx, p)
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func (m *Manager) Wait(ctx context.Context, access Access, pageID, selector string, milliseconds int) (PageInfo, error) {
	p, _, err := m.lookupOrSelectPage(ctx, access, pageID)
	if err != nil {
		return PageInfo{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if milliseconds < 0 || milliseconds > 30_000 {
		return PageInfo{}, fmt.Errorf("browser: wait duration must be between 0 and 30000 ms")
	}
	timeout := operationTimeout
	if milliseconds > 0 {
		timeout = time.Duration(milliseconds+1000) * time.Millisecond
	}
	opCtx, cancel := operationContext(ctx, p.ctx, timeout)
	defer cancel()
	if strings.TrimSpace(selector) != "" {
		if err := chromedp.Run(opCtx, chromedp.WaitVisible(selector, chromedp.ByQuery)); err != nil {
			return PageInfo{}, fmt.Errorf("browser: wait for %q: %w", selector, err)
		}
	} else if milliseconds > 0 {
		timer := time.NewTimer(time.Duration(milliseconds) * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-opCtx.Done():
			return PageInfo{}, opCtx.Err()
		}
	} else {
		return PageInfo{}, fmt.Errorf("browser: selector or duration is required")
	}
	return m.finishPageOperation(opCtx, p)
}

func (m *Manager) History(ctx context.Context, access Access, pageID, action string) (PageInfo, error) {
	p, _, err := m.lookupOrSelectPage(ctx, access, pageID)
	if err != nil {
		return PageInfo{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	opCtx, cancel := operationContext(ctx, p.ctx, operationTimeout)
	defer cancel()
	var runErr error
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "back":
		current, entries, err := page.GetNavigationHistory().Do(targetCommandContext(opCtx))
		if err != nil {
			return PageInfo{}, fmt.Errorf("browser: history back: %w", err)
		}
		if current <= 0 {
			return PageInfo{}, fmt.Errorf("browser: no previous history entry")
		}
		runErr = page.NavigateToHistoryEntry(entries[current-1].ID).Do(targetCommandContext(opCtx))
	case "forward":
		current, entries, err := page.GetNavigationHistory().Do(targetCommandContext(opCtx))
		if err != nil {
			return PageInfo{}, fmt.Errorf("browser: history forward: %w", err)
		}
		if int(current)+1 >= len(entries) {
			return PageInfo{}, fmt.Errorf("browser: no forward history entry")
		}
		runErr = page.NavigateToHistoryEntry(entries[current+1].ID).Do(targetCommandContext(opCtx))
	case "reload":
		runErr = page.Reload().Do(targetCommandContext(opCtx))
	case "stop":
		runErr = page.StopLoading().Do(targetCommandContext(opCtx))
	default:
		return PageInfo{}, fmt.Errorf("browser: history action must be back, forward, reload, or stop")
	}
	if runErr != nil {
		return PageInfo{}, fmt.Errorf("browser: history %s: %w", action, runErr)
	}
	if strings.ToLower(strings.TrimSpace(action)) != "stop" {
		if err := waitForPage(opCtx, p, "", "load"); err != nil {
			return PageInfo{}, fmt.Errorf("browser: history %s: %w", action, err)
		}
	}
	return m.finishPageOperation(opCtx, p)
}

func (m *Manager) Evaluate(ctx context.Context, access Access, pageID, expression string) (any, error) {
	if len(expression) > maxBrowserInputBytes {
		return nil, fmt.Errorf("browser: expression exceeds %d bytes", maxBrowserInputBytes)
	}
	if strings.TrimSpace(expression) == "" {
		return nil, fmt.Errorf("browser: expression is required")
	}
	p, _, err := m.lookupOrSelectPage(ctx, access, pageID)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	opCtx, cancel := operationContext(ctx, p.ctx, operationTimeout)
	defer cancel()
	var result any
	awaitPromise := func(params *cdpruntime.EvaluateParams) *cdpruntime.EvaluateParams {
		return params.WithAwaitPromise(true)
	}
	if err := chromedp.Run(opCtx, chromedp.Evaluate(expression, &result, awaitPromise)); err != nil {
		return nil, fmt.Errorf("browser: evaluate: %w", err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("browser: encode evaluation result: %w", err)
	}
	if len(encoded) > maxEvaluateBytes {
		return nil, fmt.Errorf("browser: evaluation result exceeds %d bytes", maxEvaluateBytes)
	}
	m.captureLocalStorage(opCtx, p)
	m.refreshPageAfterOperation(opCtx, p)
	return result, nil
}

func (m *Manager) EvaluateReadOnly(ctx context.Context, access Access, pageID, expression string) (any, error) {
	if len(expression) > maxBrowserInputBytes {
		return nil, fmt.Errorf("browser: expression exceeds %d bytes", maxBrowserInputBytes)
	}
	if strings.TrimSpace(expression) == "" {
		return nil, fmt.Errorf("browser: expression is required")
	}
	p, _, err := m.lookupOrSelectPage(ctx, access, pageID)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	opCtx, cancel := operationContext(ctx, p.ctx, operationTimeout)
	defer cancel()
	expression = unwrapReadOnlyPromise(expression)
	remote, exception, err := cdpruntime.Evaluate(expression).WithReturnByValue(true).WithAwaitPromise(true).WithThrowOnSideEffect(true).Do(targetCommandContext(opCtx))
	if err != nil {
		return nil, fmt.Errorf("browser: read-only evaluate: %w", err)
	}
	if exception != nil {
		return nil, fmt.Errorf("browser: read-only evaluate rejected a possible side effect: %s", exception.Text)
	}
	if remote == nil || len(remote.Value) == 0 {
		return nil, nil
	}
	if len(remote.Value) > maxEvaluateBytes {
		return nil, fmt.Errorf("browser: evaluation result exceeds %d bytes", maxEvaluateBytes)
	}
	var result any
	if err := json.Unmarshal(remote.Value, &result); err != nil {
		return nil, fmt.Errorf("browser: decode evaluation result: %w", err)
	}
	m.refreshPageAfterOperation(opCtx, p)
	return result, nil
}

func unwrapReadOnlyPromise(expression string) string {
	trimmed := strings.TrimSpace(expression)
	const prefix = "Promise.resolve("
	if !strings.HasPrefix(trimmed, prefix) || !strings.HasSuffix(trimmed, ")") {
		return expression
	}
	inner := trimmed[len(prefix) : len(trimmed)-1]
	depth := 0
	var quote rune
	escaped := false
	for _, r := range inner {
		if quote != 0 {
			if escaped {
				escaped = false
				continue
			}
			if r == '\\' {
				escaped = true
				continue
			}
			if r == quote {
				quote = 0
			}
			continue
		}
		if r == '\'' || r == '"' || r == '`' {
			quote = r
			continue
		}
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return expression
			}
		}
	}
	if depth != 0 || quote != 0 {
		return expression
	}
	return inner
}

func (m *Manager) finishPageOperation(ctx context.Context, p *managedPage) (PageInfo, error) {
	info, err := pageInfo(ctx, p.id)
	if err == nil {
		p.setInfo(info)
		info = p.cachedInfo()
	}
	m.pageChanged(p)
	return info, err
}

func (m *Manager) refreshPageAfterOperation(ctx context.Context, p *managedPage) {
	info, err := pageInfo(ctx, p.id)
	if err == nil {
		p.setInfo(info)
	}
	m.pageChanged(p)
}

func (m *Manager) lookupOrSelectPage(ctx context.Context, access Access, pageID string) (*managedPage, *workspaceScope, error) {
	if pageID = strings.TrimSpace(pageID); pageID != "" {
		return m.lookupOwnedPage(access, pageID)
	}
	owned := m.ownedPages(access.ThreadID)
	switch len(owned) {
	case 0:
		p, err := m.createPage(ctx, access)
		if err != nil {
			return nil, nil, err
		}
		_, scope, err := m.lookupOwnedPage(access, p.id)
		return p, scope, err
	case 1:
		p := owned[0]
		_, scope, err := m.lookupOwnedPage(access, p.id)
		return p, scope, err
	default:
		return nil, nil, ambiguousPageError(owned)
	}
}

func browserKey(raw string) (string, input.Modifier) {
	parts := strings.Split(strings.TrimSpace(raw), "+")
	if len(parts) == 0 {
		return "", 0
	}
	out := ""
	var modifiers input.Modifier
	for _, part := range parts {
		switch strings.ToLower(strings.TrimSpace(part)) {
		case "control", "ctrl":
			modifiers |= input.ModifierCtrl
		case "shift":
			modifiers |= input.ModifierShift
		case "alt", "option":
			modifiers |= input.ModifierAlt
		case "meta", "command", "cmd":
			modifiers |= input.ModifierMeta
		case "controlormeta":
			modifiers |= controlOrMetaModifier()
		case "enter", "return":
			out += kb.Enter
		case "tab":
			out += kb.Tab
		case "escape", "esc":
			out += kb.Escape
		case "backspace":
			out += kb.Backspace
		case "delete":
			out += kb.Delete
		case "arrowup", "up":
			out += kb.ArrowUp
		case "arrowdown", "down":
			out += kb.ArrowDown
		case "arrowleft", "left":
			out += kb.ArrowLeft
		case "arrowright", "right":
			out += kb.ArrowRight
		case "space":
			out += " "
		default:
			out += part
		}
	}
	return out, modifiers
}

func controlOrMetaModifier() input.Modifier {
	if runtime.GOOS == "darwin" {
		return input.ModifierMeta
	}
	return input.ModifierCtrl
}

func snapshotExpression() string {
	return fmt.Sprintf(`(() => {
 const clean=s=>(s||"").replace(/\s+/g," ").trim();
 const esc=s=>CSS.escape(s);
 const path=el=>{if(el.id&&el.id.length<=256)return "#"+esc(el.id);const parts=[];while(el&&el.nodeType===1&&parts.length<6){let p=el.tagName.toLowerCase();const tid=el.getAttribute("data-testid");if(tid&&tid.length<=256){p+='[data-testid="'+CSS.escape(tid)+'"]';parts.unshift(p);break}const parent=el.parentElement;if(parent){const peers=[...parent.children].filter(x=>x.tagName===el.tagName);if(peers.length>1)p+=":nth-of-type("+(peers.indexOf(el)+1)+")"}parts.unshift(p);el=parent}return parts.join(">");};
 const candidates=[...document.querySelectorAll('a,button,input,textarea,select,summary,[role],[contenteditable="true"],[tabindex]')].filter(el=>{const r=el.getBoundingClientRect();const s=getComputedStyle(el);return r.width>0&&r.height>0&&s.visibility!=="hidden"&&s.display!=="none"}).slice(0,%d);
 return {url:location.href.slice(0,%d),title:document.title.slice(0,%d),text:clean(document.body?.innerText).slice(0,%d),elements:candidates.map(el=>{const selector=path(el);return {selector,tag:el.tagName.toLowerCase(),role:(el.getAttribute("role")||"").slice(0,100),text:clean(el.innerText||el.value).slice(0,500),label:(el.getAttribute("aria-label")||el.labels?.[0]?.innerText||"").slice(0,1000),type:(el.getAttribute("type")||"").slice(0,100),href:(el.href||"").slice(0,4096),placeholder:(el.getAttribute("placeholder")||"").slice(0,1000),disabled:!!el.disabled}})};
})()`, maxSnapshotElements, maxBrowserURLBytes, maxBrowserTitleBytes, maxSnapshotText)
}
