package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
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
	snapshot, err := p.driver.Snapshot(opCtx)
	if err != nil {
		return Snapshot{}, err
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
	if opts.FullPage && opts.Clip != nil {
		return nil, fmt.Errorf("browser: screenshot clip and full_page are mutually exclusive")
	}
	if clip := opts.Clip; clip != nil {
		if !finite(clip.X) || !finite(clip.Y) || !finite(clip.Width) || !finite(clip.Height) || clip.X < 0 || clip.Y < 0 || clip.Width <= 0 || clip.Height <= 0 || clip.Width > maxFullScreenshotWidth || clip.Height > maxFullScreenshotHeight {
			return nil, fmt.Errorf("browser: screenshot clip is outside the bounded capture area")
		}
	}
	data, err := p.driver.Screenshot(opCtx, opts)
	if err != nil {
		return nil, err
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
	if err := p.driver.Click(opCtx, selector); err != nil {
		return PageInfo{}, fmt.Errorf("browser: click %q: %w", selector, err)
	}
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
	if err := p.driver.Type(opCtx, opts.Selector, opts.Text, opts.Clear); err != nil {
		return PageInfo{}, fmt.Errorf("browser: type into %q: %w", opts.Selector, err)
	}
	return m.finishPageOperation(opCtx, p)
}

func (m *Manager) Press(ctx context.Context, access Access, pageID, key string) (PageInfo, error) {
	if len(key) > maxBrowserInputBytes {
		return PageInfo{}, fmt.Errorf("browser: key input exceeds %d bytes", maxBrowserInputBytes)
	}
	if !chordHasKey(key) {
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
	// The per-tab clipboard is AO-managed state, so a paste chord is answered
	// from it rather than from any OS or engine clipboard.
	if isModifierChord(key, "v") {
		if text := p.clipboardText(); text != "" {
			if err := p.driver.TypeText(opCtx, text); err != nil {
				return PageInfo{}, fmt.Errorf("browser: paste clipboard: %w", err)
			}
			return m.finishPageOperation(opCtx, p)
		}
	}
	if err := p.driver.Press(opCtx, key); err != nil {
		return PageInfo{}, err
	}
	if isModifierChord(key, "c") {
		if selected := p.driver.SelectionText(opCtx); selected != "" {
			p.setClipboardText(selected)
		}
	}
	return m.finishPageOperation(opCtx, p)
}

// chordHasKey reports whether a key chord names anything besides modifiers. A
// modifier-only chord is rejected before it can cost a page, which is why this
// rule is the Manager's rather than an engine's.
func chordHasKey(raw string) bool {
	for _, part := range strings.Split(strings.TrimSpace(raw), "+") {
		switch strings.ToLower(strings.TrimSpace(part)) {
		case "control", "ctrl", "shift", "alt", "option", "meta", "command", "cmd", "controlormeta":
		default:
			if part != "" {
				return true
			}
		}
	}
	return false
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
	if err := validateScrollDelta(x, y); err != nil {
		return PageInfo{}, err
	}
	p, _, err := m.lookupOrSelectPage(ctx, access, pageID)
	if err != nil {
		return PageInfo{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	opCtx, cancel := operationContext(ctx, p.ctx, operationTimeout)
	defer cancel()
	if err := p.driver.Scroll(opCtx, selector, x, y); err != nil {
		return PageInfo{}, fmt.Errorf("browser: scroll: %w", err)
	}
	return m.finishPageOperation(opCtx, p)
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
		if err := p.driver.WaitVisible(opCtx, selector); err != nil {
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
	normalized := strings.ToLower(strings.TrimSpace(action))
	switch normalized {
	case "back", "forward", "reload", "stop":
	default:
		return PageInfo{}, fmt.Errorf("browser: history action must be back, forward, reload, or stop")
	}
	if err := p.driver.History(opCtx, normalized); err != nil {
		return PageInfo{}, err
	}
	if normalized != "stop" {
		if err := m.waitForPage(opCtx, p, "", "load"); err != nil {
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
	result, err := p.driver.Evaluate(opCtx, expression)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("browser: encode evaluation result: %w", err)
	}
	if len(encoded) > maxEvaluateBytes {
		return nil, fmt.Errorf("browser: evaluation result exceeds %d bytes", maxEvaluateBytes)
	}
	m.refreshPageAfterOperation(opCtx, p)
	return result, nil
}

// EvaluateReadOnly returns the bounded result and the engine's own caveat
// about what "read only" could be enforced as. The caveat is a driver
// capability answer, not a Manager judgement: an engine with engine-level
// side-effect rejection returns none, and one that can only be best-effort
// says so in the tool result rather than looking identical.
func (m *Manager) EvaluateReadOnly(ctx context.Context, access Access, pageID, expression string) (any, string, error) {
	if len(expression) > maxBrowserInputBytes {
		return nil, "", fmt.Errorf("browser: expression exceeds %d bytes", maxBrowserInputBytes)
	}
	if strings.TrimSpace(expression) == "" {
		return nil, "", fmt.Errorf("browser: expression is required")
	}
	p, _, err := m.lookupOrSelectPage(ctx, access, pageID)
	if err != nil {
		return nil, "", err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	opCtx, cancel := operationContext(ctx, p.ctx, operationTimeout)
	defer cancel()
	caveat := p.driver.ReadOnlyCaveat()
	raw, err := p.driver.EvaluateReadOnly(opCtx, unwrapReadOnlyPromise(expression))
	if err != nil {
		return nil, caveat, err
	}
	if len(raw) == 0 {
		return nil, caveat, nil
	}
	if len(raw) > maxEvaluateBytes {
		return nil, caveat, fmt.Errorf("browser: evaluation result exceeds %d bytes", maxEvaluateBytes)
	}
	var result any
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, caveat, fmt.Errorf("browser: decode evaluation result: %w", err)
	}
	m.refreshPageAfterOperation(opCtx, p)
	return result, caveat, nil
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
	info, err := m.pageInfo(ctx, p)
	if err == nil {
		p.setInfo(info)
		info = p.cachedInfo()
	}
	m.pageChanged(p)
	return info, err
}

func (m *Manager) refreshPageAfterOperation(ctx context.Context, p *managedPage) {
	info, err := m.pageInfo(ctx, p)
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

func snapshotExpression() string {
	return fmt.Sprintf(`(() => {
 const clean=s=>(s||"").replace(/\s+/g," ").trim();
 const esc=s=>CSS.escape(s);
 const path=el=>{if(el.id&&el.id.length<=256)return "#"+esc(el.id);const parts=[];while(el&&el.nodeType===1&&parts.length<6){let p=el.tagName.toLowerCase();const tid=el.getAttribute("data-testid");if(tid&&tid.length<=256){p+='[data-testid="'+CSS.escape(tid)+'"]';parts.unshift(p);break}const parent=el.parentElement;if(parent){const peers=[...parent.children].filter(x=>x.tagName===el.tagName);if(peers.length>1)p+=":nth-of-type("+(peers.indexOf(el)+1)+")"}parts.unshift(p);el=parent}return parts.join(">");};
 const candidates=[...document.querySelectorAll('a,button,input,textarea,select,summary,[role],[contenteditable="true"],[tabindex]')].filter(el=>{const r=el.getBoundingClientRect();const s=getComputedStyle(el);return r.width>0&&r.height>0&&s.visibility!=="hidden"&&s.display!=="none"}).slice(0,%d);
 return {url:location.href.slice(0,%d),title:document.title.slice(0,%d),text:clean(document.body?.innerText).slice(0,%d),elements:candidates.map(el=>{const selector=path(el);return {selector,tag:el.tagName.toLowerCase(),role:(el.getAttribute("role")||"").slice(0,100),text:clean(el.innerText||el.value).slice(0,500),label:(el.getAttribute("aria-label")||el.labels?.[0]?.innerText||"").slice(0,1000),type:(el.getAttribute("type")||"").slice(0,100),href:(el.href||"").slice(0,4096),placeholder:(el.getAttribute("placeholder")||"").slice(0,1000),disabled:!!el.disabled}})};
})()`, maxSnapshotElements, maxBrowserURLBytes, maxBrowserTitleBytes, maxSnapshotText)
}
