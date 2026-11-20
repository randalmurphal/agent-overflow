//go:build !windows

package browser

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/browser/ids"
)

// Two pages attached, the headless engine still pauses a request once: the
// browser-wide policy is the only one.
func TestHeadlessEnginePausesARequestOnce(t *testing.T) {
	browser := writeFakeChromium(t, "chromium")
	engine := newTestHeadlessEngine(t, browser.path)
	profile := testHeadlessProfile(t, engine, "/home/dev/repo", true)
	for range 2 {
		if _, err := profile.NewPage(context.Background(), testPageHooks()); err != nil {
			t.Fatalf("new page: %v", err)
		}
	}
	assertOnePolicyPause(t, browser.calls, browser.send)
}

// Popups reach the Manager only through the browser-level event handlers,
// and the Manager adopts one with CDP commands on the same connection. The
// subscriptions are made before discovery is enabled, so a popup announced
// during the handshake is reported, and adopting a popup does not wait on
// the handler that reported it.
//
// The handshake popup is announced once, before the reply, as Chromium
// reports the targets that exist when discovery is enabled. A subscription
// made after that reply never hears of it.
func TestHeadlessProfileReportsPopupsAndCanAdoptThem(t *testing.T) {
	var handshake sync.Once
	browser := writeAnnouncingFakeChromium(t, "chromium", func(call fakeCDPCall) []map[string]any {
		switch call.Method {
		case "Target.setDiscoverTargets":
			var events []map[string]any
			handshake.Do(func() { events = []map[string]any{popupCreated("P-handshake", "T-elsewhere")} })
			return events
		case "Target.createTarget":
			return []map[string]any{popupCreated("P-page", "T-1")}
		}
		return nil
	})
	engine := newTestHeadlessEngine(t, browser.path)
	profile := testHeadlessProfile(t, engine, "/home/dev/repo", true)

	var mu sync.Mutex
	reported := make(map[string]enginePopup)
	adopted := make(chan error, 1)
	engine.events.PopupOpened = func(popup enginePopup) {
		mu.Lock()
		reported[string(popup.Handle)] = popup
		mu.Unlock()
		if popup.Handle != "P-page" {
			return
		}
		// What Manager.popupOpened does with a popup it keeps: the report
		// returns at once and the adoption runs on its own goroutine.
		go func() {
			driver, err := profile.AttachPage(context.Background(), popup.Handle, testPageHooks())
			if err == nil {
				driver.Close()
			}
			adopted <- err
		}()
	}
	created := make(chan error, 1)
	go func() {
		_, err := profile.NewPage(context.Background(), testPageHooks())
		created <- err
	}()

	select {
	case err := <-adopted:
		if err != nil {
			t.Fatalf("adopt the popup: %v", err)
		}
	case <-time.After(headlessTestDeadline):
		t.Fatal("adopting the popup never finished")
	}
	select {
	case err := <-created:
		if err != nil {
			t.Fatalf("the page that opened the popup: %v", err)
		}
	case <-time.After(headlessTestDeadline):
		t.Fatal("creating the page that opened the popup never finished")
	}
	mu.Lock()
	defer mu.Unlock()
	for handle, opener := range map[string]string{"P-handshake": "T-elsewhere", "P-page": "T-1"} {
		got, ok := reported[handle]
		if !ok || got.Opener != ids.PageHandle(opener) || got.Profile != profile.handle {
			t.Fatalf("popup %s was reported as %+v (reported: %v)", handle, got, ok)
		}
		if owner, bound := engine.profileForPage(ids.PageHandle(handle)); !bound || owner != profile {
			t.Fatalf("popup %s is not bound to the profile that saw it", handle)
		}
	}
}

