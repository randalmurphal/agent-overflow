package browser

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/browser/ids"

	"github.com/chromedp/cdproto/target"
)

// chromedp.Do on a target-less remote context issues Target.createTarget,
// which Chrome answers with a throwaway tab and WebView2 refuses with
// `-32000 no browser is open`: a WebView2 target exists only as a
// launcher-created controller, so the browser-level dial must never create
// one. It must instead enable target discovery, which is the
// only thing that feeds the engine's targetDestroyed backstop.
func TestHostedEngineBrowserDialCreatesNoTargetAndEnablesDiscovery(t *testing.T) {
	wsURL, methods := fakeCDPBrowser(t)
	engine, _ := newTestHostedEngine(t, stubRelay{url: wsURL}, engineEvents{})
	engine.attachTimeout = 5 * time.Second

	if _, err := engine.ensureBrowser(); err != nil {
		t.Fatalf("ensureBrowser against the fake endpoint: %v", err)
	}
	defer engine.Interrupt()

	sawDiscovery := false
	for _, method := range methods() {
		if method == "Target.createTarget" {
			t.Fatal("the browser-level dial created a target; WebView2 answers that with -32000 no browser is open")
		}
		if method == "Target.setDiscoverTargets" {
			sawDiscovery = true
		}
	}
	if !sawDiscovery {
		t.Fatal("the dial never enabled target discovery; the targetDestroyed backstop would hear nothing")
	}
}

// The browser connection's events reach the Manager only through the
// subscriptions the dial makes: a page's title, its renderer's crash and its
// target's destruction arrive on that connection and nowhere else.
func TestHostedEngineRoutesItsBrowserConnectionEvents(t *testing.T) {
	endpoint := startFakeCDPEndpoint(t, nil, nil)
	infos := make(chan string, 1)
	closed := make(chan string, 1)
	crashed := make(chan string, 1)
	engine, _ := newTestHostedEngine(t, stubRelay{url: endpoint.wsURL}, engineEvents{
		PageInfoChanged: func(handle ids.PageHandle, url, title string, _ uint64) {
			infos <- string(handle) + " " + url + " " + title
		},
		PageClosed:  func(handle ids.PageHandle) { closed <- string(handle) },
		PageCrashed: func(handle ids.PageHandle, reason string) { crashed <- string(handle) + " " + reason },
	})
	engine.attachTimeout = 5 * time.Second
	// No launcher reports the close, so the destroy retires the page at the
	// fallback bound.
	engine.closeFallback = 10 * time.Millisecond
	bindTestPage(t, engine, "page1", "TARGET-1")
	if _, err := engine.ensureBrowser(); err != nil {
		t.Fatalf("ensureBrowser against the fake endpoint: %v", err)
	}
	defer engine.Interrupt()

	endpoint.send(map[string]any{"method": "Target.targetInfoChanged", "params": map[string]any{"targetInfo": map[string]any{
		"targetId": "TARGET-1", "type": "page", "title": "Example", "url": "https://example.test/",
		"attached": true, "canAccessOpener": false,
	}}})
	select {
	case got := <-infos:
		if got != "page1 https://example.test/ Example" {
			t.Fatalf("PageInfoChanged saw %q", got)
		}
	case <-time.After(hostedTestDeadline):
		t.Fatal("targetInfoChanged on the browser connection never reached the Manager")
	}
	endpoint.send(map[string]any{"method": "Target.targetCrashed", "params": map[string]any{"targetId": "TARGET-1", "status": "crashed", "errorCode": 11}})
	select {
	case got := <-crashed:
		if got != "page1 crashed, code 11" {
			t.Fatalf("PageCrashed saw %q", got)
		}
	case <-time.After(hostedTestDeadline):
		t.Fatal("targetCrashed on the browser connection never reached the Manager")
	}
	endpoint.send(map[string]any{"method": "Target.targetDestroyed", "params": map[string]any{"targetId": "TARGET-1"}})
	select {
	case got := <-closed:
		if got != "page1" {
			t.Fatalf("PageClosed saw %q", got)
		}
	case <-time.After(hostedTestDeadline):
		t.Fatal("targetDestroyed on the browser connection never retired the page")
	}
}

