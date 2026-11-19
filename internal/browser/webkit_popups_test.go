//go:build (linux && cgo && !gtk3 && !android && !server && !nogui) || (darwin && cgo && !ios && !server && !nogui)

package browser

import (
	"slices"
	"sync"
	"testing"
	"time"
)

// A WebKit popup's crash and close before its listing reach the Manager at
// the listing, the close alone when it closed, and the close also at once; what ended its
// view before its attach is held the same way. Once listed, the page's go
// straight on, as an ordinary page's always do. A held popup's end is kept
// only while the engine holds or attaches it.
func TestWebKitPopupEndsWaitForTheListing(t *testing.T) {
	var mu sync.Mutex
	var got []string
	events := engineEvents{
		PageClosed: func(handle string) {
			mu.Lock()
			got = append(got, "closed "+handle)
			mu.Unlock()
		},
		PageCrashed: func(handle, reason string) {
			mu.Lock()
			got = append(got, "crashed "+handle+" "+reason)
			mu.Unlock()
		},
	}
	expect := func(want ...string) {
		t.Helper()
		deadline := time.Now().Add(hostedTestDeadline)
		for {
			mu.Lock()
			now := slices.Clone(got)
			mu.Unlock()
			if slices.Equal(now, want) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("the Manager heard %q, want %q", now, want)
			}
			time.Sleep(time.Millisecond)
		}
	}

	popup := &pageEnds{popup: true}
	popup.hold(pageEnd{crashed: "early"})
	popup.report(pageEnd{closed: true}, events, "page-2")
	expect("closed page-2")
	popup.adopted(events, "page-2")
	expect("closed page-2", "closed page-2")
	popup.report(pageEnd{crashed: "late"}, events, "page-2")
	expect("closed page-2", "closed page-2", "crashed page-2 late")

	mu.Lock()
	got = nil
	mu.Unlock()
	ordinary := &pageEnds{}
	ordinary.report(pageEnd{crashed: "gone"}, events, "page-3")
	expect("crashed page-3 gone")

	var held heldPopupEnds
	if held.recordLocked("page-4", false, pageEnd{closed: true}) {
		t.Fatal("the end of a popup neither held nor attached was kept")
	}
	if !held.recordLocked("page-5", true, pageEnd{crashed: "held"}) {
		t.Fatal("a held popup's end was not kept")
	}
	held.attachLocked("page-5")
	if !held.recordLocked("page-5", false, pageEnd{closed: true}) {
		t.Fatal("an attaching popup's end was not kept")
	}
	if end := held.takeLocked("page-5"); end != (pageEnd{crashed: "held", closed: true}) {
		t.Fatalf("the attach took %+v", end)
	}
	if held.recordLocked("page-5", false, pageEnd{closed: true}) || len(held.ends) != 0 {
		t.Fatal("an end was kept for a popup whose attach ended")
	}
}
