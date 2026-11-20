package browser

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// navigationDriver is a page whose locator finds one clickable element, a
// click on which succeeds, and whose navigation reads answer marks in turn:
// the first answers first, every later one later (a zero mark with an
// error, or a mark).
type navigationDriver struct {
	pageDriver
	mu           sync.Mutex
	reads, acted int
	first, later navigationMark
	firstErr     error
	laterErr     error
}

func (d *navigationDriver) ResolveLocator(context.Context, Locator, string) ([]LocatorMatch, error) {
	return []LocatorMatch{{Selector: "#go", Visible: true, Enabled: true}}, nil
}

func (d *navigationDriver) ActOnNode(context.Context, LocatorMatch, Locator, nodeAction) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.acted++
	return nil
}

func (d *navigationDriver) NavigationMark(context.Context) (navigationMark, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reads++
	if d.reads == 1 {
		return d.first, d.firstErr
	}
	return d.later, d.laterErr
}

// A click expecting navigation counts only a navigation it read: a failed
// read of the page's navigation before the click refuses the click, and
// failed reads after it are no navigation, so a page that never moved fails
// the expectation with the read's error. A read that finds the page moved
// is one.
func TestExpectedNavigationCountsOnlyANavigationItRead(t *testing.T) {
	refused := errors.New("frame tree unreadable")
	here := navigationMark{URL: "https://example.test/a", Loader: "1"}
	for _, tc := range []struct {
		name       string
		driver     *navigationDriver
		wantActed  int
		navigated  bool
		wantReadIn bool
	}{
		{"the read before the click fails", &navigationDriver{firstErr: refused}, 0, false, true},
		{"every read after the click fails", &navigationDriver{first: here, laterErr: refused}, 1, false, true},
		{"a read after the click finds the page moved", &navigationDriver{first: here, later: navigationMark{URL: "https://example.test/b", Loader: "2"}}, 1, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, engine := newLifecycleManager(t)
			access := m.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
			driver := tc.driver
			engine.mu.Lock()
			engine.wrapDriver = func(inner pageDriver) pageDriver {
				driver.pageDriver = inner
				return driver
			}
			engine.mu.Unlock()
			page := mustOpen(t, m, access, "https://example.test/a")
			result, err := m.Locator(t.Context(), access, LocatorOptions{
				PageID: PageID(page.ID), Action: "click", Locator: Locator{CSS: "#go"},
				ExpectNavigation: true, TimeoutMS: 300,
			})
			driver.mu.Lock()
			acted := driver.acted
			driver.mu.Unlock()
			if acted != tc.wantActed {
				t.Fatalf("the click ran %d times, want %d", acted, tc.wantActed)
			}
			if tc.navigated {
				if err != nil || !result.Navigated {
					t.Fatalf("answered %+v, %v; want the navigation", result, err)
				}
				return
			}
			if err == nil || result.Navigated {
				t.Fatalf("answered %+v, %v; want no navigation", result, err)
			}
			if tc.wantReadIn && !errors.Is(err, refused) {
				t.Fatalf("answered %v, want the read's failure", err)
			}
		})
	}
}
