package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/chromedp"
)

func (m *Manager) checkpointScope(ctx context.Context, scope *workspaceScope) error {
	m.mu.Lock()
	pages := make([]*managedPage, 0, len(scope.pages))
	for _, p := range scope.pages {
		pages = append(pages, p)
	}
	m.mu.Unlock()
	for _, p := range pages {
		p.mu.Lock()
		opCtx, cancel := operationContext(ctx, p.ctx, 3*time.Second)
		m.captureLocalStorageInto(opCtx, p, scope)
		cancel()
		p.mu.Unlock()
	}
	opCtx, cancel := operationContext(ctx, scope.ctx, 3*time.Second)
	defer cancel()
	cookies, err := storage.GetCookies().WithBrowserContextID(scope.contextID).Do(browserCommandContext(opCtx))
	if err != nil {
		return fmt.Errorf("browser: checkpoint cookies: %w", err)
	}
	m.mu.Lock()
	scope.state.Cookies = cookieParams(cookies)
	state := cloneStorageState(scope.state)
	m.mu.Unlock()
	return m.state.save(state)
}

func (m *Manager) checkpointPage(ctx context.Context, p *managedPage) {
	m.mu.Lock()
	var scope *workspaceScope
	for _, candidate := range m.scopes {
		if candidate.pages[p.id] == p {
			scope = candidate
			break
		}
	}
	persist := m.config.PersistSiteData
	m.mu.Unlock()
	if scope == nil || !persist {
		return
	}
	m.captureLocalStorage(ctx, p)
}

func (m *Manager) captureLocalStorage(ctx context.Context, p *managedPage) {
	m.mu.Lock()
	var scope *workspaceScope
	for _, candidate := range m.scopes {
		if candidate.pages[p.id] == p {
			scope = candidate
			break
		}
	}
	m.mu.Unlock()
	if scope != nil {
		m.captureLocalStorageInto(ctx, p, scope)
	}
}

func (m *Manager) captureLocalStorageInto(ctx context.Context, p *managedPage, scope *workspaceScope) {
	var value struct {
		Origin string            `json:"origin"`
		Data   map[string]string `json:"data"`
	}
	expression := fmt.Sprintf(`(() => { try { if (!location.origin || location.origin === "null") return {origin:"",data:{}}; const data={}; let used=0; for(let i=0;i<localStorage.length;i++){const k=localStorage.key(i),v=localStorage.getItem(k); used+=(k?.length||0)+(v?.length||0); if(used>%d) break; data[k]=v} return {origin:location.origin,data}; } catch (_) { return {origin:"",data:{}}; } })()`, maxLocalStorageChars)
	if err := chromedp.Run(ctx, chromedp.Evaluate(expression, &value)); err != nil || value.Origin == "" {
		return
	}
	m.mu.Lock()
	if scope.state.LocalStorage == nil {
		scope.state.LocalStorage = make(map[string]map[string]string)
	}
	if _, exists := scope.state.LocalStorage[value.Origin]; !exists && len(scope.state.LocalStorage) >= maxLocalStorageOrigins {
		m.mu.Unlock()
		return
	}
	scope.state.LocalStorage[value.Origin] = value.Data
	m.mu.Unlock()
}

func (m *Manager) localStorageSnapshot(scope *workspaceScope) map[string]map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneLocalStorage(scope.state.LocalStorage)
}

func cloneStorageState(state storageState) storageState {
	cloned := state
	cloned.Cookies = append([]*network.CookieParam(nil), state.Cookies...)
	cloned.LocalStorage = cloneLocalStorage(state.LocalStorage)
	return cloned
}

func cloneLocalStorage(values map[string]map[string]string) map[string]map[string]string {
	cloned := make(map[string]map[string]string, len(values))
	for origin, entries := range values {
		entryCopy := make(map[string]string, len(entries))
		for key, value := range entries {
			entryCopy[key] = value
		}
		cloned[origin] = entryCopy
	}
	return cloned
}

func installStorageRestoreScript(ctx context.Context, values map[string]map[string]string) error {
	encoded, err := json.Marshal(values)
	if err != nil {
		return fmt.Errorf("browser: encode local storage restore: %w", err)
	}
	literal, _ := json.Marshal(string(encoded))
	script := fmt.Sprintf(`(() => { try { const all=JSON.parse(%s); const data=all[location.origin]; if(data) for(const [k,v] of Object.entries(data)) localStorage.setItem(k,v); } catch (_) {} })();`, literal)
	if _, err := page.AddScriptToEvaluateOnNewDocument(script).WithRunImmediately(true).Do(targetCommandContext(ctx)); err != nil {
		return fmt.Errorf("browser: install storage restore: %w", err)
	}
	return nil
}

func cookieParams(cookies []*network.Cookie) []*network.CookieParam {
	out := make([]*network.CookieParam, 0, len(cookies))
	for _, cookie := range cookies {
		param := &network.CookieParam{Name: cookie.Name, Value: cookie.Value, Domain: cookie.Domain, Path: cookie.Path, Secure: cookie.Secure, HTTPOnly: cookie.HTTPOnly, SameSite: cookie.SameSite, Priority: cookie.Priority, SourceScheme: cookie.SourceScheme, SourcePort: cookie.SourcePort, PartitionKey: cookie.PartitionKey}
		if !cookie.Session && cookie.Expires > 0 {
			expires := cdp.TimeSinceEpoch(time.Unix(int64(cookie.Expires), 0))
			param.Expires = &expires
		}
		out = append(out, param)
	}
	return out
}
