package browser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

func TestFileAddressClassifiesWindowsAndFileURLForms(t *testing.T) {
	for _, tc := range []struct {
		address string
		path    string
		windows string
		notFile bool
		err     bool
	}{
		{address: `C:\Users\me\report.html`, windows: `C:\Users\me\report.html`},
		{address: `c:/Users/me/report.html`, windows: `c:\Users\me\report.html`},
		{address: `D:\`, windows: `D:\`},
		{address: `\\wsl.localhost\Ubuntu\home\me\x.html`, windows: `\\wsl.localhost\Ubuntu\home\me\x.html`},
		{address: `\\wsl$\Ubuntu\home\me\x.html`, windows: `\\wsl$\Ubuntu\home\me\x.html`},
		{address: `\\server\share\x.html`, windows: `\\server\share\x.html`},
		{address: `file:///C:/Users/me/a%20b.html`, windows: `C:\Users\me\a b.html`},
		{address: `FILE:///c:/x.html`, windows: `c:\x.html`},
		{address: `file:C:/x.html`, windows: `C:\x.html`},
		{address: `file://wsl.localhost/Ubuntu/home/me/x.html`, windows: `\\wsl.localhost\Ubuntu\home\me\x.html`},
		{address: `file://wsl$/Ubuntu/home/me/x.html`, windows: `\\wsl$\Ubuntu\home\me\x.html`},
		{address: `file:///home/me/x.html`, path: filepath.FromSlash("/home/me/x.html")},
		{address: `file://localhost/home/me/x.html`, path: filepath.FromSlash("/home/me/x.html")},
		{address: `file://wsl.localhost/`, err: true},
		{address: `file:///C:/a%zz`, err: true},
		{address: `file:relative.html`, err: true},
		{address: `example.com`, notFile: true},
		{address: `localhost:3000`, notFile: true},
		{address: `C:`, notFile: true},
		{address: `what is c:\ drive`, notFile: true},
		{address: `/home/me/x.html`, notFile: true},
		{address: `https://example.test/C:/x`, notFile: true},
	} {
		t.Run(tc.address, func(t *testing.T) {
			target, isFile, err := fileAddress(tc.address)
			if tc.err {
				if err == nil || !strings.Contains(err.Error(), tc.address) {
					t.Fatalf("fileAddress = %+v, %v, %v; want an error naming the address", target, isFile, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("fileAddress error = %v", err)
			}
			if isFile == tc.notFile {
				t.Fatalf("isFile = %v, want %v", isFile, !tc.notFile)
			}
			if target.path != tc.path || target.windows != tc.windows {
				t.Fatalf("target = %+v, want path %q windows %q", target, tc.path, tc.windows)
			}
		})
	}
}

// wslEngine is the fake engine with the WSL build's file mapping: its
// renderer is on Windows, the distribution "Distro" is mounted at root and
// drive C: at drive, as `wslpath -u` maps them.
type wslEngine struct {
	*fakeEngine
	root, drive string
}

func (e wslEngine) FileURL(_ context.Context, path string) (string, error) {
	rel, err := filepath.Rel(e.root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("outside the distribution: %s", path)
	}
	return "file://wsl.localhost/Distro/" + filepath.ToSlash(rel), nil
}

func (e wslEngine) BackendFilePath(_ context.Context, rawURL string) (string, error) {
	windowsPath, err := windowsPathFromFileURL(rawURL)
	if err != nil {
		return "", err
	}
	parts := strings.Split(strings.TrimPrefix(windowsPath, `\\`), `\`)
	switch {
	case strings.HasPrefix(windowsPath, `\\`) && len(parts) >= 2 &&
		(strings.EqualFold(parts[0], "wsl.localhost") || parts[0] == "wsl$") && strings.EqualFold(parts[1], "Distro"):
		return filepath.Join(append([]string{e.root}, parts[2:]...)...), nil
	case strings.EqualFold(windowsPath[:2], "C:"):
		return filepath.Join(append([]string{e.drive}, strings.Split(windowsPath[3:], `\`)...)...), nil
	}
	return "", errors.New("wslpath: exit status 1")
}

func newNavigateFixture(t *testing.T, wsl bool) (*Manager, Access, string, string) {
	t.Helper()
	m := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Errorf("close the manager: %v", err)
		}
	})
	root := t.TempDir()
	workspace := filepath.Join(root, "repo")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "report.html"), []byte("<p>hi</p>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if wsl {
		m.engine = wslEngine{fakeEngine: newFakeEngine(), root: root, drive: workspace}
	}
	access := m.Bind(Caller{ThreadID: "thread", Workspace: workspace})
	info, err := m.Open(t.Context(), access, "https://example.test/start", OpenOptions{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return m, access, info.ID, root
}

func pageURL(t *testing.T, m *Manager, access Access, pageID string) string {
	t.Helper()
	for _, page := range m.CompanionState(access.Caller()).Pages {
		if page.ID == pageID {
			return page.URL
		}
	}
	t.Fatalf("page %s is gone", pageID)
	return ""
}

// On the WSL build every Windows spelling of a file resolves to its backend
// path through the engine's mapping and opens under the file authorization.
func TestNavigateCompanionResolvesWindowsAddressesOnTheWSLBuild(t *testing.T) {
	for _, address := range []string{
		`\\wsl.localhost\Distro\repo\report.html`,
		`\\wsl$\distro\repo\report.html`,
		`\\wsl.localhost\Distro/repo/report.html`,
		`file://wsl.localhost/Distro/repo/report.html`,
		`file://wsl$/Distro/repo/report.html`,
		`C:\report.html`,
		`c:/report.html`,
		`file:///C:/report.html`,
	} {
		t.Run(address, func(t *testing.T) {
			m, access, pageID, _ := newNavigateFixture(t, true)
			if _, err := m.NavigateCompanion(t.Context(), access, PageID(pageID), address); err != nil {
				t.Fatalf("navigate = %v", err)
			}
			if got := pageURL(t, m, access, pageID); got != "file://wsl.localhost/Distro/repo/report.html" {
				t.Fatalf("page URL = %q, want the file in the renderer's form", got)
			}
		})
	}
	// A backend path's own file URL opens too, though the renderer would
	// spell it otherwise.
	m, access, pageID, root := newNavigateFixture(t, true)
	backend := "file://" + filepath.ToSlash(filepath.Join(root, "repo", "report.html"))
	if _, err := m.NavigateCompanion(t.Context(), access, PageID(pageID), backend); err != nil {
		t.Fatalf("navigate %s = %v", backend, err)
	}
}

// A Windows path that cannot resolve is an error that names it, and nothing
// is navigated: no search, no page change.
func TestNavigateCompanionRefusesUnreachableWindowsAddresses(t *testing.T) {
	for _, tc := range []struct {
		wsl     bool
		address string
		names   string
	}{
		{true, `\\wsl.localhost\Other\home\x.html`, `\\wsl.localhost\Other\home\x.html`},
		{true, `\\wsl$\Other\home\x.html`, `\\wsl$\Other\home\x.html`},
		{true, `file://wsl.localhost/Other/home/x.html`, `\\wsl.localhost\Other\home\x.html`},
		{true, `\\server\share\x.html`, `\\server\share\x.html`},
		{true, `D:\x.html`, `D:\x.html`},
		{false, `C:\Users\me\report.html`, `C:\Users\me\report.html`},
		{false, `c:/Users/me/report.html`, `c:\Users\me\report.html`},
		{false, `file:///C:/Users/me/report.html`, `C:\Users\me\report.html`},
		{false, `\\wsl.localhost\Distro\repo\report.html`, `\\wsl.localhost\Distro\repo\report.html`},
		{false, `file://wsl$/Distro/repo/report.html`, `\\wsl$\Distro\repo\report.html`},
	} {
		t.Run(fmt.Sprintf("wsl=%v %s", tc.wsl, tc.address), func(t *testing.T) {
			m, access, pageID, _ := newNavigateFixture(t, tc.wsl)
			_, err := m.NavigateCompanion(t.Context(), access, PageID(pageID), tc.address)
			if err == nil || !strings.Contains(err.Error(), tc.names) {
				t.Fatalf("navigate = %v, want an error naming %s", err, tc.names)
			}
			if got := pageURL(t, m, access, pageID); got != "https://example.test/start" {
				t.Fatalf("page URL = %q after a refused address", got)
			}
		})
	}
}

// Stop reaches the engine while an address-bar navigation holds the page,
// and the navigation it cut short answers without an error; one that fails
// without a stop still reports its failure.
func TestStopCompanionLoadStopsAHeldNavigationWithoutAnError(t *testing.T) {
	m, access, pageID, _ := newNavigateFixture(t, false)
	p, _, err := m.lookupOwnedPage(access, PageID(pageID))
	if err != nil {
		t.Fatal(err)
	}
	fake := p.driver.(*fakePage)
	entered := make(chan struct{})
	release := make(chan struct{})
	fake.mu.Lock()
	fake.navigateHook = func(ctx context.Context, _ string) error {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return fmt.Errorf("%w: net::ERR_ABORTED", errNavigationAborted)
	}
	fake.mu.Unlock()

	done := make(chan error, 1)
	go func() {
		_, err := m.NavigateCompanion(context.Background(), access, PageID(pageID), "https://example.test/slow")
		done <- err
	}()
	<-entered
	stopped := make(chan error, 1)
	go func() { stopped <- m.StopCompanionLoad(context.Background(), access, PageID(pageID)) }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("stop = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stop waited for the navigation it was meant to stop")
	}
	fake.mu.Lock()
	stops := fake.stops
	fake.mu.Unlock()
	if stops != 1 {
		t.Fatalf("the engine saw %d stops, want 1", stops)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("the stopped navigation answered %v", err)
	}

	fake.mu.Lock()
	fake.navigateHook = func(context.Context, string) error { return errors.New("net::ERR_NAME_NOT_RESOLVED") }
	fake.mu.Unlock()
	if _, err := m.NavigateCompanion(t.Context(), access, PageID(pageID), "https://example.test/broken"); err == nil || !strings.Contains(err.Error(), "ERR_NAME_NOT_RESOLVED") {
		t.Fatalf("an unstopped failure answered %v", err)
	}
}

// stoppableFake makes the fixture page's navigation to /held wait until a
// stop reaches the engine and then fail with heldErr, fails /queued with
// queuedErr, and runs stopResult as the engine's stop. It returns a channel
// closed once /held is inside the engine.
func stoppableFake(t *testing.T, p *managedPage, heldErr, queuedErr, stopResult error) <-chan struct{} {
	t.Helper()
	fake := p.driver.(*fakePage)
	entered := make(chan struct{})
	abort := make(chan struct{})
	var once sync.Once
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.navigateHook = func(ctx context.Context, rawURL string) error {
		switch {
		case strings.HasSuffix(rawURL, "/held"):
			close(entered)
			select {
			case <-abort:
				return heldErr
			case <-ctx.Done():
				return ctx.Err()
			}
		case strings.HasSuffix(rawURL, "/queued"):
			return queuedErr
		}
		return nil
	}
	fake.stopHook = func() error {
		once.Do(func() { close(abort) })
		return stopResult
	}
	return entered
}

func navigateAsync(m *Manager, access Access, pageID, address string) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := m.NavigateCompanion(context.Background(), access, PageID(pageID), address)
		done <- err
	}()
	return done
}

func awaitResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
		return nil
	}
}

// A stop is the executing navigation's: one queued behind it keeps its own
// failure, a real one or an abort the stop did not cause.
func TestStopCompanionLoadLeavesAQueuedNavigationsFailureStanding(t *testing.T) {
	for _, queuedErr := range []error{
		errors.New("net::ERR_NAME_NOT_RESOLVED"),
		fmt.Errorf("%w: net::ERR_ABORTED by the page", errNavigationAborted),
	} {
		t.Run(queuedErr.Error(), func(t *testing.T) {
			m, access, pageID, _ := newNavigateFixture(t, false)
			p, _, err := m.lookupOwnedPage(access, PageID(pageID))
			if err != nil {
				t.Fatal(err)
			}
			entered := stoppableFake(t, p, fmt.Errorf("%w: net::ERR_ABORTED", errNavigationAborted), queuedErr, nil)
			held := navigateAsync(m, access, pageID, "https://example.test/held")
			<-entered
			queued := navigateAsync(m, access, pageID, "https://example.test/queued")
			// Let the queued navigation reach the page lock the held one owns.
			time.Sleep(50 * time.Millisecond)
			if err := m.StopCompanionLoad(t.Context(), access, PageID(pageID)); err != nil {
				t.Fatalf("stop = %v", err)
			}
			if err := awaitResult(t, held); err != nil {
				t.Fatalf("the stopped navigation answered %v", err)
			}
			if err := awaitResult(t, queued); err == nil || err.Error() != queuedErr.Error() {
				t.Fatalf("the queued navigation answered %v, want %v", err, queuedErr)
			}
		})
	}
}