// The profile enables the jar's policy on the browser session at launch, so
// it sees a page Chromium opened by itself before the Manager adopts it as
// well as every page's requests, and answers what it pauses there with the
// Manager's policy for the jar. In a jar two worktrees share, a request has
// the authority of the page its frame belongs to, or of its popup's openers.
// A request whose owner is not resolved (a popup's request before Chromium
// reports the popup, a subframe no page reported, a popup whose opener
// closed) is allowed at once what a page with no workspace's authority may
// open; any other waits for one within the owner bound, then is refused.
func TestHeadlessJarPolicyAnswersWithTheRequestsOwner(t *testing.T) {
	manager, jars := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	proj := newProject(t, "P")
	files := map[string]string{}
	for name, dir := range map[string]string{"one": proj.one, "two": proj.two} {
		path := filepath.Join(dir, name+".html")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		files[name] = "file://" + canonical(t, path)
	}
	a, b := proj.access(manager, "a", proj.one), proj.access(manager, "b", proj.two)
	setJar(t, manager, a, JarProject)
	setJar(t, manager, b, JarProject)
	pageA := openPage(t, manager, a, "https://example.test/a")
	openPage(t, manager, b, "https://example.test/b")
	openerA := paneHandle(manager, a, pageA.ID)
	jars.ownFrame("F-of-A", openerA)

	browser := writeFakeChromium(t, "chromium")
	engine := newTestHeadlessEngine(t, browser.path)
	engine.events.PopupOpened = func(enginePopup) {}
	engine.ownerWait = 100 * time.Millisecond
	profile, err := engine.NewProfile(context.Background(), profileOptions{
		Jar: "project:P", DownloadDir: filepath.Join(t.TempDir(), "downloads"), Ephemeral: true,
		Allow: jars.profile("project:P").allow,
	})
	if err != nil {
		t.Fatalf("new profile: %v", err)
	}
	if _, err := profile.(*headlessProfile).ensureBrowser(); err != nil {
		t.Fatalf("launch: %v", err)
	}

	type pattern struct {
		URLPattern   string `json:"urlPattern"`
		ResourceType string `json:"resourceType"`
		RequestStage string `json:"requestStage"`
	}
	var want []pattern
	for _, p := range navigationPolicyPatterns {
		want = append(want, pattern{p.URLPattern, string(p.ResourceType), string(p.RequestStage)})
	}
	enabled := browserCallParams[struct{ Patterns []pattern }](t, browser.calls(), "Fetch.enable")
	if len(enabled) != 1 || !slices.Equal(enabled[0].Patterns, want) {
		t.Fatalf("Fetch.enable on the browser session: %+v, want once with %+v", enabled, want)
	}

	// expect sends each request and waits until every one is answered, so a
	// later event cannot be handled before them.
	expect := func(step string, requests map[string][3]string) {
		t.Helper()
		for id, r := range requests {
			browser.send(pausedRequest(id, r[0], r[1]))
		}
		answers := map[string]bool{}
		eventually(t, step+": the paused requests to be answered", func() bool {
			calls := browser.calls()
			for _, answer := range browserCallParams[requestAnswer](t, calls, "Fetch.continueRequest") {
				answers[answer.RequestID] = true
			}
			for _, answer := range browserCallParams[requestAnswer](t, calls, "Fetch.failRequest") {
				answers[answer.RequestID] = false
			}
			for id := range requests {
				if _, ok := answers[id]; !ok {
					return false
				}
			}
			return true
		})
		for id, r := range requests {
			if got := answers[id]; got != (r[2] == "allow") {
				t.Errorf("%s: frame %s requesting %s was allowed=%v, want %s", step, r[0], r[1], got, r[2])
			}
		}
	}

	expect("before the popup is reported", map[string][3]string{
		"R-early":         {"P-popup", files["one"], "refuse"},
		"R-early-web":     {"P-popup", "https://example.test/", "allow"},
		"R-unknown-frame": {"F-unknown", files["one"], "refuse"},
		"R-frame":         {"F-of-A", files["one"], "allow"},
		"R-frame-other":   {"F-of-A", files["two"], "refuse"},
	})

	// A request paused before its popup's creation is handled waits for it.
	browser.send(pausedRequest("R-waits", "P-popup", files["one"]))
	time.Sleep(20 * time.Millisecond)
	browser.send(popupCreated("P-popup", openerA))
	browser.send(popupCreated("P-nested", "P-popup"))
	expect("a request paused before its popup was known", map[string][3]string{})
	eventually(t, "the waiting request to be allowed", func() bool {
		for _, answer := range browserCallParams[requestAnswer](t, browser.calls(), "Fetch.continueRequest") {
			if answer.RequestID == "R-waits" {
				return true
			}
		}
		return false
	})
	eventually(t, "the popups to be known", func() bool {
		_, popup := engine.profileForPage("P-popup")
		_, nested := engine.profileForPage("P-nested")
		return popup && nested
	})
	expect("after the popups are reported", map[string][3]string{
		"R-popup":       {"P-popup", files["one"], "allow"},
		"R-popup-other": {"P-popup", files["two"], "refuse"},
		"R-nested":      {"P-nested", files["one"], "allow"},
	})

	if err := manager.ClosePage(t.Context(), a, PageID(pageA.ID)); err != nil {
		t.Fatal(err)
	}
	expect("after the opener closed", map[string][3]string{
		"R-orphan":       {"P-popup", files["one"], "refuse"},
		"R-orphan-other": {"P-popup", files["two"], "refuse"},
	})
}

