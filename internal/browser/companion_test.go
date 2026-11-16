package browser

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMoveCompanionPageReordersAndNewPagesAppend(t *testing.T) {
	pages := make(map[string]*managedPage, 3)
	for i, id := range []string{"first", "second", "third"} {
		p := &managedPage{id: id, owner: "thread", createdAt: int64(i + 1)}
		p.viewport.Store(packViewport(defaultViewportWidth, defaultViewportHeight))
		p.tabOrder.Store(p.createdAt)
		p.info = PageInfo{ID: id}
		pages[id] = p
	}
	m := &Manager{
		scopes:   map[string]*jarScope{"/repo": {pages: pages}},
		sessions: map[string]SessionInfo{"thread": {}},
	}
	access := Access{ThreadID: "thread", Workspace: "/repo"}

	order := func() []string {
		state := m.threadState("thread")
		ids := make([]string, 0, len(state.Pages))
		for _, page := range state.Pages {
			ids = append(ids, page.ID)
		}
		return ids
	}

	if err := m.MoveCompanionPage(access, "third", 0); err != nil {
		t.Fatalf("move = %v", err)
	}
	if got := order(); got[0] != "third" || got[1] != "first" || got[2] != "second" {
		t.Fatalf("order after front move = %v", got)
	}
	// An out-of-range index clamps to the end instead of failing.
	if err := m.MoveCompanionPage(access, "third", 99); err != nil {
		t.Fatalf("clamped move = %v", err)
	}
	if got := order(); got[2] != "third" {
		t.Fatalf("order after clamped move = %v", got)
	}
	// A page opened after a renumbering move still lands at the end.
	late := &managedPage{id: "late", owner: "thread", createdAt: time.Now().UnixNano()}
	late.viewport.Store(packViewport(defaultViewportWidth, defaultViewportHeight))
	late.tabOrder.Store(late.createdAt)
	late.info = PageInfo{ID: "late"}
	pages["late"] = late
	if got := order(); got[3] != "late" {
		t.Fatalf("order with late page = %v", got)
	}
	if err := m.MoveCompanionPage(access, "missing", 0); err == nil {
		t.Fatal("moving an unknown page did not fail")
	}
}

// A client orders snapshots by Seq, because they are published after the
// lock and calls answer with them: every snapshot, of any thread, takes the
// next number, including ones read concurrently.
func TestCompanionStateSnapshotsAreNumberedInReadOrder(t *testing.T) {
	m := &Manager{
		scopes:   map[string]*jarScope{"/repo": {pages: map[string]*managedPage{}}},
		sessions: map[string]SessionInfo{"a": {}, "b": {}},
	}
	first := m.threadState("a")
	second := m.threadState("b")
	third := m.threadState("a")
	if first.Seq == 0 || first.Seq >= second.Seq || second.Seq >= third.Seq {
		t.Fatalf("Seq order %d, %d, %d", first.Seq, second.Seq, third.Seq)
	}
	var wg sync.WaitGroup
	seen := make(chan uint64, 64)
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			seen <- m.threadState("a").Seq
		}()
	}
	wg.Wait()
	close(seen)
	unique := make(map[uint64]bool, 64)
	for seq := range seen {
		if seq <= third.Seq || unique[seq] {
			t.Fatalf("concurrent snapshot Seq %d repeats or precedes %d", seq, third.Seq)
		}
		unique[seq] = true
	}
}

