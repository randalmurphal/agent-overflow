package wsllauncher

import (
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/webview2host"
)

// A page's bounds, show and hide reach the handler in the order the backend
// sent them, even when the handler is slow (it waits on the UI thread). One
// goroutine per directive let a show run before the bounds that preceded it.
func TestBrowserHostDirectivesApplyInArrivalOrderPerPage(t *testing.T) {
	var mu sync.Mutex
	var got []string
	release := make(chan struct{})
	done := make(chan struct{}, 16)
	client, _ := newTestBrowserHostClient(t, "ws://127.0.0.1/ws", func(d webview2host.Directive) {
		if d.Op == webview2host.OpCreate {
			<-release
		}
		mu.Lock()
		got = append(got, fmt.Sprintf("%s:%s:%g", d.PageID, d.Op, d.X))
		mu.Unlock()
		done <- struct{}{}
	})
	send := func(seq uint64, payload string) {
		t.Helper()
		if err := client.handleEvent(notificationEvent{Channel: string(eventchan.BrowserHost), Seq: seq, Data: json.RawMessage(payload)}); err != nil {
			t.Fatalf("handleEvent: %v", err)
		}
	}
	// page-1's create blocks; everything after it on page-1 waits for it,
	// and page-2 does not.
	send(1, `{"op":"create","pageId":"page-1","profileId":"ws-main"}`)
	send(2, `{"op":"bounds","pageId":"page-1","x":1,"w":10,"h":10}`)
	send(3, `{"op":"show","pageId":"page-1"}`)
	send(4, `{"op":"bounds","pageId":"page-2","x":5,"w":10,"h":10}`)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("page-2 waited on page-1's create")
	}
	mu.Lock()
	if !slices.Equal(got, []string{"page-2:bounds:5"}) {
		t.Fatalf("before the create settled, ran %v", got)
	}
	mu.Unlock()
	send(5, `{"op":"hide","pageId":"page-1"}`)
	close(release)
	for range 4 {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("page-1's lane stalled")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"page-2:bounds:5", "page-1:create:0", "page-1:bounds:1", "page-1:show:0", "page-1:hide:0"}
	if !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// Bounds queued behind bounds collapse to the newest; a bounds never jumps a
// show or hide queued between them.
func TestDirectiveLanesCollapseOnlyAdjacentBounds(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	var got []string
	finished := make(chan struct{}, 16)
	lanes := newDirectiveLanes(func(d webview2host.Directive) {
		if d.Op == webview2host.OpCreate {
			<-release
		}
		mu.Lock()
		got = append(got, fmt.Sprintf("%s:%g", d.Op, d.X))
		mu.Unlock()
		finished <- struct{}{}
	})
	bounds := func(x float64) webview2host.Directive {
		return webview2host.Directive{Op: webview2host.OpBounds, PageID: "p", X: x, W: 1, H: 1}
	}
	lanes.submit(webview2host.Directive{Op: webview2host.OpCreate, PageID: "p", ProfileID: "w"})
	lanes.submit(bounds(1))
	lanes.submit(bounds(2))
	lanes.submit(webview2host.Directive{Op: webview2host.OpHide, PageID: "p"})
	lanes.submit(bounds(3))
	lanes.submit(bounds(4))
	close(release)
	for range 4 {
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Fatal("lane stalled")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"create:0", "bounds:2", "hide:0", "bounds:4"}; !slices.Equal(got, want) {
		t.Fatalf("ran %v, want %v", got, want)
	}
	// An idle lane is gone, so the map holds only busy pages.
	deadline := time.Now().Add(5 * time.Second)
	for {
		lanes.mu.Lock()
		n := len(lanes.lanes)
		lanes.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d lanes left after draining", n)
		}
		time.Sleep(time.Millisecond)
	}
}

// A focus rides its page's lane: it runs after the show it follows, and the
// bounds around it never collapse across it.
func TestAFocusFollowsItsPagesShow(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	var got []string
	finished := make(chan struct{}, 16)
	lanes := newDirectiveLanes(func(d webview2host.Directive) {
		if d.Op == webview2host.OpShow {
			<-release
		}
		mu.Lock()
		got = append(got, fmt.Sprintf("%s:%g", d.Op, d.X))
		mu.Unlock()
		finished <- struct{}{}
	})
	bounds := func(x float64) webview2host.Directive {
		return webview2host.Directive{Op: webview2host.OpBounds, PageID: "p", X: x, W: 1, H: 1}
	}
	lanes.submit(bounds(1))
	lanes.submit(webview2host.Directive{Op: webview2host.OpShow, PageID: "p"})
	lanes.submit(bounds(2))
	lanes.submit(webview2host.Directive{Op: webview2host.OpFocus, PageID: "p"})
	lanes.submit(bounds(3))
	close(release)
	for range 5 {
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Fatal("lane stalled")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"bounds:1", "show:0", "bounds:2", "focus:0", "bounds:3"}; !slices.Equal(got, want) {
		t.Fatalf("ran %v, want %v", got, want)
	}
}
