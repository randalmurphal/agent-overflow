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
	// commit, when set, commits a new document as the click runs, and
	// commitOnRead as the first read after the click starts.
	commit, commitOnRead bool
	// laterBlocks, when set, holds every read after the click until its
	// context ends.
	laterBlocks bool
	hooks       pageHooks
}

func (d *navigationDriver) ResolveLocator(context.Context, Locator, string) ([]LocatorMatch, error) {
	return []LocatorMatch{{Selector: "#go", Visible: true, Enabled: true}}, nil
}

func (d *navigationDriver) ActOnNode(context.Context, LocatorMatch, Locator, nodeAction) error {
	d.mu.Lock()
	d.acted++
	commit, hooks := d.commit, d.hooks
	d.mu.Unlock()
	if commit {
		// As an engine reports a main-frame commit.
		if finish := hooks.DocumentCommitted(); finish != nil {
			finish()
		}
	}
	return nil
}

func (d *navigationDriver) NavigationMark(ctx context.Context) (navigationMark, error) {
	d.mu.Lock()
	d.reads++
	reads, hooks := d.reads, d.hooks
	commit := reads == 2 && d.commitOnRead
	d.mu.Unlock()
	if reads == 1 {
		return d.first, d.firstErr
	}
	if commit {
		if finish := hooks.DocumentCommitted(); finish != nil {
			finish()
		}
	}
	if d.laterBlocks {
		<-ctx.Done()
		return navigationMark{}, ctx.Err()
	}
	return d.later, d.laterErr
}

// PageStatus answers the page's load state while ctx lasts, as an engine's
// read does.
func (d *navigationDriver) PageStatus(ctx context.Context) (pageStatus, error) {
	if err := ctx.Err(); err != nil {
		return pageStatus{}, err
	}
	return d.pageDriver.PageStatus(ctx)
}

// readsAfterClick is how many navigation reads were made after the click.
func (d *navigationDriver) readsAfterClick() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return max(d.reads-1, 0)
}

// A click expecting navigation counts only a navigation it read: a failed
// read of the page's navigation before the click refuses the click, and
// failed reads after it are no navigation, so a page that never moved fails
// the expectation with the read's error. A read that finds the page moved
// is one, and so is a document commit its marks do not show (a reload on
// WebKit, whose mark is the address and the history length), even when no
// mark after it can be read.
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
		{"a reload its marks do not show counts by its commit", &navigationDriver{first: here, later: here, commit: true}, 1, true, false},
		{"a commit counts though no mark after it can be read", &navigationDriver{first: here, laterErr: refused, commit: true}, 1, true, false},
		{"no commit and the same marks are no navigation", &navigationDriver{first: here, later: here}, 1, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			driver := tc.driver
			result, err := clickExpectingNavigation(t, driver)
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

// clickExpectingNavigation clicks driver's one element on a page of a new
// Manager, expecting a navigation within 300 ms.
func clickExpectingNavigation(t *testing.T, driver *navigationDriver) (LocatorResult, error) {
	t.Helper()
	m, engine := newLifecycleManager(t)
	access := m.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	engine.mu.Lock()
	engine.onNewPage = func(hooks pageHooks) {
		driver.mu.Lock()
		driver.hooks = hooks
		driver.mu.Unlock()
	}
	engine.wrapDriver = func(inner pageDriver) pageDriver {
		driver.pageDriver = inner
		return driver
	}
	engine.mu.Unlock()
	page := mustOpen(t, m, access, "https://example.test/a")
	return m.Locator(t.Context(), access, LocatorOptions{
		PageID: PageID(page.ID), Action: "click", Locator: Locator{CSS: "#go"},
		ExpectNavigation: true, TimeoutMS: 300,
	})
}

// A commit the Manager counted decides the navigation before any engine
// mark is read: one made before the wait is never hidden by a mark read,
// whether that read answers, fails or holds to the deadline, and one made
// during a read counts once the read returns. A navigation that happened is
// reported as one even when the load state was not reached, which the
// error says. With no commit and the marks unmoved there is no navigation.
func TestAnExpectedNavigationIsDecidedByItsCommitFirst(t *testing.T) {
	refused := errors.New("frame tree unreadable")
	here := navigationMark{URL: "https://example.test/a", Loader: "1"}
	type mark struct {
		name   string
		err    error
		blocks bool
	}
	marks := []mark{{"the mark reads", nil, false}, {"the mark errors", refused, false}, {"the mark holds to the deadline", nil, true}}
	for _, commit := range []string{"before the wait", "during the wait", "never"} {
		for _, mk := range marks {
			t.Run(commit+", "+mk.name, func(t *testing.T) {
				driver := &navigationDriver{
					first: here, later: here, laterErr: mk.err, laterBlocks: mk.blocks,
					commit: commit == "before the wait", commitOnRead: commit == "during the wait",
				}
				result, err := clickExpectingNavigation(t, driver)
				switch {
				case commit == "before the wait":
					if err != nil || !result.Navigated {
						t.Fatalf("answered %+v, %v; want the navigation", result, err)
					}
					if n := driver.readsAfterClick(); n != 0 {
						t.Fatalf("the mark was read %d times after a counted commit", n)
					}
				case commit == "during the wait" && mk.blocks:
					var notReady *navigatedNotReady
					if !result.Navigated || !errors.As(err, &notReady) || !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("answered %+v, %v; want the navigation, and the load state's deadline", result, err)
					}
				case commit == "during the wait":
					if err != nil || !result.Navigated {
						t.Fatalf("answered %+v, %v; want the navigation", result, err)
					}
				default:
					if result.Navigated || !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("answered %+v, %v; want no navigation at the deadline", result, err)
					}
					if mk.err != nil && !errors.Is(err, refused) {
						t.Fatalf("answered %v, want the read's failure", err)
					}
				}
			})
		}
	}
}