// A profile answers every request it pauses, so one without a policy to ask
// is refused rather than created.
func TestHeadlessProfileRequiresANavigationPolicy(t *testing.T) {
	engine := newTestHeadlessEngine(t, writeFakeChromium(t, "chromium").path)
	if _, err := engine.NewProfile(context.Background(), profileOptions{
		Jar: "/home/dev/repo", DownloadDir: filepath.Join(t.TempDir(), "downloads"), Ephemeral: true,
	}); err == nil {
		t.Fatal("a profile with no navigation policy was created")
	}
}

// ownFrame makes the page with handle report frame as one of its subframes.
func (e *jarEngine) ownFrame(frame, handle string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.frames == nil {
		e.frames = make(map[string]string)
	}
	e.frames[frame] = handle
}

// A popup's crash reaches the Manager after its report, in the order
// Chromium sent them, however long the handling of an earlier target event
// takes: the crash is never handled before the creation that binds the
// popup, and so never dropped as a target nobody owns.
func TestHeadlessReportsAPopupsCrashAfterItsCreation(t *testing.T) {
	browser := writeFakeChromium(t, "chromium")
	engine := newTestHeadlessEngine(t, browser.path)
	release := make(chan struct{})
	var mu sync.Mutex
	var heard []string
	engine.events.PopupOpened = func(popup enginePopup) {
		if popup.Handle == "P-slow" {
			<-release
		}
		mu.Lock()
		heard = append(heard, "opened "+string(popup.Handle))
		mu.Unlock()
	}
	engine.events.PageCrashed = func(handle ids.PageHandle, reason string) {
		mu.Lock()
		heard = append(heard, "crashed "+string(handle))
		mu.Unlock()
	}
	profile := testHeadlessProfile(t, engine, "/home/dev/repo", false)
	if _, err := profile.ensureBrowser(); err != nil {
		t.Fatalf("launch: %v", err)
	}
	browser.send(popupCreated("P-slow", "T-1"))
	browser.send(popupCreated("P-1", "T-1"))
	browser.send(map[string]any{"method": "Target.targetCrashed", "params": map[string]any{"targetId": "P-1", "status": "crashed", "errorCode": 1}})
	// Long enough for a crash handled on a consumer of its own to have
	// been handled and dropped.
	time.Sleep(200 * time.Millisecond)
	close(release)
	want := []string{"opened P-slow", "opened P-1", "crashed P-1"}
	eventually(t, "the popups and the crash to be reported", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(heard) == len(want)
	})
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(heard, want) {
		t.Fatalf("the Manager heard %q, want %q", heard, want)
	}
}
