package browser

import (
	"errors"
	"testing"

	"agent-overflow/internal/browser/turnorder"
)

// A pane mount of a lifetime the thread's close retired, still registered
// because its pane has not detached yet, reaches nothing of the thread's
// next lifetime: a find it starts on the fresh lifetime's shown tab is
// refused, its focus intents find no mount, and its numbered commands pass.
func TestARetiredMountReachesNothingOfAFreshLifetime(t *testing.T) {
	f := newFindFixture(t, addr("old"))
	m, retired := f.manager, f.mount
	if err := m.CloseThread(t.Context(), f.access.ThreadID()); err != nil {
		t.Fatal(err)
	}
	fresh := m.Bind(f.access.Caller())
	tab, err := m.NewCompanionPage(t.Context(), fresh, addr("fresh"))
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	_, registered := m.panes[retired]
	m.mu.Unlock()
	if !registered {
		t.Fatal("the retired mount is not registered; the scenario needs it")
	}
	state, err := m.Find(t.Context(), fresh, PageID(tab.ID), FindStart, "x", retired, 1)
	if !errors.Is(err, errFindNoMount) {
		t.Fatalf("a find through the retired mount answered %+v, %v; want errFindNoMount", state, err)
	}
	if m.mountOf(fresh.tb, retired) {
		t.Fatal("the retired mount is the fresh lifetime's")
	}
	p := livePage(t, m, fresh, tab.ID)
	p.findMu.Lock()
	active := p.find.active
	p.findMu.Unlock()
	if active {
		t.Fatal("the retired mount started a search on the fresh tab")
	}
	m.mu.Lock()
	focus := m.mountFocusLocked(fresh.tb, retired)
	m.mu.Unlock()
	if focus != nil {
		t.Fatal("the retired mount holds a focus intent state for the fresh lifetime")
	}
	turn, err := m.awaitTurn(t.Context(), fresh, place(string(retired), 1))
	if err != nil {
		t.Fatal(err)
	}
	defer turn.Release()
	if turn.Commit() != turnorder.Passed {
		t.Fatal("a numbered command of the retired mount was admitted for the fresh lifetime")
	}

	// A stop made through the retired lifetime's Access ends no search of
	// the fresh one, whichever mount it names.
	mount, err := m.AttachPane(fresh.Caller())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Find(t.Context(), fresh, PageID(tab.ID), FindStart, "x", MountID(mount.ID), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Find(t.Context(), f.access, PageID(tab.ID), FindStop, "", MountID(mount.ID), 1); err != nil {
		t.Fatal(err)
	}
	p.findMu.Lock()
	active = p.find.active
	p.findMu.Unlock()
	if !active {
		t.Fatal("a stop through the retired Access ended the fresh lifetime's search")
	}
}

// wantSuspended fails unless pageID is still a suspended tab, no live page.
func wantSuspended(t *testing.T, m *Manager, pageID string) {
	t.Helper()
	m.mu.Lock()
	_, suspended := m.suspended[PageID(pageID)]
	live, _ := m.livePageLocked(PageID(pageID))
	m.mu.Unlock()
	if !suspended || live != nil {
		t.Fatalf("a refused find start restored the tab (suspended %v, live %v)", suspended, live != nil)
	}
}

// A find start the mount admission refuses restores nothing: one through a
// retired lifetime's mount on the fresh lifetime's suspended shown tab, and
// one from the thread's own mount on a suspended tab that is not shown or
// with a sequence not above the mount's last. An admitted start restores
// the tab it searches.
func TestARefusedFindStartRestoresNothing(t *testing.T) {
	f := newFindFixture(t, addr("old"))
	m, retired := f.manager, f.mount
	if err := m.CloseThread(t.Context(), f.access.ThreadID()); err != nil {
		t.Fatal(err)
	}
	fresh := m.Bind(f.access.Caller())
	tab, err := m.NewCompanionPage(t.Context(), fresh, addr("fresh"))
	if err != nil {
		t.Fatal(err)
	}
	suspendTab(t, m, fresh, tab.ID)
	if _, err := m.Find(t.Context(), fresh, PageID(tab.ID), FindStart, "x", retired, 1); !errors.Is(err, errFindNoMount) {
		t.Fatalf("a find through the retired mount answered %v; want errFindNoMount", err)
	}
	wantSuspended(t, m, tab.ID)

	g := newFindFixture(t, addr("shown"))
	g.find(t, FindStart, "x")
	other, err := g.manager.NewPage(t.Context(), g.access)
	if err != nil {
		t.Fatal(err)
	}
	suspendTab(t, g.manager, g.access, other.ID)
	if _, err := g.manager.Find(t.Context(), g.access, PageID(other.ID), FindStart, "x", g.mount, g.nextSeq()); !errors.Is(err, errFindNotShown) {
		t.Fatalf("a find on a tab that is not shown answered %v; want errFindNotShown", err)
	}
	wantSuspended(t, g.manager, other.ID)
	setActiveTab(g.manager, g.access, other.ID)
	if _, err := g.manager.Find(t.Context(), g.access, PageID(other.ID), FindStart, "x", g.mount, 1); !errors.Is(err, errFindStale) {
		t.Fatalf("a find with a stale sequence answered %v; want errFindStale", err)
	}
	wantSuspended(t, g.manager, other.ID)
	if _, err := g.manager.Find(t.Context(), g.access, PageID(other.ID), FindStart, "x", g.mount, g.nextSeq()); err != nil {
		t.Fatalf("an admitted find on the shown suspended tab: %v", err)
	}
	livePage(t, g.manager, g.access, other.ID)
}
