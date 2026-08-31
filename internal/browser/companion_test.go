package browser

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCompanionStateKeepsTabOrderAndUsesExplicitActivePage(t *testing.T) {
	first := &managedPage{id: "first", owner: "thread", createdAt: 1}
	first.info = PageInfo{ID: first.id, URL: "https://first.test", Title: "First"}
	first.lastUse.Store(time.Now().Add(-time.Minute).UnixNano())
	second := &managedPage{id: "second", owner: "thread", createdAt: 2}
	second.info = PageInfo{ID: second.id, URL: "file:///tmp/second.html", Title: "Second"}
	second.lastUse.Store(time.Now().UnixNano())
	m := &Manager{scopes: map[string]*workspaceScope{
		"/repo": {pages: map[string]*managedPage{first.id: first, second.id: second}},
	}, sessions: map[string]SessionInfo{"thread": {ActivePageID: "first"}}}

	state := m.threadState("thread")
	if len(state.Pages) != 2 || state.Pages[0].ID != "first" || state.Pages[1].ID != "second" {
		t.Fatalf("tab order = %#v", state.Pages)
	}
	if state.ActivePageID != "first" {
		t.Fatalf("active page = %q, want first", state.ActivePageID)
	}
	if state.Visible == nil || *state.Visible {
		t.Fatalf("new session visibility = %#v, want hidden", state.Visible)
	}
}

func TestBackgroundPageActivityDoesNotStealCompanionSelection(t *testing.T) {
	first := &managedPage{id: "first", owner: "thread", createdAt: 1}
	first.info = PageInfo{ID: first.id, URL: "https://first.test"}
	second := &managedPage{id: "second", owner: "thread", createdAt: 2}
	second.info = PageInfo{ID: second.id, URL: "https://second.test"}
	m := &Manager{
		scopes:   map[string]*workspaceScope{"/repo": {pages: map[string]*managedPage{first.id: first, second.id: second}}},
		sessions: map[string]SessionInfo{"thread": {ActivePageID: first.id, Visible: true}},
	}

	m.pageChanged(second)
	if got := m.threadState("thread").ActivePageID; got != first.id {
		t.Fatalf("background activity selected %q, want %q", got, first.id)
	}
}

func TestVisibilityRequiresExplicitPageWhenConcurrentAndPinsSelection(t *testing.T) {
	first := &managedPage{id: "first", owner: "thread", createdAt: 1}
	first.info = PageInfo{ID: first.id, Label: "app", URL: "https://first.test"}
	second := &managedPage{id: "second", owner: "thread", createdAt: 2}
	second.info = PageInfo{ID: second.id, Label: "docs", URL: "https://second.test"}
	m := &Manager{
		scopes:   map[string]*workspaceScope{"/repo": {pages: map[string]*managedPage{first.id: first, second.id: second}}},
		sessions: map[string]SessionInfo{"thread": {ActivePageID: first.id}},
	}
	access := Access{ThreadID: "thread", Workspace: "/repo"}
	show := true
	if _, err := m.Visibility(context.Background(), access, &show, ""); err == nil || !strings.Contains(err.Error(), "page_id is required") {
		t.Fatalf("ambiguous show error = %v", err)
	}
	info, err := m.Visibility(context.Background(), access, &show, second.id)
	if err != nil || !info.Visible || info.ActivePageID != second.id {
		t.Fatalf("explicit show = %#v, %v", info, err)
	}
	m.pageChanged(first)
	if got := m.threadState("thread").ActivePageID; got != second.id {
		t.Fatalf("background page stole pinned selection: %q", got)
	}
}

func TestImplicitPageResolutionIsConvenientOnlyWhenUnambiguous(t *testing.T) {
	first := &managedPage{id: "first", owner: "thread", createdAt: 1}
	first.info = PageInfo{ID: first.id, Label: "app"}
	second := &managedPage{id: "second", owner: "thread", createdAt: 2}
	second.info = PageInfo{ID: second.id, Label: "docs"}
	access := Access{ThreadID: "thread", Workspace: "/repo"}
	m := &Manager{scopes: map[string]*workspaceScope{"/repo": {pages: map[string]*managedPage{first.id: first}}}}
	resolved, _, err := m.lookupOrSelectPage(context.Background(), access, "")
	if err != nil || resolved != first {
		t.Fatalf("single page resolution = %#v, %v", resolved, err)
	}
	m.scopes["/repo"].pages[second.id] = second
	if _, _, err := m.lookupOrSelectPage(context.Background(), access, ""); err == nil || !strings.Contains(err.Error(), "first (app)") || !strings.Contains(err.Error(), "second (docs)") {
		t.Fatalf("ambiguous resolution error = %v", err)
	}
}

func TestPageLabelsAreThreadUniqueAndEditable(t *testing.T) {
	first := &managedPage{id: "first", owner: "thread", createdAt: 1}
	first.info = PageInfo{ID: first.id}
	second := &managedPage{id: "second", owner: "thread", createdAt: 2}
	second.info = PageInfo{ID: second.id}
	m := &Manager{scopes: map[string]*workspaceScope{"/repo": {pages: map[string]*managedPage{first.id: first, second.id: second}}}}
	access := Access{ThreadID: "thread", Workspace: "/repo"}
	info, err := m.LabelPage(context.Background(), access, first.id, " app-preview ")
	if err != nil || info.Label != "app-preview" {
		t.Fatalf("label first = %#v, %v", info, err)
	}
	if _, err := m.LabelPage(context.Background(), access, second.id, "APP-PREVIEW"); err == nil {
		t.Fatal("duplicate case-insensitive label accepted")
	}
	info, err = m.LabelPage(context.Background(), access, first.id, "")
	if err != nil || info.Label != "" {
		t.Fatalf("clear label = %#v, %v", info, err)
	}
}

func TestConcurrentThreadsKeepSelectionVisibilityAndLabelsScoped(t *testing.T) {
	first := &managedPage{id: "first", owner: "thread-a", createdAt: 1}
	first.info = PageInfo{ID: first.id}
	second := &managedPage{id: "second", owner: "thread-b", createdAt: 2}
	second.info = PageInfo{ID: second.id}
	m := &Manager{
		scopes:   map[string]*workspaceScope{"/repo": {pages: map[string]*managedPage{first.id: first, second.id: second}}},
		sessions: map[string]SessionInfo{},
	}
	show := true
	var wg sync.WaitGroup
	for _, tc := range []struct {
		access Access
		page   *managedPage
	}{
		{access: Access{ThreadID: "thread-a", Workspace: "/repo"}, page: first},
		{access: Access{ThreadID: "thread-b", Workspace: "/repo"}, page: second},
	} {
		wg.Go(func() {
			if _, err := m.LabelPage(context.Background(), tc.access, tc.page.id, "preview"); err != nil {
				t.Errorf("label %s: %v", tc.access.ThreadID, err)
				return
			}
			if _, err := m.Visibility(context.Background(), tc.access, &show, tc.page.id); err != nil {
				t.Errorf("show %s: %v", tc.access.ThreadID, err)
			}
		})
	}
	wg.Wait()
	for threadID, pageID := range map[string]string{"thread-a": first.id, "thread-b": second.id} {
		state := m.threadState(threadID)
		if state.ActivePageID != pageID || state.Visible == nil || !*state.Visible || len(state.Pages) != 1 || state.Pages[0].Label != "preview" {
			t.Fatalf("scoped state %s = %#v", threadID, state)
		}
	}
}