// A browser connection that ends on its own takes the sessions of the pages
// bound to it: each is reported crashed exactly once, none closed, whether
// the connection's watcher or the attach that replaces it claims them first,
// and a page of the replacement is never reported for the loss. A
// connection Stop ends reports nothing.
func TestHostedEngineReportsALostConnectionsPagesOnce(t *testing.T) {
	for _, reconnectFirst := range []bool{false, true} {
		name := "the watcher first"
		if reconnectFirst {
			name = "a reconnect first"
		}
		t.Run(name, func(t *testing.T) {
			endpoint := startFakeCDPEndpoint(t, nil, nil)
			crashed := make(chan string, 8)
			engine, _ := newTestHostedEngine(t, stubRelay{url: endpoint.wsURL}, engineEvents{
				PageClosed:  func(handle ids.PageHandle) { t.Errorf("page %s was reported closed", handle) },
				PageCrashed: func(handle ids.PageHandle, reason string) { crashed <- string(handle) + ": " + reason },
			})
			engine.attachTimeout = 5 * time.Second
			ended := make(chan struct{}, 4)
			release := make(chan struct{})
			if reconnectFirst {
				// Holds every watcher once its connection ended.
				engine.connectionEnded = func() {
					ended <- struct{}{}
					<-release
				}
			}
			defer func() {
				if reconnectFirst {
					select {
					case <-release:
					default:
						close(release)
					}
				}
			}()
			reports := func(n int) []string {
				t.Helper()
				var got []string
				for range n {
					select {
					case report := <-crashed:
						got = append(got, report)
					case <-time.After(hostedTestDeadline):
						t.Fatalf("crash reports %v, want %d", got, n)
					}
				}
				slices.Sort(got)
				return got
			}
			none := func(when string) {
				t.Helper()
				select {
				case report := <-crashed:
					t.Fatalf("%s reported %s", when, report)
				case <-time.After(100 * time.Millisecond):
				}
			}

			connA, err := engine.ensureBrowser()
			if err != nil {
				t.Fatalf("ensureBrowser: %v", err)
			}
			for i, target := range []target.ID{"TARGET-1", "TARGET-2"} {
				if err := bindForTest(engine, ids.PageHandle(fmt.Sprintf("page%d", i+1)), target, connA); err != nil {
					t.Fatal(err)
				}
			}
			endpoint.hangUp()
			select {
			case <-connA.Done():
			case <-time.After(hostedTestDeadline):
				t.Fatal("the hang-up never ended the connection")
			}
			want := []string{"page1: " + connectionLostReason, "page2: " + connectionLostReason}
			var connB context.Context
			if reconnectFirst {
				<-ended
				// A create reconnects while the lost connection's watcher is
				// held: the attach reports the lost connection's pages.
				if connB, err = engine.ensureBrowser(); err != nil {
					t.Fatalf("reconnect: %v", err)
				}
				if err := bindForTest(engine, "page3", "TARGET-3", connB); err != nil {
					t.Fatal(err)
				}
				if got := reports(2); !slices.Equal(got, want) {
					t.Fatalf("PageCrashed saw %v, want %v", got, want)
				}
				close(release)
				none("the lost connection's watcher, after the reconnect")
			} else {
				if got := reports(2); !slices.Equal(got, want) {
					t.Fatalf("PageCrashed saw %v, want %v", got, want)
				}
				if _, ok := engine.browser(); ok {
					t.Fatal("the engine still holds the lost connection")
				}
				if connB, err = engine.ensureBrowser(); err != nil {
					t.Fatalf("reconnect: %v", err)
				}
				if err := bindForTest(engine, "page3", "TARGET-3", connB); err != nil {
					t.Fatal(err)
				}
				none("the reconnect")
			}
			if err := bindForTest(engine, "late", "TARGET-9", connA); err == nil {
				t.Fatal("a page was bound on the lost connection")
			}

			engine.Stop()
			<-connB.Done()
			none("Stop")
		})
	}
}
