package browser

import (
	"testing"
)

// focusPaneHost models the one keyboard focus a windowed engine's pages
// share with the app, as the paneHost contract states it: a page takes the
// focus only by a click while shown, a hide gives the focus the page held to
// the app, and a show takes nothing. Each real engine's half is the native
// smoke's; this pins the Manager's half, that every way a pane stops showing
// a page reaches the engine as that page's hide.
type focusPaneHost struct {
	*recordingPaneHost
	shown map[string]bool
	focus string
}

const focusHeldByTheApp = "app"

func (e *focusPaneHost) ShowPage(handle string) {
	e.recordingPaneHost.ShowPage(handle)
	e.mu.Lock()
	e.shown[handle] = true
	e.mu.Unlock()
}

func (e *focusPaneHost) HidePage(handle string) {
	e.recordingPaneHost.HidePage(handle)
	e.mu.Lock()
	e.shown[handle] = false
	if e.focus == handle {
		e.focus = focusHeldByTheApp
	}
	e.mu.Unlock()
}

func (e *focusPaneHost) click(t *testing.T, handle string) {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.shown[handle] {
		t.Fatalf("click %s: the page is not shown", handle)
	}
	e.focus = handle
}

func (e *focusPaneHost) state() (focus string, shown map[string]bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	shown = make(map[string]bool, len(e.shown))
	for handle, on := range e.shown {
		shown[handle] = on
	}
	return e.focus, shown
}

// focusWorld is two threads, each with a pane showing a page: thread a has a
// second, unselected tab, thread b one page.
type focusWorld struct {
	m              *Manager
	e              *focusPaneHost
	a, b           Access
	aPage, aOther  string
	bPage          string
	aMount, bMount string
}

func (w *focusWorld) handle(access Access, pageID string) string {
	return paneHandle(w.m, access, pageID)
}

func newFocusWorld(t *testing.T) *focusWorld {
	t.Helper()
	m := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	e := &focusPaneHost{
		recordingPaneHost: &recordingPaneHost{fakeEngine: m.engine.(*fakeEngine)},
		shown:             map[string]bool{},
		focus:             focusHeldByTheApp,
	}
	m.engine = e
	w := &focusWorld{
		m: m, e: e,
		a: Access{ThreadID: "thread-a", Workspace: t.TempDir()},
		b: Access{ThreadID: "thread-b", Workspace: t.TempDir()},
	}
	other, err := m.Open(t.Context(), w.a, "https://example.test/other", OpenOptions{})
	if err != nil {
		t.Fatalf("open thread a's other page: %v", err)
	}
	w.aOther = other.ID
	page, err := m.NewCompanionPage(t.Context(), w.a)
	if err != nil {
		t.Fatalf("open thread a's page: %v", err)
	}
	w.aPage = page.ID
	bPage, err := m.Open(t.Context(), w.b, "https://example.test/b", OpenOptions{})
	if err != nil {
		t.Fatalf("open thread b's page: %v", err)
	}
	w.bPage = bPage.ID
	for _, side := range []struct {
		access Access
		page   string
		mount  *string
	}{{w.a, w.aPage, &w.aMount}, {w.b, w.bPage, &w.bMount}} {
		show := true
		if _, err := m.Visibility(t.Context(), side.access, &show, side.page); err != nil {
			t.Fatalf("show %s: %v", side.access.ThreadID, err)
		}
		*side.mount = attachSizedPane(t, m, e.recordingPaneHost, side.access)
	}
	return w
}

// remountPane mounts a pane again at the size the thread's pages already
// have, so the presentation runs inside SetPaneRect. It answers the mount.
func remountPane(t *testing.T, m *Manager, access Access) string {
	t.Helper()
	mount, err := m.AttachPane(access)
	if err != nil {
		t.Fatalf("mount the pane again: %v", err)
	}
	if err := m.SetPaneRect(mount.ID, visiblePaneRect); err != nil {
		t.Fatalf("report the pane's rect: %v", err)
	}
	return mount.ID
}

