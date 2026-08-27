package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"time"

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
	p.touch()
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
	if opts.FullPage {
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
	p.touch()
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
	p.touch()
	m.captureLocalStorage(opCtx, p)
	return pageInfo(opCtx, p.id)
}

func (m *Manager) Type(ctx context.Context, access Access, opts TypeOptions) (PageInfo, error) {
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
		selectAll := kb.Control + "a"
		if runtime.GOOS == "darwin" {
			selectAll = kb.Meta + "a"
		}
		actions = append(actions, chromedp.KeyEvent(selectAll), chromedp.KeyEvent(kb.Backspace))
	}
	actions = append(actions, chromedp.SendKeys(opts.Selector, opts.Text, chromedp.ByQuery))
	if err := chromedp.Run(opCtx, actions...); err != nil {
		return PageInfo{}, fmt.Errorf("browser: type into %q: %w", opts.Selector, err)
	}
	p.touch()
	m.captureLocalStorage(opCtx, p)
	return pageInfo(opCtx, p.id)
}

func (m *Manager) Press(ctx context.Context, access Access, pageID, key string) (PageInfo, error) {
	key = browserKey(key)
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
	if err := chromedp.Run(opCtx, chromedp.KeyEvent(key)); err != nil {
		return PageInfo{}, fmt.Errorf("browser: press key: %w", err)
	}
	p.touch()
	m.captureLocalStorage(opCtx, p)
	return pageInfo(opCtx, p.id)
}

func (m *Manager) Scroll(ctx context.Context, access Access, pageID, selector string, x, y float64) (PageInfo, error) {
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
	p.touch()
	return pageInfo(opCtx, p.id)
}

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
	p.touch()
	return pageInfo(opCtx, p.id)
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
	var command chromedp.Action
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "back":
		command = chromedp.NavigateBack()
	case "forward":
		command = chromedp.NavigateForward()
	case "reload":
		command = chromedp.Reload()
	case "stop":
		command = chromedp.Stop()
	default:
		return PageInfo{}, fmt.Errorf("browser: history action must be back, forward, reload, or stop")
	}
	if err := chromedp.Run(opCtx, command); err != nil {
		return PageInfo{}, fmt.Errorf("browser: history %s: %w", action, err)
	}
	p.touch()
	return pageInfo(opCtx, p.id)
}

func (m *Manager) Evaluate(ctx context.Context, access Access, pageID, expression string) (any, error) {
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
	p.touch()
	m.captureLocalStorage(opCtx, p)
	return result, nil
}

func (m *Manager) lookupOrSelectPage(ctx context.Context, access Access, pageID string) (*managedPage, *workspaceScope, error) {
	if pageID == "" {
		p, err := m.pageFor(ctx, access, "", false)
		if err != nil {
			return nil, nil, err
		}
		_, scope, err := m.lookupOwnedPage(access, p.id)
		return p, scope, err
	}
	return m.lookupOwnedPage(access, pageID)
}

func browserKey(raw string) string {
	parts := strings.Split(strings.TrimSpace(raw), "+")
	if len(parts) == 0 {
		return ""
	}
	out := ""
	for _, part := range parts {
		switch strings.ToLower(strings.TrimSpace(part)) {
		case "control", "ctrl":
			out += kb.Control
		case "shift":
			out += kb.Shift
		case "alt", "option":
			out += kb.Alt
		case "meta", "command", "cmd":
			out += kb.Meta
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
	return out
}

func snapshotExpression() string {
	return fmt.Sprintf(`(() => {
 const clean=s=>(s||"").replace(/\s+/g," ").trim();
 const esc=s=>CSS.escape(s);
 const path=el=>{if(el.id)return "#"+esc(el.id);const parts=[];while(el&&el.nodeType===1&&parts.length<6){let p=el.tagName.toLowerCase();if(el.getAttribute("data-testid")){p+='[data-testid="'+CSS.escape(el.getAttribute("data-testid"))+'"]';parts.unshift(p);break}const parent=el.parentElement;if(parent){const peers=[...parent.children].filter(x=>x.tagName===el.tagName);if(peers.length>1)p+=":nth-of-type("+(peers.indexOf(el)+1)+")"}parts.unshift(p);el=parent}return parts.join(">");};
 const candidates=[...document.querySelectorAll('a,button,input,textarea,select,summary,[role],[contenteditable="true"],[tabindex]')].filter(el=>{const r=el.getBoundingClientRect();const s=getComputedStyle(el);return r.width>0&&r.height>0&&s.visibility!=="hidden"&&s.display!=="none"}).slice(0,%d);
 return {url:location.href,title:document.title,text:clean(document.body?.innerText).slice(0,%d),elements:candidates.map(el=>({selector:path(el),tag:el.tagName.toLowerCase(),role:el.getAttribute("role")||"",text:clean(el.innerText||el.value).slice(0,500),label:el.getAttribute("aria-label")||el.labels?.[0]?.innerText||"",type:el.getAttribute("type")||"",href:el.href||"",placeholder:el.getAttribute("placeholder")||"",disabled:!!el.disabled}))};
})()`, maxSnapshotElements, maxSnapshotText)
}
