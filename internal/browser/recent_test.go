package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func recentURLs(m *Manager, access Access) []string {
	var urls []string
	for _, entry := range m.RecentAddresses(access.Caller()) {
		urls = append(urls, entry.URL)
	}
	return urls
}

func TestRecordableAddressKeepsWebAndFileAddressesOnly(t *testing.T) {
	for address, want := range map[string]bool{
		"https://example.test/a":                  true,
		"http://localhost:5173/":                  true,
		"file:///home/me/report.html":             true,
		"file://wsl.localhost/Ubuntu/home/x.html": true,
		"":                              false,
		"about:blank":                   false,
		"about:srcdoc":                  false,
		"data:text/html,hi":             false,
		"blob:https://example.test/1":   false,
		"chrome-error://chromewebdata/": false,
		"javascript:alert(1)":           false,
		"devtools://devtools/bundled/inspector.html": false,
		"https:///nohost": false,
		"https://example.test/" + strings.Repeat("x", maxRecentAddressBytes): false,
	} {
		if got := recordableAddress(address); got != want {
			t.Errorf("recordableAddress(%.60q) = %v, want %v", address, got, want)
		}
	}
}

// Opens by the address bar and by the agent's tools record what loaded,
// newest first, once per address; a failed open, a history move and a
// reload record nothing.
func TestRecentAddressesRecordCommittedOpensNewestFirst(t *testing.T) {
	m, access, pageID, _ := newNavigateFixture(t, false)
	if _, err := m.NavigateCompanion(t.Context(), access, pageID, "example.test/b"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.OpenFile(t.Context(), access, filepath.Join(access.Workspace, "report.html"), OpenOptions{PageID: pageID}); err != nil {
		t.Fatal(err)
	}
	// The address again moves it to the front instead of adding it twice.
	if _, err := m.Open(t.Context(), access, "https://example.test/start", OpenOptions{PageID: pageID}); err != nil {
		t.Fatal(err)
	}
	report := "file://" + filepath.ToSlash(filepath.Join(access.Workspace, "report.html"))
	want := []string{"https://example.test/start", report, "https://example.test/b"}
	if got := recentURLs(m, access); !reflect.DeepEqual(got, want) {
		t.Fatalf("recent = %q, want %q", got, want)
	}
	if got := m.RecentAddresses(access.Caller())[2].Title; got == "" {
		t.Fatalf("an entry kept no title")
	}

	p, _, err := m.lookupOwnedPage(access, pageID)
	if err != nil {
		t.Fatal(err)
	}
	fake := p.driver.(*fakePage)
	fake.mu.Lock()
	fake.navigateHook = func(context.Context, string) error { return errors.New("net::ERR_CONNECTION_REFUSED") }
	fake.mu.Unlock()
	if _, err := m.NavigateCompanion(t.Context(), access, pageID, "localhost:1"); err == nil {
		t.Fatal("the refused navigation succeeded")
	}
	fake.mu.Lock()
	fake.navigateHook = nil
	fake.mu.Unlock()
	if _, err := m.History(t.Context(), access, pageID, "back"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.History(t.Context(), access, pageID, "reload"); err != nil {
		t.Fatal(err)
	}
	if got := recentURLs(m, access); !reflect.DeepEqual(got, want) {
		t.Fatalf("recent = %q after a failed open, back and reload, want %q", got, want)
	}

	// A page left on no address of its own records nothing.
	m.recordRecentAddress(p, PageInfo{ID: pageID, URL: "about:blank"})
	if got := recentURLs(m, access); !reflect.DeepEqual(got, want) {
		t.Fatalf("recent = %q after a blank page, want %q", got, want)
	}

	// Another thread's list is its own.
	if got := m.RecentAddresses(Caller{ThreadID: "other", Workspace: access.Workspace}); len(got) != 0 {
		t.Fatalf("another thread sees %v", got)
	}
}

func TestRecentAddressesAreBounded(t *testing.T) {
	m, access, pageID, _ := newNavigateFixture(t, false)
	for i := range maxRecentAddresses + 5 {
		if _, err := m.Open(t.Context(), access, fmt.Sprintf("https://example.test/%d", i), OpenOptions{PageID: pageID}); err != nil {
			t.Fatal(err)
		}
	}
	got := recentURLs(m, access)
	if len(got) != maxRecentAddresses {
		t.Fatalf("kept %d addresses, want %d", len(got), maxRecentAddresses)
	}
	if newest := fmt.Sprintf("https://example.test/%d", maxRecentAddresses+4); got[0] != newest {
		t.Fatalf("newest = %q, want %q", got[0], newest)
	}
	if oldest := "https://example.test/5"; got[len(got)-1] != oldest {
		t.Fatalf("oldest kept = %q, want %q", got[len(got)-1], oldest)
	}
}

// A private jar keeps nothing, and a jar switch, either way, forgets the list.
func TestRecentAddressesForgetOnJarSwitchAndKeepNothingPrivate(t *testing.T) {
	m, access, pageID, _ := newNavigateFixture(t, false)
	if len(recentURLs(m, access)) != 1 {
		t.Fatalf("recent = %q, want the fixture's open", recentURLs(m, access))
	}
	if err := m.SetJar(context.Background(), access, JarThread, JarSwitchConfirmed); err != nil {
		t.Fatal(err)
	}
	if got := recentURLs(m, access); len(got) != 0 {
		t.Fatalf("recent = %q after a jar switch", got)
	}
	if _, err := m.Open(t.Context(), access, "https://example.test/thread-jar", OpenOptions{PageID: pageID}); err != nil {
		t.Fatal(err)
	}
	if got := recentURLs(m, access); !reflect.DeepEqual(got, []string{"https://example.test/thread-jar"}) {
		t.Fatalf("recent = %q in the thread jar", got)
	}
	if err := m.SetJar(context.Background(), access, JarPrivate, JarSwitchConfirmed); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Open(t.Context(), access, "https://example.test/private", OpenOptions{PageID: pageID}); err != nil {
		t.Fatal(err)
	}
	if got := recentURLs(m, access); len(got) != 0 {
		t.Fatalf("recent = %q in a private jar", got)
	}
}

func TestRecentAddressesForgetOnClearSiteDataAndThreadClose(t *testing.T) {
	m, access, _, _ := newNavigateFixture(t, false)
	other := m.Bind(Caller{ThreadID: "other", Workspace: access.Workspace})
	if _, err := m.Open(t.Context(), other, "https://example.test/other", OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := m.CloseThread(t.Context(), other.ThreadID); err != nil {
		t.Fatal(err)
	}
	if got := recentURLs(m, other); len(got) != 0 {
		t.Fatalf("recent = %q after the thread closed", got)
	}
	if got := recentURLs(m, access); len(got) != 1 {
		t.Fatalf("closing another thread took this one's list: %q", got)
	}
	if err := m.ClearSiteData(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := recentURLs(m, access); len(got) != 0 {
		t.Fatalf("recent = %q after Clear site data", got)
	}
}

// Every forget advances the Manager's forget generation, and the thread's
// pushed state reports the generation of its last forget: a jar switch, the
// close of another thread, and the close of every page (Clear site data,
// turning the browser off).
func TestRecentAddressForgetsAdvanceTheGenerationTheStateReports(t *testing.T) {
	m, access, pageID, _ := newNavigateFixture(t, false)
	var mu sync.Mutex
	pushed := map[string]string{}
	m.SetEventSink(func(event CompanionEvent) {
		if event.Kind == "state" {
			mu.Lock()
			pushed[event.ThreadID] = event.RecentGen
			mu.Unlock()
		}
	})
	lastPushed := func(threadID string) string {
		mu.Lock()
		defer mu.Unlock()
		return pushed[threadID]
	}
	reported := func() string { return m.CompanionState(access.Caller()).RecentGen }
	other := m.Bind(Caller{ThreadID: "other", Workspace: access.Workspace})
	if _, err := m.Open(t.Context(), other, "https://example.test/other", OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, forget := range []struct {
		name string
		run  func() error
		// pushes is whether the forget pushes this thread's state; closing
		// another thread does not, and its generation shows at the next push.
		pushes bool
	}{
		{"jar switch", func() error { return m.SetJar(context.Background(), access, JarThread, JarSwitchConfirmed) }, true},
		{"jar switch back", func() error { return m.SetJar(context.Background(), access, JarWorktree, JarSwitchConfirmed) }, true},
		{"another thread's close", func() error { return m.CloseThread(t.Context(), other.ThreadID) }, false},
		{"clear site data", func() error { return m.ClearSiteData(t.Context()) }, true},
		{"disable", func() error { return m.Reconfigure(Config{Enabled: false}) }, true},
	} {
		// A list to forget, so the close of every page pushes for it.
		if err := m.Reconfigure(Config{Enabled: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := m.Open(t.Context(), access, "https://example.test/"+strings.ReplaceAll(forget.name, " ", "-"), OpenOptions{PageID: pageIDFor(t, m, access, pageID)}); err != nil {
			t.Fatalf("%s: open: %v", forget.name, err)
		}
		before := reported()
		if err := forget.run(); err != nil {
			t.Fatalf("%s: %v", forget.name, err)
		}
		after := reported()
		if after == before {
			t.Fatalf("%s: the generation stayed %q", forget.name, after)
		}
		if forget.pushes && lastPushed(access.ThreadID) != after {
			t.Fatalf("%s: the last push carried %q, want %q", forget.name, lastPushed(access.ThreadID), after)
		}
	}
}

// A forget changes the generation as a JavaScript client reads the pushed
// state, where every JSON number is a double: a switch to Private with the
// same active tab still drops the field's list.
func TestTheForgetGenerationChangesOnTheWire(t *testing.T) {
	m, access, _, _ := newNavigateFixture(t, false)
	wire := func() any {
		raw, err := json.Marshal(m.CompanionState(access.Caller()))
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded["recentGen"]
	}
	before := wire()
	if err := m.SetJar(context.Background(), access, JarPrivate, JarSwitchConfirmed); err != nil {
		t.Fatal(err)
	}
	if after := wire(); after == before {
		t.Fatalf("after a forget the wire generation is still %v", after)
	}
}

// pageIDFor answers the thread's tab to open in: pageID while it exists,
// else a new page.
func pageIDFor(t *testing.T, m *Manager, access Access, pageID string) string {
	t.Helper()
	for _, page := range m.CompanionState(access.Caller()).Pages {
		if page.ID == pageID {
			return pageID
		}
	}
	return ""
}

// overtakeOpen runs an open on the page that the engine finishes only once
// overtake has taken the page out of the live set, as a load the engine had
// already committed finishes whatever its context says, and answers
// overtake's error once both are done.
func overtakeOpen(t *testing.T, m *Manager, access Access, pageID string, overtake func() error) error {
	t.Helper()
	p, _, err := m.lookupOwnedPage(access, pageID)
	if err != nil {
		t.Fatal(err)
	}
	fake := p.driver.(*fakePage)
	entered := make(chan struct{})
	release := make(chan struct{})
	fake.mu.Lock()
	fake.navigateHook = func(context.Context, string) error {
		close(entered)
		<-release
		return nil
	}
	fake.mu.Unlock()
	done := navigateAsync(m, access, pageID, "https://example.test/late")
	<-entered
	overtaken := make(chan error, 1)
	go func() { overtaken <- overtake() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		m.mu.Lock()
		live, _ := m.livePageLocked(pageID)
		m.mu.Unlock()
		if live != p {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the page never left the live set")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	<-done
	return <-overtaken
}

// An open that loads after its page left the thread's current jar (a switch
// overtook it) records nothing in the list the switch forgot.
func TestRecentAddressesIgnoreAnOpenAJarSwitchOvertook(t *testing.T) {
	m, access, pageID, _ := newNavigateFixture(t, false)
	if err := overtakeOpen(t, m, access, pageID, func() error { return m.SetJar(context.Background(), access, JarThread, JarSwitchConfirmed) }); err != nil {
		t.Fatal(err)
	}
	if got := recentURLs(m, access); len(got) != 0 {
		t.Fatalf("recent = %q: the overtaken open was recorded", got)
	}
}

// Turning the browser off, or changing whether site data persists, closes
// every page and forgets the recent addresses; an open that loads after the
// pages closed records nothing, and re-enabling brings none back.
func TestRecentAddressesForgetWhenTheBrowserIsDisabled(t *testing.T) {
	for _, change := range []struct {
		name string
		off  Config
	}{
		{"disable", Config{Enabled: false}},
		{"persistence", Config{Enabled: true, PersistSiteData: true}},
	} {
		t.Run(change.name, func(t *testing.T) {
			m, access, pageID, _ := newNavigateFixture(t, false)
			if got := recentURLs(m, access); len(got) != 1 {
				t.Fatalf("recent = %q, want the fixture's open", got)
			}
			if err := overtakeOpen(t, m, access, pageID, func() error { return m.Reconfigure(change.off) }); err != nil {
				t.Fatal(err)
			}
			if got := recentURLs(m, access); len(got) != 0 {
				t.Fatalf("recent = %q after %s", got, change.name)
			}
			if err := m.Reconfigure(Config{Enabled: true}); err != nil {
				t.Fatal(err)
			}
			if got := recentURLs(m, access); len(got) != 0 {
				t.Fatalf("recent = %q after re-enabling", got)
			}
		})
	}
}
