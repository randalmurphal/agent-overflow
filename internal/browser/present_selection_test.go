package browser

import (
	"strings"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/browser/ids"
)

// holdPresentation makes the first presentation pass that is about to decide
// on pageID, laid out at the thread's viewport, wait: held is closed when it
// waits, and closing release lets it decide.
func holdPresentation(m *Manager, pageID string) (held, release chan struct{}) {
	held, release = make(chan struct{}), make(chan struct{})
	var once atomic.Bool
	m.presentDeciding = func(_ ThreadID, candidate *managedPage) {
		if candidate == nil || candidate.id != PageID(pageID) || !m.pageViewportCurrent(candidate) {
			return
		}
		if once.CompareAndSwap(false, true) {
			close(held)
			<-release
		}
	}
	return held, release
}

// shows answers the handles the engine was told to show, in order.
func shows(calls []string) []string {
	var out []string
	for _, call := range calls {
		if handle, ok := strings.CutPrefix(call, "show:"); ok {
			out = append(out, handle)
		}
	}
	return out
}

// A switch to a tab that lags the pane's size and straight back: the pass
// that laid the left tab out decides after the switch back, so the engine is
// never told to show the left tab, and the tab the user is on is shown.
func TestASwitchAwayAndBackNeverShowsTheLeftTab(t *testing.T) {
	manager, engine, access, firstID, secondID := newPaneHostManager(t)
	show := true
	if _, err := manager.Visibility(t.Context(), access, &show, PageID(secondID)); err != nil {
		t.Fatalf("visibility: %v", err)
	}
	attachSizedPane(t, manager, engine, access)
	firstHandle := paneHandle(manager, access, firstID)
	secondHandle := paneHandle(manager, access, secondID)
	if p, _, _ := manager.lookupOwnedPage(access, PageID(firstID)); manager.pageViewportCurrent(p) {
		t.Fatal("the unselected tab is already at the pane's size; nothing lays it out")
	}
	held, release := holdPresentation(manager, firstID)

	away := make(chan error, 1)
	go func() { away <- manager.ActivateCompanionPage(t.Context(), access, PageID(firstID)) }()
	<-held
	if err := manager.ActivateCompanionPage(t.Context(), access, PageID(secondID)); err != nil {
		t.Fatalf("switch back: %v", err)
	}
	close(release)
	if err := <-away; err != nil {
		t.Fatalf("switch away: %v", err)
	}
	awaitViewportIdle(t, manager, string(access.ThreadID()))

	calls := engine.take()
	for _, handle := range shows(calls) {
		if handle == firstHandle {
			t.Fatalf("the engine was told to show the tab the user left: %v", calls)
		}
	}
	manager.mu.Lock()
	presented := manager.presented[access.ThreadID()]
	manager.mu.Unlock()
	if presented != PageID(secondID) {
		t.Fatalf("presented %q, want the selected tab %q (calls %v)", presented, secondID, calls)
	}
	if got := shows(calls); len(got) == 0 || got[len(got)-1] != secondHandle {
		t.Fatalf("the last show is not the selected tab: %v", calls)
	}
}