func TestCompanionStateKeepsTabOrderAndUsesExplicitActivePage(t *testing.T) {
	first := &managedPage{id: "first", owner: "thread", createdAt: 1}
	first.viewport.Store(packViewport(defaultViewportWidth, defaultViewportHeight))
	first.info = PageInfo{ID: first.id, URL: "https://first.test", Title: "First"}
	first.lastUse.Store(time.Now().Add(-time.Minute).UnixNano())
	second := &managedPage{id: "second", owner: "thread", createdAt: 2}
	second.viewport.Store(packViewport(defaultViewportWidth, defaultViewportHeight))
	second.info = PageInfo{ID: second.id, URL: "file:///tmp/second.html", Title: "Second"}
	second.lastUse.Store(time.Now().UnixNano())
	m := &Manager{scopes: map[string]*jarScope{
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
	first.viewport.Store(packViewport(defaultViewportWidth, defaultViewportHeight))
	first.info = PageInfo{ID: first.id, URL: "https://first.test"}
	second := &managedPage{id: "second", owner: "thread", createdAt: 2}
	second.viewport.Store(packViewport(defaultViewportWidth, defaultViewportHeight))
	second.info = PageInfo{ID: second.id, URL: "https://second.test"}
	m := &Manager{
		scopes:   map[string]*jarScope{"/repo": {pages: map[string]*managedPage{first.id: first, second.id: second}}},
		sessions: map[string]SessionInfo{"thread": {ActivePageID: first.id, Visible: true}},
	}

	m.pageChanged(second)
	if got := m.threadState("thread").ActivePageID; got != first.id {
		t.Fatalf("background activity selected %q, want %q", got, first.id)
	}
}

func TestVisibilityRequiresExplicitPageWhenConcurrentAndPinsSelection(t *testing.T) {
	first := &managedPage{id: "first", owner: "thread", createdAt: 1}
	first.viewport.Store(packViewport(defaultViewportWidth, defaultViewportHeight))
	first.info = PageInfo{ID: first.id, Label: "app", URL: "https://first.test"}
	second := &managedPage{id: "second", owner: "thread", createdAt: 2}
	second.viewport.Store(packViewport(defaultViewportWidth, defaultViewportHeight))
	second.info = PageInfo{ID: second.id, Label: "docs", URL: "https://second.test"}
	m := &Manager{
		scopes:   map[string]*jarScope{"/repo": {pages: map[string]*managedPage{first.id: first, second.id: second}}},
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
	first.viewport.Store(packViewport(defaultViewportWidth, defaultViewportHeight))
	first.info = PageInfo{ID: first.id, Label: "app"}
	second := &managedPage{id: "second", owner: "thread", createdAt: 2}
	second.viewport.Store(packViewport(defaultViewportWidth, defaultViewportHeight))
	second.info = PageInfo{ID: second.id, Label: "docs"}
	access := Access{ThreadID: "thread", Workspace: "/repo"}
	m := &Manager{scopes: map[string]*jarScope{"/repo": {pages: map[string]*managedPage{first.id: first}}}}
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
	first.viewport.Store(packViewport(defaultViewportWidth, defaultViewportHeight))
	first.info = PageInfo{ID: first.id}
	second := &managedPage{id: "second", owner: "thread", createdAt: 2}
	second.viewport.Store(packViewport(defaultViewportWidth, defaultViewportHeight))
	second.info = PageInfo{ID: second.id}
	m := &Manager{scopes: map[string]*jarScope{"/repo": {pages: map[string]*managedPage{first.id: first, second.id: second}}}}
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
	first.viewport.Store(packViewport(defaultViewportWidth, defaultViewportHeight))
	first.info = PageInfo{ID: first.id}
	second := &managedPage{id: "second", owner: "thread-b", createdAt: 2}
	second.viewport.Store(packViewport(defaultViewportWidth, defaultViewportHeight))
	second.info = PageInfo{ID: second.id}
	m := &Manager{
		scopes:   map[string]*jarScope{"/repo": {pages: map[string]*managedPage{first.id: first, second.id: second}}},
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

// Each step moves one tab from the Manager's own selection, wrapping, so
// steps sent back to back with no state read between them all count.
func TestStepCompanionPageMovesOneTabPerStepAndWraps(t *testing.T) {
	m, access, first, _ := newNavigateFixture(t, false)
	second, err := m.NewCompanionPage(t.Context(), access, "")
	if err != nil {
		t.Fatal(err)
	}
	third, err := m.NewCompanionPage(t.Context(), access, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.ActivateCompanionPage(t.Context(), access, first); err != nil {
		t.Fatal(err)
	}
	active := func() string { return m.CompanionState(access).ActivePageID }

	for _, want := range []string{second.ID, third.ID, first} {
		if err := m.StepCompanionPage(t.Context(), access, 1); err != nil {
			t.Fatalf("step = %v", err)
		}
		if got := active(); got != want {
			t.Fatalf("after a forward step the active tab is %s, want %s", got, want)
		}
	}
	if err := m.StepCompanionPage(t.Context(), access, -1); err != nil || active() != third.ID {
		t.Fatalf("a backward step from the first tab = %v, active %s, want %s", err, active(), third.ID)
	}

	// Two steps at once both apply.
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.StepCompanionPage(context.Background(), access, 1); err != nil {
				t.Errorf("step = %v", err)
			}
		}()
	}
	wg.Wait()
	if got := active(); got != second.ID {
		t.Fatalf("after two concurrent steps from the third tab the active tab is %s, want %s", got, second.ID)
	}

	if err := m.StepCompanionPage(t.Context(), access, 0); err == nil {
		t.Fatal("a step of 0 was accepted")
	}
	if err := m.StepCompanionPage(t.Context(), Access{ThreadID: "other", Workspace: access.Workspace}, 1); err == nil {
		t.Fatal("a thread with no tabs stepped")
	}
}
