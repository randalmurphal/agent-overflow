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
}
