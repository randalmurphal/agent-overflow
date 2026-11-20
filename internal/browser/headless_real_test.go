//go:build !windows

package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/chromedp"

	"agent-overflow/internal/browser/ids"
	"agent-overflow/internal/procutil/procutiltest"
)

// TestHeadlessChromiumReal and the TestHeadlessChromiumReal* smokes beside
// it are the tests that start a real browser, and they run only when
// asked, through the isolated test path: `AO_HEADLESS_CHROMIUM_SMOKE=1 make
// go-test GO_TEST_PKGS=./internal/browser GO_TEST_FLAGS='-run
// TestHeadlessChromiumReal -count=1'`. The gate is documented beside `make
// provider-smoke` in docs/architecture/development.md and is on no
// automatic target.
//
// What it proves that nothing above can: that this machine's Chromium
// accepts the command line chromiumArgs builds, sandbox and all, with only
// the mock keychain added on macOS, and hands out a target the shared CDP
// driver can attach to and navigate; that Dispose leaves no process of it running and an ephemeral
// profile nowhere on disk; that a persisted profile keeps a cookie set
// just before Dispose; that a popup's first request passes the
// workspace's navigation policy; and that the crash handler writes
// nothing into HOME. Run it after a Chromium major upgrade and before
// shipping a change to the launch flags or to how a browser is stopped.
func TestHeadlessChromiumReal(t *testing.T) {
	if os.Getenv("AO_HEADLESS_CHROMIUM_SMOKE") != "1" {
		t.Skip("set AO_HEADLESS_CHROMIUM_SMOKE=1 to launch this machine's real Chromium")
	}
	// Chromium inherits this HOME, so nothing the smoke starts writes into
	// the developer's own.
	home := t.TempDir()
	t.Setenv("HOME", home)
	engine, err := newHeadlessChromiumEngine(t.TempDir(), HeadlessChromiumOptions{Reaper: procutiltest.Reaper()}, engineEvents{
		PopupOpened:      func(enginePopup) {},
		PageClosed:       func(ids.PageHandle) {},
		PageCrashed:      func(ids.PageHandle, string) {},
		PageInfoChanged:  func(ids.PageHandle, string, string, uint64) {},
		DownloadStarted:  func(downloadStart) downloadAdmission { return downloadAdmitted },
		DownloadProgress: func(downloadProgress) {},
	})
	if err != nil {
		t.Fatalf("find a Chromium: %v", err)
	}
	t.Logf("launching %s", engine.binary)
	if runtime.GOOS == "darwin" {
		// macOS finds the keychain through HOME, and this one has none:
		// Chrome would ask for one in a dialog at every launch and keep no
		// cookie it cannot encrypt. The mock keychain keeps the smoke off
		// the keychain; a serve backend runs with the user's own HOME.
		wrapper := filepath.Join(t.TempDir(), "chromium")
		script := "#!/bin/sh\nexec '" + strings.ReplaceAll(engine.binary, "'", `'\''`) + "' --use-mock-keychain \"$@\"\n"
		if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		engine.binary = wrapper
	}
	engine.logf = func(format string, args ...any) { t.Logf(format, args...) }
	if err := engine.Start(t.Context()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer engine.Stop()

	profile := testHeadlessProfile(t, engine, t.TempDir(), false)
	page, err := profile.NewPage(t.Context(), testPageHooks())
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	// Operations run on a context derived from the page's own lifetime,
	// which is the Manager's contract (operationContext): a bare caller
	// context carries no chromedp target and chromedp refuses it.
	opCtx, cancel := operationContext(t.Context(), page.Lifetime(), operationTimeout)
	defer cancel()
	if err := page.Navigate(opCtx, "about:blank"); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	url, _, err := page.Info(opCtx)
	if err != nil {
		t.Fatalf("read page state: %v", err)
	}
	if url != "about:blank" {
		t.Fatalf("page URL = %q, want about:blank", url)
	}
	group := profile.currentProcess().cmd.Process.Pid
	page.Close()
	if err := profile.Dispose(t.Context()); err != nil {
		t.Fatalf("dispose: %v", err)
	}
	// With no process of Chromium's left, nothing can recreate the
	// directory, so its absence now is final.
	if left := chromiumLeftovers(t, group, profile.ephemeralRoot); len(left) > 0 {
		t.Fatalf("Chromium processes outlived Dispose: %v", left)
	}
	if _, err := os.Stat(profile.ephemeralRoot); !os.IsNotExist(err) {
		t.Fatalf("the ephemeral profile %s survives Dispose: %v", profile.ephemeralRoot, err)
	}

	workspace := t.TempDir()
	cookie := setRealCookie(t, testHeadlessProfile(t, engine, workspace, true))
	if got := readRealCookie(t, testHeadlessProfile(t, engine, workspace, true), cookie.Name); got != cookie.Value {
		t.Fatalf("the cookie set before Dispose reads %q after it, want %q", got, cookie.Value)
	}

	assertRealPopupsPassThePolicy(t, engine)
	assertRealDocumentTitles(t, engine)
	assertRealDocumentTitleChanges(t, engine)
	assertRealEvaluateSemantics(t, engine)
	assertRealPageOperations(t, engine)
	if err := filepath.WalkDir(home, func(path string, _ os.DirEntry, err error) error {
		if err == nil && filepath.Base(path) == "Crash Reports" {
			t.Errorf("the crash handler wrote %s outside the profile", path)
		}
		return err
	}); err != nil {
		t.Fatalf("read HOME: %v", err)
	}
}

// assertRealEvaluateSemantics holds the CDP driver to the evaluate contract
// on a page whose script policy forbids eval, as AO's own UI does.
func assertRealEvaluateSemantics(t *testing.T, engine *headlessEngine) {
	t.Helper()
	workspace := t.TempDir()
	index := filepath.Join(workspace, "evaluate.html")
	if err := os.WriteFile(index, []byte(evaluateSemanticsPage), 0o600); err != nil {
		t.Fatal(err)
	}
	profile := testHeadlessProfile(t, engine, workspace, false)
	defer func() {
		if err := profile.Dispose(context.Background()); err != nil {
			t.Errorf("dispose: %v", err)
		}
	}()
	page, err := profile.NewPage(t.Context(), testPageHooks())
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	opCtx, cancel := operationContext(t.Context(), page.Lifetime(), operationTimeout)
	defer cancel()
	if err := page.Navigate(opCtx, "file://"+index); err != nil {
		t.Fatalf("navigate to the evaluate page: %v", err)
	}
	assertEvaluateSemantics(t, evaluateEngineCDP, driverEvaluator(opCtx, page, false), driverEvaluator(opCtx, page, true))
}

// assertRealDocumentTitles proves the title watcher on a real Chromium: the
// parsed <title> and a later assignment by script both reach DocumentTitle,
// which no Target.targetInfoChanged reports, and a page script cannot see
// the binding.
func assertRealDocumentTitles(t *testing.T, engine *headlessEngine) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/frame" {
			_, _ = w.Write([]byte(`<!doctype html><title>Frame</title>`))
			return
		}
		_, _ = w.Write([]byte(`<!doctype html><title>Parsed</title><iframe src="/frame"></iframe><script>
setTimeout(() => { document.title = typeof __aoDocumentTitle + " by script" }, 300)
</script>`))
	}))
	defer server.Close()
	downloads := filepath.Join(t.TempDir(), "downloads")
	created, err := engine.NewProfile(t.Context(), profileOptions{
		Jar: jarKey(t.TempDir()), DownloadDir: downloads, OpenDownloadDir: openDirByName(downloads), Ephemeral: true, Allow: func([]frameID, string) (bool, bool) { return true, true },
	})
	if err != nil {
		t.Fatalf("new profile: %v", err)
	}
	profile := created.(*headlessProfile)
	defer func() {
		if err := profile.Dispose(context.Background()); err != nil {
			t.Errorf("dispose: %v", err)
		}
	}()
	titles := make(chan string, 16)
	hooks := testPageHooks()
	framed := make(chan string, 16)
	hooks.DocumentTitle = func(url, title string) {
		switch url {
		case server.URL + "/":
			titles <- title
		case "about:blank":
			// The tab's own first document, before the navigation.
		default:
			framed <- url
		}
	}
	page, err := profile.NewPage(t.Context(), hooks)
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	defer page.Close()
	opCtx, cancel := operationContext(t.Context(), page.Lifetime(), operationTimeout)
	defer cancel()
	if err := page.Navigate(opCtx, server.URL+"/"); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	for _, want := range []string{"Parsed", "undefined by script"} {
		select {
		case got := <-titles:
			if got != want {
				t.Fatalf("the document reported %q, want %q", got, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("the document never reported %q", want)
		}
	}
	// The frame loaded before the script's title change; only the tab's own
	// document reports.
	select {
	case url := <-framed:
		t.Fatalf("a document that is not the tab's reported its title: %s", url)
	default:
	}
}

// assertRealPopupsPassThePolicy opens an outside-workspace file in a popup
// from a workspace page, as a page script can, with and without an opener.
// Each popup runs before AttachPage installs its own policy, so what keeps
// the file out of the adopted page is the workspace's policy.
func assertRealPopupsPassThePolicy(t *testing.T, engine *headlessEngine) {
	t.Helper()
	workspace, outside := t.TempDir(), t.TempDir()
	index := filepath.Join(workspace, "index.html")
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(index, []byte("<html><body>workspace</body></html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("outside-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	inWorkspace := func(url string) bool {
		return !strings.HasPrefix(url, "file:") || strings.HasPrefix(url, "file://"+workspace+"/")
	}
	popups := make(chan enginePopup, 2)
	engine.events.PopupOpened = func(popup enginePopup) { popups <- popup }
	downloads := filepath.Join(t.TempDir(), "downloads")
	created, err := engine.NewProfile(t.Context(), profileOptions{
		Jar: jarKey(workspace), DownloadDir: downloads, OpenDownloadDir: openDirByName(downloads), Ephemeral: true, Allow: func(_ []frameID, url string) (bool, bool) { return inWorkspace(url), true },
	})
	if err != nil {
		t.Fatalf("new profile: %v", err)
	}
	profile := created.(*headlessProfile)
	defer func() {
		if err := profile.Dispose(context.Background()); err != nil {
			t.Errorf("dispose: %v", err)
		}
	}()
	hooks := testPageHooks()
	hooks.Allow = inWorkspace
	page, err := profile.NewPage(t.Context(), hooks)
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	opCtx, cancel := operationContext(t.Context(), page.Lifetime(), operationTimeout)
	defer cancel()
	if err := page.Navigate(opCtx, "file://"+index); err != nil {
		t.Fatalf("navigate to the workspace page: %v", err)
	}
	for _, features := range []string{"", "noopener"} {
		open := fmt.Sprintf(`window.open(%q, "_blank", %q); 1`, "file://"+secret, features)
		if _, err := chromedp.Call(opCtx, cdpruntime.Evaluate, cdpruntime.EvaluateParams{Expression: open, UserGesture: new(true)}); err != nil {
			t.Fatalf("open a popup with features %q: %v", features, err)
		}
		var popup enginePopup
		select {
		case popup = <-popups:
		case <-time.After(headlessTestDeadline):
			t.Fatalf("the popup with features %q was never reported", features)
		}
		driver, err := profile.AttachPage(t.Context(), popup.Handle, hooks)
		if err != nil {
			t.Fatalf("adopt the popup with features %q: %v", features, err)
		}
		popupCtx, popupCancel := operationContext(t.Context(), driver.Lifetime(), operationTimeout)
		status, _ := driver.PageStatus(popupCtx)
		for status.Ready != "complete" || status.URL == "about:blank" {
			if popupCtx.Err() != nil {
				t.Fatalf("the popup with features %q never finished loading: %+v", features, status)
			}
			time.Sleep(20 * time.Millisecond)
			status, _ = driver.PageStatus(popupCtx)
		}
		body, _, err := driverEvaluator(popupCtx, driver, false)(`document.body ? document.body.innerText : ""`, nil)
		popupCancel()
		driver.Close()
		if err != nil {
			t.Fatalf("read the popup with features %q: %v", features, err)
		}
		if strings.Contains(fmt.Sprint(body), "outside-secret") {
			t.Fatalf("the popup with features %q loaded the outside file at %s", features, status.URL)
		}
	}
	page.Close()
}

// setRealCookie sets a persistent cookie from a page of profile and
// disposes the profile, checking that its browser exited when asked.
func setRealCookie(t *testing.T, profile *headlessProfile) *network.Cookie {
	t.Helper()
	page, err := profile.NewPage(t.Context(), testPageHooks())
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	cookie := &network.Cookie{Name: "ao-smoke", Value: strconv.FormatInt(time.Now().UnixNano(), 36)}
	expires := cdp.TimeSinceEpoch(time.Now().Add(time.Hour).Unix())
	if _, err := chromedp.Call(page.(*cdpPage).ctx, network.SetCookie, network.SetCookieParams{
		Name: cookie.Name, Value: cookie.Value, Domain: "ao-smoke.test", Path: "/", Expires: expires,
	}); err != nil {
		t.Fatalf("set a cookie: %v", err)
	}
	process := profile.currentProcess()
	page.Close()
	if err := profile.Dispose(t.Context()); err != nil {
		t.Fatalf("dispose: %v", err)
	}
	if got := process.exitStatus(); got != "exit status 0" {
		t.Fatalf("the persisted profile's Chromium ended with %q, want it to exit when asked", got)
	}
	return cookie
}

// readRealCookie reads a cookie back from a fresh browser on profile, then
// disposes it.
func readRealCookie(t *testing.T, profile *headlessProfile, name string) string {
	t.Helper()
	browserCtx, err := profile.ensureBrowser()
	if err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	result, err := chromedp.CallBrowser(browserCtx, storage.GetCookies, storage.GetCookiesParams{})
	cookies := result.Cookies
	if err != nil {
		t.Fatalf("read cookies: %v", err)
	}
	if err := profile.Dispose(t.Context()); err != nil {
		t.Fatalf("dispose: %v", err)
	}
	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie.Value
		}
	}
	return ""
}