var visiblePaneRect = PaneRect{X: 10, Y: 20, Width: 800, Height: 600, ViewportWidth: 1920, ViewportHeight: 1080, Visible: true}

// Hiding a pane takes keyboard focus out of the page it showed, whatever
// hid it, and showing the page again leaves the focus with the app. A pane
// of another thread hiding leaves the focused page alone.
func TestHidingAPaneTakesKeyboardFocusOutOfItsPage(t *testing.T) {
	for _, tc := range []struct {
		name string
		hide func(t *testing.T, w *focusWorld)
		show func(t *testing.T, w *focusWorld)
		// keeps says the hide is of another thread's pane, so thread a's page
		// stays shown and keeps the focus.
		keeps bool
	}{
		{
			name: "a tab switch",
			hide: func(t *testing.T, w *focusWorld) {
				if err := w.m.ActivateCompanionPage(t.Context(), w.a, w.aOther); err != nil {
					t.Fatalf("switch tabs: %v", err)
				}
			},
			show: func(t *testing.T, w *focusWorld) {
				if err := w.m.ActivateCompanionPage(t.Context(), w.a, w.aPage); err != nil {
					t.Fatalf("switch back: %v", err)
				}
			},
		},
		{
			name: "the pane closed",
			hide: func(t *testing.T, w *focusWorld) { w.m.DetachPane(w.aMount) },
			show: func(t *testing.T, w *focusWorld) {
				w.aMount = remountPane(t, w.m, w.a)
			},
		},
		{
			name: "a compact screen change",
			hide: func(t *testing.T, w *focusWorld) {
				rect := visiblePaneRect
				rect.Visible = false
				if err := w.m.SetPaneRect(w.aMount, rect); err != nil {
					t.Fatalf("report the pane off screen: %v", err)
				}
			},
			show: func(t *testing.T, w *focusWorld) {
				if err := w.m.SetPaneRect(w.aMount, visiblePaneRect); err != nil {
					t.Fatalf("report the pane on screen: %v", err)
				}
			},
		},
		{
			name: "the page parked",
			hide: func(t *testing.T, w *focusWorld) {
				hide := false
				if _, err := w.m.Visibility(t.Context(), w.a, &hide, ""); err != nil {
					t.Fatalf("hide the companion: %v", err)
				}
			},
			show: func(t *testing.T, w *focusWorld) {
				show := true
				if _, err := w.m.Visibility(t.Context(), w.a, &show, w.aPage); err != nil {
					t.Fatalf("show the companion: %v", err)
				}
			},
		},
		{
			name:  "another thread's pane closed",
			keeps: true,
			hide:  func(t *testing.T, w *focusWorld) { w.m.DetachPane(w.bMount) },
			show: func(t *testing.T, w *focusWorld) {
				w.bMount = remountPane(t, w.m, w.b)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newFocusWorld(t)
			aHandle := w.handle(w.a, w.aPage)
			bHandle := w.handle(w.b, w.bPage)
			w.e.click(t, aHandle)

			tc.hide(t, w)
			focus, shown := w.e.state()
			if tc.keeps {
				if !shown[aHandle] || focus != aHandle {
					t.Fatalf("after the hide thread a's page is shown %v with focus %q, want it shown and focused", shown[aHandle], focus)
				}
				if shown[bHandle] {
					t.Fatal("thread b's page is still shown after its pane closed")
				}
			} else {
				if shown[aHandle] {
					t.Fatal("the page is still shown after the hide")
				}
				if focus != focusHeldByTheApp {
					t.Fatalf("after the hide the focus is %q, want the app", focus)
				}
				if !shown[bHandle] {
					t.Fatal("thread b's page stopped showing")
				}
			}

			tc.show(t, w)
			focus, shown = w.e.state()
			if !shown[aHandle] || !shown[bHandle] {
				t.Fatalf("after the show the pages are shown a=%v b=%v, want both", shown[aHandle], shown[bHandle])
			}
			want := focusHeldByTheApp
			if tc.keeps {
				want = aHandle
			}
			if focus != want {
				t.Fatalf("after the show the focus is %q, want %q", focus, want)
			}
		})
	}
}