// A stop the engine refused stopped nothing, so the abort stands.
func TestStopCompanionLoadThatFailsLeavesTheAbortStanding(t *testing.T) {
	m, access, pageID, _ := newNavigateFixture(t, false)
	p, _, err := m.lookupOwnedPage(access, PageID(pageID))
	if err != nil {
		t.Fatal(err)
	}
	entered := stoppableFake(t, p, fmt.Errorf("%w: net::ERR_ABORTED", errNavigationAborted), nil, errors.New("stop refused"))
	held := navigateAsync(m, access, pageID, "https://example.test/held")
	<-entered
	if err := m.StopCompanionLoad(t.Context(), access, PageID(pageID)); err == nil {
		t.Fatal("a refused stop answered no error")
	}
	if err := awaitResult(t, held); !errors.Is(err, errNavigationAborted) {
		t.Fatalf("the navigation answered %v, want its abort", err)
	}
}

// Only the stop's own abort is suppressed: the stopped navigation's other
// failures stand.
func TestStopCompanionLoadSuppressesOnlyTheAbort(t *testing.T) {
	m, access, pageID, _ := newNavigateFixture(t, false)
	p, _, err := m.lookupOwnedPage(access, PageID(pageID))
	if err != nil {
		t.Fatal(err)
	}
	entered := stoppableFake(t, p, errors.New("net::ERR_CONNECTION_RESET"), nil, nil)
	held := navigateAsync(m, access, pageID, "https://example.test/held")
	<-entered
	if err := m.StopCompanionLoad(t.Context(), access, PageID(pageID)); err != nil {
		t.Fatalf("stop = %v", err)
	}
	if err := awaitResult(t, held); err == nil || !strings.Contains(err.Error(), "ERR_CONNECTION_RESET") {
		t.Fatalf("the navigation answered %v, want its failure", err)
	}
}

