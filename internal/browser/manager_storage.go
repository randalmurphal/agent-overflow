package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
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
	cookies, err := scope.profile.Cookies(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	scope.state.Cookies = cookies
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

// captureLocalStorageInto folds one page's origin into the workspace
// checkpoint. The origin cap is policy; reading the origin is the driver's.
func (m *Manager) captureLocalStorageInto(ctx context.Context, p *managedPage, scope *workspaceScope) {
	origin, data, err := p.driver.LocalStorage(ctx)
	if err != nil || origin == "" {
		return
	}
	m.mu.Lock()
	if scope.state.LocalStorage == nil {
		scope.state.LocalStorage = make(map[string]map[string]string)
	}
	if _, exists := scope.state.LocalStorage[origin]; !exists && len(scope.state.LocalStorage) >= maxLocalStorageOrigins {
		m.mu.Unlock()
		return
	}
	scope.state.LocalStorage[origin] = data
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

// localStorageExpression reads the current origin's localStorage, bounded to
// the characters the checkpoint accepts. An opaque origin has no storage to
// checkpoint and reads as empty.
func localStorageExpression() string {
	return fmt.Sprintf(`(() => { try { if (!location.origin || location.origin === "null") return {origin:"",data:{}}; const data={}; let used=0; for(let i=0;i<localStorage.length;i++){const k=localStorage.key(i),v=localStorage.getItem(k); used+=(k?.length||0)+(v?.length||0); if(used>%d) break; data[k]=v} return {origin:location.origin,data}; } catch (_) { return {origin:"",data:{}}; } })()`, maxLocalStorageChars)
}

// storageRestoreScript renders the document-start script that seeds a page's
// origin from the checkpoint. Engines install it their own way.
func storageRestoreScript(values map[string]map[string]string) (string, error) {
	encoded, err := json.Marshal(values)
	if err != nil {
		return "", fmt.Errorf("browser: encode local storage restore: %w", err)
	}
	literal, _ := json.Marshal(string(encoded))
	return fmt.Sprintf(`(() => { try { const all=JSON.parse(%s); const data=all[location.origin]; if(data) for(const [k,v] of Object.entries(data)) localStorage.setItem(k,v); } catch (_) {} })();`, literal), nil
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