// A restore that finishes after the user selected another tab never shows
// the restored page over the newer selection.
func TestARestoreFinishingAfterANewerSelectionIsNotShown(t *testing.T) {
	manager, engine, access, firstID, secondID := newPaneHostManager(t)
	show := true
	if _, err := manager.Visibility(t.Context(), access, &show, PageID(firstID)); err != nil {
		t.Fatalf("visibility: %v", err)
	}
	attachSizedPane(t, manager, engine, access)
	firstHandle := paneHandle(manager, access, firstID)
	second, _, err := manager.lookupOwnedPage(access, PageID(secondID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.suspendPage(t.Context(), second); err != nil {
		t.Fatalf("suspend the second tab: %v", err)
	}
	engine.take()
	held, release := holdPresentation(manager, secondID)

	restored := make(chan error, 1)
	go func() { restored <- manager.ActivateCompanionPage(t.Context(), access, PageID(secondID)) }()
	<-held
	if err := manager.ActivateCompanionPage(t.Context(), access, PageID(firstID)); err != nil {
		t.Fatalf("select the first tab: %v", err)
	}
	close(release)
	if err := <-restored; err != nil {
		t.Fatalf("restore: %v", err)
	}
	awaitViewportIdle(t, manager, string(access.ThreadID()))

	calls := engine.take()
	for _, handle := range shows(calls) {
		if handle != firstHandle {
			t.Fatalf("the engine was told to show %s, not the selected tab %s: %v", handle, firstHandle, calls)
		}
	}
	if _, _, err := manager.lookupOwnedPage(access, PageID(secondID)); err != nil {
		t.Fatalf("the second tab did not restore: %v", err)
	}
	manager.mu.Lock()
	presented := manager.presented[access.ThreadID()]
	manager.mu.Unlock()
	if presented != PageID(firstID) {
		t.Fatalf("presented %q, want the selected tab %q (calls %v)", presented, firstID, calls)
	}
}

// A page whose process ends while a pass is about to show it stays the
// selected tab, showing its failure, and the engine is never told to show
// the dead view.
func TestAPageThatCrashedBeforeItsShowIsNotShown(t *testing.T) {
	manager, engine, access, firstID, secondID := newPaneHostManager(t)
	show := true
	if _, err := manager.Visibility(t.Context(), access, &show, PageID(secondID)); err != nil {
		t.Fatalf("visibility: %v", err)
	}
	attachSizedPane(t, manager, engine, access)
	if _, err := manager.WaitAdvanced(t.Context(), access, WaitOptions{PageID: PageID(firstID), Milliseconds: 1}); err != nil {
		t.Fatalf("lay the first tab out: %v", err)
	}
	awaitViewportIdle(t, manager, string(access.ThreadID()))
	firstHandle := paneHandle(manager, access, firstID)
	engine.take()
	held, release := holdPresentation(manager, firstID)

	switched := make(chan error, 1)
	go func() { switched <- manager.ActivateCompanionPage(t.Context(), access, PageID(firstID)) }()
	<-held
	manager.pageCrashed(ids.PageHandle(firstHandle), "test crash")
	close(release)
	if err := <-switched; err != nil {
		t.Fatalf("switch: %v", err)
	}
	awaitViewportIdle(t, manager, string(access.ThreadID()))
	calls := engine.take()
	for _, handle := range shows(calls) {
		if handle == firstHandle {
			t.Fatalf("the engine was told to show the crashed page: %v", calls)
		}
	}
	if tab, _ := findTab(manager, access, firstID); tab.Error == "" {
		t.Fatalf("the crashed tab reads %+v, want its failure", tab)
	}
}

// A pass that is switching tabs has hidden the tab the user left before it
// records what it presents. A focus intent for the left tab armed then is
// not applied to it: the intent's presented check agrees with the pass's.
func TestAFocusIntentNeverReachesATabASwitchLeft(t *testing.T) {
	manager, engine, access, firstID, secondID := newPaneHostManager(t)
	show := true
	if _, err := manager.Visibility(t.Context(), access, &show, PageID(secondID)); err != nil {
		t.Fatalf("visibility: %v", err)
	}
	mount := attachSizedPane(t, manager, engine, access)
	// An operation lays the other tab out, so the switch shows it at once.
	if _, err := manager.WaitAdvanced(t.Context(), access, WaitOptions{PageID: PageID(firstID), Milliseconds: 1}); err != nil {
		t.Fatalf("lay the first tab out: %v", err)
	}
	awaitViewportIdle(t, manager, string(access.ThreadID()))
	secondHandle := paneHandle(manager, access, secondID)
	engine.take()
	held, release := holdPresentation(manager, firstID)

	switched := make(chan error, 1)
	go func() { switched <- manager.ActivateCompanionPage(t.Context(), access, PageID(firstID)) }()
	<-held
	if err := manager.FocusCompanionPage(access, MountID(mount), PageID(secondID), 1); err != nil {
		t.Fatalf("focus: %v", err)
	}
	close(release)
	if err := <-switched; err != nil {
		t.Fatalf("switch: %v", err)
	}
	awaitViewportIdle(t, manager, string(access.ThreadID()))
	calls := engine.take()
	for _, call := range calls {
		if call == "focus:"+secondHandle {
			t.Fatalf("the focus intent reached the tab the switch left: %v", calls)
		}
	}
}