// fakeLoads models the engine's loads: each navigation to /<name> is a load
// that runs until release(name), or until a stop aborts the load running
// at that moment. beforeStop runs at the start of the nth engine stop, so a
// test can hold a stop inside the engine.
type fakeLoads struct {
	mu         sync.Mutex
	current    chan struct{}
	entered    map[string]chan struct{}
	release    map[string]chan struct{}
	stops      int
	beforeStop func(n int)
}

func installFakeLoads(t *testing.T, p *managedPage, names ...string) *fakeLoads {
	t.Helper()
	l := &fakeLoads{entered: map[string]chan struct{}{}, release: map[string]chan struct{}{}, beforeStop: func(int) {}}
	for _, name := range names {
		l.entered[name] = make(chan struct{})
		l.release[name] = make(chan struct{})
	}
	fake := p.driver.(*fakePage)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.navigateHook = func(ctx context.Context, rawURL string) error {
		name := rawURL[strings.LastIndex(rawURL, "/")+1:]
		abort := make(chan struct{})
		l.mu.Lock()
		l.current = abort
		close(l.entered[name])
		l.mu.Unlock()
		defer func() {
			l.mu.Lock()
			if l.current == abort {
				l.current = nil
			}
			l.mu.Unlock()
		}()
		select {
		case <-abort:
			return fmt.Errorf("%w: net::ERR_ABORTED", errNavigationAborted)
		case <-l.release[name]:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	fake.stopHook = func() error {
		l.mu.Lock()
		l.stops++
		n := l.stops
		before := l.beforeStop
		l.mu.Unlock()
		before(n)
		l.mu.Lock()
		if l.current != nil {
			close(l.current)
			l.current = nil
		}
		l.mu.Unlock()
		return nil
	}
	return l
}

// within reports whether ch closes within d.
func within(ch <-chan struct{}, d time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}

// A stop is dispatched against the navigation registered when it started:
// one that finishes meanwhile cannot hand its place to the next, so the
// next navigation's load is not the one stopped.
func TestStopCompanionLoadCannotStopANavigationThatReplacedItsTarget(t *testing.T) {
	m, access, pageID, _ := newNavigateFixture(t, false)
	p, _, err := m.lookupOwnedPage(access, PageID(pageID))
	if err != nil {
		t.Fatal(err)
	}
	loads := installFakeLoads(t, p, "a", "b")
	inStop := make(chan struct{})
	resume := make(chan struct{})
	loads.beforeStop = func(int) {
		close(inStop)
		<-resume
	}
	a := navigateAsync(m, access, pageID, "https://example.test/a")
	<-loads.entered["a"]
	stopped := make(chan error, 1)
	go func() { stopped <- m.StopCompanionLoad(context.Background(), access, PageID(pageID)) }()
	<-inStop
	// A finishes on its own while the stop is in the engine, and B follows.
	close(loads.release["a"])
	b := navigateAsync(m, access, pageID, "https://example.test/b")
	if within(loads.entered["b"], 200*time.Millisecond) {
		t.Error("the next navigation started while a stop was in flight")
	}
	close(resume)
	if err := <-stopped; err != nil {
		t.Fatalf("stop = %v", err)
	}
	if err := awaitResult(t, a); err != nil {
		t.Fatalf("the finished navigation answered %v", err)
	}
	<-loads.entered["b"]
	close(loads.release["b"])
	if err := awaitResult(t, b); err != nil {
		t.Fatalf("the next navigation answered %v, want its own load to finish", err)
	}
}

// Two stops pressed together: the second runs after the first, against the
// navigation registered then, never against a load the first let start.
func TestStopCompanionLoadTwiceNeverStopsTheNextNavigationUnmarked(t *testing.T) {
	m, access, pageID, _ := newNavigateFixture(t, false)
	p, _, err := m.lookupOwnedPage(access, PageID(pageID))
	if err != nil {
		t.Fatal(err)
	}
	loads := installFakeLoads(t, p, "a", "b")
	inFirst := make(chan struct{})
	resumeFirst := make(chan struct{})
	loads.beforeStop = func(n int) {
		if n == 1 {
			close(inFirst)
			<-resumeFirst
			return
		}
		// The second stop reaches the engine; give the next navigation the
		// chance to start under it, as an unguarded stop would.
		within(loads.entered["b"], 200*time.Millisecond)
	}
	a := navigateAsync(m, access, pageID, "https://example.test/a")
	<-loads.entered["a"]
	first := make(chan error, 1)
	go func() { first <- m.StopCompanionLoad(context.Background(), access, PageID(pageID)) }()
	<-inFirst
	second := make(chan error, 1)
	go func() { second <- m.StopCompanionLoad(context.Background(), access, PageID(pageID)) }()
	time.Sleep(50 * time.Millisecond)
	b := navigateAsync(m, access, pageID, "https://example.test/b")
	time.Sleep(50 * time.Millisecond)
	close(resumeFirst)
	for _, ch := range []chan error{first, second} {
		if err := <-ch; err != nil {
			t.Fatalf("stop = %v", err)
		}
	}
	if err := awaitResult(t, a); err != nil {
		t.Fatalf("the stopped navigation answered %v", err)
	}
	<-loads.entered["b"]
	close(loads.release["b"])
	// B either ran after both stops, or was the navigation the second stop
	// stopped and is marked for it: no error either way.
	if err := awaitResult(t, b); err != nil {
		t.Fatalf("the next navigation answered %v", err)
	}
}

func TestCDPNavigateErrorMarksOnlyTheAbort(t *testing.T) {
	if err := cdpNavigateError(&chromedp.LoadError{ErrorText: "net::ERR_ABORTED"}); !errors.Is(err, errNavigationAborted) {
		t.Fatalf("ERR_ABORTED = %v, want errNavigationAborted", err)
	}
	if err := cdpNavigateError(&chromedp.LoadError{ErrorText: "net::ERR_NAME_NOT_RESOLVED"}); errors.Is(err, errNavigationAborted) {
		t.Fatalf("ERR_NAME_NOT_RESOLVED = %v, marked as an abort", err)
	}
	if err := cdpNavigateError(context.DeadlineExceeded); errors.Is(err, errNavigationAborted) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline = %v", err)
	}
}

func TestStopCompanionLoadRefusesAnotherThreadsPageAndIgnoresATabWithNoPage(t *testing.T) {
	m, access, pageID, _ := newNavigateFixture(t, false)
	other := m.Bind(Caller{ThreadID: "other", Workspace: access.Workspace})
	if err := m.StopCompanionLoad(t.Context(), other, PageID(pageID)); !errors.Is(err, errPageNotFound) {
		t.Fatalf("stop of another thread's page = %v", err)
	}
	if err := m.StopCompanionLoad(t.Context(), access, "missing"); !errors.Is(err, errPageNotFound) {
		t.Fatalf("stop of a missing page = %v", err)
	}
	// A suspended tab has no live page and nothing loading: no error, and
	// nothing restores it.
	if err := m.SuspendThread(t.Context(), access.ThreadID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if err := m.StopCompanionLoad(t.Context(), access, PageID(pageID)); err != nil {
		t.Fatalf("stop of a suspended tab = %v", err)
	}
	if p, _, _ := m.lookupOwnedPage(access, PageID(pageID)); p != nil {
		t.Fatal("stop restored the suspended page")
	}
}
