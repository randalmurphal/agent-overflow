package browser

import (
	"sync"
	"testing"

	"agent-overflow/internal/keybindings"
)

// A thread's pages are presented in exactly one mount, recorded once: the
// earliest attached, kept until it detaches, whatever the other mount
// reports. The presentation's rect, the pages' size, a routed chord and a
// routed Escape all name that mount, in either attach order and after the
// presenter detaches and attaches again.
func TestOneMountPresentsAThreadsPages(t *testing.T) {
	bound := keybindings.Accelerator{Key: "k", Ctrl: true}
	first := PaneRect{X: 10, Y: 20, Width: 800, Height: 600, ViewportWidth: 1920, ViewportHeight: 1080, Visible: true}
	second := PaneRect{X: 1000, Y: 20, Width: 640, Height: 480, ViewportWidth: 1920, ViewportHeight: 1080, Visible: true}
	for _, order := range []string{"the first mount reports first", "the second mount reports first"} {
		t.Run(order, func(t *testing.T) {
			manager, engine, access, _, pageID := newPaneHostManager(t)
			handle := paneHandle(manager, access, pageID)
			show := true
			if _, err := manager.Visibility(t.Context(), access, &show, pageID); err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var placed []PanePlacement
			engine.mu.Lock()
			engine.onBounds = func(p PanePlacement) {
				mu.Lock()
				placed = append(placed, p)
				mu.Unlock()
			}
			engine.mu.Unlock()
			// The pages take a presenter's new size on their own goroutine,
			// whose pass also presents; each change of size is waited for.
			applied := make(chan string, 16)
			manager.viewportApplied = func(threadID string) { applied <- threadID }
			resized := func() {
				t.Helper()
				if got := <-applied; got != access.ThreadID {
					t.Fatalf("viewport applied for %q", got)
				}
			}
			var routed []CompanionEvent
			manager.SetEventSink(func(event CompanionEvent) {
				if event.Kind == "accelerator" || event.Kind == "escape" {
					mu.Lock()
					routed = append(routed, event)
					mu.Unlock()
				}
			})
			attach := func() string {
				t.Helper()
				mount, err := manager.AttachPane(access)
				if err != nil {
					t.Fatal(err)
				}
				return mount.ID
			}
			report := func(mount string, rect PaneRect) {
				t.Helper()
				if err := manager.SetPaneRect(mount, rect); err != nil {
					t.Fatal(err)
				}
			}
			// expect checks that the newest presentation is at rect, that
			// the pages took its size, and that a chord and an Escape are
			// routed to mount.
			expect := func(state, mount string, rect PaneRect) {
				t.Helper()
				mu.Lock()
				placed, routed = nil, nil
				mu.Unlock()
				manager.syncPanePresentation(access.ThreadID)
				manager.emitAccelerator(handle, bound)
				manager.emitEscape(handle)
				mu.Lock()
				defer mu.Unlock()
				if len(placed) == 0 || placed[len(placed)-1].Rect.X != rect.X {
					t.Fatalf("%s: presented at %+v, want at x=%v", state, placed, rect.X)
				}
				manager.mu.Lock()
				session := manager.sessions[access.ThreadID]
				manager.mu.Unlock()
				if session.PaneW != int(rect.Width) || session.PaneH != int(rect.Height) {
					t.Fatalf("%s: the pages follow a %dx%d pane, want %vx%v", state, session.PaneW, session.PaneH, rect.Width, rect.Height)
				}
				if len(routed) != 2 {
					t.Fatalf("%s: routed %d events, want a chord and an Escape", state, len(routed))
				}
				for _, event := range routed {
					if event.Mount != mount {
						t.Fatalf("%s: a %s event names mount %q, want %q", state, event.Kind, event.Mount, mount)
					}
				}
			}

			a, b := attach(), attach()
			if order == "the first mount reports first" {
				report(a, first)
				report(b, second)
			} else {
				report(b, second)
				report(a, first)
			}
			resized()
			expect("both mounted", a, first)

			// The presenter off screen keeps the role: nothing is shown,
			// and the other mount takes nothing.
			hidden := first
			hidden.Visible = false
			report(a, hidden)
			report(b, second)
			mu.Lock()
			placed, routed = nil, nil
			mu.Unlock()
			manager.syncPanePresentation(access.ThreadID)
			manager.emitAccelerator(handle, bound)
			mu.Lock()
			if len(placed) != 0 {
				mu.Unlock()
				t.Fatalf("the presenter off screen: presented at %+v", placed)
			}
			for _, event := range routed {
				if event.Kind == "accelerator" {
					mu.Unlock()
					t.Fatalf("the presenter off screen: routed a chord to %q", event.Mount)
				}
			}
			mu.Unlock()
			report(a, first)
			expect("the presenter back on screen", a, first)

			manager.DetachPane(a)
			resized()
			expect("the presenter detached", b, second)
			a = attach()
			report(a, first)
			expect("the first client attached again", b, second)
		})
	}
}
