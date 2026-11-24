package browser

import (
	"context"
	"errors"
	"strings"
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
	// infoErr, when set, fails every read of the page's info after the
	// click.
	infoErr error
	hooks   pageHooks
}

func (d *navigationDriver) Info(ctx context.Context) (string, string, error) {
	d.mu.Lock()
	acted, err := d.acted, d.infoErr
	d.mu.Unlock()
	if acted > 0 && err != nil {
		return "", "", err
	}
	return d.pageDriver.Info(ctx)
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
// is one. A document commit is one too, whatever the marks show
// (TestAnExpectedNavigationIsDecidedByItsCommitFirst).
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
	return clickExpecting(t, driver, false)
}

// clickExpecting is clickExpectingNavigation, expecting a download too with
// download.
func clickExpecting(t *testing.T, driver *navigationDriver, download bool) (LocatorResult, error) {
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
		ExpectNavigation: true, ExpectDownload: download, TimeoutMS: 300,
	})
}

// commitAtDeadline is a context whose deadline has passed by the time it is
// asked, and whose page commits a document just before: the commit lands
// after the wait's last read and before it sees the deadline.
type commitAtDeadline struct {
	context.Context
	commit func()
}

func (c commitAtDeadline) Done() <-chan struct{} {
	c.commit()
	done := make(chan struct{})
	close(done)
	return done
}

func (commitAtDeadline) Err() error { return context.DeadlineExceeded }

// A commit counted between the wait's last read and its deadline is a
// navigation: the wait reads the count once more as the deadline ends it,
// and says the load state was not seen.
func TestACommitAsTheDeadlineEndsTheWaitIsANavigation(t *testing.T) {
	here := navigationMark{URL: "https://example.test/a", Loader: "1"}
	p := &managedPage{driver: &navigationDriver{first: here, later: here}}
	before, err := readNavigation(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	ctx := commitAtDeadline{Context: t.Context(), commit: func() { p.docEpoch.Add(1) }}
	navigated, err := (&Manager{}).waitForNavigation(ctx, p, before, "", "")
	var notReady *navigatedNotReady
	if !navigated || !errors.As(err, &notReady) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("answered %v, %v; want the navigation, and the deadline for its load state", navigated, err)
	}
}

// A wait for the commit alone asks for nothing beyond the navigation: a
// commit as the deadline ends it is the whole answer, and so is one counted
// while the page's state cannot be read.
func TestAWaitForTheCommitAloneEndsOnTheNavigation(t *testing.T) {
	here := navigationMark{URL: "https://example.test/a", Loader: "1"}
	t.Run("as the deadline ends the wait", func(t *testing.T) {
		p := &managedPage{driver: &navigationDriver{first: here, later: here}}
		before, err := readNavigation(t.Context(), p)
		if err != nil {
			t.Fatal(err)
		}
		ctx := commitAtDeadline{Context: t.Context(), commit: func() { p.docEpoch.Add(1) }}
		if navigated, err := (&Manager{}).waitForNavigation(ctx, p, before, "", " Commit "); !navigated || err != nil {
			t.Fatalf("answered %v, %v; want the navigation", navigated, err)
		}
		if navigated, err := (&Manager{}).waitForNavigation(ctx, p, pageNavigation{mark: here, commits: p.docEpoch.Load()}, "**/b", "commit"); !navigated || err == nil {
			t.Fatalf("with an address to reach it answered %v, %v; want the navigation, and the deadline", navigated, err)
		}
	})
	t.Run("with the page's state unreadable", func(t *testing.T) {
		// The driver has no page behind it: any read of its state panics.
		p := &managedPage{driver: &navigationDriver{first: here, later: here}}
		before, err := readNavigation(t.Context(), p)
		if err != nil {
			t.Fatal(err)
		}
		p.docEpoch.Add(1)
		if navigated, err := (&Manager{}).waitForNavigation(t.Context(), p, before, "", "commit"); !navigated || err != nil {
			t.Fatalf("answered %v, %v; want the navigation", navigated, err)
		}
	})
}

// A click whose expected navigation happened answers the navigation with
// the failure of the page's last read, when that read fails.
func TestANavigationIsReportedWhenThePageCannotBeReadAfter(t *testing.T) {
	here := navigationMark{URL: "https://example.test/a", Loader: "1"}
	unreadable := errors.New("the page is gone")
	result, err := clickExpectingNavigation(t, &navigationDriver{first: here, later: here, commit: true, infoErr: unreadable})
	if !result.Navigated || !errors.Is(err, unreadable) || !strings.Contains(err.Error(), "the page navigated") {
		t.Fatalf("answered %+v, %v; want the navigation, and the read's failure", result, err)
	}
}

// A click that expects a navigation and a download, navigates and starts
// no download answers the navigation with the download wait's failure.
func TestANavigationIsReportedWhenItsExpectedDownloadNeverSettles(t *testing.T) {
	here := navigationMark{URL: "https://example.test/a", Loader: "1"}
	result, err := clickExpecting(t, &navigationDriver{first: here, later: here, commit: true}, true)
	if !result.Navigated || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "the page navigated") {
		t.Fatalf("answered %+v, %v; want the navigation, and the download wait's deadline", result, err)
	}
}

// A commit the Manager counted decides the navigation before any engine
// mark is read (a reload on WebKit, whose mark is the address and the
// history length, shows none): one made before the wait is never hidden by
// a mark read, whether that read answers, fails or holds to the deadline,
// and one made during a read counts once the read returns. A navigation
// that happened is reported as one even when the load state was not
// reached, which the error says. With no commit and the marks unmoved there
// is no navigation. Each case clicks once.
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
				driver.mu.Lock()
				acted := driver.acted
				driver.mu.Unlock()
				if acted != 1 {
					t.Fatalf("the click ran %d times, want 1", acted)
				}
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
