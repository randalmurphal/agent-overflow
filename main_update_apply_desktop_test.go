//go:build !nogui

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"agent-overflow/internal/startuppage"

	"github.com/wailsapp/wails/v3/pkg/application"
)

func servePage(t *testing.T, pages *desktopPages, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	pages.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	body, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	return rec.Code, string(body)
}

// TestDesktopPagesServeTheStartupPages: the window's assets are the loading
// page and the failure it was told to show, and nothing else.
func TestDesktopPagesServeTheStartupPages(t *testing.T) {
	pages := newDesktopPages("Preparing the update")
	if code, body := servePage(t, pages, startuppage.LoadingPath); code != http.StatusOK || !strings.Contains(body, "Preparing the update") {
		t.Fatalf("loading page = %d %q", code, body)
	}
	if code, _ := servePage(t, pages, "/index.html"); code != http.StatusNotFound {
		t.Fatalf("an app asset = %d; the helper serves only the startup pages", code)
	}
	if pages.showing() {
		t.Fatal("new pages show a failure")
	}

	pages.showFailure(startuppage.Failure{Title: "The update could not start.", Detail: "Close it.", Log: "/data/update.log"})
	if !pages.showing() {
		t.Fatal("the failure is not showing")
	}
	if code, body := servePage(t, pages, startuppage.FailurePath); code != http.StatusOK ||
		!strings.Contains(body, "The update could not start.") || !strings.Contains(body, "/data/update.log") {
		t.Fatalf("failure page = %d %q", code, body)
	}

	// Retry puts the loading page back.
	pages.showLoading()
	if pages.showing() {
		t.Fatal("the failure is still showing after the loading page")
	}
	if _, body := servePage(t, pages, startuppage.FailurePath); strings.Contains(body, "The update could not start.") {
		t.Fatal("the failure page outlived the loading page")
	}
	var none *desktopPages
	if none.showing() {
		t.Fatal("nil pages show a failure")
	}
}

// TestDesktopApplyWindowHidesOnlyWhileItRuns: closing the helper's window
// hides it while the loading page shows, and closes it on a failure.
func TestDesktopApplyWindowHidesOnlyWhileItRuns(t *testing.T) {
	window := &desktopApplyWindow{pages: newDesktopPages("")}
	window.loading()
	if !window.running.Load() || window.pages.showing() {
		t.Fatal("loading did not mark the window running")
	}
	window.fail(startuppage.Failure{Title: "x"})
	if window.running.Load() || !window.pages.showing() {
		t.Fatal("a failure left the window running")
	}
	window.loading()
	if !window.running.Load() || window.pages.showing() {
		t.Fatal("Retry's loading did not mark the window running")
	}
}

type recordedMenus struct{ set []*application.Menu }

func (m *recordedMenus) Set(menu *application.Menu) { m.set = append(m.set, menu) }

// TestDesktopApplyWindowDisablesQuitWhileItRuns: on macOS the helper's
// application menu holds Quit, whose Cmd+Q would end the helper mid-run.
// Quit is disabled while the loading page shows, from the first one that
// shows before the menu exists, and enabled on a failure page. Elsewhere
// the helper sets no application menu. On a Linux host the default menu is
// Linux's, with Quit under File; macOS's holds it under the application
// menu, which FindByRole searches the same way.
func TestDesktopApplyWindowDisablesQuitWhileItRuns(t *testing.T) {
	var elsewhere recordedMenus
	(&desktopApplyWindow{pages: newDesktopPages("")}).installMenu(&elsewhere, "linux")
	if len(elsewhere.set) != 0 {
		t.Fatal("the helper set an application menu off macOS")
	}

	window := &desktopApplyWindow{pages: newDesktopPages("")}
	quits := 0
	window.quitApp = func() { quits++ }
	// As runDesktopApplyWindow does before the application exists.
	window.running.Store(true)
	var menus recordedMenus
	window.installMenu(&menus, "darwin")
	if len(menus.set) != 1 || window.quitItem == nil || menus.set[0].FindByRole(application.Quit) != window.quitItem {
		t.Fatal("the helper's application menu is not set with its Quit held")
	}
	quit := window.quitItem
	if quit.Enabled() {
		t.Fatal("Quit is enabled while the first loading page shows")
	}
	window.fail(startuppage.Failure{Title: "x"})
	if !quit.Enabled() {
		t.Fatal("Quit is disabled on a failure page")
	}
	window.loading()
	if quit.Enabled() {
		t.Fatal("Quit is enabled while Retry's loading page shows")
	}
	window.fail(startuppage.Failure{Title: "x"})
	if !quit.Enabled() {
		t.Fatal("Quit is disabled on Retry's failure page")
	}
	window.quit()
	if quits != 1 || window.running.Load() {
		t.Fatalf("the helper's own quit quit %d times, running %t", quits, window.running.Load())
	}
}

// TestDesktopApplyWindowBindsOnlyRetry: the helper's window is a Wails
// service, so every exported method is callable from its pages. Only the
// Retry button's is.
func TestDesktopApplyWindowBindsOnlyRetry(t *testing.T) {
	typ := reflect.TypeOf(&desktopApplyWindow{})
	// Wails names a bound method by its receiver's package path, which is
	// "main" in the binary and the module path in this test binary.
	want := typ.Elem().PkgPath() + ".desktopApplyWindow.RetryMigration"
	if got := boundDesktopApplyMethod("RetryMigration"); got != want {
		t.Fatalf("bound method = %q, want %q", got, want)
	}
	var exported []string
	for i := range typ.NumMethod() {
		exported = append(exported, typ.Method(i).Name)
	}
	if !reflect.DeepEqual(exported, []string{"RetryMigration"}) {
		t.Fatalf("exported methods = %q", exported)
	}
}

// TestWebviewShellOpensOnAFailureShownBeforeTheWindow: a boot that refused
// before the window existed opens it on the failure, without the app's page.
func TestWebviewShellOpensOnAFailureShownBeforeTheWindow(t *testing.T) {
	pages := newDesktopPages("")
	shell := webviewShell{title: "Agent Overflow", pages: pages, pageURL: func() string { return "" }}
	if _, err := shell.windowOptions(); err == nil {
		t.Fatal("an empty page URL was accepted")
	}
	pages.showFailure(startuppage.Failure{Title: "Agent Overflow could not read its update record."})
	opts, err := shell.windowOptions()
	if err != nil || opts.URL != startuppage.FailurePath || opts.Title != "Agent Overflow" {
		t.Fatalf("windowOptions = %+v, %v", opts.URL, err)
	}
}
